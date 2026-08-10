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
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

// legalBucketName is the shape S3 requires, and the shape the documentation
// promises a generated name will have.
var legalBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func TestGenerateBucketNameUsesNamespaceAndName(t *testing.T) {
	// The default format is documented so that somebody can create the bucket by
	// hand, or scope an IAM policy to it, without asking the cluster. If this
	// changes, docs/configuration.md has to change with it.
	defaults := New()

	name, err := defaults.GenerateBucketName(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
	})
	if err != nil {
		t.Fatalf("GenerateBucketName returned an error: %v", err)
	}
	if name != "web-docs" {
		t.Errorf("GenerateBucketName = %q, want %q", name, "web-docs")
	}
}

func TestGenerateBucketNameIsReproducible(t *testing.T) {
	defaults := New()
	site := &webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web", UID: types.UID("abc")},
	}

	first, err := defaults.GenerateBucketName(site)
	if err != nil {
		t.Fatalf("GenerateBucketName returned an error: %v", err)
	}
	second, err := defaults.GenerateBucketName(site)
	if err != nil {
		t.Fatalf("GenerateBucketName returned an error: %v", err)
	}
	if first != second {
		t.Errorf("the same resource produced %q then %q", first, second)
	}
}

func TestGenerateBucketNameHonoursTheTemplate(t *testing.T) {
	defaults := New()
	defaults.BucketNameTemplate = "acme-{{.Namespace}}-{{.Name}}-sites"

	name, err := defaults.GenerateBucketName(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
	})
	if err != nil {
		t.Fatalf("GenerateBucketName returned an error: %v", err)
	}
	if name != "acme-web-docs-sites" {
		t.Errorf("GenerateBucketName = %q", name)
	}
}

func TestGenerateBucketNameCanUseTheUID(t *testing.T) {
	defaults := New()
	defaults.BucketNameTemplate = "{{.Namespace}}-{{.Name}}-{{.UID}}"

	name, err := defaults.GenerateBucketName(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{
			Name: "docs", Namespace: "web",
			UID: types.UID("6f2c0b8e-3a1d-4e5f-8a9b-0c1d2e3f4a5b"),
		},
	})
	if err != nil {
		t.Fatalf("GenerateBucketName returned an error: %v", err)
	}
	if !legalBucketName.MatchString(name) {
		t.Errorf("GenerateBucketName = %q, which is not a legal bucket name", name)
	}
	if !strings.HasPrefix(name, "web-docs-") {
		t.Errorf("GenerateBucketName = %q, want the readable part kept", name)
	}
}

func TestGenerateBucketNameRejectsABadTemplate(t *testing.T) {
	defaults := New()

	defaults.BucketNameTemplate = "{{.Namespace"
	if _, err := defaults.GenerateBucketName(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
	}); err == nil {
		t.Error("an unparsable template should be reported")
	}

	defaults.BucketNameTemplate = "{{.Nope}}"
	if _, err := defaults.GenerateBucketName(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
	}); err == nil {
		t.Error("a template naming an unknown field should be reported")
	}
}

func TestSanitizeBucketName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "already legal", input: "web-docs", want: "web-docs"},
		{name: "uppercase is lowered", input: "Web-Docs", want: "web-docs"},
		{
			name: "dots become hyphens, because a dot breaks virtual-host TLS",
			// A StaticSite name is a DNS subdomain, so it really can contain dots.
			input: "web-docs.example.com",
			want:  "web-docs-example-com",
		},
		{name: "underscores and spaces become hyphens", input: "web_docs site", want: "web-docs-site"},
		{name: "runs of hyphens collapse", input: "web---docs", want: "web-docs"},
		{name: "leading and trailing hyphens are trimmed", input: "-web-docs-", want: "web-docs"},
		{name: "surrounding whitespace is ignored", input: "  web-docs  ", want: "web-docs"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SanitizeBucketName(test.input)
			if got != test.want {
				t.Errorf("SanitizeBucketName(%q) = %q, want %q", test.input, got, test.want)
			}
			if !legalBucketName.MatchString(got) {
				t.Errorf("SanitizeBucketName(%q) = %q, which is not a legal bucket name", test.input, got)
			}
		})
	}
}

func TestSanitizeBucketNameShortensLongNames(t *testing.T) {
	long := "very-long-namespace-name-that-goes-on" + strings.Repeat("-and-on", 10) + "-site"

	got := SanitizeBucketName(long)
	if len(got) > maxBucketNameLength {
		t.Errorf("SanitizeBucketName produced %d characters: %q", len(got), got)
	}
	if !legalBucketName.MatchString(got) {
		t.Errorf("SanitizeBucketName produced an illegal name: %q", got)
	}

	// Two names that differ only past the truncation point must not collide, or two
	// sites would silently publish into one bucket.
	other := SanitizeBucketName(long + "-different-tail")
	if got == other {
		t.Errorf("two long names collided on %q", got)
	}
}

func TestSanitizeBucketNamePadsShortNames(t *testing.T) {
	got := SanitizeBucketName("a")
	if len(got) < minBucketNameLength {
		t.Errorf("SanitizeBucketName(%q) = %q, which is too short", "a", got)
	}
	if !legalBucketName.MatchString(got) {
		t.Errorf("SanitizeBucketName produced an illegal name: %q", got)
	}
}

func TestSanitizeBucketNameAvoidsShapesS3Rejects(t *testing.T) {
	// S3 refuses a name that looks like an IP address, and reserves a handful of
	// affixes. A namespace and name can produce any of them.
	tests := []string{
		"10-0-0-1",
		"192.168.1.1",
		"xn--example",
		"sthree-bucket",
		"bucket-s3alias",
		"bucket--ol-s3",
	}

	for _, input := range tests {
		got := SanitizeBucketName(input)
		if !legalBucketName.MatchString(got) {
			t.Errorf("SanitizeBucketName(%q) = %q, which is not a legal bucket name", input, got)
		}
		if looksLikeIPAddress.MatchString(got) {
			t.Errorf("SanitizeBucketName(%q) = %q, which still looks like an IP address", input, got)
		}
		if hasReservedAffix(got) {
			t.Errorf("SanitizeBucketName(%q) = %q, which still uses a reserved affix", input, got)
		}
	}
}

func TestSanitizeBucketNameReportsHopelessInput(t *testing.T) {
	for _, input := range []string{"", "   ", "---", "!!!"} {
		if got := SanitizeBucketName(input); got != "" {
			t.Errorf("SanitizeBucketName(%q) = %q, want an empty string", input, got)
		}
	}
}

func TestEffectiveGeneratesABucketNameWhenNoneIsGiven(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
		Spec: webv1alpha1.StaticSiteSpec{
			Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/docs:latest"},
		},
	})
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.Bucket != "web-docs" {
		t.Errorf("Bucket = %q, want the generated name", effective.Bucket)
	}
	if !effective.BucketGenerated {
		t.Error("BucketGenerated should report that the name was derived")
	}
}

func TestEffectiveKeepsTheRecordedBucketName(t *testing.T) {
	// The stickiness that matters: changing the cluster's template must not move an
	// existing site to a different bucket and orphan the old one.
	defaults := New()
	defaults.SyncerImage = "operator:v1"
	defaults.BucketNameTemplate = "brand-new-{{.Namespace}}-{{.Name}}"

	effective, err := defaults.Effective(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
		Spec: webv1alpha1.StaticSiteSpec{
			Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/docs:latest"},
		},
		Status: webv1alpha1.StaticSiteStatus{BucketName: "web-docs"},
	})
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.Bucket != "web-docs" {
		t.Errorf("Bucket = %q, want the name already in status", effective.Bucket)
	}
}

func TestEffectiveSpecNameWinsOverGeneration(t *testing.T) {
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	effective, err := defaults.Effective(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
		Spec: webv1alpha1.StaticSiteSpec{
			Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/docs:latest"},
			Bucket: webv1alpha1.BucketSpec{Name: "chosen-by-hand"},
		},
	})
	if err != nil {
		t.Fatalf("Effective returned an error: %v", err)
	}
	if effective.Bucket != "chosen-by-hand" {
		t.Errorf("Bucket = %q", effective.Bucket)
	}
	if effective.BucketGenerated {
		t.Error("BucketGenerated should be false for a name given in the spec")
	}
}

func TestEffectiveRefusesToMoveAnExistingSiteToANewBucket(t *testing.T) {
	// Adding spec.bucket.name to a site that already publishes to a generated
	// bucket would orphan that bucket. Refusing is what makes it visible.
	defaults := New()
	defaults.SyncerImage = "operator:v1"

	_, err := defaults.Effective(&webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "web"},
		Spec: webv1alpha1.StaticSiteSpec{
			Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/docs:latest"},
			Bucket: webv1alpha1.BucketSpec{Name: "somewhere-else"},
		},
		Status: webv1alpha1.StaticSiteStatus{BucketName: "web-docs"},
	})
	if err == nil {
		t.Fatal("pointing an existing site at a different bucket should be refused")
	}
	if !strings.Contains(err.Error(), "web-docs") {
		t.Errorf("the error should name the bucket in use, got: %v", err)
	}
}
