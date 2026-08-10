/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package syncer

import (
	"strings"
	"testing"
)

func TestContentTypeForKey(t *testing.T) {
	// These are the types a browser refuses to render if they are wrong, so they
	// are asserted exactly rather than left to whatever mime database the machine
	// running the tests happens to have.
	tests := map[string]string{
		"index.html":       "text/html; charset=utf-8",
		"assets/app.css":   "text/css; charset=utf-8",
		"assets/app.js":    "text/javascript; charset=utf-8",
		"assets/app.mjs":   "text/javascript; charset=utf-8",
		"data.json":        "application/json; charset=utf-8",
		"app.js.map":       "application/json; charset=utf-8",
		"logo.svg":         "image/svg+xml; charset=utf-8",
		"photo.webp":       "image/webp",
		"photo.avif":       "image/avif",
		"favicon.ico":      "image/x-icon",
		"font.woff2":       "font/woff2",
		"font.ttf":         "font/ttf",
		"module.wasm":      "application/wasm",
		"site.webmanifest": "application/manifest+json; charset=utf-8",
		"feed.atom":        "application/atom+xml; charset=utf-8",
		"robots.txt":       "text/plain; charset=utf-8",
	}

	for key, want := range tests {
		if got := contentTypeForKey(key); got != want {
			t.Errorf("contentTypeForKey(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestContentTypeForKeyIsCaseInsensitiveOnExtension(t *testing.T) {
	if got := contentTypeForKey("INDEX.HTML"); got != "text/html; charset=utf-8" {
		t.Errorf("contentTypeForKey(\"INDEX.HTML\") = %q, want the html type", got)
	}
}

func TestContentTypeForKeyReturnsEmptyWithoutExtension(t *testing.T) {
	for _, key := range []string{"LICENSE", "assets/Makefile", ""} {
		if got := contentTypeForKey(key); got != "" {
			t.Errorf("contentTypeForKey(%q) = %q, want an empty string", key, got)
		}
	}
}

func TestUploaderContentTypeFallsBackToTheDefault(t *testing.T) {
	uploader := &objectUploader{options: Options{DefaultContentType: "application/octet-stream"}}

	if got := uploader.contentType("LICENSE"); got != "application/octet-stream" {
		t.Errorf("contentType(\"LICENSE\") = %q, want the configured default", got)
	}
	if got := uploader.contentType("index.html"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("contentType(\"index.html\") = %q, want the html type", got)
	}
}

func TestUploaderContentTypeOverrideWins(t *testing.T) {
	rules, err := CompileGlobValues([]Rule{{Pattern: "**/*.html", Value: "text/html"}})
	if err != nil {
		t.Fatalf("CompileGlobValues returned an error: %v", err)
	}
	uploader := &objectUploader{
		types:   rules,
		options: Options{DefaultContentType: "application/octet-stream"},
	}

	if got := uploader.contentType("index.html"); got != "text/html" {
		t.Errorf("contentType(\"index.html\") = %q, want the override", got)
	}
}

func TestUploaderCacheControlFallsBackToTheDefault(t *testing.T) {
	rules, err := CompileGlobValues([]Rule{{Pattern: "assets/**", Value: "immutable"}})
	if err != nil {
		t.Fatalf("CompileGlobValues returned an error: %v", err)
	}
	uploader := &objectUploader{
		cache:   rules,
		options: Options{DefaultCacheControl: "public, max-age=60"},
	}

	if got := uploader.cacheControl("assets/app.css"); got != "immutable" {
		t.Errorf("cacheControl(\"assets/app.css\") = %q, want the rule value", got)
	}
	if got := uploader.cacheControl("index.html"); got != "public, max-age=60" {
		t.Errorf("cacheControl(\"index.html\") = %q, want the default", got)
	}
}

func TestNormalizeKeyPrefix(t *testing.T) {
	tests := map[string]string{
		"":          "",
		"docs":      "docs/",
		"docs/":     "docs/",
		"/docs":     "docs/",
		"  docs  ":  "docs/",
		"a/b":       "a/b/",
		"a/b/":      "a/b/",
		"/a/b/":     "a/b/",
		"   ":       "",
		"releases/": "releases/",
	}

	for input, want := range tests {
		if got := normalizeKeyPrefix(input); got != want {
			t.Errorf("normalizeKeyPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsPlainMD5ETag(t *testing.T) {
	tests := map[string]bool{
		"d41d8cd98f00b204e9800998ecf8427e":   true,
		"D41D8CD98F00B204E9800998ECF8427E":   true,
		"d41d8cd98f00b204e9800998ecf8427e-2": false,
		"":                                   false,
		"short":                              false,
		"g41d8cd98f00b204e9800998ecf8427e":   false,
	}

	for etag, want := range tests {
		if got := isPlainMD5ETag(etag); got != want {
			t.Errorf("isPlainMD5ETag(%q) = %v, want %v", etag, got, want)
		}
	}
}
