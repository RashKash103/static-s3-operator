/*
Copyright 2026 rashkash103.

SPDX-License-Identifier: MIT

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction. See the LICENSE file in the project root for
the full license text.
*/

// Command bucketname prints the bucket name the operator will generate for a
// StaticSite.
//
// A generated name is deliberately opaque -- it appears in public DNS, so it must
// not spell out the cluster's namespaces and resource names -- but it still has to
// be knowable in advance, so that a bucket can be created by hand or an access
// policy scoped to it before the resource exists. This computes it using the same
// code the operator does.
//
//	make bucket-name NAMESPACE=web NAME=docs
//
// The same value can be had from standard tools, which is worth knowing when this
// repository is not to hand:
//
//	CLUSTER_UUID=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
//	echo "static-s3-$(uuidgen --sha1 --namespace "$CLUSTER_UUID" --name web/docs)"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	webv1alpha1 "github.com/rashkash103/static-s3-operator/api/v1alpha1"
	operatorconfig "github.com/rashkash103/static-s3-operator/internal/config"
)

func main() {
	defaults := operatorconfig.New()

	var namespace, name string
	var clusterUUID, template string
	var fromCluster bool

	flag.StringVar(&namespace, "namespace", "", "Namespace of the StaticSite. Required.")
	flag.StringVar(&name, "name", "", "Name of the StaticSite. Required.")
	flag.StringVar(&clusterUUID, "cluster-uuid", "",
		"The cluster's UUID namespace, as printed by "+
			"`kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'`. "+
			"Omit it to read it from the current kubeconfig context.")
	flag.StringVar(&template, "template", defaults.BucketNameTemplate,
		"The operator's --default-bucket-name-template, if it is not the default.")
	flag.BoolVar(&fromCluster, "from-cluster", false,
		"Read the cluster UUID from the current kubeconfig context. Implied when "+
			"--cluster-uuid is empty.")
	flag.Parse()

	if err := run(defaults, namespace, name, clusterUUID, template); err != nil {
		fmt.Fprintf(os.Stderr, "bucketname: %v\n", err)
		os.Exit(1)
	}
}

func run(defaults *operatorconfig.Defaults, namespace, name, clusterUUID, template string) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("both --namespace and --name are required")
	}

	defaults.BucketNameTemplate = template
	defaults.BucketNameUUIDNamespace = clusterUUID

	if defaults.BucketNameUUIDNamespace == "" {
		resolved, err := clusterUUIDFromKubeconfig()
		if err != nil {
			return fmt.Errorf(
				"%w\n\nPass it directly instead:\n"+
					"  kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'", err)
		}
		defaults.BucketNameUUIDNamespace = resolved
		fmt.Fprintf(os.Stderr, "using cluster UUID %s\n", resolved)
	}

	site := &webv1alpha1.StaticSite{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	bucket, err := defaults.GenerateBucketName(site)
	if err != nil {
		return err
	}

	fmt.Println(bucket)
	return nil
}

// clusterUUIDFromKubeconfig reads the identifying UID from the cluster the current
// kubeconfig context points at.
func clusterUUIDFromKubeconfig() (string, error) {
	restConfig, err := config.GetConfig()
	if err != nil {
		return "", fmt.Errorf("no kubeconfig available to read the cluster UUID: %w", err)
	}

	reader, err := client.New(restConfig, client.Options{})
	if err != nil {
		return "", fmt.Errorf("connect to the cluster: %w", err)
	}

	defaults := operatorconfig.New()
	// Empty means "read it from the cluster", which is what this needs.
	defaults.BucketNameUUIDNamespace = ""
	if err := defaults.ResolveBucketNameUUIDNamespace(context.Background(), reader); err != nil {
		return "", err
	}
	return defaults.BucketNameUUIDNamespace, nil
}
