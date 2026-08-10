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
	"strings"
	"testing"
)

func TestPublicReadPolicyGrantsOnlyObjectReads(t *testing.T) {
	policy := PublicReadPolicy("acme-docs")

	var document struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string `json:"Effect"`
			Principal any    `json:"Principal"`
			Action    any    `json:"Action"`
			Resource  any    `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &document); err != nil {
		t.Fatalf("the generated policy is not valid JSON: %v", err)
	}

	if len(document.Statement) != 1 {
		t.Fatalf("the policy has %d statements, want exactly one", len(document.Statement))
	}
	statement := document.Statement[0]
	if statement.Effect != "Allow" {
		t.Errorf("Effect = %q", statement.Effect)
	}
	if statement.Action != "s3:GetObject" {
		t.Errorf("Action = %v, want only s3:GetObject so the bucket cannot be listed or written",
			statement.Action)
	}
	if want := "arn:aws:s3:::acme-docs/*"; statement.Resource != want {
		t.Errorf("Resource = %v, want %q", statement.Resource, want)
	}
	// Nothing in the policy may grant s3:ListBucket, which would expose every key
	// in the bucket to anonymous callers.
	if strings.Contains(policy, "ListBucket") {
		t.Error("the public policy must not grant ListBucket")
	}
}

func TestPublicReadPolicyIsStoredInTheCanonicalShape(t *testing.T) {
	// AWS rewrites `"Principal": "*"` as `{"AWS": "*"}` when it stores a policy.
	// Emitting the canonical form is what stops the operator deciding the policy
	// has drifted and rewriting it on every reconcile.
	policy := PublicReadPolicy("acme-docs")
	if !strings.Contains(policy, `"Principal":{"AWS":"*"}`) {
		t.Errorf("policy should use the canonical principal form, got %s", policy)
	}
}

func TestRenderPolicySubstitutesTheBucket(t *testing.T) {
	template := `{"Statement":[{"Resource":"arn:aws:s3:::${bucket}/*","Sid":"${bucket}"}]}`
	rendered := RenderPolicy(template, "acme-docs")

	if strings.Contains(rendered, "${bucket}") {
		t.Errorf("rendered policy still contains a placeholder: %s", rendered)
	}
	if !strings.Contains(rendered, "arn:aws:s3:::acme-docs/*") {
		t.Errorf("rendered policy = %s", rendered)
	}
}

func TestPoliciesEqualIgnoresProviderRewrites(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		want        bool
	}{
		{
			name:  "identical documents",
			left:  PublicReadPolicy("acme-docs"),
			right: PublicReadPolicy("acme-docs"),
			want:  true,
		},
		{
			name:  "whitespace and key order do not matter",
			left:  `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject"}]}`,
			right: "{\n  \"Statement\": [ { \"Action\": \"s3:GetObject\", \"Effect\": \"Allow\" } ],\n  \"Version\": \"2012-10-17\"\n}",
			want:  true,
		},
		{
			name:  "a bare star principal equals the expanded form",
			left:  `{"Statement":[{"Principal":"*","Action":"s3:GetObject"}]}`,
			right: `{"Statement":[{"Principal":{"AWS":"*"},"Action":"s3:GetObject"}]}`,
			want:  true,
		},
		{
			name:  "a single-element array equals the bare value",
			left:  `{"Statement":[{"Action":["s3:GetObject"]}]}`,
			right: `{"Statement":{"Action":"s3:GetObject"}}`,
			want:  true,
		},
		{
			name:  "action order does not matter",
			left:  `{"Statement":[{"Action":["s3:GetObject","s3:ListBucket"]}]}`,
			right: `{"Statement":[{"Action":["s3:ListBucket","s3:GetObject"]}]}`,
			want:  true,
		},
		{
			name:  "a different resource is a different policy",
			left:  PublicReadPolicy("acme-docs"),
			right: PublicReadPolicy("acme-other"),
			want:  false,
		},
		{
			name:  "a different effect is a different policy",
			left:  `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject"}]}`,
			right: `{"Statement":[{"Effect":"Deny","Action":"s3:GetObject"}]}`,
			want:  false,
		},
		{
			name:  "an extra action is a different policy",
			left:  `{"Statement":[{"Action":["s3:GetObject"]}]}`,
			right: `{"Statement":[{"Action":["s3:GetObject","s3:PutObject"]}]}`,
			want:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := policiesEqual(test.left, test.right)
			if err != nil {
				t.Fatalf("policiesEqual returned an error: %v", err)
			}
			if got != test.want {
				t.Errorf("policiesEqual = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPoliciesEqualReportsInvalidJSON(t *testing.T) {
	if _, err := policiesEqual("not json", PublicReadPolicy("acme-docs")); err == nil {
		t.Fatal("an unparsable policy should be reported")
	}
}

func TestNormalizePolicyIsStable(t *testing.T) {
	policy := PublicReadPolicy("acme-docs")

	first, err := normalizePolicy(policy)
	if err != nil {
		t.Fatalf("normalizePolicy returned an error: %v", err)
	}
	second, err := normalizePolicy(policy)
	if err != nil {
		t.Fatalf("normalizePolicy returned an error: %v", err)
	}
	if first != second {
		t.Error("normalizing the same policy twice should produce the same string")
	}
}

func TestBucketARN(t *testing.T) {
	if got := BucketARN("acme-docs"); got != "arn:aws:s3:::acme-docs" {
		t.Errorf("BucketARN = %q", got)
	}
}
