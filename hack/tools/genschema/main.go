/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Command genschema turns the generated CRDs into standalone JSON Schema
// documents.
//
// A CRD's openAPIV3Schema already describes the resource completely, but editors
// cannot use it directly: they want a JSON Schema document per resource, keyed by
// apiVersion and kind. This lifts each version's schema out of every CRD, adds
// the apiVersion, kind and metadata fields that the API server implies rather
// than declares, and writes one file per version.
//
// The result is what makes writing a StaticSite manifest in an editor pleasant:
// completion and validation come from the same source of truth as the cluster.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// JSON Schema keywords this tool writes.
const (
	schemaType        = "type"
	schemaDescription = "description"
)

func main() {
	var crdDir, outputDir, schemaID string
	flag.StringVar(&crdDir, "crd-dir", "config/crd/bases", "Directory holding generated CRD YAML.")
	flag.StringVar(&outputDir, "output-dir", "dist/schemas", "Directory to write JSON Schema documents to.")
	flag.StringVar(&schemaID, "id-prefix", "",
		"Base URL for the $id of each schema, for example the raw URL of the output directory.")
	flag.Parse()

	if err := run(crdDir, outputDir, schemaID); err != nil {
		fmt.Fprintf(os.Stderr, "genschema: %v\n", err)
		os.Exit(1)
	}
}

func run(crdDir, outputDir, schemaID string) error {
	entries, err := os.ReadDir(crdDir)
	if err != nil {
		return fmt.Errorf("read %q: %w", crdDir, err)
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", outputDir, err)
	}

	written := 0
	var index []indexEntry

	for _, entry := range entries {
		if entry.IsDir() || !isYAML(entry.Name()) {
			continue
		}

		path := filepath.Join(crdDir, entry.Name())
		raw, err := os.ReadFile(path) //nolint:gosec // generated input under the repository
		if err != nil {
			return fmt.Errorf("read %q: %w", path, err)
		}

		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return fmt.Errorf("parse %q: %w", path, err)
		}

		for _, version := range crd.Spec.Versions {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}

			document, err := buildSchema(crd, version.Name, version.Schema.OpenAPIV3Schema, schemaID)
			if err != nil {
				return err
			}

			name := fmt.Sprintf("%s-%s.json",
				strings.ToLower(crd.Spec.Names.Kind), strings.ToLower(version.Name))
			target := filepath.Join(outputDir, name)

			encoded, err := json.MarshalIndent(document, "", "  ")
			if err != nil {
				return fmt.Errorf("encode schema for %s/%s: %w", crd.Spec.Names.Kind, version.Name, err)
			}
			encoded = append(encoded, '\n')

			if err := os.WriteFile(target, encoded, 0o644); err != nil { //nolint:gosec // generated output
				return fmt.Errorf("write %q: %w", target, err)
			}

			index = append(index, indexEntry{
				APIVersion: fmt.Sprintf("%s/%s", crd.Spec.Group, version.Name),
				Kind:       crd.Spec.Names.Kind,
				File:       name,
			})
			written++
			fmt.Printf("wrote %s\n", target)
		}
	}

	if written == 0 {
		return fmt.Errorf("no CRD schemas found in %q", crdDir)
	}

	return writeIndex(outputDir, index)
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// buildSchema converts a CRD's structural schema into a standalone JSON Schema.
func buildSchema(
	crd *apiextensionsv1.CustomResourceDefinition,
	version string,
	source *apiextensionsv1.JSONSchemaProps,
	schemaID string,
) (map[string]any, error) {
	// Round-tripping through JSON is the least surprising conversion: the CRD
	// schema is already JSON Schema shaped, and re-encoding it keeps every keyword
	// the API server understood without this tool having to know them all.
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, fmt.Errorf("encode schema for %s/%s: %w", crd.Spec.Names.Kind, version, err)
	}

	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, fmt.Errorf("decode schema for %s/%s: %w", crd.Spec.Names.Kind, version, err)
	}

	apiVersion := fmt.Sprintf("%s/%s", crd.Spec.Group, version)

	properties, _ := document["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
		document["properties"] = properties
	}

	// The API server supplies these three rather than declaring them in the
	// structural schema, so an editor validating a plain manifest needs them
	// spelled out or it will reject a perfectly valid file.
	properties["apiVersion"] = map[string]any{
		schemaType:        "string",
		schemaDescription: "APIVersion of this resource.",
		"const":           apiVersion,
	}
	properties["kind"] = map[string]any{
		schemaType:        "string",
		schemaDescription: "Kind of this resource.",
		"const":           crd.Spec.Names.Kind,
	}
	if _, hasMetadata := properties["metadata"]; !hasMetadata {
		properties["metadata"] = map[string]any{
			schemaType:        "object",
			schemaDescription: "Standard object metadata.",
		}
	}

	document["$schema"] = "http://json-schema.org/draft-07/schema#"
	if schemaID != "" {
		document["$id"] = fmt.Sprintf("%s/%s-%s.json",
			strings.TrimSuffix(schemaID, "/"),
			strings.ToLower(crd.Spec.Names.Kind),
			strings.ToLower(version))
	}
	document["title"] = fmt.Sprintf("%s (%s)", crd.Spec.Names.Kind, apiVersion)
	document["required"] = mergeRequired(document["required"], "apiVersion", "kind", "spec")

	return document, nil
}

// mergeRequired adds names to an existing required list without duplicating any.
func mergeRequired(existing any, names ...string) []string {
	seen := map[string]bool{}
	var required []string

	if list, ok := existing.([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok && !seen[name] {
				seen[name] = true
				required = append(required, name)
			}
		}
	}
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			required = append(required, name)
		}
	}

	slices.Sort(required)
	return required
}

type indexEntry struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	File       string `json:"file"`
}

// writeIndex records which schema covers which kind, so tooling that maps a
// manifest to a schema does not have to open every file to find out.
func writeIndex(outputDir string, entries []indexEntry) error {
	slices.SortFunc(entries, func(a, b indexEntry) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.APIVersion, b.APIVersion)
	})

	encoded, err := json.MarshalIndent(map[string]any{"schemas": entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schema index: %w", err)
	}
	encoded = append(encoded, '\n')

	target := filepath.Join(outputDir, "index.json")
	if err := os.WriteFile(target, encoded, 0o644); err != nil { //nolint:gosec // generated output
		return fmt.Errorf("write %q: %w", target, err)
	}
	fmt.Printf("wrote %s\n", target)
	return nil
}
