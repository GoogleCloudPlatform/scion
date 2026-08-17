{{/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/}}

{{/* Chart name, overridable. */}}
{{- define "scion-hub.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified release name. */}}
{{- define "scion-hub.fullname" -}}
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

{{- define "scion-hub.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "scion-hub.labels" -}}
helm.sh/chart: {{ include "scion-hub.chart" . }}
{{ include "scion-hub.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: scion
{{- end }}

{{- define "scion-hub.selectorLabels" -}}
app.kubernetes.io/name: {{ include "scion-hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "scion-hub.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "scion-hub.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The hub ID, emitted verbatim.

There is deliberately no generator, no default and no derivation here. The value
the operator supplied is the value that is rendered: nothing is appended,
trimmed, hashed or substituted, and nothing in this chart may make the hub ID a
function of anything Helm recomputes between renders - not the release revision,
not the release name, not a random or UUID generator, not the pod hostname. Two
renders that differ only in release revision must produce a byte-identical hub
ID, and CI greps this chart for the generator functions by name, so do not
reintroduce one even in a comment.
*/}}
{{- define "scion-hub.hubId" -}}
{{- $id := required "hub.hubId is required: set it to an explicit, stable hub ID. The chart never generates one - without an explicit value the hub derives its ID from its hostname, which is random per pod." .Values.hub.hubId }}
{{- if ne $id (trim $id) }}
{{- fail "hub.hubId must not have leading or trailing whitespace: the value is used verbatim." }}
{{- end }}
{{- $id }}
{{- end }}

{{/* Namespace agent pods are created in, and the namespace RBAC is scoped to. */}}
{{- define "scion-hub.agentNamespace" -}}
{{- $rbacNs := .Values.rbac.agentNamespace | default "" }}
{{- $runtimeNs := .Values.runtime.namespace | default "" }}
{{- if and $rbacNs $runtimeNs (ne $rbacNs $runtimeNs) }}
{{- fail (printf "rbac.agentNamespace (%s) and runtime.namespace (%s) disagree. They name the same namespace; set one, or set both to the same value." $rbacNs $runtimeNs) }}
{{- end }}
{{- coalesce $rbacNs $runtimeNs .Release.Namespace }}
{{- end }}

{{/*
The identity fields of the security context, rendered at both the pod level and
the hub container level.

runAsNonRoot is a literal true. It is not read from a value, there is no knob
for it, and hub.securityContext rejects unknown properties so it cannot be
reintroduced as an override. The point is not hardening in the abstract: the
artifact this project publishes under the name "scion-hub" runs as root, and an
operator who reasons from the artifact name will eventually point the chart at
it. With a loose security context that image runs, as root, and writes
root-owned files into a share that agents running as uid 1000 cannot write -
a failure that surfaces days later and looks like a storage problem. With
runAsNonRoot the pod fails admission immediately and says why.

runAsUser and runAsGroup stay configurable because they must be able to match
the workspace share's uid and gid. Zero is rejected here as well as in the
schema, so relaxing the schema alone cannot reopen the hole.
*/}}
{{- define "scion-hub.nonRootSecurityContext" -}}
{{- $uid := int .Values.hub.securityContext.runAsUser }}
{{- $gid := int .Values.hub.securityContext.runAsGroup }}
{{- if eq $uid 0 }}
{{- fail "hub.securityContext.runAsUser may not be 0: the hub always runs with runAsNonRoot, so uid 0 would fail pod admission rather than grant root." }}
{{- end }}
{{- if eq $gid 0 }}
{{- fail "hub.securityContext.runAsGroup may not be 0: files the hub writes to the workspace share would be group 0 and unwritable by agents." }}
{{- end -}}
runAsNonRoot: true
runAsUser: {{ $uid }}
runAsGroup: {{ $gid }}
{{- end }}

{{/*
Permissions the hub needs to run agent pods. One definition, shared by the
namespaced Role and the ClusterRole, so the two cannot drift.

persistentvolumeclaims is load-bearing: on the local workspace backend the hub
creates a ReadWriteMany PVC per shared directory, and every project gets one by
default. Without these verbs that path fails with a permission error.
*/}}
{{- define "scion-hub.rbacRules" -}}
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  resources: ["pods/exec", "pods/attach", "pods/portforward"]
  verbs: ["create"]
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["create", "get", "list", "delete"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["get", "list", "watch"]
{{- end }}

{{/* Container image reference. digest wins; tag defaults to the chart appVersion. */}}
{{- define "scion-hub.image" -}}
{{- if and .Values.image.tag .Values.image.digest }}
{{- fail "image.tag and image.digest are mutually exclusive: set image.digest (preferred) or image.tag, not both." }}
{{- end }}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/* Deployment update strategy: Recreate at one replica, RollingUpdate above it. */}}
{{- define "scion-hub.updateStrategyType" -}}
{{- if .Values.updateStrategy.type }}
{{- .Values.updateStrategy.type }}
{{- else if gt (int .Values.replicaCount) 1 }}
{{- "RollingUpdate" }}
{{- else }}
{{- "Recreate" }}
{{- end }}
{{- end }}

{{/*
The hub command line.

--hosted and --host 0.0.0.0 are rendered unconditionally and no value can remove
them. Without hosted mode the server applies workstation defaults, derives
auth-enabled from a development flag and forces its bind address to 127.0.0.1,
which behind a load balancer is unreachable and unauthenticated by accident.

--production is deliberately not emitted: it is a deprecated alias of --hosted.
*/}}
{{- define "scion-hub.hubArgs" -}}
{{- $args := list
    "server" "start"
    "--foreground"
    "--hosted"
    "--enable-hub"
    "--enable-runtime-broker"
    "--enable-web"
    "--web-port" (printf "%d" (int .Values.hub.webPort))
    "--host" "0.0.0.0"
    "--auto-provide"
    "--global" }}
{{- $reserved := list "hosted" "production" "host" "web-port" "port" "session-secret" "dev-auth" "enable-test-login" }}
{{- range $arg := .Values.hub.args }}
{{- $arg := toString $arg }}
{{- $flag := trimPrefix "-" (trimPrefix "--" (first (splitList "=" $arg))) }}
{{- if has $flag $reserved }}
{{- fail (printf "hub.args may not contain --%s: it is set by the chart and overriding it would either disable hosted mode, unbind the listener, desynchronise the probes from hub.webPort, or put a secret on argv." $flag) }}
{{- end }}
{{- $args = append $args $arg }}
{{- end }}
{{- range $arg := $args }}
{{- if regexMatch "(?i)(secret|password|token)" (toString $arg) }}
{{- fail (printf "argument %q looks like secret material. Anything on argv is readable by anyone with pod read access; secrets are delivered through a Secret, never as an argument." $arg) }}
{{- end }}
{{- end }}
{{- toYaml $args }}
{{- end }}
