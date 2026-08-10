/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package config

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

func site(mutate ...func(*webv1alpha1.StaticSite)) *webv1alpha1.StaticSite {
	s := &webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
		Spec: webv1alpha1.StaticSiteSpec{
			Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/site:latest"},
			Bucket: webv1alpha1.BucketSpec{Name: "acme-docs"},
		},
	}
	for _, apply := range mutate {
		apply(s)
	}
	return s
}

func TestEffectiveFillsInOperatorDefaults(t *testing.T) {
	// These fields have no CRD default on purpose: the cluster operator chooses
	// them once, through the Helm chart, for every site.
	defaults := New()
	defaults.SourcePath = "/site"
	defaults.Region = "eu-west-2"
	defaults.PullInterval = 6 * time.Hour
	defaults.IndexDocument = "home.html"
	defaults.ErrorDocument = "oops.html"
	defaults.SyncerImage = "ghcr.io/acme/operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Bucket.Website = &webv1alpha1.WebsiteSpec{}
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}

	if effective.SourcePath != "/site" {
		t.Errorf("SourcePath = %q, want the operator default", effective.SourcePath)
	}
	if effective.Region != "eu-west-2" {
		t.Errorf("Region = %q, want the operator default", effective.Region)
	}
	if effective.PullInterval != 6*time.Hour {
		t.Errorf("PullInterval = %v, want the operator default", effective.PullInterval)
	}
	if effective.IndexDocument != "home.html" {
		t.Errorf("IndexDocument = %q, want the operator default", effective.IndexDocument)
	}
	if effective.ErrorDocument != "oops.html" {
		t.Errorf("ErrorDocument = %q, want the operator default", effective.ErrorDocument)
	}
	if effective.SyncerImage != "ghcr.io/acme/operator:v1" {
		t.Errorf("SyncerImage = %q", effective.SyncerImage)
	}
}

func TestEffectiveSpecValuesWinOverDefaults(t *testing.T) {
	defaults := New()
	defaults.SourcePath = "/site"
	defaults.Region = "eu-west-2"
	defaults.IndexDocument = "home.html"
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Source.Path = "/usr/share/nginx/html/"
		s.Spec.Bucket.Region = "us-west-2"
		s.Spec.Bucket.Website = &webv1alpha1.WebsiteSpec{IndexDocument: "index.html"}
		s.Spec.Sync.Job.Image = "operator:override"
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}

	// The trailing slash is trimmed so the path compares equal however it is
	// written.
	if effective.SourcePath != "/usr/share/nginx/html" {
		t.Errorf("SourcePath = %q", effective.SourcePath)
	}
	if effective.Region != "us-west-2" {
		t.Errorf("Region = %q", effective.Region)
	}
	if effective.IndexDocument != "index.html" {
		t.Errorf("IndexDocument = %q", effective.IndexDocument)
	}
	if effective.SyncerImage != "operator:override" {
		t.Errorf("SyncerImage = %q", effective.SyncerImage)
	}
}

func TestEffectiveZeroPullIntervalDisablesPolling(t *testing.T) {
	// A zero interval has to mean "never poll", which is different from "unset, so
	// use the default". That distinction is why the field is a pointer.
	defaults := New()
	defaults.PullInterval = 24 * time.Hour
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Source.PullInterval = &metav1.Duration{Duration: 0}
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.PullInterval != 0 {
		t.Errorf("PullInterval = %v, want 0", effective.PullInterval)
	}
}

func TestEffectiveWebsiteDisabledWhenNull(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.WebsiteEnabled {
		t.Error("website hosting should be off when spec.bucket.website is null")
	}
}

func TestEffectiveEmptyDefaultErrorDocumentMeansNone(t *testing.T) {
	defaults := New()
	defaults.ErrorDocument = ""
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Bucket.Website = &webv1alpha1.WebsiteSpec{}
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.ErrorDocument != "" {
		t.Errorf("ErrorDocument = %q, want none", effective.ErrorDocument)
	}
}

func TestEffectiveNormalizesTheEndpointScheme(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	tests := map[string]string{
		"objects.example.com":         "https://objects.example.com",
		"https://objects.example.com": "https://objects.example.com",
		"http://minio.storage:9000":   "http://minio.storage:9000",
	}

	for input, want := range tests {
		effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
			s.Spec.Bucket.Endpoint = input
		}))
		if err != nil {
			t.Fatalf("Effective returned an error: %v", err)
		}
		if effective.Endpoint != want {
			t.Errorf("Endpoint for %q = %q, want %q", input, effective.Endpoint, want)
		}
	}
}

func TestEffectiveDeletionPolicyDefaultsToDelete(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.DeletionPolicy != webv1alpha1.DeletionPolicyDelete {
		t.Errorf("DeletionPolicy = %q, want Delete", effective.DeletionPolicy)
	}
}

func TestWebsiteURLForAWS(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	tests := map[string]string{
		// AWS keeps the dash form for the older regions.
		"us-east-1": "http://acme-docs.s3-website-us-east-1.amazonaws.com",
		"eu-west-1": "http://acme-docs.s3-website-eu-west-1.amazonaws.com",
		// Newer regions use the dot form.
		"eu-west-3":      "http://acme-docs.s3-website.eu-west-3.amazonaws.com",
		"af-south-1":     "http://acme-docs.s3-website.af-south-1.amazonaws.com",
		"ca-central-1":   "http://acme-docs.s3-website.ca-central-1.amazonaws.com",
		"ap-southeast-2": "http://acme-docs.s3-website-ap-southeast-2.amazonaws.com",
	}

	for region, want := range tests {
		effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
			s.Spec.Bucket.Region = region
		}))
		if err != nil {
			t.Fatalf("Effective returned an error: %v", err)
		}
		if effective.WebsiteURL != want {
			t.Errorf("WebsiteURL for %q = %q, want %q", region, effective.WebsiteURL, want)
		}
	}
}

func TestWebsiteURLForCompatibleProviders(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	tests := []struct {
		name           string
		endpoint       string
		forcePathStyle bool
		want           string
	}{
		{
			name:     "virtual host style prefixes the bucket onto the host",
			endpoint: "https://objects.example.com",
			want:     "https://acme-docs.objects.example.com/",
		},
		{
			name:           "path style appends the bucket to the endpoint",
			endpoint:       "http://minio.storage.svc:9000",
			forcePathStyle: true,
			want:           "http://minio.storage.svc:9000/acme-docs/",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
				s.Spec.Bucket.Endpoint = test.endpoint
				s.Spec.Bucket.ForcePathStyle = ptr.To(test.forcePathStyle)
			}))
			if err != nil {
				t.Fatalf("Effective returned an error: %v", err)
			}
			if effective.WebsiteURL != test.want {
				t.Errorf("WebsiteURL = %q, want %q", effective.WebsiteURL, test.want)
			}
		})
	}
}

func TestWebsiteURLTemplateWins(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Bucket.Region = "eu-central-1"
		s.Spec.Bucket.Endpoint = "https://objects.example.com"
		s.Spec.Bucket.WebsiteEndpointTemplate = "http://{{.Bucket}}.website-{{.Region}}.example.com"
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if want := "http://acme-docs.website-eu-central-1.example.com"; effective.WebsiteURL != want {
		t.Errorf("WebsiteURL = %q, want %q", effective.WebsiteURL, want)
	}
}

func TestWebsiteURLTemplateFromOperatorDefault(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"
	defaults.WebsiteEndpointTemplate = "https://{{.Bucket}}.sites.example.com"

	effective, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if want := "https://acme-docs.sites.example.com"; effective.WebsiteURL != want {
		t.Errorf("WebsiteURL = %q, want %q", effective.WebsiteURL, want)
	}
}

// TestWebsiteURLTemplateEndpointFields pins the template data down. Endpoint used
// to be the whole endpoint, scheme and all, so the natural template for a
// virtual-hosted website host produced "https://bucket.https://host" and the
// ExternalName Service ended up aliasing the hostname "bucket.https" -- a broken
// CNAME rather than an error.
func TestWebsiteURLTemplateEndpointFields(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		template string
		want     string
	}{
		{
			name:     "Endpoint is the bare host",
			endpoint: "us-mia-1.example-object-store.com",
			template: "http://{{.Bucket}}.website-{{.Endpoint}}",
			want:     "http://acme-docs.website-us-mia-1.example-object-store.com",
		},
		{
			name:     "a scheme on the endpoint does not leak into Endpoint",
			endpoint: "https://us-mia-1.example-object-store.com",
			template: "http://{{.Bucket}}.website-{{.Endpoint}}",
			want:     "http://acme-docs.website-us-mia-1.example-object-store.com",
		},
		{
			name:     "Endpoint keeps a port",
			endpoint: "http://minio.storage.svc:9000",
			template: "http://{{.Bucket}}.{{.Endpoint}}",
			want:     "http://acme-docs.minio.storage.svc:9000",
		},
		{
			name:     "EndpointURL carries the scheme",
			endpoint: "us-mia-1.example-object-store.com",
			template: "{{.EndpointURL}}/{{.Bucket}}/",
			want:     "https://us-mia-1.example-object-store.com/acme-docs/",
		},
		{
			name:     "Scheme is available on its own",
			endpoint: "http://minio.storage.svc:9000",
			template: "{{.Scheme}}://{{.Bucket}}.{{.Endpoint}}",
			want:     "http://acme-docs.minio.storage.svc:9000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaults := New()
			defaults.SyncerImage = "operator:v1"

			effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
				s.Spec.Bucket.Endpoint = tc.endpoint
				s.Spec.Bucket.WebsiteEndpointTemplate = tc.template
			}))
			if err != nil {
				t.Fatalf("Effective returned an error: %v", err)
			}
			if effective.WebsiteURL != tc.want {
				t.Errorf("WebsiteURL = %q, want %q", effective.WebsiteURL, tc.want)
			}
		})
	}
}

// TestWebsiteURLTemplateGivesAResolvableCNAME is the consequence that actually
// mattered: whatever the template renders, the Service has to alias a hostname a
// resolver could look up.
func TestWebsiteURLTemplateGivesAResolvableCNAME(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Bucket.Endpoint = "us-mia-1.example-object-store.com"
		s.Spec.Bucket.WebsiteEndpointTemplate = "http://{{.Bucket}}.website-{{.Endpoint}}"
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}

	want := "acme-docs.website-us-mia-1.example-object-store.com"
	if effective.ServiceExternalName != want {
		t.Errorf("ServiceExternalName = %q, want %q", effective.ServiceExternalName, want)
	}
	if strings.Contains(effective.ServiceExternalName, ":") ||
		strings.Contains(effective.ServiceExternalName, "/") {
		t.Errorf("ServiceExternalName %q is not a hostname", effective.ServiceExternalName)
	}
}

func TestWebsiteURLTemplateErrorsAreReported(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	for _, template := range []string{"{{.Bucket", "{{.Nope}}"} {
		_, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
			s.Spec.Bucket.WebsiteEndpointTemplate = template
		}))
		if err == nil {
			t.Errorf("template %q should be reported as an error", template)
		}
	}
}

func TestServiceDefaultsToTheSiteName(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if !effective.ServiceEnabled {
		t.Error("the Service should be enabled by default")
	}
	if effective.ServiceName != "docs" {
		t.Errorf("ServiceName = %q, want the StaticSite name", effective.ServiceName)
	}
	if want := "acme-docs.s3-website-us-east-1.amazonaws.com"; effective.ServiceExternalName != want {
		t.Errorf("ServiceExternalName = %q, want %q", effective.ServiceExternalName, want)
	}
}

func TestServiceHostnamesBecomeTheExternalDNSAnnotation(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Service.Hostnames = []string{"docs.example.com", "www.example.com"}
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}

	got := effective.ServiceAnnotations["external-dns.alpha.kubernetes.io/hostname"]
	if want := "docs.example.com,www.example.com"; got != want {
		t.Errorf("hostname annotation = %q, want %q", got, want)
	}
}

func TestServiceExplicitAnnotationsOverrideGeneratedOnes(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Service.Hostnames = []string{"generated.example.com"}
		s.Spec.Service.Annotations = map[string]string{
			"external-dns.alpha.kubernetes.io/hostname": "explicit.example.com",
			"external-dns.alpha.kubernetes.io/ttl":      "60",
		}
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}

	if got := effective.ServiceAnnotations["external-dns.alpha.kubernetes.io/hostname"]; got != "explicit.example.com" {
		t.Errorf("hostname annotation = %q, want the explicit value", got)
	}
	if got := effective.ServiceAnnotations["external-dns.alpha.kubernetes.io/ttl"]; got != "60" {
		t.Errorf("ttl annotation = %q", got)
	}
}

func TestServiceHostnameAnnotationCanBeDisabled(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"
	defaults.ExternalDNSHostnameAnnotation = ""

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Service.Hostnames = []string{"docs.example.com"}
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if len(effective.ServiceAnnotations) != 0 {
		t.Errorf("ServiceAnnotations = %v, want none", effective.ServiceAnnotations)
	}
}

func TestServiceExternalNameOverrideWins(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(site(func(s *webv1alpha1.StaticSite) {
		s.Spec.Service.ExternalName = "cdn.example.com"
	}))
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.ServiceExternalName != "cdn.example.com" {
		t.Errorf("ServiceExternalName = %q", effective.ServiceExternalName)
	}
}

func TestSyncSpecHashChangesWithSyncAffectingFields(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	baseline, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	baselineHash := baseline.SyncSpecHash()

	tests := map[string]func(*webv1alpha1.StaticSite){
		"source path":    func(s *webv1alpha1.StaticSite) { s.Spec.Source.Path = "/other" },
		"key prefix":     func(s *webv1alpha1.StaticSite) { s.Spec.Sync.KeyPrefix = "docs" },
		"delete removed": func(s *webv1alpha1.StaticSite) { s.Spec.Sync.DeleteRemoved = ptr.To(false) },
		"acl":            func(s *webv1alpha1.StaticSite) { s.Spec.Sync.ACL = webv1alpha1.ACLPrivate },
		"excludes":       func(s *webv1alpha1.StaticSite) { s.Spec.Sync.Excludes = []string{"*.map"} },
		"cache control": func(s *webv1alpha1.StaticSite) {
			s.Spec.Sync.CacheControl = []webv1alpha1.GlobRule{{Pattern: "**", Value: "no-store"}}
		},
		"content types": func(s *webv1alpha1.StaticSite) {
			s.Spec.Sync.ContentTypes = []webv1alpha1.GlobRule{{Pattern: "*.x", Value: "text/plain"}}
		},
		"default type":    func(s *webv1alpha1.StaticSite) { s.Spec.Sync.DefaultContentType = "text/plain" },
		"bucket region":   func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.Region = "eu-west-1" },
		"bucket endpoint": func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.Endpoint = "https://x.example.com" },
		"path style":      func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.ForcePathStyle = ptr.To(true) },
	}

	for name, mutate := range tests {
		effective, err := defaults.Effective(site(mutate))
		if err != nil {
			t.Fatalf("Effective returned an error: %v", err)
		}
		if effective.SyncSpecHash() == baselineHash {
			t.Errorf("changing the %s should change the sync hash", name)
		}
	}
}

func TestSyncSpecHashIgnoresFieldsThatDoNotAffectContent(t *testing.T) {
	// The hash forces a re-sync when it changes, so anything unrelated to what
	// gets uploaded has to stay out of it: otherwise tweaking a Job's memory limit
	// would re-upload every site in the cluster.
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	baseline, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	baselineHash := baseline.SyncSpecHash()

	tests := map[string]func(*webv1alpha1.StaticSite){
		"retain history":   func(s *webv1alpha1.StaticSite) { s.Spec.Sync.RetainHistory = ptr.To(int32(10)) },
		"concurrency":      func(s *webv1alpha1.StaticSite) { s.Spec.Sync.Concurrency = ptr.To(int32(32)) },
		"backoff limit":    func(s *webv1alpha1.StaticSite) { s.Spec.Sync.Job.BackoffLimit = ptr.To(int32(9)) },
		"job image":        func(s *webv1alpha1.StaticSite) { s.Spec.Sync.Job.Image = "operator:v2" },
		"pull interval":    func(s *webv1alpha1.StaticSite) { s.Spec.Source.PullInterval = &metav1.Duration{Duration: time.Hour} },
		"service hostname": func(s *webv1alpha1.StaticSite) { s.Spec.Service.Hostnames = []string{"a.example.com"} },
		"bucket tags":      func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.Tags = map[string]string{"team": "web"} },
		"suspend":          func(s *webv1alpha1.StaticSite) { s.Spec.Suspend = true },
	}

	for name, mutate := range tests {
		effective, err := defaults.Effective(site(mutate))
		if err != nil {
			t.Fatalf("Effective returned an error: %v", err)
		}
		if effective.SyncSpecHash() != baselineHash {
			t.Errorf("changing the %s should not change the sync hash", name)
		}
	}
}

func TestSyncSpecHashIsStable(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	first, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	second, err := defaults.Effective(site())
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if first.SyncSpecHash() != second.SyncSpecHash() {
		t.Error("the same spec should hash to the same value")
	}
}
