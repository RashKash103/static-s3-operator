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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	operatorconfig "github.com/rashkash103/static-s3-operator/internal/config"
	operators3 "github.com/rashkash103/static-s3-operator/internal/s3"
)

const (
	readyTimeout = 5 * time.Minute
	syncTimeout  = 5 * time.Minute
)

var _ = Describe("StaticSite", Ordered, func() {
	// One bucket and one site name for the whole publish-and-republish story, so
	// the second publish exercises an existing bucket rather than a fresh one.
	var (
		siteName = "sample-site"
		bucket   = "e2e-sample-site"
		// imageRef is the in-cluster reference the operator resolves and the sync Job
		// pulls; pushRef is the same repository through the port-forward.
		imageRef = registryHost + "/sample-site:latest"
		pushRef  = localRegistry + "/sample-site:latest"
	)

	// staticSiteManifest describes the site under test. It sets plainHTTP because
	// the in-cluster registry has no TLS, and excludes source maps to check that
	// spec.sync.excludes keeps a file out of the bucket.
	staticSiteManifest := func() string {
		return fmt.Sprintf(`
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: %s
  namespace: %s
spec:
  source:
    image: %s
    plainHTTP: true
    # Polling off: the specs republish and then ask for a sync explicitly, which
    # keeps the run deterministic instead of waiting on an interval.
    pullInterval: "0"
  bucket:
    name: %s
    website:
      indexDocument: index.html
      errorDocument: 404.html
  sync:
    deleteRemoved: true
    excludes:
      - "**/*.map"
    cacheControl:
      - pattern: "assets/**"
        value: "public, max-age=31536000, immutable"
      - pattern: "**/*.html"
        value: "public, max-age=0, must-revalidate"
  service:
    enabled: true
    hostnames:
      - sample.e2e.example.com
`, siteName, e2eNamespace, imageRef, bucket)
	}

	BeforeAll(func() {
		By("publishing the first version of the sample site")
		image, err := buildSiteImage(sampleSite("v1"))
		Expect(err).NotTo(HaveOccurred())

		digest, err := pushSiteImage(pushRef, image)
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "pushed %s at %s\n", pushRef, digest)

		By("creating the StaticSite")
		applyManifest(staticSiteManifest())
	})

	AfterAll(func() {
		By("dumping the StaticSite for diagnosis")
		output, _ := kubectl("get", "staticsite", siteName, "-n", e2eNamespace, "-o", "yaml")
		_, _ = fmt.Fprintf(GinkgoWriter, "--- StaticSite %s ---\n%s\n", siteName, output)

		By("deleting the StaticSite")
		_, _ = kubectl("delete", "staticsite", siteName, "-n", e2eNamespace, "--wait=true", "--timeout=3m")
	})

	It("becomes Ready", func() {
		waitForSiteReady(siteName, readyTimeout)

		By("reporting the bucket it created")
		Eventually(func(g Gomega) {
			name, err := siteField(siteName, "{.status.bucketName}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(name).To(Equal(bucket))
		}, readyTimeout, 2*time.Second).Should(Succeed())

		By("recording an ownership token, which is what will permit deletion")
		token, err := siteField(siteName, "{.status.bucketOwnership.token}")
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		created, err := siteField(siteName, "{.status.bucketOwnership.created}")
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(Equal("true"))

		By("reporting a website URL")
		url, err := siteField(siteName, "{.status.websiteURL}")
		Expect(err).NotTo(HaveOccurred())
		Expect(url).To(ContainSubstring(bucket))
	})

	It("creates the bucket and uploads the site", func() {
		Expect(bucketExists(bucket)).To(BeTrue(), "the operator should have created the bucket")

		keys := bucketKeys(bucket)
		_, _ = fmt.Fprintf(GinkgoWriter, "bucket contents: %v\n", keys)

		for _, key := range []string{
			"index.html",
			"about/index.html",
			"404.html",
			"assets/app.css",
			"assets/app.js",
			"robots.txt",
		} {
			Expect(keys).To(ContainElement(key), "object %q should be in the bucket", key)
		}

		By("honouring spec.sync.excludes")
		Expect(keys).NotTo(ContainElement("assets/app.js.map"),
			"an excluded file should never be uploaded")
	})

	It("serves the site to an anonymous visitor", func() {
		// The assertion that cannot pass by accident: the bucket, its policy, the
		// extracted image, the uploaded objects and their content types all have to be
		// right for this to return the page.
		body, status, headers, err := httpGet(objectURL(bucket, "index.html"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200), "an anonymous GET should succeed; body: %s", body)
		Expect(body).To(ContainSubstring("Sample site v1"))
		Expect(headers.Get("Content-Type")).To(HavePrefix("text/html"),
			"a wrong content type makes the browser download the page instead of rendering it")

		By("serving nested pages and assets")
		body, status, headers, err = httpGet(objectURL(bucket, "about/index.html"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(body).To(ContainSubstring("About"))

		body, status, headers, err = httpGet(objectURL(bucket, "assets/app.css"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(headers.Get("Content-Type")).To(HavePrefix("text/css"))
		Expect(body).To(ContainSubstring("font-family"))
	})

	It("applies the Cache-Control rules", func() {
		_, status, headers, err := httpGet(objectURL(bucket, "assets/app.css"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(headers.Get("Cache-Control")).To(Equal("public, max-age=31536000, immutable"))

		_, status, headers, err = httpGet(objectURL(bucket, "index.html"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(headers.Get("Cache-Control")).To(Equal("public, max-age=0, must-revalidate"))
	})

	It("maintains an ExternalName Service for external-dns", func() {
		Eventually(func(g Gomega) {
			serviceType, err := kubectl("get", "service", siteName, "-n", e2eNamespace,
				"-o", "jsonpath={.spec.type}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(serviceType).To(Equal("ExternalName"))
		}, readyTimeout, 2*time.Second).Should(Succeed())

		externalName, err := kubectl("get", "service", siteName, "-n", e2eNamespace,
			"-o", "jsonpath={.spec.externalName}")
		Expect(err).NotTo(HaveOccurred())
		Expect(externalName).To(Equal("objectstore." + e2eNamespace + ".svc.cluster.local"))

		By("rendering the hostnames into the external-dns annotation")
		annotation, err := kubectl("get", "service", siteName, "-n", e2eNamespace,
			"-o", `jsonpath={.metadata.annotations.external-dns\.alpha\.kubernetes\.io/hostname}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(annotation).To(Equal("sample.e2e.example.com"))
	})

	It("records what the sync did", func() {
		uploaded, err := siteField(siteName, "{.status.lastSyncStats.uploaded}")
		Expect(err).NotTo(HaveOccurred())
		Expect(uploaded).NotTo(BeEmpty(), "the sync Job's report should reach status")

		digest, err := siteField(siteName, "{.status.syncedDigest}")
		Expect(err).NotTo(HaveOccurred())
		Expect(digest).To(HavePrefix("sha256:"))

		succeeded, err := siteField(siteName, "{.status.syncHistory[0].succeeded}")
		Expect(err).NotTo(HaveOccurred())
		Expect(succeeded).To(Equal("true"))
	})

	It("does nothing when nothing has changed", func() {
		// A site that is already up to date must not re-upload, or a daily poll would
		// cost as much as a full publish.
		before, err := siteField(siteName, "{.status.lastSyncTime}")
		Expect(err).NotTo(HaveOccurred())

		Consistently(func(g Gomega) {
			after, err := siteField(siteName, "{.status.lastSyncTime}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(after).To(Equal(before))
		}, 30*time.Second, 5*time.Second).Should(Succeed())
	})

	It("republishes when the tag moves, and mirrors the new contents", func() {
		By("pushing a second version over the same tag")
		// A changed file, a new file and a removed file: enough to tell a mirror from
		// an accumulation.
		image, err := buildSiteImage(sampleSiteV2())
		Expect(err).NotTo(HaveOccurred())

		newDigest, err := pushSiteImage(pushRef, image)
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "pushed v2 at %s\n", newDigest)

		By("asking for a sync")
		_, err = kubectl("annotate", "staticsite", siteName, "-n", e2eNamespace,
			"web.rka.sh/sync-requested-at="+time.Now().UTC().Format(time.RFC3339Nano), "--overwrite")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the new digest to be synced")
		Eventually(func(g Gomega) {
			synced, err := siteField(siteName, "{.status.syncedDigest}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(synced).To(Equal(newDigest))
		}, syncTimeout, 3*time.Second).Should(Succeed())
		waitForSiteReady(siteName, readyTimeout)

		By("serving the updated page")
		Eventually(func(g Gomega) {
			body, status, _, err := httpGet(objectURL(bucket, "index.html"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal(200))
			g.Expect(body).To(ContainSubstring("Sample site v2"))
		}, syncTimeout, 3*time.Second).Should(Succeed())

		By("uploading the new file")
		keys := bucketKeys(bucket)
		Expect(keys).To(ContainElement("new-page.html"))

		By("deleting the file that is no longer in the image")
		Expect(keys).NotTo(ContainElement("404.html"),
			"deleteRemoved should have removed the page that v2 dropped")

		_, status, _, err := httpGet(objectURL(bucket, "404.html"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(404), "the removed page should no longer be served")
	})

	It("prunes old sync Jobs", func() {
		Eventually(func(g Gomega) {
			output, err := kubectl("get", "jobs", "-n", e2eNamespace,
				"-l", "web.rka.sh/staticsite="+siteName, "-o", "name")
			g.Expect(err).NotTo(HaveOccurred())

			count := 0
			for _, line := range strings.Split(output, "\n") {
				if strings.TrimSpace(line) != "" {
					count++
				}
			}
			// spec.sync.retainHistory defaults to 3.
			g.Expect(count).To(BeNumerically("<=", 4), "sync Jobs should be pruned, found:\n%s", output)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("deletes the bucket it created when the StaticSite goes away", func() {
		Expect(bucketExists(bucket)).To(BeTrue())

		_, err := kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
			"--wait=true", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			return bucketExists(bucket)
		}, 3*time.Minute, 3*time.Second).Should(BeFalse(),
			"the bucket should be deleted along with the StaticSite")

		By("removing the ExternalName Service with it")
		Eventually(func() string {
			output, _ := kubectl("get", "service", siteName, "-n", e2eNamespace, "--ignore-not-found")
			return output
		}, time.Minute, 2*time.Second).Should(BeEmpty())
	})
})

var _ = Describe("bucket ownership", Ordered, func() {
	// The safety property that matters most: a bucket the operator did not create
	// must survive deletion of the StaticSite that pointed at it, whatever the
	// deletion policy says.
	const (
		siteName = "adopted-site"
		bucket   = "e2e-preexisting-bucket"
	)

	imageRef := registryHost + "/adopted-site:latest"
	pushRef := localRegistry + "/adopted-site:latest"

	BeforeAll(func() {
		By("creating a bucket by hand, as if it belonged to somebody else")
		manager := operators3.NewBucketManager(s3Client())
		_, err := manager.Create(context.Background(), bucket, "us-east-1")
		Expect(err).NotTo(HaveOccurred())

		By("putting an object in it that the operator must not destroy")
		_, err = s3Client().PutObject(context.Background(), &awss3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String("pre-existing.txt"),
			Body:   strings.NewReader("this file was here first"),
		})
		Expect(err).NotTo(HaveOccurred())

		By("publishing a site image")
		image, err := buildSiteImage(sampleSite("adopted"))
		Expect(err).NotTo(HaveOccurred())
		_, err = pushSiteImage(pushRef, image)
		Expect(err).NotTo(HaveOccurred())

		By("pointing a StaticSite at the existing bucket, with deletionPolicy Delete")
		applyManifest(fmt.Sprintf(`
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: %s
  namespace: %s
spec:
  source:
    image: %s
    plainHTTP: true
    pullInterval: "0"
  bucket:
    name: %s
    # Deliberately the destructive policy: adoption has to override it.
    deletionPolicy: Delete
    website:
      indexDocument: index.html
  sync:
    # The pre-existing object must not be treated as site content to remove.
    deleteRemoved: false
  service:
    enabled: false
`, siteName, e2eNamespace, imageRef, bucket))
	})

	AfterAll(func() {
		_, _ = kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
			"--ignore-not-found", "--wait=true", "--timeout=3m")
		manager := operators3.NewBucketManager(s3Client())
		_, _ = manager.Delete(context.Background(), bucket)
	})

	It("adopts the bucket without claiming ownership", func() {
		waitForSiteReady(siteName, readyTimeout)

		token, err := siteField(siteName, "{.status.bucketOwnership.token}")
		Expect(err).NotTo(HaveOccurred())
		Expect(token).To(BeEmpty(), "an adopted bucket must not get an ownership token")
	})

	It("still publishes the site into it", func() {
		keys := bucketKeys(bucket)
		Expect(keys).To(ContainElement("index.html"))
		Expect(keys).To(ContainElement("pre-existing.txt"),
			"the object that was there first should be untouched")
	})

	It("refuses to delete the bucket when the StaticSite is deleted", func() {
		_, err := kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
			"--wait=true", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())

		Expect(bucketExists(bucket)).To(BeTrue(),
			"a bucket the operator did not create must never be deleted")
		Expect(bucketKeys(bucket)).To(ContainElement("pre-existing.txt"),
			"and its contents must be intact")
	})
})

var _ = Describe("generated bucket names", Ordered, func() {
	// The default path, and the one that depends on the operator reading the
	// cluster's identity at startup -- which works in a unit test and can only
	// really be proven in a cluster.
	const siteName = "derived-name-site"

	imageRef := registryHost + "/derived-site:latest"
	pushRef := localRegistry + "/derived-site:latest"

	var expectedBucket string

	BeforeAll(func() {
		By("computing the name the way the documentation says to")
		clusterUUID, err := kubectl("get", "namespace", "kube-system", "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		Expect(clusterUUID).NotTo(BeEmpty())

		defaults := operatorconfig.New()
		defaults.BucketNameUUIDNamespace = clusterUUID
		opaque, err := defaults.OpaqueSiteID(e2eNamespace, siteName)
		Expect(err).NotTo(HaveOccurred())
		expectedBucket = "static-s3-" + opaque.String()
		_, _ = fmt.Fprintf(GinkgoWriter, "expecting bucket %s\n", expectedBucket)

		By("publishing a site image")
		image, err := buildSiteImage(sampleSite("derived"))
		Expect(err).NotTo(HaveOccurred())
		_, err = pushSiteImage(pushRef, image)
		Expect(err).NotTo(HaveOccurred())

		By("creating a StaticSite that names no bucket")
		applyManifest(fmt.Sprintf(`
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: %s
  namespace: %s
spec:
  source:
    image: %s
    plainHTTP: true
    pullInterval: "0"
  bucket:
    website:
      indexDocument: index.html
  service:
    enabled: false
`, siteName, e2eNamespace, imageRef))
	})

	AfterAll(func() {
		_, _ = kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
			"--ignore-not-found", "--wait=true", "--timeout=3m")
	})

	It("derives the name the documented command produces", func() {
		Eventually(func(g Gomega) {
			reported, err := siteField(siteName, "{.status.bucketName}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(reported).To(Equal(expectedBucket),
				"the operator and the documented derivation must agree")
		}, readyTimeout, 3*time.Second).Should(Succeed())
	})

	It("publishes into that bucket, and leaks nothing into its name", func() {
		waitForSiteReady(siteName, readyTimeout)

		Expect(bucketExists(expectedBucket)).To(BeTrue())
		Expect(bucketKeys(expectedBucket)).To(ContainElement("index.html"))

		// The reason the name is opaque at all: it ends up in public DNS.
		Expect(expectedBucket).NotTo(ContainSubstring(e2eNamespace))
		Expect(expectedBucket).NotTo(ContainSubstring(siteName))

		body, status, _, err := httpGet(objectURL(expectedBucket, "index.html"))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200), "body: %s", body)
		Expect(body).To(ContainSubstring("Sample site derived"))
	})

	It("deletes the derived bucket with the StaticSite", func() {
		_, err := kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
			"--wait=true", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			return bucketExists(expectedBucket)
		}, 3*time.Minute, 3*time.Second).Should(BeFalse())
	})
})

var _ = Describe("failure reporting", func() {
	It("reports an image that does not exist without wedging", func() {
		const siteName = "missing-image-site"

		applyManifest(fmt.Sprintf(`
apiVersion: web.rka.sh/v1alpha1
kind: StaticSite
metadata:
  name: %s
  namespace: %s
spec:
  source:
    image: %s/does-not-exist:nope
    plainHTTP: true
  bucket:
    name: e2e-missing-image
  service:
    enabled: false
`, siteName, e2eNamespace, registryHost))

		DeferCleanup(func() {
			_, _ = kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
				"--ignore-not-found", "--wait=true", "--timeout=3m")
		})

		By("reporting the failure against SourceResolved")
		Eventually(func(g Gomega) {
			status, err := siteField(siteName,
				`{.status.conditions[?(@.type=="SourceResolved")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("False"))
		}, readyTimeout, 3*time.Second).Should(Succeed())

		By("having created the bucket anyway, since that stage succeeded")
		Eventually(func(g Gomega) {
			status, err := siteField(siteName, `{.status.conditions[?(@.type=="BucketReady")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"))
		}, readyTimeout, 3*time.Second).Should(Succeed())

		By("still deleting cleanly")
		_, err := kubectl("delete", "staticsite", siteName, "-n", e2eNamespace,
			"--wait=true", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
	})
})
