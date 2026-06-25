{{- define "davi.name" -}}davi{{- end -}}

{{- define "davi.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "davi.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
TAK cert Secret name. When the operator sets discovery.sidecar.tak.certSecret
explicitly that name is used; otherwise the chart defaults to
"<release>-tak-client" so persistence is enabled out-of-the-box without
operator configuration.
*/}}
{{- define "davi.takCertSecret" -}}
{{- if .Values.discovery.sidecar.tak.certSecret -}}
  {{- .Values.discovery.sidecar.tak.certSecret -}}
{{- else -}}
  {{- printf "%s-tak-client" .Release.Name -}}
{{- end -}}
{{- end -}}

{{- define "davi.host" -}}
{{- printf "%s.public.%s.%s" .Values.subdomain .Values.hostname .Values.domain -}}
{{- end -}}

{{- define "davi.labels" -}}
app.kubernetes.io/name: {{ include "davi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "davi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "davi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
PostgREST gateway upstream resolution. The browser hits /postgres/* which
nginx proxies to one of:
  1. The in-chart PostgREST Service (when .Values.postgrest.enabled).
  2. An external PostgREST service named in .Values.backends.postgres.gatewayHost.
Returns "" if neither is configured (in which case the location block is
omitted entirely from the nginx config).
*/}}
{{- define "davi.postgrestUpstream" -}}
{{- if .Values.postgrest.enabled -}}
http://{{ include "davi.fullname" . }}-postgrest:{{ .Values.postgrest.service.port }}
{{- else if .Values.backends.postgres.gatewayHost -}}
{{ .Values.backends.postgres.gatewayScheme }}://{{ .Values.backends.postgres.gatewayHost }}:{{ .Values.backends.postgres.gatewayPort }}
{{- end -}}
{{- end -}}
