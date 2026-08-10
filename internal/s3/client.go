/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package s3 wraps the operations the operator performs against an S3 API:
// creating and configuring a website bucket, recording who owns it, and
// emptying and removing it again. It targets AWS S3 and S3-compatible providers
// equally, so every optional part of the API is treated as optional.
package s3

import (
	"crypto/tls"
	"errors"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Credentials are the S3 access key the operator acts with.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Valid reports whether both required halves of the key are present.
func (c Credentials) Valid() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// ClientOptions describe how to reach an S3 API.
type ClientOptions struct {
	// Region is the S3 region. Required, even for providers that ignore it,
	// because request signing needs a region.
	Region string
	// Endpoint overrides the AWS endpoint for an S3-compatible provider. Empty
	// means AWS S3.
	Endpoint string
	// ForcePathStyle addresses buckets as endpoint/bucket rather than
	// bucket.endpoint.
	ForcePathStyle bool
	// Credentials is the access key to sign with.
	Credentials Credentials
	// RequestChecksumWhenSupported opts in to the SDK's newer behaviour of
	// attaching a trailing CRC32 checksum to every upload.
	RequestChecksumWhenSupported bool
	// SkipTLSVerify disables endpoint certificate verification.
	SkipTLSVerify bool
	// MaxRetries bounds SDK-level retries. Zero uses the SDK default.
	MaxRetries int
}

// ErrMissingCredentials is returned when no access key was supplied.
var ErrMissingCredentials = errors.New("s3 credentials are missing an access key ID or secret access key")

// NewClient builds an S3 client for opts.
//
// Credentials are always explicit: the operator never falls back to ambient
// environment variables or instance metadata, because a site silently using the
// node's identity instead of the Secret it names would be very hard to notice.
func NewClient(opts ClientOptions) (*s3.Client, error) {
	if !opts.Credentials.Valid() {
		return nil, ErrMissingCredentials
	}
	if opts.Region == "" {
		return nil, errors.New("s3 region is empty")
	}

	cfg := aws.Config{
		Region: opts.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			opts.Credentials.AccessKeyID,
			opts.Credentials.SecretAccessKey,
			opts.Credentials.SessionToken,
		),
		HTTPClient: httpClient(opts.SkipTLSVerify),
	}
	if opts.MaxRetries > 0 {
		cfg.RetryMaxAttempts = opts.MaxRetries
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = opts.ForcePathStyle
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}

		// The SDK's default is to add a trailing CRC32 checksum to every upload,
		// which several S3-compatible providers reject outright because they do not
		// implement aws-chunked trailers. Asking for checksums only where the API
		// requires them keeps those providers working; --s3-request-checksum-when-
		// supported opts back in.
		if opts.RequestChecksumWhenSupported {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenSupported
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
		} else {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	}), nil
}

func httpClient(skipTLSVerify bool) *awshttp.BuildableClient {
	client := awshttp.NewBuildableClient().WithTimeout(5 * time.Minute)
	if !skipTLSVerify {
		return client
	}
	return client.WithTransportOptions(func(t *http.Transport) {
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{} //nolint:gosec // overridden below
		}
		t.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // opt-in, documented as dev only
	})
}
