/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package config

import (
	"flag"
	"strings"
	"testing"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

func TestParseServicePorts(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []webv1alpha1.ServicePort
	}{
		{
			name:  "the default pair",
			input: "https:443,http:80",
			want: []webv1alpha1.ServicePort{
				{Name: portNameHTTPS, Port: 443},
				{Name: portNameHTTP, Port: 80},
			},
		},
		{
			name:  "surrounding space is ignored",
			input: " https : 443 , http : 80 ",
			want: []webv1alpha1.ServicePort{
				{Name: portNameHTTPS, Port: 443},
				{Name: portNameHTTP, Port: 80},
			},
		},
		{
			name:  "a single port",
			input: "http:8080",
			want:  []webv1alpha1.ServicePort{{Name: portNameHTTP, Port: 8080}},
		},
		// Empty means "a Service with no ports", which is what the operator produced
		// before this existed, so it has to stay expressible.
		{name: "empty is no ports, not an error", input: "", want: []webv1alpha1.ServicePort{}},
		{name: "blank is no ports", input: "   ", want: []webv1alpha1.ServicePort{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseServicePorts(tc.input)
			if err != nil {
				t.Fatalf("ParseServicePorts(%q) returned an error: %v", tc.input, err)
			}
			if got == nil {
				t.Fatalf("ParseServicePorts(%q) returned nil, which is indistinguishable from unset", tc.input)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseServicePorts(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("port %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseServicePortsRejects(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		because string
	}{
		{name: "no colon", input: "https", because: "not a name:port pair"},
		{name: "no name", input: ":443", because: "no port name"},
		{name: "not a number", input: "https:https", because: "not a number"},
		{name: "port zero", input: "https:0", because: "outside 1-65535"},
		{name: "port too high", input: "https:70000", because: "outside 1-65535"},
		{name: "uppercase name", input: "HTTPS:443", because: "lowercase"},
		{name: "underscore in name", input: "web_ui:443", because: "lowercase"},
		{name: "name too long", input: "aaaaaaaaaaaaaaaa:443", because: "longer than 15"},
		{name: "repeated name", input: "http:80,http:8080", because: `"http" is repeated`},
		{name: "repeated port", input: "http:80,web:80", because: "80 is repeated"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseServicePorts(tc.input)
			if err == nil {
				t.Fatalf("ParseServicePorts(%q) should have been rejected", tc.input)
			}
			if !strings.Contains(err.Error(), tc.because) {
				t.Errorf("error for %q was %q, which does not mention %q", tc.input, err, tc.because)
			}
		})
	}
}

// TestServicePortsFlag covers the flag wiring, including that a bad value fails at
// startup rather than at the point a Service is rejected.
func TestServicePortsFlag(t *testing.T) {
	t.Run("the default is printed, not empty", func(t *testing.T) {
		defaults := New()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		defaults.BindFlags(fs)

		if got := fs.Lookup("default-service-ports").DefValue; got != "https:443,http:80" {
			t.Errorf("DefValue = %q, want %q", got, "https:443,http:80")
		}
	})

	t.Run("a value replaces the default", func(t *testing.T) {
		defaults := New()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		defaults.BindFlags(fs)

		if err := fs.Parse([]string{"--default-service-ports=http:8080"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(defaults.ServicePorts) != 1 || defaults.ServicePorts[0].Port != 8080 {
			t.Errorf("ServicePorts = %+v, want just http:8080", defaults.ServicePorts)
		}
	})

	t.Run("an empty value clears the ports", func(t *testing.T) {
		defaults := New()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		defaults.BindFlags(fs)

		if err := fs.Parse([]string{"--default-service-ports="}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(defaults.ServicePorts) != 0 {
			t.Errorf("ServicePorts = %+v, want none", defaults.ServicePorts)
		}
	})

	t.Run("a bad value is refused", func(t *testing.T) {
		defaults := New()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		defaults.BindFlags(fs)

		if err := fs.Parse([]string{"--default-service-ports=nope"}); err == nil {
			t.Error("a malformed --default-service-ports should fail at startup")
		}
	})
}

// TestNewDoesNotShareTheDefaultPortsSlice guards against two Defaults, or a
// Defaults and the package default, aliasing one backing array.
func TestNewDoesNotShareTheDefaultPortsSlice(t *testing.T) {
	first := New()
	second := New()

	first.ServicePorts[0].Name = "mutated"

	if second.ServicePorts[0].Name != "https" {
		t.Error("mutating one Defaults changed another")
	}
	if defaultServicePorts[0].Name != "https" {
		t.Error("mutating a Defaults changed the package-level default")
	}
}
