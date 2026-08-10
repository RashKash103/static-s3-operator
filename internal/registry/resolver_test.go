/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package registry

import (
	"context"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// startRegistry runs an in-memory OCI registry, which is what lets these tests
// exercise the real HTTP protocol without a container runtime or a network.
func startRegistry(t *testing.T) string {
	t.Helper()

	// The registry logs each request; routing it through the test's own logger
	// keeps that quiet unless the test fails.
	logger := log.New(testWriter{t}, "registry: ", 0)
	server := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(logger)))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	return parsed.Host
}

// testWriter sends the registry's log output to the test's own log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func pushRandomImage(t *testing.T, reference string) v1.Hash {
	t.Helper()

	image, err := random.Image(256, 2)
	if err != nil {
		t.Fatalf("build random image: %v", err)
	}
	return pushImage(t, reference, image)
}

func pushImage(t *testing.T, reference string, image v1.Image) v1.Hash {
	t.Helper()

	ref, err := name.ParseReference(reference, name.Insecure)
	if err != nil {
		t.Fatalf("parse %q: %v", reference, err)
	}
	if err := remote.Write(ref, image); err != nil {
		t.Fatalf("push %q: %v", reference, err)
	}

	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}

func TestResolveTagToDigest(t *testing.T) {
	host := startRegistry(t)
	reference := host + "/acme/site:latest"
	want := pushRandomImage(t, reference)

	resolved, err := Resolve(context.Background(), reference, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}

	if resolved.Digest != want.String() {
		t.Errorf("Digest = %q, want %q", resolved.Digest, want.String())
	}
	if expected := host + "/acme/site@" + want.String(); resolved.Pinned != expected {
		t.Errorf("Pinned = %q, want %q", resolved.Pinned, expected)
	}
	if resolved.WasPinned {
		t.Error("a tagged reference should not report WasPinned")
	}
}

func TestResolveFollowsAMovedTag(t *testing.T) {
	// This is the behaviour the whole pull interval exists for: the tag stays the
	// same and the digest underneath it changes.
	host := startRegistry(t)
	reference := host + "/acme/site:latest"

	first := pushRandomImage(t, reference)
	resolved, err := Resolve(context.Background(), reference, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}
	if resolved.Digest != first.String() {
		t.Fatalf("Digest = %q, want %q", resolved.Digest, first.String())
	}

	second := pushRandomImage(t, reference)
	if first == second {
		t.Fatal("the second random image should have a different digest")
	}

	resolved, err = Resolve(context.Background(), reference, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}
	if resolved.Digest != second.String() {
		t.Errorf("Digest = %q, want the new digest %q", resolved.Digest, second.String())
	}
}

func TestResolveDigestReferenceMakesNoRequest(t *testing.T) {
	// A digest cannot move, so resolving one must not need the registry at all.
	// That is what lets a digest-pinned site work when only the nodes have pull
	// credentials.
	reference := "registry.invalid/acme/site@sha256:" + strings.Repeat("ab", 32)

	resolved, err := Resolve(context.Background(), reference, Options{})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}
	if !resolved.WasPinned {
		t.Error("a digest reference should report WasPinned")
	}
	if resolved.Digest != "sha256:"+strings.Repeat("ab", 32) {
		t.Errorf("Digest = %q", resolved.Digest)
	}
	if resolved.Pinned != reference {
		t.Errorf("Pinned = %q, want the input reference", resolved.Pinned)
	}
}

func TestResolveMissingTagIsTerminal(t *testing.T) {
	host := startRegistry(t)

	_, err := Resolve(context.Background(), host+"/acme/missing:latest", Options{PlainHTTP: true})
	if err == nil {
		t.Fatal("resolving a missing tag should fail")
	}
	if !IsTerminal(err) {
		t.Errorf("error %v should be classified as terminal so the controller stops retrying fast", err)
	}
}

func TestResolveRejectsAnEmptyReference(t *testing.T) {
	if _, err := Resolve(context.Background(), "   ", Options{}); err == nil {
		t.Fatal("an empty reference should be rejected")
	}
}

func TestResolveRejectsAnUnparsableReference(t *testing.T) {
	_, err := Resolve(context.Background(), "NOT A REFERENCE", Options{})
	if err == nil {
		t.Fatal("an unparsable reference should be rejected")
	}
	if IsTerminal(err) {
		t.Error("a parse failure is not a registry transport error")
	}
}

func TestResolveIndexReturnsTheIndexDigest(t *testing.T) {
	// Pinning the index rather than one platform's manifest leaves the sync Job
	// free to pick the platform it can actually extract.
	host := startRegistry(t)
	reference := host + "/acme/multiarch:latest"

	index, err := random.Index(128, 1, 2)
	if err != nil {
		t.Fatalf("build random index: %v", err)
	}
	ref, err := name.ParseReference(reference, name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.WriteIndex(ref, index); err != nil {
		t.Fatalf("push index: %v", err)
	}
	want, err := index.Digest()
	if err != nil {
		t.Fatalf("index digest: %v", err)
	}

	resolved, err := Resolve(context.Background(), reference, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}
	if resolved.Digest != want.String() {
		t.Errorf("Digest = %q, want the index digest %q", resolved.Digest, want.String())
	}
}

func TestImageFetchesTheResolvedReference(t *testing.T) {
	host := startRegistry(t)
	reference := host + "/acme/site:latest"

	source, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("build random image: %v", err)
	}
	// Give the image a config so it round-trips like a real one.
	source, err = mutate.Config(source, v1.Config{Env: []string{"SITE=1"}})
	if err != nil {
		t.Fatalf("set config: %v", err)
	}
	digest := pushImage(t, reference, source)

	resolved, err := Resolve(context.Background(), reference, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}

	fetched, err := Image(context.Background(), resolved.Pinned, nil, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Image returned an error: %v", err)
	}
	fetchedDigest, err := fetched.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if fetchedDigest != digest {
		t.Errorf("fetched digest = %q, want %q", fetchedDigest, digest)
	}
}

func TestImageSelectsAPlatformFromAnIndex(t *testing.T) {
	host := startRegistry(t)
	reference := host + "/acme/multiarch:latest"

	index, err := random.Index(64, 1, 2)
	if err != nil {
		t.Fatalf("build random index: %v", err)
	}
	ref, err := name.ParseReference(reference, name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.WriteIndex(ref, index); err != nil {
		t.Fatalf("push index: %v", err)
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatalf("index manifest: %v", err)
	}
	if len(manifest.Manifests) == 0 || manifest.Manifests[0].Platform == nil {
		t.Skip("the random index carries no platform information to select on")
	}

	platform := manifest.Manifests[0].Platform
	fetched, err := Image(context.Background(), reference, platform, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Image returned an error: %v", err)
	}
	fetchedDigest, err := fetched.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if fetchedDigest != manifest.Manifests[0].Digest {
		t.Errorf("fetched digest = %q, want the platform manifest %q",
			fetchedDigest, manifest.Manifests[0].Digest)
	}
}

func TestReferenceShortDigest(t *testing.T) {
	reference := Reference{Digest: "sha256:0123456789abcdef0123456789abcdef"}
	if got := reference.ShortDigest(); got != "0123456789ab" {
		t.Errorf("ShortDigest() = %q", got)
	}

	short := Reference{Digest: "sha256:abc"}
	if got := short.ShortDigest(); got != "abc" {
		t.Errorf("ShortDigest() = %q", got)
	}
}

func TestEmptyImageResolves(t *testing.T) {
	// A site image with no layers is legal, and the operator should report a
	// digest for it rather than an error.
	host := startRegistry(t)
	reference := host + "/acme/empty:latest"
	want := pushImage(t, reference, empty.Image)

	resolved, err := Resolve(context.Background(), reference, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}
	if resolved.Digest != want.String() {
		t.Errorf("Digest = %q, want %q", resolved.Digest, want.String())
	}
}
