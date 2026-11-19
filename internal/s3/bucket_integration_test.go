//go:build integration

/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// These tests run against a real S3 API rather than a mock, because almost
// everything interesting about this package is how a provider actually behaves:
// which optional calls it implements, how it rewrites a bucket policy as it
// stores one, and whether it accepts a location constraint. A mock built from
// this package's own assumptions would agree with those assumptions and prove
// nothing.
//
// Run them with `make test-integration`, which starts a local S3 server, or point
// S3_TEST_ENDPOINT at one yourself.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// testClient connects to the local S3 server, skipping the test when none is
// configured.
func testClient(t *testing.T) *awss3.Client {
	t.Helper()

	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set S3_TEST_ENDPOINT to run the S3 integration tests, or use `make test-integration`")
	}

	client, err := NewClient(ClientOptions{
		Region:   envOrDefault("S3_TEST_REGION", "us-east-1"),
		Endpoint: endpoint,
		// Every S3-compatible server reachable at a bare endpoint needs path style
		// addressing, because "bucket.localhost" does not resolve.
		ForcePathStyle: true,
		Credentials: Credentials{
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

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// httpGet fetches a URL with no credentials at all, which is how a browser
// reaches a public website bucket and the only real test of a public-read policy.
func httpGet(ctx context.Context, url string) (string, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}

	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", response.StatusCode, err
	}
	return string(body), response.StatusCode, nil
}

// testBucket creates a uniquely named bucket and removes it when the test ends,
// so a failed test never leaves the next one a bucket to trip over.
func testBucket(t *testing.T, manager *BucketManager) string {
	t.Helper()

	name := fmt.Sprintf("sso-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := manager.Delete(context.Background(), name); err != nil {
			t.Logf("cleanup of bucket %q failed: %v", name, err)
		}
	})
	return name
}

func TestBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	exists, err := manager.Exists(ctx, bucket)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if exists {
		t.Fatal("a freshly named bucket should not exist yet")
	}

	created, err := manager.Create(ctx, bucket, "us-east-1")
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	if !created {
		t.Error("Create should report that it created the bucket")
	}

	exists, err = manager.Exists(ctx, bucket)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !exists {
		t.Error("the bucket should exist after Create")
	}

	// Creating it again has to be a no-op rather than an error, or every reconcile
	// after the first would fail.
	//
	// Whether it also reports "already existed" is up to the provider. AWS in
	// us-east-1 answers a repeat CreateBucket from the owner with 200 OK, and so
	// do several compatible providers, RustFS among them. That is why the
	// provisioner only calls Create after Exists has said the bucket is absent, and
	// never treats Create's answer as proof of who made a bucket.
	created, err = manager.Create(ctx, bucket, "us-east-1")
	if err != nil {
		t.Fatalf("the second Create returned an error: %v", err)
	}
	if created {
		t.Log("this provider answers a repeat CreateBucket from the owner with success")
	}
}

func TestBucketOwnershipRoundTrip(t *testing.T) {
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	token, err := NewOwnershipToken()
	if err != nil {
		t.Fatalf("NewOwnershipToken returned an error: %v", err)
	}
	own := Ownership{Token: token, Site: "web/docs", UID: "uid-1234"}

	kind, err := manager.WriteOwnership(ctx, bucket, own, map[string]string{"team": "web"})
	if err != nil {
		t.Fatalf("WriteOwnership returned an error: %v", err)
	}
	if kind == MarkerKindNone {
		t.Fatal("ownership must be recorded somewhere, or the bucket could never be deleted safely")
	}
	t.Logf("ownership recorded via %s", kind)

	readBack, readKind, err := manager.ReadOwnership(ctx, bucket)
	if err != nil {
		t.Fatalf("ReadOwnership returned an error: %v", err)
	}
	if readKind != kind {
		t.Errorf("ReadOwnership used %s, want %s", readKind, kind)
	}
	if readBack == nil || readBack.Token != token {
		t.Fatalf("ReadOwnership returned %+v, want token %q", readBack, token)
	}

	owned, reason, err := manager.VerifyOwnership(ctx, bucket, token, "uid-1234")
	if err != nil {
		t.Fatalf("VerifyOwnership returned an error: %v", err)
	}
	if !owned {
		t.Errorf("VerifyOwnership refused the correct token: %s", reason)
	}

	// The checks that stop the operator destroying somebody else's bucket.
	for _, test := range []struct {
		name, token, uid string
	}{
		{name: "a different token", token: "not-the-token", uid: "uid-1234"},
		{name: "no token at all", token: "", uid: "uid-1234"},
	} {
		owned, reason, err := manager.VerifyOwnership(ctx, bucket, test.token, test.uid)
		if err != nil {
			t.Fatalf("VerifyOwnership returned an error: %v", err)
		}
		if owned {
			t.Errorf("VerifyOwnership accepted %s", test.name)
		}
		if reason == "" {
			t.Errorf("VerifyOwnership should explain why it refused %s", test.name)
		}
	}
}

func TestOwnershipTagsSurviveTagReconciliation(t *testing.T) {
	// Reconciling the site's tags must not drop the ownership token, or the bucket
	// would silently become undeletable.
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	token, err := NewOwnershipToken()
	if err != nil {
		t.Fatalf("NewOwnershipToken returned an error: %v", err)
	}
	own := Ownership{Token: token, Site: "web/docs", UID: "uid-1234"}

	kind, err := manager.WriteOwnership(ctx, bucket, own, map[string]string{"team": "web"})
	if err != nil {
		t.Fatalf("WriteOwnership returned an error: %v", err)
	}
	if kind != MarkerKindTags {
		t.Skipf("this provider records ownership via %s, so there are no tags to reconcile", kind)
	}

	if _, err := manager.EnsureTags(ctx, bucket, map[string]string{"team": "platform"}, own); err != nil {
		t.Fatalf("EnsureTags returned an error: %v", err)
	}

	tags, err := manager.getTags(ctx, bucket)
	if err != nil {
		t.Fatalf("getTags returned an error: %v", err)
	}
	if tags[TagOwnershipToken] != token {
		t.Errorf("the ownership token was lost by tag reconciliation: %v", tags)
	}
	if tags["team"] != "platform" {
		t.Errorf("the site's own tag was not updated: %v", tags)
	}

	// A second identical call must report no change, or the operator would rewrite
	// the tag set on every reconcile.
	changed, err := manager.EnsureTags(ctx, bucket, map[string]string{"team": "platform"}, own)
	if err != nil {
		t.Fatalf("EnsureTags returned an error: %v", err)
	}
	if changed {
		t.Error("EnsureTags should be a no-op when the tag set already matches")
	}
}

func TestOwnershipTagsRejectReservedKeys(t *testing.T) {
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	token, err := NewOwnershipToken()
	if err != nil {
		t.Fatalf("NewOwnershipToken returned an error: %v", err)
	}
	own := Ownership{Token: token, Site: "web/docs", UID: "uid-1234"}

	// A site tag using the reserved prefix must not be able to overwrite the
	// ownership record, which would otherwise be a way to claim someone's bucket.
	siteTags := map[string]string{TagOwnershipToken: "forged", "team": "web"}
	if _, err := manager.EnsureTags(ctx, bucket, siteTags, own); err != nil {
		t.Fatalf("EnsureTags returned an error: %v", err)
	}

	tags, err := manager.getTags(ctx, bucket)
	if IsUnsupported(err) || IsMissingConfiguration(err) {
		t.Skip("this provider does not implement bucket tagging")
	}
	if err != nil {
		t.Fatalf("getTags returned an error: %v", err)
	}
	if tags[TagOwnershipToken] != token {
		t.Errorf("a site tag overwrote the ownership token: %v", tags)
	}
}

func TestEnsureWebsite(t *testing.T) {
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	want := WebsiteConfig{IndexDocument: "index.html", ErrorDocument: "404.html"}
	changed, err := manager.EnsureWebsite(ctx, bucket, want)
	if errors.Is(err, ErrWebsiteUnsupported) {
		// Several S3-compatible providers do not implement website configuration, and
		// the operator carries on without it rather than failing the site.
		t.Skipf("this provider does not implement website configuration: %v", err)
	}
	if err != nil {
		t.Fatalf("EnsureWebsite returned an error: %v", err)
	}
	if !changed {
		t.Error("configuring website hosting on a fresh bucket should be a change")
	}

	current, err := manager.getWebsite(ctx, bucket)
	if err != nil {
		t.Fatalf("getWebsite returned an error: %v", err)
	}
	if *current != want {
		t.Errorf("website configuration = %+v, want %+v", *current, want)
	}

	// The second call must be a no-op, or the operator would rewrite the
	// configuration on every reconcile.
	changed, err = manager.EnsureWebsite(ctx, bucket, want)
	if err != nil {
		t.Fatalf("EnsureWebsite returned an error: %v", err)
	}
	if changed {
		t.Error("EnsureWebsite should be a no-op when the configuration already matches")
	}

	updated := WebsiteConfig{IndexDocument: "index.html", ErrorDocument: "index.html"}
	changed, err = manager.EnsureWebsite(ctx, bucket, updated)
	if err != nil {
		t.Fatalf("EnsureWebsite returned an error: %v", err)
	}
	if !changed {
		t.Error("changing the error document should be a change")
	}

	if err := manager.RemoveWebsite(ctx, bucket); err != nil {
		t.Fatalf("RemoveWebsite returned an error: %v", err)
	}
	// Removing it twice has to succeed too, since reconciliation repeats.
	if err := manager.RemoveWebsite(ctx, bucket); err != nil {
		t.Fatalf("the second RemoveWebsite returned an error: %v", err)
	}
}

func TestEnsurePolicyIsIdempotent(t *testing.T) {
	// The point of this test is the round trip: providers rewrite a policy as they
	// store it, and the operator must recognise its own policy coming back or it
	// will rewrite it forever.
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	// Public access blocks are AWS-specific; a provider without them has nothing to
	// clear and must not be treated as failing.
	cleared, err := manager.AllowPublicPolicy(ctx, bucket)
	if err != nil {
		t.Fatalf("AllowPublicPolicy returned an error: %v", err)
	}
	t.Logf("public access block cleared: %v", cleared)

	policy := PublicReadPolicy(bucket)
	changed, err := manager.EnsurePolicy(ctx, bucket, policy)
	if IsUnsupportedRequest(err) {
		t.Skip("this provider does not implement bucket policies")
	}
	if err != nil {
		t.Fatalf("EnsurePolicy returned an error: %v", err)
	}
	if !changed {
		t.Error("applying a policy to a fresh bucket should be a change")
	}

	changed, err = manager.EnsurePolicy(ctx, bucket, policy)
	if err != nil {
		t.Fatalf("the second EnsurePolicy returned an error: %v", err)
	}
	if changed {
		t.Error("EnsurePolicy should be a no-op when the stored policy already matches; " +
			"otherwise the operator rewrites it on every reconcile")
	}

	if err := manager.RemovePolicy(ctx, bucket); err != nil {
		t.Fatalf("RemovePolicy returned an error: %v", err)
	}
	if err := manager.RemovePolicy(ctx, bucket); err != nil {
		t.Fatalf("the second RemovePolicy returned an error: %v", err)
	}
}

func TestPublicReadPolicyMakesObjectsAnonymouslyReadable(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	manager := NewBucketManager(client)
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	if _, err := manager.AllowPublicPolicy(ctx, bucket); err != nil {
		t.Fatalf("AllowPublicPolicy returned an error: %v", err)
	}
	if _, err := manager.EnsurePolicy(ctx, bucket, PublicReadPolicy(bucket)); err != nil {
		if IsUnsupported(err) {
			t.Skip("this provider does not implement bucket policies")
		}
		t.Fatalf("EnsurePolicy returned an error: %v", err)
	}

	body := "<h1>hello</h1>"
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String("index.html"),
		Body:        strings.NewReader(body),
		ContentType: aws.String("text/html"),
	}); err != nil {
		t.Fatalf("PutObject returned an error: %v", err)
	}

	// An unauthenticated GET is exactly what a browser does, and it is the only
	// thing that proves the policy works.
	url := fmt.Sprintf("%s/%s/index.html",
		strings.TrimSuffix(envOrDefault("S3_TEST_ENDPOINT", ""), "/"), bucket)
	fetched, status, err := httpGet(ctx, url)
	if err != nil {
		t.Fatalf("anonymous GET returned an error: %v", err)
	}
	if status != 200 {
		t.Fatalf("anonymous GET returned status %d, want 200; the public-read policy is not in effect", status)
	}
	if fetched != body {
		t.Errorf("anonymous GET returned %q, want %q", fetched, body)
	}
}

func TestEmptyAndDelete(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	manager := NewBucketManager(client)
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	// Enough objects to make sure batching works rather than only the single-page
	// case.
	const objectCount = 25
	for i := range objectCount {
		key := fmt.Sprintf("assets/file-%02d.txt", i)
		if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   strings.NewReader(fmt.Sprintf("contents %d", i)),
		}); err != nil {
			t.Fatalf("PutObject %q returned an error: %v", key, err)
		}
	}
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("index.html"),
		Body:   strings.NewReader("home"),
	}); err != nil {
		t.Fatalf("PutObject returned an error: %v", err)
	}

	// A prefixed Empty must leave everything outside the prefix alone.
	removed, err := manager.Empty(ctx, bucket, "assets/")
	if err != nil {
		t.Fatalf("Empty returned an error: %v", err)
	}
	if removed != objectCount {
		t.Errorf("Empty removed %d objects, want %d", removed, objectCount)
	}

	remaining, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("ListObjectsV2 returned an error: %v", err)
	}
	if len(remaining.Contents) != 1 {
		t.Errorf("the bucket holds %d objects, want only the one outside the prefix",
			len(remaining.Contents))
	}

	if _, err := manager.Delete(ctx, bucket); err != nil {
		t.Fatalf("Delete returned an error: %v", err)
	}

	exists, err := manager.Exists(ctx, bucket)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if exists {
		t.Error("the bucket should be gone after Delete")
	}

	// Deleting a bucket that is already gone has to succeed, so a retried teardown
	// converges instead of failing forever.
	if _, err := manager.Delete(ctx, bucket); err != nil {
		t.Fatalf("the second Delete returned an error: %v", err)
	}
}

func TestDeleteKeys(t *testing.T) {
	ctx := context.Background()
	client := testClient(t)
	manager := NewBucketManager(client)
	bucket := testBucket(t, manager)

	if _, err := manager.Create(ctx, bucket, "us-east-1"); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	keys := []string{"a.txt", "b.txt", "c.txt"}
	for _, key := range keys {
		if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   strings.NewReader(key),
		}); err != nil {
			t.Fatalf("PutObject %q returned an error: %v", key, err)
		}
	}

	deleted, err := manager.DeleteKeys(ctx, bucket, []string{"a.txt", "c.txt"})
	if err != nil {
		t.Fatalf("DeleteKeys returned an error: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteKeys reported %d, want 2", deleted)
	}

	remaining, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("ListObjectsV2 returned an error: %v", err)
	}
	if len(remaining.Contents) != 1 || aws.ToString(remaining.Contents[0].Key) != "b.txt" {
		t.Errorf("the wrong objects survived: %+v", remaining.Contents)
	}
}

func TestCreateBucketInANonDefaultRegion(t *testing.T) {
	// Every region other than us-east-1 needs a location constraint, and several
	// S3-compatible providers reject one; Create retries without it, and this is
	// what proves the retry path works against a real server.
	ctx := context.Background()
	manager := NewBucketManager(testClient(t))
	bucket := testBucket(t, manager)

	created, err := manager.Create(ctx, bucket, envOrDefault("S3_TEST_REGION", "us-east-1"))
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	if !created {
		t.Error("Create should report that it created the bucket")
	}
}
