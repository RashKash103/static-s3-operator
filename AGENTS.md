# static-s3-operator - AI Agent Guide

## This project in particular

The generic Kubebuilder guidance below still applies. Read this part first, because
it is what is specific to this repository.

### What it does

A `StaticSite` names a container image and a directory inside it, and an S3 bucket.
The operator makes the bucket serve that directory as a website: it creates the
bucket, configures website hosting and a public-read policy, and mirrors the files.
When the image tag starts pointing at a new digest, it republishes.

### Two binaries, one image

`cmd/main.go` is the manager. `cmd/syncer/main.go` is the syncer, which runs as a
Job, one per publish.

They ship in the same image on purpose, and the manager discovers its own image by
reading its own Pod (`internal/config/selfimage.go`) so the two cannot drift apart
across an upgrade. If you add a Dockerfile stage or change the entrypoint, keep
`/manager` and `/syncer` both present at the image root.

The Job has **no Kubernetes API access at all** — no token, no RBAC. It reports back
through its container termination message (`internal/syncer/report.go`). Do not
reach for the API from the syncer; add a field to the report instead.

### Things that look like bugs but are deliberate

Before "fixing" any of these, read the comment that explains it. Each one is load
bearing.

- **Some fields have no `+kubebuilder:default`.** `spec.source.path`,
  `spec.source.pullInterval`, `spec.bucket.region`, `spec.bucket.endpoint` and the
  website documents are defaulted by the operator, not the CRD, so the Helm chart
  can choose them once for a cluster. Adding a CRD default to one of these takes
  that away. See `internal/config`.
- **Website configuration and public access blocks failing is not fatal.** Several
  S3-compatible providers reject `PutBucketWebsite` and `PutPublicAccessBlock` with
  a 400. Making those fatal makes the operator unusable on those providers.
- **Checksums are requested only where the API requires them.** The AWS SDK's
  newer trailing-CRC32 default is rejected by several compatible providers.
- **Bucket policies are compared after canonicalisation.** Providers rewrite
  `"Principal": "*"` and single-element arrays as they store them; a naive
  comparison rewrites the policy on every reconcile.
- **Files are compared by content hash, never by timestamp.** Everything extracted
  from an image carries the image's build time.
- **Content types come from a built-in extension table before Go's.** Go's is
  seeded from `/etc/mime.types`, so the same site would upload differently from two
  machines.
- **`status.nextSyncTime` exists because a finished Job produces a watch event.**
  Without it the retry backoff is ignored and a failing site becomes a
  Job-creating loop.
- **The provisioner recovers ownership from a bucket tagged with the site's own
  UID.** A reconcile immediately after bucket creation can read the resource from a
  cache that has not caught up, and would otherwise mistake the site's own bucket
  for somebody else's.
- **`patchStatus` skips a no-op write.** An unconditional status write produces an
  update event, which reconciles again, which writes status again.

### Bucket ownership is the safety property

`spec.bucket.deletionPolicy` defaults to `Delete`, so deleting a `StaticSite`
deletes its bucket. The guard is a random token written into the bucket's tags (or
a marker object on a provider without tagging) and recorded in
`status.bucketOwnership`. Deletion requires both to agree.

If you touch `internal/s3/ownership.go`, `internal/provisioner/s3.go` or the
deletion path in the controller, keep these invariants:

- A bucket with no ownership record is **never** deleted, whatever the policy says.
- Reconciling the tag set always re-includes the ownership tags, so a tag update
  cannot drop the token.
- A site tag using the reserved `static-s3-operator/` prefix is discarded, so a
  `StaticSite` cannot forge a token.
- The marker object carries a token and nothing else: no cluster, namespace or
  resource names, because a public bucket makes it readable.
- Teardown that keeps failing releases the finalizer after
  `--sync-retry-max-delay` with a warning naming the leaked bucket, rather than
  leaving a namespace stuck in `Terminating`.

The end-to-end suite tests the important half of this: a bucket the operator did
not create survives deletion of the resource pointing at it. Keep that spec.

### Testing layers

| Command | What it covers | Needs |
| --- | --- | --- |
| `make test` | Unit tests, the controller suite against a real API server, and every example applied to one | envtest binaries |
| `make test-integration` | `internal/s3` and `internal/syncer` against a real S3 API | Docker |
| `make test-e2e` | The whole loop: image build, in-cluster registry and object store, a generated sample site, and an anonymous HTTP fetch of the result | Docker, Kind |

The controller suite replaces exactly two things: the S3 provider
(`provisioner.Provisioner`) and the registry resolver. Everything else is the
production path. Keep that seam — do not add a mock for the reconcile loop itself.

envtest runs no Job controller, so a test that wants a publish to finish sets the
Job's status itself. The API server enforces the real two-step transition
(`SuccessCriteriaMet` then `Complete`, `FailureTarget` then `Failed`); see
`finishJob` in `internal/controller/staticsite_controller_test.go`.

Integration tests are behind the `integration` build tag and skip without
`S3_TEST_ENDPOINT`. They run against a real server on purpose: what they check is
how a provider behaves, and a mock built from this code's own assumptions would
agree with those assumptions and prove nothing.

`test/examples` applies every file in `examples/` and `config/samples/` to a real
API server. If you add an example, it is checked automatically; if you change the
API, a stale example fails the build.

### Regenerating things

```sh
make manifests generate   # CRDs, RBAC, deepcopy
make schemas              # standalone JSON Schemas for the CRDs, into dist/schemas
make helm-lint            # lint and render the chart
```

CI fails if any of these leaves the tree dirty.

**The Helm chart is generated**, and regenerating it overwrites the plugin's own
files:

```sh
kubebuilder edit --plugins=helm/v2-alpha --force
```

Afterwards, re-apply the one hand-written hunk in
`dist/chart/templates/manager/manager.yaml`:

```
{{- include "static-s3-operator.operatorArgs" . | nindent 8 }}
```

Everything else specific to this project lives in files the plugin does not
generate — `templates/_operator.tpl`, `templates/operator/credentials.yaml`,
`values.schema.json` — and the `operator:` block in `values.yaml`, which is worth
checking survived. `make helm-chart` prints this reminder.

### Adding a setting

A new cluster-wide default touches four places, in this order:

1. `internal/config/config.go` — the field, its flag in `BindFlags`, and validation
   in `Validate`.
2. `internal/config/effective.go` — resolve it in `Effective`, and decide whether it
   belongs in `SyncSpecHash`. It belongs there only if changing it changes what gets
   uploaded; putting an unrelated setting in the hash re-uploads every site in the
   cluster when it changes.
3. `dist/chart/templates/_operator.tpl` and `values.yaml`, plus `values.schema.json`.
4. `docs/configuration.md`.

A new `StaticSite` field additionally needs the type in `api/v1alpha1`,
`make manifests generate schemas`, and usually an example.

### Conventions

- Comments explain **why**, not what. The code says what it does; a comment earns
  its place by recording the reason, the trade-off, or the failure it prevents.
- Errors say what failed and name the thing: `create bucket %q: %w`.
- Log messages follow the Kubernetes style guide (capitalised, no trailing period,
  past tense, balanced key-value pairs).
- Provider quirks are classified in `internal/s3/errors.go`, not spread through
  call sites. `IsUnsupportedRequest` is deliberately broad and used **only** for
  website configuration and public access blocks; do not widen its use to calls
  where a malformed request could be this code's own fault.
- Tests name the behaviour, not the function: `TestSyncSkipsUnchangedFiles`, not
  `TestSync2`.

## Project Structure

**Single-group layout (default):**
```
cmd/main.go                    Manager entry (registers controllers/webhooks)
api/<version>/*_types.go       CRD schemas (+kubebuilder markers)
api/<version>/zz_generated.*   Auto-generated (DO NOT EDIT)
internal/controller/*          Reconciliation logic
internal/webhook/*             Validation/defaulting (if present)
config/crd/bases/*             Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/*               Example CRs (edit these)
Makefile                       Build/test/deploy commands
PROJECT                        Kubebuilder metadata Auto-generated (DO NOT EDIT)
```

**Multi-group layout** (for projects with multiple API groups):
```
api/<group>/<version>/*_types.go       CRD schemas by group
internal/controller/<group>/*          Controllers by group
internal/webhook/<group>/<version>/*   Webhooks by group and version (if present)
```

Multi-group layout organizes APIs by group name (e.g., `batch`, `apps`). Check the `PROJECT` file for `multigroup: true`.

**To convert to multi-group layout:**
1. Run: `kubebuilder edit --multigroup=true`
2. Move APIs: `mkdir -p api/<group> && mv api/<version> api/<group>/`
3. Move controllers: `mkdir -p internal/controller/<group> && mv internal/controller/*.go internal/controller/<group>/`
4. Move webhooks (if present): `mkdir -p internal/webhook/<group> && mv internal/webhook/<version> internal/webhook/<group>/`
5. Update import paths in all files
6. Fix `path` in `PROJECT` file for each resource
7. Update test suite CRD paths (add one more `..` to relative paths)

## Critical Rules

### Never Edit These (Auto-Generated)
- `config/crd/bases/*.yaml` - from `make manifests`
- `config/rbac/role.yaml` - from `make manifests`
- `config/webhook/manifests.yaml` - from `make manifests`
- `**/zz_generated.*.go` - from `make generate`
- `PROJECT` - from `kubebuilder [OPTIONS]`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. CLI injects code at these markers.

### Keep Project Structure
Do not move files around. The CLI expects files in specific locations.

### Always Use CLI Commands
Always use `kubebuilder create api` and `kubebuilder create webhook` to scaffold. Do NOT create files manually.

### E2E Tests Require an Isolated Kind Cluster
The e2e tests are designed to validate the solution in an isolated environment (similar to GitHub Actions CI).
Ensure you run them against a dedicated [Kind](https://kind.sigs.k8s.io/) cluster (not your “real” dev/prod cluster).

## After Making Changes

**After editing `*_types.go` or markers:**
```
make manifests  # Regenerate CRDs/RBAC from markers
make generate   # Regenerate DeepCopy methods
```

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run unit tests
```

## CLI Commands Cheat Sheet

### Create API (your own types)
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

### Deploy Image Plugin (scaffold to deploy/manage ANY container image)

Generate a controller that deploys and manages a container image (nginx, redis, memcached, your app, etc.):

```bash
# Example: deploying memcached
kubebuilder create api --group example.com --version v1alpha1 --kind Memcached \
  --image=memcached:alpine \
  --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Scaffolds good-practice code: reconciliation logic, status conditions, finalizers, RBAC. Use as a reference implementation.


### Create Webhooks
```bash
# Validation + defaulting
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version v1 --kind <Kind> \
  --conversion --spoke v2
```

### Controller for Core Kubernetes Types
```bash
# Watch Pods
kubebuilder create api --group core --version v1 --kind Pod \
  --controller=true --resource=false

# Watch Deployments
kubebuilder create api --group apps --version v1 --kind Deployment \
  --controller=true --resource=false
```

### Controller for External Types (e.g., from other operators)

Watch resources from external APIs (cert-manager, Argo CD, Istio, etc.):

```bash
# Example: watching cert-manager Certificate resources
kubebuilder create api \
  --group cert-manager --version v1 --kind Certificate \
  --controller=true --resource=false \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

**Note:** Use `--external-api-module=<module>@<version>` only if you need a specific version. Otherwise, omit `@<version>` to use what's in go.mod.

### Webhook for External Types

```bash
# Example: validating external resources
kubebuilder create webhook \
  --group cert-manager --version v1 --kind Issuer \
  --defaulting \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

## Testing & Development

```bash
make test              # Run unit tests (uses envtest: real K8s API + etcd)
make run               # Run locally (uses current kubeconfig context)
```

Tests use **Ginkgo + Gomega** (BDD style). Check `suite_test.go` for setup.

## Deployment Workflow

```bash
# 1. Regenerate manifests
make manifests generate

# 2. Build & deploy
export IMG=<registry>/<project>:tag
make docker-build docker-push IMG=$IMG  # Or: kind load docker-image $IMG --name <cluster>
make deploy IMG=$IMG

# 3. Test
kubectl apply -k config/samples/

# 4. Debug
kubectl logs -n <project>-system deployment/<project>-controller-manager -c manager -f
```

### API Design

**Key markers for** `api/<version>/*_types.go`:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// On fields:
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern="^[a-z]+$"
// +kubebuilder:default="value"
```

- **Use** `metav1.Condition` for status (not custom string fields)
- **Use predefined types**: `metav1.Time` instead of `string` for dates
- **Follow K8s API conventions**: Standard field names (`spec`, `status`, `metadata`)

### Controller Design

**RBAC markers in** `internal/controller/*_controller.go`:

```go
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

**Implementation rules:**
- **Idempotent reconciliation**: Safe to run multiple times
- **Re-fetch before updates**: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update` to avoid conflicts
- **Structured logging**: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- **Owner references**: Enable automatic garbage collection (`SetControllerReference`)
- **Watch secondary resources**: Use `.Owns()` or `.Watches()`, not just `RequeueAfter`
- **Finalizers**: Clean up external resources (buckets, VMs, DNS entries)

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Deployment could not create Pod"`) or omitted (`"Could not create Pod"`)
- Past tense: `"Could not delete Pod"` not `"Cannot delete Pod"`
- Specify object type: `"Deleted Pod"` not `"Deleted"`
- Balanced key-value pairs

```go
log.Info("Starting reconciliation")
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### Webhooks
- **Create all types together**: `--defaulting --programmatic-validation --conversion`
- **When`--force`is used**: Backup custom logic first, then restore after scaffolding
- **For multi-version APIs**: Use hub-and-spoke pattern (`--conversion --spoke v2`)
  - Hub version: Usually oldest stable version (v1)
  - Spoke versions: Newer versions that convert to/from hub (v2, v3)
  - Example: `--group crew --version v1 --kind Captain --conversion --spoke v2` (v1 is hub, v2 is spoke)

### Learning from Examples

The **deploy-image plugin** scaffolds a complete controller following good practices. Use it as a reference implementation:

```bash
kubebuilder create api --group example --version v1alpha1 --kind MyApp \
  --image=<your-image> --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Generated code includes: status conditions (`metav1.Condition`), finalizers, owner references, events, idempotent reconciliation.

## Distribution Options

### Option 1: YAML Bundle (Kustomize)

```bash
# Generate dist/install.yaml from Kustomize manifests
make build-installer IMG=<registry>/<project>:tag
```

**Key points:**
- The `dist/install.yaml` is generated from Kustomize manifests (CRDs, RBAC, Deployment)
- Commit this file to your repository for easy distribution
- Users only need `kubectl` to install (no additional tools required)

**Example:** Users install with a single command:
```bash
kubectl apply -f https://raw.githubusercontent.com/<org>/<repo>/<tag>/dist/install.yaml
```

### Option 2: Helm Chart

```bash
kubebuilder edit --plugins=helm/v2-alpha                      # Generates dist/chart/ (default)
kubebuilder edit --plugins=helm/v2-alpha --output-dir=charts  # Generates charts/chart/
```

**For development:**
```bash
make helm-deploy IMG=<registry>/<project>:<tag>          # Deploy manager via Helm
make helm-deploy IMG=$IMG HELM_EXTRA_ARGS="--set ..."    # Deploy with custom values
make helm-status                                         # Show release status
make helm-uninstall                                      # Remove release
make helm-history                                        # View release history
make helm-rollback                                       # Rollback to previous version
```

**For end users/production:**
```bash
helm install my-release ./<output-dir>/chart/ --namespace <ns> --create-namespace
```

**Important:** If you add webhooks or modify manifests after initial chart generation:
1. Backup any customizations in `<output-dir>/chart/values.yaml` and `<output-dir>/chart/manager/manager.yaml`
2. Re-run: `kubebuilder edit --plugins=helm/v2-alpha --force` (use same `--output-dir` if customized)
3. Manually restore your custom values from the backup

### Publish Container Image

```bash
export IMG=<registry>/<project>:<version>
make docker-build docker-push IMG=$IMG
```

## References

### Essential Reading
- **Kubebuilder Book**: https://book.kubebuilder.io (comprehensive guide)
- **controller-runtime FAQ**: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md (common patterns and questions)
- **Good Practices**: https://book.kubebuilder.io/reference/good-practices.html (why reconciliation is idempotent, status conditions, etc.)
- **Logging Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines (message style, verbosity levels)

### API Design & Implementation
- **API Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- **Operator Pattern**: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
- **Markers Reference**: https://book.kubebuilder.io/reference/markers.html

### Tools & Libraries
- **controller-runtime**: https://github.com/kubernetes-sigs/controller-runtime
- **controller-tools**: https://github.com/kubernetes-sigs/controller-tools
- **Kubebuilder Repo**: https://github.com/kubernetes-sigs/kubebuilder
