/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// UserAgent identifies the operator to registries.
const UserAgent = "static-s3-operator"

// Options configure how a reference is resolved.
type Options struct {
	// Keychain authenticates the request. Nil means anonymous.
	Keychain authn.Keychain
	// PlainHTTP talks to the registry over HTTP.
	PlainHTTP bool
	// InsecureSkipTLSVerify skips registry certificate verification.
	InsecureSkipTLSVerify bool
}

// Reference is a resolved image reference.
type Reference struct {
	// Digest is the manifest digest, for example "sha256:abcd...".
	Digest string
	// Pinned is the original reference rewritten to point at Digest, which is
	// what a sync Job pulls so that the tag cannot move underneath it.
	Pinned string
	// WasPinned reports that the input reference already carried a digest, so no
	// registry round trip was needed.
	WasPinned bool
}

// ShortDigest returns the first twelve hex characters of the digest, for log
// lines and label values.
func (r Reference) ShortDigest() string {
	hex := strings.TrimPrefix(r.Digest, "sha256:")
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

// Resolve turns an image reference into a digest-pinned one.
//
// A reference that already carries a digest is returned as it is, without
// contacting the registry: it cannot change, and resolving it anyway would mean
// the operator needs pull credentials for images that only the nodes have to be
// able to fetch.
//
// A tagged reference is resolved with a HEAD against the manifest, which is
// cheap enough to poll on an interval. For a multi-architecture image this
// returns the digest of the index, so the sync Job still gets to pick the
// platform it can extract.
func Resolve(ctx context.Context, image string, opts Options) (*Reference, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return nil, errors.New("image reference is empty")
	}

	var nameOpts []name.Option
	if opts.PlainHTTP {
		nameOpts = append(nameOpts, name.Insecure)
	}

	ref, err := name.ParseReference(image, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse image reference %q: %w", image, err)
	}

	if digestRef, ok := ref.(name.Digest); ok {
		return &Reference{
			Digest:    digestRef.DigestStr(),
			Pinned:    digestRef.String(),
			WasPinned: true,
		}, nil
	}

	descriptor, err := remote.Head(ref, remoteOptions(ctx, opts)...)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", image, classifyError(err))
	}

	return &Reference{
		Digest: descriptor.Digest.String(),
		Pinned: ref.Context().Digest(descriptor.Digest.String()).String(),
	}, nil
}

// Image fetches the image for a resolved reference, selecting platform when the
// reference points at a multi-architecture index.
func Image(ctx context.Context, pinned string, platform *v1.Platform, opts Options) (v1.Image, error) {
	var nameOpts []name.Option
	if opts.PlainHTTP {
		nameOpts = append(nameOpts, name.Insecure)
	}

	ref, err := name.ParseReference(pinned, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse image reference %q: %w", pinned, err)
	}

	remoteOpts := remoteOptions(ctx, opts)
	if platform != nil {
		remoteOpts = append(remoteOpts, remote.WithPlatform(*platform))
	}

	image, err := remote.Image(ref, remoteOpts...)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", pinned, classifyError(err))
	}
	return image, nil
}

func remoteOptions(ctx context.Context, opts Options) []remote.Option {
	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithUserAgent(UserAgent),
	}
	// Falling back to authn.DefaultKeychain would read the ambient docker config
	// of whatever machine the process happens to run on, so an absent keychain
	// means anonymous and nothing else.
	if opts.Keychain != nil {
		remoteOpts = append(remoteOpts, remote.WithAuthFromKeychain(opts.Keychain))
	} else {
		remoteOpts = append(remoteOpts, remote.WithAuth(authn.Anonymous))
	}

	if opts.InsecureSkipTLSVerify {
		insecure := remote.DefaultTransport.(*http.Transport).Clone()
		if insecure.TLSClientConfig == nil {
			insecure.TLSClientConfig = &tls.Config{} //nolint:gosec // overridden below
		}
		insecure.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // opt-in, documented as dev only
		remoteOpts = append(remoteOpts, remote.WithTransport(insecure))
	}

	return remoteOpts
}

// NotFoundError reports that the registry does not have the requested manifest,
// which is worth distinguishing because it is a spec problem the user must fix
// rather than something retrying will resolve.
type NotFoundError struct {
	cause error
}

func (e *NotFoundError) Error() string { return e.cause.Error() }
func (e *NotFoundError) Unwrap() error { return e.cause }

// UnauthorizedError reports that the registry rejected the credentials, so the
// user needs to fix spec.source.imagePullSecrets.
type UnauthorizedError struct {
	cause error
}

func (e *UnauthorizedError) Error() string { return e.cause.Error() }
func (e *UnauthorizedError) Unwrap() error { return e.cause }

// classifyError wraps registry transport errors that the caller should treat as
// terminal instead of retrying at full speed.
func classifyError(err error) error {
	var transportErr *transport.Error
	if !errors.As(err, &transportErr) {
		return err
	}

	switch transportErr.StatusCode {
	case http.StatusNotFound:
		return &NotFoundError{cause: err}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &UnauthorizedError{cause: err}
	default:
		return err
	}
}

// IsTerminal reports whether an error from Resolve or Image will keep happening
// until somebody changes the StaticSite or the registry.
func IsTerminal(err error) bool {
	var notFound *NotFoundError
	var unauthorized *UnauthorizedError
	return errors.As(err, &notFound) || errors.As(err, &unauthorized)
}
