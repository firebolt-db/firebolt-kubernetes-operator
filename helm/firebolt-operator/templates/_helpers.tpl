{{/*
Expand the name of the chart.
*/}}
{{- define "firebolt-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "firebolt-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "firebolt-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to all resources.
*/}}
{{- define "firebolt-operator.labels" -}}
helm.sh/chart: {{ include "firebolt-operator.chart" . }}
{{ include "firebolt-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.extraLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels used in matchLabels and service selectors.
*/}}
{{- define "firebolt-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "firebolt-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Extract port number from a bind address (":8081" or "0.0.0.0:8081").
*/}}
{{- define "firebolt-operator.portNumber" -}}
{{- $parts := splitList ":" . -}}
{{- index $parts (sub (len $parts) 1) | int -}}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "firebolt-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "firebolt-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the chart-managed gateway-wake ClusterRole. The operator binds
this ClusterRole to each FireboltInstance's gateway ServiceAccount via
a per-instance RoleBinding, passing the same name in
`--gateway-wake-cluster-role`. Override
`gatewayWakeClusterRole.name` to point at an externally managed
ClusterRole (e.g. when `gatewayWakeClusterRole.create: false`).
*/}}
{{- define "firebolt-operator.gatewayWakeClusterRoleName" -}}
{{- default (printf "%s-gateway-wake" (include "firebolt-operator.fullname" .)) .Values.gatewayWakeClusterRole.name }}
{{- end }}
