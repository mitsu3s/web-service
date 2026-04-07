{{- define "activity-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "activity-service.fullname" -}}
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
{{- define "activity-service.labels" -}}
helm.sh/chart: {{ include "activity-service.name" . }}-{{ .Chart.Version }}
{{ include "activity-service.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "activity-service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "activity-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "activity-service.serviceAccountName" -}}
{{- include "activity-service.fullname" . }}
{{- end }}
