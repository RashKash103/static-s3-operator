//go:build e2e

/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	operators3 "github.com/rashkash103/static-s3-operator/internal/s3"
	"github.com/rashkash103/static-s3-operator/test/utils"
)

// portForward is a running `kubectl port-forward`, which is how the suite reaches
// the in-cluster registry and object store from the test process.
type portForward struct {
	command *exec.Cmd
	target  string
}

func startPortForward(target string, localPort, remotePort int) *portForward {
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	command := exec.Command("kubectl", "port-forward", target, //nolint:gosec // fixed arguments
		fmt.Sprintf("%d:%d", localPort, remotePort), "-n", e2eNamespace)
	command.Dir = dir
	command.Stdout = GinkgoWriter
	command.Stderr = GinkgoWriter

	Expect(command.Start()).To(Succeed(), "failed to start port-forward for %s", target)
	return &portForward{command: command, target: target}
}

func (f *portForward) stop() {
	if f == nil || f.command == nil || f.command.Process == nil {
		return
	}
	_ = f.command.Process.Kill()
	_, _ = f.command.Process.Wait()
}

// waitForEndpoint waits until a URL answers at all, whatever the status. A
// port-forward accepts connections slightly before it can serve them, and a first
// request that races that window looks like a broken service.
func waitForEndpoint(url string) {
	Eventually(func() error {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		return nil
	}, 2*time.Minute, time.Second).Should(Succeed(), "%s never became reachable", url)
}

// httpGet fetches a URL with no credentials, exactly as a browser would.
func httpGet(url string) (string, int, http.Header, error) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", 0, nil, err
	}

	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return "", 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", response.StatusCode, response.Header, err
	}
	return string(body), response.StatusCode, response.Header, nil
}

// objectURL is the address a visitor would use for one object in a path-style
// bucket.
func objectURL(bucket, key string) string {
	return fmt.Sprintf("%s/%s/%s", localObjectStore, bucket, key)
}

// s3Client talks to the forwarded object store with the same client the operator
// uses, so the suite can assert on the bucket itself and not only on what it
// serves.
func s3Client() *awss3.Client {
	client, err := operators3.NewClient(operators3.ClientOptions{
		Region:         "us-east-1",
		Endpoint:       localObjectStore,
		ForcePathStyle: true,
		Credentials: operators3.Credentials{
			AccessKeyID:     objectStoreAccessKey,
			SecretAccessKey: objectStoreSecretKey,
		},
		MaxRetries: 3,
	})
	Expect(err).NotTo(HaveOccurred())
	return client
}

// kubectl runs kubectl and returns its trimmed output.
func kubectl(args ...string) (string, error) {
	output, err := utils.Run(exec.Command("kubectl", args...))
	return strings.TrimSpace(output), err
}

// applyManifest pipes a manifest into kubectl apply.
func applyManifest(manifest string) {
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	command := exec.Command("kubectl", "apply", "-f", "-")
	command.Dir = dir
	command.Stdin = strings.NewReader(manifest)

	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "failed to apply manifest: %s\n%s", output, manifest)
	_, _ = fmt.Fprintf(GinkgoWriter, "applied manifest:\n%s\n", output)
}

// siteField reads one JSONPath expression from a StaticSite.
func siteField(name, jsonPath string) (string, error) {
	return kubectl("get", "staticsite", name, "-n", e2eNamespace, "-o", "jsonpath="+jsonPath)
}

// waitForSiteReady waits until the StaticSite reports Ready, and fails with the
// site's own conditions rather than a bare timeout so a CI failure is
// diagnosable.
func waitForSiteReady(name string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		ready, err := siteField(name, `{.status.conditions[?(@.type=="Ready")].status}`)
		g.Expect(err).NotTo(HaveOccurred())

		if ready != "True" {
			conditions, _ := kubectl("get", "staticsite", name, "-n", e2eNamespace,
				"-o", "jsonpath={range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{\"\\n\"}{end}")
			g.Expect(ready).To(Equal("True"), "StaticSite %s is not Ready yet:\n%s", name, conditions)
		}
	}, timeout, 3*time.Second).Should(Succeed())
}

// bucketExists reports whether the bucket is present in the object store.
func bucketExists(bucket string) bool {
	manager := operators3.NewBucketManager(s3Client())
	exists, err := manager.Exists(context.Background(), bucket)
	Expect(err).NotTo(HaveOccurred())
	return exists
}

// bucketKeys lists every object key in a bucket.
func bucketKeys(bucket string) []string {
	client := s3Client()

	var keys []string
	paginator := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{
		Bucket: &bucket,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		Expect(err).NotTo(HaveOccurred())
		for _, object := range page.Contents {
			keys = append(keys, *object.Key)
		}
	}
	return keys
}
