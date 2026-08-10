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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The operator only ever deletes a bucket it can prove it created, and the proof
// has to live in the bucket rather than only in the StaticSite's status:
// recreating a StaticSite that points at a bucket somebody else made must not
// hand the operator permission to destroy it.
//
// The proof is a random token written at creation time. It is stored in the
// bucket's tags, which are private and cost no extra request because the
// operator manages the tag set anyway. Providers that do not implement bucket
// tagging get a marker object instead. That object may be world readable when
// the site is public, so it holds nothing but the token: no cluster, namespace
// or resource names, and the token itself is a random string that grants
// nothing.
const (
	// TagPrefix namespaces the tags the operator manages. Site tags using this
	// prefix are rejected so a user cannot overwrite the ownership record.
	TagPrefix = "static-s3-operator/"

	// TagManagedBy marks the bucket as operator managed.
	TagManagedBy = TagPrefix + "managed-by"
	// TagOwnershipToken holds the ownership token.
	TagOwnershipToken = TagPrefix + "ownership-token"
	// TagSite holds the owning StaticSite as "namespace/name".
	TagSite = TagPrefix + "staticsite"
	// TagSiteUID holds the owning StaticSite's UID, which distinguishes a
	// recreated resource from the original.
	TagSiteUID = TagPrefix + "staticsite-uid"

	// ManagedByValue is the value of TagManagedBy.
	ManagedByValue = "static-s3-operator"

	// MarkerKey is the object holding the ownership token on providers without
	// bucket tagging. The sync engine never uploads or deletes this key.
	MarkerKey = ".static-s3-operator-ownership.json"

	// markerVersion versions the marker object's payload.
	markerVersion = 1
)

// MarkerKind records where a bucket's ownership token is stored.
type MarkerKind string

const (
	// MarkerKindTags means the token lives in the bucket's tag set.
	MarkerKindTags MarkerKind = "Tags"
	// MarkerKindObject means the token lives in the marker object, because the
	// provider does not implement bucket tagging.
	MarkerKindObject MarkerKind = "Object"
	// MarkerKindNone means no ownership record could be found or written.
	MarkerKindNone MarkerKind = "None"
)

// Ownership identifies the StaticSite that created a bucket.
type Ownership struct {
	// Token is the random value that proves the operator created the bucket.
	Token string
	// Site is the owning StaticSite as "namespace/name". It is only recorded in
	// bucket tags, never in the potentially public marker object.
	Site string
	// UID is the owning StaticSite's UID, recorded in tags only.
	UID string
}

// markerPayload is the marker object's body. It deliberately carries no
// identifying information, because the object can be publicly readable.
type markerPayload struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	Note    string `json:"note"`
}

const markerNote = "Ownership marker written by static-s3-operator. " +
	"Deleting this object stops the operator from deleting this bucket."

// NewOwnershipToken returns a fresh 256-bit token.
func NewOwnershipToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate ownership token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// WriteOwnership records own on the bucket, alongside the site's own tags.
//
// It prefers bucket tags and falls back to the marker object when the provider
// does not implement tagging. The returned MarkerKind says which mechanism took
// effect, and MarkerKindNone means ownership could not be recorded at all, in
// which case the operator will refuse to delete this bucket later.
func (m *BucketManager) WriteOwnership(
	ctx context.Context,
	bucket string,
	own Ownership,
	siteTags map[string]string,
) (MarkerKind, error) {
	if own.Token == "" {
		return MarkerKindNone, fmt.Errorf("ownership token is empty")
	}

	tagErr := m.putTags(ctx, bucket, ownershipTags(own, siteTags))
	if tagErr == nil {
		return MarkerKindTags, nil
	}
	if !IsUnsupported(tagErr) && !IsAccessDenied(tagErr) {
		return MarkerKindNone, fmt.Errorf("tag bucket %q: %w", bucket, tagErr)
	}

	if err := m.putMarkerObject(ctx, bucket, own.Token); err != nil {
		return MarkerKindNone, fmt.Errorf(
			"record bucket ownership: tagging failed (%v) and the marker object failed: %w", tagErr, err)
	}
	return MarkerKindObject, nil
}

// ReadOwnership reads whatever ownership record the bucket has. A bucket with no
// record is reported as MarkerKindNone with a nil Ownership, which is not an
// error: an adopted bucket looks exactly like that.
func (m *BucketManager) ReadOwnership(ctx context.Context, bucket string) (*Ownership, MarkerKind, error) {
	tags, err := m.getTags(ctx, bucket)
	switch {
	case err == nil:
		if token := tags[TagOwnershipToken]; token != "" {
			return &Ownership{
				Token: token,
				Site:  tags[TagSite],
				UID:   tags[TagSiteUID],
			}, MarkerKindTags, nil
		}
	case IsMissingConfiguration(err), IsUnsupported(err), IsAccessDenied(err):
		// No tag set, or a provider that cannot report one; fall through to the
		// marker object.
	default:
		return nil, MarkerKindNone, fmt.Errorf("read bucket tags for %q: %w", bucket, err)
	}

	token, err := m.getMarkerObject(ctx, bucket)
	switch {
	case err == nil && token != "":
		return &Ownership{Token: token}, MarkerKindObject, nil
	case err == nil, IsNotFound(err):
		return nil, MarkerKindNone, nil
	case IsAccessDenied(err):
		return nil, MarkerKindNone, nil
	default:
		return nil, MarkerKindNone, fmt.Errorf("read ownership marker for %q: %w", bucket, err)
	}
}

// VerifyOwnership reports whether the bucket's ownership record matches the
// token the StaticSite recorded when it created the bucket. It is the last gate
// before a bucket is deleted, so anything short of an exact match is a refusal.
func (m *BucketManager) VerifyOwnership(
	ctx context.Context,
	bucket, expectedToken, expectedUID string,
) (bool, string, error) {
	if expectedToken == "" {
		return false, "the StaticSite has no recorded ownership token, so the operator did not create this bucket", nil
	}

	own, kind, err := m.ReadOwnership(ctx, bucket)
	if err != nil {
		return false, "", err
	}
	if kind == MarkerKindNone || own == nil {
		return false, "the bucket carries no ownership record from this operator", nil
	}
	if own.Token != expectedToken {
		return false, "the bucket's ownership token belongs to a different StaticSite", nil
	}
	// The UID is only present in the tag-based record. When it is, it catches a
	// StaticSite that was deleted and recreated with the same name.
	if own.UID != "" && expectedUID != "" && own.UID != expectedUID {
		return false, "the bucket was created by an earlier StaticSite with the same name", nil
	}
	return true, "", nil
}

// ownershipTags merges the operator's tags with the site's, dropping any site
// tag that tries to use the reserved prefix.
func ownershipTags(own Ownership, siteTags map[string]string) map[string]string {
	merged := make(map[string]string, len(siteTags)+4)
	for key, value := range siteTags {
		if strings.HasPrefix(key, TagPrefix) {
			continue
		}
		merged[key] = value
	}
	merged[TagManagedBy] = ManagedByValue
	merged[TagOwnershipToken] = own.Token
	if own.Site != "" {
		merged[TagSite] = own.Site
	}
	if own.UID != "" {
		merged[TagSiteUID] = own.UID
	}
	return merged
}

func (m *BucketManager) putMarkerObject(ctx context.Context, bucket, token string) error {
	body, err := json.MarshalIndent(markerPayload{
		Version: markerVersion,
		Token:   token,
		Note:    markerNote,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ownership marker: %w", err)
	}

	_, err = m.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(MarkerKey),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
		// The bucket policy may still expose this object; the payload is written
		// with that in mind. Asking for a private ACL costs nothing and helps on
		// providers where the policy does not cover it.
		ACL: "private",
	})
	if err != nil {
		return fmt.Errorf("write ownership marker: %w", err)
	}
	return nil
}

func (m *BucketManager) getMarkerObject(ctx context.Context, bucket string) (string, error) {
	out, err := m.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(MarkerKey),
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Body.Close() }()

	// The marker is a few hundred bytes; the limit stops a replaced object from
	// being read into memory without bound.
	body, err := io.ReadAll(io.LimitReader(out.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read ownership marker: %w", err)
	}

	var payload markerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// Something else owns this key. That is not a token match, so treat it as
		// no ownership record rather than as a hard failure.
		return "", nil
	}
	return payload.Token, nil
}
