//go:build e2e

/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package e2e drives the operator in a real cluster, against a real object store
// and a real container registry.
//
// The suite deliberately reaches the finished site the way a visitor would, with
// an unauthenticated HTTP GET against the bucket. Everything before that -- the
// bucket, its policy, the extracted image, the uploaded objects, their content
// types -- has to be right for that request to return the page, so it is the one
// assertion that cannot pass by accident.
//
// Requires Kind and Docker. Run it with `make test-e2e`, which creates a
// throwaway cluster and deletes it afterwards.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rashkash103/static-s3-operator/test/utils"
)

const (
	// managerImage is the operator image built and loaded for the run. It carries
	// the syncer too, which is what sync Jobs run.
	managerImage = "example.com/static-s3-operator:e2e"

	// operatorNamespace is where the chart installs the operator.
	operatorNamespace = "static-s3-operator-system"
	// helmRelease is the release name used for the install.
	helmRelease = "static-s3-operator"

	// e2eNamespace holds the object store, the registry and the test sites.
	e2eNamespace = "static-s3-e2e"

	// The in-cluster addresses the operator and its Jobs use.
	minioEndpoint  = "http://minio." + e2eNamespace + ".svc.cluster.local:9000"
	registryHost   = "registry." + e2eNamespace + ".svc.cluster.local:5000"
	minioAccessKey = "minioadmin"
	minioSecretKey = "minioadmin"

	// Local ports the suite forwards to, so it can push images and fetch pages
	// from outside the cluster.
	localRegistryPort = 15000
	localMinIOPort    = 19000
)

var (
	// localRegistry and localMinIO are the forwarded addresses of the in-cluster
	// registry and object store.
	localRegistry = fmt.Sprintf("127.0.0.1:%d", localRegistryPort)
	localMinIO    = fmt.Sprintf("http://127.0.0.1:%d", localMinIOPort)

	registryForward *portForward
	minioForward    *portForward
)

// TestE2E runs the end-to-end suite against an isolated Kind cluster.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting static-s3-operator e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// Local kubectl preferences would otherwise change how these commands behave.
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		Expect(os.Setenv("KUBECTL_KUBERC", "false")).To(Succeed())
	}

	By("building the operator image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+managerImage))
	Expect(err).NotTo(HaveOccurred(), "failed to build the operator image")

	By("loading the operator image into the Kind cluster")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())

	By("creating the test namespace")
	_, _ = utils.Run(exec.Command("kubectl", "create", "namespace", e2eNamespace))

	By("deploying an object store and a registry into the cluster")
	// The sync Job pulls the site image over the registry API from inside the
	// cluster, so a registry the Pod can reach is not optional here.
	applyTestData("minio.yaml")
	applyTestData("registry.yaml")

	By("waiting for the object store and registry to become available")
	waitForDeployment("minio")
	waitForDeployment("registry")

	By("installing the operator with Helm")
	installOperator()

	By("forwarding the registry and the object store to localhost")
	registryForward = startPortForward("svc/registry", localRegistryPort, 5000)
	minioForward = startPortForward("svc/minio", localMinIOPort, 9000)

	// A forward that is not ready yet fails the first push in a way that looks like
	// a registry problem, so it is worth waiting for explicitly.
	waitForEndpoint(fmt.Sprintf("http://%s/v2/", localRegistry))
	waitForEndpoint(localMinIO + "/minio/health/live")
})

var _ = AfterSuite(func() {
	if registryForward != nil {
		registryForward.stop()
	}
	if minioForward != nil {
		minioForward.stop()
	}

	By("collecting operator logs")
	dumpOperatorLogs()

	By("uninstalling the operator")
	_, _ = utils.Run(exec.Command("helm", "uninstall", helmRelease, "--namespace", operatorNamespace))

	By("deleting the test namespace")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", e2eNamespace, "--wait=false"))
})

// installOperator installs the chart, pointing every site at the in-cluster
// object store by default. Installing through the chart rather than through
// kustomize means the release path users follow is the one under test.
func installOperator() {
	args := []string{
		"upgrade", "--install", helmRelease, "dist/chart",
		"--namespace", operatorNamespace, "--create-namespace",
		"--wait", "--timeout", "5m",
		"--set", "manager.image.repository=example.com/static-s3-operator",
		"--set", "manager.image.tag=e2e",
		"--set", "manager.image.pullPolicy=Never",
		// The operator's defaults are what a site inherits, so setting them here also
		// checks that the chart wires them through to the manager's flags.
		"--set", "operator.defaults.endpoint=" + minioEndpoint,
		"--set", "operator.defaults.forcePathStyle=true",
		"--set", "operator.defaults.region=us-east-1",
		// The syncer image is the operator image, and must not be pulled.
		"--set", "operator.syncer.imagePullPolicy=Never",
		// Short delays so a spec can watch a retry happen.
		"--set", "operator.reconcile.syncRetryBaseDelay=5s",
		"--set", "operator.reconcile.syncRetryMaxDelay=20s",
		"--set", "operator.reconcile.ownershipRecheckInterval=30s",
		"--set", "operator.credentials.create=true",
		"--set", "operator.credentials.accessKeyID=" + minioAccessKey,
		"--set", "operator.credentials.secretAccessKey=" + minioSecretKey,
		// Metrics need no certificates for this run.
		"--set", "metrics.enabled=false",
	}

	output, err := utils.Run(exec.Command("helm", args...))
	Expect(err).NotTo(HaveOccurred(), "failed to install the chart: %s", output)

	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "deployment",
			helmRelease+"-controller-manager", "-n", operatorNamespace,
			"-o", "jsonpath={.status.readyReplicas}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("1"), "the operator should have one ready replica")
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
}

func applyTestData(file string) {
	output, err := utils.Run(exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/"+file))
	Expect(err).NotTo(HaveOccurred(), "failed to apply %s: %s", file, output)
}

func waitForDeployment(name string) {
	output, err := utils.Run(exec.Command("kubectl", "wait", "--for=condition=Available",
		"deployment/"+name, "-n", e2eNamespace, "--timeout=5m"))
	Expect(err).NotTo(HaveOccurred(), "%s did not become available: %s", name, output)
}

// dumpOperatorLogs writes the manager's logs to the test output, which is the
// difference between a diagnosable CI failure and a mystery.
func dumpOperatorLogs() {
	output, err := utils.Run(exec.Command("kubectl", "logs",
		"-n", operatorNamespace, "deployment/"+helmRelease+"-controller-manager",
		"--tail=400", "--all-containers"))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "could not read operator logs: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "--- operator logs ---\n%s\n--- end operator logs ---\n", output)
}
