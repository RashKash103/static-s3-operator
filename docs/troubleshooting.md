# Troubleshooting

## First look

```sh
kubectl get staticsites -A
kubectl describe staticsite my-site -n my-namespace
```

`describe` shows the conditions and the recent events, which between them name the
stage that is failing and why. The stages run in order, so the first condition that
is `False` is the one to read:

| Condition | Meaning when `False` |
| --- | --- |
| `BucketReady` | The credentials or the bucket are the problem. Nothing else has been attempted. |
| `SourceResolved` | The bucket is fine; the image reference cannot be resolved. |
| `Synced` | Bucket and image are both fine; the publish itself failed or is in progress. |
| `Ready` | The aggregate. Read the others. |

Useful follow-ups:

```sh
# The operator's own view of what it is doing.
kubectl logs -n static-s3-operator-system \
  deployment/static-s3-operator-controller-manager -f

# The publish itself.
kubectl get jobs -n my-namespace -l web.rka.sh/staticsite=my-site
kubectl logs -n my-namespace job/my-site-sync-xxxxx

# What the last publish did.
kubectl get staticsite my-site -n my-namespace -o jsonpath='{.status.lastSyncStats}'
kubectl get staticsite my-site -n my-namespace -o jsonpath='{.status.syncHistory}'
```

## Contents

- [The site is stuck at Pending](#the-site-is-stuck-at-pending)
- [BucketReady is False](#bucketready-is-false)
- [SourceResolved is False](#sourceresolved-is-false)
- [Synced is False](#synced-is-false)
- [The site is Ready but the page does not load](#the-site-is-ready-but-the-page-does-not-load)
- [The site is Ready but out of date](#the-site-is-ready-but-out-of-date)
- [The bucket was not deleted](#the-bucket-was-not-deleted)
- [A StaticSite will not delete](#a-staticsite-will-not-delete)
- [DNS is not being published](#dns-is-not-being-published)
- [Diagnostic commands](#diagnostic-commands)

## The site is stuck at Pending

No conditions at all usually means the operator is not running or not watching:

```sh
kubectl get deployment -n static-s3-operator-system
kubectl logs -n static-s3-operator-system deployment/static-s3-operator-controller-manager --tail=50
```

A manager that will not start most often failed its configuration check. The log
line says which flag:

```
Invalid operator configuration  {"error": "invalid operator configuration: --syncer-image must be set ..."}
```

`--syncer-image` is normally discovered from the operator's own Pod, which needs
`POD_NAME` and `POD_NAMESPACE` in the environment. The chart sets both; a
hand-written Deployment has to as well, or set `operator.syncer.image` explicitly.

## BucketReady is False

### `CredentialsError`

```
Secret "my-s3-credentials" named by spec.bucket.credentials does not exist
```

Check the name and the namespace. A per-site Secret must be in the same namespace
as the `StaticSite`.

```
Secret "static-s3-operator-system/static-s3-operator-s3-credentials" is missing key(s) [accessKeyID]
```

The key names are configurable, and default to `accessKeyID` and
`secretAccessKey`:

```sh
kubectl get secret my-s3-credentials -n my-namespace -o jsonpath='{.data}' | tr ',' '\n'
```

```
the operator's default credentials live in namespace "static-s3-operator-system" and
mirroring is disabled; set spec.bucket.credentials to a Secret in namespace "web"
```

`operator.credentials.mirrorToSiteNamespaces` is off, so a site outside the
operator's namespace has to bring its own credentials. See
[credentials.md](credentials.md#sites-in-other-namespaces).

### `BucketConfigurationFailed`

```
create bucket "my-site": ... AccessDenied
```

The access key is missing a permission, or the resource pattern in its policy does
not cover this bucket name. Compare against
[the policy](credentials.md#the-access-policy). Test the key directly:

```sh
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
  aws s3api create-bucket --bucket my-site --region us-east-1
```

```
bucket "my-site" already exists and belongs to someone else
```

Bucket names are global per provider. Pick another; `spec.bucket.name` is
immutable, so this means a new `StaticSite`.

```
bucket "my-site" already carries an ownership record from another StaticSite (web/other-site)
```

Two sites are pointed at one bucket. That is refused because they would delete each
other's files. To share a bucket deliberately, give each site its own
`spec.sync.keyPrefix` — but only one of them can own the bucket, and the other will
be adopted. See
[`examples/multiple-sites-one-bucket.yaml`](../examples/multiple-sites-one-bucket.yaml).

```
InvalidAccessKeyId / SignatureDoesNotMatch
```

The key is wrong, or the endpoint and region do not match the key's provider. For
an S3-compatible endpoint check that `spec.bucket.endpoint` is the **API** endpoint
and that `forcePathStyle` is on.

## SourceResolved is False

```
resolve "ghcr.io/acme/site:latest": ... MANIFEST_UNKNOWN
```

The tag does not exist. Note that the bucket is created before this stage, so
`BucketReady` being `True` here is expected.

```
resolve "ghcr.io/acme/site:latest": ... UNAUTHORIZED
```

A private image needs `spec.source.imagePullSecrets`, naming a
`kubernetes.io/dockerconfigjson` Secret in the site's namespace:

```sh
kubectl create secret docker-registry ghcr-pull -n web \
  --docker-server=ghcr.io --docker-username="$USER" --docker-password="$TOKEN"
```

The same Secret is projected into the sync Job, which pulls the layers itself. A
kubelet pull secret alone is not enough.

```
Secret "ghcr-pull" named by spec.source.imagePullSecrets does not exist
```

Same namespace as the `StaticSite`.

```
x509: certificate signed by unknown authority
```

A registry with a private CA. For development, `spec.source.insecureSkipTLSVerify:
true`; properly, add the CA to the operator's and the Job's trust store. For a
plain-HTTP registry, `spec.source.plainHTTP: true`.

## Synced is False

### `SyncInProgress`

A publish is running. Watch it:

```sh
kubectl get jobs -n my-namespace -l web.rka.sh/staticsite=my-site -w
kubectl logs -n my-namespace -l web.rka.sh/staticsite=my-site -f --tail=100
```

### `SyncFailed`

The message is the syncer's own where it managed to write one. Common causes:

```
source path not found in image: /usr/share/nginx/html
```

The directory is not in the image. Check what is:

```sh
docker run --rm --entrypoint ls ghcr.io/acme/site:latest -la /usr/share/nginx/html
```

Then set `spec.source.path`, or change the cluster default
(`operator.defaults.sourcePath`). Common locations: `/usr/share/nginx/html`
(nginx), `/app/dist` (Vite), `/app/build` (Create React App), `/site` (a
`scratch` image), `/usr/share/caddy` (Caddy).

```
image contents under /site exceed the 2147483648 byte limit
```

Raise `operator.syncer.maxExtractBytes`, and give the Job enough disk:

```yaml
spec:
  sync:
    job:
      resources:
        limits:
          ephemeral-storage: 8Gi
```

```
BackoffLimitExceeded
```

The Job gave up. Read a Pod's logs; if they are gone, raise
`spec.sync.job.ttlSecondsAfterFinished` so the next failure sticks around.

**A Pod that is `OOMKilled`** leaves no report at all, which shows up as a failed
Job with a generic message. Raise the memory limit:

```yaml
spec:
  sync:
    job:
      resources:
        limits:
          memory: 1Gi
```

**A Pod stuck in `ErrImagePull`** cannot pull the *syncer* image, which is the
operator's own image. In a Kind or air-gapped cluster, load it onto the nodes and
set `operator.syncer.imagePullPolicy=Never`.

## The site is Ready but the page does not load

The site being `Ready` means the objects are in the bucket. If a browser cannot see
them, work down this list.

**403 on every object.** The bucket policy is not in effect. Check that
`spec.bucket.publicRead` is true, and on AWS that the account-level public access
block is not overriding the bucket:

```sh
aws s3api get-public-access-block --bucket my-site
aws s3control get-public-access-block --account-id 123456789012
```

An account-level block cannot be cleared by the operator, and silently defeats any
bucket policy.

**404 for a directory, but `/index.html` works.** The provider does not implement
website configuration, or the site is being fetched from the API endpoint rather
than the website endpoint. The condition says which:

```sh
kubectl get staticsite my-site -n my-namespace \
  -o jsonpath='{.status.conditions[?(@.type=="BucketReady")].message}'
```

On AWS, use `status.websiteURL` — the API endpoint does not do index rewriting
either. On a provider without website support, link to files directly or put a CDN
in front.

**The page downloads instead of rendering.** A wrong `Content-Type`. Check it, and
override it if the extension is unusual:

```sh
curl -sI "$(kubectl get staticsite my-site -n my-ns -o jsonpath='{.status.websiteURL}')/index.html"
```

```yaml
spec:
  sync:
    contentTypes:
      - pattern: "**/*.myext"
        value: "text/html; charset=utf-8"
```

**A single page application 404s on deep links.** Point the error document at the
app:

```yaml
spec:
  bucket:
    website:
      errorDocument: index.html
```

**Stale content in a browser.** Check `Cache-Control`. HTML must stay revalidated:

```yaml
spec:
  sync:
    cacheControl:
      - pattern: "assets/**"
        value: "public, max-age=31536000, immutable"
      - pattern: "**/*.html"
        value: "public, max-age=0, must-revalidate"
```

A CDN in front needs its own invalidation; the operator does not do that.

## The site is Ready but out of date

**`status.nextResolveTime` is in the future.** The tag is only re-checked on the
interval. Publish now:

```sh
kubectl annotate staticsite my-site -n my-namespace \
  web.rka.sh/sync-requested-at="$(date -u +%FT%TZ)" --overwrite
```

Doing this from CI after every push is better than shortening the interval, and
a registry webhook through Flux is better still: see [flux.md](flux.md).

**A webhook arrived but the site did not change.** Follow the chain in
[flux.md § Troubleshooting](flux.md#troubleshooting). In short:

```sh
# The relay delivered the request, and the site handled it, when these are equal.
kubectl get staticsite my-site -n my-namespace -o jsonpath='{.metadata.annotations.reconcile\.fluxcd\.io/requestedAt}{"  "}{.status.lastHandledReconcileAt}{"\n"}'
```

A webhook republishes only when the tag moved, and waits out the backoff after a
failed publish (`status.nextSyncTime`).

**The digest did not change.** Check what the operator sees:

```sh
kubectl get staticsite my-site -n my-namespace \
  -o custom-columns='RESOLVED:.status.resolvedDigest,SYNCED:.status.syncedDigest'
```

Equal digests mean the tag still points at the same image — your build pushed
elsewhere, or pushed the same content.

**The site is suspended.**

```sh
kubectl get staticsite my-site -n my-namespace -o jsonpath='{.spec.suspend}'
```

**The image is digest-pinned**, in which case nothing changes until the reference
does.

## The bucket was not deleted

Look for a `BucketRetained` event:

```sh
kubectl get events -n my-namespace --field-selector reason=BucketRetained
```

| Reason in the event | Why |
| --- | --- |
| `spec.bucket.deletionPolicy is Retain` | As configured |
| `the bucket carries no ownership record from this operator` | The bucket was adopted, not created here, so it is never deleted |
| `the bucket's ownership token belongs to a different StaticSite` | Another resource created it |
| `the StaticSite has no recorded ownership token` | Status was lost, or the bucket was adopted |
| `Could not delete bucket ... it has been left behind` | S3 was unreachable for the whole grace period; the finalizer was released to avoid wedging the namespace |

This is working as intended: a bucket the operator cannot prove it created is never
destroyed. Delete it yourself, or if you are certain, override before deleting the
resource:

```sh
kubectl annotate staticsite my-site -n my-namespace web.rka.sh/force-bucket-deletion=true
kubectl delete staticsite my-site -n my-namespace
```

## A StaticSite will not delete

It is waiting on its finalizer, which means teardown is failing. The reason is in
the conditions and events:

```sh
kubectl get staticsite my-site -n my-namespace -o jsonpath='{.status.conditions}'
kubectl get events -n my-namespace --field-selector involvedObject.name=my-site
```

Usually the credentials Secret was deleted first, or the endpoint is unreachable.
The operator gives up by itself after `operator.reconcile.syncRetryMaxDelay`
(30 minutes by default) and releases the finalizer with a warning, so waiting is a
valid option.

To release it now, accepting that the bucket is left behind:

```sh
kubectl patch staticsite my-site -n my-namespace \
  --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]'
```

## DNS is not being published

**No Service.**

```sh
kubectl get service my-site -n my-namespace
kubectl get staticsite my-site -n my-namespace -o jsonpath='{.status.serviceName}'
```

An empty `serviceName` means either `spec.service.enabled: false`, or that no
website hostname could be derived. Fix the second with
`spec.bucket.websiteEndpointTemplate`, or set `spec.service.externalName`
directly.

**A Service with no hostname annotation.** external-dns needs it. Either list
`spec.service.hostnames`, or check that
`operator.externalDNS.hostnameAnnotation` matches the key your external-dns reads.

**The record points somewhere useless.** A CNAME can only point at a hostname. If
your provider's website URL is path-style (`endpoint/bucket/`), the record resolves
to the endpoint and something in front has to add the prefix. Set
`spec.bucket.websiteEndpointTemplate` if the provider offers a per-bucket hostname.

**external-dns is not picking the Service up.** Check its own logs, and that its
`--source=service` is enabled and its domain filter covers the hostname.

## Diagnostic commands

```sh
# Everything the operator knows about a site.
kubectl get staticsite my-site -n my-namespace -o yaml

# A one-line status per site, cluster wide.
kubectl get staticsites -A -o custom-columns=\
'NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,BUCKET:.status.bucketName,'\
'SYNCED:.status.syncedDigest,LAST:.status.lastSyncTime,URL:.status.websiteURL'

# Which sites are not Ready.
kubectl get staticsites -A -o json | jq -r '
  .items[] | select((.status.conditions // []
    | map(select(.type=="Ready")) | .[0].status // "Unknown") != "True")
  | "\(.metadata.namespace)/\(.metadata.name): \(.status.phase)"'

# The publish history of a site.
kubectl get staticsite my-site -n my-namespace -o json | jq '.status.syncHistory'

# The exact arguments the last publish ran with.
kubectl get jobs -n my-namespace -l web.rka.sh/staticsite=my-site \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{.spec.template.spec.containers[0].args}{"\n\n"}{end}'

# What is actually in the bucket.
aws s3 ls "s3://my-site" --recursive | head -50

# What the operator was told to default to.
kubectl get deployment -n static-s3-operator-system \
  static-s3-operator-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' \
  | tr ',' '\n'
```

Turn up the operator's verbosity for the per-entry detail of an extraction and the
reasons behind requeues:

```sh
helm upgrade static-s3-operator oci://ghcr.io/rashkash103/charts/static-s3-operator \
  --namespace static-s3-operator-system --reuse-values \
  --set 'manager.args={--leader-elect,--zap-log-level=debug}'
```
