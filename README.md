# static-s3-operator

> [!WARNING]
> **This repository was generated entirely with AI.**
>
> Every line of code, configuration, test and document in it was written by an AI
> model, and no part of it has been through human review or production use. It
> compiles, its tests pass, and its end-to-end suite publishes a real site to a
> real object store — but "the tests pass" is not the same as "this is correct",
> and this operator **creates and deletes S3 buckets**, which is not a reversible
> mistake.
>
> Use caution. Read the code before you run it. Point it at a throwaway bucket
> first, scope its credentials to a bucket prefix
> ([how](docs/credentials.md#the-access-policy)), and do not put it in front of
> anything you cannot afford to lose.

Host a static website in an S3 bucket, with the site's contents coming from a
directory inside a container image.

You already build your site into an image. This operator takes that image, pulls
out one directory, and makes an S3 bucket match it — creating the bucket,
configuring it for website hosting, making it publicly readable, and mirroring the
files. When you push a new image to the same tag, it notices and republishes.

```yaml
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: docs
  namespace: web
spec:
  source:
    image: ghcr.io/acme/docs:latest
    path: /usr/share/nginx/html
  bucket:
    name: acme-docs
```

That is a complete resource. Everything else has a default, and the defaults are
set once for the cluster in the Helm chart.

## Contents

- [What it does](#what-it-does)
- [Requirements](#requirements)
- [Install](#install)
- [Publish your first site](#publish-your-first-site)
- [How it works](#how-it-works)
- [Deleting a site deletes its bucket](#deleting-a-site-deletes-its-bucket)
- [DNS with external-dns](#dns-with-external-dns)
- [Keeping a site up to date](#keeping-a-site-up-to-date)
- [Non-AWS S3 providers](#non-aws-s3-providers)
- [Editor support for writing manifests](#editor-support-for-writing-manifests)
- [Examples](#examples)
- [Documentation](#documentation)
- [Development](#development)
- [License](#license)

## What it does

- **Creates and configures the bucket.** Website hosting with your index and error
  documents, a public-read policy, the AWS public access block cleared, and your
  tags.
- **Publishes a directory from an image.** The image is pulled over the registry
  API, one directory is extracted, and the bucket is made to match it.
- **Mirrors rather than accumulates.** Files removed from the image are removed
  from the bucket, so the bucket is the site and not every version of it.
- **Follows a moving tag.** A tagged reference is re-resolved on an interval
  (24 hours by default) and republished when the digest changes. A digest-pinned
  reference is never re-polled.
- **Sets the headers that make a site work.** Content types from file extensions,
  and `Cache-Control` per glob so hashed assets can be immutable while HTML stays
  revalidated.
- **Deletes the bucket with the resource** — but only a bucket it can prove it
  created. See [below](#deleting-a-site-deletes-its-bucket).
- **Publishes DNS through external-dns.** Each site gets an `ExternalName`
  Service aliasing the bucket's website hostname, with your hostnames on it.
- **Works with S3-compatible providers**, not only AWS, and degrades gracefully
  around the parts of the S3 API they do not implement.

## Requirements

- Kubernetes 1.29 or newer
- An S3 or S3-compatible object store, and one access key for it
  ([what it needs](docs/credentials.md))
- A container registry the cluster can reach, holding your site images

## Install

Create a Secret with the S3 access key, then install the chart pointing at it.
Keeping the key in a Secret you manage means it never lands in a values file or in
Helm release history:

```sh
kubectl create namespace static-s3-operator-system

kubectl create secret generic my-s3-credentials \
  --namespace static-s3-operator-system \
  --from-literal=accessKeyID="$AWS_ACCESS_KEY_ID" \
  --from-literal=secretAccessKey="$AWS_SECRET_ACCESS_KEY"

helm install static-s3-operator \
  oci://ghcr.io/rashkash103/charts/static-s3-operator \
  --namespace static-s3-operator-system \
  --set operator.credentials.existingSecret=my-s3-credentials
```

Set the cluster-wide defaults at install time, so individual sites only have to
say what makes them different:

```sh
helm install static-s3-operator \
  oci://ghcr.io/rashkash103/charts/static-s3-operator \
  --namespace static-s3-operator-system --create-namespace \
  --set operator.credentials.existingSecret=my-s3-credentials \
  --set operator.defaults.region=eu-west-1 \
  --set operator.defaults.sourcePath=/usr/share/nginx/html \
  --set operator.defaults.indexDocument=index.html \
  --set operator.defaults.errorDocument=404.html \
  --set operator.defaults.pullInterval=24h
```

Every setting is documented inline in
[`dist/chart/values.yaml`](dist/chart/values.yaml) and validated by
[`values.schema.json`](dist/chart/values.schema.json), so `helm install` rejects a
mistyped one instead of ignoring it.

Without Helm:

```sh
kubectl apply -f https://github.com/rashkash103/static-s3-operator/releases/latest/download/install.yaml
```

## Publish your first site

Build an image with your site in it. Any image works — the operator reads the
files out of it and never runs it:

```dockerfile
FROM nginx:alpine
COPY public/ /usr/share/nginx/html/
```

Or with no base image at all, since nothing in the image is executed:

```dockerfile
FROM scratch
COPY public/ /site/
```

`spec.source.path` says which directory in the image is the site — `/site` for the
second one above. Its contents become the bucket contents, so `/site/index.html`
is published as `index.html`. Leave it out to use the cluster default
(`operator.defaults.sourcePath`, `/usr/share/nginx/html` out of the box). See
[`examples/image-paths.yaml`](examples/image-paths.yaml) for the usual locations:
`/app/dist` for Vite, `/app/build` for Create React App, `/src/public` for Hugo,
`/usr/share/caddy` for Caddy.

Push it, then create a `StaticSite`:

```yaml
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: docs
  namespace: web
spec:
  source:
    image: ghcr.io/acme/docs:latest
  bucket:
    name: acme-docs
    website:
      indexDocument: index.html
      errorDocument: 404.html
  sync:
    cacheControl:
      # Build output with a hash in the filename never changes.
      - pattern: "assets/**"
        value: "public, max-age=31536000, immutable"
      # HTML has to stay revalidated or visitors keep seeing the old site.
      - pattern: "**/*.html"
        value: "public, max-age=0, must-revalidate"
  service:
    hostnames:
      - docs.example.com
```

Watch it:

```console
$ kubectl get staticsites -n web
NAME   PHASE   BUCKET       READY   LAST SYNC   AGE
docs   Ready   acme-docs    True    12s         30s

$ kubectl get staticsite docs -n web -o jsonpath='{.status.websiteURL}'
http://acme-docs.s3-website-us-east-1.amazonaws.com
```

If it is not `Ready`, the conditions say which stage it is stuck at:

```sh
kubectl describe staticsite docs -n web
```

## How it works

```
   StaticSite                    operator                        sync Job
       │                            │                                │
       │  spec.source.image ───────▶│  HEAD the manifest             │
       │                            │  tag → digest                  │
       │                            │                                │
       │  spec.bucket ─────────────▶│  create bucket                 │
       │                            │  website + policy + tags       │
       │                            │  write ownership token         │
       │                            │                                │
       │                            │  digest changed? ─── create ──▶│  pull image by digest
       │                            │                                │  extract spec.source.path
       │                            │                                │  diff against the bucket
       │                            │                                │  upload / delete
       │                            │◀───── termination message ─────│  report what it did
       │◀───── status + events ─────│                                │
```

The controller does the small, quick work: resolving a tag to a digest, and
keeping the bucket's configuration correct. Each publish runs as a Kubernetes Job,
because that is the part that unpacks a potentially large image the operator did
not build. The Job runs unprivileged, non-root, with a read-only root filesystem
and **no Kubernetes API access at all** — it reports back through its container
termination message.

A publish happens when the resolved digest changes, when the settings that decide
what gets uploaded change, or when you ask for one. An unchanged site costs one
manifest `HEAD` and one bucket listing, so a daily poll is cheap.

Files are compared by content hash, never by timestamp — every file extracted from
an image carries the image's build time, so timestamps say nothing about whether
the contents changed.

More detail in [docs/architecture.md](docs/architecture.md).

## Deleting a site deletes its bucket

`spec.bucket.deletionPolicy` defaults to `Delete`: removing a `StaticSite` empties
and removes its bucket. For a static site that is usually what you want, and it is
still destructive, so there is a guard on it.

When the operator creates a bucket it writes a random **ownership token** into the
bucket's tags (or, on a provider without bucket tagging, into a marker object) and
records the same token in the resource's status. Deletion requires both records to
agree. In practice:

| Situation | What happens on delete |
| --- | --- |
| The operator created the bucket | Emptied and deleted |
| The bucket already existed, so it was adopted | **Retained**, with a warning event |
| The bucket carries another `StaticSite`'s token | **Retained** |
| `spec.bucket.deletionPolicy: Retain` | **Retained** |

A bucket the operator did not create is never deleted, whatever the deletion
policy says. To override that deliberately:

```sh
kubectl annotate staticsite docs -n web web.rka.sh/force-bucket-deletion=true
```

Two more things worth knowing:

- **Uninstalling the chart does not delete your sites.** The CRD is kept by
  default (`crd.keep=true`). Deleting the CRD deletes every `StaticSite`, and with
  them their buckets.
- **Teardown gives up rather than wedging.** If the operator cannot reach S3 to
  delete a bucket — a deleted credentials Secret, say — it retries, and after
  `operator.reconcile.syncRetryMaxDelay` it releases the finalizer and emits a
  warning naming the bucket it left behind. A namespace stuck in `Terminating`
  forever is worse than a bucket you can see in an event.

## DNS with external-dns

Each site gets an `ExternalName` Service aliasing the bucket's website hostname.
List the names the site should answer on and external-dns publishes CNAME records
for them:

```yaml
spec:
  service:
    hostnames:
      - docs.example.com
      - www.docs.example.com
    annotations:
      external-dns.alpha.kubernetes.io/ttl: "300"
```

```console
$ kubectl get service docs -n web
NAME   TYPE           CLUSTER-IP   EXTERNAL-IP                                          PORT(S)   AGE
docs   ExternalName   <none>       acme-docs.s3-website-us-east-1.amazonaws.com         <none>    1m
```

A CNAME can only point at a hostname, never at a path. Providers whose website URL
is path-style (`endpoint/bucket/`) cannot be expressed as a CNAME alone: the record
resolves to the endpoint and something in front has to add the bucket prefix. Set
`spec.bucket.websiteEndpointTemplate` if your provider has a per-bucket website
hostname.

Set `spec.service.enabled: false` for a site reached only through a CDN you
configure elsewhere.

## Keeping a site up to date

**A moving tag.** `image: ghcr.io/acme/docs:latest` is re-resolved every
`spec.source.pullInterval` (24h by default). When the digest changes, the site is
republished. Set it to `"0"` to stop polling.

**Immediately, from CI.** Rather than waiting for the interval, have your pipeline
ask for a sync after it pushes:

```sh
kubectl annotate staticsite docs -n web \
  web.rka.sh/sync-requested-at="$(date -u +%FT%TZ)" --overwrite
```

**A pinned digest.** `image: ghcr.io/acme/docs@sha256:...` is never re-polled, so
your deployment tool decides when the site changes. This also means the operator
needs no registry credentials for that site — a digest cannot move, so it is
never resolved.

**Pause a site** without deleting it, which would delete its bucket:

```yaml
spec:
  suspend: true
```

## Non-AWS S3 providers

The operator works against any S3-compatible object store. Usually three settings:

```yaml
operator:
  defaults:
    endpoint: https://us-east-1.example-object-store.com
    forcePathStyle: true
    region: us-east-1
```

Or per site, with `spec.bucket.endpoint` and `spec.bucket.forcePathStyle`.

Compatible providers vary in which optional parts of the S3 API they implement, and
the operator treats each missing piece as absent rather than broken: a provider
without `PutBucketWebsite` still serves the site (it just will not rewrite a
directory request to `index.html`, and the `BucketReady` condition says so), one
without bucket tagging records ownership in a marker object, and one that rejects
object ACLs gets uploads without them. The full list, and what to set, is in
[docs/credentials.md](docs/credentials.md#s3-compatible-providers).

## Editor support for writing manifests

The CRD is also published as a standalone JSON Schema, so an editor can complete
and validate a `StaticSite` manifest as you type. In VS Code with the YAML
extension:

```jsonc
// .vscode/settings.json
{
  "yaml.schemas": {
    "https://raw.githubusercontent.com/rashkash103/static-s3-operator/main/dist/schemas/staticsite-v1alpha1.json": [
      "**/staticsite*.yaml",
      "**/sites/*.yaml"
    ]
  }
}
```

Or per file:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/rashkash103/static-s3-operator/main/dist/schemas/staticsite-v1alpha1.json
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
```

The Helm chart ships
[`values.schema.json`](dist/chart/values.schema.json) for the same reason — point
your editor at it when writing a values file, and `helm install` enforces it
regardless.

Schemas are generated from the CRDs by `make schemas`, so they cannot drift from
the API.

## Examples

Complete, commented manifests in [`examples/`](examples/):

| Example | What it shows |
| --- | --- |
| [`minimal.yaml`](examples/minimal.yaml) | The smallest useful `StaticSite` |
| [`image-paths.yaml`](examples/image-paths.yaml) | Pointing at the directory a given image keeps the site in |
| [`aws-production.yaml`](examples/aws-production.yaml) | AWS with cache policy, tags and DNS |
| [`s3-compatible-provider.yaml`](examples/s3-compatible-provider.yaml) | A non-AWS endpoint, path style, per-site credentials |
| [`spa.yaml`](examples/spa.yaml) | A single page application routed client side |
| [`digest-pinned.yaml`](examples/digest-pinned.yaml) | GitOps-style pinning, no polling |
| [`private-registry.yaml`](examples/private-registry.yaml) | A site image behind a private registry |
| [`multiple-sites-one-bucket.yaml`](examples/multiple-sites-one-bucket.yaml) | Two sites sharing a bucket under prefixes |
| [`adopt-existing-bucket.yaml`](examples/adopt-existing-bucket.yaml) | Publishing into a bucket you manage yourself |

## Documentation

| Document | Contents |
| --- | --- |
| [docs/credentials.md](docs/credentials.md) | The S3 access policy, and how to create a scoped key on AWS and elsewhere |
| [docs/configuration.md](docs/configuration.md) | Every `StaticSite` field and every chart value |
| [docs/architecture.md](docs/architecture.md) | How a publish works, ownership, and the security model |
| [docs/troubleshooting.md](docs/troubleshooting.md) | What each condition means and how to diagnose it |
| [AGENTS.md](AGENTS.md) | Working on this repository, including with an AI agent |

## Development

```sh
make help              # every target
make test              # unit tests and the controller suite against a real API server
make test-integration  # against a local S3 server, started in Docker
make test-e2e          # the full loop in a throwaway Kind cluster
make lint
make manifests generate schemas   # regenerate everything derived from the code
```

`make test-e2e` builds the operator image, starts an object store and a container
registry in a Kind cluster, generates a sample static site, publishes it as an
image, and then fetches the result over plain HTTP the way a browser would. It
also republishes a second version to check that removed files are removed, and
that a bucket the operator did not create survives deletion.

CI runs the same targets: `lint`, `test` (unit, controller and examples),
`test-integration` (against a local S3 server), `test-chart` (renders the chart,
installs it, and checks the manager starts), `test-e2e`, and `verify-generated`,
which fails if anything derived from the code has drifted.

## License

MIT. See [LICENSE](LICENSE).
