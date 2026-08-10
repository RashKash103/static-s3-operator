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
	"errors"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// S3-compatible providers are not uniform about which error code they return,
// and several return an HTTP status with a body the SDK cannot model. Every
// classifier here therefore checks both the API error code and the status code.

// errorCode returns the provider's error code, or "" when the error is not an
// API error.
func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// statusCode returns the HTTP status of a failed request, or 0.
func statusCode(err error) int {
	var responseErr *awshttp.ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.HTTPStatusCode()
	}
	return 0
}

func hasCode(err error, codes ...string) bool {
	actual := errorCode(err)
	if actual == "" {
		return false
	}
	for _, code := range codes {
		if actual == code {
			return true
		}
	}
	return false
}

// IsNotFound reports that the bucket or object does not exist.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if hasCode(err, "NoSuchBucket", "NoSuchKey", "NotFound", "404") {
		return true
	}
	return statusCode(err) == http.StatusNotFound
}

// IsAlreadyOwnedByYou reports that a CreateBucket call lost a race with an
// earlier one made with the same credentials, which is a success for the
// operator's purposes.
func IsAlreadyOwnedByYou(err error) bool {
	return err != nil && hasCode(err, "BucketAlreadyOwnedByYou")
}

// IsBucketAlreadyExists reports that the bucket name is taken by someone else.
// This is terminal: the user has to pick a different name.
func IsBucketAlreadyExists(err error) bool {
	return err != nil && hasCode(err, "BucketAlreadyExists")
}

// IsAccessDenied reports that the credentials are valid but not permitted to do
// this, which usually means the S3 access policy is missing an action.
func IsAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	if hasCode(err, "AccessDenied", "AllAccessDisabled", "Forbidden") {
		return true
	}
	return statusCode(err) == http.StatusForbidden
}

// IsUnsupported reports that the provider does not implement this part of the
// S3 API. Bucket tagging, website configuration and public access blocks are
// all optional in practice, so callers treat these failures as "cannot do it
// here" rather than as an error.
func IsUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if hasCode(err, "NotImplemented", "MethodNotAllowed", "InvalidRequest", "UnsupportedOperation",
		"XNotImplemented") {
		return true
	}
	switch statusCode(err) {
	case http.StatusNotImplemented, http.StatusMethodNotAllowed:
		return true
	}
	return false
}

// IsMissingConfiguration reports that the bucket simply has no website, policy,
// tag set or public access block yet, which is the normal state of a fresh
// bucket rather than a failure.
func IsMissingConfiguration(err error) bool {
	if err == nil {
		return false
	}
	return hasCode(err,
		"NoSuchWebsiteConfiguration",
		"NoSuchBucketPolicy",
		"NoSuchTagSet",
		"NoSuchTagSetError",
		"NoSuchConfiguration",
		"NoSuchPublicAccessBlockConfiguration",
		"ServerSideEncryptionConfigurationNotFoundError",
	)
}

// IsInvalidCredentials reports that the provider rejected the access key
// outright, so retrying will not help until the Secret is fixed.
func IsInvalidCredentials(err error) bool {
	return err != nil && hasCode(err,
		"InvalidAccessKeyId",
		"SignatureDoesNotMatch",
		"InvalidSecurity",
		"ExpiredToken",
		"TokenRefreshRequired",
	)
}
