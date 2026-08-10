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
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/config"
	"github.com/rashkash103/static-s3-operator/internal/registry"
)

// Where the sync container finds its inputs and scratch space.
const (
	workspaceMountPath = "/workspace"
	tmpMountPath       = "/tmp"
	registryAuthPath   = "/etc/static-s3-operator/registry-auth"

	workspaceVolumeName    = "workspace"
	tmpVolumeName          = "tmp"
	registryAuthVolumeName = "registry-auth"

	// syncerUserID is the distroless nonroot user the operator image runs as.
	syncerUserID = 65532
)

// buildSyncJob renders the Job that performs one sync.
//
// The Pod is deliberately unprivileged and API-blind: it drops all capabilities,
// runs as a non-root user with a read-only root filesystem, and gets no
// ServiceAccount token, because this is the component that unpacks an image the
// operator did not build. Its only inputs are the flags below, the S3 credentials
// from a Secret, and any registry credentials the site named.
func (r *StaticSiteReconciler) buildSyncJob(
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	reference *registry.Reference,
	credentials *resolvedCredentials,
	syncHash string,
) *batchv1.Job {
	jobSpec := site.Spec.Sync.Job

	labels := map[string]string{
		webv1alpha1.ManagedByLabel: webv1alpha1.ManagedByValue,
		webv1alpha1.SiteNameLabel:  site.Name,
		webv1alpha1.SiteUIDLabel:   string(site.UID),
		webv1alpha1.DigestLabel:    reference.ShortDigest(),
		webv1alpha1.SyncHashLabel:  shortHash(syncHash),
	}

	podLabels := map[string]string{}
	for key, value := range labels {
		podLabels[key] = value
	}
	podAnnotations := map[string]string{}
	if jobSpec.PodMetadata != nil {
		for key, value := range jobSpec.PodMetadata.Labels {
			podLabels[key] = value
		}
		for key, value := range jobSpec.PodMetadata.Annotations {
			podAnnotations[key] = value
		}
	}

	container := corev1.Container{
		Name:            "syncer",
		Image:           effective.SyncerImage,
		ImagePullPolicy: effective.SyncerImagePullPolicy,
		Command:         []string{"/syncer"},
		Args:            syncerArgs(site, effective, reference),
		Env:             syncerEnv(credentials),
		Resources:       jobSpec.Resources,
		VolumeMounts:    syncerVolumeMounts(effective),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(int64(syncerUserID)),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		// The syncer writes a JSON report here, which is how the operator learns
		// what a Job did without needing to read Pod logs. Falling back to the logs
		// covers a Pod that died before writing one.
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: jobNamePrefix(site),
			Namespace:    site.Namespace,
			Labels:       labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            jobSpec.BackoffLimit,
			ActiveDeadlineSeconds:   jobSpec.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: jobSpec.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					// Never, rather than OnFailure, so each attempt gets a fresh Pod whose
					// logs and termination message belong to that attempt alone.
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers:         []corev1.Container{container},
					Volumes:            syncerVolumes(site, effective),
					ServiceAccountName: jobSpec.ServiceAccountName,
					// The syncer never talks to the API server, so it gets no token.
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             jobSpec.ImagePullSecrets,
					NodeSelector:                 jobSpec.NodeSelector,
					Tolerations:                  jobSpec.Tolerations,
					Affinity:                     jobSpec.Affinity,
					PriorityClassName:            jobSpec.PriorityClassName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(syncerUserID)),
						// An fsGroup makes the mounted volumes writable by the non-root user
						// without any of them needing world-writable permissions.
						FSGroup:        ptr.To(int64(syncerUserID)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				},
			},
		},
	}
}

func syncerArgs(
	site *webv1alpha1.StaticSite,
	effective *config.Effective,
	reference *registry.Reference,
) []string {
	args := []string{
		"--image=" + reference.Pinned,
		"--source-path=" + effective.SourcePath,
		"--bucket=" + effective.Bucket,
		"--region=" + effective.Region,
		"--force-path-style=" + strconv.FormatBool(effective.ForcePathStyle),
		"--delete-removed=" + strconv.FormatBool(effective.DeleteRemoved),
		"--default-content-type=" + effective.DefaultContentType,
		"--concurrency=" + strconv.Itoa(effective.Concurrency),
		"--workspace=" + workspaceMountPath,
		"--max-extract-bytes=" + strconv.FormatInt(effective.MaxExtractBytes, 10),
		"--max-extract-files=" + strconv.Itoa(effective.MaxExtractFiles),
	}

	if effective.Endpoint != "" {
		args = append(args, "--endpoint="+effective.Endpoint)
	}
	if effective.KeyPrefix != "" {
		args = append(args, "--key-prefix="+effective.KeyPrefix)
	}
	// An empty --acl is meaningful: it tells the syncer to send no ACL at all.
	args = append(args, "--acl="+effective.ACL)
	if effective.DefaultCacheControl != "" {
		args = append(args, "--default-cache-control="+effective.DefaultCacheControl)
	}
	for _, exclude := range effective.Excludes {
		args = append(args, "--exclude="+exclude)
	}
	for _, rule := range effective.CacheControl {
		args = append(args, "--cache-control="+rule.Pattern+"="+rule.Value)
	}
	for _, rule := range effective.ContentTypes {
		args = append(args, "--content-type="+rule.Pattern+"="+rule.Value)
	}
	if effective.PlainHTTP {
		args = append(args, "--plain-http=true")
	}
	if effective.InsecureSkipTLSVerify {
		args = append(args, "--registry-skip-tls-verify=true")
	}
	if len(effective.ImagePullSecrets) > 0 {
		args = append(args, "--registry-auth-dir="+registryAuthPath)
	} else {
		// Point the syncer at nothing rather than at an unmounted path, so an
		// anonymous pull is explicit instead of looking like a missing mount.
		args = append(args, "--registry-auth-dir=")
	}
	if site.Spec.Suspend {
		// Defensive: the controller does not create Jobs for a suspended site, and a
		// Job that somehow exists should not quietly do work.
		args = append(args, "--concurrency=1")
	}

	return args
}

func syncerEnv(credentials *resolvedCredentials) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name: "AWS_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credentials.SecretName},
					Key:                  credentials.AccessKeyIDKey,
				},
			},
		},
		{
			Name: "AWS_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credentials.SecretName},
					Key:                  credentials.SecretAccessKeyKey,
				},
			},
		},
	}

	// Temporary credentials need a session token; long-lived access keys have
	// none, and an optional reference keeps the Pod from failing to start when the
	// key is simply absent.
	env = append(env, corev1.EnvVar{
		Name: "AWS_SESSION_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: credentials.SecretName},
				Key:                  credentials.SessionTokenKey,
				Optional:             ptr.To(true),
			},
		},
	})

	return env
}

func syncerVolumeMounts(effective *config.Effective) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: workspaceVolumeName, MountPath: workspaceMountPath},
		// The root filesystem is read only, and the AWS SDK and Go runtime both
		// expect a writable temporary directory.
		{Name: tmpVolumeName, MountPath: tmpMountPath},
	}
	if len(effective.ImagePullSecrets) > 0 {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      registryAuthVolumeName,
			MountPath: registryAuthPath,
			ReadOnly:  true,
		})
	}
	return mounts
}

func syncerVolumes(site *webv1alpha1.StaticSite, effective *config.Effective) []corev1.Volume {
	workspace := corev1.Volume{Name: workspaceVolumeName}
	if custom := site.Spec.Sync.Job.WorkspaceVolume; custom != nil {
		workspace.VolumeSource = *custom
	} else {
		workspace.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}

	volumes := []corev1.Volume{
		workspace,
		{
			Name: tmpVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(64<<20, resource.BinarySI)},
			},
		},
	}

	if len(effective.ImagePullSecrets) > 0 {
		sources := make([]corev1.VolumeProjection, 0, len(effective.ImagePullSecrets))
		for _, name := range effective.ImagePullSecrets {
			// Each Secret becomes one file named after it, which is what lets the
			// syncer merge several registries' credentials into one keychain.
			sources = append(sources, corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Items: []corev1.KeyToPath{
						{Key: corev1.DockerConfigJsonKey, Path: name + ".json"},
					},
				},
			})
		}
		volumes = append(volumes, corev1.Volume{
			Name: registryAuthVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources:     sources,
					DefaultMode: ptr.To(int32(0o400)),
				},
			},
		})
	}

	return volumes
}

// jobNamePrefix keeps generated Job names inside the 63 character limit, leaving
// room for the suffix the API server appends.
func jobNamePrefix(site *webv1alpha1.StaticSite) string {
	const maxPrefix = 63 - len("-sync-") - 5
	return truncate(strings.TrimSuffix(site.Name, "-"), maxPrefix) + "-sync-"
}

// shortHash trims a hex fingerprint to something that fits a label value.
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// jobOutcome is the terminal state of a sync Job.
type jobOutcome string

const (
	jobRunning   jobOutcome = "Running"
	jobSucceeded jobOutcome = "Succeeded"
	jobFailed    jobOutcome = "Failed"
)

// classifyJob reports whether a Job has finished, and why it failed.
func classifyJob(job *batchv1.Job) (jobOutcome, string) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return jobSucceeded, ""
		case batchv1.JobFailed:
			message := condition.Message
			if message == "" {
				message = string(condition.Reason)
			}
			return jobFailed, message
		}
	}

	// Older API servers report success only through the counter.
	if job.Spec.Completions == nil && job.Status.Succeeded > 0 {
		return jobSucceeded, ""
	}
	return jobRunning, ""
}

// describeJobFailure builds a message for a failed Job, preferring the syncer's
// own report over the Job controller's generic wording.
func describeJobFailure(job *batchv1.Job, reportMessage, conditionMessage string) string {
	switch {
	case reportMessage != "":
		return reportMessage
	case conditionMessage != "":
		return fmt.Sprintf("sync Job %s failed: %s", job.Name, conditionMessage)
	default:
		return fmt.Sprintf("sync Job %s failed", job.Name)
	}
}
