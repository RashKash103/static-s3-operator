/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

package registry

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
)

func dockerConfigJSON(entries map[string]string) []byte {
	document := `{"auths":{`
	first := true
	for registryKey, credential := range entries {
		if !first {
			document += ","
		}
		first = false
		document += fmt.Sprintf(`%q:{"auth":%q}`, registryKey,
			base64.StdEncoding.EncodeToString([]byte(credential)))
	}
	return []byte(document + `}}`)
}

func resolve(t *testing.T, keychain authn.Keychain, reference string) authn.AuthConfig {
	t.Helper()

	ref, err := name.ParseReference(reference)
	if err != nil {
		t.Fatalf("parse %q: %v", reference, err)
	}
	authenticator, err := keychain.Resolve(ref.Context())
	if err != nil {
		t.Fatalf("resolve %q: %v", reference, err)
	}
	config, err := authn.Authorization(context.Background(), authenticator)
	if err != nil {
		t.Fatalf("authorization for %q: %v", reference, err)
	}
	return *config
}

func TestKeychainMatchesRegistryHost(t *testing.T) {
	keychain, err := KeychainFromDockerConfigs(dockerConfigJSON(map[string]string{
		"ghcr.io": "ghcr-user:ghcr-token",
	}))
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	config := resolve(t, keychain, "ghcr.io/acme/site:latest")
	if config.Username != "ghcr-user" || config.Password != "ghcr-token" {
		t.Errorf("resolved credentials = %+v", config)
	}
}

func TestKeychainPrefersTheMostSpecificEntry(t *testing.T) {
	// A credential scoped to one repository must not be sent to another on the
	// same registry, and the narrower entry has to win where both apply.
	keychain, err := KeychainFromDockerConfigs(dockerConfigJSON(map[string]string{
		"registry.example.com":              "generic:generic-pass",
		"registry.example.com/team/private": "scoped:scoped-pass",
	}))
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	scoped := resolve(t, keychain, "registry.example.com/team/private:v1")
	if scoped.Username != "scoped" {
		t.Errorf("scoped repository resolved to %q, want the scoped credential", scoped.Username)
	}

	other := resolve(t, keychain, "registry.example.com/team/other:v1")
	if other.Username != "generic" {
		t.Errorf("other repository resolved to %q, want the registry-wide credential", other.Username)
	}
}

func TestKeychainFoldsDockerHubSpellings(t *testing.T) {
	// docker login writes "https://index.docker.io/v1/", the CLI accepts
	// "docker.io", and go-containerregistry reports "index.docker.io". All three
	// have to reach the same credential.
	for _, key := range []string{
		"https://index.docker.io/v1/",
		"docker.io",
		"index.docker.io",
		"registry-1.docker.io",
	} {
		keychain, err := KeychainFromDockerConfigs(dockerConfigJSON(map[string]string{
			key: "hub-user:hub-pass",
		}))
		if err != nil {
			t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
		}

		config := resolve(t, keychain, "acme/site:latest")
		if config.Username != "hub-user" {
			t.Errorf("key %q resolved to %+v, want the Docker Hub credential", key, config)
		}
	}
}

func TestKeychainFallsBackToAnonymous(t *testing.T) {
	keychain, err := KeychainFromDockerConfigs(dockerConfigJSON(map[string]string{
		"ghcr.io": "user:pass",
	}))
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	config := resolve(t, keychain, "quay.io/acme/site:latest")
	if config.Username != "" || config.Password != "" {
		t.Errorf("an unknown registry should resolve anonymously, got %+v", config)
	}
}

func TestKeychainAcceptsExplicitUsernameAndPassword(t *testing.T) {
	keychain, err := KeychainFromDockerConfigs([]byte(
		`{"auths":{"ghcr.io":{"username":"explicit","password":"secret"}}}`))
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	config := resolve(t, keychain, "ghcr.io/acme/site:latest")
	if config.Username != "explicit" || config.Password != "secret" {
		t.Errorf("resolved credentials = %+v", config)
	}
}

func TestKeychainAcceptsTheDockercfgFormat(t *testing.T) {
	// A kubernetes.io/dockercfg Secret is the bare registry map, with no "auths".
	keychain, err := KeychainFromDockerConfigs([]byte(
		fmt.Sprintf(`{"ghcr.io":{"auth":%q}}`,
			base64.StdEncoding.EncodeToString([]byte("legacy:legacy-pass")))))
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	config := resolve(t, keychain, "ghcr.io/acme/site:latest")
	if config.Username != "legacy" {
		t.Errorf("resolved credentials = %+v", config)
	}
}

func TestKeychainLaterDocumentsWin(t *testing.T) {
	keychain, err := KeychainFromDockerConfigs(
		dockerConfigJSON(map[string]string{"ghcr.io": "first:first-pass"}),
		dockerConfigJSON(map[string]string{"ghcr.io": "second:second-pass"}),
	)
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	config := resolve(t, keychain, "ghcr.io/acme/site:latest")
	if config.Username != "second" {
		t.Errorf("resolved credentials = %+v, want the later document", config)
	}
}

func TestKeychainRejectsUnparsableDocuments(t *testing.T) {
	if _, err := KeychainFromDockerConfigs([]byte("not json")); err == nil {
		t.Fatal("an unparsable docker config should be reported")
	}
	if _, err := KeychainFromDockerConfigs([]byte(`{"auths":{"ghcr.io":{"auth":"!!not base64!!"}}}`)); err == nil {
		t.Fatal("an undecodable auth field should be reported")
	}
	if _, err := KeychainFromDockerConfigs([]byte(
		`{"auths":{"ghcr.io":{"auth":"bm9jb2xvbg=="}}}`)); err == nil {
		t.Fatal("an auth field without a colon should be reported")
	}
}

func TestKeychainIgnoresEmptyDocumentsAndEntries(t *testing.T) {
	keychain, err := KeychainFromDockerConfigs(
		[]byte("   "),
		[]byte(`{"auths":{"ghcr.io":{}}}`),
	)
	if err != nil {
		t.Fatalf("KeychainFromDockerConfigs returned an error: %v", err)
	}

	config := resolve(t, keychain, "ghcr.io/acme/site:latest")
	if config.Username != "" {
		t.Errorf("an entry with no credential should resolve anonymously, got %+v", config)
	}
}

func TestKeychainFromSecretsReadsBothSecretTypes(t *testing.T) {
	secrets := []corev1.Secret{
		{
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: dockerConfigJSON(map[string]string{"ghcr.io": "a:a-pass"}),
			},
		},
		{
			Type: corev1.SecretTypeDockercfg,
			Data: map[string][]byte{
				corev1.DockerConfigKey: []byte(fmt.Sprintf(`{"quay.io":{"auth":%q}}`,
					base64.StdEncoding.EncodeToString([]byte("b:b-pass")))),
			},
		},
		// Not a pull Secret at all: passing a namespace's Secrets unfiltered has to
		// be safe.
		{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"accessKeyID": []byte("AKIA")}},
	}

	keychain, err := KeychainFromSecrets(secrets)
	if err != nil {
		t.Fatalf("KeychainFromSecrets returned an error: %v", err)
	}

	if config := resolve(t, keychain, "ghcr.io/acme/site:latest"); config.Username != "a" {
		t.Errorf("ghcr.io resolved to %+v", config)
	}
	if config := resolve(t, keychain, "quay.io/acme/site:latest"); config.Username != "b" {
		t.Errorf("quay.io resolved to %+v", config)
	}
}

func TestNormalizeRegistryKey(t *testing.T) {
	tests := map[string]string{
		"https://index.docker.io/v1/":  "index.docker.io",
		"http://localhost:5000":        "localhost:5000",
		"registry.example.com/":        "registry.example.com",
		"registry.example.com/v2/":     "registry.example.com",
		"registry.example.com:5000":    "registry.example.com:5000",
		"docker.io/library":            "index.docker.io/library",
		" registry.example.com/team  ": "registry.example.com/team",
	}

	for input, want := range tests {
		if got := normalizeRegistryKey(input); got != want {
			t.Errorf("normalizeRegistryKey(%q) = %q, want %q", input, got, want)
		}
	}
}
