/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package main

import (
	"strings"
	"testing"
	"text/template"
)

// crdWithTemplateSyntax is the shape controller-gen produces for a field whose
// documentation contains Go template syntax, which several of this API's fields do.
const crdWithTemplateSyntax = `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    controller-gen.kubebuilder.io/version: v0.19.0
  name: staticsites.web.rka.sh
spec:
  versions:
    - schema:
        openAPIV3Schema:
          properties:
            websiteEndpointTemplate:
              description: |-
                A Go template evaluated with {{.Bucket}}, {{.Region}} and
                {{.Endpoint}}, for example "http://{{.Bucket}}.example.com".
              type: string
`

func TestWrapEscapesTemplateSyntaxSoHelmDoesNotEvaluateIt(t *testing.T) {
	wrapped, err := wrap([]byte(crdWithTemplateSyntax))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	// Rendering is the only check that matters. The stored file looks wrong on
	// purpose -- it contains the escape -- so asserting on its bytes would lock in
	// the escaping mechanism rather than the outcome.
	rendered := render(t, string(wrapped))

	for _, want := range []string{
		"{{.Bucket}}, {{.Region}} and",
		`"http://{{.Bucket}}.example.com"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered chart template lost %q.\nHelm evaluated the CRD's own\n"+
				"template syntax as an action. Rendered output:\n%s", want, rendered)
		}
	}
}

func TestWrapKeepsItsOwnDirectivesLive(t *testing.T) {
	wrapped, err := wrap([]byte(crdWithTemplateSyntax))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	// The guard helmsync adds must still be a real action; escaping everything
	// indiscriminately would turn it into literal text and the CRD would ship
	// whatever crd.enabled said.
	if got := render(t, string(wrapped)); !strings.Contains(got, "kind: CustomResourceDefinition") {
		t.Fatalf("crd.enabled=true did not emit the CRD:\n%s", got)
	}

	off := renderWith(t, string(wrapped), map[string]any{
		"Values": map[string]any{"crd": map[string]any{"enabled": false, "keep": false}},
	})
	if strings.Contains(off, "kind: CustomResourceDefinition") {
		t.Errorf("crd.enabled=false still emitted the CRD:\n%s", off)
	}
}

func TestEscapeTemplateActions(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "no syntax is untouched", input: "  description: a plain string", want: "  description: a plain string"},
		{name: "an action is escaped", input: "{{.Bucket}}", want: `{{ "{{" }}.Bucket}}`},
		{name: "every action on the line", input: "{{.A}}-{{.B}}", want: `{{ "{{" }}.A}}-{{ "{{" }}.B}}`},
		// Only "{{" opens an action, so a lone "}}" needs no escaping and escaping
		// it would show up in kubectl explain.
		{name: "a bare closer is literal", input: "a }} b", want: "a }} b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeTemplateActions(tc.input); got != tc.want {
				t.Errorf("escapeTemplateActions(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// render renders with the CRD enabled, which is the default the chart ships.
func render(t *testing.T, chartTemplate string) string {
	t.Helper()
	return renderWith(t, chartTemplate, map[string]any{
		"Values": map[string]any{"crd": map[string]any{"enabled": true, "keep": false}},
	})
}

// renderWith stands in for Helm. Helm adds sprig and its own context, but the
// template engine underneath is text/template, which is what decides whether a "{{"
// in copied output is an action.
func renderWith(t *testing.T, chartTemplate string, data map[string]any) string {
	t.Helper()

	parsed, err := template.New("crd").Parse(chartTemplate)
	if err != nil {
		t.Fatalf("the wrapped CRD is not a valid template: %v", err)
	}

	var out strings.Builder
	if err := parsed.Execute(&out, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out.String()
}
