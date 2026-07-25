{{- define "superops.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "superops.fullname" -}}
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

{{- define "superops.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "superops.labels" -}}
app.kubernetes.io/name: {{ include "superops.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: superops
helm.sh/chart: {{ include "superops.chart" . }}
{{- end }}

{{- define "superops.selectorLabels" -}}
app.kubernetes.io/name: {{ include "superops.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "superops.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "superops.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Subchart resource names.

A subchart names itself <Release.Name>-<chartName> (Bitnami's
common.names.fullname), NOT <parentFullname>-<chartName>. This chart used to
derive every hostname from superops.fullname, which only coincides when the
release is literally named "superops": `helm install prod ./superops` produced
DB_HOST=prod-superops-postgresql against a Service actually called
prod-postgresql.

Usage:
  include "superops.subchartFullname" (dict "release" .Release.Name "chart" "postgresql" "values" .Values.postgresql)
*/}}
{{- define "superops.subchartFullname" -}}
{{- $values := .values | default dict -}}
{{- if $values.fullnameOverride -}}
{{- $values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .chart $values.nameOverride -}}
{{- if contains $name .release -}}
{{- .release | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .release $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "superops.postgresqlHost" -}}
{{- if .Values.postgresql.enabled -}}
{{- include "superops.subchartFullname" (dict "release" .Release.Name "chart" "postgresql" "values" .Values.postgresql) -}}
{{- else -}}
{{- required "externalDatabase.host is required when postgresql.enabled=false" .Values.externalDatabase.host -}}
{{- end -}}
{{- end -}}

{{- define "superops.postgresqlPort" -}}
{{- if .Values.postgresql.enabled -}}5432{{- else -}}{{ .Values.externalDatabase.port | default 5432 }}{{- end -}}
{{- end -}}

{{/* Bitnami redis exposes the writable node as <fullname>-master. */}}
{{- define "superops.redisHost" -}}
{{- if .Values.redis.enabled -}}
{{- printf "%s-master" (include "superops.subchartFullname" (dict "release" .Release.Name "chart" "redis" "values" .Values.redis)) -}}
{{- else -}}
{{- required "externalRedis.host is required when redis.enabled=false" .Values.externalRedis.host -}}
{{- end -}}
{{- end -}}

{{- define "superops.natsHost" -}}
{{- if .Values.nats.enabled -}}
{{- include "superops.subchartFullname" (dict "release" .Release.Name "chart" "nats" "values" .Values.nats) -}}
{{- else -}}
{{- required "externalNats.host is required when nats.enabled=false" .Values.externalNats.host -}}
{{- end -}}
{{- end -}}

{{- define "superops.minioHost" -}}
{{- if .Values.minio.enabled -}}
{{- include "superops.subchartFullname" (dict "release" .Release.Name "chart" "minio" "values" .Values.minio) -}}
{{- else -}}
{{- required "externalMinio.host is required when minio.enabled=false" .Values.externalMinio.host -}}
{{- end -}}
{{- end -}}

{{/*
Pod annotations that force a rollout when the rendered config changes. Without
them a `helm upgrade` after a ConfigMap edit changes nothing observable: pods
keep the environment they were started with.
*/}}
{{- define "superops.configChecksums" -}}
checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
checksum/secret: {{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}
{{- end -}}
