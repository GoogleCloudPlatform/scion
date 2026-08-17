#!/usr/bin/env bash
# C1: the cluster-scoped RBAC pair must not collide across namespaces.
#
# THE DEFECT. scion-hub.clusterRoleName built "<fullname>-<namespace>-agents"
# and then applied `trunc 63`. Truncation discards the namespace, which is the
# only part making the name unique, so releases of the same name in different
# namespaces rendered ONE ClusterRole and ONE ClusterRoleBinding name with
# different ServiceAccount subjects. By the Kubernetes API's documented apply
# behaviour the second apply updates the existing object, so the first release's
# cluster-wide secrets get/list and pods/exec create end up pointed at another
# namespace's ServiceAccount. NOBODY HERE HAS A CLUSTER; the apply consequence is
# documented behaviour and the COLLISION is what is measured. Filed by
# gd-regmis-rev, graded Critical by gke-deploy-lead, reproduced at merged main
# 1b3c9418 by gd-consumer.
#
# THE GATE. These objects render only when rbac.create AND
# runtime.listAllNamespaces are BOTH true, and both halves are pinned below.
#
# 🛑 WHAT A GREEN RUN OF THIS FILE DOES NOT MEAN. READ THIS BEFORE CITING IT.
# This change fixes C1: the TRUNCATION bucket. It does NOT fix C4 or C5, which
# are two further cluster-scoped collisions, both live, both Critical, and both
# PINNED AS PRESENT here - arm (o) and arm (q). Those arms are written to PASS
# WHILE THE DEFECT EXISTS. They are fences, not regression tests.
#
#   C1  needs a LONG release name - it fires only when the 63-byte cut lands
#   C4  release R beside release R-scion-hub, one namespace, len(R) 1..43
#   C5  the "-" between fullname and namespace is legal inside both operands,
#       so the joined string splits two ways. Renders at 33 bytes.
#
# 🔴 C4 AND C5 DO NOT TRUNCATE, AND THIS FIX IS GATED ON TRUNCATION. THE GATE IS
# KEYED ON THE ONE PROPERTY BOTH REMAINING CRITICALS LACK. So this PR closes the
# class that needs long release names and declares residuals on the two classes
# that fire on ordinary ones - `prod`, and a one-character release name. An
# operator reading "fixes the RBAC name collision" gets the rarest of the three,
# which is why the PR body says so explicitly and why this paragraph exists.
#
# ------------------------------------------------------------------------
# WHY THIS FILE ASSERTS EXACT NAME STRINGS AND NOT collide/distinct VERDICTS.
#
# It is a TRUNCATION BUCKET, not a pairwise collision (gd-p3-rev). Every
# namespace truncating to the same bytes lands on ONE object. Rendered, at
# release length 41, three namespaces produce one name:
#
#     team-alpha, team-alpha-staging, team-alpha-production
#       -> rrr...rrr-scion-hub-team-alpha   ALL THREE, one ClusterRoleBinding
#
# A FIXTURE ASSERTING "THESE TWO NAMES ARE EQUAL" PASSES ON A BUCKET OF TWO AND
# SAYS NOTHING ABOUT THREE - it is not a weaker test of the same thing, it is a
# test of a different, smaller thing, and the pair-shaped assumption is encoded
# in the assertion's own shape (gd-em, gd-regmis-rev). Pinning the exact rendered
# string per (release, namespace) captures the bucket for free and encodes no
# model at all.
#
# NO CLOSED FORM DESCRIBES WHEN THIS COLLIDES, AND THREE WERE TRIED. gd-em's
# ruling after the third was falsified. What is exact is the computation itself:
#
#     collide  <=>  trunc63trimSuffix(fullname + "-" + ns1 + "-agents")
#                == trunc63trimSuffix(fullname + "-" + ns2 + "-agents")
#
# and what a reviewer can hold, which no correction has touched:
#
#     ANY SET OF NAMESPACES WHOSE RENDERED NAMES SURVIVE TRUNCATION TO THE SAME
#     BYTES SHARES ONE ClusterRole AND ONE ClusterRoleBinding. THE CHART CANNOT
#     SEE THE OTHER NAMESPACES, SO IT CANNOT DETECT THIS AT RENDER TIME.
#
# THE HISTORY IS THE ARGUMENT FOR KEEPING MODELS OUT OF THE ASSERTIONS. Three
# successive inequalities of the form len(fullname) + 1 + sharedPrefix >= 63
# were published and each was falsified by widening the corpus, never by
# re-reasoning over the old one:
#
#   1. bare namespaces          agreed with 400 renders across two rigs.
#      Falsified: when one namespace is a proper prefix of another, the hyphen
#      beginning "-agents" continues the match past the shorter namespace's end.
#      NEITHER GRID HAD SUCH A PAIR, so no input existed that could make the
#      model say the other thing - 400 agreements measured the pair set.
#   2. with the "-agents" term  scored 0 misses over 16,994,448 exhaustive cases.
#      Falsified on real helm at 4 of 728: gd-regmis-rev's alphabet was {a,b,-},
#      IN WHICH THE STRING "agents" CANNOT BE SPELLED, so the class they had
#      identified by hand was unreachable in their own corpus by construction.
#      AN EXHAUSTIVE SEARCH IS ONLY EXHAUSTIVE OVER ITS ALPHABET.
#   3. any threshold against 63 misses the same class: one name untruncated at
#      62, the other truncated to 63 and pulled BACK to 62 by trimSuffix. Case
#      (m) below is that class, rendered.
#
# A CLOSED FORM OVER A STRING OPERATION IS A REIMPLEMENTATION OF IT, AND IT
# DIVERGES WHEREVER THE STRING OPERATION HAS A BRANCH THE ARITHMETIC DOES NOT
# MODEL. `trunc | trimSuffix` has exactly one such branch - does the cut land on
# a hyphen - and it defeated all three. Every misprediction was in the UNSAFE
# direction and none was an over-prediction, so C1's trigger set only ever grew.
#
# trimSuffix IS THE BUCKET-FORMING OPERATION, not defensive hygiene (gd-p3-rev).
# At release length 41, every team-alpha-SOMETHING namespace has "team-alpha-"
# as its surviving 11 characters, ending in a hyphen, so trimSuffix strips it
# and they all land on the SAME 62-character string. The non-members survive to
# 63 characters with no trailing hyphen and nothing is stripped. trimSuffix is
# the step that MERGES the survivors by erasing the character that still
# distinguished them.
#
# ONE MORE THING THE SUFFIX GREP TURNED UP (gd-p3-rev): "-agents" is a literal in
# FOUR places with no shared constant - here, and inline in rbac-role.yaml and
# rbac-rolebinding.yaml, where it is appended AFTER scion-hub.fullname has
# already truncated to 63 and so can reach 70 characters. Those are NAMESPACED
# objects and cannot collide across namespaces, so this is not a C1 finding; it
# is recorded because a future rename would diverge silently.
#
# THE FIRST COLLIDING RELEASE LENGTH IS NOT A CHART CONSTANT. It moves with the
# namespaces, and with fullnameOverride there is no release-length floor at all:
# case (e) collides at a ONE-CHARACTER release name.
# ------------------------------------------------------------------------
#
# ONE-DIRECTIONAL FIXTURES PASS TRIVIALLY HERE. Any change that lengthens or
# hashes the name makes two colliding cases differ - INCLUDING a change that
# mangles every name in the chart. So the negatives are load-bearing: cases (f)
# and (g) assert that names which were always fine are BYTE-IDENTICAL to what
# the chart produced before the fix, and (g) sweeps rather than samples.
#
# MUTATION TESTED. Each mutation was applied to a copy of the chart, the copy
# was asserted to actually DIFFER from the original (an unchanged tree is a
# harness error, never a pass), and the suite was run against it:
#
#   A  pre-fix helper restored              red  9   the defect itself
#   B  hash unconditionally                 red 10   caught by the negatives, (f)(g)
#   C  digest over the TRUNCATED string     red  9   would re-collide the bucket
#   D  digest 10 -> 12 characters           red 17   every digest pin
#   E  orphan disclosure deleted            red  4
#   F  symmetric "delete both" cleanup      red  2   the ClusterRole asymmetry
#   G  gate on release-name length          red 10   fullnameOverride walks past it
#   H  scion-hub.fullname made injective    red  5   arm (o), the C4 fence
#   I  digest arm made to end in "-agents"  red 15   collapses (p)'s two ranges
#   J  separator "-" changed to ":"         red 30   arm (q), the C5 fence
#
# H AND J ARE THE TWO THAT ARE SUPPOSED TO HAPPEN ONE DAY. H is C4's fix and J is
# C5's. When either lands, the matching fence arm must be RE-PINNED DELIBERATELY
# rather than repaired, and the upgrade path checked, because both rename objects
# that already exist in clusters.
#
# WHAT J MEASURED THAT IS WORTH KEEPING EVEN THOUGH J IS NOT APPLIED:
#   - it DOES close all three known C5 instances;
#   - arm (q)'s four name pins go red and its SUBJECT assertion stays green,
#     which is correct - a separator change renames objects, it does not change
#     which ServiceAccount they bind;
#   - arm (o)'s subject assertion ALSO stays green, correctly reporting that a
#     separator fix does NOT fix C4;
#   - and it is NOT a one-token change. The digest arm ends
#     `trunc 52 | trimSuffix "-"`, which stops matching, so digested names come
#     out as `...-scion-hub:-7e1159cc5b` with a dangling separator. Anyone taking
#     J must change the trimSuffix with it.
#
# FAILS CLOSED, same contract as the other scripts here.
set -u

EXPECTED_TOTAL=46
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
BASE=(--set image.repository=r --set hub.hubId=h
      --set rbac.create=true --set runtime.listAllNamespaces=true)

_missing=""
for _t in "$HELM" awk python3; do command -v "$_t" >/dev/null 2>&1 || _missing="${_missing} ${_t}"; done
if [ -n "$_missing" ]; then
  echo "HARNESS ERROR: required tool(s) not on PATH:${_missing}"
  echo "NOTHING WAS ANALYSED. This is not a passing run, and it is NOT a chart failure."
  echo "ASSERTIONS_EXECUTED=0"
  exit 2
fi

executed=0
failed=0
pass() { executed=$((executed+1)); echo "ok    $1"; }
fail() { executed=$((executed+1)); failed=$((failed+1)); echo "FAIL  $1"; }
meta_failure() {
  echo "META-FAILURE: $1" >&2
  echo "NOTHING BELOW THIS POINT WAS MEASURED. This is not a passing run." >&2
  echo "ASSERTIONS_EXECUTED=${executed}"
  exit 2
}

# Render one release into one namespace and echo the cluster-scoped names as
# "<clusterrole> <clusterrolebinding> <roleref> <subject-ns>".
# DECODED BY FIELD AND BY KIND, never grepped: the two objects share a helper, so
# a grep for the name would report one object's name twice and could not see the
# ClusterRole and the binding disagreeing.
names() { # names <release> <namespace> [extra helm args...]
  local rel="$1" ns="$2"; shift 2
  "$HELM" template "$rel" "$CHART" "${BASE[@]}" "$@" -n "$ns" 2>&1 | awk '
    /^kind: ClusterRole$/        { k="cr";  next }
    /^kind: ClusterRoleBinding$/ { k="crb"; next }
    /^kind: /                    { k="";    next }
    k=="cr"  && /^  name: / && cr==""  { cr=$2 }
    k=="crb" && /^  name: / && crb=="" { crb=$2 }
    k=="crb" && /^  name: / && crb!=""  { ref=$2 }
    k=="crb" && /^    namespace: /      { sns=$2 }
    END { printf "%s %s %s %s\n", (cr?cr:"-"), (crb?crb:"-"), (ref?ref:"-"), (sns?sns:"-") }'
}
crb_name() { names "$@" | awk '{print $2}'; }

rep() { python3 -c "import sys;print(sys.argv[1]*int(sys.argv[2]))" "$1" "$2"; }
# A release name of length $1 that CONTAINS the chart name, so fullname == it.
hubrel() { python3 -c "import sys;n=int(sys.argv[1]);print('scion-hub'+'x'*(n-9))" "$1"; }

# Pin one exact rendered name. THE EXPECTED STRING IS A LITERAL, never computed
# from the same expression the template uses - a computed expectation would
# re-derive the bug and agree with it.
pin() { # pin <label> <expected> <release> <namespace> [extra helm args...]
  local label="$1" want="$2"; shift 2
  local got; got="$(crb_name "$@")"
  [ -n "$got" ] || meta_failure "rendering '$1' in namespace '$2' produced NO cluster-scoped name at all. An empty string compares equal to another empty string, so every name assertion below would be measuring nothing twice."
  if [ "$got" = "$want" ]; then
    pass "$label -> $got"
  else
    fail "$label rendered '$got', want '$want'"
  fi
}

R41="$(rep r 41)"
R30="$(rep r 30)"
H30="$(hubrel 30)"
OVR="$(rep f 31)"

# --------------------------------------------------------------------------
# (a) THE BUCKET. Three namespaces, one release, three EXACT names.
# --------------------------------------------------------------------------
# Before the fix all three of these rendered the SAME 62-character string,
# rrr...rrr-scion-hub-team-alpha: one ClusterRole and one ClusterRoleBinding
# shared by three installs, with whichever applied last owning the subject.
# The three pins below are what replaced it. They are exact, so this arm also
# pins the digest for three inputs and cannot pass on a bucket of two.
#
# THE CUT LANDS ON A HYPHEN AND trimSuffix REMOVED IT, so the collided name was
# 62 characters, not 63 - trimSuffix PARTICIPATED in the collision rather than
# breaking it, and a fixture asserting a 63-character name would have missed it.
#     THE FIVE MEMBERS  (all rendered ONE 62-character name before the fix)
pin "(a) bucket member team-alpha" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-7e1159cc5b" "$R41" team-alpha
pin "(a) bucket member team-alpha-staging" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-69d41fd93d" "$R41" team-alpha-staging
pin "(a) bucket member team-alpha-production" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-4b4fe495fa" "$R41" team-alpha-production
pin "(a) bucket member team-alpha-dev" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-b18f53c091" "$R41" team-alpha-dev
pin "(a) bucket member team-alpha-x" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-1cd78f0ea9" "$R41" team-alpha-x

#     THE THREE NON-MEMBERS, WHICH ARE THE HALF THAT MAKES THIS A TEST.
# Pinning only members passes for an implementation that maps every namespace
# into one bucket (gd-p3-rev). These three were NEVER in it: pre-fix they
# rendered three distinct 63-character names, because their 11th surviving
# character is not a hyphen so trimSuffix had nothing to strip.
pin "(a) non-member team-alphabet" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-6a3bb19283" "$R41" team-alphabet
pin "(a) non-member team-alph" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-d4a79f8324" "$R41" team-alph
pin "(a) non-member team-beta" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-057bb4e726" "$R41" team-beta

# The eight expectations above must be eight DIFFERENT strings. A copy-paste
# would make this arm assert a bucket of one and still read as eight pins. This
# checks the FIXTURE, not the chart, so it is a META-FAILURE and not a failure.
_bucket_names="$(for _ns in team-alpha team-alpha-staging team-alpha-production \
                            team-alpha-dev team-alpha-x team-alphabet team-alph team-beta; do
                   crb_name "$R41" "$_ns"; done)"
_bucket_distinct="$(printf '%s\n' "$_bucket_names" | sort -u | /usr/bin/grep -c .)"
if [ "$_bucket_distinct" -ne 8 ]; then
  meta_failure "the eight bucket pins rendered only ${_bucket_distinct} distinct strings. Either the fix regressed or the expectations were pasted - either way the pins above are not measuring eight namespaces."
fi

# --------------------------------------------------------------------------
# (b) THE DIGEST IS PINNED TO ONE SPECIFIC NAME.
# --------------------------------------------------------------------------
# gd-em: if the digest INPUT or ALGORITHM changes in a later chart version,
# every affected install renames again and orphans a SECOND object - silently,
# because the operator read the NOTES disclosure once and will not expect it
# twice. This makes that change a red test instead of a second incident.
#
#     algorithm  sha256sum, first 10 hex characters
#     input      printf "%s/%s" <fullname> <namespace>, UNTRUNCATED
#
# Independently reproduced outside helm:
#     python3 -c "import hashlib;print(hashlib.sha256(
#       b'platform-hub-production-release-scion-hub/team-alpha-production'
#       ).hexdigest()[:10])"   ->  aa14af2e08
#
# IF THIS TEST IS RED YOU HAVE NOT BROKEN A TEST, YOU HAVE PROPOSED A MIGRATION.
pin "(b) digest pinned: input and algorithm unchanged" \
    "platform-hub-production-release-scion-hub-team-alpha-aa14af2e08" \
    platform-hub-production-release team-alpha-production

# --------------------------------------------------------------------------
# (c) KEYED ON FULLNAME, NOT ON RELEASE-NAME LENGTH.
# --------------------------------------------------------------------------
# TWO RELEASES OF THE SAME LENGTH, OPPOSITE BRANCHES OF THE FIX. The helm idiom
# skips appending the chart name when the release already contains it, so a
# 30-character release containing "scion-hub" has a 30-character fullname while
# a 30-character release without it has 40. In team-alpha-production the first
# fits and must render untouched; the second truncates and must be hashed.
# Any fixture keyed on release-name length is keyed on the wrong variable, and
# so is any FIX so keyed.
pin "(c) same length, contains chart name: fits, renders untouched" \
    "scion-hubxxxxxxxxxxxxxxxxxxxxx-team-alpha-production-agents" "$H30" team-alpha-production
pin "(c) same length, does not contain it: truncates, gets a digest" \
    "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-team-alpha-3a971c0691" "$R30" team-alpha-production

# --------------------------------------------------------------------------
# (d) THE PROPER-PREFIX PAIR. The cell that falsified the published rule.
# --------------------------------------------------------------------------
# team-alpha / team-alpha-staging at release length 41. The first published form
# of the rule predicted DISTINCT here (51 + 1 + 10 = 62) and helm rendered
# COLLIDE, because the "-" beginning "-agents" continues the match past the end
# of the shorter namespace. A base namespace beside its own suffixed siblings is
# one of the commonest layouts there is, and it was in neither grid.
#
# The length-40 control is the mechanism made visible: PRE-FIX it rendered two
# 63-character names differing in their LAST BYTE - "a" from "-agents" against
# the "s" of "staging". At 41 the truncation eats exactly that character and the
# two become one. Both were truncated, so post-fix both are digested; they are
# pinned here because 40 is the last length at which the pre-fix chart still
# told these two namespaces apart, and it did so by a single character.
# The 41 side is already pinned as the first member of (a); only the control
# needs pinning here.
_r40="$(rep r 40)"
pin "(d) length-40 control, ns team-alpha" \
    "${_r40}-scion-hub-t-045ea11c4a" "$_r40" team-alpha
pin "(d) length-40 control, ns team-alpha-staging" \
    "${_r40}-scion-hub-t-96a2bf006d" "$_r40" team-alpha-staging

# --------------------------------------------------------------------------
# (e) THERE IS NO RELEASE-NAME-LENGTH FLOOR. fullnameOverride, release "a".
# --------------------------------------------------------------------------
# scion-hub.fullname returns fullnameOverride DIRECTLY, so the operator sets
# len(fullname) outright and the release name drops out of the arithmetic
# entirely. A 31-character override collides across the 31-prefix namespace pair
# at a ONE-CHARACTER release name (gd-p3-rev, rendered pre-fix).
#
# THIS IS THE ARM THAT GOES RED IF ANYONE REINTRODUCES A RELEASE-LENGTH
# SHORTCUT INTO THE HELPER: such a gate would see a 1-character release and skip
# the digest, walking straight past the defect. The helper gates on
# len(assembled) > 63 - the truncation actually happening - which is the only
# condition that sees every path in.
pin "(e) fullnameOverride at a 1-char release, east" \
    "fffffffffffffffffffffffffffffff-platform-team-alpha-7d0120db6f" \
    a platform-team-alpha-production-east --set fullnameOverride="$OVR"
pin "(e) fullnameOverride at a 1-char release, west" \
    "fffffffffffffffffffffffffffffff-platform-team-alpha-183ca5add8" \
    a platform-team-alpha-production-west --set fullnameOverride="$OVR"

# --------------------------------------------------------------------------
# (f) THE NEGATIVE HALF: names that were always fine are BYTE-IDENTICAL.
# --------------------------------------------------------------------------
# Any change that hashes everything would satisfy every collision arm above.
# This is what separates a fix from a mangling.
pin "(f) a short release renders exactly what it always did" \
    "myhub-scion-hub-team-alpha-production-agents" myhub team-alpha-production

# --------------------------------------------------------------------------
# (g) THE SAME PROPERTY SWEPT RATHER THAN SAMPLED.
# --------------------------------------------------------------------------
# The invariant is not "short names are unchanged" but "a name changes IF AND
# ONLY IF the old code truncated it". Sweeping is what makes this a claim about
# the helper instead of about three release names somebody chose.
_sweep_n=0; _sweep_bad=""
for _ns in team-alpha-production platform-team-alpha-production-east prod-a a; do
  for _len in 9 15 20 25 30 35 40 45 50 53; do
    for _mk in rep hubrel; do
      if [ "$_mk" = rep ]; then _r="$(rep q "$_len")"; else _r="$(hubrel "$_len")"; fi
      case "$_r" in *scion-hub*) _f="$_r" ;; *) _f="${_r}-scion-hub" ;; esac
      _assembled="${_f}-${_ns}-agents"
      [ "${#_assembled}" -le 63 ] || continue
      _sweep_n=$((_sweep_n + 1))
      _got="$(crb_name "$_r" "$_ns")"
      [ "$_got" = "$_assembled" ] || _sweep_bad="${_sweep_bad} [${_r}@${_ns}: got '${_got}' want '${_assembled}']"
    done
  done
done
[ "$_sweep_n" -gt 0 ] || meta_failure "the untruncated sweep selected 0 cases, so 'no readable name changed' would be vacuously true. The corpus or the 63 bound is wrong; either way nothing was measured."
if [ -z "$_sweep_bad" ]; then
  pass "(g) all $_sweep_n renders whose assembled name fits in 63 chars are byte-identical to that assembled name (a name changes IF AND ONLY IF the old code truncated it)"
else
  fail "(g) renders that fit in 63 characters were altered anyway:${_sweep_bad}. The fix must touch only the names the old code truncated."
fi

# --------------------------------------------------------------------------
# (h) EVERY NAME THIS HELPER CAN PRODUCE IS A LEGAL OBJECT NAME.
# --------------------------------------------------------------------------
# The hashed branch cuts to 52 and appends 1 + 10, so it is 63 by construction -
# but "by construction" is the claim the old code also made. Swept over the
# lengths most likely to sit on the boundary.
_len_n=0; _len_bad=""
for _ns in team-alpha team-alpha-staging team-alpha-production platform-team-alpha-production-east a; do
  for _len in 1 20 40 41 50 53; do
    _r="$(rep r "$_len")"
    _got="$(crb_name "$_r" "$_ns")"
    [ -n "$_got" ] || meta_failure "length sweep rendered nothing for release length $_len in $_ns"
    _len_n=$((_len_n + 1))
    [ "${#_got}" -le 63 ] || _len_bad="${_len_bad} [${_got} is ${#_got}]"
    case "$_got" in -*|*-) _len_bad="${_len_bad} [${_got} starts or ends with a hyphen]" ;; esac
  done
done
[ "$_len_n" -gt 0 ] || meta_failure "the name-length sweep selected 0 cases"
if [ -z "$_len_bad" ]; then
  pass "(h) all $_len_n rendered names are 63 characters or fewer and are legal DNS subdomain names"
else
  fail "(h) illegal object names rendered:${_len_bad}"
fi

# --------------------------------------------------------------------------
# (i) BOTH OBJECTS, AND THE roleRef BETWEEN THEM.
# --------------------------------------------------------------------------
# The ClusterRole collides too, not only the binding, and roleRef points at the
# colliding ClusterRole. Fixing the binding and leaving the role would leave a
# second cluster-scoped object on the collided name, and a roleRef pointing at a
# name no ClusterRole has is a binding that grants nothing - which fails
# silently, in the safe direction, and would not be noticed.
_all="$(names "$R41" team-alpha-production)"
# shellcheck disable=SC2086  # intentional: splits $_all into $1 $2 $3 $4 by design
set -- $_all
if [ "$1" = "$2" ] && [ "$2" = "$3" ] && [ "$1" != "-" ]; then
  pass "(i) the ClusterRole, the ClusterRoleBinding and its roleRef all carry the same name ($1)"
else
  fail "(i) the ClusterRole, ClusterRoleBinding and roleRef names diverged: role='$1' binding='$2' roleRef='$3'. They come from one helper and must move together."
fi
if [ "$4" = "team-alpha-production" ]; then
  pass "(i) the binding's subject namespace is the release namespace ($4)"
else
  fail "(i) the binding's subject namespace is '$4', want 'team-alpha-production'"
fi

# --------------------------------------------------------------------------
# (j) THE CASE THAT WOULD DEFEAT THIS FIX, RENDERED.
# --------------------------------------------------------------------------
# Two identities whose READABLE parts truncate to the same 52 bytes. If the
# digest were taken over the truncated string instead of the full identity,
# these two would still collide. They are separated only by the digest, which is
# why the digest input is the UNTRUNCATED "fullname/namespace".
_z="$(rep z 41)"
_j1="$(crb_name "$_z" team-alpha-production)"
_j2="$(crb_name "$_z" team-alpha-prod-eu-west-1-cluster-b)"
_jp1="${_j1%-*}"; _jp2="${_j2%-*}"
if [ -z "$_j1" ] || [ -z "$_j2" ]; then
  meta_failure "the shared-prefix case rendered an empty name, so the comparison below would compare nothing to nothing"
elif [ "$_jp1" != "$_jp2" ]; then
  meta_failure "the two shared-prefix identities did not produce a shared readable prefix ('$_jp1' vs '$_jp2'), so this case is no longer testing what it claims to test - it would pass for the wrong reason."
elif [ "$_j1" != "$_j2" ]; then
  pass "(j) two identities sharing a truncated readable prefix ('$_jp1') are separated ONLY by the digest (${_j1##*-} vs ${_j2##*-})"
else
  fail "(j) two identities sharing a truncated readable prefix both rendered '$_j1'. The digest is not separating them, which is the one case this fix exists to handle."
fi

# --------------------------------------------------------------------------
# (k) THE GATE, BOTH HALVES.
# --------------------------------------------------------------------------
# Bounds the exposure claim in both directions: no cluster-scoped object exists
# unless both values are true.
_absent="$(names t team-alpha-production --set rbac.create=false)"
if [ "$_absent" = "- - - -" ]; then
  pass "(k) rbac.create=false renders no cluster-scoped objects at all"
else
  fail "(k) rbac.create=false still rendered cluster-scoped objects: $_absent"
fi
_absent2="$(names t team-alpha-production --set runtime.listAllNamespaces=false)"
if [ "$_absent2" = "- - - -" ]; then
  pass "(k) runtime.listAllNamespaces=false renders no cluster-scoped objects at all"
else
  fail "(k) runtime.listAllNamespaces=false still rendered cluster-scoped objects: $_absent2"
fi

# --------------------------------------------------------------------------
# (l) DETERMINISM.
# --------------------------------------------------------------------------
# Two renders of identical inputs must produce identical names. A name that
# moved between renders would orphan a cluster-scoped object on every apply.
_d1="$(crb_name "$R41" team-alpha-production)"
_d2="$(crb_name "$R41" team-alpha-production)"
if [ -n "$_d1" ] && [ "$_d1" = "$_d2" ]; then
  pass "(l) the hashed name is deterministic across renders of identical inputs"
else
  fail "(l) two renders of identical inputs produced '$_d1' and '$_d2'"
fi

# --------------------------------------------------------------------------
# (m) THE ASYMMETRIC CLASS: ONE SIDE IS NEVER TRUNCATED AT ALL.
# --------------------------------------------------------------------------
# ns=core and ns=core-agents at fullname 50. THE core SIDE ASSEMBLES TO 62 AND
# IS NEVER TRUNCATED; the core-agents side assembles to 69, is cut to 63, and
# trimSuffix pulls it BACK to 62 - onto the identical string. Rendered pre-fix,
# both were:
#
#     rrrr...rrrr-scion-hub-core-agents        (62, in BOTH namespaces)
#
# This is the class that falsified every threshold-against-63 rule, because one
# member never crosses the threshold. It also constrains the FIX rather than the
# model: B appends a digest only where truncation occurred, so the core side
# keeps its readable name and only core-agents is suffixed. They differ for that
# reason and no other.
#
# IF ANYONE REGATES THIS HELPER ON THE TRUNCATED LENGTH - which is always <= 63
# and so would never fire - OR OTHERWISE MAKES BOTH SIDES TAKE THE SAME BRANCH,
# THIS ARM GOES RED AND THE OTHERS DO NOT. In a chart whose objects are named
# "-agents", a namespace called "core-agents" is not a contrived string.
_r50="$(rep r 40)"
pin "(m) asymmetric class, ns=core: never truncated, keeps its readable name" \
    "${_r50}-scion-hub-core-agents" "$_r50" core
pin "(m) asymmetric class, ns=core-agents: truncated, so digested" \
    "${_r50}-scion-hub-c-2b8cd1d69d" "$_r50" core-agents

# --------------------------------------------------------------------------
# (n) THE ORPHAN DISCLOSURE IN NOTES.txt.
# --------------------------------------------------------------------------
# The fix does not remediate anyone who already has the defect - it only stops
# new ones - so an upgraded install keeps a live cluster-scoped object under the
# OLD name. NOTES tells the operator, and that text is a deliverable of this fix
# rather than a courtesy: it is the only thing standing between an operator and
# a dangling cluster-wide secrets grant.
#
# `helm template` does not emit NOTES and `helm install --dry-run` needs a
# cluster, so the text is rendered through the SAME template engine by wrapping
# it in a define and emitting it inside a ConfigMap. It must appear EXACTLY when
# the name changed - which is exactly when the old code truncated - and never
# otherwise, because on a fresh short-named install there is no orphan and a
# spurious "ACTION REQUIRED" teaches operators to ignore the real one.
_probe="$(mktemp -d)"
trap 'rm -rf "$_probe"' EXIT
cp -R "$CHART"/. "$_probe/" || meta_failure "could not copy the chart for the NOTES probe"
{ printf '{{- define "probe.notes" -}}\n'
  cat "$CHART/templates/NOTES.txt"
  printf '{{- end -}}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes-probe\ndata:\n  notes: |\n'
  printf '{{ include "probe.notes" . | indent 4 }}\n'
} > "$_probe/templates/zz-notes-probe.yaml" || meta_failure "could not write the NOTES probe template"

notes_has_disclosure() { # notes_has_disclosure <release> <namespace> [extra args...]
  local rel="$1" ns="$2"; shift 2
  local out
  out="$("$HELM" template "$rel" "$_probe" "${BASE[@]}" "$@" -n "$ns" \
         --show-only templates/zz-notes-probe.yaml 2>&1)"
  case "$out" in
    *"kind: ConfigMap"*) : ;;
    *) meta_failure "the NOTES probe did not render a ConfigMap for release '$rel' in '$ns'. Nothing was measured, and an absent disclosure would look identical to a broken probe. Output was: $out" ;;
  esac
  case "$out" in *"ACTION REQUIRED: THIS RELEASE"*) echo yes ;; *) echo no ;; esac
}

if [ "$(notes_has_disclosure "$R41" team-alpha-production)" = yes ]; then
  pass "(n) a release whose cluster-scoped name changed gets the orphan disclosure in NOTES"
else
  fail "(n) a release whose name changed got NO orphan disclosure. Its old ClusterRoleBinding is still in the cluster with cluster-wide secrets and pods/exec authority, and nothing tells the operator it is there."
fi
if [ "$(notes_has_disclosure myhub team-alpha-production)" = no ]; then
  pass "(n) a release whose name did NOT change gets no disclosure (there is no orphan to report)"
else
  fail "(n) a release that was never truncated was told it has an orphan. It does not, and a false ACTION REQUIRED teaches operators to ignore the real one."
fi
if [ "$(notes_has_disclosure "$R41" team-alpha-production --set rbac.create=false)" = no ]; then
  pass "(n) no disclosure when the cluster-scoped objects were never rendered at all"
else
  fail "(n) an install that renders no cluster-scoped objects was told to go delete one."
fi

# The disclosure must NAME the old object, not merely mention one. gd-em: it
# must be actionable, not informative - an operator cannot delete an object the
# chart declines to name. This checks the rendered legacy name is present.
_legacy_expected="rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr-scion-hub-team-alpha"
_notes_out="$("$HELM" template "$R41" "$_probe" "${BASE[@]}" -n team-alpha-production \
              --show-only templates/zz-notes-probe.yaml 2>&1)"
case "$_notes_out" in
  *"$_legacy_expected"*)
    pass "(n) the disclosure names the pre-fix object exactly ($_legacy_expected)" ;;
  *)
    fail "(n) the disclosure does not contain the pre-fix name '$_legacy_expected'. scion-hub.clusterRoleNameLegacy exists solely so NOTES can name the leftover object; without it the text can only say 'a previous object may remain', which is information rather than an instruction." ;;
esac

# THE TWO ORPHANED OBJECTS NEED OPPOSITE INSTRUCTIONS, and a symmetric
# "delete both" cleanup is the natural thing to write. The ClusterRoleBinding
# has a partial safety check - subjects[0].namespace - and deleting it under
# that check is the remediation. THE ClusterRole HAS NO SUCH FIELD: rendered for
# two members of one bucket it is BYTE-IDENTICAL, labels and rules alike, so
# nothing on it can say who else binds it. Worse, running the binding's
# inspection command against it prints EMPTY, which reads as "unused" and argues
# for the unsafe action. An unbound ClusterRole grants nothing, so it is safe to
# leave and dangerous to delete (gd-regmis-rev).
_del_binding="kubectl delete clusterrolebinding ${_legacy_expected}"
_del_role="kubectl delete clusterrole ${_legacy_expected}"
_says_binding=no; _says_role=no
case "$_notes_out" in *"$_del_binding"*) _says_binding=yes ;; esac
case "$_notes_out" in *"$_del_role"*) _says_role=yes ;; esac
if [ "$_says_binding" = yes ] && [ "$_says_role" = no ]; then
  pass "(n) the disclosure deletes the ClusterRoleBinding and explicitly leaves the ClusterRole"
else
  fail "(n) the disclosure's cleanup is wrong: deletes binding=${_says_binding}, deletes ClusterRole=${_says_role}. It must delete the binding and LEAVE the ClusterRole - the ClusterRole is byte-identical across every install in the bucket, so no command can tell the operator whether anyone still binds it, and the inspection command returns empty on it, which argues for deleting it."
fi

# (o) THE BOUNDARY OF THIS FIX: A SECOND, UNFIXED COLLISION CLASS.
# --------------------------------------------------------------------------
# THIS ARM PINS A DEFECT THAT IS STILL PRESENT. It is not a regression test for
# the fix; it is a fence around it, so that nobody reads a green suite as
# "cluster-scoped name collisions are solved".
#
# scion-hub.fullname branches on `contains $name .Release.Name`, so a release R
# and a release R + "-scion-hub" map to ONE fullname (gd-p3-rev). Every name in
# the chart is built from that fullname, so both releases render the SAME
# ClusterRole, ClusterRoleBinding, ServiceAccount, Service and Deployment - in
# the SAME namespace, at 30 bytes, with NOTHING TRUNCATED and a four-character
# release name.
#
# WHY THIS FIX CANNOT COVER IT, MEASURED RATHER THAN ARGUED. The two inputs to
# scion-hub.clusterRoleName are the fullname and the namespace, and both are
# identical for this pair - the ONLY differing input is .Release.Name. So no
# gate helps: gate on truncation and the digest never fires; hash
# unconditionally and both sides hash the same string. And feeding
# .Release.Name in would be worse than useless, because the ClusterRoleBinding's
# SUBJECT is the ServiceAccount, whose name comes from the same collapsed
# fullname and is also identical. Two distinct ClusterRoles would then bind ONE
# ServiceAccount: the grant stays shared and the only visible symptom - the
# duplicate name - disappears. THE RENAME WOULD HIDE THE COLLISION, NOT REMOVE
# IT. The fix belongs in scion-hub.fullname, it renames every object in the
# chart, and it is filed separately.
#
# gd-consumer rendered a second route into the same class (two fullnameOverride
# values agreeing on 63 characters, one namespace, 5 of 5 namespaced kinds
# identical) and graded it low as contrived. Same root cause, same fence: the
# non-injective operation is in fullname, upstream of everything measured above.
#
# IF THIS ARM GOES RED, DO NOT "REPAIR" IT BY EDITING THE EXPECTATIONS. Red here
# means scion-hub.fullname changed. That is the fix for the other defect and it
# is good news - re-pin these strings deliberately, and check whether the
# upgrade path for existing installs was handled, because that change renames
# every object in the chart.
pin "(o) release 'prod' in ns platform, nothing truncated" \
    prod-scion-hub-platform-agents prod platform
pin "(o) release 'prod-scion-hub' in ns platform STILL COLLIDES with release 'prod' after this fix - the contains idiom in scion-hub.fullname collapses them upstream of the cluster-scoped helper, and no gate in that helper can separate inputs it never sees" \
    prod-scion-hub-platform-agents prod-scion-hub platform
pin "(o) NEGATIVE CONTROL: 'prod-scion' does not contain the chart name, so the suffix IS appended and the name stays distinct - the rig can say 'distinct' on a near miss and is not merely reporting equality" \
    prod-scion-scion-hub-platform-agents prod-scion platform

# The subject is the load-bearing half of the argument above: it is what makes a
# per-release-name rename of the cluster-scoped objects cosmetic, not corrective.
sa_subject() { # sa_subject <release> <namespace>
  "$HELM" template "$1" "$CHART" "${BASE[@]}" -n "$2" 2>&1 | awk '
    /^kind: ClusterRoleBinding$/ { k=1; next }
    /^kind: /                    { k=0; next }
    k && /kind: ServiceAccount/  { s=1; next }
    s && /^    name: /           { print $2; exit }'
}
_o_sa_a="$(sa_subject prod platform)"
_o_sa_b="$(sa_subject prod-scion-hub platform)"
if ! { [ -n "$_o_sa_a" ] && [ -n "$_o_sa_b" ]; }; then
  meta_failure "(o) could not read the ClusterRoleBinding subject for one or both releases (got '$_o_sa_a' and '$_o_sa_b'); an empty-vs-empty comparison would report 'identical' having measured nothing"
fi
if [ "$_o_sa_a" = prod-scion-hub ] && [ "$_o_sa_b" = prod-scion-hub ]; then
  pass "(o) both releases bind the SAME ServiceAccount subject (prod-scion-hub), so renaming only the cluster-scoped objects would leave the grant shared"
else
  fail "(o) the ClusterRoleBinding subjects are now '$_o_sa_a' and '$_o_sa_b'. If they differ, scion-hub.fullname was fixed and this whole arm needs re-pinning; if they are unexpected strings, the subject wiring changed and the binding may no longer grant to the ServiceAccount the pod runs as."
fi

# And the disclosure must stay SILENT here. Nothing was renamed for this pair,
# so there is no orphan; claiming one would send an operator to delete a LIVE
# object under a procedure written for a different failure.
if [ "$(notes_has_disclosure prod-scion-hub platform)" = no ]; then
  pass "(o) no orphan disclosure for the contains-idiom collision - nothing was renamed, and the disclosure must not be repurposed for a defect whose remediation is different"
else
  fail "(o) the orphan disclosure fired for a release whose cluster-scoped name did not change. It would name a CURRENT, live object as a leftover and hand the operator a delete command for it."
fi

# (q) C5: THE SEPARATOR IS AMBIGUOUS. THIS FIX DOES NOT CLOSE IT. IT COULD.
# --------------------------------------------------------------------------
# 🛑 THIS ARM PINS A LIVE, UNFIXED, CROSS-NAMESPACE CLUSTER-SCOPED COLLISION.
# Like (o) it is a fence, not a regression test. UNLIKE (o), THIS ONE IS INSIDE
# THIS HELPER'S REACH, so the fence is around a decision that was deferred, not
# around a defect that lives somewhere else.
#
# The readable arm builds  printf "%s-%s-agents" $fullname $namespace  and "-" is
# LEGAL INSIDE BOTH COMPONENTS. So the rendered string can be split two ways, and
# two different installs land on it:
#
#   release 'a'                        ns 'team-alpha-x-b'  -> a-scion-hub-team-alpha-x-b-agents
#   release 'a-scion-hub-team-alpha-x' ns 'b'               -> a-scion-hub-team-alpha-x-b-agents
#
# 33 bytes against a bound of 63. NOTHING TRUNCATES. No override. One-character
# release name. Present in the pre-fix chart and present after this fix, so it is
# not a regression introduced here - it is a case this fix does not reach.
#
# WHY IT IS NOT ANOTHER (o), AND THE DISCRIMINATOR IS THE SUBJECT:
# in (o) both installs bind ONE ServiceAccount, which is what makes a rename
# cosmetic there. HERE THE SUBJECTS DIFFER - two ServiceAccounts, two namespaces,
# contending for one ClusterRoleBinding name. Last writer wins and the loser's
# hub silently loses cluster-wide secrets get/list/create/delete.
#
# 📌 FILED AS C5, SEPARATELY FROM C1, ON gd-em's RULING - AND I ARGUED THE OTHER
# WAY AND LOST ON A POINT WORTH KEEPING. I claimed C5 belongs to C1 because the
# subjects differ, so a rename at this site WOULD be a real fix. gd-em accepted
# that reasoning and rejected the conclusion on one word: COULD.
#
#   A SEVERITY CLASS IS NOT A CLAIM ABOUT WHAT A HYPOTHETICAL REMEDY COULD CLOSE.
#   IT IS A CLAIM ABOUT WHAT THE REMEDY BEING BUILT DOES CLOSE.
#
# This change is GATED ON TRUNCATION. C5 renders at 33 bytes against a bound of
# 63, so the gate never fires on C5's inputs, no digest is applied, and the two
# names stay identical. Filing C5 under C1 would make this PR's approval imply
# coverage it does not have.
#
# 🛑 AND THE PROPERTY C4 AND C5 SHARE IS THE REAL FINDING, NOT EITHER DEFECT:
#   BOTH ARE INVISIBLE TO ANY TRUNCATION-GATED REMEDY, BECAUSE NEITHER TRUNCATES.
#   THE GATE IS KEYED ON THE ONE PROPERTY BOTH LIVE CRITICALS LACK.
#
# ⚠️ THE CLASSIFICATION IS CONTINGENT ON THE GATE, AND THAT IS RECORDED HERE
# BECAUSE A CONTINGENT RULING READ LATER LOOKS LIKE AN ABSOLUTE ONE. If anyone
# ever removes the truncation gate and applies the digest unconditionally, the
# argument above stops holding and C5 merges back into C1. Whoever does that must
# revisit C5's filing in the same change. Do not treat "C5 is separate" as a fact
# about the defect; it is a fact about the shape of this remedy.
#
# THE FIX IS KNOWN AND WAS NOT TAKEN UNILATERALLY. The digest arm ALREADY gets
# this right: it hashes  printf "%s/%s" $fullname $namespace  and "/" cannot occur
# in either component. Only the readable arm is ambiguous. Separating with ":"
# closes all three known instances, and the reason it works is a measured
# property from both ends rather than a preference:
#
#   ":" is ILLEGAL in a helm release name  - gd-consumer ran it: release "a:b" is
#       rejected, "ab" accepted as the control that proves the rig can say yes
#   ":" is ILLEGAL in a DNS-1123 label     - so it cannot occur in a namespace
#   ":" is LEGAL in an RBAC object name    - gd-p3-rev executed apimachinery
#       v0.29.0: these names go through IsValidPathSegmentName, which is why
#       system:masters is legal
#
# ILLEGAL IN BOTH INPUTS, LEGAL IN THE OUTPUT. That is what makes the split
# unambiguous BY CONSTRUCTION rather than by luck - the same property "/" already
# gives the digest arm.
#
# 🔴 MEASURED COST, AND IT IS WHY THIS WAS NOT TAKEN: it renames every install,
# 42 of 42 sampled, ZERO unchanged. A breaking rename for installs that never had
# the defect. gd-em declined to reverse the lead's cost ruling mid-build and has
# escalated the C4+C5 pair to gke-deploy-lead. Recorded here rather than slipped
# in, and my own summary of the position stands unsoftened: I DO NOT THINK A
# CHEAP FIX EXISTS - THE AMBIGUITY IS PRESENT FOR NEARLY EVERY INPUT, NOT ONLY
# FOR COLLIDING ONES.
#
# ⚠️ ONE NOTATION WARNING, FROM gd-consumer's RETRACTION OF THEIR OWN TABLE, AND
# IT IS THE TRAP THIS ARM EXISTS TO DOCUMENT. The separating identity is a TUPLE
# of (Release.Name, Namespace) - joined by a separator illegal in both operands,
# or hashed as a tuple. NEVER a bare concatenation. gd-consumer measured a tuple
# (NUL-joined) and published a column headed "Release.Name + Namespace"; anyone
# implementing that heading writes printf "%s-%s" AND REINTRODUCES EXACTLY THIS
# DEFECT. When the subject is string ambiguity, the notation is part of the
# artefact.
#
# IF THIS ARM GOES RED, THAT IS THE GOOD OUTCOME: the separator was disambiguated.
# Re-pin deliberately, and handle the upgrade path, because it renames broadly.
pin "(q) release 'a' in ns 'team-alpha-x-b' - nothing truncated, no override" \
    a-scion-hub-team-alpha-x-b-agents a team-alpha-x-b
pin "(q) release 'a-scion-hub-team-alpha-x' in ns 'b' renders the SAME cluster-scoped name as release 'a' in ns 'team-alpha-x-b' - the fullname/namespace separator '-' is legal inside both operands, so the joined string splits two ways and this fix does not disambiguate it" \
    a-scion-hub-team-alpha-x-b-agents a-scion-hub-team-alpha-x b
pin "(q) NEGATIVE CONTROL: same release, ns 'c' instead of 'b' - the rig can say 'distinct' on a one-character change and is not merely echoing the expectation" \
    a-scion-hub-team-alpha-x-c-agents a-scion-hub-team-alpha-x c
pin "(q) NEGATIVE CONTROL: release 'zz' into the same absorbed namespace text stays distinct, so the collision is not an artefact of the namespace alone" \
    zz-scion-hub-team-alpha-x-b-agents zz team-alpha-x-b

# THE SUBJECT MEASUREMENT. This is what separates C5 from C4 and it is the reason
# C5 is fixable here: the two installs are genuinely different identities.
_q_sa_a="$(sa_subject a team-alpha-x-b)"
_q_sa_b="$(sa_subject a-scion-hub-team-alpha-x b)"
if ! { [ -n "$_q_sa_a" ] && [ -n "$_q_sa_b" ]; }; then
  meta_failure "(q) could not read the ClusterRoleBinding subject for one or both releases (got '$_q_sa_a' and '$_q_sa_b'); two empty strings compare equal and would report 'identical subjects', which is C4's signature and the opposite of this arm's finding"
fi
if [ "$_q_sa_a" != "$_q_sa_b" ]; then
  pass "(q) the two colliding installs bind DIFFERENT ServiceAccounts ('$_q_sa_a' and '$_q_sa_b') in different namespaces - unlike arm (o), so a disambiguating rename at this site WOULD be a real fix. That makes C5 FIXABLE HERE; it does NOT make it fixed here, and gd-em ruled it a SEPARATE Critical because this change is gated on truncation and C5 does not truncate"
else
  fail "(q) both installs now bind the same ServiceAccount subject ('$_q_sa_a'). That is arm (o)'s signature, not this arm's, and it would mean scion-hub.fullname changed underneath this pair - re-derive which class this is before touching the expectations."
fi

# (p) WHICH ARMS OF THE GATE FIRE, AND WHY THE MIDDLE CELL IS NOT AN ACCIDENT.
# --------------------------------------------------------------------------
# gke-deploy-lead's three-cell table, which is a fact about THIS helper's arms
# and not a model of helm's string operations:
#
#   BOTH sides truncated     gate fires on both      both digested, digests differ
#   ONE side truncated       gate fires on one       one readable, one digested
#   NEITHER side truncated   gate fires on neither   readable vs readable
#
# THE MIDDLE CELL WAS PUT TO ME AS "DISAMBIGUATES BY ACCIDENT" - the two installs
# take different arms, so they differ for a reason that is not the reason the
# gate was written, and the next author to touch the helper deletes the accident.
# 100% of gd-regmis-rev's 167-case residual lives here, so it matters.
#
# IT IS NOT AN ACCIDENT, AND THE REASON IS A RANGE PROPERTY THIS ARM PINS:
#
#   readable arm ALWAYS ends with the literal  "-agents"
#   digest arm   ALWAYS ends with "-" + 10 characters of sha256sum, i.e. HEX
#   "agents" CONTAINS g, n, s, t - AND NONE OF THOSE ARE HEX DIGITS
#
# So no readable name can equal any digested name, at any length, for any input.
# The two arms have DISJOINT RANGES, and a cross-arm collision is not unlikely,
# it is unconstructable. That is a design property, so it is pinned here rather
# than labelled as luck - a comment saying "accidental" invites removal, and a
# failing assertion does not.
#
# THE THIRD CELL - "neither side truncated" - IS NOT ONE DEFECT. IT HOLDS TWO,
# AND THEY HAVE OPPOSITE ANSWERS. Do not collapse them:
#
#   C4, arm (o)   both inputs to this helper are IDENTICAL, and both installs
#                 bind the SAME ServiceAccount. No gate here can separate inputs
#                 it never sees, and a rename would remove the only symptom while
#                 leaving the grant shared. NOT FIXABLE HERE, and fixing the name
#                 would be worse than the defect.
#   C5, arm (q)   the inputs DIFFER and the two installs bind DIFFERENT
#                 ServiceAccounts in DIFFERENT namespaces. The names collide only
#                 because this helper joins fullname and namespace with "-", which
#                 is legal inside both. FIXABLE HERE, and currently UNFIXED.
#
# The discriminator is the ClusterRoleBinding SUBJECT, not the truncation state.
# Both arms measure it, because "neither side truncated" was the cell everyone
# reasoned about and nobody rendered.
_shapes_all=0; _shapes_readable=0; _shapes_digest=0; _shapes_neither=0; _shapes_both=0
_shapes_names=""
for _rl in 1 4 10 20 30 40 41 42 50 53; do
  _r="$(printf 'r%.0s' $(seq 1 "$_rl"))"
  for _ns in a core core-agents team team-alpha team-alpha-staging \
             team-alpha-production agents x-agents platform prod; do
    _n="$(crb_name "$_r" "$_ns")"
    if ! { [ -n "$_n" ] && [ "$_n" != "-" ]; }; then
      meta_failure "(p) release length ${_rl} in namespace ${_ns} rendered no cluster-scoped name; a sweep that renders nothing classifies nothing and would report every shape as absent"
    fi
    _shapes_all=$((_shapes_all+1)); _shapes_names="${_shapes_names}${_n}
"
    _is_r=no; _is_d=no
    case "$_n" in *-agents) _is_r=yes ;; esac
    printf '%s' "$_n" | grep -Eq -- '-[0-9a-f]{10}$' && _is_d=yes
    if [ "$_is_r" = yes ] && [ "$_is_d" = yes ]; then _shapes_both=$((_shapes_both+1))
    elif [ "$_is_r" = yes ]; then _shapes_readable=$((_shapes_readable+1))
    elif [ "$_is_d" = yes ]; then _shapes_digest=$((_shapes_digest+1))
    else _shapes_neither=$((_shapes_neither+1)); fi
  done
done
[ "$_shapes_all" -ge 100 ] || meta_failure "(p) the shape sweep rendered only ${_shapes_all} names; it is sized to cover both arms and a short sweep can be all one arm"

# BOTH ARMS MUST BE EXERCISED. A sweep that never digests would report perfect
# disjointness having only ever seen one range.
if [ "$_shapes_readable" -gt 0 ] && [ "$_shapes_digest" -gt 0 ]; then
  pass "(p) the sweep exercises BOTH arms (${_shapes_readable} readable, ${_shapes_digest} digested of ${_shapes_all}) - disjointness below is measured over two populated ranges, not one"
else
  fail "(p) the sweep hit only one arm (${_shapes_readable} readable, ${_shapes_digest} digested). Disjointness cannot be measured against an empty range, and this arm would pass while proving nothing."
fi
if [ "$_shapes_both" -eq 0 ]; then
  pass "(p) NO rendered name matches both shapes - the readable and digest arms have disjoint ranges"
else
  fail "(p) ${_shapes_both} name(s) end in BOTH '-agents' and 10 hex characters. The arms can now produce the same string, so a name that took the readable arm could collide with one that took the digest arm - which is the cross-arm collision the gate is assumed to make impossible."
fi
if [ "$_shapes_neither" -eq 0 ]; then
  pass "(p) EVERY rendered name has one of the two shapes - the classification is exhaustive, so no name escaped both counters"
else
  fail "(p) ${_shapes_neither} name(s) matched neither shape. The helper has grown a third output form, and the disjointness argument above covers only two."
fi
# 🛑 READ THE SCOPE OF THE NEXT ASSERTION BEFORE YOU TRUST IT. IT IS NOT AN
# INJECTIVITY RESULT AND IT MUST NOT BE CITED AS ONE.
#
# This sweep varies release LENGTH and draws namespaces from a fixed list. It
# NEVER VARIES WHERE THE BOUNDARY BETWEEN RELEASE AND NAMESPACE FALLS. A
# collision of the C5 shape - see arm (q) - needs one release name to absorb the
# other install's namespace text, and THAT IS NOT EXPRESSIBLE IN THIS CORPUS AT
# ANY LENGTH. So a clean result here says the swept cells do not collide with
# each other; it says nothing about whether the readable arm is injective.
#
# It is not. Arm (q) renders a collision this sweep cannot reach.
#
#   AN EXHAUSTIVE SEARCH IS ONLY EXHAUSTIVE OVER ITS ALPHABET. That sentence is
#   in this file's header, and I still read an injectivity claim off a corpus
#   with the counterexample constructed out of it. The count below is a claim
#   about cost, not about coverage.
_shapes_uniq="$(printf '%s' "$_shapes_names" | sort -u | grep -c .)"
if [ "$_shapes_uniq" -eq "$_shapes_all" ]; then
  pass "(p) the ${_shapes_all} swept (release, namespace) pairs render distinct names FROM EACH OTHER, across both arms and the boundary between them - this is a statement about THIS corpus and NOT about injectivity of the helper; see arm (q) for a collision this sweep cannot express"
else
  fail "(p) the sweep rendered ${_shapes_all} names but only ${_shapes_uniq} distinct ones. Distinct inputs share a cluster-scoped object."
fi
# The reason, pinned as a fact rather than left in a comment: if the digest ever
# stops being hex - base32, base36, a word list - the ranges can overlap again.
if printf '%s' "agents" | grep -q '[^0-9a-f]'; then
  pass "(p) 'agents' contains non-hex characters, so a hex digest suffix can never spell it - this is WHY the two arms cannot collide, and it stops holding if the digest encoding changes"
else
  fail "(p) 'agents' is spellable in hex. The readable and digest arms could then produce the same string and the disjointness above is coincidental."
fi

echo "ASSERTIONS_EXECUTED=${executed}"
if [ "$executed" -ne "$EXPECTED_TOTAL" ]; then
  echo "HARNESS ERROR: expected ${EXPECTED_TOTAL} assertions, executed ${executed}." >&2
  echo "A case was added or removed without updating EXPECTED_TOTAL, or a case exited early." >&2
  exit 2
fi
if [ "$failed" -ne 0 ]; then
  echo "FAILED ${failed}/${executed}"
  exit 1
fi
echo "PASSED ${executed}/${executed}"
