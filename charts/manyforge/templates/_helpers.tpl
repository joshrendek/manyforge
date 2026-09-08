{{/*
Expand the name of the chart.
*/}}
{{- define "manyforge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to
this (by the DNS naming spec).
*/}}
{{- define "manyforge.fullname" -}}
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
{{- define "manyforge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "manyforge.labels" -}}
helm.sh/chart: {{ include "manyforge.chart" . }}
{{ include "manyforge.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "manyforge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "manyforge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Shared nonsecret mail settings. The pre-install migrate hook cannot depend on
the release ConfigMap, so both templates render this same mapping.
*/}}
{{- define "manyforge.outboundMailConfig" -}}
MANYFORGE_OUTBOUND_MAIL_DISABLED: {{ .Values.outboundMailDisabled | quote }}
MANYFORGE_OUTBOUND_PROVIDER: {{ .Values.outboundMail.provider | quote }}
MANYFORGE_OUTBOUND_FROM_EMAIL: {{ .Values.outboundMail.fromEmail | quote }}
MANYFORGE_OUTBOUND_FROM_NAME: {{ .Values.outboundMail.fromName | quote }}
MANYFORGE_OUTBOUND_SES_REGION: {{ .Values.outboundMail.sesRegion | quote }}
MANYFORGE_OUTBOUND_SES_CONFIGURATION_SET: {{ .Values.outboundMail.sesConfigurationSet | quote }}
MANYFORGE_SMTP_HOST: {{ .Values.smtp.host | quote }}
MANYFORGE_SMTP_PORT: {{ .Values.smtp.port | quote }}
{{- end }}

{{/*
Only the selected backend's credentials are required in the app and migrate
containers. Explicitly disabled mail can boot without any mail Secret.
*/}}
{{- define "manyforge.outboundMailSecretEnv" -}}
{{- if not .Values.outboundMailDisabled }}
{{- if and (eq .Values.outboundMail.provider "smtp") .Values.secrets.smtp.secretName }}
- name: MANYFORGE_SMTP_USER
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.smtp.secretName | quote }}
      key: {{ .Values.secrets.smtp.userKey | quote }}
- name: MANYFORGE_SMTP_PASS
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.smtp.secretName | quote }}
      key: {{ .Values.secrets.smtp.passKey | quote }}
{{- else if .Values.secrets.outboundMail.secretName }}
{{- if eq .Values.outboundMail.provider "resend" }}
- name: MANYFORGE_OUTBOUND_RESEND_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.outboundMail.secretName | quote }}
      key: {{ .Values.secrets.outboundMail.resendAPIKeyKey | quote }}
{{- else if eq .Values.outboundMail.provider "ses" }}
- name: MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.outboundMail.secretName | quote }}
      key: {{ .Values.secrets.outboundMail.sesAccessKeyIDKey | quote }}
- name: MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.outboundMail.secretName | quote }}
      key: {{ .Values.secrets.outboundMail.sesSecretAccessKeyKey | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
