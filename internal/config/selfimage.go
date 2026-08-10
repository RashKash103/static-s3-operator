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
