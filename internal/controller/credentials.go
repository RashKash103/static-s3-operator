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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/s3"
)

// Keys used in the Secret the operator mirrors into a site's namespace.
const (
	mirroredAccessKeyIDKey     = "accessKeyID"
	mirroredSecretAccessKeyKey = "secretAccessKey"
	mirroredSessionTokenKey    = "sessionToken"
)

// resolvedCredentials is both the access key the controller signs its own
// requests with, and the Secret reference a sync Job needs to sign its own.
type resolvedCredentials struct {
	// Credentials are the values, used by the controller in process.
	Credentials s3.Credentials

	// SecretName is a Secret in the site's namespace holding the same values. The
	// sync Job reads it directly, so the operator never passes secrets through a
	// Job's environment in plain text.
	SecretName string
	// AccessKeyIDKey and the two below are the keys within SecretName.
	AccessKeyIDKey     string
	SecretAccessKeyKey string
	SessionTokenKey    string
	// HasSessionToken reports whether SessionTokenKey is actually present, so the
	// Job can mark the environment variable optional when it is not.
	HasSessionToken bool
}

// credentialsError distinguishes a misconfiguration the user must fix from a
// transient API failure, so the controller can pick a sensible retry delay.
type credentialsError struct {
	message string
}

func (e *credentialsError) Error() string { return e.message }

// resolveCredentials finds the S3 access key for a site.
//
// A site that names its own Secret uses it directly. Otherwise the operator's
// default credentials apply, and because a sync Job can only read Secrets in its
// own namespace, they are mirrored there as a Secret owned by the StaticSite.
// Mirroring copies a cluster-wide credential into a tenant namespace, so it can
// be turned off, in which case cross-namespace sites must name their own.
func (r *StaticSiteReconciler) resolveCredentials(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
) (*resolvedCredentials, error) {
	if reference := site.Spec.Bucket.Credentials; reference != nil {
		return r.credentialsFromSiteSecret(ctx, site, reference)
	}
	return r.credentialsFromDefaults(ctx, site)
}

func (r *StaticSiteReconciler) credentialsFromSiteSecret(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	reference *webv1alpha1.S3CredentialsSpec,
) (*resolvedCredentials, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: site.Namespace, Name: reference.SecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &credentialsError{
				message: fmt.Sprintf("Secret %q named by spec.bucket.credentials does not exist", key),
			}
		}
		return nil, fmt.Errorf("read Secret %q: %w", key, err)
	}

	accessKeyIDKey := orDefault(reference.AccessKeyIDKey, mirroredAccessKeyIDKey)
	secretAccessKeyKey := orDefault(reference.SecretAccessKeyKey, mirroredSecretAccessKeyKey)
	sessionTokenKey := orDefault(reference.SessionTokenKey, mirroredSessionTokenKey)

	credentials, err := credentialsFromSecret(secret, accessKeyIDKey, secretAccessKeyKey, sessionTokenKey)
	if err != nil {
		return nil, err
	}

	return &resolvedCredentials{
		Credentials:        credentials,
		SecretName:         secret.Name,
		AccessKeyIDKey:     accessKeyIDKey,
		SecretAccessKeyKey: secretAccessKeyKey,
		SessionTokenKey:    sessionTokenKey,
		HasSessionToken:    credentials.SessionToken != "",
	}, nil
}

func (r *StaticSiteReconciler) credentialsFromDefaults(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
) (*resolvedCredentials, error) {
	namespace, name := r.Defaults.CredentialsSecretRef()
	if name == "" {
		return nil, &credentialsError{
			message: "no credentials: this StaticSite sets no spec.bucket.credentials and the operator " +
				"has no --default-credentials-secret",
		}
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &credentialsError{
				message: fmt.Sprintf("the operator's default credentials Secret %q does not exist", key),
			}
		}
		return nil, fmt.Errorf("read Secret %q: %w", key, err)
	}

	credentials, err := credentialsFromSecret(secret,
		r.Defaults.AccessKeyIDKey, r.Defaults.SecretAccessKeyKey, r.Defaults.SessionTokenKey)
	if err != nil {
		return nil, err
	}

	// Already in the right namespace: the Job can read the original, and mirroring
	// would only create a second copy to keep in sync.
	if namespace == site.Namespace {
		return &resolvedCredentials{
			Credentials:        credentials,
			SecretName:         secret.Name,
			AccessKeyIDKey:     r.Defaults.AccessKeyIDKey,
			SecretAccessKeyKey: r.Defaults.SecretAccessKeyKey,
			SessionTokenKey:    r.Defaults.SessionTokenKey,
			HasSessionToken:    credentials.SessionToken != "",
		}, nil
	}

	if !r.Defaults.MirrorCredentials {
		return nil, &credentialsError{
			message: fmt.Sprintf(
				"the operator's default credentials live in namespace %q and mirroring is disabled; "+
					"set spec.bucket.credentials to a Secret in namespace %q",
				namespace, site.Namespace),
		}
	}

	mirrored, err := r.mirrorCredentials(ctx, site, credentials)
	if err != nil {
		return nil, err
	}
	return mirrored, nil
}

// mirrorCredentials keeps a copy of the default credentials in the site's
// namespace, owned by the StaticSite so it is garbage collected with it.
func (r *StaticSiteReconciler) mirrorCredentials(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	credentials s3.Credentials,
) (*resolvedCredentials, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mirroredSecretName(site),
			Namespace: site.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[webv1alpha1.ManagedByLabel] = webv1alpha1.ManagedByValue
		secret.Labels[webv1alpha1.SiteNameLabel] = site.Name

		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			mirroredAccessKeyIDKey:     []byte(credentials.AccessKeyID),
			mirroredSecretAccessKeyKey: []byte(credentials.SecretAccessKey),
		}
		if credentials.SessionToken != "" {
			secret.Data[mirroredSessionTokenKey] = []byte(credentials.SessionToken)
		}

		return controllerutil.SetControllerReference(site, secret, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("mirror the default credentials into namespace %q: %w", site.Namespace, err)
	}

	return &resolvedCredentials{
		Credentials:        credentials,
		SecretName:         secret.Name,
		AccessKeyIDKey:     mirroredAccessKeyIDKey,
		SecretAccessKeyKey: mirroredSecretAccessKeyKey,
		SessionTokenKey:    mirroredSessionTokenKey,
		HasSessionToken:    credentials.SessionToken != "",
	}, nil
}

func mirroredSecretName(site *webv1alpha1.StaticSite) string {
	return truncate(site.Name, 240) + "-s3-credentials"
}

func credentialsFromSecret(
	secret *corev1.Secret,
	accessKeyIDKey, secretAccessKeyKey, sessionTokenKey string,
) (s3.Credentials, error) {
	credentials := s3.Credentials{
		AccessKeyID:     string(secret.Data[accessKeyIDKey]),
		SecretAccessKey: string(secret.Data[secretAccessKeyKey]),
		SessionToken:    string(secret.Data[sessionTokenKey]),
	}

	var missing []string
	if credentials.AccessKeyID == "" {
		missing = append(missing, accessKeyIDKey)
	}
	if credentials.SecretAccessKey == "" {
		missing = append(missing, secretAccessKeyKey)
	}
	if len(missing) > 0 {
		return s3.Credentials{}, &credentialsError{
			message: fmt.Sprintf("Secret %q is missing key(s) %v",
				client.ObjectKeyFromObject(secret), missing),
		}
	}
	return credentials, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
