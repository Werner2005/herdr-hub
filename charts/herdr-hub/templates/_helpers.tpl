{{- define "herdr-hub.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "herdr-hub.fullname" -}}
{{- if .Values.subdomain -}}
{{- $subdomain := lower .Values.subdomain -}}
{{- if or (gt (len $subdomain) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $subdomain)) -}}
{{- fail "subdomain must be a valid DNS label (lowercase letters, digits, and hyphens; max 63 characters)" -}}
{{- end -}}
{{- $subdomain -}}
{{- else if .Values.fullnameOverride -}}
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

{{- define "herdr-hub.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "herdr-hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "herdr-hub.selectorLabels" -}}
app.kubernetes.io/name: {{ include "herdr-hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
