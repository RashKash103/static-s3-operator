/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package syncer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// tarEntry is one file, directory or link in a synthetic image layer.
type tarEntry struct {
	name     string
	typeflag byte
	contents string
	linkname string
	mode     int64
	// size overrides the declared size, for testing a header that lies.
	size *int64
}

func file(name, contents string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, contents: contents, mode: 0o644}
}

func dir(name string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeDir, mode: 0o755}
}

func symlink(name, target string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeSymlink, linkname: target, mode: 0o777}
}

// buildImage assembles an image whose layers hold the given entries. Each layer
// is applied on top of the previous one, so a later layer can replace a path an
// earlier one wrote, which is what the real flattening has to cope with.
func buildImage(t *testing.T, layers ...[]tarEntry) v1.Image {
	t.Helper()

	image := empty.Image
	for _, entries := range layers {
		buffer := &bytes.Buffer{}
		writer := tar.NewWriter(buffer)

		for _, entry := range entries {
			size := int64(len(entry.contents))
			if entry.size != nil {
				size = *entry.size
			}
			header := &tar.Header{
				Name:     entry.name,
				Typeflag: entry.typeflag,
				Mode:     entry.mode,
				Linkname: entry.linkname,
				Size:     size,
			}
			if entry.typeflag != tar.TypeReg {
				header.Size = 0
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatalf("write tar header for %q: %v", entry.name, err)
			}
			if entry.typeflag == tar.TypeReg && entry.contents != "" {
				if _, err := writer.Write([]byte(entry.contents)); err != nil {
					t.Fatalf("write tar body for %q: %v", entry.name, err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close tar writer: %v", err)
		}

		layerBytes := buffer.Bytes()
		built, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(layerBytes)), nil
		})
		if err != nil {
			t.Fatalf("build layer: %v", err)
		}
		image, err = mutate.AppendLayers(image, built)
		if err != nil {
			t.Fatalf("append layer: %v", err)
		}
	}
	return image
}

func extract(t *testing.T, image v1.Image, sourcePath string, opts ...func(*ExtractOptions)) (string, *ExtractStats, error) {
	t.Helper()

	dest := t.TempDir()
	options := ExtractOptions{
		SourcePath: sourcePath,
		Dest:       dest,
		MaxBytes:   1 << 20,
		MaxFiles:   1000,
		Log:        testr.New(t),
	}
	for _, apply := range opts {
		apply(&options)
	}

	stats, err := ExtractImagePath(context.Background(), image, options)
	return dest, stats, err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(contents)
}

func TestExtractImagePathCopiesDirectoryContents(t *testing.T) {
	image := buildImage(t, []tarEntry{
		dir("usr"),
		dir("usr/share"),
		dir("usr/share/nginx"),
		dir("usr/share/nginx/html"),
		file("usr/share/nginx/html/index.html", "<h1>home</h1>"),
		dir("usr/share/nginx/html/assets"),
		file("usr/share/nginx/html/assets/app.css", "body{}"),
		// Outside the source path, so it must not be extracted.
		file("etc/passwd", "root:x:0:0"),
	})

	dest, stats, err := extract(t, image, "/usr/share/nginx/html")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}

	if got := readFile(t, filepath.Join(dest, "index.html")); got != "<h1>home</h1>" {
		t.Errorf("index.html = %q", got)
	}
	if got := readFile(t, filepath.Join(dest, "assets", "app.css")); got != "body{}" {
		t.Errorf("assets/app.css = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "etc")); !os.IsNotExist(err) {
		t.Error("files outside the source path should not be extracted")
	}
	if stats.Files != 2 {
		t.Errorf("stats.Files = %d, want 2", stats.Files)
	}
	if stats.Bytes != int64(len("<h1>home</h1>")+len("body{}")) {
		t.Errorf("stats.Bytes = %d", stats.Bytes)
	}
}

func TestExtractImagePathAcceptsRootAsSourcePath(t *testing.T) {
	image := buildImage(t, []tarEntry{file("index.html", "root site")})

	dest, _, err := extract(t, image, "/")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}
	if got := readFile(t, filepath.Join(dest, "index.html")); got != "root site" {
		t.Errorf("index.html = %q", got)
	}
}

func TestExtractImagePathNormalizesEntryNames(t *testing.T) {
	// Real layers spell the same path as "./site/x", "site/x" and "/site/x".
	image := buildImage(t, []tarEntry{
		dir("./site"),
		file("./site/index.html", "dot slash"),
		file("/site/other.html", "leading slash"),
	})

	dest, _, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}
	if got := readFile(t, filepath.Join(dest, "index.html")); got != "dot slash" {
		t.Errorf("index.html = %q", got)
	}
	if got := readFile(t, filepath.Join(dest, "other.html")); got != "leading slash" {
		t.Errorf("other.html = %q", got)
	}
}

func TestExtractImagePathAppliesLaterLayers(t *testing.T) {
	image := buildImage(t,
		[]tarEntry{dir("site"), file("site/index.html", "first")},
		[]tarEntry{file("site/index.html", "second")},
	)

	dest, _, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}
	if got := readFile(t, filepath.Join(dest, "index.html")); got != "second" {
		t.Errorf("index.html = %q, want the later layer's contents", got)
	}
}

func TestExtractImagePathFailsWhenSourcePathIsAbsent(t *testing.T) {
	image := buildImage(t, []tarEntry{file("usr/share/nginx/html/index.html", "hello")})

	_, _, err := extract(t, image, "/site")
	if !errors.Is(err, ErrSourcePathNotFound) {
		t.Fatalf("ExtractImagePath error = %v, want ErrSourcePathNotFound", err)
	}
}

func TestExtractImagePathSkipsEntriesThatEscapeTheDestination(t *testing.T) {
	// A crafted layer naming "site/../../etc/cron.d/x" must not be able to write
	// outside the staging directory.
	image := buildImage(t, []tarEntry{
		dir("site"),
		file("site/index.html", "safe"),
		{name: "site/../../../../etc/evil", typeflag: tar.TypeReg, contents: "pwned", mode: 0o644},
	})

	dest, _, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}

	if got := readFile(t, filepath.Join(dest, "index.html")); got != "safe" {
		t.Errorf("index.html = %q", got)
	}
	// path.Clean resolves the traversal before the prefix check, so the entry no
	// longer looks like it is under the source path and is simply not extracted.
	if err := filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(path, "evil") {
			t.Errorf("a traversing entry was extracted to %q", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk destination: %v", err)
	}
}

func TestExtractImagePathSkipsAbsoluteSymlinks(t *testing.T) {
	image := buildImage(t, []tarEntry{
		dir("site"),
		file("site/index.html", "safe"),
		symlink("site/passwd", "/etc/passwd"),
		symlink("site/inside", "index.html"),
	})

	dest, stats, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}

	// An absolute target refers to the image's filesystem, which does not exist
	// here, so following it would either dangle or point at the Job's own files.
	if _, err := os.Lstat(filepath.Join(dest, "passwd")); !os.IsNotExist(err) {
		t.Error("an absolute symlink should be skipped")
	}
	if _, err := os.Lstat(filepath.Join(dest, "inside")); err != nil {
		t.Errorf("a symlink pointing inside the tree should be kept: %v", err)
	}
	if stats.Symlinks != 1 {
		t.Errorf("stats.Symlinks = %d, want 1", stats.Symlinks)
	}
	if stats.Skipped != 1 {
		t.Errorf("stats.Skipped = %d, want 1", stats.Skipped)
	}
}

// TestWriteSymlinkGuardsTheDestination exercises the containment check directly.
//
// go-containerregistry drops climbing symlinks while it flattens an image, so a
// crafted layer does not reach the extractor in practice. The guard is still the
// thing that has to hold if that ever changes, and only a direct test can prove
// it does.
func TestWriteSymlinkGuardsTheDestination(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name     string
		relative string
		linkname string
		wantErr  bool
	}{
		{name: "sibling target", relative: "inside", linkname: "index.html"},
		{name: "target in a subdirectory", relative: "a/link", linkname: "../index.html"},
		{name: "absolute target", relative: "passwd", linkname: "/etc/passwd", wantErr: true},
		{name: "climbing target", relative: "escape", linkname: "../../../etc/shadow", wantErr: true},
		{name: "deeply climbing target", relative: "a/b/escape", linkname: "../../../../x", wantErr: true},
		{name: "empty target", relative: "empty", linkname: "", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := filepath.Join(root, test.relative)
			err := writeSymlink(root, target, test.linkname)

			if test.wantErr {
				if err == nil {
					t.Fatalf("writeSymlink(%q -> %q) should have been refused", test.relative, test.linkname)
				}
				if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
					t.Errorf("a refused symlink should not exist on disk")
				}
				return
			}

			if err != nil {
				t.Fatalf("writeSymlink(%q -> %q) returned an error: %v", test.relative, test.linkname, err)
			}
			if _, statErr := os.Lstat(target); statErr != nil {
				t.Errorf("an accepted symlink should exist on disk: %v", statErr)
			}
		})
	}
}

func TestExtractImagePathSkipsDeviceEntries(t *testing.T) {
	image := buildImage(t, []tarEntry{
		dir("site"),
		file("site/index.html", "safe"),
		{name: "site/null", typeflag: tar.TypeChar, mode: 0o666},
		{name: "site/pipe", typeflag: tar.TypeFifo, mode: 0o666},
	})

	dest, stats, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "null")); !os.IsNotExist(err) {
		t.Error("a character device should be skipped")
	}
	if stats.Skipped != 2 {
		t.Errorf("stats.Skipped = %d, want 2", stats.Skipped)
	}
}

func TestExtractImagePathEnforcesTheByteLimit(t *testing.T) {
	image := buildImage(t, []tarEntry{
		dir("site"),
		file("site/big.bin", strings.Repeat("x", 4096)),
	})

	_, _, err := extract(t, image, "/site", func(o *ExtractOptions) { o.MaxBytes = 1024 })
	if err == nil {
		t.Fatal("extraction should fail once the byte limit is exceeded")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error = %v, want it to mention the byte limit", err)
	}
}

func TestExtractImagePathEnforcesTheFileLimit(t *testing.T) {
	entries := make([]tarEntry, 0, 6)
	entries = append(entries, dir("site"))
	for i := range 5 {
		entries = append(entries, file("site/page"+string(rune('a'+i))+".html", "x"))
	}
	image := buildImage(t, entries)

	_, _, err := extract(t, image, "/site", func(o *ExtractOptions) { o.MaxFiles = 3 })
	if err == nil {
		t.Fatal("extraction should fail once the file limit is exceeded")
	}
	if !strings.Contains(err.Error(), "more than 3 files") {
		t.Errorf("error = %v, want it to mention the file limit", err)
	}
}

func TestExtractImagePathWritesFixedPermissions(t *testing.T) {
	// Honouring modes from an untrusted image is how a setuid bit ends up on disk,
	// so extraction always writes the same permissions.
	image := buildImage(t, []tarEntry{
		dir("site"),
		{name: "site/script.sh", typeflag: tar.TypeReg, contents: "#!/bin/sh", mode: 0o4777},
	})

	dest, _, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, "script.sh"))
	if err != nil {
		t.Fatalf("stat extracted file: %v", err)
	}
	if got := info.Mode().Perm(); got != extractedFileMode {
		t.Errorf("mode = %o, want %o", got, extractedFileMode)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Error("the setuid bit must not survive extraction")
	}
}

func TestExtractImagePathReplacesASymlinkWithALaterRegularFile(t *testing.T) {
	image := buildImage(t,
		[]tarEntry{dir("site"), symlink("site/index.html", "other.html")},
		[]tarEntry{file("site/index.html", "real file")},
	)

	dest, _, err := extract(t, image, "/site")
	if err != nil {
		t.Fatalf("ExtractImagePath returned an error: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("index.html should be a regular file, got mode %v", info.Mode())
	}
	if got := readFile(t, filepath.Join(dest, "index.html")); got != "real file" {
		t.Errorf("index.html = %q", got)
	}
}

func TestExtractImagePathRejectsAnEmptyDestination(t *testing.T) {
	image := buildImage(t, []tarEntry{file("site/index.html", "x")})

	_, err := ExtractImagePath(context.Background(), image, ExtractOptions{
		SourcePath: "/site",
		Dest:       "",
		MaxBytes:   1 << 20,
		MaxFiles:   10,
		Log:        testr.New(t),
	})
	if err == nil {
		t.Fatal("an empty destination should be rejected")
	}
}

func TestRelativeTo(t *testing.T) {
	tests := []struct {
		root, entry string
		want        string
		inside      bool
	}{
		{"/site", "/site/index.html", "index.html", true},
		{"/site", "/site/a/b.html", "a/b.html", true},
		{"/site", "/site", "", false},
		{"/site", "/sitemap.xml", "", false},
		{"/site", "/other/index.html", "", false},
		{"/", "/index.html", "index.html", true},
		{"/", "/", "", false},
	}

	for _, test := range tests {
		got, inside := relativeTo(test.root, test.entry)
		if inside != test.inside || got != test.want {
			t.Errorf("relativeTo(%q, %q) = (%q, %v), want (%q, %v)",
				test.root, test.entry, got, inside, test.want, test.inside)
		}
	}
}

func TestNormalizeTarPath(t *testing.T) {
	tests := map[string]string{
		"usr/share/x":     "/usr/share/x",
		"./usr/share/x":   "/usr/share/x",
		"/usr/share/x":    "/usr/share/x",
		"usr/share/../x":  "/usr/x",
		"./":              "/",
		"site/../../evil": "/evil",
	}

	for input, want := range tests {
		if got := normalizeTarPath(input); got != want {
			t.Errorf("normalizeTarPath(%q) = %q, want %q", input, got, want)
		}
	}
}
