# Configuration reference

Two layers. The **chart** sets cluster-wide defaults, so a `StaticSite` only has to
say what makes it different. A **`StaticSite`** overrides any of them per site.

Anything marked *operator default* has no CRD-level default on purpose: the cluster
chooses it once, and a site inherits it.

## Contents

- [StaticSite](#staticsite)
  - [spec.source](#specsource)
  - [spec.bucket](#specbucket)
    - [How a bucket name is generated](#how-a-bucket-name-is-generated)
      - [The cluster UUID](#the-cluster-uuid)
      - [Computing the name yourself](#computing-the-name-yourself)
    - [Where the website URL comes from](#where-the-website-url-comes-from)
  - [spec.sync](#specsync)
  - [spec.service](#specservice)
  - [spec.suspend](#specsuspend)
  - [status](#status)
  - [Annotations](#annotations)
- [Chart values](#chart-values)
- [Manager flags](#manager-flags)

## StaticSite

`web.rka.sh/v1alpha1`, namespaced, short names `ss` and `statics`.

```yaml
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: docs
  namespace: web
spec:
  source: {...}   # required: where the files come from
  bucket: {...}   # optional: where they go, if not the cluster defaults
  sync: {...}     # optional: how they are copied
  service: {...}  # optional: the DNS alias
  suspend: false  # optional
```

### spec.source

Where the site's files come from.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `image` | string | **required** | Container image holding the built site. A tag is re-resolved on an interval; a digest is treated as immutable and never re-polled. |
| `path` | string | *operator default* (`/usr/share/nginx/html`) | Absolute directory inside the image whose **contents** become the site root. With `/site`, `/site/index.html` uploads as `index.html`. |
| `pullInterval` | duration | *operator default* (`24h`) | How often a tagged reference is re-resolved. `"0"` disables polling. Ignored for a digest. |
| `imagePullSecrets` | list of `{name}` | none | `dockerconfigjson` Secrets in this namespace. Used by the operator to resolve the tag **and** by the sync Job to download the layers. |
| `plainHTTP` | bool | `false` | Talk to the registry over HTTP. For an in-cluster development registry. |
| `insecureSkipTLSVerify` | bool | `false` | Skip registry certificate verification. Development only. |

Nothing in the image is executed, so any image works, including `FROM scratch`.

`path` is how you point at wherever a particular image keeps its files. Its
**contents** become the site root, so `/app/dist` publishes `/app/dist/index.html`
as `index.html`. Common locations:

| Image | `path` |
| --- | --- |
| nginx | `/usr/share/nginx/html` |
| Caddy | `/usr/share/caddy` |
| Vite, Astro, SvelteKit | `/app/dist` |
| Create React App | `/app/build` |
| Hugo | `/src/public` |
| Jekyll | `/srv/jekyll/_site` |
| `FROM scratch` | wherever you copied it |

To check an image:

```sh
docker run --rm --entrypoint find ghcr.io/acme/site:latest / -name index.html
```

[`examples/image-paths.yaml`](../examples/image-paths.yaml) shows each of these,
including two sites published from different subdirectories of one image.

### spec.bucket

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `name` | string | *generated, immutable* | Bucket name, 3–63 characters, lowercase. Leave it out and the operator derives one from the resource: see [How a bucket name is generated](#how-a-bucket-name-is-generated). |
| `region` | string | *operator default* (`us-east-1`) | S3 region. |
| `endpoint` | string | *operator default* (empty = AWS) | S3 **API** endpoint for a compatible provider. Without a scheme, https is assumed. |
| `websiteEndpointTemplate` | string | *operator default* | Go template for `status.websiteURL`, with `{{.Bucket}}`, `{{.Region}}`, `{{.Endpoint}}`. Also what the `ExternalName` Service points at. See [Where the website URL comes from](#where-the-website-url-comes-from). |
| `forcePathStyle` | bool | *operator default* (`false`) | Address the bucket as `endpoint/bucket`. Most compatible providers need this; AWS does not. |
| `credentials` | object | operator's own | A Secret in this namespace: `secretRef.name`, plus `accessKeyIDKey`, `secretAccessKeyKey`, `sessionTokenKey` (defaults `accessKeyID`, `secretAccessKey`, `sessionToken`). |
| `create` | bool | `true` | Create the bucket if absent. `false` adopts a bucket managed elsewhere, which is then never deleted. |
| `deletionPolicy` | `Delete`\|`Retain` | `Delete` | `Delete` empties and removes the bucket when the resource is deleted — **only** for a bucket the operator created. See [ownership](architecture.md#bucket-ownership). |
| `website` | object or `null` | `{}` | `indexDocument` and `errorDocument`, both *operator defaults*. `null` skips website configuration entirely. |
| `publicRead` | bool | `true` | A bucket policy granting anonymous `s3:GetObject`, and the AWS public access block cleared. |
| `policy` | string | none | A raw bucket policy document, replacing the generated one. `${bucket}` is substituted. |
| `tags` | map | none | Bucket tags, alongside the operator's ownership tags. Keys beginning `static-s3-operator/` are reserved and discarded. |

A site whose bucket is not public needs something in front that reads it with its
own credentials, and should also set `sync.acl: private`.

#### How a bucket name is generated

Leaving `spec.bucket.name` out is the normal case. The operator derives the name
from the resource, and the derivation is deliberately **opaque**: a bucket name is
part of the website hostname, so it appears in every CNAME record pointing at the
site. A name like `web-docs` would publish the fact that your cluster has a `web`
namespace containing `docs` — an inventory of the cluster, readable by anyone who
looks up the DNS.

**The name is `static-s3-<uuid>`**, where the UUID is a
[version 5 UUID](https://www.rfc-editor.org/rfc/rfc9562#name-uuid-version-5) —
the standard construction for a deterministic UUID derived from a name inside a
namespace:

| Part | Value |
| --- | --- |
| UUID namespace | the cluster's UUID (see below) |
| name | `<kubernetes namespace>/<kubernetes name>` |

So a `StaticSite` called `docs` in namespace `web` publishes to something like
`static-s3-f1acd0bd-d4a1-5773-b2b7-19b67ff61a44`. It is stable, it is the same
every time, and it says nothing about the cluster.

##### The cluster UUID

The UUID namespace is what makes the name *unguessable* rather than merely
unreadable. Kubernetes namespaces and resource names are short and predictable, so
an unsalted digest of `web/docs` could simply be enumerated by anyone who suspected
that name. Deriving under a value unique to your cluster removes that.

By default the operator uses the UID of the `kube-system` namespace: unique to the
cluster, stable for its lifetime, generated at bootstrap, and published nowhere.
Nothing has to be created or stored. Read it with:

```sh
kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'
```

Set `operator.defaults.bucketNameUUIDNamespace` to any UUID to override it. Two
reasons you might:

- **Stability across clusters.** The default ties names to one cluster. If sites
  are ever rebuilt into a fresh cluster and have to find their existing buckets,
  pin the value so the names come out the same.
- **`rbac.namespaced: true`.** Reading `kube-system` needs `get` on namespaces,
  which is cluster-scoped and not granted in namespaced mode, so the operator will
  refuse to start until you set this.

##### Computing the name yourself

This is the point of a deterministic name: you can work it out before the resource
exists, to create the bucket by hand or scope an access policy to it.

From this repository:

```sh
make bucket-name NAMESPACE=web NAME=docs
```

That reads the cluster UUID from your current kubeconfig context. Pass
`CLUSTER_UUID=…` to compute it offline, and `BUCKET_NAME_TEMPLATE=…` if your
cluster does not use the default template.

With standard tools, no repository needed — `uuidgen` from util-linux 2.38 or
newer:

```sh
CLUSTER_UUID=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')

echo "static-s3-$(uuidgen --sha1 --namespace "$CLUSTER_UUID" --name 'web/docs')"
```

Or with Python, which is more portable and gives the same answer:

```sh
CLUSTER_UUID=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')

python3 -c 'import uuid, sys; print("static-s3-" + str(uuid.uuid5(uuid.UUID(sys.argv[1]), sys.argv[2])))' \
  "$CLUSTER_UUID" "web/docs"
```

Both take the name as `<namespace>/<name>` with a slash, which is exactly what the
operator hashes. Any UUID library will do the same: this is `uuid.uuid5` in Python,
`uuid.NewSHA1` in Go, `uuidv5` in Node's `uuid` package.

Then pre-create the bucket, if that is what you are doing:

```sh
BUCKET=$(make -s bucket-name NAMESPACE=web NAME=docs 2>/dev/null)
aws s3api create-bucket --bucket "$BUCKET" --region eu-west-1 \
  --create-bucket-configuration LocationConstraint=eu-west-1
```

The operator adopts a bucket it finds rather than creating it — and because it did
not create it, it will never delete it. That is a feature if you are managing
buckets yourself; see [ownership](architecture.md#bucket-ownership).

Or just ask the cluster after the fact:

```sh
kubectl get staticsite docs -n web -o jsonpath='{.status.bucketName}'
```

##### Sanitisation, and the other template fields

Whatever the template renders is made into a legal bucket name, deterministically:
lowercased; every character outside `a-z`, `0-9` and `-` replaced with `-`; runs of
`-` collapsed; leading and trailing `-` trimmed; truncated to 54 characters plus a
short digest if it exceeds 63; padded if shorter than 3; and prefixed or suffixed
with `site` if it comes out looking like an IP address or using an affix S3
reserves. The default template needs none of that — `static-s3-<uuid>` is 46
characters of already-legal name — but a custom one might.

The template can also use:

| Field | Value |
| --- | --- |
| `{{ .OpaqueID }}` | the version 5 UUID, as above |
| `{{ .ShortID }}` | its first 12 hex digits, for when a long prefix leaves no room |
| `{{ .Namespace }}` | the `StaticSite`'s namespace — **goes into public DNS** |
| `{{ .Name }}` | the `StaticSite`'s name — **goes into public DNS** |
| `{{ .UID }}` | the `StaticSite`'s UID, already opaque but not knowable in advance |

Bucket names are global per provider, so add a prefix of your own if the provider
is shared: `acme-{{ .OpaqueID }}`. To go back to readable names, knowing what that
publishes, use `{{ .Namespace }}-{{ .Name }}` — no cluster UUID is needed then.

**The name is sticky.** Whatever a site resolves to is recorded in
`status.bucketName` and reused from then on, so changing the template or the
cluster UUID later does not move existing sites onto new buckets and orphan the old
ones. It only affects sites created afterwards.

Adding `spec.bucket.name` to a site that already publishes to a generated bucket is
refused, naming the bucket in use, rather than silently orphaning it. To publish to
a different bucket, create a new `StaticSite`.

#### Where the website URL comes from

There is no S3 API that reports a bucket's website endpoint. `GetBucketWebsite`
returns the index and error documents and nothing else, so the URL has to be
constructed. `s3cmd ws-info` does the same thing: it renders its configured
`website_endpoint` template, which defaults to
`http://%(bucket)s.s3-website-%(location)s.amazonaws.com/`.

`spec.bucket.websiteEndpointTemplate` is that template, per site, with the AWS
forms built in. The operator picks, in order:

1. `spec.bucket.websiteEndpointTemplate`, if set.
2. `operator.defaults.websiteEndpointTemplate`, if set.
3. For AWS (no endpoint): `http://<bucket>.s3-website-<region>.amazonaws.com` for
   the regions where AWS uses the dash form, and
   `http://<bucket>.s3-website.<region>.amazonaws.com` for the rest.
4. For a compatible endpoint: `<scheme>://<bucket>.<host>`, or
   `<endpoint>/<bucket>/` with `forcePathStyle`. This is a guess, and the template
   exists because it is often wrong.

To find the right value for a provider, ask s3cmd about a bucket you already have:

```sh
s3cmd ws-info s3://my-bucket
```

Then turn what it prints into a template by substituting the bucket and region:

```yaml
websiteEndpointTemplate: "http://{{ .Bucket }}.website-{{ .Region }}.example-object-store.com"
```

Because the URL is rendered from `spec.bucket.region`, that field has to match the
bucket's real region. A mismatch produces a wrong URL, and on AWS it also makes S3
answer requests with a redirect, so the sync fails loudly rather than silently
publishing to the wrong place.

### spec.sync

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `deleteRemoved` | bool | `true` | Remove objects no longer in the image, so the bucket mirrors the site. |
| `acl` | `public-read`\|`private` | `public-read` | Canned ACL for uploads. A bucket that refuses ACLs makes the operator drop it after the first refusal; `private` skips the attempt. |
| `keyPrefix` | string | none | Upload under a prefix, so the site serves at `/docs`. Deletion is confined to the prefix. |
| `excludes` | list of globs | none | Files neither uploaded nor deleted. A matching object already in the bucket is left alone. |
| `cacheControl` | list of `{pattern, value}` | none | `Cache-Control` per glob, first match winning. Unmatched files fall back to the *operator default*. |
| `contentTypes` | list of `{pattern, value}` | none | Override the type derived from the file extension, first match winning. |
| `defaultContentType` | string | `application/octet-stream` | For files whose extension is unknown. |
| `concurrency` | 1–64 | `8` | Objects transferred at once. |
| `retainHistory` | 0–20 | `3` | Attempts kept in `status.syncHistory`, and finished Jobs kept. |
| `job` | object | see below | The Job that runs the publish. |

Glob syntax:

| Pattern | Matches |
| --- | --- |
| `?` | one character, not a slash |
| `*` | any run of characters within one path segment |
| `**` | any run of characters, crossing slashes |
| `**/` | zero or more leading directories, so `**/*.css` matches `app.css` and `a/b/app.css` |
| `{a,b,c}` | any one of the alternatives, each of which may itself be a pattern |

So `**/*.{woff,woff2}` covers both font extensions at any depth, and
`{assets,static}/**` covers two directories. Alternatives nest. Everything else is
literal, and matching is case sensitive because object keys are.

Patterns match the path relative to the site root, not the object key, so a
`keyPrefix` does not change them.

#### spec.sync.job

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `image` | string | the operator's own image | Override the syncer image. |
| `imagePullPolicy` | string | *operator default* (`IfNotPresent`) | |
| `imagePullSecrets` | list of `{name}` | none | For the **syncer** image only, not the site image. |
| `serviceAccountName` | string | `default` | The Job needs no API access, so the default is fine. |
| `resources` | ResourceRequirements | none | `ephemeral-storage` should exceed the extracted site size. |
| `workspaceVolume` | VolumeSource | `emptyDir` | Replace the staging volume for a large site. |
| `backoffLimit` | 0–20 | `3` | |
| `activeDeadlineSeconds` | ≥30 | `3600` | |
| `ttlSecondsAfterFinished` | ≥0 | `3600` | |
| `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | | none | Standard scheduling controls. |
| `podMetadata` | `{labels, annotations}` | none | For example, to opt out of a service mesh sidecar. |

### spec.service

The `ExternalName` Service that aliases the bucket's website hostname, which is how
external-dns publishes DNS for the site.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | |
| `name` | string | the `StaticSite`'s name | |
| `externalName` | string | host of `status.websiteURL` | The hostname the Service points at. |
| `hostnames` | list | none | Rendered into the external-dns hostname annotation. |
| `annotations` | map | none | Merged onto the Service, overriding generated annotations. |
| `labels` | map | none | Merged onto the Service. |

A Service can only point at a hostname, never at a path, so a path-style website
URL cannot be expressed as a CNAME alone.

### spec.suspend

`true` stops reconciliation. In-flight Jobs finish. Use it to freeze a site rather
than deleting it, which would delete its bucket.

### status

| Field | Description |
| --- | --- |
| `conditions` | `Ready`, `BucketReady`, `SourceResolved`, `Synced` |
| `phase` | One-word summary for `kubectl get`. Branch on conditions, not this. |
| `observedGeneration` | The generation this status was computed from |
| `bucketName` | The bucket being managed |
| `bucketOwnership` | `created`, `token`, `createdAt`, `verifiedAt` — what permits deletion |
| `websiteURL` | Where a browser reaches the site |
| `serviceName` | The `ExternalName` Service, if any |
| `resolvedImage` / `resolvedDigest` | The digest-pinned reference the Job pulls |
| `lastResolveTime` / `nextResolveTime` | Registry polling. `nextResolveTime` is empty for a digest or when polling is off |
| `syncedDigest` | The digest whose contents are in the bucket |
| `syncedSpecHash` | Fingerprint of the sync settings that were in effect |
| `lastSyncTime` | When the bucket last matched the source |
| `nextSyncTime` | Earliest retry after a failure |
| `lastSyncStats` | `uploaded`, `deleted`, `unchanged`, `skipped`, `bytesUploaded`, `duration` |
| `activeSyncJob` | The Job in flight |
| `failedSyncs` | Consecutive failures, which drive the backoff |
| `syncHistory` | Recent attempts, newest first |
| `lastHandledSyncRequest` | The sync-request annotation value already acted on |

### Annotations

| Annotation | Effect |
| --- | --- |
| `web.rka.sh/sync-requested-at` | Publish now when the value changes. Overrides the retry backoff and the poll interval. |
| `web.rka.sh/force-bucket-deletion: "true"` | Skip the ownership check on deletion. Removes the only guard against destroying somebody else's data. |

```sh
kubectl annotate staticsite docs -n web \
  web.rka.sh/sync-requested-at="$(date -u +%FT%TZ)" --overwrite
```

## Chart values

Full, commented listing in [`dist/chart/values.yaml`](../dist/chart/values.yaml),
validated by [`values.schema.json`](../dist/chart/values.schema.json).

### operator.defaults

Fill in the `StaticSite` fields a site leaves unset.

| Value | Default | Fills in |
| --- | --- | --- |
| `sourcePath` | `/usr/share/nginx/html` | `spec.source.path` |
| `pullInterval` | `24h` | `spec.source.pullInterval` |
| `bucketNameTemplate` | `static-s3-{{ .OpaqueID }}` | `spec.bucket.name`; see [above](#how-a-bucket-name-is-generated) |
| `bucketNameUUIDNamespace` | empty (the `kube-system` UID) | the UUID namespace `{{ .OpaqueID }}` is derived under |
| `region` | `us-east-1` | `spec.bucket.region` |
| `endpoint` | `""` (AWS) | `spec.bucket.endpoint` |
| `forcePathStyle` | `false` | `spec.bucket.forcePathStyle` |
| `websiteEndpointTemplate` | `""` | `spec.bucket.websiteEndpointTemplate` |
| `indexDocument` | `index.html` | `spec.bucket.website.indexDocument` |
| `errorDocument` | `404.html` | `spec.bucket.website.errorDocument`. `""` means no error document. |
| `cacheControl` | `""` | The fallback for files matching no `spec.sync.cacheControl` rule |

### operator.credentials

| Value | Default | Description |
| --- | --- | --- |
| `existingSecret` | `""` | **Preferred.** A Secret you manage in the release namespace. |
| `create` | `false` | Render a Secret from `accessKeyID` / `secretAccessKey` below. Puts the key in your values file and Helm history. |
| `accessKeyID`, `secretAccessKey`, `sessionToken` | `""` | Only used when `create` is true. |
| `accessKeyIDKey`, `secretAccessKeyKey`, `sessionTokenKey` | `accessKeyID`, `secretAccessKey`, `sessionToken` | Keys within the Secret. |
| `mirrorToSiteNamespaces` | `true` | Copy the credentials into a site's namespace so its Job can read them. Off means cross-namespace sites must set `spec.bucket.credentials`. |

`create` and `existingSecret` are mutually exclusive, and the chart says so rather
than silently preferring one.

### operator.syncer

| Value | Default | Description |
| --- | --- | --- |
| `image` | `""` | Leave empty. The operator then reads its own Pod and reuses its own image, keeping manager and syncer in lockstep across an upgrade. |
| `imagePullPolicy` | `IfNotPresent` | |
| `maxExtractBytes` | `2147483648` | Largest total size a Job will unpack. |
| `maxExtractFiles` | `200000` | Largest number of files a Job will unpack. |

### operator.reconcile

| Value | Default | Description |
| --- | --- | --- |
| `maxConcurrentReconciles` | `4` | |
| `syncRetryBaseDelay` | `30s` | First retry after a failure, doubling from there. |
| `syncRetryMaxDelay` | `30m` | Retry ceiling, and the grace period before a failing teardown releases the finalizer. |
| `ownershipRecheckInterval` | `1h` | How often an idle, healthy site re-verifies its bucket configuration and ownership. |

### operator.s3 and operator.externalDNS

| Value | Default | Description |
| --- | --- | --- |
| `s3.requestChecksumWhenSupported` | `false` | Trailing CRC32 checksums on every upload. Off because several compatible providers reject them. |
| `s3.skipTLSVerify` | `false` | Development only. |
| `externalDNS.hostnameAnnotation` | `external-dns.alpha.kubernetes.io/hostname` | Where `spec.service.hostnames` are rendered. `""` renders nothing. |

### The rest

Standard Kubebuilder chart values: `manager.*` (image, replicas, resources,
scheduling, security contexts), `rbac.namespaced`, `rbac.helpers.enabled`,
`serviceAccount.*`, `crd.enabled`, `crd.keep`, `metrics.*`, `certManager.enabled`,
`prometheus.enabled`, `networkPolicy.enabled`.

`crd.keep` defaults to `true`. Deleting the CRD deletes every `StaticSite`, and
with them their buckets.

## Manager flags

The chart renders these; they are listed for a hand-written Deployment or for
`make run`.

```
--default-source-path
--default-pull-interval
--default-bucket-name-template
--bucket-name-uuid-namespace
--default-region
--default-endpoint
--default-force-path-style
--default-website-endpoint-template
--default-index-document
--default-error-document
--default-cache-control

--external-dns-hostname-annotation

--syncer-image
--syncer-image-pull-policy
--max-extract-bytes
--max-extract-files

--default-credentials-secret                    # "name" or "namespace/name"
--default-credentials-access-key-id-key
--default-credentials-secret-access-key-key
--default-credentials-session-token-key
--mirror-default-credentials

--s3-request-checksum-when-supported
--s3-skip-tls-verify

--sync-retry-base-delay
--sync-retry-max-delay
--ownership-recheck-interval
--max-concurrent-reconciles
```

Plus the standard controller-runtime flags (`--leader-elect`,
`--metrics-bind-address`, `--health-probe-bind-address`, `--zap-log-level` and so
on). `--help` lists everything.

Two environment variables matter: `POD_NAME` and `POD_NAMESPACE`, both set by the
chart from the downward API. They let the operator discover its own image for sync
Jobs and resolve an unqualified `--default-credentials-secret`.
