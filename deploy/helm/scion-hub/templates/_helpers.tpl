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
Name for the cluster-scoped RBAC pair. Rewritten 2026-08-17 by gd-p3-dev; the
previous version of this comment is quoted below because it is evidence.

WHY THE NAMESPACE IS IN THE NAME. scion-hub.fullname is a function of the
release name only, so two installs of the same release name in different
namespaces - a per-team or per-environment layout, which is normal - would
collide on one cluster-scoped object. Under helm install that is an ownership
error and survivable. Under helm template piped to kubectl apply, or a GitOps
pipeline, there is no ownership check: by the Kubernetes API's documented
behaviour an apply of a ClusterRoleBinding that already exists updates it in
place, so the second release's subject replaces the first's and cluster-wide
pods/exec and secrets authority points at another namespace's ServiceAccount.
NOBODY ON THIS PROJECT HAS RUN THAT SECOND APPLY - it is documented API
behaviour, not our measurement. The COLLISION below is measured.

THE COMMENT THAT USED TO BE HERE ENDED: "Including the namespace makes that
unrepresentable." The line under it was:

    printf "%s-%s-agents" (include "scion-hub.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-"

TRUNCATION DISCARDS THE NAMESPACE, WHICH IS THE ONLY PART MAKING THE NAME
UNIQUE. The guarantee was never enforced - not prose that fell behind the code,
unenforced from the first commit. Filed as C1 by gd-regmis-rev, graded Critical
by gke-deploy-lead, reproduced at merged main by gd-consumer.

  ns=team-alpha-production   CR and CRB  ...-scion-hub-team-alpha (62)  subject ns=team-alpha-production
  ns=team-alpha-staging      CR and CRB  ...-scion-hub-team-alpha (62)  subject ns=team-alpha-staging

trimSuffix PARTICIPATED IN THE COLLISION rather than breaking it: the cut lands
on a hyphen, the hyphen is removed, and the two names are still identical at 62
characters. A test asserting a 63-character name misses exactly these cases.

WHEN IT COLLIDES, AS A RULE AND NOT AS A THRESHOLD:

    collide  <=>  len(fullname) + 1 + sharedPrefix(ns1+S, ns2+S)  >=  63

        where S is the literal suffix the object's name ends in.

S IS A PARAMETER AND NOT A CONSTANT, even though it has exactly one value here.
S = "-agents" for both cluster-scoped objects in this chart, which is the whole
population: of the seven kinds this chart emits, only ClusterRole and
ClusterRoleBinding are cluster-scoped, and both take their name - and the
binding its roleRef - from this one helper. Namespaced objects cannot collide
ACROSS namespaces, because the namespace scopes them - but read the next
paragraph before taking any comfort from that. A NEW
CLUSTER-SCOPED OBJECT WITH A DIFFERENT SUFFIX NEEDS ITS OWN S AND IS NOT
COVERED BY THE NUMBERS BELOW. The parameter is here because every grid run
today held the suffix constant, so no measurement anyone has taken could have
told us whether it mattered (gd-em, 14:29).

🛑 "NAMESPACED OBJECTS CANNOT COLLIDE" IS TRUE ONLY OF THE ACROSS-NAMESPACE
CASE, AND THE SENTENCE ABOVE USED TO STOP THERE. Qualified 2026-08-17. Two
installs IN ONE NAMESPACE collide on every namespaced object the chart emits
whenever their fullnames agree - which C4 makes reachable with two ordinary
release names, "prod" and "prod-scion-hub". That is not hypothetical here:
Phase 3's session Secret is a namespaced object, both installs render it under
the same name, and the Deployment consumes it by envFrom secretRef, so the
second install's session key silently replaces the first's for BOTH. Upstream's
pkg/hub/signing_key_shared_test.go is explicit that a shared session secret
makes two processes accept each other's tokens. THE SCOPING PROPERTY THAT MAKES
NAMESPACED OBJECTS SAFE IS THE NAMESPACE, AND IT DOES NOTHING WHEN THE NAMESPACE
IS THE SAME.

  AND THERE IS NO BENIGN BRANCH (gd-em, 15:32). The session secret is always
  operator-supplied - the chart fails the render rather than generating one - so
  two colliding installs fall into exactly two cases and both are Critical:

      SAME value      -> ONE SHARED JWT SIGNING KEY -> cross-install token
                         forgery. Silent. No error, no event, no log line.
      DIFFERENT value -> two secrets, one object name -> MUTUAL CLOBBER. Each
                         apply overwrites the other; sessions invalidate on a
                         schedule nobody controls, and the settings Secret and
                         the OAuth client secret go with it.

  ANYONE ARGUING THIS COLLISION IS RARE ENOUGH TO IGNORE MUST ARGUE BOTH
  BRANCHES AWAY, NOT ONE.

THE "-agents" TERM IS LOAD-BEARING AND WAS MISSING FOR MOST OF A DAY. The rule
was first published without it, taking sharedPrefix over the bare namespaces. In
that form it agreed with gd-consumer over 40 rendered cells and with gd-p3-dev
over 360, and it is WRONG: when one namespace is a proper prefix of the other,
the hyphen beginning "-agents" continues the match past the end of the shorter
namespace. gd-regmis-rev found it over 16,994,448 exhaustive cases (2,214
under-predictions, 0 over-predictions) and named one decisive cell; gd-p3-dev
and gd-p3-rev independently rendered it on real helm:

    release 41 x "r",  ns team-alpha  vs  team-alpha-staging
      bare-namespace rule:  51 + 1 + 10 = 62  ->  predicts DISTINCT
      helm:                 ...-scion-hub-team-alpha (62) BOTH  ->  COLLIDES

    the same pair at release length 40 renders DISTINCT, and the two names
    differ in the LAST of their 63 characters: "a" from "-agents" against the
    "s" of "staging". The suffix does the matching; that is the mechanism, in
    rendered bytes rather than in argument.

BECAUSE THE ERROR WAS ALL IN ONE DIRECTION - 0 over-predictions - the corrected
rule only ever GROWS the collision set. Nothing established under the old form
was withdrawn, and neither the severity nor this fix moved when it was
corrected. Recorded because the two grids that missed it were not careless: 400
renders across two rigs, and NOT ONE PAIR IN EITHER had one namespace as a
proper prefix of the other, so no input existed that could make the model say
the other thing. 400 agreements measured the pair set, not the rule.

IT IS A BUCKET, NOT A PAIR (gd-p3-rev). Both statements of the rule take two
namespaces and ask whether those two collide, which is correct arithmetic and
under-describes the object. Every namespace truncating to the same bytes lands
on ONE ClusterRole and ONE ClusterRoleBinding - rendered, at release length 41,
team-alpha and team-alpha-staging and team-alpha-production all produce
...-scion-hub-team-alpha. A reader told "two namespaces can collide" will check
their two namespaces; the question is how many namespaces are in the bucket.

THE FIRST COLLIDING RELEASE-NAME LENGTH IS NOT A PROPERTY OF THIS CHART. It
moves with the namespaces:

    shared prefix 31   platform-team-alpha-production-{east,west}   release length 21
    shared prefix 11   team-alpha-production / team-alpha-staging   release length 41
    shared prefix  5   prod-a / prod-b                              release length 47
    shared prefix  0   a / b                                        release length 52

AND len(fullname) IS NOT len(release): the helm idiom skips appending the chart
name when the release already contains it, so two release names of the same
length can differ by ten characters of fullname and only one of them collides.
scion-hub-prod-eu-01 in platform-team-alpha-production-{east,west} is an
ordinary thing for an operator to type, and it collides.

THERE IS NO RELEASE-NAME-LENGTH FLOOR AT ALL (gd-p3-rev, rendered).
scion-hub.fullname returns fullnameOverride DIRECTLY, so an operator setting it
severs the release-to-fullname relationship: a 31-character fullnameOverride
collides at a ONE-CHARACTER release name across the 31-prefix namespace pair.
Any statement of this defect in terms of release length is therefore a
description of the default naming path only. THIS IS ALSO THE TRAP FOR THE FIX:
gating "does this need a digest" on release-name length would let
fullnameOverride walk straight past the gate. The gate below is
len($readable) > 63 - the truncation actually happening, which is the condition
the defect is made of, and the only one that sees every path into it.

THE FIX, AND WHY THIS ONE. Three options were on the table: (A) raise the 63 cap
to 253, (B) suffix a digest, (C) refuse to render and make the operator shorten
the name. A IS RULED OUT because the 253 is an inference from the word "most" in
the Kubernetes documentation, which does not name ClusterRole, and nobody here
has a cluster to confirm it against. A security fix does not rest on the word
"most". C IS NOT TAKEN because the chart cannot enumerate other releases, so it
can only refuse on TRUNCATION, not on COLLISION - it would refuse installs that
are unique and safe, and above a 46-character namespace it would refuse every
release name there is, leaving those operators no name they could choose.

So B, CONDITIONALLY. When the readable name fits it is emitted BYTE-IDENTICAL to
what this chart has always produced - no existing install is renamed, which is
half of the regression fixture and not a courtesy. Only when it would truncate
is the readable part cut back and a digest appended.

THE DIGEST IS TAKEN OVER A SEPARATOR THAT CANNOT OCCUR IN EITHER INPUT, and that
is deliberate. Hashing the assembled name would inherit its ambiguity: fullname
"a-b" in namespace "c" and fullname "a" in namespace "b-c" both assemble to
"a-b-c-agents". A slash is legal in neither a fullname nor a namespace, so
"fullname/namespace" is injective ON THE PAIR IT IS GIVEN where the assembled
string is not.

  🛑 AND THE PAIR IT IS GIVEN IS NOT THE IDENTITY THAT MATTERS. CORRECTED
  2026-08-17 - the sentence above used to end "injective where the assembled
  string is not", full stop, and that full stop was doing work it had not
  earned.

  The security-relevant identity is (Release.Name, Namespace): two installs are
  two installs because they were installed under different release names or into
  different namespaces. What this digest actually consumes is (fullname,
  Namespace). Those are not the same tuple, and the map between them is not
  injective:

      scion-hub.fullname is MANY-TO-ONE IN THE RELEASE NAME. The helm idiom
      skips appending the chart name when the release already contains it, so
      release "prod" and release "prod-scion-hub" BOTH yield fullname
      "prod-scion-hub". Same fullname, same namespace, same digest input, same
      digest. Filed as C4.

  So the join is injective, the COMPOSITION is not, and injectivity of the last
  step in a pipeline says nothing about the pipeline. A separator argument can
  only ever be as strong as the operands it separates.

  AND THERE ARE THREE STAGES, NOT TWO. I named the derivation and the join and
  stopped one short; gd-consumer measured the third and it is the one this
  helper actually applies:

      1. DERIVATION of each component   <- fullname loses the release name (C4)
      2. THE JOIN                       <- "-" is legal in both operands (C5)
      3. THE LENGTH CAP AFTER THE JOIN  <- trunc 63 discards what survived 1-2

  AN INJECTIVE JOIN DOES NOT MAKE AN INJECTIVE NAME. EVERY STAGE THE IDENTITY
  PASSES THROUGH MUST PRESERVE THE DISTINCTION, AND A FIX AIMED AT ONE STAGE
  TESTS GREEN WHILE ANOTHER STAGE STILL DISCARDS IT.

  Measured, C1's pair with the release length varied, under a corrected
  colon-on-release join: distinct at 40, 45 and 50, and COLLIDING AGAIN AT 53 -
  which is helm's own maximum release name length - because trunc 63 then cuts
  below the point where the two differ. FIXING THE JOIN ONLY MOVES THE LENGTH AT
  WHICH C1 COMES BACK. That is why the digest below is applied at the cap rather
  than the join being merely repaired: stage 3 is where this helper does its
  damage, and a prettier separator alone would leave it there.

  🛑 A NOTE ON THE 63 ITSELF, AND IT IS THE STRONGEST ARGUMENT AGAINST THE FIX
  BELOW, RECORDED HERE RATHER THAN OMITTED. The 63 is a DNS-1123 LABEL bound
  applied to a name that is not a DNS label: ValidateRBACName goes to
  IsValidPathSegmentName, which is why "system:masters" is legal, and gd-p3-rev
  measured 70 characters accepted under apimachinery v0.29.0.

  Put gd-p3-rev's result beside gd-consumer's and they compose into something
  neither states alone. TRUNCATION IS C1'S NECESSARY CONDITION - untruncated,
  the namespace is already in the name and the bucket does not collide at all.
  So:

      A CLASS WHOSE NECESSARY CONDITION IS AN AVOIDABLE IMPLEMENTATION CHOICE
      IS CLOSED BY REMOVING THE CHOICE, NOT BY DISAMBIGUATING ITS MEMBERS.

  Which decomposes the whole problem differently from how this file has been
  treating it, and the two halves have different warrants:

      C1        RAISE THE CAP. Nothing else is needed.
      C4 and C5 a join taking .Release.Name - fullname has already collapsed
                the difference, so joining fullname to anything cannot help.

  AND THE CAP CHANGE IS CONFINED, MEASURED RATHER THAN ASSUMED (gd-consumer).
  Three use sites: rbac-clusterrole.yaml:20 metadata.name,
  rbac-clusterrolebinding.yaml:20 metadata.name and :26 roleRef.name. THE NAME
  IS NEVER A LABEL VALUE - a label value is hard-capped at 63 and would have
  killed the idea outright. Positive control on the search: scion-hub.fullname
  matches 6 files, so the grep had aperture and three sites is a real scope
  rather than a broken query.

  NOT DONE IN THIS COMMIT, AND THE REASON IS NOT THAT IT IS WRONG. The true
  ceiling is unconfirmed - 253 is read off a boundary control flipping at
  253/254, NOT measured against apimachinery - and a security fix does not rest
  on a number nobody has checked, which is the same objection that ruled out
  option A above and it has not weakened. Raising the cap also renames nothing,
  so it does not carry the migration risk the digest does; if the ceiling is
  ever measured, THIS IS THE BETTER FIX FOR C1 AND THIS COMMENT SHOULD BE READ
  AS SAYING SO. Filed, not fixed, and not fixed for a reason that is itself a
  measurement someone can go and take.

  A SECOND, WEAKER LEG OF THE OLD SENTENCE ALSO FAILS AND IS WORTH NAMING
  BECAUSE IT IS THE MORE TEMPTING KIND OF WRONG. It said "both being DNS names".
  The namespace is one. The first operand is a FULLNAME, and via
  fullnameOverride it is an arbitrary operator-supplied string that this chart
  never validates - the API server would reject a slash in it, but the digest is
  computed at RENDER time, before anything validates anything. The premise was
  asserted about the inputs and only ever held of one of them.

WHAT THIS DOES NOT PROMISE, because the comment above this one made an absolute
it could not keep. Distinct identities now produce distinct names UNLESS they
collide in the first 10 hex characters of SHA-256, which is a 40-bit space: not
impossible, and not something this chart can rule out by construction, because
any fixed-length name over unbounded input has collisions by pigeonhole. The
case that would defeat this fix is two identities sharing a truncated readable
prefix; tests/rbac-collision.sh renders exactly that case and shows the digests
diverging.

🛑 WHAT INCLUDING THE NAMESPACE ACTUALLY BUYS. REWRITTEN 2026-08-17. The
sentence here used to read "What is now unrepresentable is a SILENT collision
from truncation alone, which is what C1 was." That is narrower than it sounds
and it was still the wrong shape of claim, so it is replaced by the true one:

    PUTTING THE NAMESPACE IN THE NAME REDUCES THE COLLISION SURFACE. IT DOES
    NOT ELIMINATE IT. THE (fullname, namespace) PAIR IS NOT A SECURITY
    BOUNDARY AND MUST NOT BE RELIED ON AS ONE.

Two collision classes are DECLARED RESIDUALS of this change - known, measured,
graded Critical by gke-deploy-lead, and open after this commit lands:

  C4  fullname is many-to-one in the release name, above. Release "prod" beside
      release "prod-scion-hub" in ONE namespace share every generated name.
      Measured 20 colliding release pairs over len(R) 1..43.
  C5  the "-" this helper joins with is LEGAL INSIDE BOTH OPERANDS, so the
      readable branch is an ambiguous concatenation: fullname "a-b" in namespace
      "c" and fullname "a" in namespace "b-c" both render "a-b-c-agents".
      Measured 3875 colliding pairs.

🔴 AND HERE IS THE PART A READER OF THIS FIX WILL GET WRONG IF NOBODY WRITES IT
DOWN: NEITHER RESIDUAL TRUNCATES, AND THIS FIX IS GATED ON TRUNCATION. The gate
is len($readable) > 63. C4 and C5 render at ordinary lengths - C5's example is
33 bytes against a bound of 63 - so the digest branch never executes on their
inputs and the identical names are emitted unchanged. THE GATE IS KEYED ON THE
ONE PROPERTY BOTH REMAINING CRITICALS LACK. Do not read a green
tests/rbac-collision.sh as "collisions are handled"; several of its assertions
PIN THESE RESIDUALS AS PRESENT and go red when they are fixed.

What this commit does close is the class needing a LONG name: a silent collision
arising FROM TRUNCATION, which is what C1 was. That is one class of three.

THE PRIOR VERSION OF THE PARAGRAPH BELOW ALSO UNDERSTATED ITS OWN SUBJECT and is
corrected rather than deleted, because the understatement is the instructive
part. It read: "fullnameOverride can reach the 'a-b'/'c' versus 'a'/'b-c'
ambiguity in the readable branch [...] It needs fullnameOverride set to a
colliding shape." IT NEEDS NOTHING OF THE KIND. Fullname "a-b" is what release
"a-b" produces by default. The precondition named was a sufficient one presented
as a necessary one, which turned a defect reachable by typing an ordinary
hyphenated release name into one requiring a deliberate override - and a reader
triaging their own install against that sentence would have cleared themselves.
A PRECONDITION THAT IS ONLY SUFFICIENT, WRITTEN AS THOUGH IT WERE NECESSARY, IS
AN UNDER-REPORT OF SEVERITY DRESSED AS A DETAIL.

THE DIGEST IS PART OF THE INTERFACE, NOT AN IMPLEMENTATION DETAIL. Pinned here
and in tests/rbac-collision.sh:

    algorithm  sha256sum, first 10 hex characters
    input      printf "%s/%s" <fullname> <.Release.Namespace>, UNTRUNCATED
    layout     printf "%s-%s" (<readable> | trunc 52 | trimSuffix "-") <digest>

CHANGING EITHER THE INPUT OR THE ALGORITHM RENAMES EVERY AFFECTED INSTALL AND
ORPHANS A SECOND CLUSTER-SCOPED OBJECT - and silently, because the operator read
the NOTES disclosure once and will not expect it twice. The fixture asserts one
specific rendered name, so such a change is a red test rather than a second
incident. If you are here because that test is red: you have not broken a test,
you have proposed a migration.

    release platform-hub-production-release in namespace team-alpha-production
      -> platform-hub-production-release-scion-hub-team-alpha-aa14af2e08

The input is UNTRUNCATED on purpose: it is the full identity, so two installs
whose readable parts truncate to the same 52 bytes still differ in the digest.
That is what makes the fix work at all, and it is why the digest cannot be taken
over the already-truncated string.
*/}}
{{- define "scion-hub.clusterRoleName" -}}
{{- $full := include "scion-hub.fullname" . -}}
{{- $readable := printf "%s-%s-agents" $full .Release.Namespace -}}
{{- if le (len $readable) 63 -}}
{{- $readable -}}
{{- else -}}
{{- $digest := sha256sum (printf "%s/%s" $full .Release.Namespace) | trunc 10 -}}
{{- printf "%s-%s" ($readable | trunc 52 | trimSuffix "-") $digest -}}
{{- end -}}
{{- end }}

{{/*
THE PRE-FIX NAME. This is the C1 defect, preserved deliberately and called by
exactly one thing: the orphan disclosure in NOTES.txt. DO NOT USE IT TO NAME AN
OBJECT.

It exists because the fix does not remediate anyone who already has the defect -
it only stops new ones. An install that was colliding gets a NEW cluster-scoped
name on upgrade, and under the template-piped-to-apply path there is nothing to
delete the object under the OLD name, which keeps its cluster-wide secrets and
pods/exec grant pointed at whichever ServiceAccount applied last. To tell the
operator which object to look at, NOTES has to be able to NAME it, and the name
is only computable with the algorithm that produced it. Replacing the helper in
place would leave NOTES able to say no more than "a previous object may remain",
which is information rather than an instruction (gd-consumer, 14:22).

WHEN THIS CAN BE DELETED: when no supported upgrade path starts from a chart
version at or before 0.1.0. Delete the NOTES block with it, not before.
*/}}
{{- define "scion-hub.clusterRoleNameLegacy" -}}
{{- printf "%s-%s-agents" (include "scion-hub.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
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
{{- $id := required "hub.hubId is required: set it to an explicit, stable hub ID. The chart never generates one - without an explicit value the hub derives its ID from its hostname, which is random per pod." .Values.hub.hubId | toString }}
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
it. With a loose security context that image runs as root, and once the Filestore
phase mounts a shared workspace it writes root-owned files into a share that
agents running as uid 1000 cannot write - a failure that surfaces days later and
looks like a storage problem. The REFUSAL is present-tense even though that
particular consequence is not: runAsNonRoot is rendered at this head, so a root
image fails admission today, share or no share, and says why.

runAsUser and runAsGroup stay configurable because they must be able to match the
uid and gid of the workspace share a later phase mounts. Zero is rejected here as
well as in the schema, so relaxing the schema alone cannot reopen the hole. THE
TWO ZEROES ARE REFUSED FOR DIFFERENT REASONS AND ONLY ONE OF THEM BITES TODAY;
the fail messages below say which is which rather than sharing one justification.
*/}}
{{- define "scion-hub.nonRootSecurityContext" -}}
{{- $uid := int .Values.hub.securityContext.runAsUser }}
{{- $gid := int .Values.hub.securityContext.runAsGroup }}
{{- if eq $uid 0 }}
{{- fail "hub.securityContext.runAsUser may not be 0: the hub always runs with runAsNonRoot, so uid 0 would fail pod admission rather than grant root." }}
{{- end }}
{{- if eq $gid 0 }}
{{- fail "hub.securityContext.runAsGroup may not be 0: that makes the hub's primary group root. This chart mounts no shared volume yet, so the concrete harm - files written to the workspace share landing as group 0 and unwritable by agents running as gid 1000 - arrives with the Filestore phase. It is refused now rather than then because THE VALUE OUTLIVES THE PHASE: a 0 set here today stays in the operator's values file through the upgrade that mounts the share, and by then it surfaces as unwritable agent workspaces days later instead of as this message." }}
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
{{- $repository := required "image.repository is required: set it to a hub image built from the root Dockerfile with --target hub-gke. Note that the hub-gke stage is added by the image-build change that accompanies this chart and is NOT in the root Dockerfile yet, so that build fails today with an unknown-target error. The chart has no default and cannot have one - that image is not published anywhere, and the published artifact named scion-hub is NOT it: it runs as root (image-build/hub/Dockerfile:24), which this chart's runAsNonRoot refuses, and it is built with -tags no_embed_web (image-build/scion-base/Dockerfile:55), so --enable-web has nothing to serve." .Values.image.repository }}
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

WHY 300 SECONDS. THE HARM HAS TWO HALVES AND ONLY ONE OF THEM IS PRESENT-TENSE ON
THIS CHART. Each is written in its own tense, because an earlier version of this
paragraph gave the Cloud SQL half as the live justification and the round-4
axis-(d) sweep caught it: the number was being defended by a mechanism this chart
never reaches.

  PRESENT TENSE, ON THIS CHART, TODAY - THE ORDERED SEQUENCE. First boot runs a
  sequence of schema and data migration steps before the listener binds, and a
  kill lands BETWEEN two of them. "Partially applied" is not an inference from the
  word migration: CompositeStore.Migrate (pkg/store/entadapter/composite.go:179-227)
  is a null-scope_id backfill, a dedup, entc.AutoMigrate, an allowlist-to-invited
  data migration, a verification-status backfill and a seed, with in-source
  comments at :180-184 and :202-205 stating that the order matters and why. The
  retry then starts from a different state than the attempt before it, and the
  failure stops being reproducible. THIS HALF RUNS ON SQLITE: migrateStore
  (cmd/server_foreground.go:1168-1170) returns s.Migrate(ctx) directly for every
  driver that is not postgres, and this chart's driver IS sqlite - it renders no
  --db, so the default at pkg/config/hub_config.go:540 stands.

  THE CLOUD SQL PHASE, NOT THIS ONE - THE BLOCKING LOCK. pg_advisory_lock is not
  taken here and cannot be. The postgres branch of migrateStore is its only
  caller, pkg/provision/provision.go's Locker is documented as "on SQLite it's a
  no-op (single-writer serializes already)", and composite.go:221-223 says the
  same thing from the other side. When the Cloud SQL values land the lock becomes
  real, the wait becomes unbounded in the way a lock is unbounded, and 300s stops
  being a margin and starts being a bound. RE-DERIVE IT THERE.

  This is the same treatment updateStrategyType gets below, for the same reason: a
  harm that arrives with a later phase is written in that phase's tense, or it is
  read as a present-tense claim and audited as false.

SO WHAT IS THE 300 ITSELF? A MARGIN, NOT A MEASUREMENT, and it says so rather than
being left to look like one. Nothing in this tree derives 300s from a timed SQLite
migration, and on an empty SQLite database the sequence above is fast. The number
is sized for the case the chart is being built toward - a first boot against a
cold Cloud SQL instance, behind that lock - and it costs the SQLite case nothing,
because a healthy hub passes the startup probe on the first or second check and
never approaches the budget. What the guard buys today is that nobody shrinks the
budget quietly while the phases that will need it are still being added.

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
{{- fail (printf "the startup budget is too short: probes.startup.periodSeconds (%d) x probes.startup.failureThreshold (%d) = %ds, and the minimum is 300s. The budget is the PRODUCT, so raising one factor or the other is equally valid - the schema can only bound each separately, which is why this is checked here. First boot runs an ordered sequence of schema and data migration steps before the listener binds (CompositeStore.Migrate), so a pod killed part-way through leaves a partially applied migration and the retry starts from a different state than this attempt did. The 300s itself is a margin rather than a measurement: it is sized for a first boot against Cloud SQL behind the schema-migration advisory lock, which a later phase adds. On this chart's SQLite default a healthy hub is ready long before the budget matters, so shortening it buys nothing and removes the margin." (int $startup.periodSeconds) (int $startup.failureThreshold) (int $budget)) }}
{{- end }}
{{- else if $liveness.enabled }}
{{- fail "probes.startup.enabled is false while probes.liveness.enabled is true. The startup probe is what holds the liveness probe off until the hub is up, so this combination points a killing probe at a hub that is still running its first-boot schema migration. Either leave the startup probe enabled, or disable the liveness probe - with liveness off, no probe can kill the pod and readiness simply stays false until the migration finishes." }}
{{- end }}
{{- end }}

{{/*
Deployment update strategy: Recreate at one replica, RollingUpdate above it.

A DEFAULT MAY ENCODE A PREFERENCE; A REFUSAL MUST ENCODE A HARM. This helper is
the record of that distinction, because it briefly got it wrong.

The default is a preference and owes nobody a justification: at one replica
Recreate is the simpler upgrade, and it is what VALIDATION.md scenario 20 is
written against (a short outage, with PTY sessions, port tunnels and in-memory
presence lost). An operator who would rather keep those across an upgrade sets
RollingUpdate and accepts a window with two pods. That is an ordinary Kubernetes
trade and the chart takes no position on it.

There WAS a fail here refusing explicit RollingUpdate at replicaCount 1, added
during round-2 review and removed in round 3. It is worth knowing why, because
the mechanism was sound and only the justification was invented:

  - THE SUBJECT IS WHAT THIS CHART RENDERS, NOT WHAT THE PROJECT IS BUILDING
    TOWARD. Every wrong answer below came from substituting the second for the
    first. What it renders now: an emptyDir at the hub's state directory, a
    read-only settings.yaml projected into it, and no --db on argv. Both volumes
    are pod-local and one of them cannot be written, so REPLICAS STILL SHARE NO
    MUTABLE STATE - which is the property the argument below actually needs.
  - isHADeployment IS TRUE HERE AND WAS FALSE AT PHASE 0, BY TWO INDEPENDENT
    ROUTES. This bullet used to say the chart mounted no volumes and that
    isHADeployment (cmd/server_foreground.go:927) was false at every replica
    count. Both halves were true while the chart rendered no settings file. Now
    a rendered server.database.driver of postgres satisfies the test at :931,
    and server.storage.provider gcs together with server.auth.mode proxy
    satisfies the one at :934. GKE does not set K_SERVICE, but hub.extraEnv can
    and it renders - measured - so that route is reachable from this chart too,
    and assertHAUnlanded transcribes all three rather than two.
    hostedHAGuardsRequired (:921) is therefore satisfied and
    the hosted HA preflight DOES run - which is why this chart renders the five
    Block-1 keys in both auth modes rather than leaving them to the hub.
  - The stated harm - "two hubs writing the same RWX workspace share" - is still
    not a thing this chart can produce, and the reason is now narrower than "no
    volumes". It is that the share itself does not exist: no RWX volume, no
    workspace_storage section, nothing two pods can both write.
  - The replacement harm - "the upgrade transiently enters HA mode" - has to be
    stated more carefully than it was. HA DETECTION is on, at one replica and at
    ten, so "it is off" is no longer the answer. The answer is that entering the
    hub's HA mode costs nothing here, because every consequence of it is about
    shared mutable state and there is none to share. The refusal was wrong for
    the reason above, not for this one.
  - LATER PHASES CHANGE THIS AGAIN, WHICH IS WHY IT IS WRITTEN DOWN RATHER THAN
    LEFT AS AN ABSENCE. Cloud SQL supplies the database URL behind the postgres
    driver this chart already renders; the workspace-share phase lands the RWX
    volume.
    Concurrency correctness becomes a live question at that point, and it is
    answered there by Postgres advisory locks (pg_try_advisory_lock,
    pkg/provision/provision.go:109-116, "for cross-node mutual exclusion", and
    the blocking pg_advisory_lock in migrateStore at
    cmd/server_foreground.go:1168-1200) - by the hub, not by this chart. If a
    refusal is ever warranted here, it will be warranted then, and it will need
    a harm found in that tree.
  - And no third candidate can rescue it, without anyone having to look for
    one. The refusal triggered only when replicaCount <= 1. Any harm from
    CONCURRENCY is strictly worse at two replicas - permanent instead of
    bounded by one startup - and the chart accepts two replicas without a word.
    A refusal whose trigger is "only one replica" cannot be discriminating on
    concurrency, whatever the harm turns out to be.
  - On the one axis where concurrency IS documented to cost something - more
    than one replica breaks the web terminal and exec for (N-1)/N of requests,
    see replicaCount in values.yaml - the refusal was backwards: it forced
    Recreate, which loses every session with certainty.

The shape that found it is still a good heuristic and it is still worth running:
an enum on one field, a minimum on another, and the property about the pair. It
is how probes.startup's budget was found, and that one was real. But the shape
tells you where to LOOK. The harm has to be found in source before anything is
refused, and that is the half all three of us skipped here. Recorded in
verification/p0-product-invariant-sweep.md as a benign shape-match, next to the
instance that was not.
*/}}
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
shared, so the places that call it apply the same test rather than each growing
its own near-miss version of it.

WHERE IT IS ACTUALLY CALLED FROM, AND WHERE IT IS NOT. This comment used to say
it covered "every place in this chart that puts an operator-supplied value
somewhere world-readable". That was false when it was written: the only callers
were five sites inside scion-hub.hubArgs, and a whole-render sweep found
fourteen unguarded surfaces across five object kinds. The sentence told the next
author the surface was already covered, which is worse than saying nothing.

What calls it today:
  - scion-hub.hubArgs, on every rendered argument (argv)
  - scion-hub.assertNoCredentialTree, on every leaf of hub.podAnnotations,
    hub.podLabels, hub.nodeSelector, hub.tolerations, hub.affinity,
    hub.resources, service.annotations and serviceAccount.annotations

WHAT IS STILL NOT COVERED, NAMED RATHER THAN IMPLIED: the scalar NAME fields -
nameOverride, fullnameOverride, serviceAccount.name, rbac.agentNamespace,
runtime.namespace and image.tag. A credential placed in any of those renders
today. Some of them are constrained by a grammar in values.schema.json that a
credential provably cannot satisfy, which is a stronger guard than this one
because it cannot drift; the ones that are not so constrained are a live gap.
Do not read the list above as "the surface is covered" - read it as the list it
is, and check it against the render before you trust it.

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
Apply the credential check to every leaf of an operator-supplied subtree.

Call as:
  {{- include "scion-hub.assertNoCredentialTree" (dict "value" .Values.hub.podAnnotations "source" "hub.podAnnotations") }}

WHY THIS EXISTS, AND IT IS NOT A CONVENIENCE WRAPPER. scion-hub.assertNoCredential
was called from five places, all inside scion-hub.hubArgs, while its own comment
claimed it covered "every place in this chart that puts an operator-supplied
value somewhere world-readable". A whole-render sweep found FOURTEEN unguarded
surfaces across five object kinds - not only the pod template, but Service and
ServiceAccount annotations, which an argv-reading instrument cannot see at all
because neither object has an argv. The comment described a policy the code did
not implement, which is worse than no comment: it tells the next author the
surface is already covered.

IT WALKS, RATHER THAN CHECKING toYaml OF THE WHOLE SUBTREE, AND THE DIFFERENCE
IS NOT STYLE. Two of the three axes in scion-hub.assertNoCredential are anchored
- the prefix axis matches on (^|=). Against a toYaml blob the only leaf at the
start of the string is the first one, so "annotations.x: sk-livekey..." would
render clean while the same value as the first key would fail. A guard whose
result depends on map iteration order is worse than a missing guard, because it
passes in testing. Walking to the leaves puts every value at ^ where the axis
expects it.

THE SOURCE STRING IS BUILT UP AS IT DESCENDS, so the failure names the exact key
- "hub.podAnnotations.example.com/dsn" rather than "hub.podAnnotations" - which
matters most on the nested surfaces (tolerations, affinity, resources) where the
operator has no other way to find which leaf tripped it.

IT APPLIES THE VALUE AXIS ONLY, DELIBERATELY. The name axis
(scion-hub.assertNoCredentialName) judges a hyphen-segmented flag name and fails
on any underscore; annotation and label keys are neither, and routing them
through it would reject "app_version: 1.2" as a caller error. Detecting a
credential by the name of the key holding it is also the defect filed as
finding 2. The value axis is the one that catches a DSN whatever the key is
called.
*/}}
{{/*
Assert that NO operator-supplied value anywhere in .Values is credential
material. Called once at the top of every template that renders an object.

Call as:
  {{- include "scion-hub.assertNoCredentialsInValues" . }}

IT TAKES THE WHOLE OF .Values AND NOT A LIST OF PATHS, AND THAT IS THE ENTIRE
POINT OF IT. Three separate attempts to enumerate this surface came up short in
one morning: a report of three unguarded values paths turned out to be fourteen;
an enumeration of twenty-six refusal sites could not contain a surface that has
no refusal site to enumerate; and a hand-picked subset chosen by "where a
credential is a plausible mistake" would have excluded nameOverride and
fullnameOverride, which are the two widest surfaces in the chart - they place
the operator's string into all five objects it emits.

A shorter list, a longer list and a cleverer list all share the same defect: a
values path added next month is not on any of them. Walking .Values inverts the
default. A new value is covered on the day it is added, and the only way to
escape the check is to add something that is not in .Values at all.

WHY IT DOES NOT CONDITION ON WHETHER THE VALUE REACHES A MANIFEST. Checking
only values that this particular render happens to emit would make coverage
depend on other values - serviceAccount.annotations would be checked when
serviceAccount.create is true and silently skipped when it is false. That is a
guard switched off by a condition, which is the defect class this chart has
spent the most effort on. It also gets the threat model wrong: a values file
carrying a DSN is committed to a repository whether or not the chart renders it.

WHAT IS EXCLUDED, AND IT IS EXCLUDED BY A GRAMMAR RATHER THAN BY OPINION.
Leaves that values.schema.json types as integer or boolean, and the two closed
enums (image.pullPolicy, updateStrategy.type), cannot express a credential at
all - the schema rejects any string before this check runs. That exclusion is
falsifiable and was tested. The two scalars that DO carry a string pattern were
tested the same way and both FAILED the test, so neither is excluded:
serviceAccount.gcpServiceAccount's pattern accepts "AKIAIOSFODNN7EXAMPLE" as a
local part because an AWS access key ID is [A-Z0-9]+, and hub.hubId's pattern
"^\S(.*\S)?$" accepts a DSN. A grammar is only a guard if it excludes EVERY
credential, not if one credential happens to fail it.
*/}}
{{- define "scion-hub.assertNoCredentialsInValues" -}}
{{- include "scion-hub.assertNoCredentialTree" (dict "value" .Values "source" "values") }}
{{- end }}

{{- define "scion-hub.assertNoCredentialTree" -}}
{{- $source := .source }}
{{- $v := .value }}
{{- if kindIs "map" $v }}
{{- range $k, $e := $v }}
{{- include "scion-hub.assertNoCredentialTree" (dict "value" $e "source" (printf "%s.%v" $source $k)) }}
{{- end }}
{{- else if kindIs "slice" $v }}
{{- range $i, $e := $v }}
{{- include "scion-hub.assertNoCredentialTree" (dict "value" $e "source" (printf "%s[%d]" $source $i)) }}
{{- end }}
{{- else if $v }}
{{- include "scion-hub.assertNoCredential" (dict "value" $v "source" $source) }}
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
   chart either. Each of them touches how the hub's configuration or its project
   context is selected - and NOT, as this comment claimed for three rounds, by
   redirecting where the configuration is read from.

   THAT HEADER WAS WRONG ABOUT THE MECHANISM FOR EVERY MEMBER OF THIS LIST, and
   "imprecise" would be the wrong word for it: the model it gave the reader was
   false and has to be removed rather than softened. config.GetGlobalDir()
   (pkg/config/paths.go:188-193) is os.UserHomeDir() joined with a constant. IT
   TAKES NO ARGUMENTS. There is no flag input on this command that can move the
   global configuration directory - not --config, not --project, not --profile,
   not --global. Anyone re-deriving this list should start there, because it
   disposes of the whole family in four lines.

   The reservations stand. Each now carries the reason that is actually true of
   it, and the reasons differ, so they are given per entry.

   DO NOT REMOVE THESE BECAUSE THE CHART DOES NOT SET THEM. That is the point of
   them, not evidence that they were added by mistake. There is no legitimate
   reason for this chart to emit any of them, so "nothing in the rendered args
   matches this entry" is the expected steady state forever.

   --config AND -c: READ THIS BEFORE YOU CHANGE OR CHECK IT. It reaches exactly
   one place on this command's path: config.LoadGlobalConfig(serverConfigPath),
   cmd/server_foreground.go:827, via cmd/server.go:237.

   THE STATE OF THIS FLAG, IN THE ONLY FORM THAT STAYS TRUE:

     LIVE   while the GLOBAL settings document has no non-nil top-level server:
            key. That is Phase 0, this commit.
     INERT  from the moment it has one. That is what the configuration phase is
            for.
     RESERVED IN BOTH STATES, which is the whole reason this entry is written as
            a state machine rather than as a description.

   READ THIS PARAGRAPH AND YOU MAY STOP: WHY THE ENTRY DOES NOT DEPEND ON ANY OF
   THE MECHANISM BELOW. Five separate readings of this flag have been asserted
   confidently and wrongly in a single day, and the reservation was correct under
   every one of them, because A RESERVED FLAG REFUSES THE FLAG INSTEAD OF
   REASONING ABOUT ITS EFFECT. Everything after this paragraph is an explanation;
   none of it is load-bearing for the refusal. That is deliberate, and it is the
   answer to the maintainer who audits this list by auditing effects: you will
   keep finding the effect description imperfect, and THE ENTRY DOES NOT REST ON
   THE EFFECT DESCRIPTION. It also covers a moving target - a mitigation scoped to
   "prevent redirection" does not cover an overlay, and one scoped to "prevent an
   overlay" does not cover a sole-source substitution, and this flag does two of
   those three depending on which loader is reached.

   THE TRANSITION IS THE server: KEY. NOT THE EXISTENCE OF A FILE, NOT WHO WROTE
   THE FILE, NOT WHETHER THIS CHART MOUNTS ONE. Three separate corrections
   collapsed into that one sentence and each of them was a confident wrong answer
   first, so the reasons are worth keeping:

     - loadServerFromSettingsFile (pkg/config/hub_config.go:1331-1347) reads the
       file and then tests raw["server"] for present AND NON-NIL. It never asks
       whether the file exists as a separate question, and "server: ~" parses,
       has the key, and is still not found.
     - THE GLOBAL settings.yaml IS THIS CHART'S, AND IT CARRIES A server KEY.
       That is the whole transition, and it is why the flag is inert here rather
       than merely discouraged. The chart mounts an emptyDir at $HOME/.scion and
       lands its rendered settings.yaml into it as a subPath, so the file
       GetGlobalDir() resolves to is the file this chart wrote.
     - THE HUB DOES NOT SEED ONE OVER THE TOP, AND THE REASON IS THE MOUNT. The
       seeding call is guarded at cmd/server_foreground.go:104 by
       os.Stat(globalDir) / os.IsNotExist, so config.InitGlobal (:107) fires only
       when $HOME/.scion is ABSENT. The emptyDir makes the directory exist before
       the process starts, so the guard takes the else branch and the embedded
       defaults in pkg/config/embeds/default_settings.yaml - schema_version,
       active_profile, default_template, default_harness_config, image_registry,
       cli, runtimes, profiles, AND NO server KEY - are never materialised. The
       rendered file is the only settings.yaml in the container.
     - THE PREVIOUS PARAGRAPH WAS RIGHT FOR A REASON THAT NO LONGER APPLIES, and
       it is kept here because the reasoning is what a later phase needs. At
       phase 0 the chart delivered no ConfigMap and no Secret, the directory did
       not exist, InitGlobal fired, and the seeded file had no server key - so
       the flag was LIVE, by a different route to a different answer. Anything
       that changes what is at $HOME/.scion changes which of these two paragraphs
       applies: mounting a volume with no settings.yaml in it puts the flag back
       to LIVE, and nothing in the render inspects that.

   THREE VALUES, NOT TWO. Reading the flag as binary is what produced two of the
   wrong answers above. In order of evaluation:

     :647  The GLOBAL settings.yaml is read FIRST and unconditionally. A non-nil
           server key here wins and the --config path is NEVER READ. This is the
           Phase 1 state.

     :648-659  ROUTE A - SOLE-SOURCE SUBSTITUTION, and it is NOT an overlay. When
           the global read finds nothing, and --config is set and stat-able, the
           DIRECTORY of the --config path is searched for a settings.yaml, and a
           non-nil server key found there is returned as the server config, built
           from that file ALONE (ConvertV1ServerToGlobalConfig, :1360). This is
           what an operator pointing --config at a directory actually hits, and it
           fires BEFORE the overlay below.

           A detail of route A worth its own sentence, because it is the kind that
           produces an unfalsifiable bug report: IT DOES NOT READ THE FILE THE
           OPERATOR NAMED. :651-656 stats the path and takes filepath.Dir of it
           when it is not a directory, then appends settings.yaml. So
           --config /etc/scion/myserver.yaml reads /etc/scion/settings.yaml, a
           file the operator never mentioned, and ignores the one they did.

     :699, :772-788  ROUTE B - THE OVERLAY, reached only when route A also finds
           nothing. loadGlobalConfigLegacy loads embedded defaults, then the
           global ~/.scion/server.yaml (:773-775), then LAYERS the --config path
           on top (:778-787). Here a directory means server.yaml or server.yml
           (loadServerConfigFile, :1314-1322) while a file is loaded verbatim
           (:785) - so THE SAME FLAG VALUE SELECTS A DIFFERENT FILENAME IN ROUTE A
           THAN IN ROUTE B.

   Bigger than an overlay, smaller than "the whole configuration load moves",
   which is what this comment claimed for three rounds.

   AND IT GOES INERT IN COMPLETE SILENCE, WHICH IS THE REASON A RESERVED FLAG IS
   THE ONLY GUARD AVAILABLE. There is no deprecation warning on --config and no
   log line of any kind. --config is a plain StringVarP at cmd/server.go:237 and
   carries no MarkDeprecated anywhere. Exactly two flags reachable on server start
   do carry one: "production", marked on serverStartCmd itself at cmd/server.go:236,
   and "grove", marked on rootCmd's PERSISTENT flags at cmd/root.go:251 and so
   inherited here. An earlier version of this sentence read "MarkDeprecated on
   server start covers only production (cmd/server.go:236, :290)" and was wrong
   twice in one clause - it missed the inherited --grove, and :290 is that same
   mark on serverInstallCmd, a DIFFERENT COMMAND, offered as though it were a
   second site on this one. Neither error touches the conclusion about --config,
   which is exactly why it survived three readings: A CITATION THAT FAILS IN A
   PLAUSIBLE WAY IS WORSE THAN ONE THAT FAILS OBVIOUSLY. The
   only two warnings anywhere on this path (pkg/config/hub_config.go:668 and
   :678) fire on server.yaml coexisting with settings.yaml and are not about
   --config at all. An earlier version of this comment claimed a deprecation
   warning; it does not exist, and "accepted and silently ignored" is a worse
   defect than "redirects", not a milder one - a redirect is at least detectable
   by its effects.

   THE ONE FEEDBACK PATH THE FLAG CAN PRODUCE IS WORSE THAN SILENCE. Point
   --config at a directory that also holds a server.yaml and :678 prints "Both
   settings.yaml (server key) and server.yaml exist in <their directory>. Using
   settings.yaml." - naming THEIR directory while the settings.yaml actually in
   force is the global one. The only diagnostic available reads as confirmation
   that their file was loaded.

   THE STATE IS A PROPERTY OF THE DEPLOYED CONFIGURATION, NOT OF THE BINARY, which
   is why the reservation outlives every reading of the flag: any phase can move it
   between LIVE and INERT without touching this file, this list, or cmd/. A
   reservation is the only form of this knowledge that survives that move, because
   it does not depend on which state is current.

   WHAT THE INERT HALF REQUIRES OF THE CONFIGURATION PHASE - stated here as a
   requirement, not as an observation of code that exists at this commit: the
   GLOBAL settings document must carry a top-level server: key that is a MAP.
   Nesting it under a profile, renaming it, splitting it across two files, or
   emitting it as an empty key all leave --config LIVE while this comment would say
   inert. That is the failure this entry cannot catch for you, because by then the
   flag is refused at render time and the settings file is wrong in a way no render
   inspects.

   FIVE READINGS OF THIS FLAG HAVE BEEN ASSERTED CONFIDENTLY AND WRONGLY, ALL IN
   ONE DAY: "redirects the entire configuration load"; "no-ops with a deprecation
   warning"; "the chart mounts nothing, so no settings file exists"; "the hub
   creates one on every boot"; and "it overlays" - each an improvement on the last
   and each still wrong. THE SHAPE THEY SHARE IS WHY THEY SURVIVED: every one of
   them reaches the correct conclusion, that the flag must be refused, so checking
   the conclusion returns "yes" and confirms the reason on the way past.
   AGREEMENT ON A CONCLUSION IS NOT CORROBORATION OF ITS REASON. If you are about
   to correct this paragraph a sixth time, compare derivations with whoever you
   agree with, not answers - that is the only comparison that has ever caught one
   of these.

   --project, -g, --grove, --profile and -p: THE REASON HERE IS DIFFERENT AND
   WEAKER THAN THE ONE THIS COMMENT USED TO GIVE, and it is written out rather
   than borrowed from --config because borrowing it is what made it wrong. They
   are declared in cmd/root.go as PERSISTENT flags, which is why three
   enumerations of cmd/server.go missed them.

   What they do NOT do: move the global configuration directory, or reach
   LoadGlobalConfig - the loader every paragraph above is about - at any point.
   The hub's server configuration is not reachable from any of them.

   What --project/-g/--grove DO do on this command, traced from the flag
   declaration outward: they set projectPath (cmd/root.go:249-250 - :248 is
   rootCmd.Long, not a flag declaration), and
   PersistentPreRunE passes projectPath to config.LoadSettings at cmd/root.go:123
   and config.LoadEffectiveSettings at :129 for EVERY command, server start
   included. Those two select which project's settings.yaml supplies cli.autohelp
   and cli.interactive_disabled, and the second can force the process
   non-interactive. It also reaches printDevAuthWarningIfNeeded (:187), which
   loads the same file again to decide whether to warn. That is a narrower effect
   than this comment used to claim - CLI-level settings, not the hub's server
   config - but it is a real one, it is configuration selection, and the chart's
   guarantee is over the whole rendered command line. The project-required check
   at :117 is NOT among them: :106-108 clears requiresProject for the server
   subtree.

   --grove binds the SAME VARIABLE as --project (cmd/root.go:249-250), so
   reserving one without the other leaves the alias open - the hosted/production
   pattern again. It is also MarkHidden, so it will not appear in --help to
   whoever checks.

   --profile/-p IS THE WEAK ENTRY AND IS LABELLED AS ONE. Its only consumer on
   this path is config.RequireImageRegistry at cmd/root.go:181, and that call is
   skipped for the server subtree at :168-170; LoadSettings does not take it.
   Every other consumer of the variable is a client subcommand. So it is INERT on
   server start today and I could find no harm for it - by axis (d), that is the
   same answer that removed the updateStrategy refusal. It survives here on the
   one ground that differs: a reserved flag costs an operator nothing they have
   any reason to want, whereas the updateStrategy refusal blocked a configuration
   operators do want. If you disagree, MOVE IT, DO NOT DELETE IT, and name the
   list it lands in - $aliasOrIgnored is where an inert-but-plausible flag goes,
   which is where --port sits for the same reason.

   THE TRIGGER THAT WOULD MAKE --profile/-p A STRONG ENTRY, named here so nobody
   has to re-derive the weakness to find out whether it still holds: any phase
   that adds a profile-aware consumer to the SERVER path, or that passes profile
   into config.LoadSettings or config.LoadEffectiveSettings. Either one turns this
   into an ordinary $neverPassed entry with the same reason as --project, and this
   paragraph should then be deleted rather than softened. Until one of them
   happens the entry is weak and is to be described as weak.

   Note that --global is NOT here. The chart renders it, so it is in $setByChart,
   which rejects it just as absolutely. Its hazard is its own and is not the
   --config hazard by another route, which is what this comment used to claim:
   --global makes the server chdir to $HOME so it operates from the global project
   context (cmd/server_foreground.go:120-130), so --global=false leaves the hub
   running from the container's working directory instead. It does not move
   $HOME/.scion, because nothing does.
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
4. THESE ARE DELIVERED THROUGH A CHANNEL OTHER THAN argv, AND argv WINS OVER IT
   SILENTLY. Two of the five are delivered by this chart and three are not, and
   that split is the paragraph. It was one claim about five flags until the
   settings rendering landed, and it is two claims now.

   DELIVERED HERE. argv is a second source for these today:

     base-url        SCION_SERVER_BASE_URL, templates/configmap-env.yaml:57,
                     reaching the container by envFrom at
                     templates/deployment.yaml:147-148.
     storage-bucket  server.storage.bucket in the rendered settings.yaml.

   NOT DELIVERED HERE. Live on argv, nothing to disagree with, would simply take
   effect if passed:

     db              cfg.Database.URL, cmd/server_foreground.go:875-877.
                     Arrives with Cloud SQL.
     storage-dir     cfg.Storage.LocalPath, cmd/server_foreground.go:890-892.
                     Arrives with the workspace share.
     admin-emails    cfg.Hub.AdminEmails, cmd/server_foreground.go:1402-1409 and
                     :2116-2124. Both sites read argv first and consult the
                     settings file only when argv is empty. No phase claims it.

   THIS HEADER WAS CORRECT AND STOPPED BEING CORRECT WITHOUT THE FILE BEING
   EDITED. It read "there is no second source yet for anything to disagree with"
   and "none of them lands anywhere", which were true while the chart rendered no
   ConfigMap, no Secret and no volumes, and false the moment it rendered all
   three. The refusal at the bottom of this file went stale in the same instant
   and for the same reason, which is why they are fixed together: this header is
   that refusal's justification, and fixing one alone leaves the file arguing
   with itself. An unchanged file is not evidence that its claims about the rest
   of the chart still hold - it is how they go stale unnoticed.

   ALL FIVE STAY RESERVED, AND NOT BY INERTIA. Before removing an entry, name
   where it lands instead. For base-url, storage-bucket and - since the Cloud SQL
   phase - db, that is the first list above, and the answer is still not argv.
   For the other two, admin-emails and storage-dir, it is nowhere yet. None of
   the five is rendered as an argument ($setByChart), none selects
   which configuration is loaded ($neverPassed), none is inert or misnamed
   ($aliasOrIgnored), and none weakens authentication ($unsafeToPass).

   The harm is present for three of the five and scheduled for the other two.
   Passing -base-url, -storage-bucket or -db today makes argv the silent winner
   over a value this chart rendered, and nothing logs the disagreement. -db
   joined that group when the Cloud SQL phase started rendering
   server.database.url, and the move was forced rather than remembered:
   hack/verify.sh carries the delivery state as a committed number per flag and
   goes red when a channel appears without this paragraph being re-tensed in the
   same diff. Passing admin-emails or storage-dir today changes a setting nothing
   else sets; the same silent overriding starts the day its channel lands, with
   no edit here to mark it. The
   asymmetry is what decides it - reserving costs an operator a flag they have no
   reason to want, un-reserving is a deliberate act with a place to record itself
   (see the closing paragraph), and reserving after the fact requires somebody to
   notice.

   Not verifiable against the rendered arguments, by construction - the rendered
   argument list is where these must NOT appear. Check them against the channel,
   which hack/verify.sh now does: it asserts the first list against the render and
   the second against its absence, so moving an entry between the two lists
   without moving the code, or the reverse, is a red test rather than a paragraph
   nobody re-reads.

   NAME THE CHANNEL WHEN YOU ADD AN ENTRY, because it is not the same channel for
   every entry and the precedence differs. admin-emails, db, storage-bucket and
   storage-dir belong to the settings file; base-url belongs to the
   SCION_SERVER_BASE_URL environment variable.

   Precedence for base-url, read from the hub rather than assumed, because "two
   sources" only matters if one of them silently loses:

     cmd/server_foreground.go:2102 (initWebServer, the OAuth redirect base)
       --base-url, else SCION_SERVER_BASE_URL, else http://localhost:<web-port>
     cmd/server_foreground.go:1310 (resolveHubEndpoint, the URL agents dial)
       settings file server.hub.public_url, else --base-url, else
       SCION_SERVER_BASE_URL, else project settings, else localhost

   Two consequences, both silent. ARGV BEATS THE ENVIRONMENT AT BOTH SITES, and
   the environment variable is one this chart renders, so an argv --base-url
   shadows a live value rather than a hypothetical one, with no error and, unless
   --debug is on, no log line either. And the two sites do not
   agree with each other - the settings file outranks argv when resolving the
   agent-facing endpoint but is not consulted at all for the OAuth redirect - so
   argv plus a settings file that sets public_url yields two different base URLs
   in one process, each correct by its own rule.

   That is why this entry is reserved rather than merely discouraged. A later
   phase may still choose argv as the delivery channel for base-url, and this
   list is where it says so: move the entry, do not add a second emitter beside
   the existing one. --config is the one flag that CANNOT be promoted this way,
   which is why it sits in $neverPassed instead: the condition under which
   --config stops being read is a non-nil top-level server: key in the global
   settings document, and that key is exactly what the configuration phase
   delivers (see the $neverPassed comment). Choosing --config as a delivery
   channel would disable it.
*/}}
{{- $ownedByConfig := list "admin-emails" "base-url" "db" "storage-bucket" "storage-dir" }}

{{- /*
5. These weaken authentication or place credentials where they can be read.

   PER-ENTRY, WITH THE EFFECT TRACED, because "unsafe" was the entire stated
   reason for four flags until round 4 and an unexplained reservation is the kind
   that gets deleted by whoever first wants the flag. All four are declared on
   serverStartCmd, so all four reach this command.

   ALL FOUR ARE LIVE AT THIS HEAD. Unlike $ownedByConfig above, nothing here is
   forward-looking: each of these takes effect today, on this chart, as rendered.

   --session-secret (cmd/server.go:275) is the signing key for the web session
   cookie store and the hub's JWT signing keys. Two harms, not one. It is a
   credential on argv, readable by anyone who can read the pod spec - which the
   credential guard below would also catch, but only if the operator's value
   happens to look like a credential, and a passphrase does not. And it PRE-EMPTS
   the delivery channel: resolveSessionSecret (cmd/server_foreground.go:1452-1456)
   takes the flag first and only falls back to SCION_SERVER_SESSION_SECRET, so an
   argv value silently outranks the Secret-backed environment variable the
   session-secret phase mounts. THAT CHANNEL IS NOT THE SECRET THIS CHART ALREADY
   RENDERS, and the distinction is worth the sentence: this chart renders a Secret
   holding settings.yaml, and the settings file has no session-secret key at all:
   there is no such field anywhere in V1ServerConfig, and resolveSessionSecret
   reads exactly three sources in order - the flag, SCION_SERVER_SESSION_SECRET,
   then bare SESSION_SECRET for compatibility - none of which is a file. So the
   presence of a Secret in the rendered output says nothing about
   this claim, which stays forward-tensed until SCION_SERVER_SESSION_SECRET is
   emitted. Measured at this head: zero occurrences of SESSION_SECRET in every
   permutation's render.

   --dev-auth (cmd/server.go:251) IS A DIRECT WRITE TO cfg.Auth.Enabled AT
   cmd/server_foreground.go:884-886, AND THE DANGEROUS DIRECTION IS TRUE, NOT
   FALSE. Worth stating explicitly because the natural reading is backwards. The
   workstation defaults that would turn dev auth on (applyWorkstationDefaults,
   cmd/server_config.go:35-37, and the assignment at server_foreground.go:843) are
   BOTH inside an if !hostedMode block, and this chart renders --hosted, so they
   do not run: cfg.Auth.Enabled is false here and --dev-auth=false merely restates
   the default. Passing --dev-auth (or =true) is what changes something - it
   satisfies the gate at :212 and initialises dev-token authentication in a hosted
   deployment, standing an auto-generated static token up beside the real identity
   path. That one is not silent (:216-217 logs a warning in hosted mode), but a
   warning in a log nobody reads is not a control, and a render error is.

   --enable-test-login (cmd/server.go:252) is wired to the web server at
   cmd/server_foreground.go:2163 and registers POST /api/v1/auth/test-login
   (pkg/hub/web.go:747). The route is always mounted and the flag is the gate:
   pkg/hub/handlers_test_login.go:52-55 returns 403 unless it is set, after which
   the handler mints a user session behind a challenge token rather than the
   configured identity provider. Also warned about at :2167-2168, and the same
   answer applies.

   --web-assets-dir (cmd/server.go:274) replaces the embedded web UI with a
   directory served straight off the container filesystem: pkg/hub/web.go:497
   stores it and :832-833 hands it to http.FileServer(http.Dir(...)). It is here
   rather than in $ownedByConfig because the hazard is not a second source for a
   value - it is that the served asset tree stops being the audited one that was
   built into the image.
*/}}
{{- $unsafeToPass := list "session-secret" "dev-auth" "enable-test-login" "web-assets-dir" }}

{{- /*
ADJUDICATED AND DELIBERATELY NOT RESERVED. The five lists above say what is
refused; a reader auditing them for COMPLETENESS needs to know which flags were
considered and let through, or they re-derive the same six every time. Round 4
raised these by name. None is reserved, and the ground is given per flag rather
than as one blanket sentence, because a blanket sentence is what would survive a
change that falsified it.

  --no-auto-migrate (cmd/server.go:244). RAISED AS THE ONE MOST LIKELY TO
    INTERACT WITH THE STARTUP BUDGET. IT DOES NOT, and that is the finding, not
    an absence of one: it gates only the in-process upgrade of a LEGACY raw-SQL
    hub.db to the Ent schema (cmd/server_foreground.go:1263-1266, which errors out
    when it finds one and the flag is set). CompositeStore.Migrate - the ordered
    sequence assertStartupBudget is written against - is not behind it and runs
    either way. A fresh GKE pod has no legacy hub.db, so the flag is inert here.
  --debug (cmd/server.go:255). Logging verbosity. Note it SHADOWS the persistent
    --debug at cmd/root.go:267: a local flag of the same name wins, so this sets
    enableDebug and not debugMode. Harmless either way, and recorded only so the
    duplicate is not mistaken for a finding later.
  --runtime-broker-port (cmd/server.go:248). Sets cfg.RuntimeBroker.Port
    (server_foreground.go:881-883). The chart renders --enable-runtime-broker but
    no port, exposes no broker port on the Service and points no probe at one, so
    moving it stays internally consistent inside the pod.
  --template-cache-dir, --template-cache-max (cmd/server.go:262-263). Cache
    location and size. No auth, config-selection or credential surface.
  --simulate-remote-broker (cmd/server.go:266). Test-path selector that skips
    co-located optimisations. Degrades performance, weakens nothing.

IF YOU ARE ADDING TO THIS BLOCK, the bar is the one axis (d) sets: a flag stays
off the reserved lists when no harm was FOUND, not when none was looked for. Say
which it was.
*/}}

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

KNOWN LIMIT OF THE SCAN BELOW: it classifies an element as a flag by a leading
"-", so a flag whose VALUE is in the next element and itself starts with "-"
would be counted as a flag of its own, and direction A would report a chart
defect naming something that is not a flag. Nothing the chart renders today
looks like that - every space-separated value is an address, a port or a path -
and the failure is loud, immediate and names the offending token, so it cannot
ship silently. THE REMEDY IF IT EVER FIRES: walk $args pairwise, remembering
whether the previous element was a flag that takes a separate value, instead of
testing each element independently. Written down rather than pre-implemented,
because the pairwise walk needs a list of which flags take values, and that list
would be a third thing to keep in step with the command.
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
would crash-loop as an unknown flag rather than reach either loader. This turns
that crash-loop into a render error. The name axis lowercases too, and did before
this did, which is how the inconsistency was found.

"Rather than reach either loader" is doing deliberate work and replaced "rather
than redirect the config load". The old wording used the one verb the $neverPassed
comment 260 lines above spends four paragraphs establishing is wrong, so the file
contradicted itself and the next reader would have resolved it in whichever
direction was less work. Naming the routes instead of picking a verb is the fix
that stays correct: --config reaches a sole-source substitution on one path and an
overlay on the other, and no single verb covers both.
*/}}
{{- if hasPrefix "-" $arg }}
{{- $flag := lower (trimPrefix "-" (trimPrefix "--" (first (splitList "=" $arg)))) }}
{{- if has $flag $setByChart }}
{{- fail (printf "hub.args may not contain -%s: the chart renders it, and pflag is last-wins, so this would silently replace the chart's value rather than conflict with it - disabling hosted mode, unbinding the listener, taking the daemon fork so PID 1 exits, leaving /readyz unregistered, or leaving the runtime broker off in a pod that still reports Ready and can never launch an agent." $flag) }}
{{- end }}
{{- if has $flag $neverPassed }}
{{- fail (printf "hub.args may not contain -%s: it changes which configuration the hub selects, and the chart's guarantee is that the configuration in force is the configuration it rendered. -config and -c either replace the hub's whole server section with one read from the directory they name - not from the file named on the flag - or layer a file over it, depending on which loader is reached; and once the global settings document carries a non-nil top-level server: key, the same flag is accepted and ignored instead. That key is the trigger, not the existence of any file. All three outcomes are silent: no warning, no log line, nothing observable from the running pod. -project, -g and -grove change which project's settings supply the CLI's own behaviour, including whether it runs non-interactively. -profile has no effect on this command today and is reserved against acquiring one. In every case the rendered values keep reporting the operator's intent while the process may not be following it." $flag) }}
{{- end }}
{{- if has $flag $aliasOrIgnored }}
{{- fail (printf "hub.args may not contain -%s: it is not the lever it looks like. -production is a deprecated alias bound to the same variable as -hosted, so passing it can disable hosted mode; -port is ignored whenever -enable-web is set, which this chart always sets, so passing it changes nothing observable. The chart renders neither, which is why this is a separate reservation and not a stale entry." $flag) }}
{{- end }}
{{- if has $flag $ownedByConfig }}
{{- fail (printf "hub.args may not contain -%s: this setting has a delivery channel other than argv - the settings file, or for base-url the SCION_SERVER_BASE_URL environment variable - and argv silently wins over both, so an argv copy is a second and invisible source for one value, with nothing reporting the disagreement. Two of the five are live in this release: -base-url is shadowed onto the SCION_SERVER_BASE_URL this chart renders, and -storage-bucket onto server.storage.bucket in the settings file it renders, so passing either makes argv the winner over a value already set here. The other three - -db, -storage-dir and -admin-emails - have no second source in this release and would simply take effect; they stay reserved because the channel arrives on a schedule and reserving after the fact requires somebody to notice." $flag) }}
{{- end }}
{{- if has $flag $unsafeToPass }}
{{- fail (printf "hub.args may not contain -%s: it weakens authentication or places credential material where anyone with pod read access can read it." $flag) }}
{{- end }}
{{- include "scion-hub.assertNoCredentialName" (dict "name" $flag "source" "hub.args flag") }}
{{- else if contains "=" $arg }}
{{- /*
A POSITIONAL IS IGNORED BY pflag, BUT ITS TEXT IS STILL ON argv.

The reserved-list checks above are deliberately flag-only: pflag does not read a
positional as a flag, so `global` or `c` alone cannot replace anything the chart
set, and failing the render on them was a false positive that blocked legitimate
values. That reasoning is sound for the BARE word, and the bare word is the
benign member of the class.

It does not extend to `name=value`. Nothing is SET - pflag still ignores it - but
`session-secret=hunter2` puts the secret in the pod spec and in /proc/1/cmdline,
readable by anyone with pod read access. That is the same harm $unsafeToPass and
the value axis below exist to prevent, reached by a spelling that skips both.

So the name axis, and only the name axis, runs on the left of the `=`. It judges
the NAME, so it stays silent on `global=y` and `token-ttl=5m` and fires on names
whose trailing hyphen-separated word says the value is credential material. No
list is consulted here on purpose: a positional cannot misconfigure the hub, it
can only expose, so exposure is the only thing worth failing on.

That is the whole of what this catches, and the limit is worth stating because
the axis it leans on is narrow. It does NOT catch a credential passed with no
name at all - a bare positional `hunter2` - and it cannot: there is no sound way
to recognise an arbitrary high-entropy string without failing renders that are
fine. The value axis below is not a backstop for that either. It matches URL and
query-string credentials and known prefixes, so it is silent on any secret whose
shape is unremarkable, and separately on any secret containing a character the
userinfo encoder rewrites - which is most of what a password policy would call
strong. Detect by structure, never by matching the value.

The name is trimmed before it is judged. `regexMatch` is anchored, so a derived
name that kept a trailing space could not match the credential pattern and
`session-secret =hunter2` rendered clean with the secret on argv. Neither
whitespace guard above reaches that spelling: the trim check at the top compares
the whole entry and this one's ends are ordinary characters, and the whitespace
check is flag-only, which a positional never enters.

Do not "simplify" this by moving it back inside the hasPrefix block. It exists
precisely because that block does not run for this spelling.

The underscore is translated rather than passed through, which the name axis
asks every caller to decide and which the FLAG path above deliberately does not
do. The two answers differ because the reasons differ. On the flag path an
underscore is a real error - no flag on `server start` has one, so pflag would
reject --some_var and the hub would crash-loop - and failing says so at render
time. A positional is never parsed as a flag, so there is no crash-loop to warn
about and that reason does not transfer. Passing `some_var=value` through
untranslated failed the render with a message telling the operator to translate
before calling, which is advice addressed to this line, not to them: a false
positive of exactly the kind the hasPrefix fix was written to remove. Translated,
`some_var=value` renders and `session_secret=hunter2` fails for its real reason.
*/}}
{{- include "scion-hub.assertNoCredentialName" (dict "name" (lower (replace "_" "-" (trim (first (splitList "=" $arg))))) "source" "hub.args positional") }}
{{- end }}
{{- $args = append $args $arg }}
{{- end }}
{{- range $arg := $args }}
{{- include "scion-hub.assertNoCredential" (dict "value" $arg "source" "hub.args entry") }}
{{- end }}
{{- toYaml $args }}
{{- end }}

{{/*
=============================================================================
Configuration intake: the rendered settings.yaml
=============================================================================

The hub is configured by a settings.yaml file, not by SCION_SERVER_* environment
variables. That is not a style preference.

THE RULE, MEASURED. On the path this chart uses, loadGlobalConfigFromSettings
calls applyEnvOverrides (pkg/config/hub_config.go:683 and :1191), which maps each
name through envKeyToConfigKey (:976): lowercase, split on "_", replace any
segment that has an entry in the camelCaseFields table (:919), join with ".".
So a SCION_SERVER_ name binds if and only if EVERY underscore-separated segment
is either a plain lowercase word matching its koanf tag or has a table entry.
Anything else produces a key that matches no field, and k.Unmarshal (:1198) is
called without ErrorUnused, so it is discarded with no error, no warning and no
log line.

Worked both ways, because the reachable half is the part that was wrong here for
three phases:

  SCION_SERVER_DATABASE_DRIVER  -> database.driver   BINDS
  SCION_SERVER_DATABASE_URL     -> database.url      BINDS
  SCION_SERVER_OIDC_ENABLED     -> oidc.enabled      BINDS
  SCION_SERVER_DATABASE_MAX_OPEN_CONNS -> database.max.open.conns  discarded
                                   (koanf tag is max_open_conns; the underscores
                                    became dots before anything could rejoin them)
  SCION_SERVER_OIDC_ISSUER_URL  -> oidc.issuer.url   discarded, same reason
                                   (OIDCProviderConfig.IssuerURL, koanf issuer_url)
  SCION_SERVER_OIDC_ISSUERURL   -> oidc.issuerurl    discarded, DIFFERENT reason
                                   (OIDCLoginConfig.IssuerURL is koanf issuerUrl
                                    and "issuerurl" has no camelCaseFields entry)

Two failure modes, one symptom. TestEnvKeyToConfigKey has DATABASE_DRIVER as an
explicit passing sub-case, so "the database keyspace binds under no spelling" was
one `go test` away from being checked at any point.

AND A DISCARDED VARIABLE IS NOT SILENT DOWNSTREAM - IT IS REPORTED AS APPLIED.
DetectEnvOverrides (pkg/config/opsettings/koanf.go:347) is `envKoanf.Keys()`: it
returns every SCION_SERVER_ name in the environment, having never asked whether
any of them reached a field. So the admin server-config view lists a dropped
variable as an active override. Worse than silence.
*/}}

{{/* Name of the Secret holding the rendered settings.yaml. */}}
{{- define "scion-hub.settingsSecretName" -}}
{{- if .Values.config.existingSecret }}
{{- .Values.config.existingSecret }}
{{- else }}
{{- printf "%s-settings" (include "scion-hub.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The hub's state directory: hub.home plus /.scion.

Not configurable independently, and there is no lever that separates the config
file from the rest of it. The path is os.UserHomeDir() + "/.scion", hardcoded -
there is no SCION_HOME and no config-path flag - and settings.yaml, storage/,
templates/ and scion-token all live in it. That is why the directory is backed
by a writable emptyDir and only the one file inside it is read-only.
*/}}
{{- define "scion-hub.scionDir" -}}
{{- printf "%s/.scion" (trimSuffix "/" .Values.hub.home) }}
{{- end }}

{{/* Name of the ConfigMap holding the process environment. */}}
{{- define "scion-hub.envConfigMapName" -}}
{{- printf "%s-env" (include "scion-hub.fullname" .) }}
{{- end }}

{{/*
The externally reachable hub URL, as SCION_SERVER_BASE_URL.

Required, and required to be https://. Absence only warns: the hub falls back to
http://localhost:<port>, which agents cannot reach, and - because the session
cookie's Secure attribute is literally strings.HasPrefix(baseURL, "https://") -
an http:// value silently serves session cookies without Secure.

The design conditions the https:// requirement on ingress.enabled. Ingress does
not exist in this chart yet, and there is no plaintext deployment of it, so the
requirement is unconditional here. If a non-TLS path is ever added, relax it
then, deliberately.
*/}}
{{- define "scion-hub.baseUrl" -}}
{{- $url := required "hub.baseUrl is required: the externally reachable URL of the hub, e.g. https://hub.example.com. Without it the hub falls back to http://localhost:<port>, which agents cannot reach." .Values.hub.baseUrl }}
{{- if not (hasPrefix "https://" $url) }}
{{- fail (printf "hub.baseUrl must start with https://, got %q. The session cookie's Secure attribute is derived from this prefix, so a plaintext base URL silently ships session cookies without Secure." $url) }}
{{- end }}
{{- $url }}
{{- end }}

{{/*
Reject any operator-supplied environment variable that would desynchronise the
chart's guards from the hub's configuration.

THE REASON CHANGED AND THE REFUSAL DID NOT. This guard used to be defended on the
grounds that SCION_SERVER_DATABASE_* and SCION_SERVER_OIDC_* are "unreachable by
any spelling". That is FALSE - see the intake section above - and a guard
defended by a false premise gets deleted the moment somebody checks the premise.
gd-p2-dev and gd-p3-dev checked it, from opposite ends, against a passing test in
the repo.

THE HARM, MEASURED THROUGH THE HUB. applyEnvOverrides runs AFTER settings.yaml is
loaded (pkg/config/hub_config.go:683) and wins, so a bound SCION_SERVER_DATABASE_
variable silently overrides the file this chart renders. Measured: minimal's
settings.yaml says driver: sqlite; with SCION_SERVER_DATABASE_DRIVER=postgres in
the environment, config.LoadGlobalConfig reports driver "postgres",
isHADeployment (cmd/server_foreground.go:927) flips to TRUE, and the hub aborts
at the hosted HA preflight - from a release that assertHAUnlanded passed, because
assertHAUnlanded reads .Values.database.driver, which still says sqlite.

That is the harm and it is specific to this chart: the chart's guards reason
about the configuration the chart RENDERED, and this variable changes the
configuration the hub RUNS. Every premise those guards rest on stops being true,
and nothing anywhere reports it.

The chart's own templates emit no such variable; hub.extraEnv is the one place an
operator could add one, and these two prefixes are the most likely mistake
because they are the settings a chart most wants to deliver.
*/}}
{{- define "scion-hub.assertExtraEnv" -}}
{{- /*
THE SHADOW LIST IS READ OUT OF THE RENDERED ConfigMap, NOT WRITTEN DOWN HERE.
It was written down here, and it was already wrong: it named four variables while
configmap-env.yaml emitted six, so SCION_SERVER_ADMIN_MODE and
SCION_SERVER_MAINTENANCE_MESSAGE could be shadowed silently - the two that are
emitted conditionally, which is to say the two a hand-maintained list was always
going to miss.

Rendering the ConfigMap and taking its keys makes the two lists the same list.
Adding a variable to the ConfigMap without adding it to this guard stops being
expressible, which is the only version of "keep these in sync" that is a
mechanism rather than a request. It also gets the conditional keys exactly
right: when hub.adminMode is unset the chart emits nothing to shadow, and an
extraEnv entry of that name is legitimately allowed.

POD_NAMESPACE is the one literal, and it has to be. It is not in the ConfigMap -
it is a fieldRef in the container's env list, which cannot be rendered from here
without the Deployment rendering itself. hack/verify.sh closes that by reading
the shadowable names back out of the rendered manifest, ConfigMap keys and
container env entries alike, and asserting this guard refuses every one of them.
*/}}
{{- $envDoc := fromYaml (include (print .Template.BasePath "/configmap-env.yaml") .) }}
{{- $shadowable := concat (keys (default dict $envDoc.data)) (list "POD_NAMESPACE") }}
{{- if lt (len $shadowable) 5 }}
{{- fail (printf "the environment ConfigMap rendered %d keys, which is fewer than the chart is known to emit unconditionally - hub.extraEnv's shadow guard derives its list from those keys and would be checking almost nothing." (len (default dict $envDoc.data))) }}
{{- end }}
{{- range $entry := .Values.hub.extraEnv }}
{{- $name := toString (dig "name" "" $entry) }}
{{- if regexMatch "^SCION_SERVER_(DATABASE|OIDC)_" $name }}
{{- fail (printf "hub.extraEnv may not set %s. Some of these names bind and some are discarded, and both outcomes are wrong here. If it binds - SCION_SERVER_DATABASE_DRIVER and SCION_SERVER_DATABASE_URL both do - applyEnvOverrides applies it AFTER settings.yaml is loaded (pkg/config/hub_config.go:683) and it wins, so the hub runs a configuration this chart did not render and this chart's guards did not see: set the driver to postgres this way and isHADeployment (cmd/server_foreground.go:927) becomes true while acknowledgeHAUnlanded never fires, and the hub aborts at the hosted HA preflight. If it is discarded - anything whose koanf tag contains an underscore, such as SCION_SERVER_DATABASE_MAX_OPEN_CONNS - k.Unmarshal drops it with no error, and DetectEnvOverrides (pkg/config/opsettings/koanf.go:347) still lists it to the admin server-config view as an active override, so it is reported as applied. Configure the database through the rendered settings.yaml at server.database instead." $name) }}
{{- end }}
{{- if has $name $shadowable }}
{{- fail (printf "hub.extraEnv may not set %s: the chart sets it, and hub.extraEnv is appended to the container's env list, which wins twice over - a container env entry takes precedence over the same name from envFrom, and a later entry in the list takes precedence over an earlier one. Either way the chart's value is replaced with no error and nothing in the manifest that reads as a conflict." $name) }}
{{- end }}
{{- /*
The same two rules the argument guard applies to argv, applied to env, through
the same two shared helpers rather than a second near-miss copy of them. A
literal value here is stored in the Deployment and readable by anyone who can
read the object - which is a wider set of people than can read a Secret, and a
set that grows every time somebody is granted "just read access to the
workloads". valueFrom.secretKeyRef is untouched by both checks: it carries a
reference, not the material, so neither a name nor a value test has anything to
say about it.

Both are conditioned on the entry actually carrying a literal, for that reason.

THE UNDERSCORE IS WHY $name IS TRANSLATED BEFORE THE NAME CHECK.
scion-hub.assertNoCredentialName matches a credential noun as a whole trailing
SEGMENT, and its segment separator is the hyphen, because it was written for
flag names. Environment variable names separate with underscores, so passing
SESSION_SECRET to it unchanged matches nothing at all: the guard renders, reads
as applied, and is inert. Translating _ to - puts the name into the alphabet the
helper's positional rule is expressed in, and the rule then means the same thing
on both axes - SESSION_SECRET is caught, TOKEN_TTL_SECONDS and MAX_TOKENS are
not. verify.sh asserts the catch and both non-catches, so a regression here
cannot pass as "no false positives".

Do not "simplify" this by widening the shared helper's character class instead.
The translation belongs to the caller whose names use underscores; the helper is
shared with argv, where the hyphen rule is the correct one.
*/}}
{{- if hasKey $entry "value" }}
{{- include "scion-hub.assertNoCredentialName" (dict "name" (replace "_" "-" $name) "source" (printf "hub.extraEnv entry %s: the name" $name)) }}
{{- include "scion-hub.assertNoCredential" (dict "value" (dig "value" "" $entry) "source" (printf "hub.extraEnv value of %s" $name)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
THE VALUES THAT REACH settings.yaml AND ONLY settings.yaml, AND WHAT THEY WRITE.
The single source for that list. NOTES.txt renders it, values.yaml repeats it in
prose at config.existingSecret, and hack/verify.sh checks all three against the
render rather than against each other.

Under config.existingSecret the chart writes no settings.yaml, so each of these
values silently does nothing and the operator's own file has to carry the key on
the right. That is the whole content of the list: what you now owe.

WHY THESE ARE DOCUMENTED AND THE THREE BELOW ARE REFUSED, WHICH IS NOT A
JUDGEMENT ABOUT WHICH MATTER MORE. It is about what a template can see. Helm
hands a template the MERGED values and no way to ask which of them the operator
actually wrote, so intent is only legible where the chart's default is empty:
config.extra, storage.bucket and agents.imageRegistry are empty by default, so a
non-empty one was typed by someone and can be refused. Every value here has a
non-empty default - auth.mode is "proxy", hub.name is "Scion Hub",
database.maxOpenConns is 25 - and a guard on truthiness would fire on a values
file that never mentioned them. There is no third option: a literal copy of the
default inside the guard is a second source of truth for the default, and it
goes stale in exactly the direction that turns the guard off.

So this list is not a weaker refusal. It is the part of the same problem that a
refusal cannot express, and it is checked to the same standard: hack/verify.sh
mutates every leaf of values.yaml, renders, and requires each value whose only
effect is on the settings document to be EITHER refused by the guard below OR
named here with the settings key its mutation actually moved. A value in neither
place fails the suite, and so does an entry here that no longer moves the key it
claims. The pairs below were produced by that probe, not written from memory.

Both columns are load-bearing. The left tells an operator which of their values
went nowhere; the right tells them what to write instead, which is the only half
they can act on, and it is not guessable from the left - hub.name becomes
server.hub.hub_name, and rbac.agentNamespace and runtime.namespace both become
the same runtimes.kubernetes.namespace.
*/}}
{{- define "scion-hub.existingSecretTransfers" -}}
  auth.mode                  ->  server.auth.mode
  database.connMaxIdleTime   ->  server.database.conn_max_idle_time
  database.connMaxLifetime   ->  server.database.conn_max_lifetime
  database.maxIdleConns      ->  server.database.max_idle_conns
  database.maxOpenConns      ->  server.database.max_open_conns
  hub.hubId                  ->  server.hub.hub_id
  hub.name                   ->  server.hub.hub_name
  rbac.agentNamespace        ->  runtimes.kubernetes.namespace
  runtime.listAllNamespaces  ->  runtimes.kubernetes.list_all_namespaces
  runtime.namespace          ->  runtimes.kubernetes.namespace
{{- end }}

{{/*
The other half of the same partition: the settings values that are REFUSED
alongside config.existingSecret rather than transferred. One define, because
NOTES.txt prints this list to operators and scion-hub.assertConfigSource
enforces it, and a second hand-written copy is how the two stop agreeing.

hack/verify.sh checks these names against the leaves its values walk observed
being refused BY NAME - not merely refused, which is the vacuity gd-p2-rev found
as R4. Order is the order the guard appends them in, so the refusal message and
this list read the same way.
*/}}
{{- define "scion-hub.existingSecretRefusals" -}}
config.extra, storage.bucket, agents.imageRegistry, database.name, database.user, database.password, auth.oauth.web.github.clientId, auth.oauth.web.github.clientSecret, auth.oauth.web.google.clientId, auth.oauth.web.google.clientSecret
{{- end }}

{{/*
config.existingSecret means "I supply the whole settings.yaml myself", so the
chart renders none - and every value whose only effect is on the file it did not
render becomes inert. An inert value is the same silent no-op this whole design
exists to avoid, so supplying both is an error rather than a precedence rule.

The names below are the settings values with an empty default, which is what
makes them refusable at all; the reasoning is in the comment above the transfer
list, and the two lists are checked together as one partition of the values
tree. Later phases append their own inline values here as they are introduced
(the database password, the session secret).

THE OAUTH WEB CLIENT CREDENTIALS ARE THE FIRST OF THOSE APPENDED, and they were
appended here rather than added to the transfer list because they qualify: both
halves default to empty, so a non-empty one was typed. That makes the stronger
answer available, and where it is available it is the one to take - the transfer
list tells an operator afterwards that their credential went nowhere, whereas
this refuses the release. A credential silently discarded is the shape of
failure this chart is least able to make visible later: the hub starts, passes
its probes, and refuses every login.

THE SAME NAMES ARE PRINTED TO OPERATORS, so they come from one define rather
than from a second hand-written copy in NOTES.txt. hack/verify.sh checks that
define against the leaves its values walk observed being refused BY NAME, so a
leaf added here and not there - or there and not here - goes red.

Two settings values are missing from the list on purpose and are covered anyway.
storage.provider and database.driver have non-empty defaults, so neither can be
refused on truthiness - but the only other value each can take (gcs, postgres)
is one the chart already requires storage.bucket alongside, so any render that
moves either of them is refused for the bucket. hack/verify.sh proves that by
mutation rather than by argument; if a later phase makes either reachable
without a bucket, that check goes red rather than the pair going quiet.

MUST be called from a template that always renders. Calling it only from
scion-hub.settings does not work, and does not look broken: the settings
template is reached through secret-settings.yaml, which is itself skipped when
config.existingSecret is set - so the one configuration this check exists to
reject is the one configuration in which it never runs. deployment.yaml calls
it; keep that call.
*/}}
{{- define "scion-hub.assertConfigSource" -}}
{{- if .Values.config.existingSecret }}
{{- $inline := list }}
{{- if .Values.config.extra }}{{- $inline = append $inline "config.extra" }}{{- end }}
{{- if .Values.storage.bucket }}{{- $inline = append $inline "storage.bucket" }}{{- end }}
{{- if .Values.agents.imageRegistry }}{{- $inline = append $inline "agents.imageRegistry" }}{{- end }}
{{- /*
PHASE 2 DELTA. The three database leaves below are the append this comment asked
later phases for, and phase 2 owed it: phase 2 is what introduced the database
credential. gd-p2-rev found the omission as R3 and measured it - existingSecret
plus database.password rendered BYTE-IDENTICAL manifests for two different
passwords, zero occurrences of either in the output and no warning anywhere. An
operator got a green upgrade and a hub authenticating with whatever their own
Secret happened to hold.

THREE, NOT THE FOUR THE REVIEW ASKED FOR, and the fourth is the interesting one.
database.auth is NOT refused here because it is not inert: it selects the
proxy's --auto-iam-authn flag, and the proxy is rendered under
config.existingSecret like every other container. Measured, existingSecret +
cloudsql + postgres, iam against password:

  212d211
  <             - "--auto-iam-authn"

Refusing it would break a coherent install - "I supply my own settings.yaml AND
I use IAM authentication" - by forbidding the only value that tells the proxy
so. It is half-inert instead: the proxy half lands, the DSN half is the
operator's own file to write, and NOTES.txt says that rather than this pretending
to refuse it.

The three below all have an empty default, which is what makes them refusable on
truthiness. database.driver does not, and is covered by the storage.bucket
companion instead; that pair is explained above.
*/}}
{{- if .Values.database.name }}{{- $inline = append $inline "database.name" }}{{- end }}
{{- if .Values.database.user }}{{- $inline = append $inline "database.user" }}{{- end }}
{{- if .Values.database.password }}{{- $inline = append $inline "database.password" }}{{- end }}
{{- $auth := .Values.auth | default (dict) }}
{{- $authOauth := $auth.oauth | default (dict) }}
{{- $web := $authOauth.web | default (dict) }}
{{- range $provider := list "github" "google" }}
{{- $creds := index $web $provider | default (dict) }}
{{- if $creds.clientId }}{{- $inline = append $inline (printf "auth.oauth.web.%s.clientId" $provider) }}{{- end }}
{{- if $creds.clientSecret }}{{- $inline = append $inline (printf "auth.oauth.web.%s.clientSecret" $provider) }}{{- end }}
{{- end }}
{{- if $inline }}
{{- fail (printf "config.existingSecret is set together with inline settings values (%s). With config.existingSecret the chart renders no settings.yaml, so those values would be silently discarded. Set one or the other: either supply the whole file yourself, or let the chart render it. Note that these are only the settings values the chart can PROVE you set, because their default is empty. Others - auth.mode, hub.name, the database pool sizes, the hub ID and the agent namespace - are just as inert here and cannot be refused, because a default-valued setting is indistinguishable from an unset one; they are listed with the settings keys your own file must carry in NOTES.txt and in values.yaml at config.existingSecret." (join ", " $inline)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The HA acknowledgement gate.

WHAT IT IS FOR. This chart can render a configuration that satisfies the hub's
isHADeployment test (cmd/server_foreground.go, func isHADeployment), which turns
on validateHostedHAPreflight. This release cannot satisfy the gates listed below,
so the hub aborts where runServerForeground calls
`if err := validateHostedHAPreflight(cfg); err != nil` before it serves anything.

[HISTORY 2026-08-17] THIS PARAGRAPH SAID "that preflight has thirteen gates",
eleven lines above the sentence below claiming there is no count anywhere in this
file. Both were written the same day. 1b3c9418 then made the count wrong as well
as forbidden, and the sentence that forbade it did not notice, because a prose
claim about a file's contents is not a check on that file's contents. The count
is gone; the prohibition below now has nothing to contradict it. The postgres/gcs
shape is a choice an operator can coherently have made, so this is an opt-in
acknowledgement rather than a refusal.

MEASURED, gate by gate, through config.LoadGlobalConfig and the real
validateHostedHAPreflight, on the settings.yaml this chart actually renders -
not read off this table. cmd/helm_chart_ha_contract_test.go walks it and writes
hack/ha-gates.txt; hack/verify.sh checks this table against that walk in both
directions. If the hub gains or loses a gate, that check goes red and this table
is what has to move. THERE IS NO COUNT ANYWHERE IN THIS FILE, deliberately: a
count is the thing that agreed with itself in three places for a day while
agreeing with the hub in none.

ci/values-settings.yaml, in hub order:

  GATE TABLE BEGIN
    a durable session/signing secret           session-secret phase
    server.auth.proxy.provider=iap             ingress/IAP phase
    server.auth.proxy.iap.audience             ingress/IAP phase
    server.auth.transport                      ingress/IAP phase
    server.auth.transport.mode=iap             ingress/IAP phase
    server.auth.transport.oidc_audience        ingress/IAP phase
    server.auth.transport.platform_auth_sa     ingress/IAP phase
  GATE TABLE END

ci/values-settings-oauth.yaml refuses on a STRICT SUBSET of that table: the first
two rows only. The hub's IAP gates sit inside `if cfg.Auth.Mode == "proxy"` in
validateHostedHAPreflight, so an oauth deployment never reaches them and the
ingress/IAP phase lands nothing that arm is waiting on.

[HISTORY 2026-08-17] THIS SAID THE OPPOSITE - "refuses on one gate more,
server.auth.mode=proxy, which is not an unlanded phase" - and it was true when
written. 1b3c9418 "fix: do not require IAP for hosted HA preflight" deleted that
gate and moved the IAP family inside the auth.mode test, inverting the direction:
oauth went from superset-by-one to subset-by-six. The same claim was live in three
places (here, templates/NOTES.txt, values.yaml at auth.mode). Correcting one of
them by hand is what left the other two standing with MORE authority, not less,
so the fix that matters is the exclusivity assertion in hack/verify.sh, not this
paragraph.

THE FIVE IN CIRCULATION WERE A PROBE'S EXTENT, NOT THE HUB'S. That walk stopped
at server.auth.transport because the prober could not satisfy it, and its stop
was read as the preflight's end. Gates lie past that wall and they are real.
A later walk supplying a WELL-FORMED IAP audience missed a further refusal,
isSupportedIAPAudience, which is a second objection to the value of
server.auth.proxy.iap.audience rather than a table row. Both mistakes are the
same mistake: reporting what the probe reached as what the hub does.

WHAT THIS CHART ALREADY SATISFIES, so nobody re-derives it: server.hub.hub_id,
server.database.driver=postgres, server.storage.provider=gcs with a bucket, and
- since the Cloud SQL phase - server.database.url. Those four are why the refusal
starts where it does rather than at the hub's first gate. The URL is satisfied
only where the chart renders a settings.yaml, so under config.existingSecret it
is the operator's again, and the table above is the list for the rendering case.

server.database.url LEFT THE TABLE ABOVE BECAUSE THE WALK STOPPED NAMING IT, not
because this phase decided it had landed. TestHelmChartHAGateWalk's authored
tripwire went red with "gates the authored list names and the hub no longer
refuses on: [server.database.url]" and this edit is the response to that line.

THE ROUTE SET IS TRANSCRIBED FROM THE HUB, NOT INVENTED HERE.
cmd/server_ha_preflight_test.go:248-256 (ab0d227, branch
scion/ha-deployment-tripwire - not an ancestor of this branch; fetch that ref to
read it) WILL make this a two-way contract once it lands: a route added there
and not here makes this condition UNDER-trigger, rendering an HA config with no
acknowledgement, which cannot boot; a route removed or swapped there and not
here makes it OVER-trigger, demanding an acknowledgement for a deployment that
is not HA. All three routes are transcribed below. Grep this tree for
acknowledgeHAUnlanded to find it, as that comment instructs.

Route 1 is K_SERVICE, and it is NOT dead here. GKE does not set it, but
hub.extraEnv does - measured, it renders - so an operator can turn HA detection
on through a channel that looks unrelated to the database. The hub tests
os.Getenv("K_SERVICE") != "", so an explicit empty value is not the route; a
valueFrom is counted, because the chart cannot read it and under-triggering is
the outcome that will not boot.

Routes 2 and 3 use lower-cased comparison because the hub uses strings.EqualFold
(:931, :934), while auth.mode is compared exactly (:934) and so is compared
exactly here. The schema already enums all three to lower case, which makes the
difference unobservable today - transcribed faithfully anyway, because the
contract above is with the hub's test and not with the schema.

NOT EVALUATED UNDER config.existingSecret, and that is a real hole rather than
an oversight: the chart renders no settings.yaml in that shape, so it cannot see
the driver, the storage provider or the auth mode. Route 1 is still checked,
because extraEnv is the chart's own value either way.
*/}}
{{/*
THE ROUTE SET, once, so the refusal and NOTES.txt cannot disagree about whether
this release is on one. Emits the routes joined by " and ", or the empty string
when there are none - which is falsey, so callers test it directly.

Shared deliberately, and it is NOT the same decision as the gate list. The
routes are a computed property of these values and must be identical in both
places or one of them is lying about the deployment in front of the operator.
The gate list is prose written for two different audiences and stays duplicated,
with a parity check over the copies rather than a shared definition.
*/}}
{{- define "scion-hub.haRoutes" -}}
{{- $routes := list }}
{{- range .Values.hub.extraEnv }}
{{- if eq .name "K_SERVICE" }}
{{- if or .value .valueFrom }}
{{- $routes = append $routes "hub.extraEnv sets K_SERVICE (cmd/server_foreground.go, isHADeployment: os.Getenv(\"K_SERVICE\") != \"\")" }}
{{- end }}
{{- end }}
{{- end }}
{{- if not .Values.config.existingSecret }}
{{- if eq (lower (toString .Values.database.driver)) "postgres" }}
{{- $routes = append $routes "database.driver is postgres (cmd/server_foreground.go, isHADeployment: strings.EqualFold(cfg.Database.Driver, \"postgres\"))" }}
{{- end }}
{{- $auth := .Values.auth | default (dict) }}
{{- if and (eq (lower (toString .Values.storage.provider)) "gcs") (eq (toString $auth.mode) "proxy") }}
{{- $routes = append $routes "storage.provider is gcs and auth.mode is proxy (cmd/server_foreground.go, isHADeployment: strings.EqualFold(cfg.Storage.Provider, \"gcs\") && cfg.Auth.Mode == \"proxy\")" }}
{{- end }}
{{- end }}
{{- join " and " $routes }}
{{- end }}

{{/*
THE GATE ENUMERATION IS PER-AUTH-MODE, AND IT IS THAT WAY BECAUSE THE HUB IS.

Until 1b3c9418 ("fix: do not require IAP for hosted HA preflight", Preston
Holmes, 2026-08-17) this list was the same in both modes and the chart printed
one sentence. That commit deleted the gate "hosted HA deployment requires
server.auth.mode=proxy for IAP authentication" and moved the seven IAP gates
inside `if cfg.Auth.Mode == "proxy" {` in validateHostedHAPreflight. So under
auth.mode oauth the hub now asks for NONE of the IAP configuration, and the
single sentence became false about seven of the nine gates it named - while
still refusing correctly, which is the dangerous combination: a refusal that
is right about the outcome and wrong about the reason teaches the operator to
go fix seven things that were never going to be checked.

BOTH LISTS ARE DERIVED, NOT WRITTEN. hack/ha-gates.txt is produced by
cmd/helm_chart_ha_contract_test.go driving the real validateHostedHAPreflight
over this chart's own goldens, one gate at a time; the proxy arm walks eight
and the oauth arm walks two. tests/render-guards.sh reads that artifact and
asserts, per arm, that the message below names every gate in it AND NAMES NO
OTHERS. The upper half of that assertion is new: the previous guard only
checked that nothing was missing, so this message could name a gate the hub
had deleted and stay green - which is exactly what it did for the forty
minutes between 1b3c9418 landing and being noticed.
*/}}
{{- define "scion-hub.assertHAUnlanded" -}}
{{- $routes := include "scion-hub.haRoutes" . }}
{{- if and $routes (not .Values.acknowledgeHAUnlanded) }}
{{- $gates := "a durable session/signing secret, from the session-secret phase. That is the whole list for auth.mode oauth: the hub's IAP gates sit inside `if cfg.Auth.Mode == \"proxy\"` in validateHostedHAPreflight, so this shape never reaches them and the ingress/IAP phase lands nothing this release is waiting on" }}
{{- $removal := "That flag stops being needed for auth.mode oauth when the session-secret phase has landed. The ingress/IAP phase is not on this shape's path and waiting for it would hold the flag one phase too long; Filestore lands none of them either" }}
{{- $auth := .Values.auth | default (dict) }}
{{- if eq (toString $auth.mode) "proxy" }}
{{- $gates = "a durable session/signing secret, from the session-secret phase; then server.auth.proxy.provider=iap, server.auth.proxy.iap.audience, server.auth.transport, server.auth.transport.mode=iap, server.auth.transport.oidc_audience and server.auth.transport.platform_auth_sa, all from the ingress/IAP phase" }}
{{- $removal = "That flag stops being needed for auth.mode proxy when the session-secret phase and the ingress/IAP phase have both landed. Filestore lands none of them" }}
{{- end }}
{{- fail (printf "This release cannot start the deployment these values describe. %s, so the hub's isHADeployment test is true, its hosted HA preflight runs (cmd/server_foreground.go, func validateHostedHAPreflight), and it aborts before serving where runServerForeground calls `if err := validateHostedHAPreflight(cfg); err != nil`. These preflight gates have no source in this chart, measured in hub order by walking the real preflight: %s. The chart already satisfies server.hub.hub_id, the postgres driver, gcs storage with a bucket, and now the database URL, which is why the refusal starts at the session secret and not earlier. If you are rendering this to inspect it, or to supply the rest yourself, set acknowledgeHAUnlanded: true. %s." $routes $gates $removal) }}
{{- end }}
{{- end }}

{{/*
A value, as it should appear inside a diagnostic. Quoted, except when there is
nothing to quote, in which case the word null - which is what YAML calls it.

printf %q against a nil renders the literal text %!q(<nil>), and every one of the
assertions below reaches nil by an ordinary route: a key present with no value
("mode:" and nothing after it) parses to nil, and dig's default only covers the
key being ABSENT. So the operator who writes the near-miss gets a message with a
Go format error in the middle of it, at the exact moment they are trying to work
out what the chart read. Missing and null then also print identically as "",
which hides the difference between a key they did not write and a key they wrote
wrong.

toString alone is not the fix: it turns nil into the string "<nil>", and %q then
quotes it into "<nil>", which reads as a value the operator supplied.
*/}}
{{- define "scion-hub.diagValue" -}}
{{- if kindIs "invalid" . }}null{{ else }}{{ printf "%q" (toString .) }}{{ end }}
{{- end }}

{{/*
Assertions on the settings document AS EMITTED.

Deliberately run against the bytes parsed back from the rendered text rather
than against the dictionary they were built from, and after config.extra has
been merged. config.extra is a deep merge over the whole tree, so without this
an operator could set server.mode: workstation through it and defeat the rule
that no value can disable hosted mode - the same hole for every invariant below.

Every check here has a positive form. "server.mode is not workstation" would
pass on a typo'd or absent mode; "server.mode equals hosted" does not. The hub
ID check asserts the emitted value equals the operator's, which "no Helm
generator appears in the hub-ID position" does not.
*/}}
{{- define "scion-hub.assertSettings" -}}
{{- $root := .root }}
{{- $doc := fromYaml .rendered }}
{{- if hasKey $doc "Error" }}
{{- fail (printf "the rendered settings.yaml is not valid YAML: %v" (get $doc "Error")) }}
{{- end }}

{{- /*
schema_version. This is a safety property, not a formality, and it is the one
check in this file most likely to be deleted by someone tidying up.

The hub does not migrate settings at startup. MigrateSettingsFile has exactly
two callers: the "scion config migrate" CLI, and a lazy auto-migration inside
SetSettingValue that fires ONLY when the file has no schema_version key. So a
file that declares schema_version never migrates, and a file that omits it can
migrate at an arbitrary moment during normal operation.

That matters because settings.yaml is delivered as a subPath bind mount, and
MigrateSettingsFile replaces the file with os.Rename. Renaming over a bind mount
returns EBUSY. Every other write path to this file is soft - a warning, or a 500
to one caller, with the server continuing - so omitting schema_version is the
one way to turn a mount that merely refuses writes into a hard failure.

Present is not enough; it has to be the value that stops migration, as a string.
*/}}
{{- if ne (dig "schema_version" "" $doc) "1" }}
{{- fail (printf "rendered settings.yaml must carry schema_version: \"1\" as a string, got %s. Without it the hub's lazy auto-migration can fire during operation, and it replaces the file with os.Rename, which returns EBUSY against the subPath mount this file is delivered through." (include "scion-hub.diagValue" (dig "schema_version" "" $doc))) }}
{{- end }}
{{- if not (dig "active_profile" "" $doc) }}
{{- fail "rendered settings.yaml must set a non-empty top-level active_profile" }}
{{- end }}
{{- if not (dig "profiles" "" $doc) }}
{{- fail "rendered settings.yaml must carry a non-empty top-level profiles map" }}
{{- end }}
{{- if not (dig "runtimes" "" $doc) }}
{{- fail "rendered settings.yaml must carry a non-empty top-level runtimes map" }}
{{- end }}

{{- /*
The top-level server: key, and it carries a load-bearing property that nothing
else in this file would suggest.

It is not only that the hub's configuration lives under it. It is that EMITTING
THIS KEY IS WHAT MAKES --config INERT. Not "--config is inert" - it is not, as a
general fact, and writing it that way is how the property gets dropped. At Phase
0, which rendered no settings Secret, the flag was fully live.

AND NOT BECAUSE NO FILE EXISTED. A global settings.yaml may well exist without
this chart writing one: the hub seeds it from its own embedded defaults on a
first boot (cmd/server_foreground.go:104-109 -> config.InitMachine,
pkg/config/init.go:588-599), and those defaults carry no server key.
loadServerFromSettingsFile does not test existence, it tests the key
(:1344-1347).

THE TRIGGER IS THE KEY, NOT THE FILE, which is the whole reason this assertion
is worth its lines. "The chart mounts a settings.yaml" does not keep --config
inert. Nest the server section under a profile, rename it, deliver it in a
second file - every one of those still mounts a settings.yaml, and every one of
them hands the flag back its effect. This phase supplies the key; drop it and the
deployment is back where Phase 0 was.

  LoadGlobalConfig            pkg/config/hub_config.go:628
  loadGlobalConfigFromSettings                        :640
    reads GetGlobalDir() FIRST and UNCONDITIONALLY, and consults the --config
    path only `if !found`                             :647-660
  loadServerFromSettingsFile                          :1331
    found = the file parses AND raw["server"] exists AND is non-nil
                                                      :1344-1347

WHAT THE FLAG DOES WHEN IT IS LIVE, WHICH IS NOT A REDIRECT. It cannot be one:
GetGlobalDir (pkg/config/paths.go:188-194) is os.UserHomeDir() joined with
GlobalDir and TAKES NO ARGUMENTS, so no flag value can move the directory the
hub reads first. Dropping this key opens two narrower routes instead:

  :648-659  the --config path's own directory is searched for a settings.yaml,
            and if that file has a server key it becomes the SOLE source of the
            server config - a substitution of that section, nothing merged
  :635      failing that, loadGlobalConfigLegacy(configPath) (:699), which loads
            defaults, then ~/.scion/server.yaml (:772-775), then LAYERS the
            --config path over the result (:777-787) - an overlay

Both are real and neither is "the whole configuration load moves". Keep the
distinction: a mitigation scoped to preventing redirection does not cover an
overlay. A settings.yaml of only profiles and runtimes - a plausible
minimisation, and one that would look like a simplification - is what opens
them.

IN THE INERT STATE IT IS A NO-OP WITH NO SIGNAL, WHICH IS WHY THIS ASSERTION IS
THE ONLY WARNING THERE WILL EVER BE. --config is not marked deprecated -
MarkDeprecated appears twice in cmd/server.go, :236 and :290, both for
--production; the flag itself is a plain StringVarP at :237 - and the two
warnings in the load path (:668, :678) are about a server.yaml beside
settings.yaml, the second of them additionally requiring hasServerYAML(dir)
(:1393), which this chart creates nowhere. So while this key is emitted the flag
is accepted and ignored in silence, and without it it takes effect in the same
silence. The author of the refactor that flips it gets no runtime symptom to
discover. They get this message, at render time, or they get nothing.

Non-nil is asserted, not merely present, because that is the condition the
binary tests. `server:` with nothing under it satisfies hasKey and fails
raw["server"] != nil.

hack/verify.sh asserts the same property from the rendered output, in every
permutation. Two checks, on purpose: this one is the one config.extra cannot get
past, that one is the one a template change cannot get past.

The six keys below are nested under server: in V1ServerConfig. A file that
places any of them at the top level parses, installs, and is silently not read.
*/}}
{{- if not (hasKey $doc "server") }}
{{- fail "rendered settings.yaml has no top-level server: section. Two consequences. (1) Every server setting in this file is lost: the hub reads the server section and nothing else from it (pkg/config/hub_config.go:1344-1347). (2) --config goes back to being live, and it is Phase 0's reserved flag. That flag is not inert by nature - at Phase 0 it was fully live, and not because no settings.yaml existed: the hub seeds one from embedded defaults that carry no server key (cmd/server_foreground.go:104-109, pkg/config/init.go:588-599), and the loader tests the key, not the file (:1344-1347). Emitting this key is what makes the global settings read succeed (:647) and the --config path go unread; drop it and loadGlobalConfigFromSettings consults that path instead (:648-659), where its own settings.yaml becomes the sole source of the server config, and failing that loadGlobalConfigLegacy layers the --config file over the loaded configuration (:777-787). Neither state announces itself: --config is silently accepted and ignored while this key is here - no error, no warning, no log line, and it is not marked deprecated (cmd/server.go:237 defines it; the MarkDeprecated calls at :236 and :290 are both for --production) - so this render-time failure is the only signal a settings-shape refactor will ever get." }}
{{- end }}
{{- if not (kindIs "map" (get $doc "server")) }}
{{- fail (printf "rendered settings.yaml has a top-level server: key that is not a map (%v). The hub tests raw[\"server\"] != nil (pkg/config/hub_config.go:1344-1347), so an empty or nulled server section reads as no settings file at all: every server setting is lost, and --config - reserved by Phase 0, live there, and silently accepted and ignored only while this chart emits this key as a map - returns to live as a sole-source substitution at :648-659 or as an overlay at :777-787. Same consequence as omitting the key entirely; see the comment above this check." (get $doc "server")) }}
{{- end }}
{{- range $key := list "notification_channels" "message_broker" "native_chat" "plugins" "scheduler" "github_app" }}
{{- if hasKey $doc $key }}
{{- fail (printf "rendered settings.yaml has %s at the top level. It belongs under server: - the top-level position parses and is silently ignored. If this came from config.extra, move it to config.extra.server.%s." $key $key) }}
{{- end }}
{{- end }}

{{- /* Hosted mode. Not a tuning knob: without it the server applies workstation
defaults, takes auth-enabled from a development flag and binds 127.0.0.1. */}}
{{- if ne (dig "server" "mode" "" $doc) "hosted" }}
{{- fail (printf "rendered settings.yaml must set server.mode: hosted, got %s. Hosted mode cannot be disabled through this chart, config.extra included." (include "scion-hub.diagValue" (dig "server" "mode" "" $doc))) }}
{{- end }}

{{- /* HA preflight block 1, part 1: an explicit, operator-supplied hub ID. */}}
{{- $emittedHubId := dig "server" "hub" "hub_id" "" $doc }}
{{- if ne $emittedHubId .hubId }}
{{- fail (printf "rendered settings.yaml has server.hub.hub_id: %s, which is not the value supplied in hub.hubId (%s). The hub ID is emitted verbatim and nothing, config.extra included, may substitute it." (include "scion-hub.diagValue" $emittedHubId) (include "scion-hub.diagValue" .hubId)) }}
{{- end }}

{{- /* HA preflight block 1, part 2: the store. */}}
{{- $emittedDriver := dig "server" "database" "driver" "" $doc }}
{{- if ne $emittedDriver $root.Values.database.driver }}
{{- fail (printf "rendered settings.yaml has server.database.driver: %s but database.driver is %s. Overriding the driver through config.extra bypasses the schema rules that depend on it, including the requirement for a GCS bucket under Postgres." (include "scion-hub.diagValue" $emittedDriver) (include "scion-hub.diagValue" $root.Values.database.driver)) }}
{{- end }}

{{- /* HA preflight block 1, part 3: hub blob storage. GCS, and not the
Filestore share - workspace storage is a different subsystem under
server.workspace_storage and does not satisfy this. */}}
{{- if eq (lower (toString $emittedDriver)) "postgres" }}
{{- if ne (lower (toString (dig "server" "storage" "provider" "" $doc))) "gcs" }}
{{- fail (printf "rendered settings.yaml must set server.storage.provider: gcs under Postgres, got %s. Local blob storage is not HA-safe and the hub refuses to start. This is the hub's own blob store; the Filestore workspace share does not satisfy it." (include "scion-hub.diagValue" (dig "server" "storage" "provider" "" $doc))) }}
{{- end }}
{{- if not (dig "server" "storage" "bucket" "" $doc) }}
{{- fail "rendered settings.yaml must set a non-empty server.storage.bucket under Postgres" }}
{{- end }}
{{- end }}

{{- /*
server.hub.public_url is refused outright. This is the only assertion in this
file that guards a channel the chart itself owns, and that is exactly why it was
missing until someone went looking.

The base URL has two consumers and they do not read the same source. Read from
the hub rather than assumed, and every step verified independently by review:

  settings_v1.go:517            PublicURL carries koanf:"public_url"
  settings_v1.go:1404-1405      if v1.Hub.PublicURL != "" { gc.Hub.Endpoint = it }
  server_foreground.go:1311-12  resolveHubEndpoint returns cfg.Hub.Endpoint - and
                                this is its FIRST statement, ahead of --base-url
                                at :1323 and SCION_SERVER_BASE_URL at :1331
  server_foreground.go:2102-08  initWebServer never reads cfg.Hub.Endpoint

So public_url outranks both other channels for the agent endpoint, and the OAuth
side cannot see it at any precedence. SCION_SERVER_BASE_URL is the only source
both consumers honour, which is why the chart sets that and nothing else - it
makes the two agree by construction rather than by the operator keeping them in
step.

REFUSED OUTRIGHT, not merely when it disagrees with hub.baseUrl. Permitting an
equal value would create a second source of truth that has to be kept in sync,
and the failure of that sync IS the bug: set both equal today, change hub.baseUrl
tomorrow, and you get the split - from a values file that passed the guard on the
day it was written. The permissive rule guards the moment of authorship, not the
lifetime of the file, and there is nothing on the other side of the trade. A
public_url equal to baseUrl buys the operator nothing, so the only configurations
it would admit are the ones that are useless now and dangerous later.

Rendering public_url therefore does not override the base URL. It SPLITS it: the
agent endpoint moves and the OAuth redirect does not, in one process, with both
values looking correct from their own side and no line in any manifest that
looks wrong. The failure surfaces later as redirects to the wrong host.

Reachable today, not hypothetically: config.extra is deep-merged over this tree
before these assertions run, so config.extra.server.hub.public_url renders. That
is the path hack/verify.sh proves this assertion against. The argv channel is
closed by the reserved-flag list and the environment channel by the schema; this
is the third channel and the chart is the only thing that writes it.

public_url is an ALIAS: a settings key that is a second name for a value the
chart already sets through a different channel. That is the hazard, and it is
not the server.hub prefix - nothing about that prefix is dangerous, and an alias
need not live anywhere near the value it renames. The collision check below
cannot see this one either, because the chart never writes public_url; the two
rules cover disjoint halves.

So this is a denylist of one, and Phase 5a owns replacing it with the alias
enumeration: every settings key that is a second name for something the chart
sets elsewhere, derived by walking what the chart sets and asking what else
names each quantity. Small, closed, checkable from the chart side alone, and it
narrows config.extra not at all. Phase 5a because it is the phase that wants a
public hostname and will reach for this key first.

Do not add a second key here instead of converting it. The failure mode of a
denylist of one is not that it stays at one - it is that the second key goes in
cheaply and the addition makes the list look adequate. Two entries read as a
considered policy; one entry reads as a stub, and a stub is the only thing that
ever gets converted.
*/}}
{{- if dig "server" "hub" "public_url" "" $doc }}
{{- fail (printf "rendered settings.yaml sets server.hub.public_url: %s. The chart refuses this key. It does not override the hub's base URL, it splits it: the agent endpoint reads server.hub.public_url, the OAuth redirect resolver never reads the settings file, and the two then disagree inside one process while both look correct. Set the base URL through hub.baseUrl, which the chart renders as SCION_SERVER_BASE_URL - the only source both resolvers honour, so they agree by construction. If you reached this through config.extra, remove server.hub.public_url from it." (include "scion-hub.diagValue" (dig "server" "hub" "public_url" "" $doc))) }}
{{- end }}

{{- /* The discriminator for the two auth modes. The subtree it selects is not
rendered yet; see the comment in the rendered file. */}}
{{- $rootAuth := $root.Values.auth | default (dict) }}
{{- if ne (dig "server" "auth" "mode" "" $doc) $rootAuth.mode }}
{{- fail (printf "rendered settings.yaml has server.auth.mode: %s but auth.mode is %s." (include "scion-hub.diagValue" (dig "server" "auth" "mode" "" $doc)) (include "scion-hub.diagValue" $rootAuth.mode)) }}
{{- end }}

{{- /*
The oauth client credentials. THIS REPLACED AN ACKNOWLEDGEMENT KEY, and the
replacement is the point rather than an implementation detail.

auth.acknowledgeOAuthUnlanded used to stand here: a required opt-in that made the
operator confirm they understood oauth mode would render without the credentials
it needs. That was correct while the chart had no channel for them. It is not
correct now, and leaving it would have been the worse outcome of the two - a
permanent required key that guards nothing teaches the reflex of setting
acknowledgements to true, which is the reflex that makes the NEXT one useless.

WHAT IS CHECKED IS THE RENDERED DOCUMENT, NOT THE VALUES. So config.extra
satisfies it exactly as auth.oauth.web does, which is deliberate: an operator who
supplies server.oauth through config.extra has met the requirement, and a guard
that demanded the chart's own value instead would be demanding a spelling rather
than a state.

THE HARM IS SILENT, WHICH IS WHY THIS IS A REFUSAL. Nothing validates these
credentials at startup - they are copied into the server config unchecked at
cmd/server_foreground.go:1514-1545 - so a hub in oauth mode with no credentials
STARTS, binds, and passes /readyz. The failure arrives per request, at login:
pkg/hub/web.go:1770-1776 returns 503 "OAuth not configured" or 400 "OAuth
provider %s is not configured". Green in every Kubernetes signal, and nobody can
log in. That is worse than a crashloop, because nothing pages for it.

WEB, NOT ANY CLIENT TYPE. The hub keys the login check by client type -
IsProviderConfiguredForClient(OAuthClientTypeWeb, provider), pkg/hub/oauth.go:194
- so credentials under server.oauth.cli or server.oauth.device satisfy nothing
for a browser login. A check that accepted any client type would pass exactly the
configuration that fails. cli and device are not refused; they are just not
counted here.

BOTH HALVES, PER PROVIDER. IsProviderConfigured tests the client ID alone
(pkg/hub/oauth.go:51-58), so an ID with no secret makes the hub report the
provider as configured, offer the login button, and fail at the token exchange.
Half a credential is worse than none: none is caught here, half is caught by the
user.

Scoped to the rendered document, and therefore not evaluated under
config.existingSecret - this whole template is skipped there, correctly, because
the chart renders no auth mode in that shape and the operator's file is theirs.
*/}}
{{- if eq (dig "server" "auth" "mode" "" $doc) "oauth" }}
{{- $web := dig "server" "oauth" "web" (dict) $doc }}
{{- $complete := list }}
{{- $partial := list }}
{{- range $provider, $creds := $web }}
{{- $id := dig "client_id" "" $creds }}
{{- $secret := dig "client_secret" "" $creds }}
{{- if and $id $secret }}
{{- $complete = append $complete $provider }}
{{- else if or $id $secret }}
{{- $partial = append $partial (printf "%s (has %s, missing %s)" $provider (ternary "client_id" "client_secret" (ne $id "")) (ternary "client_secret" "client_id" (ne $id ""))) }}
{{- end }}
{{- end }}
{{- if $partial }}
{{- fail (printf "rendered settings.yaml has an incomplete OAuth web client credential: %s. A provider needs both halves, and the two missing halves fail differently - neither of them loudly. With client_id and no client_secret the hub reports the provider as CONFIGURED, because IsProviderConfigured tests the client ID alone (pkg/hub/oauth.go:51-58); it offers the login button and fails at the token exchange. With client_secret and no client_id the provider is not offered at all, and the secret sits in the settings Secret doing nothing. Either way a half-set credential fails later, and less legibly, than an unset one. Set both auth.oauth.web.<provider>.clientId and .clientSecret, or neither." (join ", " $partial)) }}
{{- end }}
{{- if not $complete }}
{{- fail "settings.yaml renders server.auth.mode: oauth, but no complete OAuth web client credential is present, so nobody would be able to log in to this deployment. Nothing catches this at runtime: the credentials are copied into the server config unvalidated (cmd/server_foreground.go:1514-1545), so the hub starts, binds and passes /readyz, and every login fails with \"OAuth provider is not configured\" (pkg/hub/web.go:1770-1776) - green in Kubernetes, unusable by humans. Set auth.oauth.web.google.clientId and auth.oauth.web.google.clientSecret (or the github pair), supply server.oauth.web through config.extra, or use auth.mode=proxy. Credentials under server.oauth.cli or server.oauth.device do NOT satisfy this: the hub keys the login check by client type (pkg/hub/oauth.go:194) and a browser login reads the web client only." }}
{{- end }}
{{- end }}

{{- /*
The camelCase trap, refused by name.

settings.yaml binds client_id/client_secret (V1OAuthProviderConfig,
pkg/config/settings_v1.go:635). The SCION_SERVER_* env mapper binds
clientId/clientSecret (OAuthProviderConfig, pkg/config/hub_config.go:334). The
doc comment on envKeyToConfigKey states the camelCase form explicitly, so it is
the spelling a careful reader arrives at - and in this file it binds nothing.
yaml.v3 drops the unknown key silently: no error, no warning, an empty field, a
hub that starts and refuses every login.

Reachable only through config.extra, because the chart's own render always emits
snake_case. That makes it the same class of hazard as server.hub.public_url
above - a key the chart cannot produce but an operator can - and it is checked
for the same reason.

Measured rather than reasoned: both spellings written into a real settings.yaml
and read back through config.LoadGlobalConfig, snake_case populating
cfg.OAuth.Web.Google and camelCase leaving it empty, each with the other as its
twin. harness/zz_p3_oauth_settings_probe_test.go.

Walks every client type, not just web. cli and device are not rendered by the
chart and are not required by the check above, but they bind through the same
struct, so the misspelling is silent there too.
*/}}
{{- range $clientType, $providers := (dig "server" "oauth" (dict) $doc) }}
{{- if kindIs "map" $providers }}
{{- range $provider, $creds := $providers }}
{{- if kindIs "map" $creds }}
{{- range $key, $_ := $creds }}
{{- if or (eq $key "clientId") (eq $key "clientSecret") }}
{{- fail (printf "rendered settings.yaml sets server.oauth.%s.%s.%s. That spelling binds nothing in a settings file and fails silently: settings.yaml reads client_id and client_secret (V1OAuthProviderConfig, pkg/config/settings_v1.go:635), while clientId and clientSecret are the SCION_SERVER_* environment mapper's spelling (pkg/config/hub_config.go:334). yaml.v3 drops the unknown key with no error, so the credential is simply absent and the hub starts and refuses every login. Rename it to %s. If you reached this through config.extra, that is the only way to reach it - the chart's own render emits snake_case." $clientType $provider $key (ternary "client_id" "client_secret" (eq $key "clientId"))) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The rendered settings.yaml.

Built as a dictionary and marshalled, so the output is valid YAML by
construction rather than by careful indentation, and so config.extra can be a
real deep merge rather than a text append.
*/}}
{{- define "scion-hub.settings" -}}
{{- $hubId := include "scion-hub.hubId" . }}
{{- include "scion-hub.assertConfigSource" . }}
{{- $driver := .Values.database.driver }}

{{- /* server.hub. hub_name, not name: the koanf tag is hub_name. */}}
{{- include "scion-hub.assertNoCredential" (dict "value" .Values.hub.name "source" "hub.name") }}
{{- $hub := dict "hub_id" $hubId "hub_name" .Values.hub.name }}

{{- /*
server.database. The key is url, not dsn. Pool settings are here now because they
are reachable no other way - SCION_SERVER_DATABASE_MAXOPENCONNS and its siblings
have snake_case koanf tags with no camelCase entry, so mapper #1 produces
database.max.open.conns and the variable never binds.
*/}}
{{- $database := dict "driver" $driver
    "max_open_conns" (int .Values.database.maxOpenConns)
    "max_idle_conns" (int .Values.database.maxIdleConns)
    "conn_max_lifetime" .Values.database.connMaxLifetime
    "conn_max_idle_time" .Values.database.connMaxIdleTime }}

{{- /*
The reserved position, now filled. Under password auth this value carries the
credential, which is why it is built here - inside the document that becomes the
settings Secret - and never passed through a ConfigMap, an argument vector or a
pod annotation. Only under postgres: a sqlite hub has no URL and rendering an
empty one would make server.database.url present-but-blank, which reads to the
HA preflight as configured.
*/}}
{{- if eq $driver "postgres" }}
{{- $database = set $database "url" (include "scion-hub.databaseUrl" .) }}
{{- end }}

{{- /* server.storage: the HUB'S BLOB STORE. Not the Filestore workspace share. */}}
{{- $storage := dict "provider" .Values.storage.provider }}
{{- if eq (lower (toString .Values.storage.provider)) "gcs" }}
{{- $bucket := .Values.storage.bucket }}
{{- if not $bucket }}
{{- fail "storage.bucket is required when storage.provider is gcs: a GCS storage provider cannot function without a bucket name." }}
{{- end }}
{{- $storage = set $storage "bucket" $bucket }}
{{- end }}

{{- /*
server.oauth: the web client credentials, and ONLY when they are set.

SNAKE_CASE HERE, camelCase IN THE VALUES, AND THAT IS NOT AN INCONSISTENCY -
it is two different schemas that happen to describe the same field. settings.yaml
binds through V1OAuthProviderConfig (pkg/config/settings_v1.go:635), whose yaml
tags are client_id and client_secret. The SCION_SERVER_* env mapper binds through
OAuthProviderConfig (pkg/config/hub_config.go:334), whose koanf tags are clientId
and clientSecret, reached via camelCaseFields["clientid"]. Same value, same hub,
two spellings, selected by channel.

MEASURED, BOTH SPELLINGS, ON A REAL LOAD. Written into a settings.yaml on disk
and read back through config.LoadGlobalConfig: client_id/client_secret populate
cfg.OAuth.Web.Google and make IsProviderConfiguredForClient(web, google) true;
clientId/clientSecret leave both fields empty and it returns false. yaml.v3 drops
the unknown key without an error, so the wrong spelling is not a failure the
operator sees - it is a hub that starts and refuses every login. The probe is
parked at harness/zz_p3_oauth_settings_probe_test.go with its negative twin.

EMITTED ONLY WHEN NON-EMPTY. An empty client_id is not the same as no client_id
to the collision check, and it is not the same to a reader of the rendered file:
a rendered `client_id: ""` looks like a credential that failed to interpolate.
Absent means absent.

A provider needs BOTH halves to be emitted at all. Half a credential configures
nothing - IsProviderConfigured tests ClientID alone (pkg/hub/oauth.go:51-58), so
an ID with no secret reports the provider as CONFIGURED and then fails the token
exchange at login. The assertion in assertSettings refuses that pairing by name;
this is only the render.
*/}}
{{- $oauthWeb := dict }}
{{- $auth := .Values.auth | default (dict) }}
{{- $authOauth := $auth.oauth | default (dict) }}
{{- $webValues := $authOauth.web | default (dict) }}
{{- range $provider := list "google" "github" }}
{{- $creds := index $webValues $provider }}
{{- if or $creds.clientId $creds.clientSecret }}
{{- $entry := dict }}
{{- if $creds.clientId }}
{{- $entry = set $entry "client_id" $creds.clientId }}
{{- end }}
{{- if $creds.clientSecret }}
{{- $entry = set $entry "client_secret" $creds.clientSecret }}
{{- end }}
{{- $oauthWeb = set $oauthWeb $provider $entry }}
{{- end }}
{{- end }}

{{- $server := dict
    "mode" "hosted"
    "hub" $hub
    "database" $database
    "storage" $storage
    "auth" (dict "mode" $auth.mode)
    "broker" (dict "host" "127.0.0.1" "port" (int (include "scion-hub.brokerPort" .)) "auto_provide" true) }}
{{- if $oauthWeb }}
{{- $server = set $server "oauth" (dict "web" $oauthWeb) }}
{{- end }}

{{- /*
LOAD-BEARING. schema_version is not boilerplate and it is not redundant with
anything. Do not drop it, and do not let it be dropped by an override path that
happens not to be covered.

It is what stops the hub's lazy settings migration from ever firing.
SetSettingValue auto-migrates when the file's format cannot be detected
(pkg/config/settings.go:590-600), and the format detector keys on this field;
with it present the hub delegates to the v1 handler and never migrates. The
migration itself replaces the file with os.Rename
(pkg/config/settings_v1.go:2694), which returns EBUSY against a bind-mounted
path - and this file is delivered as a subPath bind mount.

The hub deliberately does NOT guard this path in hosted mode. That decision was
taken on the basis that the chart controls the input, which means this line is
the guard. Every other write to settings.yaml under the mount is soft; this is
the only one that turns into a hard failure.

hack/verify.sh enforces it under the name migration-rename-hazard, across every
values permutation rather than only the default one.
*/}}
{{- $doc := dict "schema_version" "1" "active_profile" "default" "server" $server }}
{{- if .Values.agents.imageRegistry }}
{{- $doc = set $doc "image_registry" .Values.agents.imageRegistry }}
{{- end }}
{{- $doc = set $doc "profiles" (dict "default" (dict "runtime" "kubernetes")) }}
{{- $doc = set $doc "runtimes" (dict "kubernetes" (dict
    "type" "kubernetes"
    "namespace" (include "scion-hub.agentNamespace" .)
    "gke" true
    "list_all_namespaces" .Values.runtime.listAllNamespaces)) }}

{{- /* config.extra, deep-merged over the tree, so an unmodelled setting never
forces a chart fork. Merged before the assertions run, not after. */}}
{{- $preMerge := deepCopy $doc }}
{{- if .Values.config.extra }}
{{- $doc = mergeOverwrite $doc (deepCopy .Values.config.extra) }}
{{- end }}

{{- $rendered := toYaml $doc }}
{{- include "scion-hub.assertSettings" (dict "root" . "rendered" $rendered "hubId" $hubId) }}
{{- include "scion-hub.assertNoExtraCollision" (dict "preMerge" $preMerge "extra" .Values.config.extra) }}
{{- /* config.extra is an arbitrary subtree the operator controls, so it can put
a credential at a path no redaction list names - a DSN at server.database.url
was measured doing exactly that, landing in the Secret AND moving the
checksum/settings digest. The projection cannot enumerate its way out of an
open-ended surface; this turns the injection into a render failure instead. */}}
{{- include "scion-hub.assertNoCredentialTree" (dict "value" .Values.config.extra "source" "config.extra") }}
{{- $rendered }}
{{- end }}

{{/*
Every leaf path in a settings document, one per line, dotted.

Recursive: it calls itself through include. Leaves only - an intermediate map is
not emitted, because "server" and "server.hub" exist in every document and
reporting those as collisions would flag every use of config.extra.

Dots in a key name would produce an ambiguous path. No key in the settings
surface has one, and if one ever does the failure is a false positive naming the
right key, not a miss.
*/}}
{{- define "scion-hub.leafPaths" -}}
{{- $prefix := .prefix }}
{{- range $k, $v := .obj }}
{{- $path := ternary $k (printf "%s.%s" $prefix $k) (eq $prefix "") }}
{{- if and (kindIs "map" $v) (gt (len $v) 0) }}
{{- include "scion-hub.leafPaths" (dict "obj" $v "prefix" $path) }}
{{- else }}
{{ $path }}
{{- end }}
{{- end }}
{{- end }}

{{/*
config.extra may add settings. It may not silently overwrite ones the chart
itself wrote.

This is the settings-file analogue of the reserved-flag list, and it is the same
argument one channel over: a value the chart computes and an operator's override
of it are indistinguishable in the rendered output, so the manifest keeps
reporting the operator's intent and every assertion downstream of the merge
still passes. The difference from the flag list is that this one needs no
enumeration - both documents are in hand at merge time, so the rule is derived
rather than listed, and it cannot be incomplete the way a list can.

WHAT THIS DOES NOT CATCH, and it is deliberate rather than an oversight. It sees
only keys the chart WRITES. It cannot see an ALIAS - a settings key that is a
second name for a value the chart already sets through a different channel.
server.hub.public_url is exactly that: the chart controls the base URL through
hub.baseUrl -> SCION_SERVER_BASE_URL and never writes public_url, so this check
is silent on it and the explicit refusal in assertSettings is what catches it.
The two rules cover disjoint halves and neither can see the other's. Phase 5a
owns enumerating the aliases; do not delete either rule believing the other
covers it.

Runs AFTER assertSettings so that a collision on a key with its own assertion -
the hub ID, the driver, server.mode, schema_version - still reports that
assertion's specific message. A generic "you overwrote a key" would be a
regression in every one of those cases.
*/}}
{{- define "scion-hub.assertNoExtraCollision" -}}
{{- if .extra }}
{{- $chartKeys := splitList "\n" (trim (include "scion-hub.leafPaths" (dict "obj" .preMerge "prefix" ""))) }}
{{- $collisions := list }}
{{- range $path := splitList "\n" (trim (include "scion-hub.leafPaths" (dict "obj" .extra "prefix" ""))) }}
{{- $p := trim $path }}
{{- if $p }}
{{- range $chartPath := $chartKeys }}
{{- $c := trim $chartPath }}
{{- if or (eq $p $c) (hasPrefix (printf "%s." $c) $p) (hasPrefix (printf "%s." $p) $c) }}
{{- $collisions = append $collisions $p }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- if $collisions }}
{{- fail (printf "config.extra overwrites %s, which the chart itself sets. config.extra is for settings the chart does not model; overriding one it does write is invisible afterwards, because the rendered file reports your value and every check downstream of the merge passes on it. If the chart's value is wrong for you, change the value that produces it - or say why it cannot, because that is a gap in the chart's own interface rather than a job for the escape hatch." (join ", " (uniq $collisions))) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
================================================================================
PHASE 2 - CLOUD SQL. Appended at end of file by agreement with gd-p1-dev and
gd-p3-dev: three phases edit this file, and appending is the only edit that
cannot collide with an insertion point somebody else is also moving.
================================================================================
*/}}

{{/*
Percent-encode one userinfo component of a URL.

The IAM database role is a Google service account with the domain trimmed, so
it CONTAINS AN @: "scion-hub@my-project.iam". Go's net/url splits the authority
at the LAST @, so the raw two-@ form does in fact parse correctly - I measured
it against the hub's real parser rather than assuming it. It is still encoded
here, for two reasons that outlive that measurement:

  - The last-@ rule is net/url's, not RFC 3986's. Any intermediary that splits
    at the FIRST @ - a log scrubber, a URL rewriter, a different language's
    parser - reads the host as "my-project.iam@127.0.0.1:5432" and is wrong in
    a way that produces a connection error nobody can explain from the DSN as
    written.
  - The password arm has no such luck. A password containing @ or : is
    misparsed outright, and that is an operator's arbitrary string, not a
    Google-shaped identifier.

% MUST BE FIRST or it re-encodes the escapes emitted after it. The set is the
RFC 3986 userinfo-illegal characters that can plausibly occur here; a character
outside it passes through unchanged, which is correct - over-encoding a legal
character is as wrong as under-encoding an illegal one.

Verified by round-trip through pgx's own parser, not by inspection: see
hack/verify.sh dsn-roundtrip and tests/.
*/}}
{{- define "scion-hub.pctEncodeUserinfo" -}}
{{- $s := toString . -}}
{{- $s = replace "%" "%25" $s -}}
{{- $s = replace "@" "%40" $s -}}
{{- $s = replace ":" "%3A" $s -}}
{{- $s = replace "/" "%2F" $s -}}
{{- $s = replace "?" "%3F" $s -}}
{{- $s = replace "#" "%23" $s -}}
{{- $s = replace "[" "%5B" $s -}}
{{- $s = replace "]" "%5D" $s -}}
{{- $s = replace " " "%20" $s -}}
{{- $s -}}
{{- end }}

{{/*
The Postgres role, unencoded.

Under auth: iam the role is the Google service account with the trailing
".gserviceaccount.com" removed - Cloud SQL registers IAM principals under that
truncated form, and the untruncated one is simply not a role that exists. The
chart derives it from serviceAccount.gcpServiceAccount rather than asking for it
twice, because two fields that must agree are two fields that can disagree.

database.user still overrides, for the case the derivation does not cover: a
user-managed role name, or a service account whose Cloud SQL role was created
under a different spelling.
*/}}
{{- define "scion-hub.databaseUser" -}}
{{- if .Values.database.user -}}
{{- .Values.database.user -}}
{{- else if eq .Values.database.auth "iam" -}}
{{- $gsa := .Values.serviceAccount.gcpServiceAccount -}}
{{- if not $gsa -}}
{{- fail "database.auth is iam but neither database.user nor serviceAccount.gcpServiceAccount is set. IAM database authentication logs in AS the pod's Google service account, so with no service account there is no role to log in as. Either set serviceAccount.gcpServiceAccount and let the chart derive the role from it, or set database.user to the role name explicitly." -}}
{{- end -}}
{{- trimSuffix ".gserviceaccount.com" $gsa -}}
{{- else -}}
{{- fail "database.user is required when database.auth is password. Under password authentication the chart has nothing to derive a role name from - unlike iam, where the role is the service account." -}}
{{- end -}}
{{- end }}

{{/*
server.database.url, the position secret-settings.yaml reserves.

Shape: postgres://USER[:PASSWORD]@127.0.0.1:PORT/NAME?sslmode=disable

127.0.0.1 is not a placeholder. The proxy runs in this pod and every container
in a pod shares one network namespace, so the loopback address IS the tunnel
entrance. The port is cloudsql.port, read here and by the proxy's --port from
the same key, so the DSN and the listener cannot drift apart.

sslmode=disable is correct and is not a downgrade. The encrypted, mutually
authenticated leg is proxy-to-Cloud-SQL; the hub-to-proxy leg never leaves the
pod's network namespace. Asking for TLS on it fails, because the proxy's local
listener does not serve TLS - so an operator who "hardens" this to require
breaks the connection without adding a metre of protected path.

UNDER auth: iam THERE IS NO PASSWORD IN THIS STRING AT ALL - not an empty one,
no colon. The proxy mints an OAuth token per connection with --auto-iam-authn.
Under auth: password the credential is here, and this string only ever exists
inside the settings Secret.
*/}}
{{- define "scion-hub.databaseUrl" -}}
{{- /*
THE HOST IN THIS URL IS A CONSTANT, so the thing that makes it true has to be
checked before it is written. 127.0.0.1 is correct only because the Auth Proxy
is in this pod listening on it; with cloudsql.enabled false the chart would emit
a DSN pointing at a loopback port nothing binds, the hub would come up, and the
first database call would fail with connection refused - a runtime mystery
manufactured at template time.

The schema refuses this too. It is checked in both places on purpose and this
layer is not redundant: the schema can only say WHICH key is wrong, and its
message for a conditional is "(root): Must validate "then" as "if" was valid -
cloudsql.enabled does not match: true", which does not tell an operator that the
driver they chose is what demands the proxy. This layer is also the one an
operator reaches with --skip-schema-validation.
*/ -}}
{{- if not .Values.cloudsql.enabled -}}
{{- fail "database.driver is postgres but cloudsql.enabled is false. This chart reaches Postgres only through the Cloud SQL Auth Proxy: it renders server.database.url with the host fixed at 127.0.0.1 and the proxy is what listens there, so with the proxy off the hub would start and then fail every query with connection refused. There is no database.host key and this is deliberate - a direct-to-Postgres path needs its own TLS, credential and network-policy story, and none of it is written. Set cloudsql.enabled: true with cloudsql.instanceConnectionName, or set database.driver: sqlite." -}}
{{- end -}}
{{- $user := include "scion-hub.databaseUser" . | include "scion-hub.pctEncodeUserinfo" -}}
{{- $cred := $user -}}
{{- if eq .Values.database.auth "password" -}}
{{- if not .Values.database.password -}}
{{- fail "database.password is required when database.auth is password." -}}
{{- end -}}
{{- $cred = printf "%s:%s" $user (include "scion-hub.pctEncodeUserinfo" .Values.database.password) -}}
{{- end -}}
{{- printf "postgres://%s@127.0.0.1:%d/%s?sslmode=disable" $cred (int .Values.cloudsql.port) .Values.database.name -}}
{{- end }}

{{/*
The Cloud SQL Auth Proxy container.

ONE DEFINITION, TWO PLACEMENTS. As a native sidecar it is an initContainers
entry carrying restartPolicy: Always; on clusters below 1.29 it is an ordinary
container appended after the hub. Rendering it from one define means the two
placements cannot drift into being two different proxies - the failure this
would otherwise invite is a fix applied to the native path and not the fallback,
which nobody runs until the day they run it on an old cluster.

restartPolicy is added by the CALLER, not here, because it is the single field
that distinguishes the two placements and putting it inside a conditional here
would hide the distinction inside the thing being distinguished.
*/}}
{{- define "scion-hub.cloudsqlProxyContainer" -}}
{{- $cs := .Values.cloudsql -}}
{{- $args := list "--structured-logs" -}}
{{- /*
--port and the DSN's port come from the same value. They are the two ends of one
loopback connection and a chart that let them disagree would produce a hub
dialling a port nothing listens on, with both halves individually plausible.
*/ -}}
{{- $args = append $args (printf "--port=%d" (int $cs.port)) -}}
{{- $args = append $args "--health-check" -}}
{{- /*
0.0.0.0 and not 127.0.0.1. The probes are issued by the kubelet from OUTSIDE the
pod's network namespace, so a health server bound to loopback is unreachable by
the very thing it exists to answer - and the symptom is a readiness probe that
fails while the proxy is perfectly healthy.
*/ -}}
{{- $args = append $args "--http-address=0.0.0.0" -}}
{{- $args = append $args (printf "--http-port=%d" (int $cs.healthCheckPort)) -}}
{{- if eq .Values.database.auth "iam" -}}
{{- $args = append $args "--auto-iam-authn" -}}
{{- end -}}
{{- if $cs.privateIp -}}
{{- $args = append $args "--private-ip" -}}
{{- end -}}
{{- $args = append $args $cs.instanceConnectionName -}}
{{- /*
Phase 0's guard, reused rather than reimplemented. Every argument is checked for
an embedded credential before it is rendered. Under auth: password the password
must reach the process through the settings Secret and NOTHING else; argv is
world-readable to anything that can read /proc in this pod, and it is echoed by
kubectl describe, by the API server's audit log and by every controller that
logs a pod spec.
*/ -}}
{{- range $a := $args -}}
{{- include "scion-hub.assertNoCredential" (dict "value" $a "source" "cloud-sql-proxy argument") -}}
{{- end -}}
- name: cloud-sql-proxy
  image: {{ include "scion-hub.cloudsqlProxyImage" . | quote }}
  imagePullPolicy: {{ $cs.image.pullPolicy }}
  args:
    {{- range $a := $args }}
    - {{ $a | quote }}
    {{- end }}
  {{- /*
  /startup and /readiness are the PROXY's endpoints. They are not the hub's, and
  the hub's /readyz is not the proxy's. Pointing either process's probe at the
  other's path produces a probe that answers about the wrong process.

  The startup probe is what makes the native sidecar worth having: the kubelet
  holds the hub container until this one reports started, so the hub's
  AutoMigrate does not race the tunnel.
  */}}
  startupProbe:
    httpGet:
      path: /startup
      port: {{ int $cs.healthCheckPort }}
    periodSeconds: 1
    failureThreshold: 60
    timeoutSeconds: 5
  readinessProbe:
    httpGet:
      path: /readiness
      port: {{ int $cs.healthCheckPort }}
    periodSeconds: 10
    failureThreshold: 3
    timeoutSeconds: 5
  securityContext:
    {{- /*
    Restated at container level for the same reason the hub container restates
    it: a container-level securityContext shadows the pod-level one field by
    field, so a change that only reaches the pod block cannot quietly return
    this container to root.

    THE PROXY DOES NOT RUN AS ITS IMAGE'S OWN UID. The image declares USER
    65532, but this pod's securityContext sets runAsUser from
    hub.securityContext.runAsUser (1000 by default) and that is inherited by
    every container, so the proxy runs as the hub's uid. There is no way to
    opt a single container back out of a pod-level runAsUser - it can be
    overridden, not unset - so this is a property of the pod, not a choice made
    here. It is recorded because the obvious reading of the image is wrong.

    That is EXPECTED to be harmless: the proxy writes nothing, opens no
    uid-owned files, and needs only a socket and the metadata server. It is
    NOT VERIFIED, because verifying it requires running the pod and there is no
    cluster - see VALIDATION.md, where it sits with the rest of the unrun smoke
    test rather than being asserted here as though it had been checked.

    readOnlyRootFilesystem is safe here and is NOT safe on the hub container -
    the hub writes its state directory - which is why it appears on this
    container only. It is unverified for the same reason and in the same place.
    */}}
    {{- include "scion-hub.nonRootSecurityContext" . | nindent 4 }}
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop:
        - ALL
  {{- with $cs.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}

{{/*
The proxy image, pinned by digest.

DIGEST ONLY, and there is deliberately no tag value to set. image.repository /
image.digest for the hub refuse a tag and a digest together, on the grounds that
two sources for one identity can disagree; the same reasoning applies here, and
the proxy has no reason to ever run unpinned. A tag can be repointed underneath
a running cluster by whoever owns the registry. A digest cannot.
*/}}
{{- define "scion-hub.cloudsqlProxyImage" -}}
{{- $img := .Values.cloudsql.image -}}
{{- $repo := required "cloudsql.image.repository is required when cloudsql.enabled." $img.repository -}}
{{- $digest := required "cloudsql.image.digest is required: the Cloud SQL Auth Proxy is pinned by digest, not by tag. Resolve one with: curl -sSI -H 'Accept: application/vnd.oci.image.index.v1+json' https://gcr.io/v2/cloud-sql-connectors/cloud-sql-proxy/manifests/<version> and read the docker-content-digest header." $img.digest -}}
{{- printf "%s@%s" $repo $digest -}}
{{- end }}

{{/*
The hub's in-process runtime broker port. ONE SOURCE, TWO READERS.

This number is written into server.broker.port in the settings document and is
also an entry in the port-collision guard below, and those two are not
independent facts: the guard's entire job is to refuse an operator port that
collides with what the settings file actually configures. It was a bare 9800 in
both places, including inside a human-readable message. Two fields that must
agree are two fields that can disagree, and this pair would disagree SILENTLY -
the settings file would move, the guard would keep refusing the old port, and
the new collision it exists to catch would render clean.

Nothing in the values surface sets this today and this helper deliberately does
not add one: the broker is in-process and its port is not an operator's to
choose. It is a helper so that the two readers cannot drift, not to make the
number configurable. hack/verify.sh measures the linkage from outside, deriving
the port from the rendered settings document rather than naming it.
*/}}
{{- define "scion-hub.brokerPort" -}}
9800
{{- end }}

{{/*
Port collisions inside the pod's single network namespace.

EVERY CONTAINER IN A POD SHARES ONE NETWORK NAMESPACE. Two processes in
different containers binding the same port is not isolated by the container
boundary - it is an ordinary bind conflict, and the loser fails at startup with
"address already in use" while the manifest looks entirely reasonable, because
each port is declared in a different container's block and nothing in the
Kubernetes API cross-checks them.

Four ports live in this namespace once the proxy is added, and only two of them
are visible in the Deployment: the hub's web port and the proxy's two. The
fourth, the hub's in-process runtime broker on 9800, is set in settings.yaml and
does not appear in the pod spec at all - so an operator who moves the proxy's
health port onto it gets a conflict with a process they cannot see declared
anywhere nearby. That is the case this guard is really for.
*/}}
{{- define "scion-hub.assertCloudsqlPorts" -}}
{{- if .Values.cloudsql.enabled }}
{{- $seen := dict }}
{{- $ports := list
    (dict "n" (int .Values.hub.webPort)              "name" "hub.webPort")
    (dict "n" (int .Values.cloudsql.port)            "name" "cloudsql.port (the proxy's Postgres listener)")
    (dict "n" (int .Values.cloudsql.healthCheckPort) "name" "cloudsql.healthCheckPort (the proxy's health server)")
    (dict "n" (int (include "scion-hub.brokerPort" .)) "name" (printf "the hub's in-process runtime broker, fixed at %s in settings.yaml" (include "scion-hub.brokerPort" .))) }}
{{- range $p := $ports }}
{{- $k := printf "p%d" $p.n }}
{{- if hasKey $seen $k }}
{{- fail (printf "port %d is claimed by both %s and %s. Every container in a pod shares one network namespace, so these are the same port and the second process to bind it fails at startup with 'address already in use'. Container boundaries do not separate ports; only the namespace does, and there is one." $p.n (get $seen $k) $p.name) }}
{{- end }}
{{- $seen = set $seen $k $p.name }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The native sidecar's version requirement, asserted where it applies.

THIS REPLACES A kubeVersion FLOOR IN Chart.yaml, AND THE REASON IT IS HERE
INSTEAD OF THERE IS THAT THE REQUIREMENT IS CONDITIONAL ON A VALUE. Chart.yaml's
kubeVersion is evaluated before any value is read, so ">=1.29.0-0" rejected
every cluster below 1.29 - including the ones cloudsql.nativeSidecar: false
exists to serve, and which values.yaml and NOTES.txt both tell the operator to
use. gd-p2-rev measured that contradiction. The requirement is real, but it is
the requirement of one branch of one template, so it is asserted from that
branch.

WHY Major/Minor AND NOT semverCompare. .Capabilities.KubeVersion.Version is
whatever the API server reports, and real ones are not plain semver: GKE reports
v1.29.4-gke.1043002, and Minor is frequently "28+" on managed distributions.
semverCompare on those strings either errors or silently mis-orders them, and
the usual workaround - stripping non-digits before comparing - turns
1.28.5-gke.1200 into 1.28.51200, which compares as NEWER than 1.29. Comparing
the two integers directly has none of those failure modes.

WHY THIS DOES NOT REFUSE A VERSION IT CANNOT PARSE, AND WHY THAT IS NOT THE
SAME AS PASSING. If Minor does not yield a number, the only honest statement is
that this render does not know the cluster's version - and "unknown" is not
"below 1.29". Refusing there would block a conformant 1.33 cluster whose only
sin is an unusual version string, and the remedy the message names (set
nativeSidecar false) would then push it into the crash-loop window NOTES.txt
warns about, which is worse than where it started.

🔴 SO THE THIRD OUTCOME IS LOUD. There are three results here, not two: refused,
checked-and-fine, and NOT CHECKED. The third one emits a notice into the
rendered manifest and into NOTES.txt naming the version string it could not
read. A guard that silently declines to guard is indistinguishable from a guard
that ran and approved, and the whole reason this replaced a kubeVersion floor is
that an unasserted claim looked like a checked one for weeks. Per the lead's
standing rule: pass, fail and did-not-measure are three outcomes and the third
one says so.

WHY THE DECISION TAKES ITS INPUTS AS ARGUMENTS. .Capabilities cannot be forged
from the command line - helm parses --kube-version as semver and always hands
the template a numeric Major and Minor - so the not-checked branch is
UNREACHABLE FROM helm template AND WOULD HAVE SHIPPED UNTESTED. Taking the two
strings as parameters makes the branch reachable from a probe template, which is
how hack/verify.sh gets at it. The caller in deployment.yaml is what binds them
to the real cluster; this define does the deciding and nothing else.
*/}}
{{- define "scion-hub.nativeSidecarGuard" -}}
{{- $major := regexReplaceAll "[^0-9]" .major "" }}
{{- $minor := regexReplaceAll "[^0-9]" .minor "" }}
{{- if and (ne $major "") (ne $minor "") }}
{{- if and (eq (int $major) 1) (lt (int $minor) 29) }}
{{- fail (printf "cloudsql.nativeSidecar is true and this cluster reports Kubernetes %s, which is below 1.29. Native sidecars (an initContainers entry with restartPolicy: Always) are only honoured from 1.29. Below that the API server ACCEPTS the field and ignores it, so the proxy becomes an ordinary init container that never exits and the pod hangs in Init forever with no error anywhere - which is why this is refused at render time rather than left to be diagnosed in a cluster. Set cloudsql.nativeSidecar=false to run the proxy as a plain sidecar instead; read the warning NOTES.txt prints on that path first, because the hub crash-loops until the tunnel is up." .version) }}
{{- end }}
{{- else }}
{{- printf "# scion-hub: NATIVE SIDECAR VERSION CHECK NOT RUN. cloudsql.nativeSidecar is true, which needs Kubernetes 1.29 or later, and this cluster reports a version this chart could not read a major/minor out of (version=%q major=%q minor=%q). The check was SKIPPED, not passed - nothing here has established that this cluster honours restartPolicy on an init container. If it does not, the pod hangs in Init forever with no error: set cloudsql.nativeSidecar=false. Verify with: kubectl version" .version .major .minor }}
{{- end }}
{{- end }}

{{- /*
scion-hub.settingsChecksum - ADOPTED FROM PHASE 3, NOT WRITTEN HERE.

Provenance, because a reader who does not know this will "simplify" it. Author
gd-p3-dev, branch scion/gke-chart-p3, handed over 2026-08-17 with a mutation
matrix in which every row was run. The oauth branch below is theirs and is
inert in this phase; the server.database.url branch is the one this phase
reaches, and it was written before any input existed that could reach it. It
arrived here as UNTESTED CODE and is labelled as such in their handover. The
differential in hack/verify.sh is what turned it into a tested one - if you
change this helper, that gate is where you find out.

Keep the define body byte-identical to the phase 3 copy where you can: both
branches carry it and the integration merge resolves by taking either side only
for as long as that stays true. The known deltas are the floor constant and the
paragraph that justifies it, both marked PHASE 2 DELTA below.
*/}}
{{- define "scion-hub.settingsChecksum" -}}
{{- $obj := fromYaml (include (print .Template.BasePath "/secret-settings.yaml") .) }}
{{- if hasKey $obj "Error" }}
{{- fail (printf "scion-hub.settingsChecksum could not parse the rendered settings Secret as YAML: %s. This annotation is a digest of a redacted projection of that document, so a parse failure would digest an error string instead - a value that never changes, which is worse than no annotation because it looks like coverage." (get $obj "Error")) }}
{{- end }}
{{- $doc := fromYaml (dig "stringData" "settings.yaml" "" $obj) }}
{{- if not (hasKey $doc "server") }}
{{- fail "scion-hub.settingsChecksum parsed the settings Secret but the document has no top-level server key. Every settings document the chart renders has one, so this means the projection is operating on an empty or unexpected document - and a digest of an empty document is a constant, which would silently stop rolling pods on every future settings change. Failing instead." }}
{{- end }}
{{- $redacted := "[redacted-from-checksum]" }}
{{- $marks := 0 }}
{{- $rendered := list }}
{{- range $provider, $entry := (dig "server" "oauth" "web" (dict) $doc) }}
{{- if hasKey $entry "client_secret" }}
{{- $rendered = append $rendered (toString (get $entry "client_secret")) }}
{{- $_ := set $entry "client_secret" $redacted }}
{{- $marks = add1 $marks }}
{{- end }}
{{- end }}
{{- $db := dig "server" "database" (dict) $doc }}
{{- if hasKey $db "url" }}
{{- $was := toString (get $db "url") }}
{{- /*
PHASE 2 DELTA. THE USERNAME IS CAPTURED AND RE-EMITTED; only the password is
replaced. gd-p2-rev found this as C1, measured: rotating database.user left
checksum/settings byte-identical, so the pods did not roll, while NOTES.txt told
the operator - in the section written to prevent exactly this - that rotating
the user rolls them automatically. An operator rotating a LEAKED credential got
a green upgrade and kept serving on the retired one.

The old pattern consumed the username as part of the match and dropped it:

  "://[^/@[:space:]]*:[^/@[:space:]]+@"  ->  "://[redacted-from-checksum]@"

That is strictly more redaction than this projection was specified to do. The
username is not a credential - it is in values.yaml, in NOTES.txt and in the
plain settings document - and blanking it made a non-secret field invisible to
the digest, which is how a real configuration change stopped rolling pods.
Keeping it is what makes the annotation's promise true for every part of the DSN
except the one part that must never be digested.
*/}}
{{- $now := regexReplaceAll "://([^:/@[:space:]]*):[^/@[:space:]]+@" $was (printf "://${1}:%s@" $redacted) }}
{{- if ne $now $was }}
{{- $_ := set $db "url" $now }}
{{- $marks = add1 $marks }}
{{- end }}
{{- end }}
{{- $projection := toYaml $doc }}
{{- $found := sub (len (splitList $redacted $projection)) 1 }}
{{- if ne (int $found) (int $marks) }}
{{- fail (printf "scion-hub.settingsChecksum performed %d redactions but the projection carries %d redaction markers. The two must agree: this is how the helper proves its own edits reached the document that gets digested, rather than assuming the assignment landed. Either a `set` above did not take effect on the parsed document - in which case a credential is about to be digested - or some rendered settings value contains the literal marker text %q, which the chart cannot distinguish from its own mark. Fix the first; for the second, change the marker." (int $marks) (int $found) $redacted) }}
{{- end }}
{{- /* SECOND PROOF, AND IT HAS A FLOOR THAT IS THERE FOR A MEASURED REASON.
The marker count above proves the edits landed at the paths the helper knows.
It cannot prove the same credential is not ALSO sitting at some path the list
does not know about, so each rendered credential is searched for by value in the
finished projection.

THE FLOOR. That search is a plain substring test, and a short credential
collides with ordinary prose: measured, a client secret of "def" is found inside
the word "default" in the rendered settings, and the render was refused with a
message telling the maintainer to add a path that does not exist. False, loud,
and with remediation advice that cannot be followed. So the value search applies
only at 12 characters or more. Google issues 24-character client secrets and
GitHub 40, so no real credential is below the floor.

WHAT THE FLOOR CANNOT SEE, STATED PLAINLY: a credential shorter than 12
characters copied to a path outside the redaction list would be digested and
this check would not say so. The path redaction still applies to every path it
knows, the marker count still proves those landed, and a secret that short is
not a secret. This is a narrowed check, not a disabled one, and it is narrowed
in the direction that removes false refusals rather than the direction that
removes refusals.

AND THE ONE CASE NEITHER HALF COVERS, BECAUSE A MUTATION FOUND IT RATHER THAN
REASONING. The marker count proves a marker EXISTS; it does not prove the marker
is at the right key. Rewriting the `set` above to a misspelled key leaves the
credential in place AND inserts a marker, so the count still balances - and with
a sub-floor credential the value check is silent too. Measured: the render
succeeds and `client_secret: shrt` reaches the digest. Both guards see the same
mutation the moment the credential is of realistic length, and removing the
`set` outright is caught at any length, so what is uncovered is the intersection
of two unlikely things. It is written down rather than closed because a guard
whose gap is named is a guard someone can widen; an unnamed one is a guard
people trust past its edge. */ -}}
{{- range $s := $rendered }}
{{- if and (ge (len $s) 12) (contains $s $projection) }}
{{- fail (printf "scion-hub.settingsChecksum redacted the credential paths it knows about and a rendered credential is STILL present in the digest input. The path list above is missing the path this value came from. Do not silence this by widening the value check - add the path, because the annotation is published to a wider audience than the Secret and a digest of a credential is a verification oracle for it. Value begins %q." (trunc 4 $s)) }}
{{- end }}
{{- end }}
{{- /*
PHASE 2 DELTA, FORCED BY THE ONE ABOVE. The backstop below looks for a
scheme://user:password@host URL surviving into the digest input. Now that the
redaction preserves the username, its own output - ://user:[redacted...]@ - has
the shape the backstop hunts for, and the backstop would fire on every render it
had just correctly redacted. So the known-redacted form is removed first, by
plain string replacement rather than by widening the pattern.

The distinction matters: a WIDER pattern would also stop matching real
credentials that happen to resemble the marker, which is how a backstop quietly
stops backstopping. Removing the exact literal this helper just wrote leaves the
pattern as strict as it was for everything the helper did not write.
*/}}
{{- $probe := replace (printf ":%s@" $redacted) "@" $projection }}
{{- if regexMatch "://[^/@[:space:]]*:[^/@[:space:]]+@" $probe }}
{{- fail "scion-hub.settingsChecksum found a scheme://user:password@host URL in the digest input AFTER redaction. This is a backstop and reaching it means an upstream guard was missed, so fix the upstream one rather than this: either some settings path now carries a credential-bearing URL and is not in the redaction list above (add the path), or a values surface that feeds settings.yaml is not running scion-hub.assertNoCredential (add the call, and prefer that - it names the value the operator actually set, which this message cannot)." }}
{{- end }}
{{- $_ := set $obj "stringData" (dict "settings.yaml" $projection) }}
{{- toYaml $obj | sha256sum }}
{{- end }}

{{/*
================================================================================
PHASE 3 - SESSION SECRET, OAUTH CREDENTIALS, CREDENTIAL PLACEMENT CHECKER.
================================================================================
*/}}

{{/*
The name of the Secret carrying the session secret.

Two sources, never both, and the caller must have run assertSessionSecret first
so that "neither" is already refused by the time this renders.
*/}}
{{- define "scion-hub.sessionSecretName" -}}
{{- $auth := .Values.auth | default (dict) }}
{{- if $auth.existingSecret }}
{{- $auth.existingSecret }}
{{- else }}
{{- printf "%s-session" (include "scion-hub.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The key inside that Secret which holds the session secret.

For the chart-rendered Secret this is fixed: the hub reads the value through a
literal os.Getenv("SCION_SERVER_SESSION_SECRET") (resolveSessionSecret,
cmd/server_foreground.go:1452-1463), and the chart-rendered Secret is consumed
with envFrom, which turns each KEY INTO AN ENV VAR NAME. So the key name is the
env var name and it is not a free choice.

For an operator's existing Secret the key IS a free choice - theirs, not ours -
which is what auth.existingSecretKey is for, and why that path cannot use
envFrom. See assertSessionSecret.
*/}}
{{- define "scion-hub.sessionSecretKey" -}}
{{- $auth := .Values.auth | default (dict) }}
{{- if $auth.existingSecret }}
{{- default "SCION_SERVER_SESSION_SECRET" $auth.existingSecretKey }}
{{- else }}
{{- print "SCION_SERVER_SESSION_SECRET" }}
{{- end }}
{{- end }}

{{/*
The session secret must have exactly one source, and "none" is refused here
rather than at runtime.

WHY THIS IS A TEMPLATE-TIME FAILURE AND NOT A DEFAULT. The hub does not fail
without a session secret. resolveSessionSecret returns "" and, in hosted mode,
emits a single slog.Warn (cmd/server_foreground.go:1460-1462) - then the hub
starts, binds, and passes every probe. What it has actually done is derive a
per-process signing key, so each replica signs tokens the others reject and each
replica has its own cookie encryption key. The symptom is users being logged out
when the load balancer moves them, which reads as a flaky session store rather
than as a configuration error, and it gets worse with replica count. There is no
value this chart could default to that would be correct, so it asks.

AND THE CHART DOES NOT GENERATE ONE. randAlphaNum would regenerate on every helm
upgrade unless guarded with lookup, and lookup returns empty under helm template
and --dry-run - so the golden output and the installed output would diverge, and
a chart whose rendered form differs from its installed form cannot be reviewed.
Worse, a silent rotation invalidates every live session AND the shared JWT
signing key at once, which is the exact harm SCION_REQUIRE_STABLE_SIGNING_KEY
exists to prevent. Failing loudly is the only honest option. See design.md 6.

BOTH sources set is also refused. It is not a precedence question: one of the
two would be silently inert, and an inert secret value is indistinguishable from
a working one until sessions start breaking.
*/}}
{{- define "scion-hub.assertSessionSecret" -}}
{{- $auth := .Values.auth | default (dict) }}
{{- if and $auth.sessionSecret $auth.existingSecret }}
{{- fail "auth.sessionSecret and auth.existingSecret are both set. The chart cannot use both: one would be silently ignored, and a session secret that is silently ignored presents as intermittent logouts rather than as an error. Set exactly one - auth.existingSecret to reference a Secret you manage, or auth.sessionSecret to have the chart render one." }}
{{- end }}
{{- if not (or $auth.sessionSecret $auth.existingSecret) }}
{{- fail "auth.sessionSecret is not set and neither is auth.existingSecret, so this release has no session secret. The chart will not generate one: a generated secret rotates on every helm upgrade, which silently invalidates every session and the shared JWT signing key, and it cannot be rendered reproducibly because lookup returns empty under helm template. Nor will the hub refuse to start without one - it logs a single warning (cmd/server_foreground.go:1460) and then derives a per-process signing key, so each replica signs tokens the others reject and users are logged out whenever the load balancer moves them. Set auth.existingSecret to the name of a Secret you manage (preferred - the value never enters your values file or Helm's release storage), or set auth.sessionSecret to have the chart render one." }}
{{- end }}
{{- if and $auth.existingSecretKey (not $auth.existingSecret) }}
{{- fail "auth.existingSecretKey is set but auth.existingSecret is not. The key names an entry inside a Secret the chart is not being given, so it selects nothing. Set auth.existingSecret too, or remove the key." }}
{{- end }}
{{- end }}
