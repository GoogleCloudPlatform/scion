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

{{/*
Container image reference. digest wins; tag defaults to the chart appVersion.

image.repository is required HERE as well as in the schema, and the second layer
is the point. With the schema layer removed - deleted, or skipped with
helm template --skip-schema-validation, which is a real flag - an empty
repository used to render

    image: ":ci"

which is a well-formed manifest. It passes helm template, and it passes
kubeconform -strict at 5 valid, 0 skipped, so BOTH of this chart's static gates
report green on a reference that cannot resolve. The failure arrives at pod
creation as an invalid-reference error naming neither the chart, the value, nor
the schema, and the operator starts debugging a registry problem they do not
have.

hub.hubId already had this second layer. This one did not, and the difference was
invisible to a test that asserted only that a bad value was rejected - the schema
answered first every time, so the missing layer behind it could not be seen. Both
layers are now asserted separately in the guard table.
*/}}
{{- define "scion-hub.image" -}}
{{- $repository := required "image.repository is required: set it to the hub image built from the root Dockerfile with --target hub-gke. The chart has no default and cannot have one - that image is not published anywhere, and the published artifact named scion-hub is NOT it: it runs as root, which this chart's runAsNonRoot refuses, and it has no embedded web UI." .Values.image.repository }}
{{- if and .Values.image.tag .Values.image.digest }}
{{- fail "image.tag and image.digest are mutually exclusive: set image.digest (preferred) or image.tag, not both." }}
{{- end }}
{{- if .Values.image.digest }}
{{- printf "%s@%s" $repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" $repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
The startup budget, asserted as a DURATION rather than as a threshold count.

periodSeconds x failureThreshold is the time the hub gets to become ready before
the kubelet starts killing it. The schema pins each factor separately and cannot
pin the product, so both of these passed with the schema fully active:

  probes.startup.periodSeconds=1   -> 60 x 1s  = 60s, not 300s
  probes.startup.enabled=false     -> no startupProbe at all

Both rendered clean and passed kubeconform -strict, while the schema's own
description stated the safety property as though it were enforced. A guard whose
stated contract is wider than what it enforces is worse than no guard, because it
stops the reader thinking: an operator lowering periodSeconds for faster
readiness detection reads "at least 60", sees failureThreshold: 60 untouched, and
has cut the first-boot budget by 80% with nothing to tell them.

Why 300 seconds: first boot blocks on an unbounded schema-migration advisory lock
before the listener binds. Killing the pod during that leaves a partially applied
migration, so the retry starts from a different state than the attempt before it
and the failure stops being reproducible.

DISABLING THE STARTUP PROBE IS PERMITTED ONLY WHILE THE LIVENESS PROBE IS OFF,
which is a real distinction and not a loophole. A startup probe's job is to hold
the liveness probe off until the container is up. With liveness disabled - the
default - nothing can kill the pod during the migration; readiness simply stays
false and the pod stays out of the Service, which is correct. With liveness
enabled and no startup probe, the liveness probe begins immediately and kills the
hub mid-migration, which is the exact failure the budget exists to prevent.
*/}}
{{- define "scion-hub.assertStartupBudget" -}}
{{- $startup := .Values.probes.startup }}
{{- $liveness := .Values.probes.liveness }}
{{- if $startup.enabled }}
{{- $budget := mul (int $startup.periodSeconds) (int $startup.failureThreshold) }}
{{- if lt $budget 300 }}
{{- fail (printf "the startup budget is too short: probes.startup.periodSeconds (%d) x probes.startup.failureThreshold (%d) = %ds, and the minimum is 300s. The budget is the PRODUCT, so raising one factor or the other is equally valid - the schema can only bound each separately, which is why this is checked here. First boot blocks on an unbounded schema-migration advisory lock before the listener binds; a pod killed during it leaves a partially applied migration, and the retry starts from a different state than this attempt did." (int $startup.periodSeconds) (int $startup.failureThreshold) (int $budget)) }}
{{- end }}
{{- else if $liveness.enabled }}
{{- fail "probes.startup.enabled is false while probes.liveness.enabled is true. The startup probe is what holds the liveness probe off until the hub is up, so this combination points a killing probe at a hub that is still running its first-boot schema migration. Either leave the startup probe enabled, or disable the liveness probe - with liveness off, no probe can kill the pod and readiness simply stays false until the migration finishes." }}
{{- end }}
{{- end }}

{{/*
Deployment update strategy: Recreate at one replica, RollingUpdate above it.

FOUND BY THE SWEEP FOR PRODUCT INVARIANTS, not by review. It is the second
instance of the startup-budget shape: the schema constrains updateStrategy.type
with an enum and constrains replicaCount with a minimum, and the property is
about the PAIR. Explicit RollingUpdate at replicaCount 1 renders clean, with the
schema fully active and no skip flag, and passes kubeconform - and it is the
exact hazard the default derivation exists to avoid.

The chart's own hardening makes the override worse rather than better. Whenever
the type resolves to RollingUpdate the Deployment renders maxUnavailable: 0,
which is correct above one replica and means the new pod must become Available
before the old one is deleted. At one replica that does not merely permit two
hubs, it GUARANTEES two hubs - for as long as the new one takes to become ready,
which the startup budget above allows to be five minutes - both mounting and
writing the same RWX workspace share. The symptom is corrupted workspace state,
which points at the workspace, the share and the agent long before it points at
a strategy field the operator set once and considered settled.

Explicit Recreate is accepted at any replica count: it costs downtime and risks
nothing. Above one replica RollingUpdate is accepted because concurrent hubs are
already what more than one replica means - the choice was made by replicaCount,
not by this field.
*/}}
{{- define "scion-hub.updateStrategyType" -}}
{{- $explicit := (.Values.updateStrategy | default dict).type | default "" }}
{{- if and (eq $explicit "RollingUpdate") (le (int .Values.replicaCount) 1) }}
{{- fail "updateStrategy.type is RollingUpdate at replicaCount 1. A rolling update renders maxUnavailable: 0, so the replacement hub must become Available before the old one is deleted - at one replica that means two hubs run at once, for up to the whole startup budget, both writing the same workspace share. Leave updateStrategy.type empty (it resolves to Recreate at one replica and RollingUpdate above it) or set Recreate explicitly. If you want a rolling update to avoid downtime, the value to change is replicaCount, and read its comment first." }}
{{- end }}
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

THE SEGMENT SEPARATOR IS THE HYPHEN, AND A CALLER WHOSE NAMES USE ANY OTHER
SEPARATOR MUST TRANSLATE BEFORE CALLING. That is argv semantics and it is
deliberate; it is also a trap, because environment variable names separate with
underscores. Passed SESSION_SECRET unchanged, the pattern above matches nothing:
it needs a hyphen or start-of-string before "secret", and "session_secret"
offers neither. The guard would render, appear in the diff, read as applied, and
catch precisely the value it was added for. A reviewer asking "is this guard
correct" finds a correct guard; the question that finds this is "is it reachable
in the state it guards against".

So the underscore check below is not input validation, it is the reachability
check, and it fails loudly rather than letting the caller proceed with an inert
guard. Translate at the call site - "_" to "-" - and call this unchanged. The
alternative, widening the class to (^|[-_]), was rejected: the hyphen rule is
correct for argv, and the caller with the other convention is the one that
should adapt. Do one or the other, never both.

An underscore in a flag name is also a real error on the argv path: no flag on
`server start` uses one, so pflag would reject it and the hub would crash-loop.
Failing here says so at render time.
*/}}
{{- define "scion-hub.assertNoCredentialName" -}}
{{- $n := lower (toString .name) }}
{{- if contains "_" $n }}
{{- fail (printf "%s %q contains an underscore, and this check separates segments on the hyphen. Translate \"_\" to \"-\" before calling (environment variable names need this; flag names do not, and no flag on `server start` has an underscore). Called with the name as-is, the check matches nothing and silently protects nothing." .source $n) }}
{{- end }}
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

What it catches: credentials in URL userinfo, credentials in a URL query string
under a well-known parameter name, and a handful of well-known credential
prefixes. What it does NOT catch, and cannot: an opaque high-entropy string with
no recognisable shape. There is no reliable way to tell one of those from a
legitimate identifier, and a heuristic that guessed would reject real values with
no override, which for an append-only list is worse than the hole. Do not add an
entropy heuristic here.

THE USERNAME IN THE USERINFO PATTERN IS OPTIONAL, and that is not a typo. An
empty username is the standard form for a Redis URL and is valid for Postgres,
so redis://:hunter2@10.0.0.1:6379 carries a real password. Requiring one
username character missed it - the same class as the DSN case the name axis
cannot see, since the flag holding it can be called anything.

THE PEM ALTERNATIVE IS UNREACHABLE THROUGH hub.args, and live for every other
caller. Every PEM header contains spaces and starts with a dash, so on the argv
path the whitespace guard in scion-hub.hubArgs always rejects it first. Do not
conclude the branch is dead and delete it: it is the branch that catches a
multi-line private key in an environment value, where whitespace is legal, and
its only test coverage chart-wide lives with those callers rather than here. If
you change this alternative, that is the test that will tell you.

The matched value is redacted or truncated in the failure message. A guard whose
error message prints the secret it just caught has moved the secret from argv
into CI logs.
*/}}
{{- define "scion-hub.assertNoCredential" -}}
{{- $s := toString .value }}
{{- $source := .source }}
{{- if regexMatch "://[^/@[:space:]]*:[^/@[:space:]]+@" $s }}
{{- fail (printf "%s %q embeds credentials in a URL (scheme://user:password@host, and the username may be empty as in redis://:password@host). Anything on argv or in a plain environment value is readable by anyone with pod read access; credentials are delivered through a Secret." $source (regexReplaceAll "://[^/@[:space:]]*:[^/@[:space:]]+@" $s "://REDACTED@")) }}
{{- end }}
{{- if regexMatch "(?i)[?&](access_token|refresh_token|id_token|auth_token|api_?key|client_secret|password|passwd|signature)=[^&[:space:]]" $s }}
{{- fail (printf "%s carries a credential in a URL query string (%s=...). A query string is not a hiding place: it reaches argv, process listings, proxy logs and Referer headers alike. Deliver it through a Secret and let the hub read it from the environment." $source (regexFind "(?i)[?&](access_token|refresh_token|id_token|auth_token|api_?key|client_secret|password|passwd|signature)=" $s | trimAll "?&=")) }}
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

PFLAG IS LAST-WINS, which is the premise the whole reserved list rests on.
Appending a flag the chart already rendered does not conflict, error or warn -
it silently replaces the chart's value. So "the chart renders it" is not
protection; the reserved list is the protection.

THE FLAG SET IS cmd/server.go PLUS cmd/root.go. server start inherits rootCmd's
PERSISTENT flags, so the flags it accepts are not all declared in the file that
declares the command. Two rounds of this list were built from cmd/server.go
alone and both were incomplete in the same direction: --global, --project, -g
and --grove are all inherited, all accepted by server start, and all absent from
cmd/server.go. If you extend this list, enumerate both files.

The RESERVED flags are grouped by WHY they are reserved, in five lists below,
and the grouping is load-bearing rather than tidy. Only one group is verifiable
against the rendered arguments. For the other four, finding no match in the
rendered arguments is the expected steady state and NOT evidence that the entry
is stale - which is exactly the reasoning that would delete them. A flat list
under one comment invites the next maintainer to verify each entry against what
the chart renders, conclude that --config was added in error, and remove it;
that removal would look like tidying and would reopen the largest hole in this
guard. The failure messages differ per group so the reason is visible at the
point it fires, to the person arguing with the guard rather than only to the
person reading this file.

SHORTHANDS MUST BE LISTED BY LETTER. The normalisation below reduces -x and --x
to the same token, so a shorthand is only caught if its letter is on the list.
"c" is on it for --config and "g" for --project; those are the two shorthands
that matter today. Any future flag registered with a *VarP form needs its letter
added here.

CASE IS NORMALISED before the lists are consulted. pflag itself is
case-sensitive, so --CONFIG would not reach the hub as --config; it would be an
unknown flag and the hub would crash-loop. Rejecting it here turns that
crash-loop into a render error, which is the same reasoning as the whitespace
guards below.

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
Five lists, not one, because they are reserved for five different reasons and a
flat list loses the reason. See the block comment above for why that matters:
the entries below are NOT all verifiable by checking what the chart renders.
Exactly one list is.
*/}}

{{- /*
1. The chart renders these itself, and pflag is last-wins, so an appended copy
   silently replaces the chart's value rather than conflicting with it.

   THIS IS THE ONLY LIST VERIFIABLE AGAINST THE RENDERED ARGS, and it is now
   verified mechanically rather than by instruction, in BOTH directions: the
   invariant below fails the render if the chart emits a flag this list omits,
   AND if this list names a flag the chart does not emit. So this list is exactly
   the set of flags the chart renders - not by discipline, by construction.

   Nothing that the chart does NOT render belongs here, however true it is that
   the flag should be reserved. Two entries used to sit here that the chart never
   emitted, under a comment telling the maintainer to delete entries that do not
   appear in the rendered args - a deletion instruction pointed at two live
   guards. They are in $aliasOrIgnored now, and the second containment is what
   keeps that from recurring; the previous version of this comment asked the
   maintainer to enforce it by hand, which is enforcement by whoever read the
   comment most recently.

   Cross-references in these comments use the LIST VARIABLE NAME, never the list
   number. Numbers renumber; this comment was written while adding a fifth list.
*/}}
{{- $setByChart := list "foreground" "hosted" "host" "web-port" "enable-hub" "enable-runtime-broker" "enable-web" "auto-provide" "global" }}

{{- /*
2. NOTHING may pass these. Not the operator, and not a future phase of this
   chart either. Every one of them redirects where the hub's configuration comes
   from.

   DO NOT REMOVE THESE BECAUSE THE CHART DOES NOT SET THEM. That is the point of
   them, not evidence that they were added by mistake. There is no legitimate
   reason for this chart to emit any of them, so "nothing in the rendered args
   matches this entry" is the expected steady state forever.

   --config: READ THIS BEFORE YOU CHANGE OR CHECK IT, BECAUSE ITS EFFECT CHANGES
   OVER THE LIFE OF THE CHART AND ANY SINGLE-MECHANISM DESCRIPTION OF IT WILL BE
   FALSE HALF THE TIME. It reaches exactly one place on this command's path:
   config.LoadGlobalConfig(serverConfigPath), cmd/server_foreground.go:827, via
   cmd/server.go:237. LoadGlobalConfig tries loadGlobalConfigFromSettings first,
   and that function reads $HOME/.scion/settings.yaml FIRST and UNCONDITIONALLY
   (pkg/config/hub_config.go:640-660); the path from --config is consulted only
   when the global file is missing or has no top-level "server:" key.

   So: TODAY, on this chart as it stands, --config is fully live - this phase
   mounts nothing at $HOME/.scion, the global lookup finds no settings.yaml, and
   --config selects the file the hub's server configuration is read from. ONCE
   THE CONFIGURATION PHASE MOUNTS A settings.yaml WITH A TOP-LEVEL server: KEY,
   the same flag becomes inert for that config and yields only a deprecation
   warning.

   That reversal is the durable reason to reserve it, and it is stronger than
   either half taken alone: the flag's effect is a property of WHAT THIS CHART
   RENDERS rather than of the binary, so any phase can turn it live or inert
   again without touching this file or this list. A reservation is the only form
   of this knowledge that survives that. Reserving it also costs nothing while it
   is inert, and an inert deprecated path is exactly the kind of thing that is
   deprecated further later.

   Both readings of this flag were asserted confidently and wrongly today - once
   as "redirects the entire configuration load", once as "no-ops with a warning".
   Each was true of a different tree. Check which tree you are in before you
   correct this comment again: the question is whether the chart renders a
   settings.yaml with a server: key, not what the flag is called.

   --project, -g and --grove reach the same place by another door, and this list
   was incomplete without them for two rounds because they are declared in
   cmd/root.go rather than cmd/server.go. They redirect project resolution and
   therefore the config location. --grove binds the SAME VARIABLE as --project
   (cmd/root.go:249-250), so reserving one without the other leaves the alias
   open - the same pattern as hosted/production. --profile is here for the same
   family of reasons: it does not move the file, but it selects which
   configuration applies, and the chart's guarantee is that the settings file it
   renders is the one in force.

   Note that --global is NOT here. The chart renders it, so it is in list 1, and
   list 1 rejects it just as absolutely. It belongs to the same hazard family -
   --global=false is the --config hazard by another route - and it is filed by
   how it is checked rather than by how bad it is.
*/}}
{{- $neverPassed := list "config" "c" "project" "g" "grove" "profile" "p" }}

{{- /*
3. Not the lever they appear to be. Each of these is a flag an operator could
   reasonably reach for, which either aliases something the chart controls or is
   silently ignored in the configuration this chart renders. NOT verifiable
   against the rendered args - the chart emits neither - and that is why they are
   not in $setByChart, whose comment would instruct their deletion.

   Per-entry, because the two are here for different failures:

   --production binds the SAME VARIABLE as --hosted (cmd/server.go:235, a
   deprecated alias). So --production=false disables hosted mode - the first
   hazard this guard was ever written for - while the operator believes they
   passed a no-op about a deprecated spelling.

   --port is the hub API port for standalone mode and is IGNORED whenever
   --enable-web is set (cmd/server.go:241), which this chart always sets. An
   operator moving the port with it changes nothing at all: the listener stays on
   --web-port, the probes still pass, and the flag they set has no effect they can
   observe. A silent no-op that looks like a change is worth a render error.
*/}}
{{- $aliasOrIgnored := list "production" "port" }}

{{- /*
4. The chart already delivers these settings through a channel other than argv,
   so passing them here creates a second source for one value. Not verifiable
   against the rendered arguments, by construction - the rendered argument list
   is where these must NOT appear. Check them against the other channel.

   NAME THE CHANNEL WHEN YOU ADD AN ENTRY, because it is not the same channel
   for every entry and the precedence differs. admin-emails, db, storage-bucket
   and storage-dir are delivered through the settings file. base-url is
   delivered as the SCION_SERVER_BASE_URL environment variable.

   Precedence for base-url, read from the hub rather than assumed, because "two
   sources" only matters if one of them silently loses:

     cmd/server_foreground.go:2102 (initWebServer, the OAuth redirect base)
       --base-url, else SCION_SERVER_BASE_URL, else http://localhost:<web-port>
     cmd/server_foreground.go:1310 (resolveHubEndpoint, the URL agents dial)
       settings file server.hub.public_url, else --base-url, else
       SCION_SERVER_BASE_URL, else project settings, else localhost

   Two consequences, both silent. ARGV BEATS THE ENVIRONMENT AT BOTH SITES: a
   future phase that emits --base-url shadows the environment variable, with no
   error and, unless --debug is on, no log line either. And the two sites do not
   agree with each other - the settings file outranks argv when resolving the
   agent-facing endpoint but is not consulted at all for the OAuth redirect - so
   argv plus a settings file that sets public_url yields two different base URLs
   in one process, each correct by its own rule.

   That is why this entry is reserved rather than merely discouraged. A later
   phase may still choose argv as the delivery channel for base-url, and this
   list is where it says so: move the entry, do not add a second emitter beside
   the existing one. For --config that choice would be self-defeating, which is
   why it is in list 2 instead.
*/}}
{{- $ownedByConfig := list "admin-emails" "base-url" "db" "storage-bucket" "storage-dir" }}

{{- /*
5. These weaken authentication or place credentials where they can be read.
*/}}
{{- $unsafeToPass := list "session-secret" "dev-auth" "enable-test-login" "web-assets-dir" }}

{{- /*
THE INVARIANT THAT MAKES $setByChart SELF-CHECKING, IN BOTH DIRECTIONS.

$setByChart and the flags rendered above must be the SAME SET. Two containments,
four lines, and they catch opposite mistakes:

  A. rendered is a subset of $setByChart - catches a flag the chart passes that
     nobody reserved. The list was incomplete twice this way: six flags the chart
     itself renders (foreground, enable-hub, enable-runtime-broker, enable-web,
     auto-provide, global) were missing, and because pflag is last-wins an
     operator could append --enable-runtime-broker=false and get a hub that stays
     Ready, keeps its RBAC, and can never launch an agent.

  B. $setByChart is a subset of rendered - catches a member the chart claims to
     set and does not. Two entries sat here that the chart never emitted, under
     the comment above telling the maintainer to delete entries not present in
     the rendered args: a deletion instruction pointed at two live guards, one of
     them --production, whose removal reopens disable-hosted-mode because it
     binds the same variable as --hosted. They are in $aliasOrIgnored now, and
     direction B is what keeps them out rather than the comment.

A alone was implemented first and would not have caught B's case at all. Both
mistakes are the same mistake - the list and the render drifting - and one
containment only ever sees one direction of drift.

DIRECTION B IS MEANINGFUL FOR $setByChart AND FOR NO OTHER LIST. The other three
are reserved precisely BECAUSE the chart does not render them, so for them the
empty intersection is the expected steady state forever. That is what the split
into reasons bought beyond documentation: it isolated the one group whose
membership is a checkable claim about this file, and made it checkable.

Both run against the chart's own arguments only, before hub.args is appended, so
they assert a property of THIS FILE rather than of operator input.

IF A LATER PHASE RENDERS A FLAG CONDITIONALLY, BUILD $setByChart BESIDE THE
RENDER - append to both inside the same if - rather than weakening either
containment. I had argued for keeping A one-directional on the grounds that B
would fire on a legitimate conditional flag; that was the wrong trade. It buys a
future convenience with a present hole, and the convenience is available anyway
by keeping the list and the command in step, which is the thing being asserted.

CONSIDERED AND REJECTED: deriving $setByChart from $args. It would make both
containments true by construction and delete this block, and that is precisely
the objection - DERIVING ONE SIDE OF A COMPARISON FROM THE OTHER PRODUCES A CHECK
THAT CANNOT FAIL. Both directions become tautologies over a set defined as the
thing they are compared against, and the render stays green forever whatever the
command does.

That is a different move from removing a coupling, though the two look identical
in a diff. service.port and hub.webPort need no assertion because targetPort: http
means there is no longer anything to violate - the INVARIANT is gone. Deriving
this list would leave the invariant exactly as breachable as it is now and delete
only the CHECK. Prefer the first; refuse the second.

A second and lesser objection, kept because it is independently true: the
derivation silently expands the operator-facing reserved set every time a
maintainer adds a flag to the command, which is a contract change with nothing
announcing it.

The explicit list is the statement; these two checks are what keep the statement
true.
*/}}
{{- $renderedFlags := list }}
{{- range $chartArg := $args }}
{{- if hasPrefix "-" $chartArg }}
{{- $chartFlag := lower (trimPrefix "-" (trimPrefix "--" (first (splitList "=" $chartArg)))) }}
{{- $renderedFlags = append $renderedFlags $chartFlag }}
{{- if not (has $chartFlag $setByChart) }}
{{- fail (printf "chart defect, not a values error: scion-hub.hubArgs renders -%s but $setByChart does not list it, so hub.args could append a second copy and pflag - which is last-wins - would silently take the operator's value over the chart's. Add %q to $setByChart in _helpers.tpl." $chartFlag $chartFlag) }}
{{- end }}
{{- end }}
{{- end }}
{{- range $listed := $setByChart }}
{{- if not (has $listed $renderedFlags) }}
{{- fail (printf "chart defect, not a values error: $setByChart lists %q but scion-hub.hubArgs does not render it, and that list's stated reason for reserving a flag is that the chart sets it. Do NOT fix this by deleting the entry - a reserved flag the chart does not render may still be dangerous to accept, and deleting it would silently reopen whatever it was guarding. Move it to the list whose reason actually applies ($neverPassed, $aliasOrIgnored, $ownedByConfig or $unsafeToPass), or render it. If a later phase renders it conditionally, append to $setByChart inside the same conditional." $listed) }}
{{- end }}
{{- end }}
{{- range $raw := .Values.hub.args }}
{{- $arg := toString $raw }}
{{- if ne $arg (trim $arg) }}
{{- fail (printf "hub.args entry %q has leading or trailing whitespace. pflag would read it as a positional argument rather than a flag, and the hub would crash-loop instead of failing here." $arg) }}
{{- end }}
{{- if and (hasPrefix "-" $arg) (regexMatch "[[:space:]]" $arg) }}
{{- fail (printf "hub.args entry %q contains whitespace. Pass a flag and its value as two separate array elements. If the VALUE itself contains whitespace - a PEM block, a multi-line banner - splitting will not help: it does not belong on argv at all, where it is readable by anyone with pod read access, and a later phase delivers values like that through a Secret or an environment value instead." $arg) }}
{{- end }}
{{- /*
Lowercased before the lists are consulted: pflag is case-sensitive, so --CONFIG
would crash-loop as an unknown flag rather than redirect the config load. This
turns that crash-loop into a render error. The name axis lowercases too, and did
before this did, which is how the inconsistency was found.
*/}}
{{- $flag := lower (trimPrefix "-" (trimPrefix "--" (first (splitList "=" $arg)))) }}
{{- if has $flag $setByChart }}
{{- fail (printf "hub.args may not contain -%s: the chart renders it, and pflag is last-wins, so this would silently replace the chart's value rather than conflict with it - disabling hosted mode, unbinding the listener, taking the daemon fork so PID 1 exits, leaving /readyz unregistered, or leaving the runtime broker off in a pod that still reports Ready and can never launch an agent." $flag) }}
{{- end }}
{{- if has $flag $neverPassed }}
{{- fail (printf "hub.args may not contain -%s: it selects where the hub's configuration is read from. Whether it redirects the load outright or silently no-ops depends on what this chart renders at the time - it has already been both - and in either case the chart can no longer guarantee that the configuration in force is the configuration it rendered, while every rendered value keeps reporting the operator's intent." $flag) }}
{{- end }}
{{- if has $flag $aliasOrIgnored }}
{{- fail (printf "hub.args may not contain -%s: it is not the lever it looks like. -production is a deprecated alias bound to the same variable as -hosted, so passing it can disable hosted mode; -port is ignored whenever -enable-web is set, which this chart always sets, so passing it changes nothing observable. The chart renders neither, which is why this is a separate reservation and not a stale entry." $flag) }}
{{- end }}
{{- if has $flag $ownedByConfig }}
{{- fail (printf "hub.args may not contain -%s: the chart already delivers this setting through another channel - the settings file, or for base-url the SCION_SERVER_BASE_URL environment variable - and argv silently wins over both, so this creates a second source for one value with nothing reporting the disagreement." $flag) }}
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
