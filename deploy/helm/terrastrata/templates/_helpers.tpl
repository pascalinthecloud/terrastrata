{{/* Expand the name of the chart. */}}
{{- define "terrastrata.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "terrastrata.fullname" -}}
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

{{- define "terrastrata.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "terrastrata.labels" -}}
helm.sh/chart: {{ include "terrastrata.chart" . }}
{{ include "terrastrata.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Selector labels. */}}
{{- define "terrastrata.selectorLabels" -}}
app.kubernetes.io/name: {{ include "terrastrata.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ServiceAccount name. */}}
{{- define "terrastrata.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "terrastrata.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Secret name holding the auth token. */}}
{{- define "terrastrata.authSecretName" -}}
{{- if .Values.auth.existingSecret -}}{{ .Values.auth.existingSecret }}{{- else -}}{{ include "terrastrata.fullname" . }}-auth{{- end -}}
{{- end -}}

{{/* Secret name holding S3 credentials. */}}
{{- define "terrastrata.s3SecretName" -}}
{{- if .Values.s3.existingSecret -}}{{ .Values.s3.existingSecret }}{{- else -}}{{ include "terrastrata.fullname" . }}-s3{{- end -}}
{{- end -}}
