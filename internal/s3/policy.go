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
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// BucketARN returns the ARN of a bucket. S3-compatible providers reuse the AWS
// ARN form in policy documents, so this is not AWS specific.
func BucketARN(bucket string) string {
	return "arn:aws:s3:::" + bucket
}

// PublicReadPolicy is the bucket policy that lets a browser fetch the site.
//
// It grants nothing but s3:GetObject to anonymous callers, so the bucket cannot
// be listed or written to. The document is emitted in the shape AWS itself
// stores, which keeps the operator from rewriting an unchanged policy on every
// reconcile.
func PublicReadPolicy(bucket string) string {
	document := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Sid":       "StaticS3OperatorPublicRead",
				"Effect":    "Allow",
				"Principal": map[string]any{"AWS": "*"},
				"Action":    "s3:GetObject",
				"Resource":  BucketARN(bucket) + "/*",
			},
		},
	}

	// The map above is built from plain types, so encoding cannot fail.
	encoded, _ := json.Marshal(document)
	return string(encoded)
}

// RenderPolicy substitutes ${bucket} in a user-supplied policy document, so a
// policy can be written once and reused across sites.
func RenderPolicy(policy, bucket string) string {
	return strings.ReplaceAll(policy, "${bucket}", bucket)
}

// policiesEqual reports whether two policy documents mean the same thing.
//
// Providers rewrite policies as they store them: AWS turns `"Principal": "*"`
// into `{"AWS": "*"}` and may collapse or expand single-element arrays. Without
// normalising both sides, the operator would decide the policy had drifted and
// write it again on every single reconcile.
func policiesEqual(left, right string) (bool, error) {
	normalizedLeft, err := normalizePolicy(left)
	if err != nil {
		return false, err
	}
	normalizedRight, err := normalizePolicy(right)
	if err != nil {
		return false, err
	}
	return normalizedLeft == normalizedRight, nil
}

// normalizePolicy parses a policy document and returns a canonical string form:
// single-element arrays become scalars, a bare "*" principal becomes
// {"AWS": "*"}, and object keys are sorted by the JSON encoder.
func normalizePolicy(policy string) (string, error) {
	var parsed any
	if err := json.Unmarshal([]byte(policy), &parsed); err != nil {
		return "", fmt.Errorf("policy document is not valid JSON: %w", err)
	}

	encoded, err := json.Marshal(canonicalizePolicyValue(parsed, ""))
	if err != nil {
		return "", fmt.Errorf("re-encode policy document: %w", err)
	}
	return string(encoded), nil
}

// canonicalizePolicyValue rewrites one node of a policy document. key is the
// object key the value was found under, which is what distinguishes a Principal
// from any other string.
func canonicalizePolicyValue(value any, key string) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			result[childKey] = canonicalizePolicyValue(childValue, childKey)
		}
		return result

	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, canonicalizePolicyValue(item, key))
		}
		// A one-element list and the bare element are the same policy.
		if len(items) == 1 {
			return items[0]
		}
		// Action and Resource lists are unordered sets.
		slices.SortFunc(items, func(a, b any) int {
			return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
		})
		return items

	case string:
		if (key == "Principal" || key == "NotPrincipal") && typed == "*" {
			return map[string]any{"AWS": "*"}
		}
		return typed

	default:
		return typed
	}
}
