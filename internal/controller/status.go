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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

// maxConditionMessage keeps a condition message inside what the API server will
// accept, since an S3 or registry error can be very long.
const maxConditionMessage = 1024

// setCondition records a condition, stamping it with the generation it was
// computed from so that a stale condition is recognisable.
func setCondition(
	site *webv1alpha1.StaticSite,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&site.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message, maxConditionMessage),
		ObservedGeneration: site.Generation,
	})
}

// conditionStatus returns a condition's status, or Unknown when it is absent.
func conditionStatus(site *webv1alpha1.StaticSite, conditionType string) metav1.ConditionStatus {
	condition := meta.FindStatusCondition(site.Status.Conditions, conditionType)
	if condition == nil {
		return metav1.ConditionUnknown
	}
	return condition.Status
}

// derivePhase summarises the conditions for kubectl output. Phase is never the
// source of truth, so it is recomputed from scratch on every pass rather than
// being tracked as its own state machine.
func derivePhase(site *webv1alpha1.StaticSite) webv1alpha1.StaticSitePhase {
	if !site.DeletionTimestamp.IsZero() {
		return webv1alpha1.PhaseDeleting
	}
	if site.Spec.Suspend {
		return webv1alpha1.PhaseSuspended
	}

	if conditionStatus(site, webv1alpha1.ConditionReady) == metav1.ConditionTrue {
		return webv1alpha1.PhaseReady
	}
	if site.Status.ActiveSyncJob != "" {
		return webv1alpha1.PhaseSyncing
	}

	// A False bucket or source condition is a real failure; Unknown just means the
	// operator has not got there yet.
	for _, conditionType := range []string{
		webv1alpha1.ConditionBucketReady,
		webv1alpha1.ConditionSourceResolved,
		webv1alpha1.ConditionSynced,
	} {
		if conditionStatus(site, conditionType) == metav1.ConditionFalse {
			return webv1alpha1.PhaseFailed
		}
	}

	if conditionStatus(site, webv1alpha1.ConditionBucketReady) != metav1.ConditionTrue {
		return webv1alpha1.PhaseProvisioning
	}
	return webv1alpha1.PhasePending
}

// patchStatus writes the status subresource, and does nothing when nothing
// changed. Skipping the no-op write matters: an unconditional patch on every
// reconcile produces an update event, which reconciles again, which patches
// again.
func (r *StaticSiteReconciler) patchStatus(
	ctx context.Context,
	site *webv1alpha1.StaticSite,
	base *webv1alpha1.StaticSite,
) error {
	if equality.Semantic.DeepEqual(base.Status, site.Status) {
		return nil
	}
	return r.Status().Patch(ctx, site, client.MergeFrom(base))
}

// recordAttempt prepends an attempt to the history and trims it to the retention
// limit.
func recordAttempt(site *webv1alpha1.StaticSite, attempt webv1alpha1.SyncAttempt, retain int) {
	if retain <= 0 {
		site.Status.SyncHistory = nil
		return
	}

	history := append([]webv1alpha1.SyncAttempt{attempt}, site.Status.SyncHistory...)
	if len(history) > retain {
		history = history[:retain]
	}
	site.Status.SyncHistory = history
}

// backoff returns the delay before retrying after failures consecutive failures.
func backoff(failures int32, base, max time.Duration) time.Duration {
	if failures <= 1 {
		return base
	}

	delay := base
	for range int(failures) - 1 {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	return delay
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}
