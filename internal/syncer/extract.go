/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package syncer extracts a directory out of a container image and mirrors it
// into an S3 bucket. It is used by the syncer binary that the operator runs as a
// Job, and is written so the same code can be exercised directly from tests.
package syncer

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Extraction writes files with fixed permissions rather than the image's own.
// Static site content only ever needs to be read back by this process, and
// honouring arbitrary modes from an untrusted image invites setuid surprises.
const (
	extractedFileMode = 0o644
	extractedDirMode  = 0o755
)

// ExtractOptions configure extraction of one directory from an image.
type ExtractOptions struct {
	// SourcePath is the absolute directory inside the image to extract. Its
	// contents land directly in Dest, so "/site" puts "/site/index.html" at
	// "<Dest>/index.html".
	SourcePath string
	// Dest is the local directory to extract into. It must already exist.
	Dest string
	// MaxBytes caps the total bytes written, guarding against an image built to
	// exhaust the Job's disk.
	MaxBytes int64
	// MaxFiles caps the number of entries written.
	MaxFiles int
	// Log receives a line per skipped entry.
	Log logr.Logger
}

// ExtractStats counts what extraction produced.
type ExtractStats struct {
	Files       int
	Directories int
	Symlinks    int
	Hardlinks   int
	Skipped     int
	Bytes       int64
}

// ErrSourcePathNotFound reports that the image has no such directory, which is
// almost always a mistyped spec.source.path.
var ErrSourcePathNotFound = errors.New("source path not found in image")

// ExtractImagePath flattens the image and writes the contents of
// opts.SourcePath into opts.Dest.
//
// The image is untrusted input, so every entry is checked to land inside Dest
// before anything is written: names that climb out with "..", absolute or
// escaping symlinks, hard links pointing outside the extracted tree, and device
// or socket entries are all skipped rather than trusted. Extraction also stops
// if the image turns out to be larger than the configured limits.
func ExtractImagePath(ctx context.Context, image v1.Image, opts ExtractOptions) (*ExtractStats, error) {
	source := path.Clean("/" + strings.Trim(opts.SourcePath, "/"))
	if opts.Dest == "" {
		return nil, errors.New("extract destination is empty")
	}

	destRoot, err := filepath.Abs(opts.Dest)
	if err != nil {
		return nil, fmt.Errorf("resolve extract destination: %w", err)
	}

	flattened := mutate.Extract(image)
	defer func() { _ = flattened.Close() }()

	stats := &ExtractStats{}
	foundSource := source == "/"
	reader := tar.NewReader(flattened)

	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("read image filesystem: %w", err)
		}

		entry := normalizeTarPath(header.Name)
		if entry == source {
			foundSource = true
			continue
		}

		relative, inside := relativeTo(source, entry)
		if !inside {
			continue
		}
		foundSource = true

		target := filepath.Join(destRoot, filepath.FromSlash(relative))
		if !withinRoot(destRoot, target) {
			stats.Skipped++
			opts.Log.V(1).Info("Skipped image entry that escapes the extract directory", "entry", header.Name)
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, extractedDirMode); err != nil {
				return stats, fmt.Errorf("create directory %q: %w", relative, err)
			}
			stats.Directories++

		case tar.TypeReg:
			if stats.Files >= opts.MaxFiles {
				return stats, fmt.Errorf("image contains more than %d files under %s", opts.MaxFiles, source)
			}
			if stats.Bytes+header.Size > opts.MaxBytes {
				return stats, fmt.Errorf(
					"image contents under %s exceed the %d byte limit", source, opts.MaxBytes)
			}
			written, err := writeFile(target, reader, header.Size)
			if err != nil {
				return stats, err
			}
			stats.Files++
			stats.Bytes += written

		case tar.TypeSymlink:
			if err := writeSymlink(destRoot, target, header.Linkname); err != nil {
				stats.Skipped++
				opts.Log.V(1).Info("Skipped symlink from image",
					"entry", header.Name, "target", header.Linkname, "reason", err.Error())
				continue
			}
			stats.Symlinks++

		case tar.TypeLink:
			linkSource, inside := relativeTo(source, normalizeTarPath(header.Linkname))
			if !inside {
				stats.Skipped++
				opts.Log.V(1).Info("Skipped hard link pointing outside the source path",
					"entry", header.Name, "target", header.Linkname)
				continue
			}
			if err := writeHardlink(filepath.Join(destRoot, filepath.FromSlash(linkSource)), target); err != nil {
				stats.Skipped++
				opts.Log.V(1).Info("Skipped hard link from image",
					"entry", header.Name, "target", header.Linkname, "reason", err.Error())
				continue
			}
			stats.Hardlinks++

		default:
			// Character devices, block devices, FIFOs and sockets cannot be part of a
			// website and are not worth the privileges needed to recreate them.
			stats.Skipped++
			opts.Log.V(1).Info("Skipped unsupported image entry",
				"entry", header.Name, "type", string(header.Typeflag))
		}
	}

	if !foundSource {
		return stats, fmt.Errorf("%w: %s", ErrSourcePathNotFound, source)
	}
	return stats, nil
}

// normalizeTarPath turns a tar entry name into an absolute, cleaned path, so
// that "./usr/share/x", "usr/share/x" and "/usr/share/x" compare equal.
func normalizeTarPath(name string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/"))
}

// relativeTo returns entry's path relative to root, and whether entry is inside
// root at all.
func relativeTo(root, entry string) (string, bool) {
	if root == "/" {
		return strings.TrimPrefix(entry, "/"), entry != "/"
	}
	if !strings.HasPrefix(entry, root+"/") {
		return "", false
	}
	return strings.TrimPrefix(entry, root+"/"), true
}

// withinRoot reports whether target is root itself or below it.
func withinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func writeFile(target string, contents io.Reader, size int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), extractedDirMode); err != nil {
		return 0, fmt.Errorf("create parent directory for %q: %w", target, err)
	}

	// A later layer may replace a path that an earlier one wrote as a symlink or a
	// directory, so clear whatever is there before creating the file.
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		if err := os.RemoveAll(target); err != nil {
			return 0, fmt.Errorf("replace %q: %w", target, err)
		}
	}

	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, extractedFileMode)
	if err != nil {
		return 0, fmt.Errorf("create %q: %w", target, err)
	}
	defer func() { _ = file.Close() }()

	// Copying exactly the declared size means a header that lies about its length
	// cannot be used to write more than the accounting above allowed.
	written, err := io.CopyN(file, contents, size)
	if err != nil && !errors.Is(err, io.EOF) {
		return written, fmt.Errorf("write %q: %w", target, err)
	}
	if err := file.Close(); err != nil {
		return written, fmt.Errorf("close %q: %w", target, err)
	}
	return written, nil
}

// writeSymlink recreates a symlink only when its target stays inside the
// extracted tree. An absolute target refers to the image's filesystem, which
// does not exist here, and a climbing relative target would let a crafted image
// publish files from the Job's own filesystem.
func writeSymlink(destRoot, target, linkname string) error {
	if linkname == "" {
		return errors.New("symlink target is empty")
	}
	if path.IsAbs(linkname) {
		return errors.New("symlink target is absolute")
	}

	resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(linkname))
	if !withinRoot(destRoot, resolved) {
		return errors.New("symlink target escapes the extract directory")
	}

	if err := os.MkdirAll(filepath.Dir(target), extractedDirMode); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("replace existing entry: %w", err)
	}
	return os.Symlink(filepath.FromSlash(linkname), target)
}

func writeHardlink(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), extractedDirMode); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("replace existing entry: %w", err)
	}
	if err := os.Link(source, target); err != nil {
		// Falling back to a copy keeps a site usable on filesystems that refuse
		// hard links, which is more useful than failing the whole sync.
		return copyFile(source, target)
	}
	return nil
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, extractedFileMode)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
