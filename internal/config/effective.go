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
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

// Effective is a StaticSite spec with every optional field filled in from the
// operator's defaults. Both the controller and the sync Job configuration are
// derived from it, so defaulting happens exactly once and in one place.
type Effective struct {
	// Source
	Image                 string
	SourcePath            string
	PullInterval          time.Duration
	ImagePullSecrets      []string
	PlainHTTP             bool
	InsecureSkipTLSVerify bool

	// Bucket
	Bucket         string
	Region         string
	Endpoint       string
	ForcePathStyle bool
	CreateBucket   bool
	DeletionPolicy webv1alpha1.DeletionPolicy
	WebsiteEnabled bool
	IndexDocument  string
	ErrorDocument  string
	PublicRead     bool
	Policy         string
	Tags           map[string]string
	WebsiteURL     string

	// Sync
	DeleteRemoved       bool
	ACL                 string
	KeyPrefix           string
	Excludes            []string
	CacheControl        []webv1alpha1.GlobRule
	DefaultCacheControl string
	ContentTypes        []webv1alpha1.GlobRule
	DefaultContentType  string
	Concurrency         int
	RetainHistory       int

	// Job
	SyncerImage           string
	SyncerImagePullPolicy corev1.PullPolicy
	MaxExtractBytes       int64
	MaxExtractFiles       int

	// Service
	ServiceEnabled      bool
	ServiceName         string
	ServiceExternalName string
	ServiceAnnotations  map[string]string
	ServiceLabels       map[string]string
}

// Effective resolves site against the operator defaults. It returns an error
// only for problems the CRD schema cannot express, such as an unparsable
// website endpoint template.
func (d *Defaults) Effective(site *webv1alpha1.StaticSite) (*Effective, error) {
	src := site.Spec.Source
	bucket := site.Spec.Bucket
	sync := site.Spec.Sync

	e := &Effective{
		Image:                 src.Image,
		SourcePath:            strings.TrimRight(firstNonEmpty(src.Path, d.SourcePath), "/"),
		PullInterval:          d.PullInterval,
		PlainHTTP:             src.PlainHTTP,
		InsecureSkipTLSVerify: src.InsecureSkipTLSVerify,

		Bucket:         bucket.Name,
		Region:         firstNonEmpty(bucket.Region, d.Region),
		Endpoint:       normalizeEndpoint(firstNonEmpty(bucket.Endpoint, d.Endpoint)),
		ForcePathStyle: boolValue(bucket.ForcePathStyle, d.ForcePathStyle),
		CreateBucket:   boolValue(bucket.Create, true),
		DeletionPolicy: bucket.DeletionPolicy,
		PublicRead:     boolValue(bucket.PublicRead, true),
		Policy:         bucket.Policy,
		Tags:           bucket.Tags,

		DeleteRemoved:       boolValue(sync.DeleteRemoved, true),
		ACL:                 string(sync.ACL),
		KeyPrefix:           sync.KeyPrefix,
		Excludes:            sync.Excludes,
		CacheControl:        sync.CacheControl,
		DefaultCacheControl: d.CacheControl,
		ContentTypes:        sync.ContentTypes,
		DefaultContentType:  firstNonEmpty(sync.DefaultContentType, "application/octet-stream"),
		Concurrency:         int(int32Value(sync.Concurrency, 8)),
		RetainHistory:       int(int32Value(sync.RetainHistory, 3)),

		SyncerImage:           firstNonEmpty(sync.Job.Image, d.SyncerImage),
		SyncerImagePullPolicy: corev1.PullPolicy(firstNonEmpty(string(sync.Job.ImagePullPolicy), d.SyncerImagePullPolicy)),
		MaxExtractBytes:       d.MaxExtractBytes,
		MaxExtractFiles:       d.MaxExtractFiles,
	}

	// "0" means polling off, which is distinct from "unset means use the default".
	if src.PullInterval != nil {
		e.PullInterval = src.PullInterval.Duration
	}

	if e.DeletionPolicy == "" {
		e.DeletionPolicy = webv1alpha1.DeletionPolicyDelete
	}
	if e.ACL == "" {
		e.ACL = string(webv1alpha1.ACLPublicRead)
	}

	for _, ref := range src.ImagePullSecrets {
		if ref.Name != "" {
			e.ImagePullSecrets = append(e.ImagePullSecrets, ref.Name)
		}
	}

	if bucket.Website != nil {
		e.WebsiteEnabled = true
		e.IndexDocument = firstNonEmpty(bucket.Website.IndexDocument, d.IndexDocument)
		// An empty --default-error-document deliberately means "no error document",
		// so only the spec value can turn one on when the default is empty.
		e.ErrorDocument = firstNonEmpty(bucket.Website.ErrorDocument, d.ErrorDocument)
	}

	websiteURL, err := renderWebsiteURL(
		firstNonEmpty(bucket.WebsiteEndpointTemplate, d.WebsiteEndpointTemplate), e)
	if err != nil {
		return nil, err
	}
	e.WebsiteURL = websiteURL

	d.applyServiceDefaults(site, e)

	return e, nil
}

// applyServiceDefaults resolves the ExternalName Service settings.
func (d *Defaults) applyServiceDefaults(site *webv1alpha1.StaticSite, e *Effective) {
	service := site.Spec.Service

	e.ServiceEnabled = boolValue(service.Enabled, true)
	e.ServiceName = firstNonEmpty(service.Name, site.Name)
	e.ServiceLabels = service.Labels

	e.ServiceExternalName = service.ExternalName
	if e.ServiceExternalName == "" {
		// A Service can only ever point at a hostname, so the path part of a
		// path-style website URL is necessarily dropped here. That limitation is
		// documented on spec.service.externalName.
		if parsed, err := url.Parse(e.WebsiteURL); err == nil {
			e.ServiceExternalName = parsed.Hostname()
		}
	}

	annotations := map[string]string{}
	if d.ExternalDNSHostnameAnnotation != "" && len(service.Hostnames) > 0 {
		annotations[d.ExternalDNSHostnameAnnotation] = strings.Join(service.Hostnames, ",")
	}
	for key, value := range service.Annotations {
		annotations[key] = value
	}
	if len(annotations) > 0 {
		e.ServiceAnnotations = annotations
	}
}

// SyncSpecHash fingerprints the settings that change what a sync would write.
// A change here means the bucket contents may be stale even though the image
// digest is unchanged, so the operator runs a fresh sync.
func (e *Effective) SyncSpecHash() string {
	// Only sync-affecting fields belong here: adding, say, the retry backoff
	// would make unrelated operator flag changes re-sync every site in the
	// cluster.
	fingerprint := struct {
		SourcePath          string                 `json:"sourcePath"`
		Bucket              string                 `json:"bucket"`
		Region              string                 `json:"region"`
		Endpoint            string                 `json:"endpoint"`
		ForcePathStyle      bool                   `json:"forcePathStyle"`
		DeleteRemoved       bool                   `json:"deleteRemoved"`
		ACL                 string                 `json:"acl"`
		KeyPrefix           string                 `json:"keyPrefix"`
		Excludes            []string               `json:"excludes"`
		CacheControl        []webv1alpha1.GlobRule `json:"cacheControl"`
		DefaultCacheControl string                 `json:"defaultCacheControl"`
		ContentTypes        []webv1alpha1.GlobRule `json:"contentTypes"`
		DefaultContentType  string                 `json:"defaultContentType"`
	}{
		SourcePath:          e.SourcePath,
		Bucket:              e.Bucket,
		Region:              e.Region,
		Endpoint:            e.Endpoint,
		ForcePathStyle:      e.ForcePathStyle,
		DeleteRemoved:       e.DeleteRemoved,
		ACL:                 e.ACL,
		KeyPrefix:           e.KeyPrefix,
		Excludes:            e.Excludes,
		CacheControl:        e.CacheControl,
		DefaultCacheControl: e.DefaultCacheControl,
		ContentTypes:        e.ContentTypes,
		DefaultContentType:  e.DefaultContentType,
	}

	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		// The struct above is made of plain types, so this cannot happen; hashing
		// the error keeps the function total rather than panicking in a reconcile.
		encoded = []byte(err.Error())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// websiteURLData is the data a website endpoint template is rendered with.
type websiteURLData struct {
	Bucket   string
	Region   string
	Endpoint string
}

// awsDashWebsiteRegions are the AWS regions whose website endpoints use
// "s3-website-<region>" rather than "s3-website.<region>". AWS keeps both forms
// working for the older regions, but only the dash form for some of them.
var awsDashWebsiteRegions = map[string]bool{
	"us-east-1":      true,
	"us-west-1":      true,
	"us-west-2":      true,
	"ap-east-1":      true,
	"ap-northeast-1": true,
	"ap-northeast-2": true,
	"ap-northeast-3": true,
	"ap-southeast-1": true,
	"ap-southeast-2": true,
	"ap-south-1":     true,
	"eu-west-1":      true,
	"sa-east-1":      true,
	"us-gov-west-1":  true,
}

// renderWebsiteURL works out the URL a browser should use for the site.
//
// A template always wins, because only the provider knows its own website
// hostname. Without one, AWS gets its documented website endpoint and an
// S3-compatible endpoint gets a best guess derived from the API endpoint, which
// is right often enough to be useful and overridable when it is not.
func renderWebsiteURL(tmpl string, e *Effective) (string, error) {
	data := websiteURLData{Bucket: e.Bucket, Region: e.Region, Endpoint: e.Endpoint}

	if tmpl != "" {
		parsed, err := template.New("websiteEndpoint").Option("missingkey=error").Parse(tmpl)
		if err != nil {
			return "", fmt.Errorf("parse website endpoint template: %w", err)
		}
		var out strings.Builder
		if err := parsed.Execute(&out, data); err != nil {
			return "", fmt.Errorf("render website endpoint template: %w", err)
		}
		return strings.TrimSpace(out.String()), nil
	}

	if e.Endpoint == "" {
		host := fmt.Sprintf("s3-website.%s.amazonaws.com", e.Region)
		if awsDashWebsiteRegions[e.Region] {
			host = fmt.Sprintf("s3-website-%s.amazonaws.com", e.Region)
		}
		return fmt.Sprintf("http://%s.%s", e.Bucket, host), nil
	}

	parsed, err := url.Parse(e.Endpoint)
	if err != nil || parsed.Host == "" {
		// The endpoint is validated elsewhere; an unusable one just means the
		// operator cannot guess a URL, which is not worth failing a reconcile over.
		return "", nil
	}
	if e.ForcePathStyle {
		return strings.TrimRight(parsed.String(), "/") + "/" + e.Bucket + "/", nil
	}
	parsed.Host = e.Bucket + "." + parsed.Host
	return strings.TrimRight(parsed.String(), "/") + "/", nil
}

// normalizeEndpoint gives a scheme-less endpoint the https scheme, so that
// "s3.example.com" and "https://s3.example.com" behave the same.
func normalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if !strings.Contains(endpoint, "://") {
		return "https://" + endpoint
	}
	return endpoint
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func int32Value(value *int32, fallback int32) int32 {
	if value == nil {
		return fallback
	}
	return *value
}
