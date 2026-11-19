/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package s3

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// deleteBatchSize is the maximum number of keys in one DeleteObjects request,
// which the S3 API caps at 1000.
const deleteBatchSize = 1000

// BucketManager performs the bucket-level operations the operator needs.
type BucketManager struct {
	// Client is the S3 client every call is made with.
	Client *s3.Client
}

// NewBucketManager wraps an S3 client.
func NewBucketManager(client *s3.Client) *BucketManager {
	return &BucketManager{Client: client}
}

// Exists reports whether the bucket is present and reachable with the current
// credentials. A bucket that exists but is not readable by these credentials is
// reported as existing, because creating it would fail too.
func (m *BucketManager) Exists(ctx context.Context, bucket string) (bool, error) {
	_, err := m.Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err):
		return false, nil
	case IsAccessDenied(err):
		return true, nil
	default:
		return false, fmt.Errorf("head bucket %q: %w", bucket, err)
	}
}

// Create creates the bucket. It reports created=false when the provider says the
// bucket already existed under the same credentials (BucketAlreadyOwnedByYou).
//
// A true result means the provider accepted the request, not that the bucket is
// new. AWS in us-east-1, and several compatible providers, answer a repeat
// CreateBucket from the bucket's owner with plain success. Ownership must
// therefore never be inferred from this alone: callers check Exists first, which
// is what keeps a bucket somebody already had from being recorded as created,
// and so from being deleted with a StaticSite.
func (m *BucketManager) Create(ctx context.Context, bucket, region string) (bool, error) {
	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	// us-east-1 is the API's implicit location and rejects an explicit
	// constraint, while every other region requires one.
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}

	_, err := m.Client.CreateBucket(ctx, input)
	if err != nil && input.CreateBucketConfiguration != nil && isInvalidLocationConstraint(err) {
		// Several S3-compatible providers accept only the bare form, even for a
		// region they otherwise report correctly.
		input.CreateBucketConfiguration = nil
		_, err = m.Client.CreateBucket(ctx, input)
	}

	switch {
	case err == nil:
		return true, nil
	case IsAlreadyOwnedByYou(err):
		return false, nil
	case IsBucketAlreadyExists(err):
		return false, fmt.Errorf("bucket %q already exists and belongs to someone else: %w", bucket, err)
	default:
		return false, fmt.Errorf("create bucket %q: %w", bucket, err)
	}
}

func isInvalidLocationConstraint(err error) bool {
	return hasCode(err, "InvalidLocationConstraint", "IllegalLocationConstraintException", "InvalidArgument")
}

// WebsiteConfig is a bucket's static website hosting configuration.
type WebsiteConfig struct {
	// IndexDocument is served for requests ending in a slash.
	IndexDocument string
	// ErrorDocument is served for 4xx responses. Empty means none.
	ErrorDocument string
}

// ErrWebsiteUnsupported reports that the provider does not implement website
// configuration.
//
// Several widely used S3-compatible providers do not, and a website bucket is
// still perfectly usable without it: the objects are served, only the
// index-document rewrite for a directory request is missing. Callers treat this as
// a limitation to report rather than a failure to retry.
var ErrWebsiteUnsupported = errors.New("this S3 provider does not support website configuration")

// EnsureWebsite makes the bucket's website configuration match want, and reports
// whether anything had to change. It returns ErrWebsiteUnsupported when the
// provider does not implement the call at all.
func (m *BucketManager) EnsureWebsite(ctx context.Context, bucket string, want WebsiteConfig) (bool, error) {
	if want.IndexDocument == "" {
		return false, errors.New("website index document is empty")
	}

	current, err := m.getWebsite(ctx, bucket)
	switch {
	case err == nil:
		if current != nil && *current == want {
			return false, nil
		}
	case IsMissingConfiguration(err), IsNotFound(err):
		// No configuration yet, which is what a fresh bucket looks like.
	case IsUnsupported(err):
		// A provider that cannot report the configuration may still accept one, so
		// write it unconditionally rather than giving up here.
	default:
		return false, fmt.Errorf("read website configuration for %q: %w", bucket, err)
	}

	input := &s3.PutBucketWebsiteInput{
		Bucket: aws.String(bucket),
		WebsiteConfiguration: &types.WebsiteConfiguration{
			IndexDocument: &types.IndexDocument{Suffix: aws.String(want.IndexDocument)},
		},
	}
	if want.ErrorDocument != "" {
		input.WebsiteConfiguration.ErrorDocument = &types.ErrorDocument{Key: aws.String(want.ErrorDocument)}
	}

	if _, err := m.Client.PutBucketWebsite(ctx, input); err != nil {
		if IsUnsupportedRequest(err) {
			return false, fmt.Errorf("%w: %v", ErrWebsiteUnsupported, err)
		}
		return false, fmt.Errorf("configure website hosting on %q: %w", bucket, err)
	}
	return true, nil
}

// RemoveWebsite drops the bucket's website configuration. A bucket that has none
// is already in the desired state.
func (m *BucketManager) RemoveWebsite(ctx context.Context, bucket string) error {
	_, err := m.Client.DeleteBucketWebsite(ctx, &s3.DeleteBucketWebsiteInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil, IsMissingConfiguration(err), IsNotFound(err), IsUnsupported(err):
		return nil
	default:
		return fmt.Errorf("remove website configuration from %q: %w", bucket, err)
	}
}

func (m *BucketManager) getWebsite(ctx context.Context, bucket string) (*WebsiteConfig, error) {
	out, err := m.Client.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{Bucket: aws.String(bucket)})
	if err != nil {
		return nil, err
	}

	current := &WebsiteConfig{}
	if out.IndexDocument != nil {
		current.IndexDocument = aws.ToString(out.IndexDocument.Suffix)
	}
	if out.ErrorDocument != nil {
		current.ErrorDocument = aws.ToString(out.ErrorDocument.Key)
	}
	return current, nil
}

// EnsurePolicy makes the bucket policy match policy, comparing the documents
// semantically so that reformatting does not count as a change. An empty policy
// removes any policy the operator would otherwise manage.
func (m *BucketManager) EnsurePolicy(ctx context.Context, bucket, policy string) (bool, error) {
	if strings.TrimSpace(policy) == "" {
		return false, m.RemovePolicy(ctx, bucket)
	}

	if _, err := normalizePolicy(policy); err != nil {
		return false, err
	}

	out, err := m.Client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil:
		if equal, compareErr := policiesEqual(aws.ToString(out.Policy), policy); compareErr == nil && equal {
			return false, nil
		}
	case IsMissingConfiguration(err), IsNotFound(err), IsUnsupported(err), IsAccessDenied(err):
		// No policy yet, or a provider that will not show it. Write ours.
	default:
		return false, fmt.Errorf("read bucket policy for %q: %w", bucket, err)
	}

	_, err = m.Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(policy),
	})
	if err != nil {
		return false, fmt.Errorf("apply bucket policy to %q: %w", bucket, err)
	}
	return true, nil
}

// RemovePolicy deletes the bucket policy if there is one.
func (m *BucketManager) RemovePolicy(ctx context.Context, bucket string) error {
	_, err := m.Client.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil, IsMissingConfiguration(err), IsNotFound(err), IsUnsupported(err):
		return nil
	default:
		return fmt.Errorf("remove bucket policy from %q: %w", bucket, err)
	}
}

// AllowPublicPolicy clears the bucket's public access block so that a
// public-read policy can take effect.
//
// AWS blocks public access on new buckets by default, which silently defeats the
// bucket policy, so this has to run before the policy is written. Public access
// blocks are an AWS-specific feature that most S3-compatible providers do not
// implement, and a provider that has nothing to clear is not a failure.
//
// It reports whether the block was actually cleared, so a caller can say so, but
// never fails for a provider that does not implement the call. The bucket policy
// is the operation that really has to succeed, and it fails loudly if a block is
// still in the way.
func (m *BucketManager) AllowPublicPolicy(ctx context.Context, bucket string) (bool, error) {
	_, err := m.Client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(false),
			BlockPublicPolicy:     aws.Bool(false),
			IgnorePublicAcls:      aws.Bool(false),
			RestrictPublicBuckets: aws.Bool(false),
		},
	})
	switch {
	case err == nil:
		return true, nil
	case IsMissingConfiguration(err), IsUnsupportedRequest(err), IsAccessDenied(err):
		return false, nil
	default:
		return false, fmt.Errorf("clear the public access block on %q: %w", bucket, err)
	}
}

// EnsureTags makes the bucket's tag set match the site's tags plus the
// operator's ownership tags. The ownership tags are always included, so
// reconciling tags can never drop the token that permits deletion.
func (m *BucketManager) EnsureTags(
	ctx context.Context,
	bucket string,
	siteTags map[string]string,
	own Ownership,
) (bool, error) {
	want := ownershipTags(own, siteTags)

	current, err := m.getTags(ctx, bucket)
	switch {
	case err == nil:
		if reflect.DeepEqual(current, want) {
			return false, nil
		}
	case IsMissingConfiguration(err), IsNotFound(err):
	case IsUnsupported(err), IsAccessDenied(err):
		// Tagging is optional; ownership falls back to the marker object.
		return false, nil
	default:
		return false, fmt.Errorf("read bucket tags for %q: %w", bucket, err)
	}

	if err := m.putTags(ctx, bucket, want); err != nil {
		if IsUnsupported(err) || IsAccessDenied(err) {
			return false, nil
		}
		return false, fmt.Errorf("tag bucket %q: %w", bucket, err)
	}
	return true, nil
}

func (m *BucketManager) getTags(ctx context.Context, bucket string) (map[string]string, error) {
	out, err := m.Client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	if err != nil {
		return nil, err
	}
	tags := make(map[string]string, len(out.TagSet))
	for _, tag := range out.TagSet {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return tags, nil
}

func (m *BucketManager) putTags(ctx context.Context, bucket string, tags map[string]string) error {
	tagSet := make([]types.Tag, 0, len(tags))
	for key, value := range tags {
		tagSet = append(tagSet, types.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	// A stable order keeps request bodies comparable in test fixtures and logs.
	slices.SortFunc(tagSet, func(a, b types.Tag) int {
		return strings.Compare(aws.ToString(a.Key), aws.ToString(b.Key))
	})

	_, err := m.Client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket:  aws.String(bucket),
		Tagging: &types.Tagging{TagSet: tagSet},
	})
	return err
}

// DeleteKeys removes specific object keys, batching them into as few requests as
// the API allows. It is what a mirroring sync uses to drop objects that are no
// longer part of the site.
func (m *BucketManager) DeleteKeys(ctx context.Context, bucket string, keys []string) (int, error) {
	objects := make([]types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
	}
	return m.deleteObjects(ctx, bucket, objects)
}

// Empty removes every object under prefix, including non-current versions and
// delete markers on a versioned bucket. It returns the number of objects
// removed. An empty prefix empties the whole bucket.
func (m *BucketManager) Empty(ctx context.Context, bucket, prefix string) (int, error) {
	deleted, err := m.emptyVersions(ctx, bucket, prefix)
	if err == nil {
		return deleted, nil
	}
	if !IsUnsupported(err) && !IsAccessDenied(err) && !IsMissingConfiguration(err) {
		return deleted, err
	}
	// The provider does not implement version listing, so the current objects are
	// all there is to remove.
	return m.emptyCurrent(ctx, bucket, prefix)
}

func (m *BucketManager) emptyVersions(ctx context.Context, bucket, prefix string) (int, error) {
	input := &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	deleted := 0
	for {
		page, err := m.Client.ListObjectVersions(ctx, input)
		if err != nil {
			return deleted, err
		}

		batch := make([]types.ObjectIdentifier, 0, len(page.Versions)+len(page.DeleteMarkers))
		for _, version := range page.Versions {
			batch = append(batch, types.ObjectIdentifier{Key: version.Key, VersionId: version.VersionId})
		}
		for _, marker := range page.DeleteMarkers {
			batch = append(batch, types.ObjectIdentifier{Key: marker.Key, VersionId: marker.VersionId})
		}

		count, err := m.deleteObjects(ctx, bucket, batch)
		deleted += count
		if err != nil {
			return deleted, err
		}

		if !aws.ToBool(page.IsTruncated) {
			return deleted, nil
		}
		input.KeyMarker = page.NextKeyMarker
		input.VersionIdMarker = page.NextVersionIdMarker
	}
}

func (m *BucketManager) emptyCurrent(ctx context.Context, bucket, prefix string) (int, error) {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(bucket)}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	deleted := 0
	paginator := s3.NewListObjectsV2Paginator(m.Client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, fmt.Errorf("list objects in %q: %w", bucket, err)
		}

		batch := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, object := range page.Contents {
			batch = append(batch, types.ObjectIdentifier{Key: object.Key})
		}

		count, err := m.deleteObjects(ctx, bucket, batch)
		deleted += count
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// deleteObjects removes keys in batches, falling back to one request per object
// on providers that do not implement the multi-object delete API.
func (m *BucketManager) deleteObjects(
	ctx context.Context,
	bucket string,
	objects []types.ObjectIdentifier,
) (int, error) {
	deleted := 0

	for start := 0; start < len(objects); start += deleteBatchSize {
		end := min(start+deleteBatchSize, len(objects))
		batch := objects[start:end]

		out, err := m.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			if IsUnsupported(err) {
				count, individualErr := m.deleteObjectsIndividually(ctx, bucket, batch)
				deleted += count
				if individualErr != nil {
					return deleted, individualErr
				}
				continue
			}
			return deleted, fmt.Errorf("delete objects from %q: %w", bucket, err)
		}

		deleted += len(batch) - len(out.Errors)
		if len(out.Errors) > 0 {
			first := out.Errors[0]
			return deleted, fmt.Errorf("delete %q from %q: %s: %s",
				aws.ToString(first.Key), bucket, aws.ToString(first.Code), aws.ToString(first.Message))
		}
	}

	return deleted, nil
}

func (m *BucketManager) deleteObjectsIndividually(
	ctx context.Context,
	bucket string,
	objects []types.ObjectIdentifier,
) (int, error) {
	deleted := 0
	for _, object := range objects {
		_, err := m.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket:    aws.String(bucket),
			Key:       object.Key,
			VersionId: object.VersionId,
		})
		if err != nil && !IsNotFound(err) {
			return deleted, fmt.Errorf("delete %q from %q: %w", aws.ToString(object.Key), bucket, err)
		}
		deleted++
	}
	return deleted, nil
}

// Delete empties and then removes the bucket. A bucket that is already gone is
// treated as success, so a retried deletion converges.
func (m *BucketManager) Delete(ctx context.Context, bucket string) (int, error) {
	deleted, err := m.Empty(ctx, bucket, "")
	if err != nil {
		if IsNotFound(err) {
			return deleted, nil
		}
		return deleted, err
	}

	_, err = m.Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil, IsNotFound(err):
		return deleted, nil
	default:
		return deleted, fmt.Errorf("delete bucket %q: %w", bucket, err)
	}
}
