{{/*
Operator behaviour flags.

Every setting the operator understands is a command line flag, and this template
turns the `operator` values block into that flag list. It lives in its own file
so that regenerating the chart with the Kubebuilder Helm plugin only clobbers a
single line in templates/manager/manager.yaml rather than this logic.

A flag is emitted only when its value is set, so the operator's own compiled-in
default stays in effect for anything the chart leaves empty.
*/}}
{{- define "static-s3-operator.operatorArgs" -}}
{{- $operator := .Values.operator | default dict }}
{{- $defaults := $operator.defaults | default dict }}
{{- $syncer := $operator.syncer | default dict }}
{{- $reconcile := $operator.reconcile | default dict }}
{{- $s3 := $operator.s3 | default dict }}
{{- $credentials := $operator.credentials | default dict }}
{{- $externalDNS := $operator.externalDNS | default dict }}

{{- /* Defaults that fill in optional StaticSite fields. */ -}}
{{- with $defaults.sourcePath }}
- {{ printf "--default-source-path=%s" . | quote }}
{{- end }}
{{- with $defaults.pullInterval }}
- {{ printf "--default-pull-interval=%s" . | quote }}
{{- end }}
{{- with $defaults.bucketNameTemplate }}
- {{ printf "--default-bucket-name-template=%s" . | quote }}
{{- end }}
{{- with $defaults.bucketNameUUIDNamespace }}
- {{ printf "--bucket-name-uuid-namespace=%s" . | quote }}
{{- end }}
{{- with $defaults.region }}
- {{ printf "--default-region=%s" . | quote }}
{{- end }}
{{- with $defaults.endpoint }}
- {{ printf "--default-endpoint=%s" . | quote }}
{{- end }}
{{- if kindIs "bool" $defaults.forcePathStyle }}
- {{ printf "--default-force-path-style=%t" $defaults.forcePathStyle | quote }}
{{- end }}
{{- with $defaults.websiteEndpointTemplate }}
- {{ printf "--default-website-endpoint-template=%s" . | quote }}
{{- end }}
{{- with $defaults.indexDocument }}
- {{ printf "--default-index-document=%s" . | quote }}
{{- end }}
{{- /* An explicitly empty errorDocument means "configure no error document", which
     is different from leaving the key out, so it is passed through as given. */ -}}
{{- if hasKey $defaults "errorDocument" }}
- {{ printf "--default-error-document=%s" ($defaults.errorDocument | toString) | quote }}
{{- end }}
{{- if hasKey $defaults "cacheControl" }}
- {{ printf "--default-cache-control=%s" ($defaults.cacheControl | toString) | quote }}
{{- end }}

{{- /* The ExternalName Service the operator maintains per site. */ -}}
{{- if hasKey $externalDNS "hostnameAnnotation" }}
- {{ printf "--external-dns-hostname-annotation=%s" ($externalDNS.hostnameAnnotation | toString) | quote }}
{{- end }}
{{- /* A list of {name, port} in values, one name:port flag on the command line. An
     explicitly empty list means "a Service with no ports", which differs from
     leaving the key out, so the key's presence is what decides. */ -}}
{{- if hasKey $defaults "servicePorts" }}
{{- $pairs := list }}
{{- range $defaults.servicePorts }}
{{- $pairs = append $pairs (printf "%s:%v" .name .port) }}
{{- end }}
- {{ printf "--default-service-ports=%s" (join "," $pairs) | quote }}
{{- end }}

{{- /* Sync Jobs. Leaving syncer.image empty is the recommended setting: the
     operator then reads its own Pod and reuses its own image, so the manager and
     the syncer can never drift apart across an upgrade. */}}
{{- with $syncer.image }}
- {{ printf "--syncer-image=%s" . | quote }}
{{- end }}
{{- with $syncer.imagePullPolicy }}
- {{ printf "--syncer-image-pull-policy=%s" . | quote }}
{{- end }}
{{- if $syncer.maxExtractBytes }}
- {{ printf "--max-extract-bytes=%d" (int64 $syncer.maxExtractBytes) | quote }}
{{- end }}
{{- if $syncer.maxExtractFiles }}
- {{ printf "--max-extract-files=%d" (int64 $syncer.maxExtractFiles) | quote }}
{{- end }}

{{- /* Reconcile pacing. */ -}}
{{- if $reconcile.maxConcurrentReconciles }}
- {{ printf "--max-concurrent-reconciles=%d" (int64 $reconcile.maxConcurrentReconciles) | quote }}
{{- end }}
{{- with $reconcile.syncRetryBaseDelay }}
- {{ printf "--sync-retry-base-delay=%s" . | quote }}
{{- end }}
{{- with $reconcile.syncRetryMaxDelay }}
- {{ printf "--sync-retry-max-delay=%s" . | quote }}
{{- end }}
{{- with $reconcile.ownershipRecheckInterval }}
- {{ printf "--ownership-recheck-interval=%s" . | quote }}
{{- end }}

{{- /* Provider quirks. */ -}}
{{- if kindIs "bool" $s3.requestChecksumWhenSupported }}
- {{ printf "--s3-request-checksum-when-supported=%t" $s3.requestChecksumWhenSupported | quote }}
{{- end }}
{{- if kindIs "bool" $s3.skipTLSVerify }}
- {{ printf "--s3-skip-tls-verify=%t" $s3.skipTLSVerify | quote }}
{{- end }}

{{- /* Credentials. The secret name is always passed, so the operator never falls
     back to a name the chart did not choose. */}}
- {{ printf "--default-credentials-secret=%s" (include "static-s3-operator.credentialsSecretName" .) | quote }}
{{- with $credentials.accessKeyIDKey }}
- {{ printf "--default-credentials-access-key-id-key=%s" . | quote }}
{{- end }}
{{- with $credentials.secretAccessKeyKey }}
- {{ printf "--default-credentials-secret-access-key-key=%s" . | quote }}
{{- end }}
{{- with $credentials.sessionTokenKey }}
- {{ printf "--default-credentials-session-token-key=%s" . | quote }}
{{- end }}
{{- if kindIs "bool" $credentials.mirrorToSiteNamespaces }}
- {{ printf "--mirror-default-credentials=%t" $credentials.mirrorToSiteNamespaces | quote }}
{{- end }}
{{- end }}

{{/*
The name of the Secret holding the operator's default S3 credentials.

An existing Secret wins, so a cluster that keeps credentials in an external
secret store never has to put them in a values file.
*/}}
{{- define "static-s3-operator.credentialsSecretName" -}}
{{- $credentials := (.Values.operator | default dict).credentials | default dict }}
{{- if $credentials.existingSecret }}
{{- $credentials.existingSecret }}
{{- else }}
{{- include "static-s3-operator.resourceName" (dict "suffix" "s3-credentials" "context" .) }}
{{- end }}
{{- end }}
