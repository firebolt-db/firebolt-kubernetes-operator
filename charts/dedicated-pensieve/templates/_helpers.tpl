{{/*
Expand the name of the chart.
*/}}
{{- define "dedicated-pensieve.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "dedicated-pensieve.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "dedicated-pensieve.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "dedicated-pensieve.labels" -}}
{{ include "dedicated-pensieve.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.Version | quote }}
helm.sh/chart: {{ include "dedicated-pensieve.chart" . }}
{{- if .Values.extra_labels }}
{{- toYaml .Values.extra_labels | nindent 0 }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "dedicated-pensieve.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dedicated-pensieve.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Returns pod's DNS domain name
*/}}
{{- define "dedicated-pensieve.domain" -}}
{{ include "dedicated-pensieve.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local
{{- end -}}

