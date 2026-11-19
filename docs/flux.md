# Publishing on push with Flux webhooks

A `StaticSite` that follows a tag checks the registry every
`spec.source.pullInterval`, which is 24 hours by default. With
[Flux](https://fluxcd.io) installed, the registry can tell the operator about a
push as it happens, and the site is republished within seconds.

## Contents

- [How it works](#how-it-works)
- [Why there is an ImageRepository in the middle](#why-there-is-an-imagerepository-in-the-middle)
- [Requirements](#requirements)
- [Setting it up](#setting-it-up)
  - [1. The ImageRepository](#1-the-imagerepository)
  - [2. The Receiver](#2-the-receiver)
  - [3. Point the registry at it](#3-point-the-registry-at-it)
  - [4. Check it end to end](#4-check-it-end-to-end)
- [Other registries, and CI](#other-registries-and-ci)
- [Which sites a request reaches](#which-sites-a-request-reaches)
- [What a request does, and does not do](#what-a-request-does-and-does-not-do)
- [Lengthening the pull interval](#lengthening-the-pull-interval)
- [Security](#security)
- [Troubleshooting](#troubleshooting)

## How it works

```
registry ──webhook──▶ Flux Receiver
                          │ annotates reconcile.fluxcd.io/requestedAt
                          ▼
                   Flux ImageRepository  (spec.image: ghcr.io/acme/docs)
                          │ the operator's relay copies the annotation to
                          │ every StaticSite in the same namespace that
                          │ follows a tag in ghcr.io/acme/docs
                          ▼
                      StaticSite  (spec.source.image: ghcr.io/acme/docs:latest)
                          │ re-resolves :latest now, ignoring pullInterval
                          ▼
                 digest moved? ── no ──▶ nothing else happens
                          │ yes
                          ▼
                  sync Job publishes the new digest
```

1. The registry sends a webhook to a Flux `Receiver`.
2. The Receiver checks it and stamps `reconcile.fluxcd.io/requestedAt` on the
   objects it lists, here a Flux `ImageRepository`. This is ordinary Flux
   behaviour and has nothing to do with this operator.
3. The operator watches `ImageRepository` objects. When one receives a new
   request, it copies the annotation onto each matching `StaticSite`.
4. A `StaticSite` with a new `reconcile.fluxcd.io/requestedAt` resolves its tag
   straight away, and records the value in `status.lastHandledReconcileAt`.
5. If the tag now points at a different digest, the site is republished in the
   usual way. If not, nothing more happens.

## Why there is an ImageRepository in the middle

A Receiver cannot name a `StaticSite`. Its `spec.resources[].kind` is checked
against a fixed list of Flux's own kinds, and the API server rejects anything
else. Pointing the Receiver at the `Kustomization` that applies your `StaticSite`
does not help either: re-applying an unchanged manifest changes nothing, so the
operator never hears about it.

An `ImageRepository` is what Flux already uses to represent "an image in a
registry", and a Receiver pointed at one is the setup Flux documents for registry
webhooks. If you use Flux image automation, you may already have one for your
site's image, and that one works as it is.

The operator only reads the `ImageRepository`'s `spec.image` and its annotation.
It does not use Flux's tag scanning or `ImagePolicy`, and does not need them.

## Requirements

- **Flux's notification-controller**, which serves Receivers. It is part of
  every standard Flux install.
- **The `ImageRepository` API** (`image.toolkit.fluxcd.io`). This ships with
  Flux's image-reflector-controller, which is an optional component:

  ```sh
  flux install --components-extra=image-reflector-controller,image-automation-controller
  # or, when bootstrapping:
  flux bootstrap github ... --components-extra=image-reflector-controller,image-automation-controller
  ```

  The relay needs only the CRD. With the controller running too, Flux also scans
  the repository's tags. That does no harm, and it gives you
  `flux get image repository` for checking the image is reachable.
- **The operator started after that API existed.** The operator checks for it
  once, at startup. If you install Flux's image components after the operator,
  restart it:

  ```sh
  kubectl -n static-s3-operator-system rollout restart deployment \
    -l app.kubernetes.io/name=static-s3-operator
  ```

  The startup log says which case applies:

  ```
  Relaying Flux reconcile requests from ImageRepositories  apiVersion=image.toolkit.fluxcd.io/v1
  Flux ImageRepository API not found; Flux webhook relay disabled until restart
  ```

Nothing needs enabling on the operator. Its role already lets it read
`imagerepositories`, and Flux needs no extra permissions, because Flux only ever
annotates its own `ImageRepository`.

## Setting it up

The example publishes `docker.io/acme/docs:latest` in namespace `web`, with
Docker Hub sending the webhook. [Other registries](#other-registries-and-ci)
differ only in the Receiver's `type`.

```yaml
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: docs
  namespace: web
spec:
  source:
    image: docker.io/acme/docs:latest
    # The webhook is now the fast path; polling is only a safety net.
    pullInterval: 168h
  bucket:
    name: acme-docs
```

### 1. The ImageRepository

Put it in the **same namespace as the `StaticSite`**, and give it the same
repository without a tag:

```yaml
apiVersion: image.toolkit.fluxcd.io/v1
kind: ImageRepository
metadata:
  name: docs
  namespace: web
spec:
  image: docker.io/acme/docs
  # How often Flux scans the tags. The relay does not depend on it.
  interval: 24h
  # For a private repository, the same dockerconfigjson Secret the StaticSite
  # uses in spec.source.imagePullSecrets.
  # secretRef:
  #   name: registry-credentials
```

Repository names are normalised before they are compared, so `acme/docs`,
`docker.io/acme/docs` and `index.docker.io/acme/docs` all match each other.

### 2. The Receiver

The token is the shared secret for the webhook. Generate a random one:

```sh
TOKEN=$(head -c 12 /dev/urandom | shasum | cut -d ' ' -f1)
kubectl -n web create secret generic docs-webhook-token --from-literal=token="$TOKEN"
```

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Receiver
metadata:
  name: docs
  namespace: web
spec:
  type: dockerhub
  secretRef:
    name: docs-webhook-token
  resources:
    - apiVersion: image.toolkit.fluxcd.io/v1
      kind: ImageRepository
      name: docs
```

The Receiver may sit in another namespace than the `ImageRepository`, if your
Flux install allows cross-namespace references. The `ImageRepository` and the
`StaticSite` must share a namespace.

### 3. Point the registry at it

Flux serves Receivers from notification-controller's `webhook-receiver` Service,
which you expose with an Ingress or a load balancer. The webhook URL is that
address followed by the Receiver's `status.webhookPath`:

```sh
kubectl -n web get receiver docs -o jsonpath='{.status.webhookPath}'
# /hook/bed6d00b5555b1603e1f59b94d7fdbca58089cb5663633fb83f2815dc626d92b
```

On Docker Hub, add `https://<your-flux-webhook-host>/hook/bed6d00b...` as a
webhook on the `acme/docs` repository. See Flux's
[Receiver documentation](https://fluxcd.io/flux/components/notification/receivers/)
for exposing the Service.

### 4. Check it end to end

You do not need to push an image to test the operator's part. `flux reconcile`
sets the same annotation a webhook would:

```sh
flux reconcile image repository docs -n web
```

Then:

```sh
# The relay copied the request onto the site...
kubectl -n web get staticsite docs \
  -o jsonpath='{.metadata.annotations.reconcile\.fluxcd\.io/requestedAt}{"\n"}'

# ...and the site acted on it. These two values should be equal.
kubectl -n web get staticsite docs \
  -o jsonpath='{.status.lastHandledReconcileAt}{"\n"}'

# lastResolveTime should be a few seconds ago.
kubectl -n web get staticsite docs -o jsonpath='{.status.lastResolveTime}{"\n"}'
```

The operator log has one line per site reached:

```
Relayed a Flux reconcile request to StaticSite  staticSite=docs repository=index.docker.io/acme/docs
```

To test the webhook itself, push a new image to the tag and watch the site go
through `SyncInProgress`:

```sh
kubectl -n web get staticsite docs -w
```

## Other registries, and CI

Any Receiver type that Flux supports for registries works the same way. Only
`spec.type` changes, plus any secret fields that type needs:

| Registry | Receiver `type` |
| --- | --- |
| Docker Hub | `dockerhub` |
| Harbor | `harbor` |
| Quay | `quay` |
| Azure Container Registry | `acr` |
| Google Artifact Registry / GCR (via Pub/Sub) | `gcr` |
| Sonatype Nexus | `nexus` |

**GitHub Container Registry, and anything else.** These have no dedicated type.
The simplest option is to have the pipeline that pushes the image call a
`generic` Receiver after the push:

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Receiver
metadata:
  name: docs-ci
  namespace: web
spec:
  type: generic
  secretRef:
    name: docs-webhook-token
  resources:
    - apiVersion: image.toolkit.fluxcd.io/v1
      kind: ImageRepository
      name: docs
```

```yaml
# .github/workflows/publish.yaml, after the image is pushed
- name: Tell the cluster about the new image
  run: curl -fsS -X POST "https://flux-webhook.example.com${{ secrets.DOCS_WEBHOOK_PATH }}"
```

A `generic` Receiver is authenticated only by its URL, so keep the
`/hook/...` path secret. `generic-hmac` also checks a signature of the request
body, if you want more than that.

This replaces running `kubectl annotate` in CI, so the pipeline no longer needs
credentials for the cluster.

## Which sites a request reaches

A request on an `ImageRepository` reaches every `StaticSite` that:

- is in the **same namespace** as the `ImageRepository`;
- follows a **tag** (not a digest) in the **same repository**, after
  normalisation; and
- has not already been given this same request value.

So one webhook can republish several sites built from the same repository, for
example `docs:latest` and `docs:beta`. Each re-resolves its own tag and
republishes only if that tag moved.

Things that are deliberately excluded:

- **Other namespaces.** One tenant's `ImageRepository` cannot make another
  tenant's site do anything, even something as cheap as a registry check.
- **Digest-pinned sites.** A digest cannot move, so a webhook has nothing to tell
  them.

## What a request does, and does not do

`reconcile.fluxcd.io/requestedAt` means "check the registry now". It differs from
the operator's own `web.rka.sh/sync-requested-at`:

| | `reconcile.fluxcd.io/requestedAt` | `web.rka.sh/sync-requested-at` |
| --- | --- | --- |
| Re-resolves the tag immediately | Yes | Yes |
| Republishes when the digest is unchanged | **No** | Yes |
| Overrides the retry backoff after a failed publish | No | Yes |
| Recorded in | `status.lastHandledReconcileAt` | `status.lastHandledSyncRequest` |
| Typically set by | A Flux Receiver, by way of the relay | A person, or CI with cluster access |

The difference is on purpose. A registry webhook fires for **every** push to a
repository, including tags this site does not follow, such as a `pr-123` build.
Each of those costs one manifest `HEAD` request and nothing more.

The same holds when you set the annotation on a `StaticSite` yourself:

```sh
kubectl annotate staticsite docs -n web \
  reconcile.fluxcd.io/requestedAt="$(date -u +%FT%TZ)" --overwrite
```

Use `web.rka.sh/sync-requested-at` instead when you want to force a full
republish, for example after somebody edited the bucket by hand.

A suspended site (`spec.suspend: true`) keeps the request and acts on it once it
is resumed.

## Lengthening the pull interval

With webhooks in place, `spec.source.pullInterval` (or the chart's
`operator.defaults.pullInterval`) only catches webhooks that were never delivered.
A week is reasonable. Setting it to `"0"` turns polling off completely, and then
a lost webhook leaves the site stale until the next push.

## Security

- **The operator gains read access to `ImageRepository` objects** cluster wide,
  and nothing else. It never writes to Flux objects.
- **The operator writes one annotation on `StaticSite` objects**, the same way
  Flux annotates the objects it is pointed at. GitOps tools leave annotations they
  did not set alone, so this does not show up as drift.
- **Flux's permissions are unchanged.** notification-controller only annotates
  the `ImageRepository`, which it can already do.
- **A forged webhook can cause, at most, a registry check.** What gets published
  is whatever the registry says the tag points at, never anything in the webhook
  body. Keep the Receiver's token and URL secret regardless.

## Troubleshooting

Work down the chain until a step fails.

**Did the webhook reach Flux?**

```sh
kubectl -n web describe receiver docs        # Ready, and events per request
kubectl -n flux-system logs deploy/notification-controller | grep docs
```

**Did the ImageRepository get the request?**

```sh
kubectl -n web get imagerepository docs \
  -o jsonpath='{.metadata.annotations.reconcile\.fluxcd\.io/requestedAt}{"\n"}'
```

If this is empty or old, the problem is between the registry and Flux: the URL,
the token, or the Receiver's `type`.

**Is the relay running?** Look for the startup line in the operator log:

```sh
kubectl -n static-s3-operator-system logs deploy/static-s3-operator-controller-manager \
  | grep -i 'imagerepositor'
```

`Flux ImageRepository API not found` means the operator started before Flux's
image API was installed. Restart it.

**Did the relay reach the site?** If the `ImageRepository` has the request but
the `StaticSite`'s `reconcile.fluxcd.io/requestedAt` annotation does not:

- Compare namespaces: they must be equal.
- Compare `ImageRepository.spec.image` with the repository part of
  `StaticSite.spec.source.image`. A different registry host, for example
  `ghcr.io` against `docker.io`, is a different repository.
- Check the site follows a tag. A digest-pinned site is skipped.

**Did the site act on it?** If the annotation is there but
`status.lastHandledReconcileAt` differs, look at the site's conditions. A
suspended site, or one whose bucket stage is failing, does not reach the resolve
step. See [troubleshooting.md](troubleshooting.md).

**It resolved, but nothing was published.** Compare `status.resolvedDigest` with
what you pushed. If they match `status.syncedDigest`, the tag had not moved,
perhaps because the push was to a different tag. If a previous publish failed,
the new one waits until `status.nextSyncTime`. Set
`web.rka.sh/sync-requested-at` to skip the wait.
