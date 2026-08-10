/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Command helmsync copies controller-gen's output into the Helm chart's templates.
//
// The Kubebuilder Helm plugin writes those templates, but only when the whole chart
// is regenerated -- and regenerating the whole chart overwrites the customisations
// this project keeps in it. So anything generated from the code has to be copied
// into the chart separately, or the chart quietly falls behind the operator.
//
// That has happened twice. First a chart shipping a CRD that still required a field
// the API had made optional, so installing the chart rejected a valid manifest.
// Then a manager role missing a permission the operator had started needing, so the
// operator would not start. In both cases the operator was right, the drift check
// was happy, and the thing users install was broken.
//
// Two things are therefore synced on every `make manifests`: the CRDs, and the rules
// of the manager role.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// The wrapper the Helm plugin puts around a CRD, reproduced exactly so this tool's
// output is what the plugin would have written.
const (
	enabledGuardOpen  = "{{- if .Values.crd.enabled }}"
	enabledGuardClose = "{{- end }}"

	// keepPolicy is injected into metadata.annotations so that uninstalling the
	// release can leave the CRD, and with it every StaticSite, in place.
	keepPolicy = `    {{- if .Values.crd.keep }}
    "helm.sh/resource-policy": keep
    {{- end }}`

	annotationsKey = "  annotations:"
)

func main() {
	var sourceDir, targetDir, roleSource, roleTarget string
	var check bool
	flag.StringVar(&sourceDir, "crd-dir", "config/crd/bases", "Directory holding the generated CRDs.")
	flag.StringVar(&targetDir, "chart-crd-dir", "dist/chart/templates/crd",
		"Directory of chart CRD templates to write.")
	flag.StringVar(&roleSource, "role", "config/rbac/role.yaml", "The generated manager role.")
	flag.StringVar(&roleTarget, "chart-role", "dist/chart/templates/rbac/manager-role.yaml",
		"The chart's manager role template.")
	flag.BoolVar(&check, "check", false,
		"Report whether the chart is out of date instead of updating it.")
	flag.Parse()

	changed, err := run(sourceDir, targetDir, check)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helmsync: %v\n", err)
		os.Exit(1)
	}

	roleChanged, err := syncManagerRole(roleSource, roleTarget, check)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helmsync: %v\n", err)
		os.Exit(1)
	}
	if roleChanged {
		changed = append(changed, filepath.Base(roleTarget))
	}

	if check && len(changed) > 0 {
		fmt.Fprintf(os.Stderr,
			"helmsync: the chart is out of date (%s). Run 'make manifests' and commit the result.\n",
			strings.Join(changed, ", "))
		os.Exit(1)
	}
}

// rulesKey is the line the RBAC rules begin at in both files.
const rulesKey = "rules:"

// syncManagerRole replaces the chart role's rules with the generated ones.
//
// Only the rules are copied. The chart's own header stays untouched, because it
// carries logic controller-gen knows nothing about -- whether the role is a Role or
// a ClusterRole, and the templated name.
func syncManagerRole(source, target string, check bool) (bool, error) {
	generated, err := os.ReadFile(source) //nolint:gosec // generated input under the repository
	if err != nil {
		return false, fmt.Errorf("read %q: %w", source, err)
	}
	existing, err := os.ReadFile(target) //nolint:gosec // generated output under the repository
	if err != nil {
		return false, fmt.Errorf("read %q: %w", target, err)
	}

	rules, err := rulesBlock(generated)
	if err != nil {
		return false, fmt.Errorf("%q: %w", source, err)
	}
	header, err := headerBlock(existing)
	if err != nil {
		return false, fmt.Errorf("%q: %w", target, err)
	}

	wanted := []byte(header + rules)
	if bytes.Equal(existing, wanted) {
		return false, nil
	}
	if check {
		return true, nil
	}

	if err := os.WriteFile(target, wanted, 0o644); err != nil { //nolint:gosec // generated output
		return false, fmt.Errorf("write %q: %w", target, err)
	}
	fmt.Printf("synced %s\n", target)
	return true, nil
}

// rulesBlock returns everything from the "rules:" line onward.
func rulesBlock(document []byte) (string, error) {
	lines := strings.Split(strings.TrimRight(string(document), "\n"), "\n")
	for i, line := range lines {
		if line == rulesKey {
			return strings.Join(lines[i:], "\n") + "\n", nil
		}
	}
	return "", fmt.Errorf("no %q line found", rulesKey)
}

// headerBlock returns everything up to and including the "rules:" line.
func headerBlock(document []byte) (string, error) {
	lines := strings.Split(strings.TrimRight(string(document), "\n"), "\n")
	for i, line := range lines {
		if line == rulesKey {
			return strings.Join(lines[:i], "\n") + "\n", nil
		}
	}
	return "", fmt.Errorf("no %q line found", rulesKey)
}

func run(sourceDir, targetDir string, check bool) ([]string, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", sourceDir, err)
	}

	if !check {
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return nil, fmt.Errorf("create %q: %w", targetDir, err)
		}
	}

	var changed []string
	var written []string
	found := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		found++

		source := filepath.Join(sourceDir, name)
		raw, err := os.ReadFile(source) //nolint:gosec // generated input under the repository
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", source, err)
		}

		wrapped, err := wrap(raw)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", source, err)
		}

		// The plugin names each template after the CRD it holds, not after the file
		// controller-gen wrote, and getting that wrong leaves the old template behind
		// as a second copy of the same resource.
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, fmt.Errorf("parse %q: %w", source, err)
		}
		if crd.Name == "" {
			return nil, fmt.Errorf("%q has no metadata.name", source)
		}
		templateName := crd.Name + ".yaml"
		written = append(written, templateName)

		target := filepath.Join(targetDir, templateName)
		existing, readErr := os.ReadFile(target) //nolint:gosec // generated output under the repository
		if readErr == nil && bytes.Equal(existing, wrapped) {
			continue
		}
		changed = append(changed, name)

		if check {
			continue
		}
		if err := os.WriteFile(target, wrapped, 0o644); err != nil { //nolint:gosec // generated output
			return nil, fmt.Errorf("write %q: %w", target, err)
		}
		fmt.Printf("synced %s\n", target)
	}

	if found == 0 {
		return nil, fmt.Errorf("no CRDs found in %q", sourceDir)
	}

	// A CRD that no longer exists, or one whose name changed, would otherwise leave
	// a template behind that installs a resource the operator does not serve.
	orphans, err := pruneOrphans(targetDir, written, check)
	if err != nil {
		return nil, err
	}
	changed = append(changed, orphans...)

	return changed, nil
}

// pruneOrphans removes chart CRD templates that no longer correspond to a
// generated CRD.
func pruneOrphans(targetDir string, written []string, check bool) ([]string, error) {
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %q: %w", targetDir, err)
	}

	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".yaml") || slices.Contains(written, name) {
			continue
		}
		removed = append(removed, name)

		if check {
			continue
		}
		if err := os.Remove(filepath.Join(targetDir, name)); err != nil {
			return nil, fmt.Errorf("remove stale template %q: %w", name, err)
		}
		fmt.Printf("removed stale %s\n", filepath.Join(targetDir, name))
	}
	return removed, nil
}

// wrap turns a generated CRD into the chart template form.
func wrap(raw []byte) ([]byte, error) {
	body := strings.TrimPrefix(strings.TrimLeft(string(raw), "\n"), "---\n")

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	annotations := -1
	for i, line := range lines {
		if line == annotationsKey {
			annotations = i
			break
		}
	}
	if annotations < 0 {
		// controller-gen always writes an annotations block, so its absence means the
		// input is not the file this tool expects.
		return nil, fmt.Errorf("no %q line found; is this a generated CRD?", strings.TrimSpace(annotationsKey))
	}

	var out strings.Builder
	out.WriteString(enabledGuardOpen + "\n")
	for i, line := range lines {
		out.WriteString(line + "\n")
		if i == annotations {
			out.WriteString(keepPolicy + "\n")
		}
	}
	out.WriteString(enabledGuardClose + "\n")

	return []byte(out.String()), nil
}
