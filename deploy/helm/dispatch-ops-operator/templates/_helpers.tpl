{{- define "dispatch-ops-operator.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "dispatch-ops-operator.selectorLabels" -}}
app.kubernetes.io/name: dispatch-ops-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "dispatch-ops-operator.labels" -}}
{{ include "dispatch-ops-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "dispatch-ops-operator.serviceAccountName" -}}
{{- .Values.serviceAccount.name | default (include "dispatch-ops-operator.fullname" .) -}}
{{- end -}}

{{- define "dispatch-ops-operator.installationNamespace" -}}
{{- .Values.installation.namespace | default .Release.Namespace -}}
{{- end -}}

{{- /* Labels for the installation's objects, which aren't the operator's. */ -}}
{{- define "dispatch-ops-operator.installationLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}
