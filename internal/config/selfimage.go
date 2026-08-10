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
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagerContainerName is the container the operator runs in, and the one whose
// image is reused for sync Jobs.
const ManagerContainerName = "manager"

// clusterIdentityNamespace is the namespace whose UID identifies the cluster.
//
// kube-system exists in every cluster, is created once at bootstrap, and keeps its
// UID for the cluster's life -- which makes its UID a stable per-cluster value
// without anything having to be generated or stored.
const clusterIdentityNamespace = "kube-system"

// ResolveSyncerImage decides which image sync Jobs should run.
//
// The syncer ships in the same image as the manager, so the two must stay in
// lockstep: a manager rolled forward while its Jobs still run last release's
// syncer is a genuinely confusing failure. Rather than asking the chart to repeat
// the image in a flag and hoping it matches, the operator reads its own Pod and
// uses whatever image it is itself running.
//
// The precedence is: an explicit --syncer-image, then the SYNCER_IMAGE
// environment variable, then self-discovery.
func (d *Defaults) ResolveSyncerImage(ctx context.Context, reader client.Reader) error {
	if d.SyncerImage != "" {
		return nil
	}
	if fromEnvironment := os.Getenv("SYNCER_IMAGE"); fromEnvironment != "" {
		d.SyncerImage = fromEnvironment
		return nil
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" || d.Namespace == "" {
		return fmt.Errorf(
			"cannot determine the syncer image: pass --syncer-image, or set POD_NAME and POD_NAMESPACE " +
				"so the operator can read its own image from its Pod")
	}

	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: d.Namespace, Name: podName}
	if err := reader.Get(ctx, key, pod); err != nil {
		return fmt.Errorf("read own Pod %q to determine the syncer image: %w", key, err)
	}

	for _, container := range pod.Spec.Containers {
		if container.Name == ManagerContainerName {
			d.SyncerImage = container.Image
			return nil
		}
	}
	return fmt.Errorf("pod %q has no container named %q, so the syncer image is unknown",
		key, ManagerContainerName)
}

// ResolveBucketNameUUIDNamespace fills in the UUID namespace that opaque bucket
// names are derived under.
//
// It defaults to the UID of the kube-system namespace. That gives a value which is
// stable for the cluster's lifetime, unique to it, and not published anywhere --
// which is all that is needed, because the thing being kept private is which
// Kubernetes namespaces and resource names exist, and anyone who can read this UID
// can already list those directly.
//
// The value has to be knowable outside the cluster so a bucket name can be
// computed before the bucket exists:
//
//	kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'
//
// Set --bucket-name-uuid-namespace explicitly to keep names stable across
// clusters, which matters if sites are ever rebuilt into a new cluster and have to
// find their existing buckets.
func (d *Defaults) ResolveBucketNameUUIDNamespace(ctx context.Context, reader client.Reader) error {
	if d.BucketNameUUIDNamespace != "" {
		return nil
	}
	if !templateUsesOpaqueID(d.BucketNameTemplate) {
		// A cluster whose template names sites some other way needs no UUID namespace,
		// and should not need the cluster-scoped read to start.
		return nil
	}

	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: clusterIdentityNamespace}, namespace); err != nil {
		return fmt.Errorf(
			"read the %s namespace to derive the bucket name UUID namespace: %w; "+
				"grant the operator get on namespaces, or set --bucket-name-uuid-namespace",
			clusterIdentityNamespace, err)
	}
	if namespace.UID == "" {
		return fmt.Errorf("the %s namespace has no UID", clusterIdentityNamespace)
	}

	d.BucketNameUUIDNamespace = string(namespace.UID)
	return nil
}
