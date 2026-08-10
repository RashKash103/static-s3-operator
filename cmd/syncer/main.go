/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Command syncer copies a directory out of a container image and mirrors it into
// an S3 bucket.
//
// The operator runs it as a Job, one per sync, rather than doing the work in the
// controller. That keeps a large image extraction off the controller's heap, lets
// each site carry its own resource limits and deadline, and means the component
// handling an untrusted image has no Kubernetes API access at all.
//
// Every setting arrives as a flag so that "kubectl describe job" shows exactly
// what a sync was asked to do. Only the S3 credentials come from the environment,
// where a Secret reference can put them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/rashkash103/static-s3-operator/internal/registry"
	operators3 "github.com/rashkash103/static-s3-operator/internal/s3"
	"github.com/rashkash103/static-s3-operator/internal/syncer"
)

// stringList collects a flag that may be repeated.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// ruleList collects repeated "pattern=value" flags in the order given, which is
// the order the first-match-wins rules are evaluated in.
type ruleList []syncer.Rule

func (l *ruleList) String() string {
	parts := make([]string, 0, len(*l))
	for _, rule := range *l {
		parts = append(parts, rule.Pattern+"="+rule.Value)
	}
	return strings.Join(parts, ",")
}

func (l *ruleList) Set(value string) error {
	// Split on the first "=" so that header values containing "=" survive, which
	// Cache-Control values routinely do.
	pattern, headerValue, found := strings.Cut(value, "=")
	if !found || pattern == "" || headerValue == "" {
		return fmt.Errorf("expected pattern=value, got %q", value)
	}
	*l = append(*l, syncer.Rule{Pattern: pattern, Value: headerValue})
	return nil
}

type options struct {
	image      string
	sourcePath string
	platform   string

	bucket         string
	region         string
	endpoint       string
	forcePathStyle bool
	keyPrefix      string

	deleteRemoved       bool
	acl                 string
	defaultContentType  string
	defaultCacheControl string
	excludes            stringList
	cacheControl        ruleList
	contentTypes        ruleList
	concurrency         int

	workspace       string
	registryAuthDir string
	maxExtractBytes int64
	maxExtractFiles int

	plainHTTP                    bool
	registrySkipTLSVerify        bool
	s3SkipTLSVerify              bool
	requestChecksumWhenSupported bool

	terminationLog string
}

func main() {
	opts := &options{}
	bindFlags(opts, flag.CommandLine)

	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&zapOptions))
	ctrl.SetLogger(log)

	// Kubernetes sends SIGTERM when a Job exceeds its deadline or is deleted;
	// cancelling the context lets an in-flight upload stop promptly and still
	// write a report.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	report, err := run(ctx, opts, log)
	if err != nil {
		report.Succeeded = false
		report.Message = err.Error()
		log.Error(err, "Sync failed")
	}

	if writeErr := syncer.WriteReport(opts.terminationLog, report); writeErr != nil {
		// The report is a convenience for status reporting; losing it must not turn
		// a successful sync into a failed one.
		log.Error(writeErr, "Failed to write the sync report")
	}

	if err != nil {
		os.Exit(1)
	}
}

func bindFlags(opts *options, fs *flag.FlagSet) {
	fs.StringVar(&opts.image, "image", "",
		"Digest-pinned image reference to sync, for example registry/repo@sha256:...")
	fs.StringVar(&opts.sourcePath, "source-path", "/usr/share/nginx/html",
		"Absolute directory inside the image whose contents become the site root.")
	fs.StringVar(&opts.platform, "platform", "linux/"+runtime.GOARCH,
		"Platform to select when the reference points at a multi-architecture index.")

	fs.StringVar(&opts.bucket, "bucket", "", "Destination bucket.")
	fs.StringVar(&opts.region, "region", "us-east-1", "S3 region.")
	fs.StringVar(&opts.endpoint, "endpoint", "", "S3 API endpoint. Empty means AWS S3.")
	fs.BoolVar(&opts.forcePathStyle, "force-path-style", false,
		"Address the bucket as endpoint/bucket instead of bucket.endpoint.")
	fs.StringVar(&opts.keyPrefix, "key-prefix", "",
		"Upload the site under this prefix inside the bucket instead of at its root.")

	fs.BoolVar(&opts.deleteRemoved, "delete-removed", true,
		"Remove objects that are no longer part of the site.")
	fs.StringVar(&opts.acl, "acl", "public-read",
		"Canned ACL for uploaded objects. Empty sends none.")
	fs.StringVar(&opts.defaultContentType, "default-content-type", "application/octet-stream",
		"Content-Type for files whose extension is unknown.")
	fs.StringVar(&opts.defaultCacheControl, "default-cache-control", "",
		"Cache-Control for files matched by no --cache-control rule.")
	fs.Var(&opts.excludes, "exclude",
		"Site-relative glob that is neither uploaded nor deleted. Repeatable.")
	fs.Var(&opts.cacheControl, "cache-control",
		"Cache-Control rule as glob=value, first match winning. Repeatable.")
	fs.Var(&opts.contentTypes, "content-type",
		"Content-Type override as glob=value, first match winning. Repeatable.")
	fs.IntVar(&opts.concurrency, "concurrency", 8, "How many objects to transfer at once.")

	fs.StringVar(&opts.workspace, "workspace", "/workspace",
		"Writable directory used to stage the extracted site.")
	fs.StringVar(&opts.registryAuthDir, "registry-auth-dir", "/etc/static-s3-operator/registry-auth",
		"Directory of mounted docker config files used to authenticate the image pull.")
	fs.Int64Var(&opts.maxExtractBytes, "max-extract-bytes", 2<<30,
		"Largest total size to extract from the image.")
	fs.IntVar(&opts.maxExtractFiles, "max-extract-files", 200_000,
		"Largest number of files to extract from the image.")

	fs.BoolVar(&opts.plainHTTP, "plain-http", false, "Talk to the registry over HTTP.")
	fs.BoolVar(&opts.registrySkipTLSVerify, "registry-skip-tls-verify", false,
		"Do not verify the registry's certificate.")
	fs.BoolVar(&opts.s3SkipTLSVerify, "s3-skip-tls-verify", false,
		"Do not verify the S3 endpoint's certificate.")
	fs.BoolVar(&opts.requestChecksumWhenSupported, "s3-request-checksum-when-supported", false,
		"Send trailing CRC32 checksums on every upload.")

	fs.StringVar(&opts.terminationLog, "termination-log", "/dev/termination-log",
		"File the sync report is written to for the operator to read.")
}

func (o *options) validate() error {
	var problems []string
	if o.image == "" {
		problems = append(problems, "--image is required")
	}
	if o.bucket == "" {
		problems = append(problems, "--bucket is required")
	}
	if !strings.HasPrefix(o.sourcePath, "/") {
		problems = append(problems, "--source-path must be absolute")
	}
	if o.workspace == "" {
		problems = append(problems, "--workspace is required")
	}
	if o.concurrency < 1 {
		problems = append(problems, "--concurrency must be at least 1")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func run(ctx context.Context, opts *options, log logr.Logger) (syncer.Report, error) {
	report := syncer.Report{}

	if err := opts.validate(); err != nil {
		return report, err
	}

	platform, err := v1.ParsePlatform(opts.platform)
	if err != nil {
		return report, fmt.Errorf("parse --platform: %w", err)
	}

	keychain, err := keychainFromDir(opts.registryAuthDir)
	if err != nil {
		return report, err
	}

	log.Info("Pulling image", "image", opts.image, "platform", opts.platform, "path", opts.sourcePath)
	image, err := registry.Image(ctx, opts.image, platform, registry.Options{
		Keychain:              keychain,
		PlainHTTP:             opts.plainHTTP,
		InsecureSkipTLSVerify: opts.registrySkipTLSVerify,
	})
	if err != nil {
		return report, err
	}

	if digest, err := image.Digest(); err == nil {
		report.Digest = digest.String()
	}

	siteDir := filepath.Join(opts.workspace, "site")
	if err := os.MkdirAll(siteDir, 0o755); err != nil { //nolint:gosec // staged content, served publicly anyway
		return report, fmt.Errorf("create staging directory: %w", err)
	}

	extracted, err := syncer.ExtractImagePath(ctx, image, syncer.ExtractOptions{
		SourcePath: opts.sourcePath,
		Dest:       siteDir,
		MaxBytes:   opts.maxExtractBytes,
		MaxFiles:   opts.maxExtractFiles,
		Log:        log,
	})
	if extracted != nil {
		report.ExtractedFiles = extracted.Files
		report.ExtractedBytes = extracted.Bytes
	}
	if err != nil {
		return report, err
	}
	log.Info("Extracted site from image",
		"files", extracted.Files, "bytes", extracted.Bytes,
		"symlinks", extracted.Symlinks, "skipped", extracted.Skipped)

	client, err := operators3.NewClient(operators3.ClientOptions{
		Region:                       opts.region,
		Endpoint:                     opts.endpoint,
		ForcePathStyle:               opts.forcePathStyle,
		Credentials:                  credentialsFromEnvironment(),
		RequestChecksumWhenSupported: opts.requestChecksumWhenSupported,
		SkipTLSVerify:                opts.s3SkipTLSVerify,
	})
	if err != nil {
		return report, err
	}

	result, err := syncer.Sync(ctx, syncer.Options{
		Client:              client,
		Bucket:              opts.bucket,
		SourceDir:           siteDir,
		KeyPrefix:           opts.keyPrefix,
		DeleteRemoved:       opts.deleteRemoved,
		ACL:                 opts.acl,
		Excludes:            opts.excludes,
		CacheControl:        opts.cacheControl,
		DefaultCacheControl: opts.defaultCacheControl,
		ContentTypes:        opts.contentTypes,
		DefaultContentType:  opts.defaultContentType,
		Concurrency:         opts.concurrency,
		Log:                 log,
	})
	if err != nil {
		return report, err
	}

	report.Succeeded = true
	report.Uploaded = result.Uploaded
	report.Deleted = result.Deleted
	report.Unchanged = result.Unchanged
	report.Skipped = result.Skipped
	report.BytesUploaded = result.BytesUploaded
	report.Duration = result.Duration.Round(time.Millisecond).String()
	report.ACLDropped = result.ACLDropped
	return report, nil
}

// credentialsFromEnvironment reads the S3 access key. The standard AWS variable
// names are used so that a Job can wire them straight from a Secret without the
// operator ever handling the values.
func credentialsFromEnvironment() operators3.Credentials {
	return operators3.Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
}

// keychainFromDir reads every mounted docker config in dir. A missing directory
// means the site named no pull secrets, so the pull is anonymous.
func keychainFromDir(dir string) (authn.Keychain, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry auth directory %q: %w", dir, err)
	}

	var documents [][]byte
	for _, entry := range entries {
		// A projected Secret volume keeps its payload behind "..data" symlinks;
		// reading those as well would parse every credential twice.
		if strings.HasPrefix(entry.Name(), "..") || entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read registry credentials %q: %w", entry.Name(), err)
		}
		documents = append(documents, raw)
	}
	if len(documents) == 0 {
		return nil, nil
	}

	return registry.KeychainFromDockerConfigs(documents...)
}
