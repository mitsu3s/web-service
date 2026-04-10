{{- define "task-orchestrator-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "task-orchestrator-service.fullname" -}}
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
{{- define "task-orchestrator-service.labels" -}}
helm.sh/chart: {{ include "task-orchestrator-service.name" . }}-{{ .Chart.Version }}
{{ include "task-orchestrator-service.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "task-orchestrator-service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "task-orchestrator-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "task-orchestrator-service.serviceAccountName" -}}
{{- include "task-orchestrator-service.fullname" . }}
{{- end }}
