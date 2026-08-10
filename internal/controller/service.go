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
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/config"
)

// reconcileService maintains the ExternalName Service that points at the
// bucket's website hostname.
//
// The Service is what makes a site addressable by name: external-dns watches it
// and publishes CNAME records from the annotations, so a site gets DNS without
// anybody touching a zone file. It is a pure alias, with no selector and no
// endpoints of its own.
func (r *StaticSiteReconciler) reconcileService(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
) error {
	log := logf.FromContext(ctx)

	// A Service whose name changed leaves the old one behind, so clean up before
	// creating the new one.
	if site.Status.ServiceName != "" && site.Status.ServiceName != effective.ServiceName {
		if err := r.deleteService(ctx, site.Namespace, site.Status.ServiceName); err != nil {
			return err
		}
		site.Status.ServiceName = ""
	}

	if !effective.ServiceEnabled {
		if err := r.deleteService(ctx, site.Namespace, effective.ServiceName); err != nil {
			return err
		}
		site.Status.ServiceName = ""
		return nil
	}

	if effective.ServiceExternalName == "" {
		// Without a website hostname there is nothing to alias. This happens when a
		// provider's website URL could not be derived, which
		// spec.bucket.websiteEndpointTemplate exists to fix.
		log.V(1).Info("Skipped the ExternalName Service because no website hostname is known",
			"bucket", effective.Bucket)
		if err := r.deleteService(ctx, site.Namespace, effective.ServiceName); err != nil {
			return err
		}
		site.Status.ServiceName = ""
		return nil
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      effective.ServiceName,
			Namespace: site.Namespace,
		},
	}

	operation, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.Labels == nil {
			service.Labels = map[string]string{}
		}
		service.Labels[webv1alpha1.ManagedByLabel] = webv1alpha1.ManagedByValue
		service.Labels[webv1alpha1.SiteNameLabel] = site.Name
		maps.Copy(service.Labels, effective.ServiceLabels)

		// Annotations are merged rather than replaced, so a hostname written by
		// something else on the same Service is not clobbered.
		if service.Annotations == nil {
			service.Annotations = map[string]string{}
		}
		maps.Copy(service.Annotations, effective.ServiceAnnotations)

		service.Spec.Type = corev1.ServiceTypeExternalName
		service.Spec.ExternalName = effective.ServiceExternalName
		// An ExternalName Service resolves by DNS alone; a selector would make the
		// API server try to manage endpoints for it.
		service.Spec.Selector = nil
		service.Spec.Ports = nil

		return controllerutil.SetControllerReference(site, service, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile Service %q: %w", effective.ServiceName, err)
	}
	if operation != controllerutil.OperationResultNone {
		log.Info("Reconciled ExternalName Service",
			"service", service.Name, "externalName", service.Spec.ExternalName, "operation", operation)
	}

	site.Status.ServiceName = service.Name
	return nil
}

// deleteService removes a Service the operator owns. It ignores a Service that
// is already gone, and refuses to touch one it does not own so that an
// unfortunate spec.service.name cannot delete an unrelated Service.
func (r *StaticSiteReconciler) deleteService(ctx context.Context, namespace, name string) error {
	if name == "" {
		return nil
	}

	service := &corev1.Service{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, service); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read Service %q: %w", key, err)
	}

	if service.Labels[webv1alpha1.ManagedByLabel] != webv1alpha1.ManagedByValue {
		logf.FromContext(ctx).Info("Left a Service alone because this operator does not manage it",
			"service", key.String())
		return nil
	}

	if err := r.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Service %q: %w", key, err)
	}
	return nil
}
