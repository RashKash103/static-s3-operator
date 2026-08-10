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
	"context"
	"crypto/md5" //nolint:gosec // matches the S3 ETag algorithm, not used for security
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rashkash103/static-s3-operator/internal/s3"
)

// contentMD5Metadata is the user metadata key the syncer stores its own content
// hash under, sent as "x-amz-meta-static-s3-md5".
//
// It exists because an object uploaded in several parts has an ETag that is not
// the content's MD5, so ETag comparison alone cannot tell whether a large file
// changed. Recording the hash makes the comparison exact for every object,
// whatever the provider does with ETags.
const contentMD5Metadata = "static-s3-md5"

// LocalFile is one file staged for upload.
type LocalFile struct {
	// Key is the full object key, including spec.sync.keyPrefix.
	Key string
	// RelPath is the path relative to the site root, which is what
	// spec.sync.excludes, cacheControl and contentTypes patterns match against.
	RelPath string
	// Path is where the file lives on the sync Job's disk.
	Path string
	// Size is the file's size in bytes.
	Size int64
	// MD5 is the hex-encoded MD5 of the file's contents.
	MD5 string
}

// RemoteObject is one object already in the bucket.
type RemoteObject struct {
	Key  string
	Size int64
	ETag string
}

// Plan is the work one sync will do.
type Plan struct {
	// Uploads are files to create or replace, in a stable order.
	Uploads []LocalFile
	// Deletes are object keys to remove because they are no longer in the image.
	Deletes []string
	// Unchanged counts objects already up to date.
	Unchanged int
	// Skipped counts local files excluded by spec.sync.excludes.
	Skipped int
	// UploadBytes is the total size of Uploads.
	UploadBytes int64
}

// scanLocal walks the staged site and hashes every file that is not excluded.
//
// Hashing the whole tree costs one read per file, which is what makes the
// comparison against the bucket exact rather than a guess from size and
// timestamps. Timestamps are useless here anyway: files extracted from an image
// all carry the image's build time, not the time their contents last changed.
func scanLocal(root, keyPrefix string, excludes GlobSet) ([]LocalFile, int, error) {
	var files []LocalFile
	skipped := 0

	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relPath := filepath.ToSlash(relative)

		info, isFile, err := describeEntry(current, entry)
		if err != nil {
			return err
		}
		if !isFile {
			return nil
		}

		if excludes.Matches(relPath) {
			skipped++
			return nil
		}

		sum, err := hashFile(current)
		if err != nil {
			return err
		}

		files = append(files, LocalFile{
			Key:     keyPrefix + relPath,
			RelPath: relPath,
			Path:    current,
			Size:    info.Size(),
			MD5:     sum,
		})
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan staged site: %w", err)
	}

	// A stable order makes logs and test expectations reproducible.
	sort.Slice(files, func(i, j int) bool { return files[i].Key < files[j].Key })
	return files, skipped, nil
}

// describeEntry decides whether a walked entry is a file worth uploading,
// resolving symlinks to whatever they point at. A symlink to a directory is
// ignored rather than followed, since a cycle would make the walk run forever.
func describeEntry(path string, entry fs.DirEntry) (os.FileInfo, bool, error) {
	if entry.IsDir() {
		return nil, false, nil
	}

	if entry.Type()&fs.ModeSymlink != 0 {
		info, err := os.Stat(path)
		if err != nil {
			// A symlink whose target was skipped during extraction is expected, not
			// an error worth failing the sync over.
			return nil, false, nil //nolint:nilerr // dangling links are ignored on purpose
		}
		return info, info.Mode().IsRegular(), nil
	}

	info, err := entry.Info()
	if err != nil {
		return nil, false, err
	}
	return info, info.Mode().IsRegular(), nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	digest := md5.New() //nolint:gosec // matches the S3 ETag algorithm
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// listRemote lists every object under prefix.
func listRemote(ctx context.Context, client *awss3.Client, bucket, prefix string) (map[string]RemoteObject, error) {
	input := &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	objects := map[string]RemoteObject{}
	paginator := awss3.NewListObjectsV2Paginator(client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects in %q: %w", bucket, err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			objects[key] = RemoteObject{
				Key:  key,
				Size: aws.ToInt64(object.Size),
				ETag: strings.Trim(aws.ToString(object.ETag), `"`),
			}
		}
	}
	return objects, nil
}

// buildPlan works out which files to upload and which objects to remove.
func buildPlan(
	ctx context.Context,
	client *awss3.Client,
	bucket string,
	local []LocalFile,
	remote map[string]RemoteObject,
	skipped int,
	deleteRemoved bool,
	excludes GlobSet,
	keyPrefix string,
) (*Plan, error) {
	plan := &Plan{Skipped: skipped}
	localKeys := make(map[string]struct{}, len(local))

	for _, file := range local {
		localKeys[file.Key] = struct{}{}

		existing, found := remote[file.Key]
		if !found {
			plan.Uploads = append(plan.Uploads, file)
			plan.UploadBytes += file.Size
			continue
		}

		same, err := objectMatches(ctx, client, bucket, file, existing)
		if err != nil {
			return nil, err
		}
		if same {
			plan.Unchanged++
			continue
		}
		plan.Uploads = append(plan.Uploads, file)
		plan.UploadBytes += file.Size
	}

	if deleteRemoved {
		for key := range remote {
			if _, stillPresent := localKeys[key]; stillPresent {
				continue
			}
			// The ownership marker is the operator's own bookkeeping and is never part
			// of the site, so a mirror sync must not remove it.
			if key == s3.MarkerKey {
				continue
			}
			// An excluded file was never uploaded, so an object matching an exclude
			// belongs to whoever put it there.
			if excludes.Matches(strings.TrimPrefix(key, keyPrefix)) {
				continue
			}
			plan.Deletes = append(plan.Deletes, key)
		}
		sort.Strings(plan.Deletes)
	}

	return plan, nil
}

// objectMatches reports whether the bucket already holds this exact content.
//
// A single-part upload has an ETag equal to the content's MD5, which settles it
// without another request. A multipart ETag says nothing about the content, so
// the stored md5 metadata is fetched instead; an object uploaded by something
// other than this syncer will not have it, and is re-uploaded.
func objectMatches(
	ctx context.Context,
	client *awss3.Client,
	bucket string,
	local LocalFile,
	remote RemoteObject,
) (bool, error) {
	if remote.Size != local.Size {
		return false, nil
	}

	if isPlainMD5ETag(remote.ETag) {
		return strings.EqualFold(remote.ETag, local.MD5), nil
	}

	head, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(remote.Key),
	})
	if err != nil {
		if s3.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("head object %q in %q: %w", remote.Key, bucket, err)
	}

	stored, ok := head.Metadata[contentMD5Metadata]
	if !ok {
		return false, nil
	}
	return strings.EqualFold(stored, local.MD5), nil
}

// isPlainMD5ETag reports whether an ETag is a bare MD5 hex digest, as opposed to
// the "<hash>-<parts>" form a multipart upload produces.
func isPlainMD5ETag(etag string) bool {
	if len(etag) != 32 {
		return false
	}
	for _, char := range etag {
		isHex := (char >= '0' && char <= '9') ||
			(char >= 'a' && char <= 'f') ||
			(char >= 'A' && char <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
