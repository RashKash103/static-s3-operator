/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package examples checks that everything in examples/ and config/samples/ is
// actually accepted by the CRD.
//
// Documentation that does not apply is worse than no documentation, and a manifest
// in a README drifts silently as the API changes. These tests apply each one to a
// real API server, so a renamed field or a tightened pattern breaks the build
// rather than someone's afternoon.
package examples

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

var (
	testEnv    *envtest.Environment
	testClient client.Client
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: firstEnvtestBinaryDir(),
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "register the core types: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	if err := webv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "register the API types: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	testClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// firstEnvtestBinaryDir finds the envtest binaries `make setup-envtest` downloaded,
// so the tests also run from an editor.
func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}

func TestExamplesAreAccepted(t *testing.T) {
	for _, dir := range []string{
		filepath.Join("..", "..", "examples"),
		filepath.Join("..", "..", "config", "samples"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %q: %v", dir, err)
		}

		found := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".yaml") || name == "kustomization.yaml" {
				continue
			}
			found++

			t.Run(filepath.Base(dir)+"/"+name, func(t *testing.T) {
				applyManifest(t, filepath.Join(dir, name))
			})
		}
		if found == 0 {
			t.Errorf("no example manifests found in %q", dir)
		}
	}
}

// applyManifest creates every document in a file in its own namespace, then
// removes them. Creating them is the test: the API server applies the CRD's
// schema, its defaults and its validation rules.
func applyManifest(t *testing.T, path string) {
	t.Helper()

	raw, err := os.ReadFile(path) //nolint:gosec // repository-local documentation
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}

	ctx := context.Background()
	// A namespace per file keeps two examples that use the same resource name from
	// colliding.
	namespace := fmt.Sprintf("example-%d", time.Now().UnixNano())
	if err := testClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	objects := 0
	for index, document := range splitDocuments(string(raw)) {
		object := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(document), object); err != nil {
			t.Fatalf("document %d in %q is not valid YAML: %v", index, path, err)
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.GetKind() == "" || object.GetAPIVersion() == "" {
			t.Fatalf("document %d in %q has no kind or apiVersion", index, path)
		}

		object.SetNamespace(namespace)
		if err := testClient.Create(ctx, object); err != nil {
			t.Fatalf("the API server rejected %s/%s from %q: %v",
				object.GetKind(), object.GetName(), path, err)
		}
		objects++

		t.Cleanup(func() {
			_ = testClient.Delete(ctx, object)
		})

		if object.GetKind() == "StaticSite" {
			assertDefaultsApplied(t, ctx, namespace, object.GetName())
		}
	}

	if objects == 0 {
		t.Fatalf("%q contains no resources", path)
	}
}

// assertDefaultsApplied reads a created StaticSite back and checks that the CRD's
// defaults landed, which is what lets an example leave most fields out.
func assertDefaultsApplied(t *testing.T, ctx context.Context, namespace, name string) {
	t.Helper()

	site := &webv1alpha1.StaticSite{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, site); err != nil {
		t.Fatalf("read back the StaticSite: %v", err)
	}

	if site.Spec.Bucket.Create == nil {
		t.Error("spec.bucket.create should be defaulted by the CRD")
	}
	if site.Spec.Bucket.DeletionPolicy == "" {
		t.Error("spec.bucket.deletionPolicy should be defaulted by the CRD")
	}
	if site.Spec.Sync.DeleteRemoved == nil {
		t.Error("spec.sync.deleteRemoved should be defaulted by the CRD")
	}
	if site.Spec.Sync.Job.BackoffLimit == nil {
		t.Error("spec.sync.job.backoffLimit should be defaulted by the CRD")
	}
	if site.Spec.Service.Enabled == nil {
		t.Error("spec.service.enabled should be defaulted by the CRD")
	}
}

// splitDocuments splits a multi-document YAML file on its separators.
func splitDocuments(contents string) []string {
	var documents []string
	for part := range strings.SplitSeq(contents, "\n---") {
		if strings.TrimSpace(stripComments(part)) != "" {
			documents = append(documents, part)
		}
	}
	return documents
}

// stripComments removes comment-only lines, so a file that is entirely comments is
// recognised as empty rather than parsed as a document.
func stripComments(document string) string {
	var kept []string
	for line := range strings.SplitSeq(document, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestBucketNameIsImmutable(t *testing.T) {
	// The CEL rule on spec.bucket.name is the guard that stops a site being pointed
	// at a different bucket, orphaning the first one and adopting the second.
	ctx := context.Background()
	namespace := fmt.Sprintf("immutable-%d", time.Now().UnixNano())
	if err := testClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	site := &webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: namespace},
		Spec: webv1alpha1.StaticSiteSpec{
			Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/docs:latest"},
			Bucket: webv1alpha1.BucketSpec{Name: "acme-docs"},
		},
	}
	if err := testClient.Create(ctx, site); err != nil {
		t.Fatalf("create StaticSite: %v", err)
	}

	site.Spec.Bucket.Name = "acme-docs-renamed"
	err := testClient.Update(ctx, site)
	if err == nil {
		t.Fatal("changing spec.bucket.name should be rejected")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Errorf("error should explain that the field is immutable, got: %v", err)
	}
}

func TestInvalidSpecsAreRejected(t *testing.T) {
	ctx := context.Background()
	namespace := fmt.Sprintf("invalid-%d", time.Now().UnixNano())
	if err := testClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*webv1alpha1.StaticSite)
	}{
		{
			name:   "an empty image",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Source.Image = "" },
		},
		{
			name:   "a relative source path",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Source.Path = "usr/share/nginx/html" },
		},
		{
			name:   "an uppercase bucket name",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.Name = "Acme-Docs" },
		},
		{
			name:   "a bucket name that is too short",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.Name = "ab" },
		},
		{
			name: "an index document containing a slash",
			mutate: func(s *webv1alpha1.StaticSite) {
				s.Spec.Bucket.Website = &webv1alpha1.WebsiteSpec{IndexDocument: "nested/index.html"}
			},
		},
		{
			name:   "an unknown ACL",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Sync.ACL = "public-read-write" },
		},
		{
			name:   "an unknown deletion policy",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Bucket.DeletionPolicy = "Destroy" },
		},
		{
			name:   "a key prefix starting with a slash",
			mutate: func(s *webv1alpha1.StaticSite) { s.Spec.Sync.KeyPrefix = "/docs" },
		},
		{
			name: "a hostname that is not a hostname",
			mutate: func(s *webv1alpha1.StaticSite) {
				s.Spec.Service.Hostnames = []string{"https://docs.example.com"}
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			site := &webv1alpha1.StaticSite{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("invalid-%d", index),
					Namespace: namespace,
				},
				Spec: webv1alpha1.StaticSiteSpec{
					Source: webv1alpha1.SourceSpec{Image: "ghcr.io/acme/docs:latest"},
					Bucket: webv1alpha1.BucketSpec{Name: "acme-docs"},
				},
			}
			test.mutate(site)

			if err := testClient.Create(context.Background(), site); err == nil {
				_ = testClient.Delete(context.Background(), site)
				t.Errorf("the API server should have rejected %s", test.name)
			}
		})
	}
}

func TestInvalidPullIntervalIsRejected(t *testing.T) {
	// metav1.Duration cannot hold an invalid value, so this goes in as raw YAML,
	// which is also how a user would hit it.
	ctx := context.Background()
	namespace := fmt.Sprintf("duration-%d", time.Now().UnixNano())
	if err := testClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	manifest := fmt.Sprintf(`
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: bad-duration
  namespace: %s
spec:
  source:
    image: ghcr.io/acme/docs:latest
    pullInterval: "every day"
  bucket:
    name: acme-docs
`, namespace)

	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), object); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	if err := testClient.Create(ctx, object); err == nil {
		_ = testClient.Delete(ctx, object)
		t.Fatal("a pull interval that is not a Go duration should be rejected")
	}
}
