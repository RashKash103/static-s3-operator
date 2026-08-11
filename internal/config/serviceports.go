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
	"fmt"
	"regexp"
	"strconv"
	"strings"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
)

// portNamePattern is the IANA service name rule Kubernetes applies to a named
// port, repeated here so a bad flag is refused at startup rather than surfacing
// later as a rejected Service.
var portNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxPortNameLength = 15

// servicePortsValue parses --default-service-ports, a comma-separated list of
// name:port pairs such as "https:443,http:80".
//
// A flag rather than a repeated one so that the Helm chart can render a single
// argument, and so that an empty value can mean "no ports at all" -- which is
// distinguishable from the flag being absent.
type servicePortsValue struct {
	target *[]webv1alpha1.ServicePort
}

func (v servicePortsValue) String() string {
	// The flag package builds a zero value of this type to decide whether to print
	// a default, so this has to tolerate an unset target.
	if v.target == nil {
		return ""
	}
	parts := make([]string, 0, len(*v.target))
	for _, port := range *v.target {
		parts = append(parts, fmt.Sprintf("%s:%d", port.Name, port.Port))
	}
	return strings.Join(parts, ",")
}

func (v servicePortsValue) Set(raw string) error {
	ports, err := ParseServicePorts(raw)
	if err != nil {
		return err
	}
	*v.target = ports
	return nil
}

// ParseServicePorts turns "https:443,http:80" into ports. An empty string is not
// an error: it means a Service with no ports, and returns a non-nil empty slice so
// that it stays distinguishable from "unset".
func ParseServicePorts(raw string) ([]webv1alpha1.ServicePort, error) {
	ports := []webv1alpha1.ServicePort{}

	if strings.TrimSpace(raw) == "" {
		return ports, nil
	}

	seenNames := map[string]bool{}
	seenPorts := map[int32]bool{}

	for pair := range strings.SplitSeq(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		name, number, found := strings.Cut(pair, ":")
		if !found {
			return nil, fmt.Errorf("%q is not a name:port pair", pair)
		}
		name = strings.TrimSpace(name)
		number = strings.TrimSpace(number)

		switch {
		case name == "":
			return nil, fmt.Errorf("%q has no port name", pair)
		case len(name) > maxPortNameLength:
			return nil, fmt.Errorf("port name %q is longer than %d characters", name, maxPortNameLength)
		case !portNamePattern.MatchString(name):
			return nil, fmt.Errorf(
				"port name %q must be lowercase alphanumerics and hyphens, starting and ending with one", name)
		case seenNames[name]:
			return nil, fmt.Errorf("port name %q is repeated", name)
		}

		parsed, err := strconv.Atoi(number)
		if err != nil {
			return nil, fmt.Errorf("port %q in %q is not a number", number, pair)
		}
		if parsed < 1 || parsed > 65535 {
			return nil, fmt.Errorf("port %d in %q is outside 1-65535", parsed, pair)
		}
		if seenPorts[int32(parsed)] {
			return nil, fmt.Errorf("port %d is repeated", parsed)
		}

		seenNames[name] = true
		seenPorts[int32(parsed)] = true
		ports = append(ports, webv1alpha1.ServicePort{Name: name, Port: int32(parsed)})
	}

	return ports, nil
}
