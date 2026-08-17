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
{{- else if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- fail "serviceAccount.create is false but serviceAccount.name is empty. The usual Helm fallback here is the namespace's \"default\" ServiceAccount, and this chart will not do that: the RoleBinding grants pods create/delete, pods/exec create and secrets get/list/create/delete, so binding it to \"default\" would hand agent-management authority to every pod in the namespace that does not name a ServiceAccount. Set serviceAccount.name to an existing ServiceAccount, or leave serviceAccount.create true." }}
{{- end }}
{{- end }}

{{/*
Name for the cluster-scoped RBAC pair.

Deliberately different from the namespaced pair: scion-hub.fullname is a
function of the release name only, so two installs of the same release name in
different namespaces - a per-team or per-environment layout, which is normal -
would collide on one cluster-scoped object. Under helm install that is an
ownership error and survivable. Under helm template | kubectl apply, or a GitOps
pipeline, the second apply silently rewrites the first's ClusterRoleBinding
subject and points cluster-wide pods/exec and secrets authority at another
namespace's ServiceAccount. Including the namespace makes that unrepresentable.
*/}}
{{- define "scion-hub.clusterRoleName" -}}
{{- printf "%s-%s-agents" (include "scion-hub.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" }}
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
{{- $explicit := (.Values.updateStrategy | default dict).type | default "" }}
{{- if $explicit }}
{{- $explicit }}
{{- else if gt (int .Values.replicaCount) 1 }}
{{- "RollingUpdate" }}
{{- else }}
{{- "Recreate" }}
{{- end }}
{{- end }}

{{/*
Assert that a flag or variable NAME does not announce credential material.

Call as:
  {{- include "scion-hub.assertNoCredentialName" (dict "name" $n "source" "hub.args flag") }}

THIS IS NOT REDUNDANT WITH THE VALUE CHECK BELOW, and the next person to read
both will suspect that it is. It is not, for one reason: a credential value is
not distinguishable by inspection. --admin-token=hunter2 is caught here and
cannot be caught there, because "hunter2" has no shape - no scheme, no prefix,
nothing to match on. It is an ordinary word. The value axis reads what the value
looks like; the name axis reads what the operator called it, and when the value
is unremarkable the name is the only signal left.

This is not hypothetical. An earlier revision of this guard dropped the name
axis in favour of value shapes, on the reasoning that names are what produce
false positives. That regressed exactly this case, and it did so while looking
like a security improvement. Do not remove either axis.

The rule is position, not substring, and the distinction is the whole reason this
is not a naive contains-check. A credential noun at the END of a flag name says
what the value IS: --admin-token, --api-key, --session-secret, --gh-pat. The same
noun at the START says what the flag is ABOUT, and the value is then a duration,
a count or a project name: --token-ttl, --secret-manager-project. A plural is
also about, not is: --max-tokens is a limit.

So this matches a credential noun only as a whole trailing segment, or as the
entire name. Substring matching was tried first and rejected: it fired on
--max-tokens, --token-ttl and --secret-manager-project, and because hub.args is
append-only with no override, a false positive there is unusable rather than
merely annoying.
*/}}
{{- define "scion-hub.assertNoCredentialName" -}}
{{- $n := lower (toString .name) }}
{{- if regexMatch "(^|-)(secret|password|passwd|token|credential|key|apikey|pat)$" $n }}
{{- fail (printf "%s %q names credential material. Anything on argv or in a plain environment value is readable by anyone with pod read access; credentials are delivered through a Secret. (The match is on a trailing word: --token-ttl and --max-tokens are fine, --admin-token is not.)" .source $n) }}
{{- end }}
{{- end }}

{{/*
Assert that a single value does not look like credential material.

Call as:
  {{- include "scion-hub.assertNoCredential" (dict "value" $v "source" "hub.args entry") }}

It renders nothing and fails the render when the value matches. Defined once and
shared, so every place in this chart that puts an operator-supplied value
somewhere world-readable - argv today, environment values later - applies the
same test rather than each growing its own near-miss version of it.

It inspects the VALUE, not the name of the thing holding it. Name-based checks
miss the case that actually occurs: postgres://scion:hunter2@10.0.0.1/scion
carries a password and contains none of the words a name pattern would look for.
A name-based check is still worth having, but it is a different axis and belongs
with whatever owns the name.

What it catches: credentials in URL userinfo, and a handful of well-known
credential prefixes. What it does NOT catch, and cannot: an opaque
high-entropy string with no recognisable shape. There is no reliable way to tell
one of those from a legitimate identifier, and a heuristic that guessed would
reject real values with no override, which for an append-only list is worse than
the hole. Do not add an entropy heuristic here.

The matched value is redacted or truncated in the failure message. A guard whose
error message prints the secret it just caught has moved the secret from argv
into CI logs.
*/}}
{{- define "scion-hub.assertNoCredential" -}}
{{- $s := toString .value }}
{{- $source := .source }}
{{- if regexMatch "://[^/@[:space:]]+:[^/@[:space:]]+@" $s }}
{{- fail (printf "%s %q embeds credentials in a URL (scheme://user:password@host). Anything on argv or in a plain environment value is readable by anyone with pod read access; credentials are delivered through a Secret." $source (regexReplaceAll "://[^/@[:space:]]+:[^/@[:space:]]+@" $s "://REDACTED@")) }}
{{- end }}
{{- if regexMatch "(?i)(^|=)(sk-[A-Za-z0-9]|ghp_|gho_|ghs_|github_pat_|xox[abprs]-|AKIA[A-Z0-9]{8}|-----BEGIN )" $s }}
{{- fail (printf "%s (starting %q) has the shape of a credential. Anything on argv or in a plain environment value is readable by anyone with pod read access; credentials are delivered through a Secret." $source (trunc 10 $s)) }}
{{- end }}
{{- end }}

{{/*
The hub command line.

--hosted and --host 0.0.0.0 are rendered unconditionally and no value can remove
them. Without hosted mode the server applies workstation defaults, derives
auth-enabled from a development flag and forces its bind address to 127.0.0.1,
which behind a load balancer is unreachable and unauthenticated by accident.

--production is deliberately not emitted: it is a deprecated alias of --hosted.

hub.args appends to this list and can never replace it. Two guards apply to
anything appended.

The RESERVED flags are grouped by WHY they are reserved, in four lists below,
and the grouping is load-bearing rather than tidy. Most entries are reserved
because the chart already sets them, and those can be checked against the
rendered arguments. Two are reserved because nothing may ever set them - not the
operator and not a future phase of this chart - and for those, finding no match
in the rendered arguments is the expected steady state, not evidence that the
entry is stale. A flat list under one comment invites the next maintainer to
verify each entry against what the chart renders, conclude that --config was
added in error, and remove it; that removal would look exactly like tidying and
would reopen the largest hole in this guard. The failure messages differ per
group so the reason is visible at the point it fires.

Every entry was checked against cmd/server.go's actual flag set rather than
guessed - the first version of this list was guessed and missed five flags,
including --config.

SHORTHANDS MUST BE LISTED BY LETTER. The normalisation below reduces -x and --x
to the same token, so a shorthand is only caught if its letter is on the list.
"c" is on it for --config, and -c is the only shorthand defined on server start
today. Any future flag registered with a *VarP form needs its letter added here.

The CREDENTIAL check is scion-hub.assertNoCredential, applied to every rendered
argument. It looks at values rather than flag names - see its own comment for
why, and for what it deliberately does not catch. It replaced a substring match
for "secret", "password" and "token" anywhere in the argument, which both missed
the real case (a DSN) and rejected legitimate flags such as --max-tokens with no
way to override; names are handled by the exact-match reserved list instead.
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
{{- /*
Four lists, not one, because they are reserved for four different reasons and a
flat list loses the reason. See the block comment above for why that matters:
the entries below are NOT all verifiable by checking what the chart renders.
*/}}

{{- /*
1. The chart renders these itself. A second value would contradict the manifest.
   Verifiable against the rendered args - if the chart stops setting one of
   these, it should leave this list at the same time.
*/}}
{{- $setByChart := list "hosted" "production" "host" "web-port" "port" }}

{{- /*
2. NOTHING may pass these. Not the operator, and not a future phase of this
   chart either.

   DO NOT REMOVE THESE BECAUSE THE CHART DOES NOT SET THEM. That is the point of
   them, not evidence that they were added by mistake. --config redirects
   config.LoadGlobalConfig away from $HOME/.scion/settings.yaml, which is where
   every value this chart renders is delivered; a hub started with it runs on a
   file the chart has never seen while every guard, the schema and the hub-id
   annotation continue to report the operator's intent. There is no legitimate
   reason for this chart to emit it, so "nothing in the rendered args matches
   this entry" is the expected steady state forever.
*/}}
{{- $neverPassed := list "config" "c" }}

{{- /*
3. The chart's configuration owns these, so passing them on argv creates a
   second source for a setting that is already delivered through the settings
   file. Judgment call worth recording: base-url sits here rather than in list 2
   because a later phase could legitimately choose argv as its delivery channel
   for it, whereas for --config that choice would be self-defeating.
*/}}
{{- $ownedByConfig := list "admin-emails" "base-url" "db" "storage-bucket" "storage-dir" }}

{{- /*
4. These weaken authentication or place credentials where they can be read.
*/}}
{{- $unsafeToPass := list "session-secret" "dev-auth" "enable-test-login" "web-assets-dir" }}
{{- range $raw := .Values.hub.args }}
{{- $arg := toString $raw }}
{{- if ne $arg (trim $arg) }}
{{- fail (printf "hub.args entry %q has leading or trailing whitespace. pflag would read it as a positional argument rather than a flag, and the hub would crash-loop instead of failing here." $arg) }}
{{- end }}
{{- if and (hasPrefix "-" $arg) (regexMatch "[[:space:]]" $arg) }}
{{- fail (printf "hub.args entry %q contains whitespace. Pass a flag and its value as two separate array elements; a single element with a space in it is an unknown flag name to pflag, so the guards below would not see the flag and the hub would crash-loop at startup." $arg) }}
{{- end }}
{{- $flag := trimPrefix "-" (trimPrefix "--" (first (splitList "=" $arg))) }}
{{- if has $flag $setByChart }}
{{- fail (printf "hub.args may not contain -%s: the chart sets it, and a second value would contradict the rendered manifest - disabling hosted mode, unbinding the listener, or desynchronising the probes from hub.webPort." $flag) }}
{{- end }}
{{- if has $flag $neverPassed }}
{{- fail (printf "hub.args may not contain -%s: it redirects the hub's entire configuration load away from the settings file this chart delivers, so the hub would run on a file the chart has never seen while every rendered value continued to report the operator's intent." $flag) }}
{{- end }}
{{- if has $flag $ownedByConfig }}
{{- fail (printf "hub.args may not contain -%s: the chart's configuration already owns this setting, and passing it on argv creates a second source that silently wins over the rendered one." $flag) }}
{{- end }}
{{- if has $flag $unsafeToPass }}
{{- fail (printf "hub.args may not contain -%s: it weakens authentication or places credential material where anyone with pod read access can read it." $flag) }}
{{- end }}
{{- if hasPrefix "-" $arg }}
{{- include "scion-hub.assertNoCredentialName" (dict "name" $flag "source" "hub.args flag") }}
{{- end }}
{{- $args = append $args $arg }}
{{- end }}
{{- range $arg := $args }}
{{- include "scion-hub.assertNoCredential" (dict "value" $arg "source" "hub.args entry") }}
{{- end }}
{{- toYaml $args }}
{{- end }}
