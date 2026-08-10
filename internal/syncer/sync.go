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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"

	"github.com/rashkash103/static-s3-operator/internal/s3"
)

// multipartPartSize is the part size the upload manager uses.
//
// It is deliberately larger than the SDK default so that ordinary site assets
// upload in one part and keep an ETag equal to their MD5, which lets the next
// sync compare them without an extra HeadObject per file.
const multipartPartSize = 16 << 20

// Options describe one sync of a staged directory into a bucket.
type Options struct {
	// Client is the S3 client to use.
	Client *awss3.Client
	// Bucket is the destination bucket.
	Bucket string
	// SourceDir is the local directory holding the site, already extracted.
	SourceDir string
	// KeyPrefix places the site under a prefix inside the bucket. Empty uploads to
	// the bucket root.
	KeyPrefix string
	// DeleteRemoved removes objects that are no longer part of the site.
	DeleteRemoved bool
	// ACL is the canned ACL for uploaded objects. Empty sends none.
	ACL string
	// Excludes are site-relative globs that are neither uploaded nor deleted.
	Excludes []string
	// CacheControl maps globs to Cache-Control values, first match winning.
	CacheControl []Rule
	// DefaultCacheControl applies to files matched by no CacheControl rule. Empty
	// sends no header.
	DefaultCacheControl string
	// ContentTypes maps globs to Content-Type values, overriding the type derived
	// from the file extension.
	ContentTypes []Rule
	// DefaultContentType applies to files whose type cannot be derived.
	DefaultContentType string
	// Concurrency is how many objects are transferred at once.
	Concurrency int
	// Log receives progress.
	Log logr.Logger
}

// Result is what a sync did.
type Result struct {
	Uploaded      int           `json:"uploaded"`
	Deleted       int           `json:"deleted"`
	Unchanged     int           `json:"unchanged"`
	Skipped       int           `json:"skipped"`
	BytesUploaded int64         `json:"bytesUploaded"`
	Duration      time.Duration `json:"duration"`
	// ACLDropped reports that the bucket refuses object ACLs, so the site relies
	// on the bucket policy for public access.
	ACLDropped bool `json:"aclDropped,omitempty"`
}

// Sync makes the bucket's contents match the staged directory.
//
// It is the Go equivalent of an "s3cmd sync --delete-removed" over the site, with
// content types derived from file extensions and the same object either skipped
// or replaced based on an exact content hash rather than a timestamp.
func Sync(ctx context.Context, opts Options) (*Result, error) {
	started := time.Now()

	if opts.Client == nil {
		return nil, errors.New("sync requires an S3 client")
	}
	if opts.Bucket == "" {
		return nil, errors.New("sync requires a bucket name")
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	keyPrefix := normalizeKeyPrefix(opts.KeyPrefix)

	excludes, err := CompileGlobSet(opts.Excludes)
	if err != nil {
		return nil, fmt.Errorf("spec.sync.excludes: %w", err)
	}
	cacheRules, err := CompileGlobValues(opts.CacheControl)
	if err != nil {
		return nil, fmt.Errorf("spec.sync.cacheControl: %w", err)
	}
	typeRules, err := CompileGlobValues(opts.ContentTypes)
	if err != nil {
		return nil, fmt.Errorf("spec.sync.contentTypes: %w", err)
	}

	local, skipped, err := scanLocal(opts.SourceDir, keyPrefix, excludes)
	if err != nil {
		return nil, err
	}
	opts.Log.Info("Scanned staged site", "files", len(local), "excluded", skipped)

	remote, err := listRemote(ctx, opts.Client, opts.Bucket, keyPrefix)
	if err != nil {
		return nil, err
	}

	plan, err := buildPlan(ctx, opts.Client, opts.Bucket, local, remote,
		skipped, opts.DeleteRemoved, excludes, keyPrefix)
	if err != nil {
		return nil, err
	}
	opts.Log.Info("Planned sync",
		"upload", len(plan.Uploads), "delete", len(plan.Deletes),
		"unchanged", plan.Unchanged, "excluded", plan.Skipped)

	uploader := &objectUploader{
		client:  opts.Client,
		bucket:  opts.Bucket,
		acl:     opts.ACL,
		manager: manager.NewUploader(opts.Client, func(u *manager.Uploader) { u.PartSize = multipartPartSize }),
		cache:   cacheRules,
		types:   typeRules,
		options: opts,
		log:     opts.Log,
	}

	if err := uploader.uploadAll(ctx, plan.Uploads, opts.Concurrency); err != nil {
		return nil, err
	}

	deleted := 0
	if len(plan.Deletes) > 0 {
		buckets := s3.NewBucketManager(opts.Client)
		deleted, err = buckets.DeleteKeys(ctx, opts.Bucket, plan.Deletes)
		if err != nil {
			return nil, err
		}
	}

	result := &Result{
		Uploaded:      len(plan.Uploads),
		Deleted:       deleted,
		Unchanged:     plan.Unchanged,
		Skipped:       plan.Skipped,
		BytesUploaded: plan.UploadBytes,
		Duration:      time.Since(started),
		ACLDropped:    uploader.aclDropped.Load(),
	}
	opts.Log.Info("Completed sync",
		"uploaded", result.Uploaded, "deleted", result.Deleted,
		"unchanged", result.Unchanged, "bytes", result.BytesUploaded,
		"duration", result.Duration.String())
	return result, nil
}

// normalizeKeyPrefix makes a prefix either empty or slash terminated, so joining
// it with a relative path never needs a special case.
func normalizeKeyPrefix(prefix string) string {
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return strings.TrimSuffix(prefix, "/") + "/"
}

type objectUploader struct {
	client  *awss3.Client
	bucket  string
	acl     string
	manager *manager.Uploader
	cache   GlobValues
	types   GlobValues
	options Options
	log     logr.Logger

	// aclDropped records that the bucket rejected object ACLs, so the rest of the
	// sync stops asking for them instead of failing every single object.
	aclDropped atomic.Bool
	aclOnce    sync.Once
}

func (u *objectUploader) uploadAll(ctx context.Context, files []LocalFile, concurrency int) error {
	if len(files) == 0 {
		return nil
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(concurrency)

	for _, file := range files {
		group.Go(func() error {
			if err := u.upload(groupCtx, file); err != nil {
				return fmt.Errorf("upload %q: %w", file.Key, err)
			}
			return nil
		})
	}

	return group.Wait()
}

func (u *objectUploader) upload(ctx context.Context, file LocalFile) error {
	err := u.putObject(ctx, file, !u.aclDropped.Load())
	if err == nil {
		return nil
	}

	// A bucket with ACLs disabled rejects every upload that asks for one. Retry
	// without the ACL and stop sending it for the rest of the sync: the site is
	// public through the bucket policy, not through per-object grants.
	if u.acl != "" && !u.aclDropped.Load() && s3.IsACLNotSupported(err) {
		u.aclOnce.Do(func() {
			u.aclDropped.Store(true)
			u.log.Info("Bucket does not accept object ACLs, continuing without them; "+
				"public access comes from the bucket policy",
				"bucket", u.bucket, "acl", u.acl)
		})
		return u.putObject(ctx, file, false)
	}
	return err
}

func (u *objectUploader) putObject(ctx context.Context, file LocalFile, withACL bool) error {
	handle, err := os.Open(file.Path)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()

	input := &awss3.PutObjectInput{
		Bucket:        aws.String(u.bucket),
		Key:           aws.String(file.Key),
		Body:          handle,
		ContentLength: aws.Int64(file.Size),
		ContentType:   aws.String(u.contentType(file.RelPath)),
		Metadata:      map[string]string{contentMD5Metadata: file.MD5},
	}
	if cacheControl := u.cacheControl(file.RelPath); cacheControl != "" {
		input.CacheControl = aws.String(cacheControl)
	}
	if withACL && u.acl != "" {
		input.ACL = types.ObjectCannedACL(u.acl)
	}

	_, err = u.manager.Upload(ctx, input)
	return err
}

func (u *objectUploader) contentType(relPath string) string {
	if override, ok := u.types.Lookup(relPath); ok {
		return override
	}
	if derived := contentTypeForKey(relPath); derived != "" {
		return derived
	}
	if u.options.DefaultContentType != "" {
		return u.options.DefaultContentType
	}
	return "application/octet-stream"
}

func (u *objectUploader) cacheControl(relPath string) string {
	if value, ok := u.cache.Lookup(relPath); ok {
		return value
	}
	return u.options.DefaultCacheControl
}
