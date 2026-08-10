/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/google/uuid"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

// S3 bucket naming rules the generated name has to satisfy.
const (
	maxBucketNameLength = 63
	minBucketNameLength = 3
	// bucketNameHashLength is how many hex characters of the full name's digest are
	// appended when a name has to be shortened, which is what keeps two long names
	// sharing a prefix from colliding.
	bucketNameHashLength = 8
)

// BucketNameData is what a bucket name template is rendered with.
type BucketNameData struct {
	// OpaqueID is a version 5 UUID derived from the site's namespace and name
	// under the cluster's UUID namespace. It is what the default template uses.
	//
	// A bucket name reaches the public internet: it appears in the website
	// hostname, and so in any CNAME record pointing at the site. Putting the
	// Kubernetes namespace and resource name in it publishes an inventory of the
	// cluster to anyone who looks up the DNS. This does not.
	OpaqueID string
	// ShortID is the first twelve hex digits of OpaqueID, for a template that needs
	// to leave room for a long prefix. It is still 48 bits of the digest.
	ShortID string

	// Namespace is the StaticSite's namespace. Using it in a template puts it in
	// public DNS; see OpaqueID.
	Namespace string
	// Name is the StaticSite's name. The same caveat applies.
	Name string
	// UID is the StaticSite's UID. It is already opaque, but it changes if the
	// resource is deleted and recreated, so a template using it cannot be computed
	// before the resource exists.
	UID string
}

// invalidBucketNameCharacters matches everything a bucket name may not contain.
//
// Dots are excluded deliberately even though S3 permits them: a bucket whose name
// contains a dot cannot be addressed as bucket.endpoint over HTTPS, because the
// wildcard certificate does not cover the extra label.
var invalidBucketNameCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

// looksLikeIPAddress matches the one shape S3 rejects outright.
var looksLikeIPAddress = regexp.MustCompile(`^\d+-\d+-\d+-\d+$|^\d+\.\d+\.\d+\.\d+$`)

// reservedBucketNamePrefixes are prefixes S3 keeps for itself.
var reservedBucketNamePrefixes = []string{"xn--", "sthree-", "sthree-configurator", "amzn-s3-demo-"}

// reservedBucketNameSuffixes are suffixes S3 keeps for itself.
var reservedBucketNameSuffixes = []string{"-s3alias", "--ol-s3", "--x-s3", "--table-s3"}

// OpaqueSiteID returns the version 5 UUID identifying a site.
//
// Version 5 is the standard construction for a deterministic UUID derived from a
// name inside a namespace (RFC 9562, formerly RFC 4122), which is exactly this
// case: the name is "<namespace>/<name>" and the namespace is the cluster's own
// UUID. Using a per-cluster namespace is what makes the result unguessable --
// Kubernetes namespaces and resource names are short and predictable, so an
// unsalted digest of them could simply be enumerated.
//
// It is reproducible outside the cluster with standard tools, which is the point:
//
//	uuidgen --sha1 --namespace "$CLUSTER_UUID" --name "web/docs"
func (d *Defaults) OpaqueSiteID(namespace, name string) (uuid.UUID, error) {
	if d.BucketNameUUIDNamespace == "" {
		return uuid.Nil, fmt.Errorf(
			"no bucket name UUID namespace is configured; the operator normally reads it from the " +
				"cluster, or set --bucket-name-uuid-namespace")
	}

	clusterNamespace, err := uuid.Parse(d.BucketNameUUIDNamespace)
	if err != nil {
		return uuid.Nil, fmt.Errorf("--bucket-name-uuid-namespace %q is not a UUID: %w",
			d.BucketNameUUIDNamespace, err)
	}

	return uuid.NewSHA1(clusterNamespace, []byte(namespace+"/"+name)), nil
}

// GenerateBucketName derives a bucket name for a site that does not name one.
//
// The result is documented and reproducible on purpose: someone who would rather
// create the bucket themselves, or write an IAM policy scoped to it, needs to be
// able to work out the name without asking the cluster.
func (d *Defaults) GenerateBucketName(site *webv1alpha1.StaticSite) (string, error) {
	templateText := d.BucketNameTemplate
	if templateText == "" {
		return "", fmt.Errorf("no bucket name template is configured")
	}

	parsed, err := template.New("bucketName").Option("missingkey=error").Parse(templateText)
	if err != nil {
		return "", fmt.Errorf("parse bucket name template: %w", err)
	}

	data := BucketNameData{
		Namespace: site.Namespace,
		Name:      site.Name,
		UID:       string(site.UID),
	}

	// The opaque identifier needs the cluster's UUID namespace, which a template
	// that does not use it should not have to have configured.
	if templateUsesOpaqueID(templateText) {
		opaque, err := d.OpaqueSiteID(site.Namespace, site.Name)
		if err != nil {
			return "", err
		}
		data.OpaqueID = opaque.String()
		data.ShortID = strings.ReplaceAll(opaque.String(), "-", "")[:12]
	}

	var rendered strings.Builder
	err = parsed.Execute(&rendered, data)
	if err != nil {
		return "", fmt.Errorf("render bucket name template: %w", err)
	}

	name := SanitizeBucketName(rendered.String())
	if name == "" {
		return "", fmt.Errorf(
			"the bucket name template produced %q, which cannot be made into a valid bucket name",
			rendered.String())
	}
	return name, nil
}

// SanitizeBucketName turns an arbitrary string into a legal S3 bucket name, or
// returns "" if it cannot.
//
// It is deterministic: the same input always produces the same output, which is
// what lets the name be documented and pre-created rather than discovered.
func SanitizeBucketName(candidate string) string {
	name := strings.ToLower(strings.TrimSpace(candidate))
	name = invalidBucketNameCharacters.ReplaceAllString(name, "-")

	// Collapse runs of hyphens, which a substitution above can easily create.
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "-")

	if name == "" {
		return ""
	}

	// A name that is too long is truncated and given a digest of the original, so
	// two sites whose names differ only past the cut still get different buckets.
	if len(name) > maxBucketNameLength {
		keep := maxBucketNameLength - bucketNameHashLength - 1
		name = strings.Trim(name[:keep], "-") + "-" + shortDigest(candidate)
	}

	// S3 rejects a name shaped like an IP address, and reserves a few prefixes and
	// suffixes. Padding the offending end sidesteps each without disturbing the
	// readable middle. A prefix has to be added before a suffix is tested, since
	// prefixing can only help the front.
	if looksLikeIPAddress.MatchString(name) || hasReservedPrefix(name) {
		name = "site-" + name
	}
	if hasReservedSuffix(name) {
		name += "-site"
	}
	if len(name) > maxBucketNameLength {
		keep := maxBucketNameLength - bucketNameHashLength - 1
		name = strings.Trim(name[:keep], "-") + "-" + shortDigest(candidate)
	}

	// Pad a very short name with a digest rather than an arbitrary filler, so it
	// stays unique and stays reproducible.
	if len(name) < minBucketNameLength {
		name += "-" + shortDigest(candidate)
	}

	if len(name) < minBucketNameLength || len(name) > maxBucketNameLength {
		return ""
	}
	return name
}

func hasReservedPrefix(name string) bool {
	for _, prefix := range reservedBucketNamePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func hasReservedSuffix(name string) bool {
	for _, suffix := range reservedBucketNameSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// hasReservedAffix reports whether either end of the name is reserved by S3.
func hasReservedAffix(name string) bool {
	return hasReservedPrefix(name) || hasReservedSuffix(name)
}

// templateUsesOpaqueID reports whether a template refers to the derived
// identifier, so that a cluster whose template does not need one is not forced to
// configure a UUID namespace.
func templateUsesOpaqueID(templateText string) bool {
	return strings.Contains(templateText, ".OpaqueID") || strings.Contains(templateText, ".ShortID")
}

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:bucketNameHashLength]
}
