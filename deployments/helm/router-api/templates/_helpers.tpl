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
Permissions every manager needs on the objects it works with.
*/}}
{{- define "router-api.commonRules" -}}
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["create", "patch"]
{{- end }}

{{/*
A binding of a manager's ClusterRole to its ServiceAccount. Cluster-wide by
default. With watchNamespace it is a RoleBinding in that namespace: a
RoleBinding to a ClusterRole grants the role's permissions in the binding's
namespace and nowhere else, so a manager that only watches one namespace
cannot read the Secrets of the others.
Call with (dict "root" $ "manager" "core" "role" "<name of the ClusterRole>").
*/}}
{{- define "router-api.binding" -}}
apiVersion: rbac.authorization.k8s.io/v1
{{- if .root.Values.watchNamespace }}
kind: RoleBinding
metadata:
  name: {{ .role }}
  namespace: {{ .root.Values.watchNamespace }}
{{- else }}
kind: ClusterRoleBinding
metadata:
  name: {{ .role }}
{{- end }}
  labels:
    {{- include "router-api.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ .role }}
subjects:
  - kind: ServiceAccount
    name: {{ include "router-api.managerName" . }}
    namespace: {{ .root.Release.Namespace }}
{{- end }}
