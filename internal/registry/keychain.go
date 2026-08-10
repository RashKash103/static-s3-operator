/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Package registry resolves container image references against a remote
// registry and authenticates with Kubernetes image pull Secrets.
package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
)

// dockerHubHost is the registry host go-containerregistry reports for Docker
// Hub references, and the key every Docker Hub credential is folded onto.
const dockerHubHost = "index.docker.io"

// dockerConfig is the subset of a Docker config file the operator reads. It
// covers both the dockerconfigjson format, where credentials live under
// "auths", and the older dockercfg format, which is the bare map.
type dockerConfig struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Auth is base64("username:password"), the form docker login writes.
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// secretKeychain authenticates against registries using credentials collected
// from Docker config files, keyed by normalised registry (or registry plus
// repository path) so that a credential scoped to one repository is not sent to
// another.
type secretKeychain struct {
	entries map[string]authn.AuthConfig
}

// KeychainFromSecrets builds a Keychain from Kubernetes image pull Secrets. It
// accepts both kubernetes.io/dockerconfigjson and kubernetes.io/dockercfg
// Secrets and ignores Secrets carrying neither payload, so a caller can pass a
// namespace's Secrets without pre-filtering. Later Secrets win over earlier
// ones for the same registry.
func KeychainFromSecrets(secrets []corev1.Secret) (authn.Keychain, error) {
	documents := make([][]byte, 0, len(secrets))
	for i := range secrets {
		secret := &secrets[i]
		if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
			documents = append(documents, raw)
			continue
		}
		if raw, ok := secret.Data[corev1.DockerConfigKey]; ok {
			documents = append(documents, raw)
		}
	}
	return KeychainFromDockerConfigs(documents...)
}

// KeychainFromDockerConfigs builds a Keychain from raw Docker config documents.
// The sync Job uses this directly, reading the pull Secrets that were projected
// into its filesystem rather than talking to the Kubernetes API.
func KeychainFromDockerConfigs(documents ...[]byte) (authn.Keychain, error) {
	keychain := &secretKeychain{entries: map[string]authn.AuthConfig{}}

	for _, raw := range documents {
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}

		var wrapped dockerConfig
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, fmt.Errorf("parse docker config: %w", err)
		}
		auths := wrapped.Auths
		if auths == nil {
			// A dockercfg Secret is the bare registry map, with no "auths" wrapper.
			if err := json.Unmarshal(raw, &auths); err != nil {
				return nil, fmt.Errorf("parse docker config: %w", err)
			}
		}

		for registry, entry := range auths {
			auth, err := entry.authConfig()
			if err != nil {
				return nil, fmt.Errorf("parse credentials for %q: %w", registry, err)
			}
			if auth == nil {
				continue
			}
			keychain.entries[normalizeRegistryKey(registry)] = *auth
		}
	}

	return keychain, nil
}

// authConfig turns one Docker config entry into an AuthConfig, expanding the
// base64 "auth" field when username and password are not spelled out. It
// returns nil for an entry that carries no usable credential.
func (e dockerAuthEntry) authConfig() (*authn.AuthConfig, error) {
	auth := authn.AuthConfig{
		Username:      e.Username,
		Password:      e.Password,
		IdentityToken: e.IdentityToken,
		RegistryToken: e.RegistryToken,
	}

	if e.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e.Auth))
		if err != nil {
			return nil, fmt.Errorf("decode auth field: %w", err)
		}
		username, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return nil, fmt.Errorf("auth field is not %q", "username:password")
		}
		if auth.Username == "" {
			auth.Username = username
		}
		if auth.Password == "" {
			auth.Password = password
		}
	}

	if auth.Username == "" && auth.Password == "" && auth.IdentityToken == "" && auth.RegistryToken == "" {
		return nil, nil
	}
	return &auth, nil
}

// Resolve implements authn.Keychain. It prefers the most specific credential
// available: an entry for "registry/org/repo" beats one for "registry/org",
// which beats one for the registry alone.
func (k *secretKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	candidate := normalizeRegistryKey(target.String())
	for {
		if auth, ok := k.entries[candidate]; ok {
			return authn.FromConfig(auth), nil
		}
		index := strings.LastIndex(candidate, "/")
		if index < 0 {
			break
		}
		candidate = candidate[:index]
	}

	if auth, ok := k.entries[normalizeRegistryKey(target.RegistryStr())]; ok {
		return authn.FromConfig(auth), nil
	}
	return authn.Anonymous, nil
}

// normalizeRegistryKey reduces the many spellings of a registry to one form, so
// that "https://index.docker.io/v1/", "docker.io" and "index.docker.io" all
// match a Docker Hub reference.
func normalizeRegistryKey(key string) string {
	key = strings.TrimSpace(key)
	if scheme := strings.Index(key, "://"); scheme >= 0 {
		key = key[scheme+3:]
	}
	key = strings.TrimSuffix(key, "/")
	for _, apiSuffix := range []string{"/v1", "/v2"} {
		key = strings.TrimSuffix(key, apiSuffix)
	}
	key = strings.TrimSuffix(key, "/")

	host, path, hasPath := strings.Cut(key, "/")
	switch host {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		host = dockerHubHost
	}
	if hasPath {
		return host + "/" + path
	}
	return host
}
