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
	"mime"
	"path"
	"strings"
)

// Content types used by more than one extension.
const (
	htmlType       = "text/html; charset=utf-8"
	javascriptType = "text/javascript; charset=utf-8"
	jsonType       = "application/json; charset=utf-8"
)

// Content types are decided from the file extension alone, never by sniffing
// the file's contents. That is deliberate: sniffing is what makes a static site
// upload non-reproducible, since the answer depends on which magic database
// happens to be installed.
//
// This table is consulted before Go's mime.TypeByExtension for the same reason.
// Go's table is seeded from /etc/mime.types, so the same site uploaded from two
// machines could otherwise end up with different Content-Type headers, and a
// missing entry for something as ordinary as .webp or .wasm would leave a file
// unusable in a browser.
var webContentTypes = map[string]string{
	// Documents
	".html":  htmlType,
	".htm":   htmlType,
	".xhtml": "application/xhtml+xml; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".csv":   "text/csv; charset=utf-8",
	".xml":   "application/xml; charset=utf-8",
	".pdf":   "application/pdf",

	// Scripts and data
	".js":          javascriptType,
	".mjs":         javascriptType,
	".cjs":         javascriptType,
	".json":        jsonType,
	".jsonld":      "application/ld+json; charset=utf-8",
	".map":         jsonType,
	".wasm":        "application/wasm",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".rss":         "application/rss+xml; charset=utf-8",
	".atom":        "application/atom+xml; charset=utf-8",

	// Images
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".svg":  "image/svg+xml; charset=utf-8",
	".ico":  "image/x-icon",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",

	// Fonts
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",

	// Media
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".ogv":  "video/ogg",
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".vtt":  "text/vtt; charset=utf-8",

	// Archives
	".zip": "application/zip",
	".gz":  "application/gzip",
	".br":  "application/x-brotli",
	".bz2": "application/x-bzip2",
	".xz":  "application/x-xz",
	".tar": "application/x-tar",
}

// contentTypeForKey returns the Content-Type for an object key, or "" when the
// extension is unknown.
func contentTypeForKey(key string) string {
	ext := strings.ToLower(path.Ext(key))
	if ext == "" {
		return ""
	}
	if contentType, ok := webContentTypes[ext]; ok {
		return contentType
	}
	return mime.TypeByExtension(ext)
}
