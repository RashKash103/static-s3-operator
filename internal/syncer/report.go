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
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// MaxTerminationMessageBytes is the size Kubernetes truncates a container's
// termination message to.
const MaxTerminationMessageBytes = 4096

// Report is how a sync Job tells the operator what happened.
//
// It travels through the container's termination message, which the operator
// reads off the Pod status. That keeps the Job free of any Kubernetes API access
// at all: it needs no ServiceAccount token, no RBAC and no network path to the
// API server, which matters because it is the component that handles a
// potentially untrusted image.
type Report struct {
	// Succeeded reports whether the sync completed.
	Succeeded bool `json:"succeeded"`
	// Message describes the failure when Succeeded is false.
	Message string `json:"message,omitempty"`
	// Digest is the image digest that was synced.
	Digest string `json:"digest,omitempty"`

	Uploaded      int    `json:"uploaded"`
	Deleted       int    `json:"deleted"`
	Unchanged     int    `json:"unchanged"`
	Skipped       int    `json:"skipped"`
	BytesUploaded int64  `json:"bytesUploaded"`
	Duration      string `json:"duration,omitempty"`

	// ExtractedFiles is how many files came out of the image.
	ExtractedFiles int `json:"extractedFiles,omitempty"`
	// ExtractedBytes is their total size.
	ExtractedBytes int64 `json:"extractedBytes,omitempty"`
	// ACLDropped reports that the bucket refuses object ACLs, so public access
	// depends on the bucket policy.
	ACLDropped bool `json:"aclDropped,omitempty"`
}

// WriteReport writes r to path, which is normally /dev/termination-log.
//
// The message is truncated to what Kubernetes will keep. A failure to write is
// returned rather than swallowed, but callers treat it as non-fatal: losing the
// report only costs the operator some detail in status, whereas failing the Job
// over it would hide a sync that actually worked.
func WriteReport(path string, report Report) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode sync report: %w", err)
	}

	if len(encoded) > MaxTerminationMessageBytes && report.Message != "" {
		// The message is the only field that can grow without bound, so shrink it
		// until the whole report fits.
		overflow := len(encoded) - MaxTerminationMessageBytes
		trimmed := report
		if overflow < len(report.Message) {
			trimmed.Message = report.Message[:len(report.Message)-overflow-3] + "..."
		} else {
			trimmed.Message = "(truncated)"
		}
		encoded, err = json.Marshal(trimmed)
		if err != nil {
			return fmt.Errorf("encode truncated sync report: %w", err)
		}
	}

	if err := os.WriteFile(path, encoded, 0o644); err != nil { //nolint:gosec // report is not sensitive
		return fmt.Errorf("write sync report to %q: %w", path, err)
	}
	return nil
}

// ParseReport reads a report out of a container termination message. It returns
// nil without an error for an empty message, since a Pod that was evicted or
// OOM killed never wrote one.
func ParseReport(message string) (*Report, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, nil
	}

	var report Report
	if err := json.Unmarshal([]byte(message), &report); err != nil {
		return nil, fmt.Errorf("parse sync report: %w", err)
	}
	return &report, nil
}
