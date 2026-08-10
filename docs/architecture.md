# Architecture

How a publish works, why the work is split the way it is, and what the operator
will and will not do to a bucket.

## Contents

- [The shape of it](#the-shape-of-it)
- [A reconcile pass](#a-reconcile-pass)
- [Why publishing runs as a Job](#why-publishing-runs-as-a-job)
- [How the syncer decides what to upload](#how-the-syncer-decides-what-to-upload)
- [Bucket ownership](#bucket-ownership)
- [Security model](#security-model)
- [Failure handling](#failure-handling)
- [Compatibility with non-AWS providers](#compatibility-with-non-aws-providers)
- [Code layout](#code-layout)

## The shape of it

Two binaries, both in the same image.

**The manager** watches `StaticSite` resources. It does the small, quick work: it
resolves an image tag to a digest with one HTTP `HEAD`, keeps the bucket's
configuration correct, maintains the `ExternalName` Service, and decides when a
publish is due.

**The syncer** runs as a Kubernetes Job, one per publish. It pulls the image,
extracts a directory, compares it to the bucket, and uploads and deletes objects.

They ship together because they must not drift apart: the manager reads its own
Pod to discover the image it is running and uses that same image for its Jobs, so
an upgrade moves both at once. `operator.syncer.image` can override it, and the
default of leaving it empty is the right one.

## A reconcile pass

Stages run in dependency order, and each owns a condition. A site that is stuck
says exactly where.

| Order | Stage | Condition | What it does |
| --- | --- | --- | --- |
| 1 | Credentials | `BucketReady` (`CredentialsError`) | Reads the access key from a Secret, mirroring the operator's own into the site's namespace if needed |
| 2 | Bucket | `BucketReady` | Creates the bucket, writes the ownership token, applies website configuration, policy and tags |
| 3 | Service | `Ready` | Maintains the `ExternalName` Service |
| 4 | Source | `SourceResolved` | Resolves the image reference to a digest |
| 5 | Sync | `Synced` | Starts a Job when the bucket is stale, and records the outcome of one that finished |

`Ready` is the aggregate. `status.phase` is a one-word summary derived from the
conditions for `kubectl get`, and is never the source of truth.

Status is written once per pass, and only when something actually changed. That
matters more than it sounds: an unconditional status write produces an update
event, which triggers another reconcile, which writes status again.

### When a publish happens

Three triggers, and nothing else:

1. **The resolved digest changed.** A tagged reference is re-resolved every
   `spec.source.pullInterval` (24h by default); a digest reference never is.
2. **The sync settings changed.** The fields that decide what gets uploaded and
   with which headers are fingerprinted into `status.syncedSpecHash`. Adding a
   `cacheControl` rule republishes; raising a Job memory limit does not.
3. **Somebody asked.** Setting `web.rka.sh/sync-requested-at` to a new value
   publishes immediately, and overrides a retry backoff.

An idle site costs one manifest `HEAD` plus, on the reconcile that follows, one
bucket listing. That is what makes a daily poll affordable at any number of sites.

## Why publishing runs as a Job

Doing the copy inside the controller would be simpler, and wrong for this workload:

- **It is the part that handles an untrusted image.** The Job runs unprivileged,
  non-root, read-only, and with no Kubernetes API access at all. The controller
  cannot make those guarantees about itself.
- **Sites are not the same size.** A Job carries its own CPU, memory and
  ephemeral-storage limits, and its own deadline, per site.
- **A big extraction should not be on the controller's heap**, and should not
  block other sites' reconciles.
- **Kubernetes already retries batch work**, with a backoff limit and a TTL.

The cost is a channel back. The Job writes a JSON report to its container
termination message, which the operator reads off the Pod status — so the Job needs
no ServiceAccount token, no RBAC, and no route to the API server. `uploaded`,
`deleted` and `unchanged` in `status.lastSyncStats` come from there.

Jobs are found by label (`web.rka.sh/staticsite-uid`) rather than only by the name
in status, and carry the digest and settings fingerprint they ran with as
annotations. So an operator restarted mid-publish adopts the running Job and
records its outcome correctly, instead of starting a second one.

## How the syncer decides what to upload

```
pull image by digest → extract spec.source.path → hash every file
                                                        │
                     list the bucket under keyPrefix ────┤
                                                        ▼
                                          upload / skip / delete
```

**Content, not timestamps.** Every file extracted from an image carries the
image's build time, so a timestamp says nothing about whether the contents changed.
The syncer hashes each local file and compares:

- If the object's ETag is a plain MD5 — which it is for anything uploaded in one
  part — that settles it with no extra request.
- Otherwise the syncer reads the hash it stored itself in object metadata
  (`x-amz-meta-static-s3-md5`), which makes the comparison exact for multipart
  objects too. An object uploaded by something else has no such metadata and is
  replaced.

**Content types from extensions only.** Never by sniffing the file. A built-in
table is consulted before Go's, because Go's is seeded from `/etc/mime.types` and
would otherwise make the same site upload differently from two machines — and
because a missing entry for something as ordinary as `.webp` or `.wasm` leaves a
file unusable in a browser.

**Mirroring.** With `deleteRemoved` on, objects under the prefix that are not in
the image are deleted. Three things are never deleted: the ownership marker,
anything matching `spec.sync.excludes` (an excluded file was never uploaded, so
such an object belongs to whoever put it there), and anything outside
`spec.sync.keyPrefix`.

**Extraction treats the image as hostile.** Entries are checked to land inside the
staging directory; absolute or climbing symlinks are skipped; hard links pointing
outside the tree are skipped; device, FIFO and socket entries are ignored; file
modes are fixed rather than taken from the image, so a setuid bit cannot survive;
and total size and file count are capped (`--max-extract-bytes`,
`--max-extract-files`).

## Bucket ownership

`spec.bucket.deletionPolicy` defaults to `Delete`, so removing a `StaticSite`
removes its bucket. That is a destructive default, and this is the mechanism that
makes it safe.

When the operator creates a bucket it generates a random 256-bit token and writes
it in two places:

- **The bucket's tags** — `static-s3-operator/ownership-token`, along with the
  owning resource and its UID. Tags are private and cost no extra request, since
  the operator manages the tag set anyway.
- **`status.bucketOwnership.token`** on the resource.

Deletion requires both to agree. Anything less is a refusal:

| What the operator finds | Result |
| --- | --- |
| Token in the bucket matches the token in status | Deleted |
| Bucket has no ownership record | **Retained** — adopted, never created here |
| Bucket's token belongs to another resource | **Retained** |
| Bucket records a different UID for the same name | **Retained** — a recreated resource is not the original |
| Status has no token | **Retained** |

Reconciling the tag set always re-includes the ownership tags, so a tag update can
never drop the token and quietly make a bucket undeletable. A site tag using the
reserved `static-s3-operator/` prefix is discarded rather than applied, so a
`StaticSite` cannot forge a token.

Recovery works in the other direction too: a bucket tagged with this resource's own
UID is recognised as its own even when status does not say so. That covers a lost
status write, and the ordinary case of a reconcile reading the resource from a cache
that has not caught up a moment after the bucket was created.

**Providers without bucket tagging** get a marker object at
`.static-s3-operator-ownership.json` instead. A public bucket makes that object
readable, so it holds a random token and nothing else — no cluster, namespace or
resource names. The token is not a credential; it only proves the bucket the
operator is looking at is the one it created.

The override is `web.rka.sh/force-bucket-deletion: "true"`, which skips the check
entirely. It exists for the case where you know better, and it removes the only
guard against destroying somebody else's data.

## Security model

**The operator's credentials.** Read from a Secret on every reconcile, and never
taken from the ambient environment — no environment variables, no instance profile,
no projected token. A site silently using the node's identity instead of the Secret
it names would be very hard to notice.

**Secrets are not cached.** The manager's client is configured not to cache
`Secret` objects, so the operator does not watch every Secret in the cluster to
read a handful of credentials.

**Secrets never pass through a Job's spec.** The Job references the Secret with a
`secretKeyRef`; the operator does not copy values into environment variables. The
standard `AWS_ACCESS_KEY_ID` names are used so nothing bespoke is needed.

**Cross-namespace mirroring is visible and optional.** A Job can only read Secrets
in its own namespace, so the operator's default credentials are copied into a
site's namespace as a Secret owned by that `StaticSite`. Set
`operator.credentials.mirrorToSiteNamespaces=false` to forbid it, after which
cross-namespace sites must bring their own credentials.

**Sync Pods are minimal.** `runAsNonRoot`, `runAsUser: 65532`,
`readOnlyRootFilesystem`, all capabilities dropped, `seccompProfile:
RuntimeDefault`, no privilege escalation, `automountServiceAccountToken: false`.
Writable space is two `emptyDir` volumes: the staging area and `/tmp`.

**The public-read policy grants only `s3:GetObject`.** Not `ListBucket`, so the
bucket's contents cannot be enumerated by an anonymous caller.

**Nothing in the image is executed.** The image is a file archive as far as this
operator is concerned. `FROM scratch` works fine.

## Failure handling

**Configuration errors do not retry fast.** A missing Secret or an image that does
not exist is reported and then waited on, because the operator is waiting for a
person. A watch event on the resource brings it back immediately when the spec
changes.

**Everything else backs off exponentially** on `status.failedSyncs`, from
`syncRetryBaseDelay` to `syncRetryMaxDelay`.

**A failed publish does not hot-loop.** `status.nextSyncTime` records the earliest
next attempt, because a finished Job produces a watch event that would otherwise
start the next attempt immediately, whatever the backoff said.

**Teardown gives up rather than wedging.** If S3 cannot be reached to delete a
bucket — a credentials Secret deleted first, say — the operator retries, and after
`syncRetryMaxDelay` releases the finalizer with a warning event naming the bucket
it left behind. A namespace stuck in `Terminating` with no obvious cause is worse
than a leaked bucket that is named in an event.

**A bucket claimed by another resource is a hard stop.** Two sites publishing into
one bucket would silently delete each other's files, so the operator refuses rather
than guessing. Sharing a bucket deliberately is what `spec.sync.keyPrefix` is for.

## Compatibility with non-AWS providers

Every optional part of the S3 API is treated as optional:

| Behaviour | Why |
| --- | --- |
| `PutBucketWebsite` failing is not fatal | Several providers do not implement it; the bucket still serves the site, and the `BucketReady` condition explains what is missing |
| `PutPublicAccessBlock` failing is not fatal | An AWS-specific feature; a provider without it has nothing to clear |
| `PutBucketTagging` failing falls back to a marker object | Ownership has to be recordable somewhere |
| An ACL rejection drops the ACL for the rest of the sync | AWS disables ACLs on buckets created since April 2023 |
| `CreateBucket` retries without a location constraint | Some providers accept only the bare form |
| Multi-object delete falls back to one request per object | Some providers do not implement the batch API |
| Version listing is skipped when unsupported | Only current objects exist to remove |
| Checksums are requested only where required | The SDK's newer trailing-CRC32 default is rejected by several providers |
| Bucket policies are compared after canonicalisation | Providers rewrite `"Principal": "*"` and single-element arrays as they store them, which would otherwise look like drift on every reconcile |

## Code layout

| Path | Contents |
| --- | --- |
| `api/v1alpha1/` | The `StaticSite` API |
| `cmd/main.go` | The manager |
| `cmd/syncer/` | The syncer, whose settings are all flags so a publish is legible in `kubectl describe job` |
| `internal/config/` | Cluster-wide defaults, and resolving a site against them in one place |
| `internal/controller/` | The reconcile loop, credential resolution, the Job and Service builders |
| `internal/provisioner/` | The seam between the controller and S3, so the reconcile loop is testable without an object store |
| `internal/registry/` | Digest resolution, and a keychain built from pull Secrets |
| `internal/s3/` | Bucket lifecycle, ownership, policies, and the provider-quirk error classifiers |
| `internal/syncer/` | Image extraction, the diff, and the upload |
| `test/e2e/` | The full loop in a Kind cluster |
| `test/examples/` | Every example applied to a real API server |
