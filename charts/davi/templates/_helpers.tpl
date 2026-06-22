{{- define "davi.name" -}}davi{{- end -}}

{{- define "davi.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "davi.name" .) | trunc 63 | trimSuffix "-" -}}
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
