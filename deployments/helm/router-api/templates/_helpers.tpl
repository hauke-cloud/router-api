{{/*
Expand the name of the chart.
*/}}
{{- define "router-api.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "router-api.fullname" -}}
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

{{- define "router-api.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Name of one manager's resources. Call with (dict "root" $ "manager" "core").
*/}}
{{- define "router-api.managerName" -}}
{{- printf "%s-%s" (include "router-api.fullname" .root | trunc 40 | trimSuffix "-") .manager | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Labels of one manager's resources.
*/}}
{{- define "router-api.labels" -}}
helm.sh/chart: {{ include "router-api.chart" .root }}
{{ include "router-api.selectorLabels" . }}
{{- if .root.Chart.AppVersion }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/part-of: router-api
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- end }}

{{- define "router-api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "router-api.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .manager }}
{{- end }}

{{/*
Permissions every manager needs: its leader election lease, events, and the
CustomResourceDefinitions it installs.
*/}}
{{- define "router-api.commonRules" -}}
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["create", "patch"]
{{- if .Values.crds.install }}
- apiGroups: ["apiextensions.k8s.io"]
  resources: ["customresourcedefinitions"]
  verbs: ["get", "list", "watch", "create", "patch"]
{{- end }}
{{- end }}
