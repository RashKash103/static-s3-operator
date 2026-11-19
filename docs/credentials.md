# S3 credentials

The operator needs one S3 access key. With it, it creates buckets, configures them
for website hosting, uploads and deletes objects, and deletes buckets it created.
This page has the access policy it needs and the commands to create a key with
exactly that policy, for AWS and for S3-compatible providers.

## Contents

- [What the operator does with the key](#what-the-operator-does-with-the-key)
- [The access policy](#the-access-policy)
- [Giving the key to the operator](#giving-the-key-to-the-operator)
- [AWS](#aws)
- [S3-compatible providers](#s3-compatible-providers)
  - [Which providers are tested](#which-providers-are-tested)
  - [Garage](#garage)
- [Per-site credentials](#per-site-credentials)
- [Rotating the key](#rotating-the-key)
- [Reducing what the key can do](#reducing-what-the-key-can-do)

## What the operator does with the key

| Operation | Why | S3 actions |
| --- | --- | --- |
| Create a bucket | One bucket per `StaticSite` | `s3:CreateBucket` |
| Check a bucket | Is it there, is it reachable | `s3:ListBucket`, `s3:GetBucketLocation` |
| Website hosting | Serve `index.html` for a directory request | `s3:GetBucketWebsite`, `s3:PutBucketWebsite`, `s3:DeleteBucketWebsite` |
| Public read | Let a browser fetch the objects | `s3:GetBucketPolicy`, `s3:PutBucketPolicy`, `s3:DeleteBucketPolicy` |
| Clear the public access block | AWS blocks public policies on new buckets by default | `s3:*BucketPublicAccessBlock` |
| Record ownership | Prove the operator created the bucket before deleting it | `s3:GetBucketTagging`, `s3:PutBucketTagging` |
| Upload the site | Publish the files | `s3:PutObject`, `s3:PutObjectAcl`, `s3:GetObject` |
| Mirror the site | Remove files that are no longer in the image | `s3:DeleteObject` |
| Delete a bucket | Tear a site down with its `StaticSite` | `s3:DeleteBucket`, `s3:DeleteObject`, `s3:ListBucketVersions` |

The key is only ever read from a Kubernetes Secret. It is used by the operator
itself, and by each sync Job through a `secretKeyRef`, so the values are never
written into a Job's command line or environment in plain text.

## The access policy

[`docs/policies/operator-policy.json`](policies/operator-policy.json) is the full
policy. It is scoped to `static-s3-*`, which is the prefix every generated bucket
name carries by default, so the key cannot touch anything else in the account:

```json
{
  "Version": "2012-10-17",
  "Id": "static-s3-operator",
  "Statement": [
    {
      "Sid": "ManageSiteBuckets",
      "Effect": "Allow",
      "Action": [
        "s3:CreateBucket",
        "s3:DeleteBucket",
        "s3:ListBucket",
        "s3:ListBucketVersions",
        "s3:ListBucketMultipartUploads",
        "s3:GetBucketLocation",
        "s3:GetBucketPolicy",
        "s3:PutBucketPolicy",
        "s3:DeleteBucketPolicy",
        "s3:GetBucketWebsite",
        "s3:PutBucketWebsite",
        "s3:DeleteBucketWebsite",
        "s3:GetBucketTagging",
        "s3:PutBucketTagging",
        "s3:GetBucketPublicAccessBlock",
        "s3:PutBucketPublicAccessBlock",
        "s3:DeleteBucketPublicAccessBlock"
      ],
      "Resource": "arn:aws:s3:::static-s3-*"
    },
    {
      "Sid": "ManageSiteObjects",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:PutObjectAcl",
        "s3:GetObject",
        "s3:DeleteObject",
        "s3:DeleteObjectVersion",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": "arn:aws:s3:::static-s3-*/*"
    }
  ]
}
```

A resource prefix is the single most useful restriction here. `s3:DeleteBucket`
is in the policy because deleting a `StaticSite` deletes its bucket; scoping the
resource is what stops a mistyped `spec.bucket.name` from reaching a bucket that
matters.

For the prefix to cover every site, it has to match how buckets get named. Sites
that leave `spec.bucket.name` out are named by
[`operator.defaults.bucketNameTemplate`](configuration.md#how-a-bucket-name-is-generated),
which defaults to `static-s3-{{ .OpaqueID }}` — so the default prefix is
`static-s3-`, and `arn:aws:s3:::static-s3-*` covers every generated bucket without
any further configuration.

To use a prefix of your own, change both together:

```yaml
operator:
  defaults:
    # Matches "arn:aws:s3:::my-sites-*" in the policy above.
    bucketNameTemplate: "my-sites-{{ .OpaqueID }}"
```

Then a site that names its own bucket outside that prefix fails loudly at
`CreateBucket` rather than succeeding somewhere unintended.

The operator's own ownership check is the second line of defence — it refuses to
delete a bucket it cannot prove it created — but a scoped key is the one that does
not depend on the operator being correct.

The `arn:aws:s3:::` form is what S3-compatible providers use in policy documents
too, whatever their own naming.

## Giving the key to the operator

Create the Secret yourself and point the chart at it. This keeps the key out of
your values file and out of the Helm release:

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

If you manage secrets with External Secrets, Sealed Secrets, SOPS or a CSI driver,
have it produce a Secret with those two keys and reference it the same way. The key
names are configurable:

```yaml
operator:
  credentials:
    existingSecret: my-s3-credentials
    accessKeyIDKey: AWS_ACCESS_KEY_ID
    secretAccessKeyKey: AWS_SECRET_ACCESS_KEY
```

For temporary credentials, add a `sessionToken` key. The operator sends it when
present. Remember that the site will stop syncing when the token expires, so
long-lived keys suit an operator better.

`operator.credentials.create=true` renders the Secret from values instead. That
is convenient for a first look and for CI, and it puts the key in your values file
and in the Helm release history, so prefer `existingSecret` for anything real.

### Sites in other namespaces

A sync Job can only read Secrets in its own namespace. When a `StaticSite` lives
outside the operator's namespace, the operator copies the credentials there as a
Secret owned by that `StaticSite`, which is garbage collected with it.

That copies a cluster-wide credential into a tenant namespace, which may not be
what you want. Turn it off:

```yaml
operator:
  credentials:
    mirrorToSiteNamespaces: false
```

Then every `StaticSite` outside the operator's namespace must set
`spec.bucket.credentials` to a Secret in its own namespace. See
[Per-site credentials](#per-site-credentials).

## AWS

Create a user with only this policy, then take a key for it:

```sh
aws iam create-user --user-name static-s3-operator

# The policy is already scoped to static-s3-*, the default bucket name prefix.
aws iam put-user-policy \
  --user-name static-s3-operator \
  --policy-name static-s3-operator \
  --policy-document file://docs/policies/operator-policy.json

aws iam create-access-key --user-name static-s3-operator
```

If you set a prefix of your own, substitute it into the policy first, and set the
matching `bucketNameTemplate`:

```sh
BUCKET_PREFIX=my-sites
sed "s/static-s3-/${BUCKET_PREFIX}-/" docs/policies/operator-policy.json > /tmp/policy.json
```

The last command prints the only copy of the secret access key. Put it straight
into the Secret:

```sh
kubectl create secret generic my-s3-credentials \
  --namespace static-s3-operator-system \
  --from-literal=accessKeyID=AKIAEXAMPLE \
  --from-literal=secretAccessKey=EXAMPLESECRET
```

### IRSA and other ambient credentials

The operator does not use ambient credentials. It signs every request with the key
from the Secret, and never falls back to environment variables, an instance profile
or a projected service account token.

That is deliberate: a site quietly using the node's identity instead of the Secret
it names is very hard to notice, and it means "which credentials published this
bucket" always has one answer. If you use IRSA today, mint an access key for the
operator's role and put it in a Secret.

### ACLs are disabled on new AWS buckets

Buckets created since April 2023 have object ACLs disabled, so an upload asking
for `public-read` is rejected. The operator notices the first rejection, drops the
ACL, and carries on: public access comes from the bucket policy, not from
per-object grants. It says so in an event.

To skip the pointless first attempt, set `spec.sync.acl: private` on the site.

## S3-compatible providers

Everything works the same way against any S3-compatible object store. Three
settings usually need attention:

```yaml
operator:
  defaults:
    endpoint: https://us-east-1.example-object-store.com
    # Most compatible providers address buckets as endpoint/bucket.
    forcePathStyle: true
    region: us-east-1
    # Only needed when the website hostname differs from the API endpoint.
    websiteEndpointTemplate: "http://{{ .Bucket }}.website-{{ .Region }}.example-object-store.com"
```

Create the key in the provider's console or CLI. Most offer either a full-access
key or a policy editor that accepts the document above; if the provider has no
policy editor, a key scoped to the buckets you intend to use is the next best
thing.

For a store you administer with a MinIO-compatible client:

```sh
mc admin user add myprovider static-s3-operator "$(openssl rand -base64 24)"
mc admin policy create myprovider static-s3-operator docs/policies/operator-policy.json
mc admin policy attach myprovider static-s3-operator --user static-s3-operator
```

### What compatible providers often do not implement

The operator treats each of these as absent rather than broken, so a provider
missing any of them still works:

| Feature | If it is missing |
| --- | --- |
| `PutBucketWebsite` | The site is served, but a request for a directory is not rewritten to `index.html`. The `BucketReady` condition says so. Link to files directly, or put a CDN in front that does the rewrite. |
| `PutPublicAccessBlock` | Nothing to clear. The bucket policy alone makes the site readable. |
| `PutBucketTagging` | Ownership is recorded in a marker object at `.static-s3-operator-ownership.json` instead. It holds a random token and nothing else — no cluster, namespace or resource names — because a public bucket makes it readable. |
| Object ACLs | The first upload is refused, the operator drops the ACL and continues. Set `spec.sync.acl: private` to skip the attempt. |
| Bucket versioning | Emptying a bucket removes current objects only, which is all there is. |

**Bucket policies are the exception.** They are how `publicRead` makes a site
readable, so a provider without them fails `BucketConfigurationFailed` with
`NotImplemented` on `PutBucketPolicy`. Set `spec.bucket.publicRead: false` there,
and make the bucket public the provider's own way. [Garage](#garage) is the
common case, and has a worked setup below.

### Trailing checksums

The AWS SDK's newer default is to attach a trailing CRC32 checksum to every
upload. Several compatible providers reject those requests outright, so the
operator asks for checksums only where the API requires them. If your provider
supports them and you want them:

```yaml
operator:
  s3:
    requestChecksumWhenSupported: true
```

### Which providers are tested

The integration and end-to-end suites run against
[RustFS](https://github.com/rustfs/rustfs), pinned in the `Makefile` and in
`test/e2e/testdata/objectstore.yaml`. RustFS implements everything the operator
uses, so no test is skipped there. To check another provider, point the
integration suite at it:

```sh
S3_TEST_ENDPOINT=https://s3.example.com S3_TEST_ACCESS_KEY=... S3_TEST_SECRET_KEY=... \
  go test -tags=integration -count=1 ./internal/s3/... ./internal/syncer/...
```

Tests for a feature the provider lacks are skipped with a message saying which.

### Garage

[Garage](https://garagehq.deuxfleurs.fr) is **partially supported**. It can serve
a site, but not with the defaults, and only through a bucket an administrator
creates. This was measured against Garage v2.5.0.

| Operator feature | On Garage |
| --- | --- |
| Creating, listing, emptying and deleting buckets | Works |
| Uploading, diffing and deleting objects | Works. The object ACL header is accepted and ignored. |
| Website configuration (`PutBucketWebsite`) | Works. Index documents, the `/dir` → `/dir/` redirect and a custom error document all behave. |
| **Public read (`spec.bucket.publicRead`, the default)** | **Not supported.** Garage implements no bucket policies, and `PutBucketPolicy` fails with `501 NotImplemented`. A site left at the default is stuck at `BucketConfigurationFailed`. |
| **Custom `spec.bucket.policy`** | **Not supported**, for the same reason. |
| Anonymous reads on the S3 API | Never. Garage serves anonymous requests only from its separate website endpoint (`[s3_web]`, port 3902 by default), which chooses the bucket by `Host` header. |
| **Buckets the operator creates** | **Not reachable as a website.** Garage gives a bucket created through the S3 API only a local alias, private to the key that created it, and its website endpoint looks buckets up by global alias. An administrator has to add one (`garage bucket alias --global`) before the site can be served. |
| Bucket tagging | Not supported. Ownership is recorded in the [marker object](#what-compatible-providers-often-do-not-implement) instead. |
| Public access blocks, versioning | Not supported, and not needed. |

**What works** is to let Garage make the bucket public through its website
endpoint, and have the operator adopt a bucket an administrator created:

```sh
# On the Garage side, once per site.
garage bucket create acme-docs                 # created by the CLI, so it has a global alias
garage bucket website --allow acme-docs
garage key create static-s3-operator           # or reuse an existing key
garage bucket allow --read --write --owner acme-docs --key static-s3-operator
```

`--owner` is needed, because configuring the website is an owner operation.

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
    endpoint: http://garage.storage.svc:3900
    region: garage                             # Garage's s3_region
    forcePathStyle: true
    # Garage publishes the site from [s3_web], not from the S3 API.
    websiteEndpointTemplate: "http://{{ .Bucket }}.web.garage.example.com"
    create: false                              # adopt the bucket made above
    publicRead: false                          # Garage has no bucket policies
  sync:
    acl: private                               # Garage ignores ACLs; skip asking
```

The same file is in [`examples/garage.yaml`](../examples/garage.yaml). With
`create: false`, the bucket is adopted: deleting the `StaticSite` leaves it in
place, whatever `deletionPolicy` says.

Point a hostname at Garage's website endpoint whose name matches the bucket. The
`root_domain` in `[s3_web]` decides the suffix: with
`root_domain = ".web.garage.example.com"`, bucket `acme-docs` is served at
`acme-docs.web.garage.example.com`. A host that is exactly the bucket name also
works. The `ExternalName` Service follows `websiteEndpointTemplate`, so
external-dns can publish that alias as usual.

## Per-site credentials

A `StaticSite` can bring its own key, which is how you point different sites at
different accounts or different providers:

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
    endpoint: https://us-east-1.example-object-store.com
    forcePathStyle: true
    credentials:
      secretRef:
        name: web-team-s3
      # Optional; these are the defaults.
      accessKeyIDKey: accessKeyID
      secretAccessKeyKey: secretAccessKey
```

The Secret must be in the same namespace as the `StaticSite`. Nothing is mirrored
in this case, and the operator's own credentials are not used at all.

## Rotating the key

Update the Secret. Nothing else is needed:

```sh
kubectl create secret generic my-s3-credentials \
  --namespace static-s3-operator-system \
  --from-literal=accessKeyID=AKIANEW \
  --from-literal=secretAccessKey=NEWSECRET \
  --dry-run=client -o yaml | kubectl apply -f -
```

The operator reads the Secret on every reconcile, so the next pass uses the new
key. Mirrored copies in site namespaces are refreshed by the same pass. To make
that happen immediately rather than on the next poll:

```sh
kubectl annotate staticsite --all --all-namespaces \
  web.rka.sh/sync-requested-at="$(date -u +%FT%TZ)" --overwrite
```

Revoke the old key once every site has reconciled — check that each one still
reports `Ready`.

## Reducing what the key can do

**Adopt buckets instead of creating them.** If you would rather create buckets
yourself, use
[`operator-policy-adopt-only.json`](policies/operator-policy-adopt-only.json),
which drops `CreateBucket`, `DeleteBucket` and the public access block entirely,
and set `spec.bucket.create: false` on each site. A bucket the operator did not
create is never deleted, so `spec.bucket.deletionPolicy` has no effect on it.

**Take away deletion.** Remove `s3:DeleteBucket` from the policy and set
`spec.bucket.deletionPolicy: Retain`. Deleting a `StaticSite` then leaves the
bucket behind for you to remove.

**Do not make buckets public.** Set `spec.bucket.publicRead: false` and put a CDN
in front that reads the bucket with its own credentials. You can then also drop
`s3:PutObjectAcl` and the public access block actions.

**One key per team.** Give each team a key scoped to its own bucket prefix and
have their `StaticSite` resources name it with `spec.bucket.credentials`. Turn
`operator.credentials.mirrorToSiteNamespaces` off so the operator's own key never
reaches a tenant namespace.
