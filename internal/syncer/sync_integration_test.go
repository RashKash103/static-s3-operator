//go:build integration

/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// The sync engine's whole job is to make a bucket match a directory, so these
// tests check the bucket afterwards rather than checking that the right SDK calls
// were made. They run against a real S3 API: whether an unchanged file is
// skipped, whether a removed file is deleted, and which headers an object ends up
// with are all facts about the server, not about this package.
//
// Run them with `make test-integration`.
package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-logr/logr/testr"

	operators3 "github.com/rashkash103/static-s3-operator/internal/s3"
)

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func testClient(t *testing.T) *awss3.Client {
	t.Helper()

	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set S3_TEST_ENDPOINT to run the sync integration tests, or use `make test-integration`")
	}

	client, err := operators3.NewClient(operators3.ClientOptions{
		Region:         envOrDefault("S3_TEST_REGION", "us-east-1"),
		Endpoint:       endpoint,
		ForcePathStyle: true,
		Credentials: operators3.Credentials{
			AccessKeyID:     envOrDefault("S3_TEST_ACCESS_KEY", "rustfsadmin"),
			SecretAccessKey: envOrDefault("S3_TEST_SECRET_KEY", "rustfsadmin"),
		},
		MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("build S3 client: %v", err)
	}
	return client
}

// testBucket creates a bucket for one test and removes it afterwards.
func testBucket(t *testing.T, client *awss3.Client) string {
	t.Helper()

	manager := operators3.NewBucketManager(client)
	name := fmt.Sprintf("sso-sync-%d", time.Now().UnixNano())

	if _, err := manager.Create(context.Background(), name, envOrDefault("S3_TEST_REGION", "us-east-1")); err != nil {
		t.Fatalf("create bucket %q: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := manager.Delete(context.Background(), name); err != nil {
			t.Logf("cleanup of bucket %q failed: %v", name, err)
		}
	})
	return name
}

// sampleSite writes a small static site to disk, the same shape as the sample the
// end-to-end suite builds into an image.
func sampleSite(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for relative, contents := range files {
		target := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("create directory for %q: %v", relative, err)
		}
		if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %q: %v", relative, err)
		}
	}
	return root
}

func defaultSite() map[string]string {
	return map[string]string{
		"index.html":        "<h1>home</h1>",
		"404.html":          "<h1>not found</h1>",
		"about/index.html":  "<h1>about</h1>",
		"assets/app.css":    "body{margin:0}",
		"assets/app.js":     "console.log('hi')",
		"assets/app.js.map": `{"version":3}`,
		"robots.txt":        "User-agent: *\n",
	}
}

func syncOptions(t *testing.T, client *awss3.Client, bucket, dir string) Options {
	t.Helper()
	return Options{
		Client:             client,
		Bucket:             bucket,
		SourceDir:          dir,
		DeleteRemoved:      true,
		ACL:                "public-read",
		DefaultContentType: "application/octet-stream",
		Concurrency:        4,
		Log:                testr.New(t),
	}
}

// listKeys returns every object key in the bucket.
func listKeys(t *testing.T, client *awss3.Client, bucket string) []string {
	t.Helper()

	var keys []string
	paginator := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			t.Fatalf("list objects: %v", err)
		}
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}
	return keys
}

func headObject(t *testing.T, client *awss3.Client, bucket, key string) *awss3.HeadObjectOutput {
	t.Helper()

	out, err := client.HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("head %q: %v", key, err)
	}
	return out
}

func getObject(t *testing.T, client *awss3.Client, bucket, key string) string {
	t.Helper()

	out, err := client.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	buffer := make([]byte, aws.ToInt64(out.ContentLength))
	if _, err := out.Body.Read(buffer); err != nil && err.Error() != "EOF" {
		t.Fatalf("read %q: %v", key, err)
	}
	return string(buffer)
}

func TestSyncUploadsTheWholeSite(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	files := defaultSite()
	dir := sampleSite(t, files)

	result, err := Sync(ctx, syncOptions(t, client, bucket, dir))
	if err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	if result.Uploaded != len(files) {
		t.Errorf("Uploaded = %d, want %d", result.Uploaded, len(files))
	}
	if result.Deleted != 0 || result.Unchanged != 0 {
		t.Errorf("a first sync should only upload, got %+v", result)
	}

	keys := listKeys(t, client, bucket)
	if len(keys) != len(files) {
		t.Errorf("the bucket holds %d objects, want %d: %v", len(keys), len(files), keys)
	}
	for key := range files {
		if !contains(keys, key) {
			t.Errorf("object %q is missing from the bucket", key)
		}
	}

	if got := getObject(t, client, bucket, "index.html"); got != files["index.html"] {
		t.Errorf("index.html contents = %q", got)
	}
}

func TestSyncSetsContentTypesFromExtensions(t *testing.T) {
	// The reason this matters: an object served as application/octet-stream is
	// downloaded by the browser instead of rendered, so a site with wrong content
	// types is a broken site even though every byte is present.
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, defaultSite())

	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	want := map[string]string{
		"index.html":     "text/html; charset=utf-8",
		"assets/app.css": "text/css; charset=utf-8",
		"assets/app.js":  "text/javascript; charset=utf-8",
		"robots.txt":     "text/plain; charset=utf-8",
	}
	for key, expected := range want {
		if got := aws.ToString(headObject(t, client, bucket, key).ContentType); got != expected {
			t.Errorf("Content-Type of %q = %q, want %q", key, got, expected)
		}
	}
}

func TestSyncAppliesCacheControlRules(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, defaultSite())

	options := syncOptions(t, client, bucket, dir)
	options.CacheControl = []Rule{
		{Pattern: "assets/**", Value: "public, max-age=31536000, immutable"},
		{Pattern: "**/*.html", Value: "public, max-age=0, must-revalidate"},
	}
	options.DefaultCacheControl = "public, max-age=300"

	if _, err := Sync(ctx, options); err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	want := map[string]string{
		"assets/app.css":   "public, max-age=31536000, immutable",
		"index.html":       "public, max-age=0, must-revalidate",
		"about/index.html": "public, max-age=0, must-revalidate",
		"robots.txt":       "public, max-age=300",
	}
	for key, expected := range want {
		if got := aws.ToString(headObject(t, client, bucket, key).CacheControl); got != expected {
			t.Errorf("Cache-Control of %q = %q, want %q", key, got, expected)
		}
	}
}

func TestSyncContentTypeOverrides(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, map[string]string{"feed.xml": "<rss/>", "data.bin": "binary"})

	options := syncOptions(t, client, bucket, dir)
	options.ContentTypes = []Rule{{Pattern: "*.xml", Value: "application/rss+xml"}}

	if _, err := Sync(ctx, options); err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	if got := aws.ToString(headObject(t, client, bucket, "feed.xml").ContentType); got != "application/rss+xml" {
		t.Errorf("Content-Type of feed.xml = %q, want the override", got)
	}
	if got := aws.ToString(headObject(t, client, bucket, "data.bin").ContentType); got != "application/octet-stream" {
		t.Errorf("Content-Type of data.bin = %q, want the default", got)
	}
}

func TestSyncSkipsUnchangedFiles(t *testing.T) {
	// Re-uploading a site that has not changed would make a daily poll cost as much
	// as a full publish, so this is the property that makes the poll interval
	// affordable.
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, defaultSite())

	first, err := Sync(ctx, syncOptions(t, client, bucket, dir))
	if err != nil {
		t.Fatalf("the first Sync returned an error: %v", err)
	}

	second, err := Sync(ctx, syncOptions(t, client, bucket, dir))
	if err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}

	if second.Uploaded != 0 {
		t.Errorf("the second sync uploaded %d objects, want none", second.Uploaded)
	}
	if second.Unchanged != first.Uploaded {
		t.Errorf("Unchanged = %d, want %d", second.Unchanged, first.Uploaded)
	}
	if second.Deleted != 0 {
		t.Errorf("the second sync deleted %d objects, want none", second.Deleted)
	}
}

func TestSyncReuploadsChangedContentOfTheSameLength(t *testing.T) {
	// Files extracted from an image all carry the image's build time, so a
	// timestamp comparison would miss this entirely. Same length, different bytes,
	// has to count as changed.
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)

	dir := sampleSite(t, map[string]string{"index.html": "<h1>aaaa</h1>"})
	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("the first Sync returned an error: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>bbbb</h1>"), 0o644); err != nil {
		t.Fatalf("rewrite index.html: %v", err)
	}

	result, err := Sync(ctx, syncOptions(t, client, bucket, dir))
	if err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}
	if result.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1", result.Uploaded)
	}
	if got := getObject(t, client, bucket, "index.html"); got != "<h1>bbbb</h1>" {
		t.Errorf("index.html contents = %q, want the new bytes", got)
	}
}

func TestSyncDeletesRemovedFiles(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)

	dir := sampleSite(t, map[string]string{
		"index.html":     "home",
		"old.html":       "gone soon",
		"assets/app.css": "body{}",
	})
	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("the first Sync returned an error: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "old.html")); err != nil {
		t.Fatalf("remove old.html: %v", err)
	}

	result, err := Sync(ctx, syncOptions(t, client, bucket, dir))
	if err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", result.Deleted)
	}

	keys := listKeys(t, client, bucket)
	if contains(keys, "old.html") {
		t.Errorf("old.html should have been deleted: %v", keys)
	}
	if !contains(keys, "index.html") {
		t.Errorf("index.html should still be present: %v", keys)
	}
}

func TestSyncLeavesRemovedFilesWhenDeleteRemovedIsOff(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)

	dir := sampleSite(t, map[string]string{"index.html": "home", "old.html": "stays"})
	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("the first Sync returned an error: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "old.html")); err != nil {
		t.Fatalf("remove old.html: %v", err)
	}

	options := syncOptions(t, client, bucket, dir)
	options.DeleteRemoved = false
	result, err := Sync(ctx, options)
	if err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}
	if result.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", result.Deleted)
	}
	if !contains(listKeys(t, client, bucket), "old.html") {
		t.Error("old.html should have been left in place")
	}
}

func TestSyncExcludesAreNeitherUploadedNorDeleted(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)

	dir := sampleSite(t, map[string]string{
		"index.html":    "home",
		"assets/app.js": "code",
		"app.js.map":    "sourcemap",
	})

	options := syncOptions(t, client, bucket, dir)
	options.Excludes = []string{"**/*.map"}

	result, err := Sync(ctx, options)
	if err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
	if contains(listKeys(t, client, bucket), "app.js.map") {
		t.Error("an excluded file should not be uploaded")
	}

	// An object matching an exclude belongs to whoever put it there, so a mirroring
	// sync must leave it alone rather than delete it.
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("app.js.map"),
		Body:   strings.NewReader("someone else's object"),
	}); err != nil {
		t.Fatalf("PutObject returned an error: %v", err)
	}

	if _, err := Sync(ctx, options); err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}
	if !contains(listKeys(t, client, bucket), "app.js.map") {
		t.Error("an object matching an exclude must not be deleted by a mirroring sync")
	}
}

func TestSyncNeverTouchesTheOwnershipMarker(t *testing.T) {
	// Deleting the ownership marker would leave the operator unable to prove it
	// created the bucket, and so unable to ever delete it.
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, map[string]string{"index.html": "home"})

	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(operators3.MarkerKey),
		Body:   strings.NewReader(`{"version":1,"token":"abc"}`),
	}); err != nil {
		t.Fatalf("PutObject returned an error: %v", err)
	}

	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	if !contains(listKeys(t, client, bucket), operators3.MarkerKey) {
		t.Error("the ownership marker was deleted by a mirroring sync")
	}
}

func TestSyncUnderAKeyPrefix(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, map[string]string{"index.html": "docs home", "assets/app.css": "body{}"})

	options := syncOptions(t, client, bucket, dir)
	options.KeyPrefix = "docs"

	if _, err := Sync(ctx, options); err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	keys := listKeys(t, client, bucket)
	for _, want := range []string{"docs/index.html", "docs/assets/app.css"} {
		if !contains(keys, want) {
			t.Errorf("object %q is missing: %v", want, keys)
		}
	}

	// An object outside the prefix belongs to something else and must survive.
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("other/index.html"),
		Body:   strings.NewReader("not ours"),
	}); err != nil {
		t.Fatalf("PutObject returned an error: %v", err)
	}
	if _, err := Sync(ctx, options); err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}
	if !contains(listKeys(t, client, bucket), "other/index.html") {
		t.Error("a prefixed sync must not delete objects outside its prefix")
	}
}

func TestSyncStoresItsOwnContentHash(t *testing.T) {
	// The stored hash is what makes the comparison exact for objects whose ETag is
	// not an MD5, which is every object uploaded in more than one part.
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, map[string]string{"index.html": "home"})

	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("Sync returned an error: %v", err)
	}

	head := headObject(t, client, bucket, "index.html")
	if _, present := head.Metadata[contentMD5Metadata]; !present {
		t.Errorf("the object carries no %q metadata: %v", contentMD5Metadata, head.Metadata)
	}
}

func TestSyncOnAnEmptyDirectoryEmptiesTheBucket(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)

	dir := sampleSite(t, map[string]string{"index.html": "home"})
	if _, err := Sync(ctx, syncOptions(t, client, bucket, dir)); err != nil {
		t.Fatalf("the first Sync returned an error: %v", err)
	}

	empty := t.TempDir()
	result, err := Sync(ctx, syncOptions(t, client, bucket, empty))
	if err != nil {
		t.Fatalf("the second Sync returned an error: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", result.Deleted)
	}
	if keys := listKeys(t, client, bucket); len(keys) != 0 {
		t.Errorf("the bucket should be empty, holds %v", keys)
	}
}

func TestSyncRejectsBadConfiguration(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	bucket := testBucket(t, client)
	dir := sampleSite(t, map[string]string{"index.html": "home"})

	options := syncOptions(t, client, bucket, dir)
	options.Excludes = []string{""}
	if _, err := Sync(ctx, options); err == nil {
		t.Error("an empty exclude pattern should be rejected")
	}

	options = syncOptions(t, client, bucket, dir)
	options.CacheControl = []Rule{{Pattern: "", Value: "no-store"}}
	if _, err := Sync(ctx, options); err == nil {
		t.Error("an empty cache-control pattern should be rejected")
	}

	options = syncOptions(t, client, bucket, dir)
	options.Client = nil
	if _, err := Sync(ctx, options); err == nil {
		t.Error("a missing client should be rejected")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
