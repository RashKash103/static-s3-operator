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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	"github.com/rashkash103/static-s3-operator/internal/config"
	"github.com/rashkash103/static-s3-operator/internal/provisioner"
	"github.com/rashkash103/static-s3-operator/internal/registry"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

// The suite runs the real reconciler against a real API server, with the two
// things it cannot reach from a test replaced: the S3 provider and the container
// registry. Everything else -- finalizers, Job creation and adoption, status
// conditions, owner references, the ExternalName Service -- is the production
// code path.
//
// envtest runs no kubelet and no Job controller, so a test that wants a sync to
// finish sets the Job's status itself, exactly as the real Job controller would.

const (
	// operatorNamespace stands in for the namespace the operator is installed in,
	// and is where its default credentials Secret lives.
	operatorNamespace = "static-s3-operator-system"
	// defaultCredentialsSecret is the operator's own credentials Secret.
	defaultCredentialsSecret = "static-s3-operator-s3-credentials"

	eventuallyTimeout = 20 * time.Second
	eventuallyPoll    = 100 * time.Millisecond
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client

	// fakeS3 is the stand-in object store the reconciler provisions against.
	fakeS3 *fakeProvisioner
	// fakeRegistry is the stand-in registry that resolves image tags to digests.
	fakeRegistry *fakeResolver
	// defaults are the operator settings the reconciler runs with; tests may
	// adjust them, and resetTestState puts them back.
	defaults *config.Defaults
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = webv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("creating the operator namespace and its default credentials")
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: operatorNamespace},
	})).To(Succeed())
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: defaultCredentialsSecret, Namespace: operatorNamespace},
		Data: map[string][]byte{
			"accessKeyID":     []byte("AKIAOPERATOR"),
			"secretAccessKey": []byte("operator-secret"),
		},
	})).To(Succeed())

	By("starting the StaticSite controller")
	fakeS3 = newFakeProvisioner()
	fakeRegistry = newFakeResolver()
	defaults = testDefaults()

	manager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Client: client.Options{
			// Mirrors production: Secrets are read live rather than watched.
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	})
	Expect(err).NotTo(HaveOccurred())

	Expect((&StaticSiteReconciler{
		Client: manager.GetClient(),
		Scheme: manager.GetScheme(),
		// Matches production; see the note in SetupWithManager.
		Recorder:     manager.GetEventRecorderFor("staticsite-controller"), //nolint:staticcheck // see above
		Defaults:     defaults,
		Provisioner:  fakeS3,
		ResolveImage: fakeRegistry.Resolve,
	}).SetupWithManager(manager)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(manager.Start(ctx)).To(Succeed(), "the manager should stop cleanly")
	}()

	Eventually(func() bool {
		return manager.GetCache().WaitForCacheSync(ctx)
	}, eventuallyTimeout, eventuallyPoll).Should(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// testDefaults are the operator settings the suite runs with. The retry delays
// are short so a test can observe a retry without waiting for production
// backoff.
func testDefaults() *config.Defaults {
	settings := config.New()
	settings.Namespace = operatorNamespace
	settings.CredentialsSecret = defaultCredentialsSecret
	settings.SyncerImage = "ghcr.io/rashkash103/static-s3-operator:test"
	settings.SourcePath = "/usr/share/nginx/html"
	settings.Region = "us-east-1"
	settings.SyncRetryBaseDelay = time.Second
	settings.SyncRetryMaxDelay = 5 * time.Second
	settings.OwnershipRecheckInterval = 10 * time.Second
	settings.MaxConcurrentReconciles = 1
	return settings
}

// resetTestState puts the fakes and the operator settings back to their starting
// point between tests.
func resetTestState() {
	fakeS3.reset()
	fakeRegistry.reset()

	fresh := testDefaults()
	*defaults = *fresh
}

// fakeResolver resolves image references from a table, so a test can move a tag
// by rewriting the table.
type fakeResolver struct {
	mu sync.Mutex
	// digests maps an image reference to the digest it currently resolves to.
	digests map[string]string
	// err, when set, is returned for every reference.
	err error
	// calls counts resolutions, which is how a test checks that the pull interval
	// is actually preventing them.
	calls int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{digests: map[string]string{}}
}

func (r *fakeResolver) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.digests = map[string]string{}
	r.err = nil
	r.calls = 0
}

// setDigest points an image reference at a digest. Passing the same reference
// again is how a test simulates a tag that has moved.
func (r *fakeResolver) setDigest(image, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.digests[image] = digest
}

func (r *fakeResolver) setError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *fakeResolver) resolveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *fakeResolver) Resolve(
	_ context.Context,
	image string,
	_ registry.Options,
) (*registry.Reference, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls++
	if r.err != nil {
		return nil, r.err
	}

	digest, known := r.digests[image]
	if !known {
		return nil, fmt.Errorf("fake registry has no image %q", image)
	}

	repository := repositoryOf(image)
	return &registry.Reference{
		Digest: digest,
		Pinned: repository + "@" + digest,
	}, nil
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// Ensure the fake satisfies the interface the reconciler depends on.
var _ provisioner.Provisioner = (*fakeProvisioner)(nil)
