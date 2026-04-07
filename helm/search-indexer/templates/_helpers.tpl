{{- define "search-indexer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "search-indexer.fullname" -}}
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
{{- define "search-indexer.labels" -}}
helm.sh/chart: {{ include "search-indexer.name" . }}-{{ .Chart.Version }}
{{ include "search-indexer.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "search-indexer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "search-indexer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "search-indexer.serviceAccountName" -}}
{{- include "search-indexer.fullname" . }}
{{- end }}
