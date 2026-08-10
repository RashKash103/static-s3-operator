/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/config"
	"github.com/rashkash103/static-s3-operator/internal/provisioner"
	"github.com/rashkash103/static-s3-operator/internal/registry"
	"github.com/rashkash103/static-s3-operator/internal/syncer"
)

// syncPollInterval is how often a site with a Job in flight is re-checked. Job
// events drive most transitions; this is only a safety net for a missed watch
// event.
const syncPollInterval = 30 * time.Second

// Annotations the operator stamps on a sync Job, so an outcome can be attributed
// to exactly what the Job was asked to do. They live on the Job rather than only
// in status, which is what lets an operator that restarted mid-sync still record
// the result correctly.
const (
	digestAnnotation      = webv1alpha1.GroupName + "/digest"
	syncHashAnnotation    = webv1alpha1.GroupName + "/sync-hash"
	syncRequestAnnotation = webv1alpha1.GroupName + "/sync-requested-at"
)

// StaticSiteReconciler reconciles a StaticSite object.
type StaticSiteReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Defaults are the operator-wide settings that fill in optional spec fields.
	Defaults *config.Defaults
	// Provisioner reconciles the bucket. It is an interface so the reconcile loop
	// can be tested without an object store.
	Provisioner provisioner.Provisioner
	// ResolveImage turns an image reference into a digest. Tests substitute one
	// that resolves against a local registry.
	ResolveImage func(ctx context.Context, image string, opts registry.Options) (*registry.Reference, error)
	// Now supplies the current time, so tests can control requeue arithmetic.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=web.rka.sh,resources=staticsites,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=web.rka.sh,resources=staticsites/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=web.rka.sh,resources=staticsites/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a StaticSite towards its desired state.
//
// The pass is ordered so each step only runs once what it depends on is known to
// be in place: credentials, then the bucket, then the DNS alias, then the image
// digest, and finally the sync. Every step owns a condition, so a site that is
// stuck says exactly which stage it is stuck at.
//
// Status is written once, at the end, by the deferred patch.
func (r *StaticSiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	site := &webv1alpha1.StaticSite{}
	if err := r.Get(ctx, req.NamespacedName, site); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := site.DeepCopy()
	defer func() {
		site.Status.ObservedGeneration = site.Generation
		site.Status.Phase = derivePhase(site)
		if patchErr := r.patchStatus(ctx, site, base); patchErr != nil {
			err = errors.Join(err, fmt.Errorf("patch status: %w", patchErr))
		}
	}()

	effective, effectiveErr := r.Defaults.Effective(site)
	if effectiveErr != nil {
		// Only a malformed website endpoint template reaches this, and retrying will
		// not fix it, so the operator waits for the spec to change.
		setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse,
			webv1alpha1.ReasonInvalidSpec, effectiveErr.Error())
		r.Recorder.Event(site, corev1.EventTypeWarning, webv1alpha1.ReasonInvalidSpec, effectiveErr.Error())
		return ctrl.Result{}, nil
	}
	site.Status.BucketName = effective.Bucket

	if !site.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, site, effective)
	}

	if !controllerutil.ContainsFinalizer(site, webv1alpha1.Finalizer) {
		// The finalizer has to be in place before the bucket exists, or a StaticSite
		// deleted moments after creation would leave the bucket behind.
		controllerutil.AddFinalizer(site, webv1alpha1.Finalizer)
		if err := r.Update(ctx, site); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if site.Spec.Suspend {
		log.V(1).Info("Skipped a suspended StaticSite")
		setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse,
			webv1alpha1.ReasonSuspended, "Reconciliation is suspended by spec.suspend")
		return ctrl.Result{}, nil
	}

	credentials, err := r.resolveCredentials(ctx, site)
	if err != nil {
		return r.failStage(ctx, site, webv1alpha1.ConditionBucketReady,
			webv1alpha1.ReasonCredentialsError, err)
	}

	if err := r.provisionBucket(ctx, site, effective, credentials); err != nil {
		return r.failStage(ctx, site, webv1alpha1.ConditionBucketReady,
			webv1alpha1.ReasonBucketFailed, err)
	}

	if err := r.reconcileService(ctx, site, effective); err != nil {
		return r.failStage(ctx, site, webv1alpha1.ConditionReady, webv1alpha1.ReasonReconciling, err)
	}

	reference, err := r.resolveSource(ctx, site, effective)
	if err != nil {
		return r.failStage(ctx, site, webv1alpha1.ConditionSourceResolved,
			webv1alpha1.ReasonResolveFailed, err)
	}

	return r.reconcileSync(ctx, site, effective, reference, credentials)
}

// failStage records a stage failure and decides when to try again.
//
// A configuration error is reported without a fast requeue: the operator is
// waiting for a person, and retrying a mistyped bucket name every thirty seconds
// only fills the log. Everything else backs off exponentially on the site's
// failure count, so a provider outage does not become a hot loop.
func (r *StaticSiteReconciler) failStage(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	conditionType, reason string,
	cause error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	setCondition(site, conditionType, metav1.ConditionFalse, reason, cause.Error())
	setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse, reason, cause.Error())
	r.Recorder.Event(site, corev1.EventTypeWarning, reason, truncate(cause.Error(), maxConditionMessage))

	var configurationProblem *credentialsError
	if errors.As(cause, &configurationProblem) || registry.IsTerminal(cause) {
		log.Info("Waiting for the StaticSite to be corrected", "reason", reason, "error", cause.Error())
		// A watch event on the StaticSite brings the operator back sooner than this.
		return ctrl.Result{RequeueAfter: r.Defaults.SyncRetryMaxDelay}, nil
	}

	site.Status.FailedSyncs++
	delay := backoff(site.Status.FailedSyncs, r.Defaults.SyncRetryBaseDelay, r.Defaults.SyncRetryMaxDelay)
	log.Error(cause, "Failed to reconcile StaticSite", "reason", reason, "retryAfter", delay.String())
	return ctrl.Result{RequeueAfter: delay}, nil
}

// provisionBucket makes the bucket match spec.bucket and records ownership.
func (r *StaticSiteReconciler) provisionBucket(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	credentials *resolvedCredentials,
) error {
	log := logf.FromContext(ctx)

	result, err := r.Provisioner.Provision(ctx, provisioner.ProvisionRequest{
		Effective:   effective,
		Credentials: credentials.Credentials,
		Site:        site.Namespace + "/" + site.Name,
		SiteUID:     string(site.UID),
		Ownership:   site.Status.BucketOwnership,
	})
	if err != nil {
		return err
	}

	if result.Ownership != nil {
		site.Status.BucketOwnership = result.Ownership
	}
	site.Status.WebsiteURL = effective.WebsiteURL

	if result.Created {
		log.Info("Created bucket", "bucket", effective.Bucket, "region", effective.Region)
		r.Recorder.Eventf(site, corev1.EventTypeNormal, "BucketCreated",
			"Created bucket %q in region %q", effective.Bucket, effective.Region)
	}
	if result.Adopted {
		// Worth saying out loud, since it silently changes what deletion will do.
		log.Info("Adopted an existing bucket; it will not be deleted with this StaticSite",
			"bucket", effective.Bucket)
		r.Recorder.Eventf(site, corev1.EventTypeNormal, "BucketAdopted",
			"Bucket %q already existed and carries no ownership record, so it will be left in place "+
				"when this StaticSite is deleted", effective.Bucket)
	}
	if len(result.Changes) > 0 && !result.Created {
		log.Info("Reconciled bucket configuration",
			"bucket", effective.Bucket, "changed", strings.Join(result.Changes, ","))
	}

	message := fmt.Sprintf("Bucket %q is configured for website hosting", effective.Bucket)
	if result.Adopted {
		message = fmt.Sprintf(
			"Bucket %q was adopted rather than created, so it will not be deleted with this StaticSite",
			effective.Bucket)
	}
	setCondition(site, webv1alpha1.ConditionBucketReady, metav1.ConditionTrue,
		webv1alpha1.ReasonBucketReady, message)
	return nil
}

// resolveSource turns spec.source.image into a digest, respecting the poll
// interval so a mutable tag is not re-checked on every reconcile.
func (r *StaticSiteReconciler) resolveSource(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
) (*registry.Reference, error) {
	log := logf.FromContext(ctx)
	now := r.now()

	if cached := r.cachedReference(site, effective, now); cached != nil {
		return cached, nil
	}

	keychain, err := r.keychainFor(ctx, site, effective)
	if err != nil {
		return nil, err
	}

	reference, err := r.ResolveImage(ctx, effective.Image, registry.Options{
		Keychain:              keychain,
		PlainHTTP:             effective.PlainHTTP,
		InsecureSkipTLSVerify: effective.InsecureSkipTLSVerify,
	})
	if err != nil {
		return nil, err
	}

	if site.Status.ResolvedDigest != "" && site.Status.ResolvedDigest != reference.Digest {
		log.Info("Detected a new image digest",
			"image", effective.Image, "from", site.Status.ResolvedDigest, "to", reference.Digest)
		r.Recorder.Eventf(site, corev1.EventTypeNormal, "NewImageDigest",
			"Image %q now resolves to %s", effective.Image, reference.Digest)
	}

	resolvedAt := metav1.NewTime(now)
	site.Status.ResolvedImage = reference.Pinned
	site.Status.ResolvedDigest = reference.Digest
	site.Status.LastResolveTime = &resolvedAt
	site.Status.NextResolveTime = nextResolveTime(now, effective.PullInterval, reference.WasPinned)

	setCondition(site, webv1alpha1.ConditionSourceResolved, metav1.ConditionTrue,
		webv1alpha1.ReasonResolved,
		fmt.Sprintf("Image %q resolves to %s", effective.Image, reference.Digest))
	return reference, nil
}

// cachedReference returns the digest already in status while it is still fresh.
//
// Without this, every reconcile would hit the registry, which is exactly what
// spec.source.pullInterval exists to prevent: a site is meant to be re-checked
// once a day, not once a minute because a Job event fired.
func (r *StaticSiteReconciler) cachedReference(
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	now time.Time,
) *registry.Reference {
	status := site.Status
	if status.ResolvedDigest == "" || status.ResolvedImage == "" {
		return nil
	}
	// The recorded digest belongs to whatever image was configured then, so a
	// changed spec.source.image invalidates it.
	if !referencesSameImage(status.ResolvedImage, effective.Image) {
		return nil
	}
	if conditionStatus(site, webv1alpha1.ConditionSourceResolved) != metav1.ConditionTrue {
		return nil
	}
	// A manual sync request means "check now", whatever the interval says.
	if request := syncRequest(site); request != "" && request != status.LastHandledSyncRequest {
		return nil
	}

	if status.NextResolveTime == nil {
		// No next poll: either the reference is digest pinned, or polling is off. The
		// second case only holds while the interval is still zero.
		if effective.PullInterval > 0 && !isDigestReference(effective.Image) {
			return nil
		}
	} else if !status.NextResolveTime.Time.After(now) {
		return nil
	}

	return &registry.Reference{
		Digest:    status.ResolvedDigest,
		Pinned:    status.ResolvedImage,
		WasPinned: isDigestReference(effective.Image),
	}
}

func isDigestReference(image string) bool {
	return strings.Contains(image, "@")
}

// referencesSameImage reports whether a pinned reference came from the configured
// image, by comparing the repository part of each.
func referencesSameImage(pinned, configured string) bool {
	return repositoryOf(pinned) == repositoryOf(configured)
}

// repositoryOf strips a tag or digest, leaving the repository.
func repositoryOf(reference string) string {
	if index := strings.Index(reference, "@"); index >= 0 {
		return reference[:index]
	}
	// A colon before the last slash is a registry port, not a tag.
	lastSlash := strings.LastIndex(reference, "/")
	if index := strings.LastIndex(reference, ":"); index > lastSlash {
		return reference[:index]
	}
	return reference
}

// nextResolveTime is when the registry should be polled again. A digest-pinned
// reference and a zero interval both mean never.
func nextResolveTime(now time.Time, interval time.Duration, wasPinned bool) *metav1.Time {
	if wasPinned || interval <= 0 {
		return nil
	}
	next := metav1.NewTime(now.Add(interval))
	return &next
}

// keychainFor builds registry credentials from the site's image pull Secrets.
func (r *StaticSiteReconciler) keychainFor(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
) (authn.Keychain, error) {
	if len(effective.ImagePullSecrets) == 0 {
		return nil, nil
	}

	secrets := make([]corev1.Secret, 0, len(effective.ImagePullSecrets))
	for _, name := range effective.ImagePullSecrets {
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: site.Namespace, Name: name}
		if err := r.Get(ctx, key, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, &credentialsError{
					message: fmt.Sprintf("Secret %q named by spec.source.imagePullSecrets does not exist", key),
				}
			}
			return nil, fmt.Errorf("read Secret %q: %w", key, err)
		}
		secrets = append(secrets, *secret)
	}

	keychain, err := registry.KeychainFromSecrets(secrets)
	if err != nil {
		return nil, &credentialsError{message: err.Error()}
	}
	return keychain, nil
}

// reconcileSync advances the sync state machine: observe a Job in flight, decide
// whether the bucket is already up to date, and start a Job if it is not.
func (r *StaticSiteReconciler) reconcileSync(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	reference *registry.Reference,
	credentials *resolvedCredentials,
) (ctrl.Result, error) {
	syncHash := effective.SyncSpecHash()

	pending, err := r.findPendingJob(ctx, site)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending != nil {
		return r.observeSyncJob(ctx, site, effective, pending)
	}

	if !r.needsSync(site, reference, syncHash) {
		setCondition(site, webv1alpha1.ConditionSynced, metav1.ConditionTrue, webv1alpha1.ReasonUpToDate,
			fmt.Sprintf("Bucket %q holds the contents of %s", effective.Bucket, reference.Digest))
		setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionTrue, webv1alpha1.ReasonReady,
			fmt.Sprintf("Site is served from bucket %q", effective.Bucket))
		return ctrl.Result{RequeueAfter: r.idleRequeue(site)}, nil
	}

	return r.startSyncJob(ctx, site, effective, reference, credentials, syncHash)
}

// needsSync decides whether the bucket contents are stale.
func (r *StaticSiteReconciler) needsSync(
	site *webv1alpha1.StaticSite,
	reference *registry.Reference,
	syncHash string,
) bool {
	if site.Status.SyncedDigest != reference.Digest {
		return true
	}
	// The digest can be unchanged while the settings deciding what gets uploaded,
	// and with which headers, have changed.
	if site.Status.SyncedSpecHash != syncHash {
		return true
	}
	if request := syncRequest(site); request != "" && request != site.Status.LastHandledSyncRequest {
		return true
	}
	return false
}

// syncRequest returns the value of the manual sync annotation.
func syncRequest(site *webv1alpha1.StaticSite) string {
	return site.Annotations[webv1alpha1.SyncRequestedAtAnnotation]
}

// findPendingJob returns the sync Job whose outcome is not yet recorded, whether
// it is still running or has finished.
//
// Jobs are found by label rather than only by the name in status, so a Job
// created just before a failed status write is adopted instead of duplicated.
func (r *StaticSiteReconciler) findPendingJob(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
) (*batchv1.Job, error) {
	jobs, err := r.listSyncJobs(ctx, site)
	if err != nil {
		return nil, err
	}

	for i := range jobs {
		if outcome, _ := classifyJob(&jobs[i]); outcome == jobRunning {
			return &jobs[i], nil
		}
	}

	// Nothing is running. If status still points at a Job, settle its outcome now,
	// which is what recovers a sync the operator was restarted in the middle of.
	if site.Status.ActiveSyncJob != "" {
		for i := range jobs {
			if jobs[i].Name == site.Status.ActiveSyncJob {
				return &jobs[i], nil
			}
		}
		logf.FromContext(ctx).Info("Sync Job disappeared before its outcome was recorded",
			"job", site.Status.ActiveSyncJob)
		site.Status.ActiveSyncJob = ""
	}
	return nil, nil
}

func (r *StaticSiteReconciler) listSyncJobs(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
) ([]batchv1.Job, error) {
	jobs := &batchv1.JobList{}
	err := r.List(ctx, jobs,
		client.InNamespace(site.Namespace),
		client.MatchingLabels{webv1alpha1.SiteUIDLabel: string(site.UID)},
	)
	if err != nil {
		return nil, fmt.Errorf("list sync Jobs: %w", err)
	}
	return jobs.Items, nil
}

// observeSyncJob records the outcome of a finished Job, or reports that one is
// still running.
func (r *StaticSiteReconciler) observeSyncJob(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	job *batchv1.Job,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	outcome, conditionMessage := classifyJob(job)

	if outcome == jobRunning {
		site.Status.ActiveSyncJob = job.Name
		setCondition(site, webv1alpha1.ConditionSynced, metav1.ConditionFalse, webv1alpha1.ReasonSyncing,
			fmt.Sprintf("Sync Job %s is copying %s into bucket %q",
				job.Name, job.Annotations[digestAnnotation], effective.Bucket))
		setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse, webv1alpha1.ReasonSyncing,
			fmt.Sprintf("Sync Job %s is in progress", job.Name))
		return ctrl.Result{RequeueAfter: syncPollInterval}, nil
	}

	report := r.readSyncReport(ctx, job)
	attempt := webv1alpha1.SyncAttempt{
		JobName:        job.Name,
		Digest:         job.Annotations[digestAnnotation],
		StartTime:      job.Status.StartTime,
		CompletionTime: job.Status.CompletionTime,
	}
	if attempt.CompletionTime == nil {
		now := metav1.NewTime(r.now())
		attempt.CompletionTime = &now
	}
	if report != nil {
		attempt.Stats = statsFromReport(report)
	}

	site.Status.ActiveSyncJob = ""

	if outcome == jobFailed {
		reportMessage := ""
		if report != nil {
			reportMessage = report.Message
		}
		message := describeJobFailure(job, reportMessage, conditionMessage)

		attempt.Succeeded = false
		attempt.Message = truncate(message, 2048)
		recordAttempt(site, attempt, effective.RetainHistory)

		site.Status.FailedSyncs++
		setCondition(site, webv1alpha1.ConditionSynced, metav1.ConditionFalse,
			webv1alpha1.ReasonSyncFailed, message)
		setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse,
			webv1alpha1.ReasonSyncFailed, message)
		r.Recorder.Event(site, corev1.EventTypeWarning, webv1alpha1.ReasonSyncFailed,
			truncate(message, maxConditionMessage))
		log.Info("Sync Job failed", "job", job.Name, "message", message)

		delay := backoff(site.Status.FailedSyncs, r.Defaults.SyncRetryBaseDelay, r.Defaults.SyncRetryMaxDelay)
		if err := r.pruneSyncJobs(ctx, site, effective.RetainHistory); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: delay}, nil
	}

	attempt.Succeeded = true
	recordAttempt(site, attempt, effective.RetainHistory)

	site.Status.FailedSyncs = 0
	site.Status.SyncedDigest = attempt.Digest
	site.Status.SyncedSpecHash = job.Annotations[syncHashAnnotation]
	site.Status.LastSyncTime = attempt.CompletionTime
	site.Status.LastSyncStats = attempt.Stats
	site.Status.LastHandledSyncRequest = job.Annotations[syncRequestAnnotation]

	setCondition(site, webv1alpha1.ConditionSynced, metav1.ConditionTrue, webv1alpha1.ReasonSynced,
		syncedMessage(effective, attempt.Digest, attempt.Stats))
	setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionTrue, webv1alpha1.ReasonReady,
		fmt.Sprintf("Site is served from bucket %q", effective.Bucket))

	r.Recorder.Eventf(site, corev1.EventTypeNormal, webv1alpha1.ReasonSynced,
		"Synced %s into bucket %q", attempt.Digest, effective.Bucket)
	log.Info("Sync succeeded", "job", job.Name, "digest", attempt.Digest)

	if report != nil && report.ACLDropped {
		r.Recorder.Event(site, corev1.EventTypeNormal, "ObjectACLsUnsupported",
			"The bucket does not accept object ACLs, so public access relies on the bucket policy. "+
				"Set spec.sync.acl to private to stop asking for them.")
	}

	if err := r.pruneSyncJobs(ctx, site, effective.RetainHistory); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.idleRequeue(site)}, nil
}

func syncedMessage(effective *config.Effective, digest string, stats *webv1alpha1.SyncStats) string {
	if stats == nil {
		return fmt.Sprintf("Bucket %q holds the contents of %s", effective.Bucket, digest)
	}
	return fmt.Sprintf("Bucket %q holds the contents of %s (%d uploaded, %d deleted, %d unchanged)",
		effective.Bucket, digest, stats.Uploaded, stats.Deleted, stats.Unchanged)
}

func statsFromReport(report *syncer.Report) *webv1alpha1.SyncStats {
	// Object counts are bounded by --max-extract-files, well inside int32.
	return &webv1alpha1.SyncStats{
		Uploaded:      int32(report.Uploaded),  //nolint:gosec // bounded by the extract file limit
		Deleted:       int32(report.Deleted),   //nolint:gosec // bounded by the extract file limit
		Unchanged:     int32(report.Unchanged), //nolint:gosec // bounded by the extract file limit
		Skipped:       int32(report.Skipped),   //nolint:gosec // bounded by the extract file limit
		BytesUploaded: report.BytesUploaded,
		Duration:      report.Duration,
	}
}

// readSyncReport pulls the syncer's JSON report out of its Pod's termination
// message. A missing report is normal for a Pod that was evicted or OOM killed,
// so it is never treated as an error.
func (r *StaticSiteReconciler) readSyncReport(ctx context.Context, job *batchv1.Job) *syncer.Report {
	log := logf.FromContext(ctx)

	pods := &corev1.PodList{}
	err := r.List(ctx, pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{webv1alpha1.SiteUIDLabel: job.Labels[webv1alpha1.SiteUIDLabel]},
	)
	if err != nil {
		log.V(1).Info("Could not list sync Pods to read the report", "error", err.Error())
		return nil
	}

	// The newest Pod owned by this Job is the attempt that decided its outcome.
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedByJob(pod, job) {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated == nil {
				continue
			}
			report, err := syncer.ParseReport(status.State.Terminated.Message)
			if err != nil {
				log.V(1).Info("Ignored an unparsable sync report", "pod", pod.Name, "error", err.Error())
				continue
			}
			if report != nil {
				return report
			}
		}
	}
	return nil
}

func ownedByJob(pod *corev1.Pod, job *batchv1.Job) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.UID == job.UID {
			return true
		}
	}
	return false
}

// startSyncJob creates the Job that copies the image contents into the bucket.
func (r *StaticSiteReconciler) startSyncJob(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	reference *registry.Reference,
	credentials *resolvedCredentials,
	syncHash string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	job := r.buildSyncJob(site, effective, reference, credentials, syncHash)
	job.Annotations = map[string]string{
		digestAnnotation:      reference.Digest,
		syncHashAnnotation:    syncHash,
		syncRequestAnnotation: syncRequest(site),
	}
	if err := controllerutil.SetControllerReference(site, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner on sync Job: %w", err)
	}

	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create sync Job: %w", err)
	}

	site.Status.ActiveSyncJob = job.Name
	setCondition(site, webv1alpha1.ConditionSynced, metav1.ConditionFalse, webv1alpha1.ReasonSyncing,
		fmt.Sprintf("Sync Job %s is copying %s into bucket %q", job.Name, reference.Digest, effective.Bucket))
	setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse, webv1alpha1.ReasonSyncing,
		fmt.Sprintf("Sync Job %s is in progress", job.Name))

	log.Info("Started sync Job", "job", job.Name, "digest", reference.Digest, "bucket", effective.Bucket)
	r.Recorder.Eventf(site, corev1.EventTypeNormal, webv1alpha1.ReasonSyncing,
		"Started sync Job %s for %s", job.Name, reference.Digest)

	return ctrl.Result{RequeueAfter: syncPollInterval}, nil
}

// pruneSyncJobs keeps the most recent finished Jobs and deletes the rest, along
// with their Pods, so a long-lived site does not accumulate history forever.
func (r *StaticSiteReconciler) pruneSyncJobs(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	retain int,
) error {
	jobs, err := r.listSyncJobs(ctx, site)
	if err != nil {
		return err
	}

	finished := make([]batchv1.Job, 0, len(jobs))
	for i := range jobs {
		if outcome, _ := classifyJob(&jobs[i]); outcome != jobRunning {
			finished = append(finished, jobs[i])
		}
	}
	if len(finished) <= retain {
		return nil
	}

	sort.Slice(finished, func(i, j int) bool {
		return finished[j].CreationTimestamp.Before(&finished[i].CreationTimestamp)
	})

	policy := metav1.DeletePropagationBackground
	for i := retain; i < len(finished); i++ {
		job := &finished[i]
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("prune sync Job %q: %w", job.Name, err)
		}
		logf.FromContext(ctx).V(1).Info("Pruned an old sync Job", "job", job.Name)
	}
	return nil
}

// idleRequeue is when to wake a healthy site again: to poll for a new image
// digest, and failing that to re-check the bucket's configuration.
func (r *StaticSiteReconciler) idleRequeue(site *webv1alpha1.StaticSite) time.Duration {
	recheck := r.Defaults.OwnershipRecheckInterval
	if recheck <= 0 {
		recheck = time.Hour
	}
	if site.Status.NextResolveTime == nil {
		return recheck
	}

	untilResolve := site.Status.NextResolveTime.Time.Sub(r.now())
	if untilResolve < time.Second {
		untilResolve = time.Second
	}
	return min(untilResolve, recheck)
}

// reconcileDelete tears the bucket down, if this site is allowed to, and then
// releases the finalizer.
func (r *StaticSiteReconciler) reconcileDelete(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(site, webv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}

	setCondition(site, webv1alpha1.ConditionReady, metav1.ConditionFalse, webv1alpha1.ReasonDeleting,
		"Tearing down the site")

	credentials, err := r.resolveCredentials(ctx, site)
	if err != nil {
		return r.abandonOrRetryDeletion(ctx, site, effective, err,
			"Could not read S3 credentials to delete bucket %q")
	}

	force := site.Annotations[webv1alpha1.ForceBucketDeletionAnnotation] == "true"
	result, err := r.Provisioner.Deprovision(ctx, provisioner.DeprovisionRequest{
		Effective:   effective,
		Credentials: credentials.Credentials,
		Ownership:   site.Status.BucketOwnership,
		SiteUID:     string(site.UID),
		Force:       force,
	})
	if err != nil {
		return r.abandonOrRetryDeletion(ctx, site, effective, err, "Could not delete bucket %q")
	}

	switch {
	case result.Deleted:
		log.Info("Deleted bucket", "bucket", effective.Bucket, "objectsRemoved", result.ObjectsRemoved)
		r.Recorder.Eventf(site, corev1.EventTypeNormal, "BucketDeleted",
			"Deleted bucket %q and %d objects", effective.Bucket, result.ObjectsRemoved)
	default:
		log.Info("Retained bucket", "bucket", effective.Bucket, "reason", result.RetainedReason)
		r.Recorder.Eventf(site, corev1.EventTypeNormal, "BucketRetained",
			"Left bucket %q in place: %s", effective.Bucket, result.RetainedReason)
	}

	return r.releaseFinalizer(ctx, site)
}

// abandonOrRetryDeletion retries teardown for a while, then gives the bucket up.
//
// Blocking on the finalizer forever would be the safer choice for the data, but
// it leaves the namespace stuck in Terminating with no obvious cause, which is
// worse in practice for a resource whose whole job is to be disposable. The
// bucket is left behind with a warning naming it, so it can be cleaned up by
// hand.
func (r *StaticSiteReconciler) abandonOrRetryDeletion(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	cause error,
	messageFormat string,
) (ctrl.Result, error) {
	if !r.deletionDeadlineExceeded(site) {
		return r.failStage(ctx, site, webv1alpha1.ConditionBucketReady,
			webv1alpha1.ReasonBucketFailed, cause)
	}

	logf.FromContext(ctx).Error(cause, "Released the finalizer without deleting the bucket",
		"bucket", effective.Bucket)
	r.Recorder.Eventf(site, corev1.EventTypeWarning, "BucketRetained",
		messageFormat+" within %s (%v); it has been left behind and must be removed by hand",
		effective.Bucket, r.Defaults.SyncRetryMaxDelay, cause)
	return r.releaseFinalizer(ctx, site)
}

// deletionDeadlineExceeded reports whether teardown has been failing long enough
// to give up on it.
func (r *StaticSiteReconciler) deletionDeadlineExceeded(site *webv1alpha1.StaticSite) bool {
	if site.DeletionTimestamp == nil {
		return false
	}
	return r.now().Sub(site.DeletionTimestamp.Time) > r.Defaults.SyncRetryMaxDelay
}

func (r *StaticSiteReconciler) releaseFinalizer(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(site, webv1alpha1.Finalizer)
	if err := r.Update(ctx, site); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *StaticSiteReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager.
func (r *StaticSiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Defaults == nil {
		return errors.New("StaticSiteReconciler needs Defaults")
	}
	if r.Provisioner == nil {
		return errors.New("StaticSiteReconciler needs a Provisioner")
	}
	if r.ResolveImage == nil {
		r.ResolveImage = registry.Resolve
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("staticsite-controller")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&webv1alpha1.StaticSite{}).
		// Job events are what move a sync forward, so the loop reacts to them rather
		// than polling on a timer.
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Defaults.MaxConcurrentReconciles}).
		Named("staticsite").
		Complete(r)
}
