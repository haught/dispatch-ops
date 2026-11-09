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

{{- /*
allowedJobNamespaces as the operator's ALLOWED_JOB_NAMESPACES:
jobs-a=team-a,jobs-b=team-b|team-c. Fails on a value that isn't a map of
job namespace to a non-empty list of namespaces, and on a job namespace
that is also watched: a namespace that hosts DispatchOps (and so their
encryption keys and database Secrets) must never be another
installation's job namespace.
*/ -}}
{{- define "dispatch-ops-operator.allowedJobNamespaces" -}}
{{- $allowed := .Values.allowedJobNamespaces | default dict -}}
{{- if not (kindIs "map" $allowed) -}}
{{- fail "allowedJobNamespaces must map each job namespace to the namespaces whose DispatchOps may run jobs there, e.g. {dispatch-jobs: [dispatch-ops]}" -}}
{{- end -}}
{{- $watched := .Values.watchNamespaces | default list -}}
{{- $entries := list -}}
{{- range $jobNS := keys $allowed | sortAlpha -}}
{{- $sources := get $allowed $jobNS -}}
{{- if or (not (kindIs "slice" $sources)) (empty $sources) -}}
{{- fail (printf "allowedJobNamespaces.%s must list the namespaces whose DispatchOps may run jobs there, e.g. [dispatch-ops]" $jobNS) -}}
{{- end -}}
{{- if has $jobNS $watched -}}
{{- fail (printf "allowedJobNamespaces: %s is also in watchNamespaces. A namespace that hosts DispatchOps can't be another installation's job namespace, whose task manager could create pods there that mount its Secrets; give jobs a namespace of their own" $jobNS) -}}
{{- end -}}
{{- range $src := $sources -}}
{{- if and $watched (not (has $src $watched)) -}}
{{- fail (printf "allowedJobNamespaces.%s: %s is not in watchNamespaces" $jobNS $src) -}}
{{- end -}}
{{- end -}}
{{- $entries = append $entries (printf "%s=%s" $jobNS (join "|" $sources)) -}}
{{- end -}}
{{- join "," $entries -}}
{{- end -}}

{{- /* The namespaces the operator gets a Role in with watchNamespaces set: those and the allowed job namespaces. */ -}}
{{- define "dispatch-ops-operator.accessibleNamespaces" -}}
{{- $_ := include "dispatch-ops-operator.allowedJobNamespaces" . -}}
{{- $jobNamespaces := keys (.Values.allowedJobNamespaces | default dict) | sortAlpha -}}
{{- uniq (concat .Values.watchNamespaces $jobNamespaces) | toJson -}}
{{- end -}}
