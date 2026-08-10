/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package config holds the operator's cluster-wide settings: the defaults that
// fill in optional StaticSite fields, and the knobs that control how the
// operator talks to registries and S3 providers.
//
// Every setting here is a command line flag on the manager, which the Helm
// chart renders from its values file. Fields deliberately have no CRD-level
// default when the cluster operator should be able to choose it once for the
// whole cluster: spec.source.path, spec.source.pullInterval,
// spec.bucket.region, spec.bucket.endpoint and the website documents all work
// that way.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
)

// Built-in defaults that appear in more than one place.
const (
	defaultRegion        = "us-east-1"
	defaultIndexDocument = "index.html"
)

// Defaults are the operator-wide settings.
type Defaults struct {
	// SourcePath fills in spec.source.path.
	SourcePath string
	// PullInterval fills in spec.source.pullInterval. Zero disables polling.
	PullInterval time.Duration
	// BucketNameTemplate generates spec.bucket.name when a site leaves it out. It
	// is a Go template evaluated with {{.OpaqueID}}, {{.ShortID}}, {{.Namespace}},
	// {{.Name}} and {{.UID}}.
	BucketNameTemplate string
	// BucketNameUUIDNamespace is the UUID namespace {{.OpaqueID}} is derived
	// under, which is what keeps a generated name from being guessable. Empty means
	// read it from the cluster at startup.
	BucketNameUUIDNamespace string
	// Region fills in spec.bucket.region.
	Region string
	// Endpoint fills in spec.bucket.endpoint. Empty means AWS S3.
	Endpoint string
	// ForcePathStyle fills in spec.bucket.forcePathStyle.
	ForcePathStyle bool
	// WebsiteEndpointTemplate fills in spec.bucket.websiteEndpointTemplate.
	WebsiteEndpointTemplate string
	// IndexDocument fills in spec.bucket.website.indexDocument.
	IndexDocument string
	// ErrorDocument fills in spec.bucket.website.errorDocument.
	ErrorDocument string
	// CacheControl is the Cache-Control header for objects matched by no
	// spec.sync.cacheControl rule. Empty sends no header.
	CacheControl string

	// ExternalDNSHostnameAnnotation is the annotation key that spec.service
	// hostnames are rendered into. It matches external-dns by default, and can be
	// pointed at another controller's key or emptied to render nothing.
	ExternalDNSHostnameAnnotation string

	// SyncerImage is the image used for sync Jobs. It defaults to the operator's
	// own image, which ships the syncer binary, and is normally set by the Helm
	// chart to the exact digest of the running operator.
	SyncerImage string
	// SyncerImagePullPolicy is the pull policy for sync Jobs.
	SyncerImagePullPolicy string

	// CredentialsSecret is the "namespace/name" (or bare "name", meaning the
	// operator's own namespace) of the Secret holding the S3 credentials used by
	// sites that do not name their own.
	CredentialsSecret string
	// AccessKeyIDKey is the key in CredentialsSecret holding the access key ID.
	AccessKeyIDKey string
	// SecretAccessKeyKey is the key holding the secret access key.
	SecretAccessKeyKey string
	// SessionTokenKey is the key holding an optional session token.
	SessionTokenKey string
	// MirrorCredentials lets the operator copy the default credentials Secret
	// into a site's namespace so the sync Job can read it. With this off, a site
	// in another namespace must name its own credentials Secret.
	MirrorCredentials bool

	// RequestChecksumWhenSupported opts in to the AWS SDK's newer default of
	// sending trailing CRC32 checksums on every upload. Several S3-compatible
	// providers reject those requests, so the operator sends checksums only when
	// the API requires them unless this is enabled.
	RequestChecksumWhenSupported bool
	// SkipTLSVerify disables certificate verification for S3 endpoints. For
	// development against a self-signed endpoint only.
	SkipTLSVerify bool

	// MaxExtractBytes caps the total uncompressed size a sync Job will extract
	// from an image, as protection against a decompression bomb.
	MaxExtractBytes int64
	// MaxExtractFiles caps the number of files a sync Job will extract.
	MaxExtractFiles int

	// SyncRetryBaseDelay is the first retry delay after a failed sync. Later
	// retries back off exponentially up to SyncRetryMaxDelay.
	SyncRetryBaseDelay time.Duration
	// SyncRetryMaxDelay caps the retry backoff after repeated sync failures.
	SyncRetryMaxDelay time.Duration
	// OwnershipRecheckInterval is how often a healthy, idle site re-verifies its
	// bucket configuration and ownership marker.
	OwnershipRecheckInterval time.Duration
	// MaxConcurrentReconciles is the controller's worker count.
	MaxConcurrentReconciles int

	// Namespace is the namespace the operator runs in, used to resolve a bare
	// CredentialsSecret name and as the source for credential mirroring.
	Namespace string
}

// New returns the built-in defaults, before flags are parsed.
func New() *Defaults {
	return &Defaults{
		SourcePath:   "/usr/share/nginx/html",
		PullInterval: 24 * time.Hour,
		// A bucket name ends up in public DNS, so the default carries no cluster
		// namespace or resource name. See OpaqueSiteID.
		BucketNameTemplate: "static-s3-{{.OpaqueID}}",
		Region:             defaultRegion,
		IndexDocument:      defaultIndexDocument,
		ErrorDocument:      "404.html",

		ExternalDNSHostnameAnnotation: "external-dns.alpha.kubernetes.io/hostname",

		SyncerImagePullPolicy:    "IfNotPresent",
		CredentialsSecret:        "static-s3-operator-s3-credentials",
		AccessKeyIDKey:           "accessKeyID",
		SecretAccessKeyKey:       "secretAccessKey",
		SessionTokenKey:          "sessionToken",
		MirrorCredentials:        true,
		MaxExtractBytes:          2 << 30, // 2 GiB
		MaxExtractFiles:          200_000,
		SyncRetryBaseDelay:       30 * time.Second,
		SyncRetryMaxDelay:        30 * time.Minute,
		OwnershipRecheckInterval: time.Hour,
		MaxConcurrentReconciles:  4,
		Namespace:                os.Getenv("POD_NAMESPACE"),
	}
}

// BindFlags registers every setting on fs.
func (d *Defaults) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&d.SourcePath, "default-source-path", d.SourcePath,
		"Directory inside the image used when a StaticSite omits spec.source.path.")
	fs.DurationVar(&d.PullInterval, "default-pull-interval", d.PullInterval,
		"How often to re-resolve a mutable image tag when a StaticSite omits spec.source.pullInterval. "+
			"Zero disables polling.")
	fs.StringVar(&d.BucketNameTemplate, "default-bucket-name-template", d.BucketNameTemplate,
		"Go template generating a bucket name when a StaticSite omits spec.bucket.name, evaluated "+
			"with {{.OpaqueID}}, {{.ShortID}}, {{.Namespace}}, {{.Name}} and {{.UID}}. A bucket name "+
			"reaches public DNS, so prefer the opaque identifier over the namespace and name.")
	fs.StringVar(&d.BucketNameUUIDNamespace, "bucket-name-uuid-namespace", d.BucketNameUUIDNamespace,
		"UUID namespace that {{.OpaqueID}} is derived under, which is what makes a generated name "+
			"unguessable. Empty reads it from the cluster, which is stable per cluster; set it "+
			"explicitly to keep names stable across clusters.")
	fs.StringVar(&d.Region, "default-region", d.Region,
		"S3 region used when a StaticSite omits spec.bucket.region.")
	fs.StringVar(&d.Endpoint, "default-endpoint", d.Endpoint,
		"S3 API endpoint used when a StaticSite omits spec.bucket.endpoint. Empty means AWS S3.")
	fs.BoolVar(&d.ForcePathStyle, "default-force-path-style", d.ForcePathStyle,
		"Address buckets as endpoint/bucket when a StaticSite omits spec.bucket.forcePathStyle. "+
			"Most S3-compatible providers need this.")
	fs.StringVar(&d.WebsiteEndpointTemplate, "default-website-endpoint-template", d.WebsiteEndpointTemplate,
		"Go template for status.websiteURL, evaluated with {{.Bucket}}, {{.Region}} and {{.Endpoint}}. "+
			"Used when a StaticSite omits spec.bucket.websiteEndpointTemplate.")
	fs.StringVar(&d.IndexDocument, "default-index-document", d.IndexDocument,
		"Index document used when a StaticSite omits spec.bucket.website.indexDocument.")
	fs.StringVar(&d.ErrorDocument, "default-error-document", d.ErrorDocument,
		"Error document used when a StaticSite omits spec.bucket.website.errorDocument. "+
			"Empty configures no error document.")
	fs.StringVar(&d.CacheControl, "default-cache-control", d.CacheControl,
		"Cache-Control header for objects matched by no spec.sync.cacheControl rule. Empty sends no header.")

	fs.StringVar(&d.ExternalDNSHostnameAnnotation, "external-dns-hostname-annotation",
		d.ExternalDNSHostnameAnnotation,
		"Annotation key that spec.service.hostnames are rendered into on the generated "+
			"ExternalName Service. Empty renders no hostname annotation.")

	fs.StringVar(&d.SyncerImage, "syncer-image", d.SyncerImage,
		"Image for sync Jobs. Defaults to the operator's own image, which ships the syncer binary.")
	fs.StringVar(&d.SyncerImagePullPolicy, "syncer-image-pull-policy", d.SyncerImagePullPolicy,
		"Pull policy for sync Jobs: Always, IfNotPresent or Never.")

	fs.StringVar(&d.CredentialsSecret, "default-credentials-secret", d.CredentialsSecret,
		"Secret holding the S3 credentials for sites that do not name their own, as "+
			"\"namespace/name\" or a bare name in the operator's namespace.")
	fs.StringVar(&d.AccessKeyIDKey, "default-credentials-access-key-id-key", d.AccessKeyIDKey,
		"Key in the default credentials Secret holding the access key ID.")
	fs.StringVar(&d.SecretAccessKeyKey, "default-credentials-secret-access-key-key", d.SecretAccessKeyKey,
		"Key in the default credentials Secret holding the secret access key.")
	fs.StringVar(&d.SessionTokenKey, "default-credentials-session-token-key", d.SessionTokenKey,
		"Key in the default credentials Secret holding an optional session token.")
	fs.BoolVar(&d.MirrorCredentials, "mirror-default-credentials", d.MirrorCredentials,
		"Copy the default credentials Secret into a site's namespace so its sync Job can read it. "+
			"Disable it to require every StaticSite to reference credentials in its own namespace.")

	fs.BoolVar(&d.RequestChecksumWhenSupported, "s3-request-checksum-when-supported",
		d.RequestChecksumWhenSupported,
		"Send trailing CRC32 checksums on every S3 upload. Off by default because several "+
			"S3-compatible providers reject those requests.")
	fs.BoolVar(&d.SkipTLSVerify, "s3-skip-tls-verify", d.SkipTLSVerify,
		"Do not verify S3 endpoint certificates. For development only.")

	fs.Int64Var(&d.MaxExtractBytes, "max-extract-bytes", d.MaxExtractBytes,
		"Largest total uncompressed size a sync Job will extract from an image.")
	fs.IntVar(&d.MaxExtractFiles, "max-extract-files", d.MaxExtractFiles,
		"Largest number of files a sync Job will extract from an image.")

	fs.DurationVar(&d.SyncRetryBaseDelay, "sync-retry-base-delay", d.SyncRetryBaseDelay,
		"First retry delay after a failed sync; later retries back off exponentially.")
	fs.DurationVar(&d.SyncRetryMaxDelay, "sync-retry-max-delay", d.SyncRetryMaxDelay,
		"Upper bound on the retry delay after repeated sync failures.")
	fs.DurationVar(&d.OwnershipRecheckInterval, "ownership-recheck-interval", d.OwnershipRecheckInterval,
		"How often an idle, healthy site re-verifies its bucket configuration and ownership marker.")
	fs.IntVar(&d.MaxConcurrentReconciles, "max-concurrent-reconciles", d.MaxConcurrentReconciles,
		"Number of StaticSite reconcile workers.")
}

// Validate reports settings that would fail later in a confusing place.
func (d *Defaults) Validate() error {
	var problems []string

	if !strings.HasPrefix(d.SourcePath, "/") {
		problems = append(problems, "--default-source-path must be an absolute path")
	}
	if d.PullInterval < 0 {
		problems = append(problems, "--default-pull-interval must not be negative")
	}
	if d.Region == "" {
		problems = append(problems, "--default-region must not be empty")
	}
	if d.BucketNameTemplate == "" {
		problems = append(problems, "--default-bucket-name-template must not be empty")
	} else if _, err := template.New("bucketName").Parse(d.BucketNameTemplate); err != nil {
		problems = append(problems, fmt.Sprintf("--default-bucket-name-template is not a valid template: %v", err))
	}
	if templateUsesOpaqueID(d.BucketNameTemplate) && d.BucketNameUUIDNamespace == "" {
		problems = append(problems,
			"--bucket-name-uuid-namespace is required by --default-bucket-name-template but could not "+
				"be read from the cluster")
	}
	if d.BucketNameUUIDNamespace != "" {
		if _, err := uuid.Parse(d.BucketNameUUIDNamespace); err != nil {
			problems = append(problems,
				fmt.Sprintf("--bucket-name-uuid-namespace is not a UUID: %v", err))
		}
	}
	if d.IndexDocument == "" {
		problems = append(problems, "--default-index-document must not be empty")
	}
	if strings.Contains(d.IndexDocument, "/") {
		problems = append(problems, "--default-index-document must not contain a slash")
	}
	switch d.SyncerImagePullPolicy {
	case "Always", "IfNotPresent", "Never":
	default:
		problems = append(problems, "--syncer-image-pull-policy must be Always, IfNotPresent or Never")
	}
	if d.SyncerImage == "" {
		problems = append(problems,
			"--syncer-image must be set (normally to the operator's own image, which ships the syncer)")
	}
	if d.MaxExtractBytes <= 0 {
		problems = append(problems, "--max-extract-bytes must be positive")
	}
	if d.MaxExtractFiles <= 0 {
		problems = append(problems, "--max-extract-files must be positive")
	}
	if d.SyncRetryBaseDelay <= 0 {
		problems = append(problems, "--sync-retry-base-delay must be positive")
	}
	if d.SyncRetryMaxDelay < d.SyncRetryBaseDelay {
		problems = append(problems, "--sync-retry-max-delay must be at least --sync-retry-base-delay")
	}
	if d.MaxConcurrentReconciles < 1 {
		problems = append(problems, "--max-concurrent-reconciles must be at least 1")
	}
	if d.CredentialsSecret != "" && strings.Count(d.CredentialsSecret, "/") > 1 {
		problems = append(problems, "--default-credentials-secret must be \"name\" or \"namespace/name\"")
	}
	if d.CredentialsSecret != "" && !strings.Contains(d.CredentialsSecret, "/") && d.Namespace == "" {
		problems = append(problems,
			"--default-credentials-secret is a bare name but the operator namespace is unknown; "+
				"set POD_NAMESPACE or qualify the secret as \"namespace/name\"")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid operator configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

// CredentialsSecretRef splits CredentialsSecret into a namespace and a name.
// The namespace falls back to the operator's own. Both are empty when no
// default credentials Secret is configured.
func (d *Defaults) CredentialsSecretRef() (namespace, name string) {
	if d.CredentialsSecret == "" {
		return "", ""
	}
	if ns, n, ok := strings.Cut(d.CredentialsSecret, "/"); ok {
		return ns, n
	}
	return d.Namespace, d.CredentialsSecret
}
