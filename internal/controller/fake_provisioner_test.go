/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/provisioner"
)

// fakeProvisioner is an in-memory object store standing in for a real S3
// provider.
//
// It reproduces the one behaviour the controller's correctness depends on: a
// bucket remembers who created it, and only that creator may delete it. Tests
// can also pre-create a bucket with no ownership record, which is how an adopted
// bucket looks, or with somebody else's token, which is how a name collision
// between two StaticSites looks.
type fakeProvisioner struct {
	mu      sync.Mutex
	buckets map[string]*fakeBucket

	// provisionErr and deprovisionErr make the provider fail, for the retry and
	// abandon paths.
	provisionErr   error
	deprovisionErr error

	provisionCalls   int
	deprovisionCalls int

	// nextToken makes generated ownership tokens predictable.
	nextToken int
}

type fakeBucket struct {
	// ownershipToken is empty for a bucket the operator did not create.
	ownershipToken string
	siteUID        string
	site           string
	website        *webv1alpha1.WebsiteSpec
	policy         string
	tags           map[string]string
	deleted        bool
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{buckets: map[string]*fakeBucket{}}
}

func (p *fakeProvisioner) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buckets = map[string]*fakeBucket{}
	p.provisionErr = nil
	p.deprovisionErr = nil
	p.provisionCalls = 0
	p.deprovisionCalls = 0
	p.nextToken = 0
}

// preCreateUnowned puts a bucket in place with no ownership record, which is what
// a bucket created by somebody else looks like.
func (p *fakeProvisioner) preCreateUnowned(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buckets[name] = &fakeBucket{}
}

// preCreateOwnedByOther puts a bucket in place carrying a different site's token.
func (p *fakeProvisioner) preCreateOwnedByOther(name, site string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buckets[name] = &fakeBucket{ownershipToken: "someone-elses-token", site: site, siteUID: "other-uid"}
}

func (p *fakeProvisioner) failProvision(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.provisionErr = err
}

func (p *fakeProvisioner) failDeprovision(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deprovisionErr = err
}

func (p *fakeProvisioner) bucket(name string) *fakeBucket {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, found := p.buckets[name]
	if !found {
		return nil
	}
	copied := *existing
	return &copied
}

func (p *fakeProvisioner) exists(name string) bool {
	bucket := p.bucket(name)
	return bucket != nil && !bucket.deleted
}

func (p *fakeProvisioner) Provision(
	_ context.Context,
	request provisioner.ProvisionRequest,
) (*provisioner.ProvisionResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.provisionCalls++
	if p.provisionErr != nil {
		return nil, p.provisionErr
	}
	if !request.Credentials.Valid() {
		return nil, errors.New("fake provider was given no credentials")
	}

	name := request.Effective.Bucket
	result := &provisioner.ProvisionResult{}

	existing, found := p.buckets[name]
	if !found || existing.deleted {
		if !request.Effective.CreateBucket {
			return nil, fmt.Errorf("bucket %q does not exist and spec.bucket.create is false", name)
		}
		p.nextToken++
		token := fmt.Sprintf("fake-token-%d", p.nextToken)
		now := metav1.Now()

		p.buckets[name] = &fakeBucket{
			ownershipToken: token,
			siteUID:        request.SiteUID,
			site:           request.Site,
			policy:         request.Effective.Policy,
			tags:           request.Effective.Tags,
		}
		result.Created = true
		result.Changes = []string{"created"}
		result.Ownership = &webv1alpha1.BucketOwnership{
			Created:    true,
			Token:      token,
			CreatedAt:  &now,
			VerifiedAt: &now,
		}
		return result, nil
	}

	switch {
	case existing.ownershipToken == "":
		result.Adopted = true
		return result, nil

	case request.Ownership != nil && request.Ownership.Token == existing.ownershipToken:
		now := metav1.Now()
		recovered := *request.Ownership
		recovered.VerifiedAt = &now
		result.Ownership = &recovered
		return result, nil

	case existing.siteUID != "" && existing.siteUID == request.SiteUID:
		// The bucket records this StaticSite's own UID, so it is ours even when the
		// status handed in does not say so, which is what a cache that has not caught
		// up looks like.
		now := metav1.Now()
		result.Ownership = &webv1alpha1.BucketOwnership{
			Created:    true,
			Token:      existing.ownershipToken,
			VerifiedAt: &now,
		}
		return result, nil

	default:
		return nil, fmt.Errorf("bucket %q is owned by another StaticSite (%s)", name, existing.site)
	}
}

func (p *fakeProvisioner) Deprovision(
	_ context.Context,
	request provisioner.DeprovisionRequest,
) (*provisioner.DeprovisionResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.deprovisionCalls++
	if p.deprovisionErr != nil {
		return nil, p.deprovisionErr
	}

	if request.Effective.DeletionPolicy == webv1alpha1.DeletionPolicyRetain {
		return &provisioner.DeprovisionResult{
			RetainedReason: "spec.bucket.deletionPolicy is Retain",
		}, nil
	}

	name := request.Effective.Bucket
	existing, found := p.buckets[name]
	if !found || existing.deleted {
		return &provisioner.DeprovisionResult{Deleted: true}, nil
	}

	if !request.Force {
		token := ""
		if request.Ownership != nil {
			token = request.Ownership.Token
		}
		switch {
		case token == "":
			return &provisioner.DeprovisionResult{
				RetainedReason: "the StaticSite has no recorded ownership token, " +
					"so the operator did not create this bucket",
			}, nil
		case existing.ownershipToken == "":
			return &provisioner.DeprovisionResult{
				RetainedReason: "the bucket carries no ownership record from this operator",
			}, nil
		case existing.ownershipToken != token:
			return &provisioner.DeprovisionResult{
				RetainedReason: "the bucket's ownership token belongs to a different StaticSite",
			}, nil
		}
	}

	existing.deleted = true
	return &provisioner.DeprovisionResult{Deleted: true, ObjectsRemoved: 3}, nil
}
