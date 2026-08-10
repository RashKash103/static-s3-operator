/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package syncer

import "testing"

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		matches []string
		misses  []string
	}{
		{
			name:    "single star does not cross directories",
			pattern: "*.html",
			matches: []string{"index.html", "404.html"},
			misses:  []string{"docs/index.html", "index.htm", "index.html.gz"},
		},
		{
			name:    "prefix with double star matches any depth below it",
			pattern: "assets/**",
			matches: []string{"assets/app.css", "assets/img/logo.svg", "assets/a/b/c/d.js"},
			misses:  []string{"assets", "static/assets/app.css"},
		},
		{
			name:    "leading double-star slash also matches at the root",
			pattern: "**/*.woff2",
			matches: []string{"font.woff2", "assets/font.woff2", "a/b/font.woff2"},
			misses:  []string{"font.woff", "woff2"},
		},
		{
			name:    "question mark matches one non-slash character",
			pattern: "page-?.html",
			matches: []string{"page-1.html", "page-a.html"},
			misses:  []string{"page-.html", "page-10.html", "page-/.html"},
		},
		{
			name:    "literal characters that are regex metacharacters are escaped",
			pattern: "report(final).pdf",
			matches: []string{"report(final).pdf"},
			misses:  []string{"reportfinal.pdf", "reportXfinalY.pdf"},
		},
		{
			name:    "double star in the middle crosses directories",
			pattern: "docs/**/index.html",
			matches: []string{"docs/a/index.html", "docs/a/b/index.html"},
			misses:  []string{"docs/index.htm"},
		},
		{
			name:    "matching is case sensitive because object keys are",
			pattern: "*.html",
			matches: []string{"index.html"},
			misses:  []string{"index.HTML"},
		},
		{
			name:    "exact literal matches only itself",
			pattern: "robots.txt",
			matches: []string{"robots.txt"},
			misses:  []string{"a/robots.txt", "robots.txt.bak"},
		},
		{
			// Without this, a rule written the obvious way silently matches nothing:
			// the pattern would only match a file literally called "{woff,woff2}".
			name:    "brace alternatives",
			pattern: "**/*.{woff,woff2}",
			matches: []string{"font.woff", "font.woff2", "assets/font.woff", "a/b/font.woff2"},
			misses:  []string{"font.ttf", "font.{woff,woff2}", "woff"},
		},
		{
			name:    "alternatives may themselves be patterns",
			pattern: "{assets,static}/**",
			matches: []string{"assets/app.css", "static/img/logo.svg"},
			misses:  []string{"public/app.css", "assets", "other/assets/app.css"},
		},
		{
			name:    "nested alternatives",
			pattern: "*.{png,jp{g,eg}}",
			matches: []string{"a.png", "a.jpg", "a.jpeg"},
			misses:  []string{"a.gif", "a.jp"},
		},
		{
			name:    "an empty alternative matches nothing extra",
			pattern: "index{,.html}",
			matches: []string{"index", "index.html"},
			misses:  []string{"index.htm"},
		},
		{
			name:    "an unmatched brace is literal",
			pattern: "weird{name.txt",
			matches: []string{"weird{name.txt"},
			misses:  []string{"weirdname.txt"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			glob, err := CompileGlob(test.pattern)
			if err != nil {
				t.Fatalf("CompileGlob(%q) returned an error: %v", test.pattern, err)
			}

			for _, path := range test.matches {
				if !glob.Match(path) {
					t.Errorf("pattern %q should match %q", test.pattern, path)
				}
			}
			for _, path := range test.misses {
				if glob.Match(path) {
					t.Errorf("pattern %q should not match %q", test.pattern, path)
				}
			}
		})
	}
}

func TestCompileGlobRejectsEmptyPattern(t *testing.T) {
	if _, err := CompileGlob(""); err == nil {
		t.Fatal("CompileGlob(\"\") should return an error")
	}
}

func TestGlobDoesNotMatchPartialPath(t *testing.T) {
	// The pattern is anchored at both ends. Without that, "assets/app" would match
	// "assets/app.css.map" and a Cache-Control rule would leak onto files it was
	// never meant to cover.
	glob, err := CompileGlob("assets/app.css")
	if err != nil {
		t.Fatalf("CompileGlob returned an error: %v", err)
	}
	if glob.Match("assets/app.css.map") {
		t.Error("pattern should not match a longer path with the same prefix")
	}
	if glob.Match("public/assets/app.css") {
		t.Error("pattern should not match a path with an extra leading segment")
	}
}

func TestGlobSetMatches(t *testing.T) {
	set, err := CompileGlobSet([]string{"*.map", "private/**"})
	if err != nil {
		t.Fatalf("CompileGlobSet returned an error: %v", err)
	}

	for _, path := range []string{"app.map", "private/notes.txt", "private/a/b.txt"} {
		if !set.Matches(path) {
			t.Errorf("set should match %q", path)
		}
	}
	for _, path := range []string{"app.js", "public/notes.txt"} {
		if set.Matches(path) {
			t.Errorf("set should not match %q", path)
		}
	}
}

func TestGlobSetOnEmptyListMatchesNothing(t *testing.T) {
	set, err := CompileGlobSet(nil)
	if err != nil {
		t.Fatalf("CompileGlobSet(nil) returned an error: %v", err)
	}
	if set.Matches("index.html") {
		t.Error("an empty exclude list should match nothing")
	}
}

func TestGlobBraceAlternativesInACacheControlRule(t *testing.T) {
	// The shape a real cache policy is written in, and the one that used to match
	// nothing at all.
	rules, err := CompileGlobValues([]Rule{
		{Pattern: "**/*.{woff,woff2,ttf,otf}", Value: "public, max-age=31536000, immutable"},
		{Pattern: "**/*.{html,htm}", Value: "public, max-age=0, must-revalidate"},
	})
	if err != nil {
		t.Fatalf("CompileGlobValues returned an error: %v", err)
	}

	immutable := map[string]bool{
		"assets/inter.woff2": true,
		"fonts/mono.woff":    true,
		"fonts/serif.ttf":    true,
		"index.html":         false,
		"docs/page.htm":      false,
	}
	for path, wantImmutable := range immutable {
		value, ok := rules.Lookup(path)
		if !ok {
			t.Errorf("no rule matched %q", path)
			continue
		}
		isImmutable := value == "public, max-age=31536000, immutable"
		if isImmutable != wantImmutable {
			t.Errorf("Lookup(%q) = %q", path, value)
		}
	}

	if value, ok := rules.Lookup("robots.txt"); ok {
		t.Errorf("robots.txt should match no rule, got %q", value)
	}
}

func TestGlobValuesFirstMatchWins(t *testing.T) {
	// Rule order is the whole contract of spec.sync.cacheControl: a specific rule
	// listed before a broad one has to win, or every file would get the broad
	// value.
	rules, err := CompileGlobValues([]Rule{
		{Pattern: "assets/**", Value: "public, max-age=31536000, immutable"},
		{Pattern: "**/*.html", Value: "public, max-age=0, must-revalidate"},
		{Pattern: "**", Value: "public, max-age=3600"},
	})
	if err != nil {
		t.Fatalf("CompileGlobValues returned an error: %v", err)
	}

	tests := map[string]string{
		"assets/app.css": "public, max-age=31536000, immutable",
		"index.html":     "public, max-age=0, must-revalidate",
		"docs/page.html": "public, max-age=0, must-revalidate",
		"robots.txt":     "public, max-age=3600",
	}
	for path, want := range tests {
		got, ok := rules.Lookup(path)
		if !ok {
			t.Errorf("no rule matched %q", path)
			continue
		}
		if got != want {
			t.Errorf("Lookup(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestGlobValuesLookupReportsNoMatch(t *testing.T) {
	rules, err := CompileGlobValues([]Rule{{Pattern: "assets/**", Value: "immutable"}})
	if err != nil {
		t.Fatalf("CompileGlobValues returned an error: %v", err)
	}
	if value, ok := rules.Lookup("index.html"); ok {
		t.Errorf("Lookup should report no match, got %q", value)
	}
}

func TestCompileGlobValuesReportsBadPattern(t *testing.T) {
	if _, err := CompileGlobValues([]Rule{{Pattern: "", Value: "x"}}); err == nil {
		t.Fatal("an empty pattern should be rejected")
	}
}
