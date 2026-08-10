/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types reported on a StaticSite.
const (
	// ConditionReady is the aggregate condition: the bucket exists, is configured
	// for website hosting, and its contents match the resolved source image.
	ConditionReady = "Ready"

	// ConditionBucketReady reports whether the S3 bucket exists and its website,
	// policy and tag configuration match the spec.
	ConditionBucketReady = "BucketReady"

	// ConditionSourceResolved reports whether spec.source.image could be resolved
	// to an immutable digest in the remote registry.
	ConditionSourceResolved = "SourceResolved"

	// ConditionSynced reports whether the bucket contents match the digest
	// recorded in status.resolvedDigest.
	ConditionSynced = "Synced"
)

// Condition reasons reported on a StaticSite.
const (
	ReasonReconciling        = "Reconciling"
	ReasonSuspended          = "Suspended"
	ReasonReady              = "Ready"
	ReasonBucketReady        = "BucketReady"
	ReasonBucketFailed       = "BucketConfigurationFailed"
	ReasonCredentialsError   = "CredentialsError"
	ReasonResolved           = "Resolved"
	ReasonResolveFailed      = "ResolveFailed"
	ReasonSyncing            = "SyncInProgress"
	ReasonSynced             = "SyncSucceeded"
	ReasonSyncFailed         = "SyncFailed"
	ReasonUpToDate           = "UpToDate"
	ReasonInvalidSpec        = "InvalidSpec"
	ReasonDeleting           = "Deleting"
	ReasonAwaitingBucket     = "AwaitingBucket"
	ReasonAwaitingResolution = "AwaitingSourceResolution"
	ReasonOwnershipConflict  = "BucketOwnershipConflict"
)

// Annotations recognised on a StaticSite.
const (
	// SyncRequestedAtAnnotation triggers an immediate re-resolve and re-sync when
	// its value changes. Set it to a timestamp to force a sync out of band:
	//
	//	kubectl annotate staticsite my-site \
	//	  web.rka.sh/sync-requested-at="$(date -u +%FT%TZ)" --overwrite
	SyncRequestedAtAnnotation = GroupName + "/sync-requested-at"

	// ForceBucketDeletionAnnotation permits deletion of a bucket the operator did
	// not create. Without it, a bucket that fails the ownership check is always
	// retained, whatever spec.bucket.deletionPolicy says. Use it deliberately:
	// it removes the only guard against destroying somebody else's data.
	ForceBucketDeletionAnnotation = GroupName + "/force-bucket-deletion"
)

// Labels applied to resources the operator creates.
const (
	// ManagedByLabel marks resources created by this operator.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of ManagedByLabel on generated resources.
	ManagedByValue = "static-s3-operator"
	// SiteNameLabel is the StaticSite a generated resource belongs to.
	SiteNameLabel = GroupName + "/staticsite"
	// SiteUIDLabel is the StaticSite UID a generated resource belongs to. It is
	// what lets the operator recognise its own Jobs after a restart, including a
	// Job whose name it never managed to record.
	SiteUIDLabel = GroupName + "/staticsite-uid"
	// DigestLabel is the short image digest a sync Job was pinned to.
	DigestLabel = GroupName + "/digest"
	// SyncHashLabel is the short sync-settings fingerprint a Job ran with.
	SyncHashLabel = GroupName + "/sync-hash"
)

// Finalizer is placed on a StaticSite so the operator can tear down the bucket
// before the resource disappears.
const Finalizer = GroupName + "/finalizer"

// StaticSitePhase is a coarse, human-oriented summary of where a StaticSite is
// in its lifecycle. It is derived from the conditions and exists for `kubectl
// get` output; controllers and scripts should branch on conditions instead.
// +kubebuilder:validation:Enum=Pending;Provisioning;Syncing;Ready;Failed;Suspended;Deleting
type StaticSitePhase string

const (
	// PhasePending means the resource has been accepted but not yet acted upon.
	PhasePending StaticSitePhase = "Pending"
	// PhaseProvisioning means the bucket is being created or reconfigured.
	PhaseProvisioning StaticSitePhase = "Provisioning"
	// PhaseSyncing means a sync Job is currently copying content into the bucket.
	PhaseSyncing StaticSitePhase = "Syncing"
	// PhaseReady means the bucket serves the contents of the resolved image.
	PhaseReady StaticSitePhase = "Ready"
	// PhaseFailed means the last reconcile attempt failed; see conditions.
	PhaseFailed StaticSitePhase = "Failed"
	// PhaseSuspended means spec.suspend is true and reconciliation is paused.
	PhaseSuspended StaticSitePhase = "Suspended"
	// PhaseDeleting means the resource is being torn down.
	PhaseDeleting StaticSitePhase = "Deleting"
)

// DeletionPolicy controls what happens to the S3 bucket when the StaticSite is
// deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain leaves the bucket and its objects untouched.
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete empties and removes the bucket, but only when the
	// operator can prove it created that bucket. See BucketOwnership.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// CannedACL is an S3 canned ACL applied to uploaded objects.
// +kubebuilder:validation:Enum=public-read;private
type CannedACL string

const (
	// ACLPublicRead makes uploaded objects world readable, which is what a
	// website bucket normally needs.
	ACLPublicRead CannedACL = "public-read"
	// ACLPrivate uploads objects without a public grant. Use this when read
	// access comes from a bucket policy or a CDN with its own credentials, or
	// against a provider that does not implement object ACLs.
	ACLPrivate CannedACL = "private"
)

// SourceSpec describes the container image that carries the built static site.
type SourceSpec struct {
	// image is the container image reference that contains the built site, for
	// example "ghcr.io/acme/marketing-site:latest", "docker.io/acme/site:v1.4.2"
	// or a digest-pinned "ghcr.io/acme/site@sha256:abc...".
	//
	// When the reference carries a tag, the operator re-resolves that tag to a
	// digest every pullInterval and re-syncs when the digest changes. When the
	// reference carries a digest it is treated as immutable and never re-polled.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Image string `json:"image"`

	// path is the absolute directory inside the image whose contents become the
	// root of the website. The directory contents are copied, not the directory
	// itself: with path "/site", "/site/index.html" is uploaded as "index.html".
	//
	// Defaults to the operator's --default-source-path (itself set by the Helm
	// chart, "/usr/share/nginx/html" out of the box).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path,omitempty"`

	// pullInterval is how often a tagged (mutable) image reference is
	// re-resolved to check for a new digest. Set it to "0" to disable polling so
	// that syncs only happen on spec changes or on an explicit
	// "web.rka.sh/sync-requested-at" annotation.
	//
	// Ignored for digest-pinned references, which cannot change. Defaults to the
	// operator's --default-pull-interval (24h out of the box).
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^0$|^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	PullInterval *metav1.Duration `json:"pullInterval,omitempty"`

	// imagePullSecrets are dockerconfigjson Secrets in the same namespace as this
	// StaticSite, used both by the operator when it resolves the tag to a digest
	// and by the sync Job when it downloads the image layers.
	// +optional
	// +listType=map
	// +listMapKey=name
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// plainHTTP talks to the registry over HTTP instead of HTTPS. Intended for
	// in-cluster development registries; do not enable it for a registry reached
	// over an untrusted network.
	// +optional
	PlainHTTP bool `json:"plainHTTP,omitempty"`

	// insecureSkipTLSVerify skips registry certificate verification. Intended for
	// registries with self-signed certificates in development environments.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// S3CredentialsSpec points at a Secret holding S3 API credentials.
type S3CredentialsSpec struct {
	// secretRef names a Secret in the same namespace as this StaticSite.
	// +required
	SecretRef corev1.LocalObjectReference `json:"secretRef"`

	// accessKeyIDKey is the Secret key holding the access key ID.
	// +optional
	// +kubebuilder:default="accessKeyID"
	// +kubebuilder:validation:MinLength=1
	AccessKeyIDKey string `json:"accessKeyIDKey,omitempty"`

	// secretAccessKeyKey is the Secret key holding the secret access key.
	// +optional
	// +kubebuilder:default="secretAccessKey"
	// +kubebuilder:validation:MinLength=1
	SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`

	// sessionTokenKey is the Secret key holding an optional session token, for
	// temporary STS credentials. The key may be absent from the Secret.
	// +optional
	// +kubebuilder:default="sessionToken"
	// +kubebuilder:validation:MinLength=1
	SessionTokenKey string `json:"sessionTokenKey,omitempty"`
}

// WebsiteSpec configures S3 static website hosting on the bucket.
type WebsiteSpec struct {
	// indexDocument is the object served for requests to a "directory", so that
	// with "index.html" a request for "/docs/" serves the "docs/index.html"
	// object. Defaults to the operator's --default-index-document.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^/]+$`
	IndexDocument string `json:"indexDocument,omitempty"`

	// errorDocument is the object served for 4xx responses. Single page
	// applications that route client side usually set this to "index.html";
	// otherwise point it at a "404.html" shipped in the image. Defaults to the
	// operator's --default-error-document.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ErrorDocument string `json:"errorDocument,omitempty"`
}

// BucketSpec describes the S3 bucket that hosts the site.
type BucketSpec struct {
	// name is the S3 bucket name. It is immutable: to move a site to a different
	// bucket, create a new StaticSite. Bucket names are global per provider, so
	// pick something unlikely to collide.
	// +required
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bucket.name is immutable"
	Name string `json:"name"`

	// region is the S3 region the bucket lives in. Defaults to the operator's
	// --default-region.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region,omitempty"`

	// endpoint is the S3 API endpoint for S3-compatible providers, for example
	// "https://us-east-1.example-object-store.com" or an in-cluster
	// "http://minio.storage.svc.cluster.local:9000". Leave it empty to use the
	// operator's --default-endpoint, which is itself empty for AWS S3. When set
	// without a scheme, https is assumed.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint,omitempty"`

	// websiteEndpointTemplate builds status.websiteURL for providers whose
	// website hostname differs from their API endpoint. It is a Go template
	// evaluated with {{.Bucket}}, {{.Region}} and {{.Endpoint}}, for example
	// "http://{{.Bucket}}.website-{{.Region}}.example-object-store.com". Falls
	// back to the operator's --default-website-endpoint-template, and then to the
	// AWS or path-style form derived from the endpoint.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	WebsiteEndpointTemplate string `json:"websiteEndpointTemplate,omitempty"`

	// forcePathStyle addresses buckets as "endpoint/bucket" instead of
	// "bucket.endpoint". Most self-hosted and S3-compatible providers need this;
	// AWS S3 does not. Defaults to the operator's --default-force-path-style.
	// +optional
	ForcePathStyle *bool `json:"forcePathStyle,omitempty"`

	// credentials selects a Secret in this namespace holding the S3 access key
	// used for this site. When omitted, the operator's own default credentials
	// are used, and are mirrored into this namespace as a Secret owned by this
	// StaticSite so the sync Job can read them.
	// +optional
	Credentials *S3CredentialsSpec `json:"credentials,omitempty"`

	// create lets the operator create the bucket when it does not exist. Set it
	// to false to adopt a bucket managed elsewhere; the operator then only
	// verifies that the bucket is reachable. A bucket the operator did not create
	// is never deleted, regardless of deletionPolicy.
	// +optional
	// +kubebuilder:default=true
	Create *bool `json:"create,omitempty"`

	// deletionPolicy decides the fate of the bucket when this StaticSite is
	// deleted.
	//
	// "Delete" (the default) empties and removes the bucket, but only if the
	// operator can prove it created it: status.bucketOwnership.token must match
	// the ownership marker stored in the bucket. Buckets that fail that check are
	// retained and reported as an ownership conflict.
	//
	// "Retain" always leaves the bucket in place.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// website configures S3 static website hosting. Set it to null to skip
	// website configuration entirely, for example when a CDN in front of the
	// bucket does its own index rewriting, or when the provider does not
	// implement PutBucketWebsite.
	// +optional
	// +kubebuilder:default={}
	Website *WebsiteSpec `json:"website,omitempty"`

	// publicRead grants anonymous s3:GetObject on the bucket's objects via a
	// bucket policy, and clears the bucket's public access block so the policy
	// can take effect. This is what makes the site readable by a browser.
	// Disable it when a CDN reads the bucket with its own credentials.
	// +optional
	// +kubebuilder:default=true
	PublicRead *bool `json:"publicRead,omitempty"`

	// policy is a raw S3 bucket policy document (JSON). When set it replaces the
	// policy the operator would generate from publicRead, so it must grant
	// whatever read access the site needs. "${bucket}" in the document is
	// substituted with the bucket name.
	// +optional
	Policy string `json:"policy,omitempty"`

	// tags are applied to the bucket, alongside the operator's own ownership
	// tags. Tags the operator does not know about are removed, so it owns the
	// bucket's tag set. Providers that do not implement bucket tagging are
	// tolerated: tagging failures are logged, not fatal.
	// +optional
	// +kubebuilder:validation:MaxProperties=40
	Tags map[string]string `json:"tags,omitempty"`
}

// GlobRule maps a glob over object keys to a header value.
type GlobRule struct {
	// pattern is a glob matched against each object key relative to the site
	// root, for example "assets/**", "*.html" or "**/*.woff2". "**" crosses
	// directory separators, "*" does not.
	// +required
	// +kubebuilder:validation:MinLength=1
	Pattern string `json:"pattern"`

	// value is the literal header value, for example
	// "public, max-age=31536000, immutable" for a Cache-Control rule.
	// +required
	// +kubebuilder:validation:MinLength=1
	Value string `json:"value"`
}

// SyncJobSpec tunes the Kubernetes Job that performs the copy.
type SyncJobSpec struct {
	// image overrides the syncer image. It must provide the static-s3-operator
	// syncer entrypoint; by default the operator uses its own image, which ships
	// both binaries.
	// +optional
	Image string `json:"image,omitempty"`

	// imagePullPolicy for the syncer image.
	// +optional
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// imagePullSecrets are passed to the sync Pod so kubelet can pull the syncer
	// image from a private registry. These are unrelated to
	// spec.source.imagePullSecrets, which authenticate the site image download.
	// +optional
	// +listType=map
	// +listMapKey=name
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// serviceAccountName runs the sync Pod under a specific ServiceAccount. The
	// sync Pod needs no Kubernetes API access, so the default ServiceAccount is
	// fine.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// resources are the sync container's compute resources. Sites are staged on
	// disk before upload, so ephemeral-storage should exceed the extracted size.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// workspaceVolume replaces the default emptyDir scratch space used to stage
	// extracted files. Point it at a larger or faster volume for big sites.
	// +optional
	WorkspaceVolume *corev1.VolumeSource `json:"workspaceVolume,omitempty"`

	// backoffLimit is how many times a failing sync Job is retried before the
	// Job is marked failed.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// activeDeadlineSeconds caps the wall clock duration of a sync Job.
	// +optional
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=30
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// ttlSecondsAfterFinished is how long a finished sync Job is kept before the
	// cluster's TTL controller removes it. Its outcome is recorded in
	// status.syncHistory either way.
	// +optional
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// nodeSelector constrains the sync Pod to matching nodes.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations applied to the sync Pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// affinity applied to the sync Pod.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// priorityClassName applied to the sync Pod.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// podMetadata adds labels and annotations to the sync Pod, for example to
	// opt the Pod out of a service mesh sidecar.
	// +optional
	PodMetadata *PodMetadata `json:"podMetadata,omitempty"`
}

// PodMetadata holds labels and annotations to merge onto generated Pods.
type PodMetadata struct {
	// labels merged onto the Pod.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations merged onto the Pod.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SyncSpec tunes how image contents are copied into the bucket.
type SyncSpec struct {
	// deleteRemoved removes objects from the bucket that no longer exist in the
	// image, so the bucket is a mirror of the image directory rather than an
	// accumulation of every version ever published. Turn it off if the bucket
	// also holds objects written by something other than this operator.
	// +optional
	// +kubebuilder:default=true
	DeleteRemoved *bool `json:"deleteRemoved,omitempty"`

	// acl is the canned ACL applied to uploaded objects. Set it to "private" on
	// providers that reject object ACLs; a publicRead bucket policy still makes
	// the site readable.
	// +optional
	// +kubebuilder:default=public-read
	ACL CannedACL `json:"acl,omitempty"`

	// keyPrefix uploads the site under a prefix inside the bucket instead of at
	// its root, for example "docs/" to serve the site at "/docs/". Deletion of
	// removed objects is confined to this prefix.
	// +optional
	// +kubebuilder:validation:MaxLength=900
	// +kubebuilder:validation:Pattern=`^$|^[^/].*$`
	KeyPrefix string `json:"keyPrefix,omitempty"`

	// excludes are globs, relative to the site root, that are not uploaded.
	// Objects already in the bucket that match an exclude are left alone rather
	// than deleted, even when deleteRemoved is on.
	// +optional
	Excludes []string `json:"excludes,omitempty"`

	// cacheControl sets Cache-Control headers per glob. Rules are evaluated in
	// order and the first match wins, so list specific patterns before broad
	// ones. Files matched by no rule fall back to the operator's
	// --default-cache-control, which is empty by default.
	// +optional
	CacheControl []GlobRule `json:"cacheControl,omitempty"`

	// contentTypes overrides the Content-Type that would be derived from a
	// file's extension. Rules are evaluated in order and the first match wins.
	// +optional
	ContentTypes []GlobRule `json:"contentTypes,omitempty"`

	// defaultContentType is used for files whose extension is unknown and which
	// match no contentTypes rule.
	// +optional
	// +kubebuilder:default="application/octet-stream"
	// +kubebuilder:validation:MinLength=1
	DefaultContentType string `json:"defaultContentType,omitempty"`

	// concurrency is how many objects are uploaded or deleted in parallel.
	// +optional
	// +kubebuilder:default=8
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Concurrency *int32 `json:"concurrency,omitempty"`

	// retainHistory keeps this many sync attempts in status.syncHistory.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	RetainHistory *int32 `json:"retainHistory,omitempty"`

	// job tunes the Kubernetes Job that runs the sync.
	// +optional
	// +kubebuilder:default={}
	Job SyncJobSpec `json:"job,omitzero"`
}

// ServiceSpec configures the ExternalName Service the operator maintains for the
// site.
//
// The Service is a CNAME-shaped handle on the bucket's website hostname. Its
// main use is external-dns: annotate it with the hostnames you want and
// external-dns publishes CNAME records pointing at the bucket, so a site gets
// its own DNS name without anybody editing a zone by hand.
type ServiceSpec struct {
	// enabled creates and maintains the Service. Turn it off for sites reached
	// only through a CDN you configure elsewhere.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// name overrides the Service name, which defaults to the StaticSite's own
	// name.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name,omitempty"`

	// externalName overrides the hostname the Service points at. By default the
	// operator uses the host of status.websiteURL.
	//
	// A Service can only point at a hostname, never at a path. Providers whose
	// website URL is path style, "endpoint/bucket/", therefore cannot be
	// expressed as a CNAME alone: the record will resolve to the endpoint and a
	// proxy in front of it has to add the bucket prefix.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ExternalName string `json:"externalName,omitempty"`

	// hostnames are the DNS names this site should answer on. They are rendered
	// into the external-dns hostname annotation, whose key the operator's
	// --external-dns-hostname-annotation flag controls, so the common case needs
	// no annotation boilerplate. Anything set in annotations wins over this.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Hostnames []string `json:"hostnames,omitempty"`

	// annotations are merged onto the Service, and override any annotation the
	// operator would generate. Use this for the rest of an external-dns
	// configuration, such as a record TTL or a provider-specific hint.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// labels are merged onto the Service.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// StaticSiteSpec defines the desired state of StaticSite.
type StaticSiteSpec struct {
	// source describes the container image holding the built site.
	// +required
	Source SourceSpec `json:"source"`

	// bucket describes the S3 bucket that serves the site.
	// +required
	Bucket BucketSpec `json:"bucket"`

	// sync tunes how content is copied into the bucket.
	// +optional
	// +kubebuilder:default={}
	Sync SyncSpec `json:"sync,omitzero"`

	// service configures the ExternalName Service pointing at the bucket's
	// website hostname, which is how external-dns publishes DNS for the site.
	// +optional
	// +kubebuilder:default={}
	Service ServiceSpec `json:"service,omitzero"`

	// suspend stops reconciliation of this StaticSite. In-flight sync Jobs are
	// left to finish. Use it to freeze a site during an incident without deleting
	// the resource, which would delete the bucket.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// BucketOwnership records whether this operator created the bucket, and the
// token that proves it. The same token is written into the bucket itself, so the
// operator can distinguish a bucket it created from one that merely has the
// right name. Deletion requires both records to agree.
type BucketOwnership struct {
	// created is true when this operator created the bucket. When false the
	// bucket was adopted and will never be deleted by the operator.
	// +optional
	Created bool `json:"created,omitempty"`

	// token is a random value written into the bucket's ownership marker at
	// creation time. It is not a credential: it only proves that the bucket the
	// operator is looking at is the one it created.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Token string `json:"token,omitempty"`

	// createdAt is when the operator created the bucket.
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`

	// verifiedAt is when the ownership marker in the bucket was last confirmed to
	// match token.
	// +optional
	VerifiedAt *metav1.Time `json:"verifiedAt,omitempty"`
}

// SyncStats summarises what one sync did.
type SyncStats struct {
	// uploaded is the number of objects created or replaced.
	// +optional
	Uploaded int32 `json:"uploaded,omitempty"`

	// deleted is the number of objects removed from the bucket.
	// +optional
	Deleted int32 `json:"deleted,omitempty"`

	// unchanged is the number of objects already up to date.
	// +optional
	Unchanged int32 `json:"unchanged,omitempty"`

	// skipped is the number of local files excluded by spec.sync.excludes.
	// +optional
	Skipped int32 `json:"skipped,omitempty"`

	// bytesUploaded is the total size of the uploaded objects.
	// +optional
	BytesUploaded int64 `json:"bytesUploaded,omitempty"`

	// duration is how long the sync took, as a Go duration string.
	// +optional
	Duration string `json:"duration,omitempty"`
}

// SyncAttempt records the outcome of one sync Job.
type SyncAttempt struct {
	// jobName is the name of the Job that ran the sync.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// digest is the image digest the Job was pinned to.
	// +optional
	Digest string `json:"digest,omitempty"`

	// startTime is when the Job started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is when the Job reached a terminal state.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// succeeded reports whether the Job completed successfully.
	// +optional
	Succeeded bool `json:"succeeded,omitempty"`

	// message carries the failure reason when succeeded is false.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Message string `json:"message,omitempty"`

	// stats summarises the work the Job did, when it reported any.
	// +optional
	Stats *SyncStats `json:"stats,omitempty"`
}

// StaticSiteStatus defines the observed state of StaticSite.
type StaticSiteStatus struct {
	// conditions represent the current state of the StaticSite.
	//
	// Condition types:
	//   - "Ready":          the site is served from the bucket and up to date
	//   - "BucketReady":    the bucket exists and matches spec.bucket
	//   - "SourceResolved": spec.source.image resolves in the registry
	//   - "Synced":         bucket contents match status.resolvedDigest
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the metadata.generation this status was computed
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is a coarse summary for kubectl output. Branch on conditions
	// instead.
	// +optional
	Phase StaticSitePhase `json:"phase,omitempty"`

	// bucketName is the bucket the operator manages for this site.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// bucketOwnership records whether the operator created the bucket, which is
	// what allows it to be deleted with the StaticSite.
	// +optional
	BucketOwnership *BucketOwnership `json:"bucketOwnership,omitempty"`

	// websiteURL is the endpoint serving the site, when the provider exposes a
	// predictable one.
	// +optional
	WebsiteURL string `json:"websiteURL,omitempty"`

	// serviceName is the ExternalName Service pointing at the site, if one is
	// enabled.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// resolvedImage is spec.source.image rewritten to a digest reference, which
	// is exactly what the sync Job pulls.
	// +optional
	ResolvedImage string `json:"resolvedImage,omitempty"`

	// resolvedDigest is the digest spec.source.image currently points at.
	// +optional
	ResolvedDigest string `json:"resolvedDigest,omitempty"`

	// lastResolveTime is when the image reference was last resolved against the
	// registry.
	// +optional
	LastResolveTime *metav1.Time `json:"lastResolveTime,omitempty"`

	// nextResolveTime is when the operator will next poll the registry for a new
	// digest. Empty for digest-pinned sources and when polling is disabled.
	// +optional
	NextResolveTime *metav1.Time `json:"nextResolveTime,omitempty"`

	// syncedDigest is the digest whose contents are currently in the bucket.
	// +optional
	SyncedDigest string `json:"syncedDigest,omitempty"`

	// syncedSpecHash fingerprints the sync-affecting parts of the spec that were
	// in effect for the last successful sync. When it changes, the operator
	// re-syncs even though the digest is unchanged.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	SyncedSpecHash string `json:"syncedSpecHash,omitempty"`

	// lastSyncTime is when the bucket contents last successfully matched the
	// source.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// lastSyncStats summarises the last successful sync.
	// +optional
	LastSyncStats *SyncStats `json:"lastSyncStats,omitempty"`

	// activeSyncJob is the Job currently performing a sync, if any.
	// +optional
	ActiveSyncJob string `json:"activeSyncJob,omitempty"`

	// lastHandledSyncRequest is the value of the
	// "web.rka.sh/sync-requested-at" annotation that last triggered a sync. The
	// operator compares the annotation against this to tell a new request from
	// one it has already acted on.
	// +optional
	LastHandledSyncRequest string `json:"lastHandledSyncRequest,omitempty"`

	// failedSyncs counts consecutive failed sync attempts. It resets to zero on
	// success and drives the retry backoff.
	// +optional
	FailedSyncs int32 `json:"failedSyncs,omitempty"`

	// syncHistory holds the most recent sync attempts, newest first, capped by
	// spec.sync.retainHistory.
	// +optional
	// +listType=atomic
	SyncHistory []SyncAttempt `json:"syncHistory,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ss;statics,categories=all;web
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.bucketName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSyncTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.websiteURL`,priority=1
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.syncedDigest`,priority=1

// StaticSite is a static website hosted from an S3 bucket, whose contents come
// from a directory inside a container image.
type StaticSite struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StaticSite
	// +required
	Spec StaticSiteSpec `json:"spec"`

	// status defines the observed state of StaticSite
	// +optional
	Status StaticSiteStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StaticSiteList contains a list of StaticSite
type StaticSiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StaticSite `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &StaticSite{}, &StaticSiteList{})
		return nil
	})
}
