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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

const (
	testImage   = "registry.example.com/acme/site:latest"
	testDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

var _ = Describe("StaticSite Controller", func() {
	var (
		namespace string
		siteName  string
		bucket    string
		siteKey   types.NamespacedName
	)

	BeforeEach(func() {
		resetTestState()
		fakeRegistry.setDigest(testImage, testDigest)

		// A namespace per spec keeps Jobs, Services and mirrored Secrets from one
		// test out of the next one's listings.
		namespace = fmt.Sprintf("site-%d", time.Now().UnixNano())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())

		siteName = "docs"
		bucket = "acme-docs-" + strings.TrimPrefix(namespace, "site-")
		siteKey = types.NamespacedName{Name: siteName, Namespace: namespace}
	})

	// newSite builds a StaticSite for this spec's namespace and bucket.
	newSite := func(mutate ...func(*webv1alpha1.StaticSite)) *webv1alpha1.StaticSite {
		site := &webv1alpha1.StaticSite{
			ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: namespace},
			Spec: webv1alpha1.StaticSiteSpec{
				Source: webv1alpha1.SourceSpec{Image: testImage},
				Bucket: webv1alpha1.BucketSpec{
					Name:    bucket,
					Website: &webv1alpha1.WebsiteSpec{},
				},
			},
		}
		for _, apply := range mutate {
			apply(site)
		}
		return site
	}

	create := func(site *webv1alpha1.StaticSite) {
		Expect(k8sClient.Create(ctx, site)).To(Succeed())
	}

	fetch := func() *webv1alpha1.StaticSite {
		site := &webv1alpha1.StaticSite{}
		Expect(k8sClient.Get(ctx, siteKey, site)).To(Succeed())
		return site
	}

	listJobs := func() []batchv1.Job {
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(namespace))).To(Succeed())
		return jobs.Items
	}

	// waitForJob waits until a sync Job exists whose recorded digest matches, which
	// is how a spec asserts that the operator started the sync it should have.
	waitForJob := func(digest string) *batchv1.Job {
		var found *batchv1.Job
		Eventually(func(g Gomega) {
			for _, job := range listJobs() {
				if job.Annotations[digestAnnotation] == digest {
					found = job.DeepCopy()
					return
				}
			}
			g.Expect(found).NotTo(BeNil(), "no sync Job for digest %s yet", digest)
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		return found
	}

	// updateJobStatus applies a change to a Job's status, retrying on conflict.
	updateJobStatus := func(job *batchv1.Job, mutate func(*batchv1.Job)) {
		Eventually(func(g Gomega) {
			current := &batchv1.Job{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), current)).To(Succeed())
			mutate(current)
			g.Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	}

	// finishJob drives a Job to a terminal state, standing in for the Job
	// controller that envtest does not run.
	//
	// The API server enforces the same two-step transition the real Job controller
	// makes: a Job reaches SuccessCriteriaMet or FailureTarget first, and only then
	// Complete or Failed. Setting the terminal condition on its own is rejected.
	finishJob := func(job *batchv1.Job, conditionType batchv1.JobConditionType, message string) {
		interim := batchv1.JobSuccessCriteriaMet
		if conditionType == batchv1.JobFailed {
			interim = batchv1.JobFailureTarget
		}

		updateJobStatus(job, func(current *batchv1.Job) {
			now := metav1.Now()
			if current.Status.StartTime == nil {
				current.Status.StartTime = &now
			}
			if conditionType == batchv1.JobComplete {
				current.Status.Succeeded = 1
			} else {
				current.Status.Failed = 1
			}
			current.Status.Conditions = append(current.Status.Conditions, batchv1.JobCondition{
				Type:    interim,
				Status:  corev1.ConditionTrue,
				Reason:  string(interim),
				Message: message,
			})
		})

		updateJobStatus(job, func(current *batchv1.Job) {
			// completionTime is only valid on a Job that completed; a failed Job leaves
			// it unset, which is why the operator falls back to its own clock when
			// recording the attempt.
			if conditionType == batchv1.JobComplete {
				now := metav1.Now()
				current.Status.CompletionTime = &now
			}
			current.Status.Conditions = append(current.Status.Conditions, batchv1.JobCondition{
				Type:    conditionType,
				Status:  corev1.ConditionTrue,
				Reason:  string(conditionType),
				Message: message,
			})
		})
	}

	expectCondition := func(conditionType string, status metav1.ConditionStatus, reason string) {
		Eventually(func(g Gomega) {
			site := fetch()
			condition := findCondition(site, conditionType)
			g.Expect(condition).NotTo(BeNil(), "condition %s is absent", conditionType)
			g.Expect(condition.Status).To(Equal(status), "condition %s: %s", conditionType, condition.Message)
			if reason != "" {
				g.Expect(condition.Reason).To(Equal(reason), "condition %s: %s", conditionType, condition.Message)
			}
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	}

	// syncToReady drives a freshly created site all the way to Ready.
	syncToReady := func(digest string) *batchv1.Job {
		job := waitForJob(digest)
		finishJob(job, batchv1.JobComplete, "")
		expectCondition(webv1alpha1.ConditionReady, metav1.ConditionTrue, webv1alpha1.ReasonReady)
		return job
	}

	deleteSite := func() {
		site := &webv1alpha1.StaticSite{}
		if err := k8sClient.Get(ctx, siteKey, site); err != nil {
			return
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, site))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, siteKey, &webv1alpha1.StaticSite{}))
		}, eventuallyTimeout, eventuallyPoll).Should(BeTrue(), "the StaticSite should be fully removed")
	}

	AfterEach(func() {
		deleteSite()
	})

	Context("when a StaticSite is created", func() {
		It("provisions the bucket, starts a sync, and becomes Ready", func() {
			create(newSite())

			By("adding the finalizer before anything external is created")
			Eventually(func() []string {
				return fetch().Finalizers
			}, eventuallyTimeout, eventuallyPoll).Should(ContainElement(webv1alpha1.Finalizer))

			By("creating the bucket")
			Eventually(func() bool {
				return fakeS3.exists(bucket)
			}, eventuallyTimeout, eventuallyPoll).Should(BeTrue())
			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionTrue, webv1alpha1.ReasonBucketReady)

			By("recording an ownership token, which is what permits deletion later")
			Eventually(func(g Gomega) {
				ownership := fetch().Status.BucketOwnership
				g.Expect(ownership).NotTo(BeNil())
				g.Expect(ownership.Created).To(BeTrue())
				g.Expect(ownership.Token).NotTo(BeEmpty())
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			By("resolving the image to a digest")
			expectCondition(webv1alpha1.ConditionSourceResolved, metav1.ConditionTrue, webv1alpha1.ReasonResolved)
			Eventually(func() string {
				return fetch().Status.ResolvedDigest
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(testDigest))

			By("reporting Syncing while the Job runs")
			job := waitForJob(testDigest)
			expectCondition(webv1alpha1.ConditionSynced, metav1.ConditionFalse, webv1alpha1.ReasonSyncing)
			Eventually(func() string {
				return fetch().Status.ActiveSyncJob
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(job.Name))

			By("becoming Ready once the Job completes")
			finishJob(job, batchv1.JobComplete, "")
			// The reason is deliberately not pinned: it reads SyncSucceeded on the pass
			// that observes the Job and UpToDate on every pass after that, and both
			// mean the same thing.
			expectCondition(webv1alpha1.ConditionSynced, metav1.ConditionTrue, "")
			expectCondition(webv1alpha1.ConditionReady, metav1.ConditionTrue, webv1alpha1.ReasonReady)

			Eventually(func(g Gomega) {
				site := fetch()
				g.Expect(site.Status.SyncedDigest).To(Equal(testDigest))
				g.Expect(site.Status.Phase).To(Equal(webv1alpha1.PhaseReady))
				g.Expect(site.Status.ActiveSyncJob).To(BeEmpty())
				g.Expect(site.Status.SyncHistory).To(HaveLen(1))
				g.Expect(site.Status.SyncHistory[0].Succeeded).To(BeTrue())
				g.Expect(site.Status.ObservedGeneration).To(Equal(site.Generation))
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		})

		It("derives an opaque bucket name when none is given", func() {
			// The name goes into public DNS, so it must not spell out the namespace or
			// the resource name -- while still being computable in advance, so a bucket
			// can be created by hand or a policy scoped to it.
			site := newSite()
			site.Spec.Bucket.Name = ""
			create(site)

			opaque, err := defaults.OpaqueSiteID(namespace, siteName)
			Expect(err).NotTo(HaveOccurred())
			expected := "static-s3-" + opaque.String()

			Eventually(func() string {
				return fetch().Status.BucketName
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(expected))

			By("not leaking the namespace or the name")
			Expect(expected).NotTo(ContainSubstring(namespace))
			Expect(expected).NotTo(ContainSubstring(siteName))

			Eventually(func() bool {
				return fakeS3.exists(expected)
			}, eventuallyTimeout, eventuallyPoll).Should(BeTrue(),
				"the operator should create the bucket it derived")

			By("keeping that bucket even if the cluster template changes later")
			defaults.BucketNameTemplate = "renamed-{{.ShortID}}"
			_, err = kubectlFreeAnnotate(fetch())
			Expect(err).NotTo(HaveOccurred())

			Consistently(func() string {
				return fetch().Status.BucketName
			}, 3*time.Second, 500*time.Millisecond).Should(Equal(expected))
		})

		It("builds a sync Job that is unprivileged and has no API access", func() {
			create(newSite())
			job := waitForJob(testDigest)

			pod := job.Spec.Template.Spec
			Expect(pod.Containers).To(HaveLen(1))
			container := pod.Containers[0]

			Expect(container.Command).To(Equal([]string{"/syncer"}))
			Expect(container.Image).To(Equal(defaults.SyncerImage))
			Expect(pod.RestartPolicy).To(Equal(corev1.RestartPolicyNever))

			// The syncer unpacks an image the operator did not build, so it must not
			// be able to reach the API server even if it wanted to.
			Expect(pod.AutomountServiceAccountToken).To(Equal(ptr.To(false)))

			Expect(container.SecurityContext.RunAsNonRoot).To(Equal(ptr.To(true)))
			Expect(container.SecurityContext.ReadOnlyRootFilesystem).To(Equal(ptr.To(true)))
			Expect(container.SecurityContext.AllowPrivilegeEscalation).To(Equal(ptr.To(false)))
			Expect(container.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

			By("passing the digest-pinned reference rather than the tag")
			Expect(container.Args).To(ContainElement(
				"--image=registry.example.com/acme/site@" + testDigest))
			Expect(container.Args).To(ContainElement("--bucket=" + bucket))
			Expect(container.Args).To(ContainElement("--source-path=/usr/share/nginx/html"))
			Expect(container.Args).To(ContainElement("--delete-removed=true"))

			By("taking credentials from a Secret rather than a literal value")
			var accessKey *corev1.EnvVar
			for i := range container.Env {
				if container.Env[i].Name == "AWS_ACCESS_KEY_ID" {
					accessKey = &container.Env[i]
				}
			}
			Expect(accessKey).NotTo(BeNil())
			Expect(accessKey.Value).To(BeEmpty())
			Expect(accessKey.ValueFrom.SecretKeyRef).NotTo(BeNil())

			By("being owned by the StaticSite so it is garbage collected with it")
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Kind).To(Equal("StaticSite"))
			Expect(job.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		})

		It("mirrors the operator's credentials into the site's namespace", func() {
			// A sync Job can only read Secrets in its own namespace, so the operator's
			// cluster-wide credentials have to be copied there.
			create(newSite())
			waitForJob(testDigest)

			mirrored := &corev1.Secret{}
			key := types.NamespacedName{Name: siteName + "-s3-credentials", Namespace: namespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, mirrored)
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			Expect(mirrored.Data).To(HaveKeyWithValue("accessKeyID", []byte("AKIAOPERATOR")))
			Expect(mirrored.Data).To(HaveKeyWithValue("secretAccessKey", []byte("operator-secret")))
			Expect(mirrored.OwnerReferences).To(HaveLen(1),
				"the mirrored Secret should be garbage collected with the StaticSite")
		})

		It("uses a per-site credentials Secret when one is named", func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "site-creds", Namespace: namespace},
				Data: map[string][]byte{
					"accessKeyID":     []byte("AKIASITE"),
					"secretAccessKey": []byte("site-secret"),
				},
			})).To(Succeed())

			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Bucket.Credentials = &webv1alpha1.S3CredentialsSpec{
					SecretRef: corev1.LocalObjectReference{Name: "site-creds"},
				}
			}))

			job := waitForJob(testDigest)
			var accessKey *corev1.EnvVar
			for i := range job.Spec.Template.Spec.Containers[0].Env {
				if job.Spec.Template.Spec.Containers[0].Env[i].Name == "AWS_ACCESS_KEY_ID" {
					accessKey = &job.Spec.Template.Spec.Containers[0].Env[i]
				}
			}
			Expect(accessKey.ValueFrom.SecretKeyRef.Name).To(Equal("site-creds"))

			By("not mirroring anything, since the site brought its own")
			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name: siteName + "-s3-credentials", Namespace: namespace,
				}, &corev1.Secret{}))
			}, 2*time.Second, 200*time.Millisecond).Should(BeTrue())
		})
	})

	Context("the ExternalName Service", func() {
		It("points at the bucket's website hostname", func() {
			create(newSite())

			service := &corev1.Service{}
			key := types.NamespacedName{Name: siteName, Namespace: namespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, service)
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			Expect(service.Spec.Type).To(Equal(corev1.ServiceTypeExternalName))
			Expect(service.Spec.ExternalName).To(Equal(
				bucket + ".s3-website-us-east-1.amazonaws.com"))
			// An ExternalName Service resolves by DNS alone; a selector would make the
			// API server try to manage endpoints for it.
			Expect(service.Spec.Selector).To(BeEmpty())
			Expect(service.OwnerReferences).To(HaveLen(1))

			Eventually(func() string {
				return fetch().Status.ServiceName
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(siteName))
		})

		It("declares the default named ports", func() {
			create(newSite())

			service := &corev1.Service{}
			key := types.NamespacedName{Name: siteName, Namespace: namespace}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, key, service)).To(Succeed())
				g.Expect(service.Spec.Ports).To(HaveLen(2))
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			// Named, because an Ingress or HTTPRoute selects a backend port by name.
			Expect(service.Spec.Ports[0].Name).To(Equal("https"))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(443)))
			Expect(service.Spec.Ports[1].Name).To(Equal("http"))
			Expect(service.Spec.Ports[1].Port).To(Equal(int32(80)))
		})

		It("uses the ports a site asks for, and none when it asks for none", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Service.Ports = &[]webv1alpha1.ServicePort{{Name: "web", Port: 8080}}
			}))

			service := &corev1.Service{}
			key := types.NamespacedName{Name: siteName, Namespace: namespace}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, key, service)).To(Succeed())
				g.Expect(service.Spec.Ports).To(HaveLen(1))
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
			Expect(service.Spec.Ports[0].Name).To(Equal("web"))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(8080)))

			// An explicitly empty list is not the same as an absent one: it means a
			// Service with no ports, which is what this produced before.
			site := fetch()
			site.Spec.Service.Ports = &[]webv1alpha1.ServicePort{}
			Expect(k8sClient.Update(ctx, site)).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, key, service)).To(Succeed())
				g.Expect(service.Spec.Ports).To(BeEmpty())
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		})

		It("renders hostnames into the external-dns annotation", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Service.Hostnames = []string{"docs.example.com", "www.example.com"}
				s.Spec.Service.Annotations = map[string]string{
					"external-dns.alpha.kubernetes.io/ttl": "120",
				}
				s.Spec.Service.Labels = map[string]string{"tier": "web"}
			}))

			service := &corev1.Service{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: siteName, Namespace: namespace}, service)).To(Succeed())
				g.Expect(service.Annotations).To(HaveKeyWithValue(
					"external-dns.alpha.kubernetes.io/hostname", "docs.example.com,www.example.com"))
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			Expect(service.Annotations).To(HaveKeyWithValue("external-dns.alpha.kubernetes.io/ttl", "120"))
			Expect(service.Labels).To(HaveKeyWithValue("tier", "web"))
			Expect(service.Labels).To(HaveKeyWithValue(webv1alpha1.ManagedByLabel, webv1alpha1.ManagedByValue))
		})

		It("uses a custom name and removes the old Service when the name changes", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Service.Name = "docs-first"
			}))

			Eventually(func() error {
				return k8sClient.Get(ctx,
					types.NamespacedName{Name: "docs-first", Namespace: namespace}, &corev1.Service{})
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			site := fetch()
			site.Spec.Service.Name = "docs-second"
			Expect(k8sClient.Update(ctx, site)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx,
					types.NamespacedName{Name: "docs-second", Namespace: namespace}, &corev1.Service{})
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: "docs-first", Namespace: namespace}, &corev1.Service{}))
			}, eventuallyTimeout, eventuallyPoll).Should(BeTrue(), "the renamed-away Service should be removed")
		})

		It("is not created when disabled", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Service.Enabled = ptr.To(false)
			}))
			waitForJob(testDigest)

			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: siteName, Namespace: namespace}, &corev1.Service{}))
			}, 2*time.Second, 200*time.Millisecond).Should(BeTrue())
		})
	})

	Context("when the source image changes", func() {
		It("syncs again after the tag moves", func() {
			create(newSite())
			syncToReady(testDigest)

			By("moving the tag to a new digest")
			fakeRegistry.setDigest(testImage, otherDigest)

			By("forcing a re-resolve with the sync-requested-at annotation")
			site := fetch()
			site.Annotations = map[string]string{
				webv1alpha1.SyncRequestedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
			}
			Expect(k8sClient.Update(ctx, site)).To(Succeed())

			job := waitForJob(otherDigest)
			finishJob(job, batchv1.JobComplete, "")

			Eventually(func() string {
				return fetch().Status.SyncedDigest
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(otherDigest))
		})

		It("does not re-resolve the registry on every reconcile", func() {
			// This is the whole point of spec.source.pullInterval: a Job event must not
			// turn into a registry request.
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Source.PullInterval = &metav1.Duration{Duration: time.Hour}
			}))
			syncToReady(testDigest)

			before := fakeRegistry.resolveCount()
			Consistently(func() int {
				return fakeRegistry.resolveCount()
			}, 3*time.Second, 250*time.Millisecond).Should(Equal(before),
				"the digest should be reused until the pull interval expires")
		})

		It("starts a new sync when sync settings change even though the digest has not", func() {
			create(newSite())
			syncToReady(testDigest)

			site := fetch()
			site.Spec.Sync.KeyPrefix = "docs"
			Expect(k8sClient.Update(ctx, site)).To(Succeed())

			Eventually(func(g Gomega) {
				var prefixed bool
				for _, job := range listJobs() {
					for _, arg := range job.Spec.Template.Spec.Containers[0].Args {
						if arg == "--key-prefix=docs" {
							prefixed = true
						}
					}
				}
				g.Expect(prefixed).To(BeTrue(), "a Job should run with the new key prefix")
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		})
	})

	Context("when a sync fails", func() {
		It("reports the failure and counts it", func() {
			// A long retry delay keeps the failed state observable: with the suite's
			// usual one second backoff the operator starts the next attempt immediately,
			// and the Synced condition moves straight back to SyncInProgress.
			defaults.SyncRetryBaseDelay = 30 * time.Second
			defaults.SyncRetryMaxDelay = 30 * time.Second

			create(newSite())
			job := waitForJob(testDigest)
			finishJob(job, batchv1.JobFailed, "BackoffLimitExceeded")

			expectCondition(webv1alpha1.ConditionSynced, metav1.ConditionFalse, webv1alpha1.ReasonSyncFailed)
			expectCondition(webv1alpha1.ConditionReady, metav1.ConditionFalse, webv1alpha1.ReasonSyncFailed)

			Eventually(func(g Gomega) {
				site := fetch()
				g.Expect(site.Status.FailedSyncs).To(BeNumerically(">=", 1))
				g.Expect(site.Status.SyncedDigest).To(BeEmpty(),
					"a failed sync must not claim the bucket holds the new content")
				g.Expect(site.Status.Phase).To(Equal(webv1alpha1.PhaseFailed))
				g.Expect(site.Status.SyncHistory).NotTo(BeEmpty())
				g.Expect(site.Status.SyncHistory[0].Succeeded).To(BeFalse())
				g.Expect(site.Status.SyncHistory[0].Message).To(ContainSubstring("BackoffLimitExceeded"))
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		})

		It("reports a provider failure against BucketReady", func() {
			fakeS3.failProvision(fmt.Errorf("the object store is unreachable"))
			create(newSite())

			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionFalse,
				webv1alpha1.ReasonBucketFailed)
			Eventually(func() webv1alpha1.StaticSitePhase {
				return fetch().Status.Phase
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(webv1alpha1.PhaseFailed))

			By("recovering once the provider does")
			fakeS3.failProvision(nil)
			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionTrue,
				webv1alpha1.ReasonBucketReady)
		})

		It("reports a missing credentials Secret without hammering the provider", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Bucket.Credentials = &webv1alpha1.S3CredentialsSpec{
					SecretRef: corev1.LocalObjectReference{Name: "does-not-exist"},
				}
			}))

			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionFalse,
				webv1alpha1.ReasonCredentialsError)
			Consistently(func() int {
				return fakeS3.provisionCalls
			}, 2*time.Second, 250*time.Millisecond).Should(BeZero(),
				"a configuration error should not reach the provider at all")
		})

		It("reports a registry that is unreachable, and recovers when it returns", func() {
			// Distinct from an image that does not exist: an outage is transient, so the
			// operator has to retry rather than wait for the spec to change.
			fakeRegistry.setError(fmt.Errorf("dial tcp: connection refused"))

			create(newSite())
			expectCondition(webv1alpha1.ConditionSourceResolved, metav1.ConditionFalse,
				webv1alpha1.ReasonResolveFailed)
			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionTrue,
				webv1alpha1.ReasonBucketReady)

			By("recovering once the registry answers again")
			fakeRegistry.setError(nil)
			expectCondition(webv1alpha1.ConditionSourceResolved, metav1.ConditionTrue,
				webv1alpha1.ReasonResolved)
			syncToReady(testDigest)
		})

		It("reports an unresolvable image against SourceResolved", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Source.Image = "registry.example.com/acme/missing:latest"
			}))

			expectCondition(webv1alpha1.ConditionSourceResolved, metav1.ConditionFalse,
				webv1alpha1.ReasonResolveFailed)
			Consistently(func() int {
				return len(listJobs())
			}, 2*time.Second, 250*time.Millisecond).Should(BeZero(),
				"no Job should run for an image that does not resolve")
		})
	})

	Context("when a StaticSite is suspended", func() {
		It("stops reconciling without deleting anything", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) { s.Spec.Suspend = true }))

			expectCondition(webv1alpha1.ConditionReady, metav1.ConditionFalse, webv1alpha1.ReasonSuspended)
			Eventually(func() webv1alpha1.StaticSitePhase {
				return fetch().Status.Phase
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(webv1alpha1.PhaseSuspended))

			Consistently(func() int {
				return len(listJobs())
			}, 2*time.Second, 250*time.Millisecond).Should(BeZero())

			By("resuming when suspend is cleared")
			site := fetch()
			site.Spec.Suspend = false
			Expect(k8sClient.Update(ctx, site)).To(Succeed())
			waitForJob(testDigest)
		})
	})

	Context("when a StaticSite is deleted", func() {
		It("deletes a bucket it created", func() {
			create(newSite())
			syncToReady(testDigest)
			Expect(fakeS3.exists(bucket)).To(BeTrue())

			deleteSite()
			Expect(fakeS3.exists(bucket)).To(BeFalse(), "the bucket should be gone with the StaticSite")
		})

		It("retains the bucket when deletionPolicy is Retain", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Bucket.DeletionPolicy = webv1alpha1.DeletionPolicyRetain
			}))
			syncToReady(testDigest)

			deleteSite()
			Expect(fakeS3.exists(bucket)).To(BeTrue(), "a Retain policy should leave the bucket alone")
		})

		It("refuses to delete a bucket it only adopted", func() {
			// The safety property that matters most: a bucket the operator did not
			// create is never destroyed, whatever the deletion policy says.
			fakeS3.preCreateUnowned(bucket)

			create(newSite())
			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionTrue,
				webv1alpha1.ReasonBucketReady)
			Eventually(func() *webv1alpha1.BucketOwnership {
				return fetch().Status.BucketOwnership
			}, eventuallyTimeout, eventuallyPoll).Should(BeNil(),
				"an adopted bucket should record no ownership")

			syncToReady(testDigest)
			deleteSite()

			Expect(fakeS3.exists(bucket)).To(BeTrue(),
				"an adopted bucket must survive deletion of the StaticSite")
		})

		It("still releases the finalizer when the bucket cannot be deleted", func() {
			// A StaticSite that can never finish deleting leaves its namespace stuck in
			// Terminating, which is worse than leaking a bucket that is named in an
			// event.
			create(newSite())
			syncToReady(testDigest)

			fakeS3.failDeprovision(fmt.Errorf("the object store is unreachable"))
			// The abandon deadline is measured from the deletion timestamp against
			// SyncRetryMaxDelay, which this suite sets to a few seconds.
			deleteSite()

			Expect(fakeS3.exists(bucket)).To(BeTrue(), "the bucket was left behind, as reported")
		})
	})

	Context("when a bucket belongs to another StaticSite", func() {
		It("refuses to touch it", func() {
			fakeS3.preCreateOwnedByOther(bucket, "other-namespace/other-site")

			create(newSite())
			expectCondition(webv1alpha1.ConditionBucketReady, metav1.ConditionFalse,
				webv1alpha1.ReasonBucketFailed)

			Eventually(func(g Gomega) {
				condition := findCondition(fetch(), webv1alpha1.ConditionBucketReady)
				g.Expect(condition.Message).To(ContainSubstring("owned by another StaticSite"))
			}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

			Consistently(func() int {
				return len(listJobs())
			}, 2*time.Second, 250*time.Millisecond).Should(BeZero())
		})
	})

	Context("sync history", func() {
		It("keeps at most retainHistory attempts, newest first", func() {
			create(newSite(func(s *webv1alpha1.StaticSite) {
				s.Spec.Sync.RetainHistory = ptr.To(int32(2))
			}))

			// Three failures in a row, so the oldest has to fall off the list.
			for attempt := 1; attempt <= 3; attempt++ {
				var job *batchv1.Job
				Eventually(func(g Gomega) {
					for _, candidate := range listJobs() {
						if outcome, _ := classifyJob(&candidate); outcome == jobRunning {
							job = candidate.DeepCopy()
							return
						}
					}
					g.Expect(job).NotTo(BeNil(), "a new Job should be started after a failure")
				}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

				finishJob(job, batchv1.JobFailed, "attempt failed")

				// The failure counter is what to wait on: activeSyncJob does not stay
				// empty, because the operator starts the next attempt as soon as the
				// backoff expires.
				Eventually(func() int32 {
					return fetch().Status.FailedSyncs
				}, eventuallyTimeout, eventuallyPoll).Should(BeNumerically(">=", int32(attempt)))
			}

			Eventually(func() int {
				return len(fetch().Status.SyncHistory)
			}, eventuallyTimeout, eventuallyPoll).Should(Equal(2))
		})
	})
})

// kubectlFreeAnnotate nudges a StaticSite so a reconcile runs, which is how a spec
// checks behaviour that only shows up on the next pass.
func kubectlFreeAnnotate(site *webv1alpha1.StaticSite) (*webv1alpha1.StaticSite, error) {
	if site.Annotations == nil {
		site.Annotations = map[string]string{}
	}
	site.Annotations["test.rka.sh/nudge"] = time.Now().Format(time.RFC3339Nano)
	return site, k8sClient.Update(ctx, site)
}

func findCondition(site *webv1alpha1.StaticSite, conditionType string) *metav1.Condition {
	for i := range site.Status.Conditions {
		if site.Status.Conditions[i].Type == conditionType {
			return &site.Status.Conditions[i]
		}
	}
	return nil
}
