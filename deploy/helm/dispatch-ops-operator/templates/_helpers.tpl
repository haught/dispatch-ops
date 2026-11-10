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

{{- /*
The installation's namespace: installation.namespace, else the first watched
namespace, else (watching all namespaces) dispatch-ops. Never by default the
release namespace, where a task manager could start pods as the operator's
ServiceAccount.
*/ -}}
{{- define "dispatch-ops-operator.installationNamespace" -}}
{{- if .Values.installation.namespace -}}
{{- .Values.installation.namespace -}}
{{- else -}}
{{- $watched := include "dispatch-ops-operator.watchNamespaces" . | fromJsonArray -}}
{{- if $watched -}}{{ first $watched }}{{- else -}}dispatch-ops{{- end -}}
{{- end -}}
{{- end -}}

{{- /* Labels for the installation's objects, which aren't the operator's. */ -}}
{{- define "dispatch-ops-operator.installationLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- /*
The namespaces the operator watches, as a JSON list: watchNamespaces, or []
(every namespace) with watchAllNamespaces. An empty watchNamespaces fails, so
the cluster-wide mode is only ever chosen by name.
*/ -}}
{{- define "dispatch-ops-operator.watchNamespaces" -}}
{{- if .Values.watchAllNamespaces -}}
{{- list | toJson -}}
{{- else -}}
{{- $watched := .Values.watchNamespaces -}}
{{- if empty $watched -}}
{{- fail "watchNamespaces is empty: list the namespaces whose DispatchOps the operator manages, or set watchAllNamespaces: true to manage every namespace with cluster-wide permissions" -}}
{{- end -}}
{{- if not (kindIs "slice" $watched) -}}
{{- fail "watchNamespaces must be a list of namespaces, e.g. [dispatch-ops]" -}}
{{- end -}}
{{- range $watched -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$" (toString .)) -}}
{{- fail (printf "watchNamespaces: %q is not a namespace name" (toString .)) -}}
{{- end -}}
{{- end -}}
{{- $watched | toJson -}}
{{- end -}}
{{- end -}}

{{- /*
Each watched namespace's default job namespace, <namespace><jobNamespaceSuffix>,
as a JSON map. Empty with watchAllNamespaces or an empty suffix.
*/ -}}
{{- define "dispatch-ops-operator.defaultJobNamespaces" -}}
{{- $out := dict -}}
{{- $suffix := .Values.jobNamespaceSuffix | default "" -}}
{{- if and $suffix (not .Values.watchAllNamespaces) -}}
{{- $watched := include "dispatch-ops-operator.watchNamespaces" . | fromJsonArray -}}
{{- range $src := $watched -}}
{{- $jobNS := printf "%s%s" $src $suffix -}}
{{- if or (gt (len $jobNS) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $jobNS)) -}}
{{- fail (printf "jobNamespaceSuffix: %s is not a valid namespace name (at most 63 lowercase letters, digits and -)" $jobNS) -}}
{{- end -}}
{{- if has $jobNS $watched -}}
{{- fail (printf "jobNamespaceSuffix: %s, the job namespace of %s, is also in watchNamespaces. A namespace that hosts DispatchOps can't be another installation's job namespace" $jobNS $src) -}}
{{- end -}}
{{- $_ := set $out $src $jobNS -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{- /*
Every job namespace the operator allows, as a JSON map of job namespace to the
namespaces whose DispatchOps may use it: allowedJobNamespaces plus the default
job namespaces. Fails on a value that isn't a map of job namespace to a
non-empty list of namespaces, and on a job namespace that is also watched: a
namespace that hosts DispatchOps (and so their encryption keys and database
Secrets) must never be another installation's job namespace.
*/ -}}
{{- define "dispatch-ops-operator.allowedJobNamespacesMap" -}}
{{- $allowed := .Values.allowedJobNamespaces | default dict -}}
{{- if not (kindIs "map" $allowed) -}}
{{- fail "allowedJobNamespaces must map each job namespace to the namespaces whose DispatchOps may run jobs there, e.g. {dispatch-ops-jobs: [dispatch-ops]}" -}}
{{- end -}}
{{- $watched := include "dispatch-ops-operator.watchNamespaces" . | fromJsonArray -}}
{{- $out := dict -}}
{{- range $jobNS := keys $allowed | sortAlpha -}}
{{- $sources := get $allowed $jobNS -}}
{{- if or (not (kindIs "slice" $sources)) (empty $sources) -}}
{{- fail (printf "allowedJobNamespaces.%s must list the namespaces whose DispatchOps may run jobs there, e.g. [dispatch-ops]" $jobNS) -}}
{{- end -}}
{{- $_ := set $out $jobNS $sources -}}
{{- end -}}
{{- $defaults := include "dispatch-ops-operator.defaultJobNamespaces" . | fromJson -}}
{{- range $src, $jobNS := $defaults -}}
{{- $_ := set $out $jobNS (append (get $out $jobNS | default list) $src) -}}
{{- end -}}
{{- range $jobNS, $sources := $out -}}
{{- if has $jobNS $watched -}}
{{- fail (printf "allowedJobNamespaces: %s is also in watchNamespaces. A namespace that hosts DispatchOps can't be another installation's job namespace, whose task manager could create pods there that mount its Secrets; give jobs a namespace of their own" $jobNS) -}}
{{- end -}}
{{- range $src := $sources -}}
{{- if and $watched (not (has $src $watched)) -}}
{{- fail (printf "allowedJobNamespaces.%s: %s is not in watchNamespaces" $jobNS $src) -}}
{{- end -}}
{{- end -}}
{{- $_ := set $out $jobNS ($sources | uniq | sortAlpha) -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{- /* The allowed job namespaces as the operator's ALLOWED_JOB_NAMESPACES: jobs-a=team-a,jobs-b=team-b|team-c. */ -}}
{{- define "dispatch-ops-operator.allowedJobNamespaces" -}}
{{- $allowed := include "dispatch-ops-operator.allowedJobNamespacesMap" . | fromJson -}}
{{- $entries := list -}}
{{- range $jobNS := keys $allowed | sortAlpha -}}
{{- $entries = append $entries (printf "%s=%s" $jobNS (join "|" (get $allowed $jobNS))) -}}
{{- end -}}
{{- join "," $entries -}}
{{- end -}}

{{- /* The default job namespaces as the operator's DEFAULT_JOB_NAMESPACES: team-a=team-a-jobs,team-b=team-b-jobs. */ -}}
{{- define "dispatch-ops-operator.defaultJobNamespacesFlag" -}}
{{- $defaults := include "dispatch-ops-operator.defaultJobNamespaces" . | fromJson -}}
{{- $entries := list -}}
{{- range $src := keys $defaults | sortAlpha -}}
{{- $entries = append $entries (printf "%s=%s" $src (get $defaults $src)) -}}
{{- end -}}
{{- join "," $entries -}}
{{- end -}}

{{- /* The namespaces the operator gets a Role in with watchNamespaces: those and the allowed job namespaces. */ -}}
{{- define "dispatch-ops-operator.accessibleNamespaces" -}}
{{- $watched := include "dispatch-ops-operator.watchNamespaces" . | fromJsonArray -}}
{{- $jobNamespaces := keys (include "dispatch-ops-operator.allowedJobNamespacesMap" . | fromJson) | sortAlpha -}}
{{- uniq (concat $watched $jobNamespaces) | toJson -}}
{{- end -}}

{{- /*
The namespaces the chart creates with namespaces.create: the ones the operator
has a Role in, and the installation's (when watching all namespaces, the only
one it knows of besides the allowed job namespaces).
*/ -}}
{{- define "dispatch-ops-operator.createdNamespaces" -}}
{{- $namespaces := include "dispatch-ops-operator.accessibleNamespaces" . | fromJsonArray -}}
{{- if .Values.installation.enabled -}}
{{- $namespaces = append $namespaces (include "dispatch-ops-operator.installationNamespace" .) -}}
{{- end -}}
{{- uniq $namespaces | toJson -}}
{{- end -}}

{{- /*
Refuses the release namespace as a watched, job or installation namespace: a
task manager can start pods there as any ServiceAccount, the operator's
included, and so take over its permissions.
*/ -}}
{{- define "dispatch-ops-operator.validateNamespaces" -}}
{{- $namespaces := include "dispatch-ops-operator.accessibleNamespaces" . | fromJsonArray -}}
{{- if .Values.installation.enabled -}}
{{- $namespaces = append $namespaces (include "dispatch-ops-operator.installationNamespace" .) -}}
{{- end -}}
{{- if has .Release.Namespace $namespaces -}}
{{- fail (printf "%s, the operator's namespace, is also a watched, job or installation namespace. A task manager there could start pods as the operator's ServiceAccount; install the chart in a namespace of its own (e.g. dispatch-ops-system)" .Release.Namespace) -}}
{{- end -}}
{{- end -}}
