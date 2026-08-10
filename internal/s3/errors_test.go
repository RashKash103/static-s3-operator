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
	"fmt"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// apiError builds the error shape the SDK produces when a provider returns a
// recognised S3 error code.
func apiError(code string) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "HeadBucket",
		Err:           &smithy.GenericAPIError{Code: code, Message: code},
	}
}

// statusError builds the error shape the SDK produces when a provider returns a
// status with a body it cannot model, which S3-compatible providers do often
// enough to matter.
func statusError(status int) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "HeadBucket",
		Err: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      fmt.Errorf("status %d", status),
			},
		},
	}
}

func TestIsNotFound(t *testing.T) {
	for _, err := range []error{
		apiError("NoSuchBucket"),
		apiError("NoSuchKey"),
		apiError("NotFound"),
		statusError(http.StatusNotFound),
	} {
		if !IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = false, want true", err)
		}
	}

	for _, err := range []error{
		nil,
		errors.New("plain error"),
		apiError("AccessDenied"),
		statusError(http.StatusInternalServerError),
	} {
		if IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = true, want false", err)
		}
	}
}

func TestIsAccessDenied(t *testing.T) {
	for _, err := range []error{
		apiError("AccessDenied"),
		apiError("AllAccessDisabled"),
		statusError(http.StatusForbidden),
	} {
		if !IsAccessDenied(err) {
			t.Errorf("IsAccessDenied(%v) = false, want true", err)
		}
	}
	if IsAccessDenied(apiError("NoSuchBucket")) {
		t.Error("a missing bucket is not an access problem")
	}
}

func TestIsUnsupported(t *testing.T) {
	// A provider without bucket tagging or website configuration is the normal
	// case, not a failure, so these have to be recognised or every S3-compatible
	// store would look broken.
	for _, err := range []error{
		apiError("NotImplemented"),
		apiError("MethodNotAllowed"),
		apiError("XNotImplemented"),
		statusError(http.StatusNotImplemented),
		statusError(http.StatusMethodNotAllowed),
	} {
		if !IsUnsupported(err) {
			t.Errorf("IsUnsupported(%v) = false, want true", err)
		}
	}
	if IsUnsupported(apiError("NoSuchBucket")) {
		t.Error("a missing bucket does not mean the operation is unsupported")
	}
	if IsUnsupported(nil) {
		t.Error("IsUnsupported(nil) should be false")
	}
}

func TestIsMissingConfiguration(t *testing.T) {
	// A fresh bucket has no website, policy or tag set, and reading one has to be
	// distinguishable from a real error.
	for _, code := range []string{
		"NoSuchWebsiteConfiguration",
		"NoSuchBucketPolicy",
		"NoSuchTagSet",
		"NoSuchTagSetError",
		"NoSuchPublicAccessBlockConfiguration",
	} {
		if !IsMissingConfiguration(apiError(code)) {
			t.Errorf("IsMissingConfiguration(%s) = false, want true", code)
		}
	}
	if IsMissingConfiguration(apiError("AccessDenied")) {
		t.Error("AccessDenied is not a missing configuration")
	}
}

func TestIsAlreadyOwnedByYouAndAlreadyExists(t *testing.T) {
	if !IsAlreadyOwnedByYou(apiError("BucketAlreadyOwnedByYou")) {
		t.Error("BucketAlreadyOwnedByYou should be recognised")
	}
	if IsAlreadyOwnedByYou(apiError("BucketAlreadyExists")) {
		t.Error("BucketAlreadyExists means somebody else has the name")
	}

	if !IsBucketAlreadyExists(apiError("BucketAlreadyExists")) {
		t.Error("BucketAlreadyExists should be recognised")
	}
	if IsBucketAlreadyExists(apiError("BucketAlreadyOwnedByYou")) {
		t.Error("BucketAlreadyOwnedByYou is a success for the operator")
	}
}

func TestIsACLNotSupported(t *testing.T) {
	// AWS disables ACLs on buckets created since April 2023, so this is the
	// ordinary case rather than an exotic one.
	for _, code := range []string{
		"AccessControlListNotSupported",
		"InvalidBucketAclWithObjectOwnership",
		"NotImplemented",
	} {
		if !IsACLNotSupported(apiError(code)) {
			t.Errorf("IsACLNotSupported(%s) = false, want true", code)
		}
	}
	if IsACLNotSupported(apiError("NoSuchBucket")) {
		t.Error("a missing bucket is not an ACL problem")
	}
	if IsACLNotSupported(nil) {
		t.Error("IsACLNotSupported(nil) should be false")
	}
}

func TestIsInvalidCredentials(t *testing.T) {
	for _, code := range []string{
		"InvalidAccessKeyId",
		"SignatureDoesNotMatch",
		"ExpiredToken",
	} {
		if !IsInvalidCredentials(apiError(code)) {
			t.Errorf("IsInvalidCredentials(%s) = false, want true", code)
		}
	}
	if IsInvalidCredentials(apiError("AccessDenied")) {
		t.Error("AccessDenied means the key is valid but not permitted")
	}
}

func TestIsInvalidLocationConstraint(t *testing.T) {
	// Some S3-compatible providers only accept CreateBucket without a location
	// constraint, and the retry depends on recognising their refusal.
	for _, code := range []string{
		"InvalidLocationConstraint",
		"IllegalLocationConstraintException",
		"InvalidArgument",
	} {
		if !isInvalidLocationConstraint(apiError(code)) {
			t.Errorf("isInvalidLocationConstraint(%s) = false, want true", code)
		}
	}
	if isInvalidLocationConstraint(apiError("AccessDenied")) {
		t.Error("AccessDenied is not a location constraint problem")
	}
}
