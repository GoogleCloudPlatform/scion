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
    first, so: this chart mounts no volumes and renders no --db. Replicas share
    NO mutable state, and isHADeployment (cmd/server_foreground.go:927) is FALSE
    at every replica count - K_SERVICE is unset on GKE, the driver is not
    postgres, and the gcs-plus-proxy branch is unset - so the hosted HA preflight
    at :921 does not run either.
  - The stated harm - "two hubs writing the same RWX workspace share" - is
    therefore not a thing this chart can produce.
  - The replacement harm - "the upgrade transiently enters HA mode" - is false
    for the same reason, and not for the reason first given for it. HA is not
    "already on"; it is off, at one replica and at ten.
  - LATER PHASES FALSIFY THAT ON PURPOSE, WHICH IS WHY IT IS WRITTEN DOWN RATHER
    THAN LEFT AS AN ABSENCE. The Cloud SQL phase sets the postgres driver and
    turns isHADeployment true; the Filestore phase lands the shared volumes.
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
     - A GLOBAL settings.yaml ALREADY EXISTS AT PHASE 0. This chart delivers no
       ConfigMap and no Secret, but the hub writes one before it reads one:
       cmd/server_foreground.go:107 calls config.InitGlobal, seeded from
       pkg/config/embeds/default_settings.yaml - which carries schema_version,
       active_profile, default_template, default_harness_config, image_registry,
       cli, runtimes and profiles, AND NO server KEY. So "the chart ships no
       settings file" is true and is NOT the reason; the reason is that the file
       which does exist has no server key.
     - AND "the hub always creates it" IS NOT DURABLE EITHER. InitGlobal is
       guarded at cmd/server_foreground.go:104 by os.Stat(globalDir) /
       os.IsNotExist. It fires only when $HOME/.scion is absent. A phase that
       mounts a volume there skips the seeding entirely and the contents become
       whatever the volume holds. Phase 0's answer does not move - an empty
       mounted directory still has no server key - but a comment keyed off the
       file would.

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
4. A LATER PHASE DELIVERS THESE THROUGH A CHANNEL OTHER THAN argv, AND argv WINS
   OVER IT SILENTLY. The tense is the correction from round 4 and it is the whole
   point of this paragraph. This comment used to open "the chart ALREADY delivers
   these settings through a channel other than argv", WHICH IS FALSE AT THIS HEAD:
   the chart renders no ConfigMap, no Secret, no env, no envFrom and no volumes,
   so it delivers none of these five through any channel whatsoever. Four of them
   - db, storage-bucket, storage-dir, base-url - are fully LIVE on argv today
   (cmd/server_foreground.go:875-877, :889-891, :892-894, :2102) and would simply
   take effect if passed. There is no second source yet for anything to disagree
   with.

   ALL FIVE STAY RESERVED, AND NOT BY INERTIA. Before removing an entry, name
   where it lands instead - and none of them lands anywhere. They are not rendered
   ($setByChart), they do not select which configuration is loaded ($neverPassed),
   they are neither inert nor misnamed ($aliasOrIgnored), and they do not weaken
   authentication ($unsafeToPass). The harm is real and arrives on a known
   schedule: when the configuration phase lands, an argv copy added today becomes
   the silent winner over the channel that phase delivers, and nothing logs the
   disagreement. The asymmetry is what decides it - reserving now costs an
   operator a flag they have no reason to want and un-reserving later is a
   deliberate act with a place to record itself (see the closing paragraph),
   whereas reserving after the fact requires somebody to notice.

   Not verifiable against the rendered arguments, by construction - the rendered
   argument list is where these must NOT appear. Check them against the channel.

   NAME THE CHANNEL WHEN YOU ADD AN ENTRY, because it is not the same channel
   for every entry and the precedence differs. admin-emails, db, storage-bucket
   and storage-dir are destined for the settings file. base-url is destined for
   the SCION_SERVER_BASE_URL environment variable.

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
   argv value silently outranks the Secret-backed environment variable a later
   phase mounts.

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
{{- fail (printf "hub.args may not contain -%s: a later phase delivers this setting through another channel - the settings file, or for base-url the SCION_SERVER_BASE_URL environment variable - and argv silently wins over both, so an argv copy added now becomes a second and invisible source for one value the moment that channel lands, with nothing reporting the disagreement. This chart delivers none of them yet: today the flag would simply take effect, which is why this reservation cannot be inferred from the rendered output and is written down here instead." $flag) }}
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
