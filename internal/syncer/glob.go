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
	"fmt"
	"regexp"
	"strings"
)

// Glob is a compiled path pattern.
//
// Go's path.Match cannot express "any depth", which is the one thing a static
// site's rules always need ("everything under assets/", "every .woff2
// anywhere"), so patterns are compiled to anchored regular expressions with
// these rules:
//
//	?    one character, not a slash
//	*    any run of characters, not crossing a slash
//	**   any run of characters, crossing slashes
//	**/  zero or more leading directories, so **/*.css matches a.css and x/a.css
//
// Everything else is literal. Matching is case sensitive, because object keys
// are.
type Glob struct {
	pattern string
	re      *regexp.Regexp
}

// CompileGlob compiles a pattern.
func CompileGlob(pattern string) (*Glob, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob pattern is empty")
	}

	var expr strings.Builder
	expr.WriteString(`\A`)

	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			// Zero or more directories, so the pattern also matches at the root.
			expr.WriteString(`(?:[^/]*/)*`)
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			expr.WriteString(`.*`)
			i += 2
		case pattern[i] == '*':
			expr.WriteString(`[^/]*`)
			i++
		case pattern[i] == '?':
			expr.WriteString(`[^/]`)
			i++
		default:
			expr.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		}
	}

	expr.WriteString(`\z`)

	re, err := regexp.Compile(expr.String())
	if err != nil {
		return nil, fmt.Errorf("compile glob %q: %w", pattern, err)
	}
	return &Glob{pattern: pattern, re: re}, nil
}

// Match reports whether path matches the pattern. path is always a
// slash-separated path relative to the site root, never absolute.
func (g *Glob) Match(path string) bool {
	return g.re.MatchString(path)
}

// Pattern returns the pattern the Glob was compiled from.
func (g *Glob) Pattern() string { return g.pattern }

// GlobSet is an ordered list of patterns, used for spec.sync.excludes.
type GlobSet []*Glob

// CompileGlobSet compiles every pattern, reporting the first that is invalid.
func CompileGlobSet(patterns []string) (GlobSet, error) {
	set := make(GlobSet, 0, len(patterns))
	for _, pattern := range patterns {
		glob, err := CompileGlob(pattern)
		if err != nil {
			return nil, err
		}
		set = append(set, glob)
	}
	return set, nil
}

// Matches reports whether any pattern in the set matches path.
func (s GlobSet) Matches(path string) bool {
	for _, glob := range s {
		if glob.Match(path) {
			return true
		}
	}
	return false
}

// Rule is a glob paired with the header value it selects. It mirrors
// v1alpha1.GlobRule without making this package depend on the API types, so the
// sync engine can be driven straight from a test or the syncer's own flags.
type Rule struct {
	Pattern string `json:"pattern"`
	Value   string `json:"value"`
}

// GlobValue is a compiled Rule.
type GlobValue struct {
	Glob  *Glob
	Value string
}

// GlobValues is an ordered rule list where the first match wins, which is what
// spec.sync.cacheControl and spec.sync.contentTypes document.
type GlobValues []GlobValue

// CompileGlobValues compiles rules in order.
func CompileGlobValues(rules []Rule) (GlobValues, error) {
	compiled := make(GlobValues, 0, len(rules))
	for _, rule := range rules {
		glob, err := CompileGlob(rule.Pattern)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, GlobValue{Glob: glob, Value: rule.Value})
	}
	return compiled, nil
}

// Lookup returns the value of the first rule matching path, and whether one did.
func (v GlobValues) Lookup(path string) (string, bool) {
	for _, rule := range v {
		if rule.Glob.Match(path) {
			return rule.Value, true
		}
	}
	return "", false
}
