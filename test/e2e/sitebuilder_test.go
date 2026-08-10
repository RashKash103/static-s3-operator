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
	"archive/tar"
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// sitePath is where the sample site lives inside the image, matching the
// operator's default source path so the test exercises that default.
const sitePath = "usr/share/nginx/html"

// sampleSite is a small static website: a page, a nested page, an error page,
// hashed assets, and a file that should be excluded from the upload.
//
// It is generated here rather than committed as fixtures so the suite can publish
// a second version with a file changed, a file removed and a file added, which is
// the only way to test that the bucket is mirrored rather than merely filled.
func sampleSite(version string) map[string]string {
	return map[string]string{
		"index.html": fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Sample site %s</title>
  <link rel="stylesheet" href="/assets/app.css">
</head>
<body>
  <h1>Sample site %s</h1>
  <p>Served from an S3 bucket by static-s3-operator.</p>
  <a href="/about/">About</a>
  <script src="/assets/app.js"></script>
</body>
</html>
`, version, version),

		"about/index.html": fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>About %s</title></head>
<body><h1>About</h1><p>Version %s.</p></body></html>
`, version, version),

		"404.html": `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Not found</title></head>
<body><h1>404</h1></body></html>
`,

		"assets/app.css": fmt.Sprintf("/* %s */\nbody{font-family:system-ui;margin:2rem}\n", version),
		"assets/app.js":  fmt.Sprintf("console.log(%q);\n", "sample site "+version),
		// A source map, which the test excludes to check that spec.sync.excludes
		// keeps a file out of the bucket.
		"assets/app.js.map": `{"version":3,"sources":["app.js"]}`,

		"robots.txt": "User-agent: *\nAllow: /\n",
	}
}

// sampleSiteV2 is the second publish: index.html and app.css change, a new page
// appears, and 404.html is gone.
func sampleSiteV2() map[string]string {
	files := sampleSite("v2")
	delete(files, "404.html")
	files["new-page.html"] = "<!doctype html><html lang=\"en\"><body><h1>New in v2</h1></body></html>\n"
	return files
}

// buildSiteImage packages a set of files into a single-layer image with the site
// at sitePath.
//
// The image is assembled in Go rather than with `docker build` so the suite needs
// no container builder, and so the bytes are deterministic for a given set of
// files.
func buildSiteImage(files map[string]string) (v1.Image, error) {
	buffer := &bytes.Buffer{}
	writer := tar.NewWriter(buffer)

	// Directory entries first, then files, both in a stable order so the layer is
	// reproducible.
	for _, dir := range directoriesFor(files) {
		if err := writer.WriteHeader(&tar.Header{
			Name:     dir + "/",
			Typeflag: tar.TypeDir,
			Mode:     0o755,
		}); err != nil {
			return nil, fmt.Errorf("write directory %q: %w", dir, err)
		}
	}

	names := make([]string, 0, len(files))
	for relative := range files {
		names = append(names, relative)
	}
	sort.Strings(names)

	for _, relative := range names {
		contents := files[relative]
		if err := writer.WriteHeader(&tar.Header{
			Name:     path.Join(sitePath, relative),
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(contents)),
		}); err != nil {
			return nil, fmt.Errorf("write header for %q: %w", relative, err)
		}
		if _, err := writer.Write([]byte(contents)); err != nil {
			return nil, fmt.Errorf("write %q: %w", relative, err)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}

	layer, err := tarball.LayerFromReader(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("build layer: %w", err)
	}

	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, fmt.Errorf("append layer: %w", err)
	}
	return mutate.ConfigFile(image, &v1.ConfigFile{
		Architecture: "amd64",
		OS:           "linux",
		Config:       v1.Config{Labels: map[string]string{"org.opencontainers.image.title": "sample-site"}},
	})
}

// directoriesFor lists every ancestor directory of the given files, shortest
// first, so a tar reader always sees a directory before its contents.
func directoriesFor(files map[string]string) []string {
	seen := map[string]bool{}
	for relative := range files {
		current := path.Join(sitePath, relative)
		for {
			current = path.Dir(current)
			if current == "." || current == "/" || seen[current] {
				break
			}
			seen[current] = true
		}
	}

	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if depth(dirs[i]) != depth(dirs[j]) {
			return depth(dirs[i]) < depth(dirs[j])
		}
		return dirs[i] < dirs[j]
	})
	return dirs
}

func depth(dir string) int { return strings.Count(dir, "/") }

// pushSiteImage publishes an image to the registry and returns its digest.
func pushSiteImage(reference string, image v1.Image) (string, error) {
	ref, err := name.ParseReference(reference, name.Insecure)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", reference, err)
	}
	if err := remote.Write(ref, image); err != nil {
		return "", fmt.Errorf("push %q: %w", reference, err)
	}

	digest, err := image.Digest()
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	return digest.String(), nil
}
