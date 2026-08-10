/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package provisioner is the seam between the controller and the S3 API.
//
// The controller depends on the Provisioner interface rather than on a concrete
// S3 client, which is what lets the reconcile loop be tested against envtest
// with no object store in sight, while the S3 implementation is tested directly
// against a real server.
package provisioner

import (
	"context"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/config"
	"github.com/rashkash103/static-s3-operator/internal/s3"
)

// ProvisionRequest is everything needed to bring a bucket to its desired state.
type ProvisionRequest struct {
	// Effective is the site's spec with operator defaults applied.
	Effective *config.Effective
	// Credentials authenticate against the provider.
	Credentials s3.Credentials
	// Site is the owning StaticSite as "namespace/name", recorded in bucket tags.
	Site string
	// SiteUID is the owning StaticSite's UID, recorded in bucket tags.
	SiteUID string
	// Ownership is what the site already recorded, if anything. A site that has
	// created its bucket before passes its existing record so the token survives.
	Ownership *webv1alpha1.BucketOwnership
}

// ProvisionResult reports what the provisioner found or changed.
type ProvisionResult struct {
	// Created is true when this call created the bucket.
	Created bool
	// Adopted is true when the bucket already existed without an ownership record
	// from this site, which permanently excludes it from deletion.
	Adopted bool
	// Ownership is the record to store in status. It is nil for an adopted bucket.
	Ownership *webv1alpha1.BucketOwnership
	// Changes names what was reconciled, for an event and a log line. It is empty
	// when the bucket was already in the desired state.
	Changes []string
}

// DeprovisionRequest is everything needed to decide whether a bucket may be
// destroyed, and to destroy it.
type DeprovisionRequest struct {
	// Effective is the site's spec with operator defaults applied.
	Effective *config.Effective
	// Credentials authenticate against the provider.
	Credentials s3.Credentials
	// Ownership is what the site recorded when it created the bucket. Without a
	// matching record the bucket is retained.
	Ownership *webv1alpha1.BucketOwnership
	// SiteUID is the owning StaticSite's UID.
	SiteUID string
	// Force skips the ownership check. It is only set by the
	// "web.rka.sh/force-bucket-deletion" annotation.
	Force bool
}

// DeprovisionResult reports what happened to the bucket.
type DeprovisionResult struct {
	// Deleted is true when the bucket was removed.
	Deleted bool
	// ObjectsRemoved counts the objects deleted on the way.
	ObjectsRemoved int
	// RetainedReason explains why the bucket was left in place. It is empty when
	// the bucket was deleted.
	RetainedReason string
}

// Provisioner reconciles the S3 side of a StaticSite.
type Provisioner interface {
	// Provision creates the bucket if needed and makes its website configuration,
	// policy, public access and tags match the spec. It is idempotent.
	Provision(ctx context.Context, request ProvisionRequest) (*ProvisionResult, error)

	// Deprovision removes the bucket, but only when ownership can be proven or
	// the caller forces it. Otherwise it reports why the bucket was retained.
	Deprovision(ctx context.Context, request DeprovisionRequest) (*DeprovisionResult, error)
}
