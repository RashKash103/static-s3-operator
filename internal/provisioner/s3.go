/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package provisioner

import (
	"context"
	"fmt"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/config"
	"github.com/rashkash103/static-s3-operator/internal/s3"
)

// S3Provisioner implements Provisioner against a real S3 API.
type S3Provisioner struct {
	// RequestChecksumWhenSupported opts in to the SDK's trailing-checksum uploads.
	RequestChecksumWhenSupported bool
	// SkipTLSVerify disables endpoint certificate verification.
	SkipTLSVerify bool
	// Now supplies timestamps, so tests can pin them.
	Now func() metav1.Time
	// NewClient builds the S3 client. Tests point it at a stub server.
	NewClient func(s3.ClientOptions) (*awss3.Client, error)
}

// NewS3Provisioner returns a provisioner using the real S3 client.
func NewS3Provisioner(requestChecksumWhenSupported, skipTLSVerify bool) *S3Provisioner {
	return &S3Provisioner{
		RequestChecksumWhenSupported: requestChecksumWhenSupported,
		SkipTLSVerify:                skipTLSVerify,
		Now:                          func() metav1.Time { return metav1.Now() },
		NewClient:                    s3.NewClient,
	}
}

func (p *S3Provisioner) buckets(request clientRequest) (*s3.BucketManager, error) {
	newClient := p.NewClient
	if newClient == nil {
		newClient = s3.NewClient
	}

	client, err := newClient(s3.ClientOptions{
		Region:                       request.region,
		Endpoint:                     request.endpoint,
		ForcePathStyle:               request.forcePathStyle,
		Credentials:                  request.credentials,
		RequestChecksumWhenSupported: p.RequestChecksumWhenSupported,
		SkipTLSVerify:                p.SkipTLSVerify,
	})
	if err != nil {
		return nil, err
	}
	return s3.NewBucketManager(client), nil
}

type clientRequest struct {
	region         string
	endpoint       string
	forcePathStyle bool
	credentials    s3.Credentials
}

func (p *S3Provisioner) now() metav1.Time {
	if p.Now != nil {
		return p.Now()
	}
	return metav1.Now()
}

// Provision brings the bucket to the state spec.bucket describes.
//
// The order matters. The bucket has to exist before anything can be configured
// on it, the public access block has to be cleared before a public-read policy
// takes effect, and the ownership record has to be written as part of the same
// call that created the bucket so that a bucket can never end up created but
// unclaimed.
func (p *S3Provisioner) Provision(
	ctx context.Context,
	request ProvisionRequest,
) (*ProvisionResult, error) {
	effective := request.Effective

	buckets, err := p.buckets(clientRequest{
		region:         effective.Region,
		endpoint:       effective.Endpoint,
		forcePathStyle: effective.ForcePathStyle,
		credentials:    request.Credentials,
	})
	if err != nil {
		return nil, err
	}

	result := &ProvisionResult{}

	ownership, err := p.establishOwnership(ctx, buckets, request, result)
	if err != nil {
		return nil, err
	}
	result.Ownership = ownership

	if err := p.configureAccess(ctx, buckets, effective, result); err != nil {
		return nil, err
	}
	if err := p.configureWebsite(ctx, buckets, effective, result); err != nil {
		return nil, err
	}

	// Tags carry the ownership token, so they are only reconciled once ownership
	// exists; for an adopted bucket the operator leaves the tag set alone rather
	// than stamping its own name on something it does not own.
	if ownership != nil && ownership.Token != "" {
		changed, err := buckets.EnsureTags(ctx, effective.Bucket, effective.Tags, s3.Ownership{
			Token: ownership.Token,
			Site:  request.Site,
			UID:   request.SiteUID,
		})
		if err != nil {
			return nil, err
		}
		if changed {
			result.Changes = append(result.Changes, "tags")
		}
	}

	return result, nil
}

// establishOwnership works out whether this site owns the bucket, creating it
// when it does not exist yet.
//
// A bucket that already exists is inspected rather than trusted: if it carries
// this site's token the operator recovers its own record, which is what makes
// the ownership survive a lost status update. If it carries somebody else's
// token, that is a conflict worth failing on, because two sites writing to one
// bucket would silently delete each other's files. If it carries no token at all
// the bucket is adopted, and adopted buckets are never deleted.
func (p *S3Provisioner) establishOwnership(
	ctx context.Context,
	buckets *s3.BucketManager,
	request ProvisionRequest,
	result *ProvisionResult,
) (*webv1alpha1.BucketOwnership, error) {
	effective := request.Effective

	exists, err := buckets.Exists(ctx, effective.Bucket)
	if err != nil {
		return nil, err
	}

	if !exists {
		if !effective.CreateBucket {
			return nil, fmt.Errorf(
				"bucket %q does not exist and spec.bucket.create is false", effective.Bucket)
		}

		token, err := s3.NewOwnershipToken()
		if err != nil {
			return nil, err
		}

		created, err := buckets.Create(ctx, effective.Bucket, effective.Region)
		if err != nil {
			return nil, err
		}

		if created {
			kind, err := buckets.WriteOwnership(ctx, effective.Bucket, s3.Ownership{
				Token: token,
				Site:  request.Site,
				UID:   request.SiteUID,
			}, effective.Tags)
			if err != nil {
				return nil, err
			}
			if kind == s3.MarkerKindNone {
				return nil, fmt.Errorf(
					"created bucket %q but could not record ownership, so it could never be deleted safely",
					effective.Bucket)
			}

			now := p.now()
			result.Created = true
			result.Changes = append(result.Changes, "created")
			return &webv1alpha1.BucketOwnership{
				Created:    true,
				Token:      token,
				CreatedAt:  &now,
				VerifiedAt: &now,
			}, nil
		}
		// Create reported the bucket already belonged to these credentials, so fall
		// through and inspect whatever is already recorded on it.
	}

	existing, kind, err := buckets.ReadOwnership(ctx, effective.Bucket)
	if err != nil {
		return nil, err
	}

	switch {
	case kind == s3.MarkerKindNone:
		result.Adopted = true
		return nil, nil

	case request.Ownership != nil && request.Ownership.Token == existing.Token:
		now := p.now()
		recovered := *request.Ownership
		recovered.VerifiedAt = &now
		return &recovered, nil

	case request.Ownership == nil || request.Ownership.Token == "":
		// The bucket is claimed but this site has no record of claiming it. That is
		// either somebody else's bucket or a status that was lost along with the
		// only copy of the token; either way the operator must not assume it owns it.
		return nil, fmt.Errorf(
			"bucket %q already carries an ownership record from another StaticSite (%s); "+
				"pick a different bucket name or delete that resource first",
			effective.Bucket, describeOwner(existing))

	default:
		return nil, fmt.Errorf(
			"bucket %q is owned by another StaticSite (%s)", effective.Bucket, describeOwner(existing))
	}
}

func describeOwner(own *s3.Ownership) string {
	if own == nil {
		return "unknown"
	}
	if own.Site != "" {
		return own.Site
	}
	return "token " + shorten(own.Token)
}

func shorten(token string) string {
	if len(token) > 12 {
		return token[:12] + "..."
	}
	return token
}

// configureAccess makes the bucket's public readability match the spec.
func (p *S3Provisioner) configureAccess(
	ctx context.Context,
	buckets *s3.BucketManager,
	effective *config.Effective,
	result *ProvisionResult,
) error {
	policy := effective.Policy
	if policy != "" {
		policy = s3.RenderPolicy(policy, effective.Bucket)
	} else if effective.PublicRead {
		policy = s3.PublicReadPolicy(effective.Bucket)
	}

	if policy != "" {
		// AWS blocks public bucket policies by default, which silently defeats the
		// policy below, so the block is cleared first.
		if err := buckets.AllowPublicPolicy(ctx, effective.Bucket); err != nil {
			return err
		}
	}

	changed, err := buckets.EnsurePolicy(ctx, effective.Bucket, policy)
	if err != nil {
		return err
	}
	if changed {
		result.Changes = append(result.Changes, "policy")
	}
	return nil
}

// configureWebsite makes the bucket's website hosting match the spec.
func (p *S3Provisioner) configureWebsite(
	ctx context.Context,
	buckets *s3.BucketManager,
	effective *config.Effective,
	result *ProvisionResult,
) error {
	if !effective.WebsiteEnabled {
		return buckets.RemoveWebsite(ctx, effective.Bucket)
	}

	changed, err := buckets.EnsureWebsite(ctx, effective.Bucket, s3.WebsiteConfig{
		IndexDocument: effective.IndexDocument,
		ErrorDocument: effective.ErrorDocument,
	})
	if err != nil {
		return err
	}
	if changed {
		result.Changes = append(result.Changes, "website")
	}
	return nil
}

// Deprovision destroys the bucket, but only for a bucket this site created.
//
// This is the one operation that can lose data irrecoverably, so it refuses by
// default: without an ownership record that matches the token in the bucket
// itself, the bucket is retained and the reason is reported. The
// force-bucket-deletion annotation is the only way past that check.
func (p *S3Provisioner) Deprovision(
	ctx context.Context,
	request DeprovisionRequest,
) (*DeprovisionResult, error) {
	effective := request.Effective

	if effective.DeletionPolicy == webv1alpha1.DeletionPolicyRetain {
		return &DeprovisionResult{RetainedReason: "spec.bucket.deletionPolicy is Retain"}, nil
	}

	buckets, err := p.buckets(clientRequest{
		region:         effective.Region,
		endpoint:       effective.Endpoint,
		forcePathStyle: effective.ForcePathStyle,
		credentials:    request.Credentials,
	})
	if err != nil {
		return nil, err
	}

	exists, err := buckets.Exists(ctx, effective.Bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &DeprovisionResult{Deleted: true}, nil
	}

	if !request.Force {
		token := ""
		if request.Ownership != nil {
			token = request.Ownership.Token
		}
		owned, reason, err := buckets.VerifyOwnership(ctx, effective.Bucket, token, request.SiteUID)
		if err != nil {
			return nil, err
		}
		if !owned {
			return &DeprovisionResult{RetainedReason: reason}, nil
		}
	}

	removed, err := buckets.Delete(ctx, effective.Bucket)
	if err != nil {
		return &DeprovisionResult{ObjectsRemoved: removed}, err
	}
	return &DeprovisionResult{Deleted: true, ObjectsRemoved: removed}, nil
}
