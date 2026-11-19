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

	"github.com/google/go-containerregistry/pkg/name"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

// FluxImageRepositoryGroupKind is the Flux object a registry webhook is normally
// delivered to.
var FluxImageRepositoryGroupKind = schema.GroupKind{Group: "image.toolkit.fluxcd.io", Kind: "ImageRepository"}

// FluxRelayReconciler carries Flux reconcile requests from ImageRepository objects
// to the StaticSites that publish the same image repository.
//
// It exists because a Flux Receiver cannot name a StaticSite: the Receiver CRD
// restricts spec.resources[].kind to an enum of Flux's own kinds. The idiomatic
// Flux setup for "react when an image is pushed" is already a Receiver pointed at
// an ImageRepository, so this meets users where that wiring is, rather than
// asking them to run a webhook endpoint of ours.
//
// Matching is by repository, within the ImageRepository's own namespace. Staying
// inside the namespace keeps one tenant's annotations from steering another
// tenant's sites; the worst a request can do is make a site check the registry
// once, but there is no reason to allow even that across a namespace boundary.
//
// The relay writes the request onto the StaticSite rather than enqueueing it
// directly, so there is exactly one mechanism on the StaticSite side, and it is
// visible: the annotation and status.lastHandledReconcileAt say what happened.
// Flux itself annotates objects it does not own in exactly this way.
//
// It never imports Flux's API types. They would pin a Flux version into this
// module for the sake of one string field, and the API version actually served
// is discovered at startup instead.
type FluxRelayReconciler struct {
	client.Client

	// ImageRepository is the served version of Flux's ImageRepository.
	ImageRepository schema.GroupVersionKind
}

// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagerepositories,verbs=get;list;watch

// Reconcile relays the ImageRepository's reconcile request, if it has one, to
// every matching StaticSite that has not already seen it.
func (r *FluxRelayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	imageRepository := &unstructured.Unstructured{}
	imageRepository.SetGroupVersionKind(r.ImageRepository)
	if err := r.Get(ctx, req.NamespacedName, imageRepository); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	request := imageRepository.GetAnnotations()[webv1alpha1.ReconcileRequestAnnotation]
	if request == "" {
		return ctrl.Result{}, nil
	}

	image, _, err := unstructured.NestedString(imageRepository.Object, "spec", "image")
	if err != nil || image == "" {
		log.V(1).Info("Skipped an ImageRepository without spec.image")
		return ctrl.Result{}, nil
	}
	repository, err := name.NewRepository(image)
	if err != nil {
		// Flux reports this on the ImageRepository itself; repeating it here would
		// only be noise in the operator's log.
		log.V(1).Info("Skipped an ImageRepository with an unparseable spec.image", "image", image)
		return ctrl.Result{}, nil
	}

	sites := &webv1alpha1.StaticSiteList{}
	if err := r.List(ctx, sites, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list StaticSites in namespace %q: %w", req.Namespace, err)
	}

	var errs []error
	for i := range sites.Items {
		site := &sites.Items[i]
		if !publishesRepository(site, repository) || reconcileRequest(site) == request {
			continue
		}

		patch := client.MergeFrom(site.DeepCopy())
		if site.Annotations == nil {
			site.Annotations = map[string]string{}
		}
		site.Annotations[webv1alpha1.ReconcileRequestAnnotation] = request
		if err := r.Patch(ctx, site, patch); err != nil {
			errs = append(errs, fmt.Errorf("annotate StaticSite %q: %w", client.ObjectKeyFromObject(site), err))
			continue
		}
		log.Info("Relayed a Flux reconcile request to StaticSite",
			"staticSite", site.Name, "repository", repository.Name(), "requestedAt", request)
	}
	return ctrl.Result{}, errors.Join(errs...)
}

// publishesRepository reports whether a site follows a tag in the given
// repository. A digest-pinned site is excluded: nothing a webhook says can change
// what it resolves to.
func publishesRepository(site *webv1alpha1.StaticSite, repository name.Repository) bool {
	image := site.Spec.Source.Image
	if isDigestReference(image) {
		return false
	}
	reference, err := name.ParseReference(image)
	if err != nil {
		return false
	}
	// Both sides are normalised, so "acme/site" in one and
	// "index.docker.io/acme/site" in the other still match.
	return reference.Context().Name() == repository.Name()
}

// SetupWithManager sets up the relay with the Manager.
func (r *FluxRelayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	watched := &unstructured.Unstructured{}
	watched.SetGroupVersionKind(r.ImageRepository)

	return ctrl.NewControllerManagedBy(mgr).
		For(watched, builder.WithPredicates(reconcileRequestChanged())).
		Named("flux-imagerepository-relay").
		Complete(r)
}

// reconcileRequestChanged admits an ImageRepository only when its reconcile
// request is new.
//
// Every other change is filtered out, and most of them come from Flux's own image
// reflector writing status after each scan. Create events are kept, because they
// are how a request that arrived while the operator was down is still delivered
// after it starts; re-delivering one already relayed is a no-op, since Reconcile
// compares values before writing.
func reconcileRequestChanged() predicate.Funcs {
	value := func(object client.Object) string {
		return object.GetAnnotations()[webv1alpha1.ReconcileRequestAnnotation]
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return value(e.Object) != "" },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return value(e.ObjectNew) != "" && value(e.ObjectNew) != value(e.ObjectOld)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// DiscoverFluxImageRepository returns the served version of Flux's
// ImageRepository, or false when the API is not installed.
//
// The relay is optional by nature: a cluster without Flux's image automation has
// nothing to relay, and watching a kind the API server does not know would fail
// the manager's cache sync. Discovery happens once, at startup, so installing
// Flux later takes an operator restart to pick up.
func DiscoverFluxImageRepository(mapper meta.RESTMapper) (schema.GroupVersionKind, bool, error) {
	mapping, err := mapper.RESTMapping(FluxImageRepositoryGroupKind)
	if meta.IsNoMatchError(err) {
		return schema.GroupVersionKind{}, false, nil
	}
	if err != nil {
		return schema.GroupVersionKind{}, false, fmt.Errorf("discover %s: %w", FluxImageRepositoryGroupKind, err)
	}
	return mapping.GroupVersionKind, true, nil
}
