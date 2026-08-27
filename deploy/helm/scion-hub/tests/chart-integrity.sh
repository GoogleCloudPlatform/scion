#!/usr/bin/env bash
#
# chart-integrity.sh -- the positive twin for .helmignore breadth.
#
# WHY THIS EXISTS.
#
# `.helmignore` is applied when Helm loads a chart DIRECTORY, not only when it packages one.
# An over-broad pattern therefore silently removes files from every `helm template`, `helm lint`
# and `helm package` invocation at once. Measured at 721fc77:
#
#   ignore templates/service.yaml -> `helm template` catches it (5 kinds -> 4)
#                                    `helm lint --strict` is BLIND (0 chart(s) failed)
#   ignore values.schema.json     -> `helm template` is BLIND (still 5 kinds, byte-identical)
#                                    `helm lint --strict` is BLIND
#
# The second row is why this file exists. Deleting the values contract makes the chart accept
# MORE and render IDENTICALLY, so the guard's removal is invisible to the guard's own success
# criterion. Every positive signal stays green while the protection is gone.
#
# The measurement this replaces was "helm package emits 0 files under tests/" -- a bare negative.
# It says what is absent and nothing about what survived. This script asserts what survived.
#
# Provenance: written by gd-p0-rev-2, adopted here whole. The 25 assertions and
# their messages are rev-2's; the changes made on adoption are the tool-presence
# arm, the ASSERTIONS_EXECUTED line for run-all.sh, and the inequality below.
#
# CONTRACT (shared with reserved-flags.sh and update-strategy.sh):
#   exit 0 -- all EXPECTED_TOTAL assertions ran and passed
#   exit 1 -- an assertion ran and failed
#   exit 2 -- the number of assertions executed was not EXACTLY EXPECTED_TOTAL,
#             or a required tool was absent so NOTHING WAS ANALYSED. Short and
#             long are both failures: a long run means assertions were added
#             without the number being committed in the same diff.
# Rule 9: assert the presence of N successes, never the absence of a failure.

set -u -o pipefail

HELM="${HELM:-helm}"
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# Minimum values that make the chart render. All four are `required` or enforced by a `fail`,
# with no default, by design.
# hub.baseUrl became required in Phase 1: the hub's own fallback is a localhost URL that agents
# cannot reach and that disables the session cookie's Secure attribute, so there is no safe
# default. Before it was added here every render in this file returned empty, which section D's
# empty-render guard reported correctly rather than scoring it as zero channels.
#
# auth.sessionSecret became required in the session-secret phase, for the same reason and with
# the same consequence if it is left out: scion-hub.assertSessionSecret fails the render, so
# every BASE render in this file returns an ERROR STRING rather than manifests, and sections A
# and B accuse the chart of dropping templates it never dropped. The chart will not default it -
# a generated secret rotates on every upgrade - so the harness supplies one, exactly as it
# supplies a base URL.
BASE_NO_SECRET=(--set image.repository=example.invalid/scion-hub --set hub.hubId=h --set hub.baseUrl=https://h.example.invalid)
BASE=("${BASE_NO_SECRET[@]}" --set auth.sessionSecret=chart-integrity-not-a-real-secret)

# HELD AT 26 ON PURPOSE, AND THIS SCRIPT THEREFORE EXITS 2.
#
# The true Phase 1 figure is 35: 26 as adopted, +2 kinds (ConfigMap, Secret) in
# section B, +2 files (configmap-env.yaml, secret-settings.yaml) in section C,
# +5 in section E (the signing-key flip). The session-secret phase adds +3: E6
# and E7 in the inverted section E, and one more in section C, because that
# section asserts one file PER ENTRY in EXPECTED_FILES and secret-session.yaml
# is a seventeenth entry. -> 38. E8 then adds the last one, refusing a pod
# annotation derived from the session secret. -> 39.
# Nothing FAILS -- every assertion the chart is accused by passes. The only red
# is this number.
#
# gd-em is holding every numeric delta against 8cc8d9b while three blocking
# findings are open against it and EXPECTED_ASSERTIONS may still move. Exit 2 in
# this harness means "the run is not evidence", which is exactly the status of a
# count that has been deliberately not committed -- so leaving it wrong is the
# honest encoding of the hold, and bumping it would be the defeat gd-p0-dev
# warned about. Lifting the hold is a two-line change: 26 -> 35 here, and
# 107 -> 116 in run-all.sh, in one diff.
EXPECTED_TOTAL=49

# TOOL-PRESENCE ARM. A MISSING TOOLCHAIN MUST NOT BE REPORTED AS A BROKEN CHART.
# Without this every helm invocation fails, every assertion fails, and the output
# accuses the chart of dropping templates when the truth is that helm is not
# installed. Found by the first person to run this suite who was not its author,
# in a container without helm, in four minutes. A mutation suite inherits its
# author's environment, so the environment is the one variable it cannot mutate
# from the inside - the same shape as axis (d), answerable only from outside.
# "Nothing was analysed" is a THIRD outcome, distinct from clean and from failing,
# and it exits 2 with the other harness errors rather than 1.
_missing=""
for _t in "$HELM" tar mktemp; do command -v "$_t" >/dev/null 2>&1 || _missing="${_missing} ${_t}"; done
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

# ---------------------------------------------------------------------------
# A. The values contract is present AND enforcing.  (3 assertions)
#
# Asserting the *error text* rather than merely a non-zero exit is deliberate: a `fail` in
# _helpers.tpl also exits non-zero, so "it was rejected" alone does not prove the schema is
# what rejected it. This is the difference between testing the guard and testing any guard.
# ---------------------------------------------------------------------------

schema_rejects() { # $1 = --set expr, $2 = leaf name (the key the schema should reject)
  local out rc
  out="$("$HELM" template t "$CHART" "${BASE[@]}" --set "$1" 2>&1)"; rc=$?
  # Match the key name in the schema error. Helm ≤3.16 prints
  #   "- (root): Additional property bogusKey is not allowed"
  # while Helm ≥3.17 prints
  #   "- at '': additional properties 'bogusKey' not allowed"
  # Both formats contain the key name; matching on that is portable across
  # versions. The render must also fail (non-zero exit); a successful render
  # means the schema is absent or not enforcing.
  if [ "$rc" -ne 0 ] \
     && printf '%s' "$out" | grep -qi "additional propert" \
     && printf '%s' "$out" | grep -qF "$2"; then
    pass "schema rejects unknown key ($1) naming '$2'"
  else
    fail "schema did NOT reject unknown key ($1) naming '$2' -- values.schema.json missing or not enforcing"
  fi
}

schema_rejects "bogusKeyNotInSchema=1" "bogusKeyNotInSchema"
schema_rejects "hub.bogusNested=1"     "bogusNested"

# POSITIVE TWIN. Without this, a schema that rejects EVERYTHING passes both cases above.
if "$HELM" template t "$CHART" "${BASE[@]}" >/dev/null 2>&1; then
  pass "positive twin: valid values still render"
else
  fail "positive twin: valid values were REJECTED -- schema is over-strict, not merely present"
fi

# ---------------------------------------------------------------------------
# B. The rendered manifest set is intact.  (8 assertions: 7 kinds + the total)
# Catches an over-broad pattern that reaches templates/.
# ---------------------------------------------------------------------------

RENDER="$("$HELM" template t "$CHART" "${BASE[@]}" 2>/dev/null)" || RENDER=""

# ConfigMap and Secret are Phase 1's, and they were the two kinds this loop did
# not name -- so the manifests the phase added were the only ones nothing
# asserted by kind, which is the general shape of how a per-phase enumeration
# goes short.
# NOW A KIND -> COUNT TABLE, BECAUSE THE SESSION-SECRET PHASE BROKE THE
# ASSUMPTION THE OLD ONE ENCODED. This was a flat list of kinds, and the total
# was derived from its LENGTH - which silently assumed exactly one manifest per
# kind. The base render now contains TWO Secrets, the settings Secret and the
# session Secret, so that assumption is false and the derivation was off by one.
#
# The twin's own comment named this exact case in advance - "a template that
# started emitting a second Secret ... satisfies every iteration above" - so the
# fix keeps its property rather than bumping a number past it. The count moves
# INTO the table, the per-kind loop asserts an exact count instead of mere
# presence, and the total is still derived from one structure rather than
# maintained beside it. Strictly stronger than what it replaces: a stray second
# Deployment is now caught by its own kind's assertion and not only in aggregate.
declare -A EXPECTED_KINDS=(
  [ConfigMap]=1
  [Deployment]=1
  [Role]=1
  [RoleBinding]=1
  [Secret]=2
  [Service]=1
  [ServiceAccount]=1
)

_kind_total=0
for k in $(printf '%s\n' "${!EXPECTED_KINDS[@]}" | sort); do
  want="${EXPECTED_KINDS[$k]}"
  _kind_total=$((_kind_total + want))
  got="$(printf '%s\n' "$RENDER" | grep -cx "kind: $k")"
  if [ "$got" -eq "$want" ]; then
    pass "render contains ${want}x kind: $k"
  else
    fail "render contains ${got}x kind: $k, expected ${want} -- template dropped (check .helmignore breadth) or an unexpected manifest appeared"
  fi
done

# THE TWIN, AND IT IS NOT THE LOOP AGAIN. The loop says each named kind appears
# the expected number of times; it is silent on any kind present that is NOT
# named at all. A stray manifest of some kind nobody listed satisfies every
# iteration above.
kinds="$(printf '%s\n' "$RENDER" | grep -c '^kind:')"
if [ "$kinds" -eq "$_kind_total" ]; then
  pass "render emits exactly ${_kind_total} manifests"
else
  fail "render emits ${kinds} manifests, expected ${_kind_total} -- add it to EXPECTED_KINDS deliberately, or find out what it is"
fi

# ---------------------------------------------------------------------------
# C. The packaged chart carries what it must.  (18 assertions; D adds 1 more)
#
# Separate from B because `helm package` and `helm template` do not fail together:
# values.schema.json is absent from B's signal entirely, and VALIDATION.md is absent from
# both unless asserted here.
# ---------------------------------------------------------------------------

EXPECTED_FILES=(
  scion-hub/.helmignore
  scion-hub/Chart.yaml
  scion-hub/VALIDATION.md
  scion-hub/values.schema.json
  scion-hub/values.yaml
  scion-hub/templates/NOTES.txt
  scion-hub/templates/_helpers.tpl
  scion-hub/templates/configmap-env.yaml
  scion-hub/templates/deployment.yaml
  scion-hub/templates/rbac-clusterrole.yaml
  scion-hub/templates/rbac-clusterrolebinding.yaml
  scion-hub/templates/rbac-role.yaml
  scion-hub/templates/rbac-rolebinding.yaml
  scion-hub/templates/secret-session.yaml
  scion-hub/templates/secret-settings.yaml
  scion-hub/templates/service.yaml
  scion-hub/templates/serviceaccount.yaml
)

pkgdir="$(mktemp -d)"
trap 'rm -rf "$pkgdir"' EXIT

if "$HELM" package "$CHART" -d "$pkgdir" >/dev/null 2>&1; then
  listing="$(tar tzf "$pkgdir"/*.tgz | grep -vE '/$' | sort)"
else
  listing=""
fi

for f in "${EXPECTED_FILES[@]}"; do
  if printf '%s\n' "$listing" | grep -qxF "$f"; then
    pass "package contains $f"
  else
    fail "package is MISSING $f -- .helmignore is too broad, or packaging failed"
  fi
done

# ci/ is ignored by design (fixture values must never be mistaken for defaults inside a
# packaged chart) and tests/ is ignored because helm package does not preserve the
# executable bit. Both are negatives; each is only meaningful beside the 14 positives above.
# 🔴 THE EMPTY GUARD COMES FIRST AND IS THE REASON THIS ASSERTION MEANS ANYTHING.
# Without it this is a bare negative satisfied by an empty listing: helm absent
# or helm package failing leaves listing="", nothing matches the pattern, and it
# prints "ok package excludes ci/ and tests/" - the single assertion in this
# script that went green on a machine with no helm, while the twenty-five around
# it went red. Found by rev-2 in its own script, and it is the same shape as the
# bare negative it had filed against me an hour earlier.
#
# It also becomes strictly more load-bearing as the exclusion list grows: a later
# phase adding golden/ and hack/ to .helmignore doubles the number of things this
# one line certifies, and an empty listing would certify all four.
# Phase 1 added /golden/ and /hack/ to .helmignore, so this one line now certifies FOUR
# exclusions, exactly as the paragraph above predicted it would. Each pattern is ANCHORED with a
# leading slash: rev-2 measured a bare `hack/` swallowing templates/hack/nested.txt, and `tests/`
# is the dangerous one to leave bare because templates/tests/ is where Helm's own test hooks
# conventionally live. The anchoring and this assertion land in the same diff, because a pattern
# change without its assertion is the shape this suite exists to catch.
_excluded=""
for _d in ci tests golden hack; do
  printf '%s\n' "$listing" | grep -qE "^scion-hub/${_d}/" && _excluded="${_excluded} ${_d}/"
done
if [ -z "$listing" ]; then
  fail "package exclusion check: the listing is EMPTY, so nothing was examined -- helm package failed or produced no tarball"
elif [ -n "$_excluded" ]; then
  fail "package contains${_excluded} -- these are ignored by design; check .helmignore"
else
  pass "package excludes ci/, tests/, golden/ and hack/"
fi

# THE OTHER DIRECTION, AND IT IS NOT THE SAME ASSERTION. The check above says the four
# directories did not survive packaging. It cannot tell "the pattern excluded them" from "the
# pattern excluded them AND half the chart" -- an over-broad /hack/ that also ate
# templates/hack-something.yaml passes it. The EXPECTED_FILES loop above is that twin: it names
# every file that MUST survive. This is only recorded here so the pair is visible in one place;
# it adds no assertion.

count="$(printf '%s\n' "$listing" | grep -cE '^scion-hub/')"
if [ "$count" -eq "${#EXPECTED_FILES[@]}" ]; then
  pass "package contains exactly ${#EXPECTED_FILES[@]} files"
else
  fail "package contains ${count} files, expected ${#EXPECTED_FILES[@]} -- update EXPECTED_FILES deliberately, do not just bump the number"
fi

# ---------------------------------------------------------------------------
# D. The base-url channel tripwire.  (1 assertion)
#
# 🔴 THIS ASSERTION EXISTS TO GO RED AT THE NEXT PHASE BOUNDARY. That is its
# purpose, not a side effect, and a reviewer who "fixes" it by bumping the
# constant without editing the prose below has defeated it.
#
# WHY IT IS HERE AND NOT IN PHASE 1. _helpers.tpl:1089 tells an operator that
# hub.args may not carry -base-url because "a later phase delivers this setting
# through another channel ... This chart delivers none of them yet: today the
# flag would simply take effect." :835 says the same of the whole $ownedByConfig
# list: "none of them lands anywhere. They are not rendered". Both are TRUE at
# this commit - measured, both channels empty, see the positive control below -
# and both become FALSE the moment the settings ConfigMap and
# SCION_SERVER_BASE_URL land. Eleven claims of exactly this shape have gone
# stale in this subtree and all eleven were caught by a person reading prose.
# None was caught by a check, because the only commit at which a transition
# tripwire can be installed is one BEFORE the transition. Landed after the
# boundary it watches, this is theatre: it would be written already knowing the
# answer, and it would never have been red.
#
# WHY IT IS ONE ASSERTION AND NOT TWO. The two halves rev-2 specified - measure
# the render against the committed number, and check the prose agrees with the
# committed number - are not independently useful. A render that gained a
# channel while the prose still denies it, and prose updated ahead of a render
# that has not moved, are the same defect seen from two sides: THE FILE AND THE
# CHART DISAGREE ABOUT WHAT THE CHART DOES. Reporting that once, with both
# measurements printed, is the honest count. gd-em's ruling fixed 25 -> 26 and
# 106 -> 107; this is the shape that fits that number, and the split is raised
# rather than silently resolved.
# ---------------------------------------------------------------------------

# PHASE 1 CROSSED THIS BOUNDARY. The render now carries exactly one channel,
# env(SCION_SERVER_BASE_URL) from configmap-env.yaml; argv still carries none.
# Both prose sites were edited in the diff that set this constant, and neither
# was edited by deleting the sentence and leaving the paragraph:
#   P0 :835  -> the $ownedByConfig header, now at _helpers.tpl:865, which names
#               which of the listed keys this release DOES deliver and which it
#               does not, rather than saying none of them land.
#   P0 :1089 -> the hub.args reservation, now at _helpers.tpl:1152, which names
#               -base-url and -storage-bucket as live and the other three as
#               still having no second source.
DELIVERS_BASE_URL_CHANNEL=1

# THE POSITIVE CONTROL COMES FIRST. "Zero channels deliver base-url" is a
# negative assertion and an empty render satisfies it perfectly. Section B
# already rendered successfully, but relying on that is relying on a fact
# established forty lines away by code that could later be reordered, so the
# subject is re-read here at the point of use.
if [ -z "$RENDER" ]; then
  fail "base-url channel tripwire: the render is EMPTY, so no channel could have been observed"
else
  _chan=0
  _via=""
  # Channel 1: argv. Channel 2: the environment variable named in the reservation.
  if printf '%s\n' "$RENDER" | grep -qF -- '--base-url'; then
    _chan=$((_chan + 1)); _via="${_via} argv(--base-url)"
  fi
  if printf '%s\n' "$RENDER" | grep -qF 'SCION_SERVER_BASE_URL'; then
    _chan=$((_chan + 1)); _via="${_via} env(SCION_SERVER_BASE_URL)"
  fi
  # Does the prose still claim zero? Matched on the two sentences that make the
  # claim, not on the whole paragraph, so rewording around them does not count
  # as retracting them.
  _h="${CHART}/templates/_helpers.tpl"

  # THE PROBE READS DESCRIPTIVE PROSE ONLY, AND THAT IS A PHASE 1 CHANGE.
  #
  # The paragraph that REPLACED the stale claim quotes the stale claim, in order
  # to say it went stale. _helpers.tpl:869-870 now reads: It read "there is no
  # second source yet ..." and "none of them lands anywhere", which were true
  # while the chart rendered no ConfigMap ... and false the moment it rendered
  # all three. A raw grep cannot tell that sentence from the sentence it is
  # about. The two have opposite truth values and identical bytes.
  #
  # So the subject is the file with double-quoted spans removed. Quotation is
  # the only mood this strips; a phase that retracts a claim WITHOUT quoting it
  # is unaffected. LIMIT, stated because it is real: sed works a line at a time,
  # so a quotation that wraps across a newline is not stripped and would be read
  # as descriptive - a false RED, which is the safe direction. The instance
  # above is balanced within its line, and CONTROL 2 below is what establishes
  # that rather than my say-so.
  _strip_quotes() { sed 's/"[^"]*"//g'; }

  # BOTH CONTROLS ARE META-FAILURES (exit 2), NOT ASSERTIONS. The count 26 is
  # pre-registered by gd-em and a control that moves it is a control that has to
  # argue for itself; a broken stripper does not make the CHART wrong, it makes
  # this run not evidence, which is exactly what exit 2 means here.
  #
  # CONTROL 1 - the apparatus fires. A quoted instance must vanish.
  if printf '%s\n' 'It read "none of them lands anywhere", which was true then.' \
       | _strip_quotes | grep -qF 'none of them lands anywhere'; then
    echo "META-FAILURE: the quotation stripper did not strip a quoted phrase." >&2
    echo "  Everything the base-url tripwire reports about prose is unreliable." >&2
    exit 2
  fi
  # CONTROL 2 - the apparatus does not over-fire, and this is the twin that
  # matters. A stripper that deleted the whole line would pass control 1
  # perfectly and would silence every real claim in the file. The fixture puts
  # an UNRELATED quoted span on the same line as an UNQUOTED claim, because that
  # is the layout a per-line stripper gets wrong.
  if ! printf '%s\n' 'Today "hub.baseUrl" is set and none of them lands anywhere.' \
       | _strip_quotes | grep -qF 'none of them lands anywhere'; then
    echo "META-FAILURE: the quotation stripper removed an UNQUOTED claim." >&2
    echo "  The base-url tripwire would report green by deleting its own subject." >&2
    exit 2
  fi

  # THE JOIN IS OR, WHERE PHASE 0 USED AND, AND THE CHANGE IS DELIBERATE.
  # Before the boundary the question was "does the prose still claim zero", and
  # both sentences claimed it together. After the boundary the question is "did
  # any stale claim survive the edit", and ONE survivor is a defect. AND would
  # let a half-finished retraction through.
  #
  # THE STRIPPER IS LOAD-BEARING ON THE REAL CORPUS, NOT DECORATIVE, AND THAT IS
  # MEASURABLE: run this probe without _strip_quotes and it goes RED right now,
  # on the quotation at :870 alone. The corpus is its own coverage control.
  _claims_zero=0
  if _strip_quotes < "$_h" 2>/dev/null \
       | grep -qE 'delivers none of them yet|none of them lands anywhere'; then
    _claims_zero=1
  fi
  _want_claim=0; [ "$DELIVERS_BASE_URL_CHANNEL" -eq 0 ] && _want_claim=1

  if [ "$_chan" -eq "$DELIVERS_BASE_URL_CHANNEL" ] && [ "$_claims_zero" -eq "$_want_claim" ]; then
    pass "base-url channel tripwire: ${_chan} channel(s), prose agrees (committed ${DELIVERS_BASE_URL_CHANNEL})"
  else
    fail "base-url channel tripwire: THE CHART AND ITS OWN PROSE DISAGREE, or the phase boundary was crossed without updating both."
    echo "        channels measured in the render: ${_chan}${_via:+ --}${_via}"
    echo "        DELIVERS_BASE_URL_CHANNEL committed as: ${DELIVERS_BASE_URL_CHANNEL}"
    echo "        _helpers.tpl still claims zero channels: ${_claims_zero} (expected ${_want_claim})"
    echo "        IF YOU JUST LANDED THE SETTINGS ConfigMap OR SCION_SERVER_BASE_URL:"
    echo "          this is the intended red. Set DELIVERS_BASE_URL_CHANNEL=${_chan}, bump"
    echo "          EXPECTED_TOTAL nowhere (the count is unchanged), and edit BOTH prose"
    echo "          sites - _helpers.tpl:835 and :1089 - IN THE SAME DIFF. Bumping the"
    echo "          constant alone leaves the chart lying to operators in its own error text."
  fi
fi

# ---------------------------------------------------------------------------
# E. The signing-key flip.  (7 assertions)
#
# THE SECRET HAS LANDED. THIS SECTION HAS BEEN INVERTED, NOT RETIRED.
#
# Phase 1 wrote this section while auth.requireStableSigningKey defaulted to
# false and true was UNSATISFIABLE: the hub resolves a stable key from a
# pre-configured key, SharedSigningSecret, a secret backend or its store
# (pkg/hub/server.go:1445); SharedSigningSecret comes only from --session-secret,
# SCION_SERVER_SESSION_SECRET or bare SESSION_SECRET
# (cmd/server_foreground.go:1452-1462); and the chart rendered none of the three.
# With true and no key, pkg/hub/server.go:1634 errors, pkg/hub/server.go:1008
# makes it fatal, cmd/server_foreground.go:259 calls log.Fatalf. So E2 asserted a
# `fail` in configmap-env.yaml that REFUSED true, and E5 was a tripwire waiting
# for the day the Secret landed and nobody flipped the default back.
#
# The session-secret phase is that day. templates/secret-session.yaml renders the
# Secret, scion-hub.assertSessionSecret makes it unconditional, and the default is
# now true. The tripwire fired and was answered.
#
# E2 IS INVERTED RATHER THAN DELETED, ON A RULING, AND THE REASON IS GENERAL. The
# input Phase 1 refused - true with no config.existingSecret - is now the input
# that must RENDER. Deleting the assertion would leave that transition untested in
# both directions: nothing would catch the guard being reinstated by a bad merge,
# and nothing would record that the refusal was removed on purpose. An inverted
# assertion carries the history a deleted one throws away.
#
# THE DANGEROUS DIRECTION HAS ALSO FLIPPED, WHICH IS WHY E5 KEEPS BOTH ARMS.
# Under Phase 1 the harm was flag=false with a source present (silent logouts).
# Now the harm is flag=true with NO source present (every pod log.Fatalf on first
# boot), and that is reachable in one edit: delete secret-session.yaml, or make
# assertSessionSecret conditional, and the default render loses its only source
# while the default stays true. E6 and E7 close that off from the other side.
# ---------------------------------------------------------------------------

# "DEFAULT" HERE MEANS BASE, WHICH NOW CARRIES A SESSION SECRET - see the BASE
# definition at the top. That is the default in the sense that matters: what an
# operator gets having supplied the one value the chart refuses to invent for
# them. BASE_NO_SECRET is the same render without it, and it does not produce
# manifests at all; that failure is E6's subject.
_DEFAULT_RENDER="$("$HELM" template t "$CHART" "${BASE[@]}" 2>&1)"

# THE SUBJECT IS THE RENDER WITH FULL-LINE COMMENTS REMOVED, AND THAT IS NOT
# TIDINESS - IT IS THE DIFFERENCE BETWEEN RED AND GREEN TODAY.
#
# configmap-env.yaml documents this exact mechanism in a plain YAML `#` comment,
# which means the comment RENDERS, which means every render of this chart -
# including one with no session secret in it anywhere - contains the string
# SCION_SERVER_SESSION_SECRET. A probe that cannot tell a source from a sentence
# ABOUT sources is therefore permanently satisfied, and cannot report the absence
# of the real thing.
#
# FULL-LINE comments only - first non-space character is `#`. A trailing comment
# after a value is left alone, because `#` inside a quoted YAML scalar is legal
# and stripping to end-of-line would delete real content.
#
# THE SAFE DIRECTION REVERSED WHEN THE DEFAULT DID, and the stripper must now be
# read the other way round. Under Phase 1, under-stripping gave a false RED and
# over-stripping a false GREEN. Now the probe's job is to confirm a source is
# PRESENT, so it is OVER-stripping that yields the false red and UNDER-stripping
# that yields the false green. Control 2 below is the one carrying the weight,
# and it is the control that would have been dropped as redundant.
#
# STILL LOAD-BEARING, RE-MEASURED AT THE SESSION-SECRET HEAD, AND NOW IT GUARDS
# THE OPPOSITE DIRECTION. Phase 1 recorded that the stripper was what kept the
# default render reading as "no sources"; that reading is now obsolete, because
# the render really does carry one. Measured here, four renders:
#
#   secret-session.yaml PRESENT, with the stripper:  SCION_SERVER_SESSION_SECRET
#   secret-session.yaml PRESENT, no stripper:        --session-secret SCION_SERVER_SESSION_SECRET SESSION_SECRET
#   secret-session.yaml DELETED, with the stripper:  (no sources)   <- E5 fires
#   secret-session.yaml DELETED, no stripper:        --session-secret SCION_SERVER_SESSION_SECRET SESSION_SECRET
#
# The last two lines are the whole argument. Deleting the Secret template while
# the default stays true is the log.Fatalf-on-every-pod regression E5 now exists
# to catch, and WITHOUT the stripper that deletion is invisible: configmap-env.
# yaml's own explanatory comment names all three source spellings, so the probe
# keeps finding "sources" in prose after the real one is gone and E5 stays green
# through the outage. The stripper is the only reason the alarm can fire at all.
#
# Same shape as section D's quotation problem, and the same shape as before -
# the explanatory prose and the thing it describes have identical bytes - but the
# consequence of getting it wrong has moved from a false red to a FALSE GREEN.
_strip_comment_lines() { grep -v '^[[:space:]]*#'; }

_secret_sources() { # stdin = render text; stdout = space-separated distinct tokens
  _strip_comment_lines \
    | grep -oE 'SCION_SERVER_SESSION_SECRET|SESSION_SECRET|--session-secret' \
    | sort -u | tr '\n' ' '
}

# BOTH CONTROLS ARE META-FAILURES (exit 2), NOT ASSERTIONS, for section D's
# reason: a broken stripper does not make the CHART wrong, it makes this run not
# evidence.
#
# CONTROL 1 - the apparatus fires. A full-line comment naming a source vanishes.
# The fixture is the real line out of today's default render, not an invention.
if printf '%s\n' '  # --session-secret, SCION_SERVER_SESSION_SECRET and bare SESSION_SECRET' \
     | _secret_sources | grep -q .; then
  echo "META-FAILURE: the comment stripper did not strip a full-line YAML comment." >&2
  echo "  Every limb below would read the chart's own documentation as a secret source." >&2
  echo "  E5 and E7 would then score green on a chart that renders NO session secret at" >&2
  echo "  all - the deleted-template regression, invisible, with the default still true" >&2
  echo "  and every pod hitting log.Fatalf at cmd/server_foreground.go:259 on first boot." >&2
  exit 2
fi
# CONTROL 2 - the apparatus does not over-fire. A stripper that deleted
# everything passes control 1 perfectly.
#
# UNDER THE INVERTED SECTION THIS CONTROL PROTECTS AGAINST A FALSE RED, NOT A
# FALSE GREEN, and it is retained for that reason rather than in spite of it. An
# over-firing stripper now makes E5 and E7 accuse a correct chart of shipping
# true with no secret source. The remedy text on that failure tells the reader to
# set the default back to false, which reintroduces the silent-logout bug Phase 1
# wrote this section to prevent. A detector that recommends the harm is worse
# than no detector - the same sentence as before, pointing the other way.
#
# Four fixtures because the three source spellings sit at three different indents
# and one of them is an argv element.
#
# THE DENOMINATOR IS ASSERTED, NOT DISPLAYED (gd-em, 08:22). Both loops below
# score "no misses" over whatever the heredoc contains, and an empty heredoc
# yields no misses - the vacuous green, reproduced inside the control written to
# prevent it. So each loop counts what it evaluated and the count is committed.
_c2_missed=""
_c2_seen=0
while IFS= read -r _line; do
  [ -z "$_line" ] && continue
  _c2_seen=$((_c2_seen+1))
  printf '%s\n' "$_line" | _secret_sources | grep -qE . || _c2_missed="${_c2_missed}
    ${_line}"
done <<'EOF'
        - name: SCION_SERVER_SESSION_SECRET
          key: SESSION_SECRET
        - --session-secret=$(HUB_SESSION_SECRET)
  value: "x"  # not a comment line: SESSION_SECRET named after a real value
EOF
if [ "$_c2_seen" -ne 4 ]; then
  echo "META-FAILURE: control 2 evaluated ${_c2_seen} fixtures, expected exactly 4." >&2
  echo "  A control that evaluated nothing reports the same green as one that passed." >&2
  exit 2
fi
if [ -n "$_c2_missed" ]; then
  echo "META-FAILURE: the comment stripper removed lines that are not comments:${_c2_missed}" >&2
  echo "  The LANDING limb would report green by deleting its own subject." >&2
  exit 2
fi

# --- E1. POSITIVE. Written before the negative, on purpose. -----------------
# The probe can find a source when one exists. Without this every limb below is
# satisfiable by a probe that finds nothing ever, which is the vacuous pass this
# project has shipped twice. Seeded with the three shapes the session-secret
# phase can plausibly render: a container env entry, a key inside the Secret it
# creates (envFrom means the NAME appears in the Secret, not in the Deployment),
# and the argv flag.
_e1_missed=""
_e1_seen=0
while IFS= read -r _line; do
  [ -z "$_line" ] && continue
  _e1_seen=$((_e1_seen+1))
  printf '%s\n' "$_line" | _secret_sources | grep -qE . || _e1_missed="${_e1_missed} [${_line}]"
done <<'EOF'
            - name: SCION_SERVER_SESSION_SECRET
  SCION_SERVER_SESSION_SECRET: c2VjcmV0
            - --session-secret=$(HUB_SESSION_SECRET)
        - name: SESSION_SECRET
EOF
if [ "$_e1_seen" -ne 4 ]; then
  echo "META-FAILURE: E1 evaluated ${_e1_seen} seeded fixtures, expected exactly 4." >&2
  echo "  An empty seed list scores 'no misses' - the vacuous green, inside the check" >&2
  echo "  written to prevent it. The denominator is asserted, not displayed." >&2
  exit 2
fi
if [ -z "$_e1_missed" ]; then
  pass "session-secret probe finds all ${_e1_seen} seeded source shapes"
else
  fail "session-secret probe MISSED seeded source(s):${_e1_missed} -- every limb below is vacuous"
fi

# --- E2. INVERTED. The input Phase 1 refused is the input that must render. --
# Phase 1 asserted here that `--set auth.requireStableSigningKey=true` WITHOUT
# config.existingSecret was rejected by a `fail` in configmap-env.yaml citing
# cmd/server_foreground.go:259. That `fail` is deleted and this asserts its
# absence by asserting the success it blocked - the same input, the opposite
# verdict, which is what makes a bad merge reinstating the guard visible.
#
# ASSERTING THE RENDERED VALUE, NOT MERELY A ZERO EXIT. A chart that rendered
# successfully while emitting "false" - because someone rewired the ternary
# rather than the guard - would pass an exit-code check and ship the bug.
#
# The old refusal's own text is asserted ABSENT alongside, because the guard
# could also be reinstated somewhere that leaves this particular render working;
# the string is the guard's fingerprint and it should exist nowhere now.
_e2="$("$HELM" template t "$CHART" "${BASE[@]}" \
         --set auth.requireStableSigningKey=true 2>&1)"
if printf '%s\n' "$_e2" | grep -q 'SCION_REQUIRE_STABLE_SIGNING_KEY: "true"' \
   && ! printf '%s\n' "$_e2" | grep -q 'auth.requireStableSigningKey is true'; then
  pass "requireStableSigningKey=true renders without config.existingSecret (Phase 1's refusal is gone)"
else
  fail "requireStableSigningKey=true did NOT render true without config.existingSecret -- Phase 1's guard is back, or the ternary was rewired"
  printf '%s\n' "$_e2" | sed 's/^/        | /' | head -3
fi

# --- E3. POSITIVE TWIN OF E2. Nothing refuses true anywhere. ----------------
# config.existingSecret was the ONE shape where true was satisfiable under Phase
# 1, so it is the shape a reinstated guard would most plausibly keep working.
# Kept as E2's twin for that reason: E2 alone cannot distinguish "no guard" from
# "a guard with this one exemption", which is exactly the state Phase 1 shipped.
_e3="$("$HELM" template t "$CHART" "${BASE[@]}" \
         --set auth.requireStableSigningKey=true \
         --set config.existingSecret=operator-settings 2>&1)"
if printf '%s\n' "$_e3" | grep -qF 'SCION_REQUIRE_STABLE_SIGNING_KEY: "true"'; then
  pass "requireStableSigningKey=true is permitted under config.existingSecret"
else
  fail "requireStableSigningKey=true was refused even under config.existingSecret -- the guard is unconditional"
  printf '%s\n' "$_e3" | sed 's/^/        | /' | head -3
fi

# --- E4. NON-VACUITY, on the render E5 reads. -------------------------------
# E5 compares two things it reads out of _DEFAULT_RENDER. If that render is empty
# or has lost the key, E5's comparison is between two absences and passes. This
# is the assertion that makes E5 mean something.
_e4_flag="$(printf '%s\n' "$_DEFAULT_RENDER" | sed -n 's/^[[:space:]]*SCION_REQUIRE_STABLE_SIGNING_KEY:[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' | head -1)"
if [ "$_e4_flag" = "true" ] || [ "$_e4_flag" = "false" ]; then
  pass "default render carries SCION_REQUIRE_STABLE_SIGNING_KEY (= \"${_e4_flag}\")"
else
  fail "default render does NOT carry a readable SCION_REQUIRE_STABLE_SIGNING_KEY -- the LANDING limb below is vacuous"
  printf '%s\n' "$_DEFAULT_RENDER" | sed 's/^/        | /' | head -3
fi

# --- E5. LANDED. The same two arms, with the live one now the second. -------
# BOTH ARMS ARE KEPT AND THEIR ROLES HAVE SWAPPED. Under Phase 1 the first arm
# was the tripwire and the second was documented as unreachable. The secret has
# landed and the default is true, so the first arm is now the unreachable one -
# retained because a phase that makes the session secret optional again would
# otherwise remove the last check on the silent-logout combination - and the
# SECOND arm is the live tripwire, one edit away from firing: delete
# secret-session.yaml, or put a condition back on scion-hub.assertSessionSecret,
# and the render loses its only source while this default stays true.
_e5_src="$(printf '%s\n' "$_DEFAULT_RENDER" | _secret_sources)"
if [ -n "$_e5_src" ] && [ "$_e4_flag" = "false" ]; then
  fail "A SESSION SECRET IS RENDERED AND THE DEFAULT IS BACK TO false."
  echo "        the default render carries: ${_e5_src}"
  echo "        values.yaml defaults auth.requireStableSigningKey to false, so this"
  echo "        release ships replicas that sign tokens each other reject - silently,"
  echo "        presenting as intermittent logouts rather than as a configuration error."
  echo "        SET IT TO true. This assertion stays red until you do."
elif [ -z "$_e5_src" ] && [ "$_e4_flag" = "true" ]; then
  fail "THE SESSION SECRET STOPPED RENDERING AND THE DEFAULT IS STILL true."
  echo "        the default render carries no session-secret source at all, so every pod"
  echo "        would log.Fatalf at cmd/server_foreground.go:259 on first boot - the whole"
  echo "        release dead, not degraded. Either restore the rendered secret or set"
  echo "        auth.requireStableSigningKey back to false IN THE SAME DIFF."
else
  pass "signing-key default agrees with the default render's secret sources (flag=\"${_e4_flag}\", sources=${_e5_src:-none})"
fi

# --- E6. NEGATIVE. No secret at all is refused, and refused BY NAME. --------
# The twin of E2/E3, and the assertion that makes the true default defensible:
# true is only safe because there is no way to render without a secret. This is
# the acceptance criterion "install with neither auth.sessionSecret nor
# auth.existingSecret fails at template time with a message naming the value".
#
# BOTH VALUE NAMES ARE ASSERTED, not the failure. Several other `fail`s and the
# schema also exit non-zero, so a non-zero exit does not prove THIS guard fired;
# and an operator who is told only "no session secret" does not learn which of
# the two values to set. A message that named just one would send everyone who
# wanted the bring-your-own path down the inline one.
_e6="$("$HELM" template t "$CHART" "${BASE_NO_SECRET[@]}" 2>&1)"
if printf '%s\n' "$_e6" | grep -q 'auth.sessionSecret' \
   && printf '%s\n' "$_e6" | grep -q 'auth.existingSecret' \
   && ! printf '%s\n' "$_e6" | grep -q 'SCION_REQUIRE_STABLE_SIGNING_KEY'; then
  pass "render with no session secret is refused, naming auth.sessionSecret and auth.existingSecret"
else
  fail "render with no session secret was NOT refused, or the refusal stopped naming both values"
  printf '%s\n' "$_e6" | sed 's/^/        | /' | head -3
fi

# --- E7. THE PAIRING, OVER THE WHOLE FIXTURE CORPUS. ------------------------
# E5 checks one render. This checks every shipped permutation, and it is the
# assertion the signing-key flip owes: NO combination of values the chart
# supports may render SCION_REQUIRE_STABLE_SIGNING_KEY="true" with no session
# secret source. That is the pod-wide log.Fatalf, and a default that is safe on
# the default render is not the same claim as one that is safe everywhere -
# ci/values-varied.yaml sets the flag false, and any fixture may set it either
# way, so the invariant is the PAIRING and not either value alone.
#
# THE DENOMINATOR IS ASSERTED AGAINST THE FILES ON DISK, IN BOTH DIRECTIONS. A
# loop over a glob that matched nothing scores "no violations" and reports the
# vacuous green; a loop whose list was hand-copied silently stops covering a
# fixture added later. So the count comes from the glob, is required to be
# non-zero, and is required to equal the number of ci/values-*.yaml files.
_e7_bad=""
_e7_seen=0
_e7_files=("$CHART"/ci/values-*.yaml)
for _f in "${_e7_files[@]}"; do
  [ -e "$_f" ] || continue
  _e7_seen=$((_e7_seen+1))
  if ! _r="$("$HELM" template t "$CHART" -f "$_f" 2>&1)"; then
    # A fixture that FAILS TO RENDER cannot ship a pod. The harm is a
    # successful render of flag=true with no source, not a refused render.
    # Negative-control fixtures (values-unknown-key.yaml) and fixtures whose
    # schema rejects them are expected to fail here.
    continue
  fi
  _flag="$(printf '%s\n' "$_r" | sed -n 's/^[[:space:]]*SCION_REQUIRE_STABLE_SIGNING_KEY:[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' | head -1)"
  if [ -z "$_flag" ]; then
    _e7_bad="${_e7_bad} [$(basename "$_f"): rendered no readable flag]"
    continue
  fi
  if [ "$_flag" = "true" ] && [ -z "$(printf '%s\n' "$_r" | _secret_sources)" ]; then
    _e7_bad="${_e7_bad} [$(basename "$_f"): flag true, no secret source]"
  fi
done
if [ "$_e7_seen" -eq 0 ]; then
  echo "META-FAILURE: E7 evaluated no ci/values-*.yaml fixtures." >&2
  echo "  A loop over an empty list reports the same green as one that passed." >&2
  exit 2
fi
_e7_ondisk="$(find "$CHART/ci" -maxdepth 1 -name 'values-*.yaml' | wc -l | tr -d ' ')"
if [ "$_e7_seen" -ne "$_e7_ondisk" ]; then
  echo "META-FAILURE: E7 evaluated ${_e7_seen} fixtures but ${_e7_ondisk} exist on disk." >&2
  exit 2
fi
if [ -z "$_e7_bad" ]; then
  pass "all ${_e7_seen} ci fixtures pair SCION_REQUIRE_STABLE_SIGNING_KEY=true with a session-secret source"
else
  fail "ci fixtures render the signing-key flag true with no secret source:${_e7_bad}"
fi

# --- E8. NO POD ANNOTATION IS DERIVED FROM THE SESSION SECRET. --------------
# check-secret-placement.sh already refuses the secret's VALUE in an
# annotation. This refuses a DIGEST of it, which that scanner cannot see and
# which is the form the mistake would actually take.
#
# The mistake is attractive, which is why it needs a guard rather than a
# comment. The session secret reaches the container through env, env is
# resolved once at container start, so rotating it does nothing until the pods
# restart - and the chart's own checksum/settings annotation is the established
# local idiom for exactly that problem. Adding checksum/session would look like
# consistency.
#
# It is not, because the two are not the same trade. Pod annotations are
# readable by anyone with pod read access, which is a strictly wider audience
# than the Secret's RBAC, and a digest over a single low-entropy credential is
# recoverable offline. checksum/settings covers a whole rendered document and
# is load-bearing in a way this would not be: settings.yaml is a subPath mount,
# frozen for the container's lifetime, with no restart remedy an operator can
# be told to run. The session secret has one, and values.yaml documents it.
#
# THE POSITIVE CONTROL IS A META-FAILURE, NOT AN ASSERTION, because a parser
# that has stopped finding annotations would report this green forever in the
# same words as a clean run. So the extractor must find checksum/env-config -
# an annotation this chart renders unconditionally - before its silence about
# 'session' is allowed to count as evidence.
_e8_keys() { # stdin = render text; stdout = annotation keys, one per line
  awk '
    /^[[:space:]]*annotations:[[:space:]]*$/ {
      match($0, /^[[:space:]]*/); ind = RLENGTH; in_ann = 1; next
    }
    in_ann {
      match($0, /^[[:space:]]*/)
      if (RLENGTH <= ind || $0 ~ /^[[:space:]]*$/) { in_ann = 0; next }
      if (match($0, /^[[:space:]]*[^[:space:]:]+:/))
        { k = substr($0, RSTART, RLENGTH - 1); gsub(/^[[:space:]]+/, "", k); print k }
    }
  '
}
_e8_bad=""
_e8_seen=0
_e8_rendered=0
_e8_control=0
for _f in "${_e7_files[@]}"; do
  [ -e "$_f" ] || continue
  _e8_seen=$((_e8_seen+1))
  _r="$("$HELM" template t "$CHART" -f "$_f" 2>&1)" || continue
  _e8_rendered=$((_e8_rendered+1))
  _k="$(printf '%s\n' "$_r" | _e8_keys)"
  printf '%s\n' "$_k" | grep -qx 'checksum/env-config' && _e8_control=$((_e8_control+1))
  _hit="$(printf '%s\n' "$_k" | grep -i 'session' | tr '\n' ' ')"
  [ -n "$_hit" ] && _e8_bad="${_e8_bad} [$(basename "$_f"): ${_hit}]"
done
if [ "$_e8_seen" -ne "$_e7_ondisk" ]; then
  echo "META-FAILURE: E8 evaluated ${_e8_seen} fixtures but ${_e7_ondisk} exist on disk." >&2
  exit 2
fi
if [ "$_e8_rendered" -eq 0 ]; then
  echo "META-FAILURE: E8 found no renderable fixtures among ${_e8_seen} on disk." >&2
  exit 2
fi
if [ "$_e8_control" -ne "$_e8_rendered" ]; then
  echo "META-FAILURE: E8's annotation extractor found checksum/env-config in ${_e8_control}" >&2
  echo "  of ${_e8_rendered} renderable fixtures. Every renderable fixture renders it, so the" >&2
  echo "  extractor is broken and its silence about 'session' is not evidence about the chart." >&2
  exit 2
fi
if [ -z "$_e8_bad" ]; then
  pass "no pod annotation in ${_e8_seen} ci fixtures is derived from the session secret"
else
  fail "a pod annotation names or digests the session secret:${_e8_bad} -- see values.yaml on why checksum/session is deliberately absent"
fi

# ---------------------------------------------------------------------------
# E9. checksum/settings digests a REDACTED PROJECTION, not the document.
# ---------------------------------------------------------------------------
#
# E8 guards the annotation surface on the NAME axis: no annotation KEY may name
# the session secret. It cannot see this one, because checksum/settings names
# nothing - the credential is in what the digest was TAKEN OVER, not in the key.
# That is the derivation axis and until this check it had no guard at all, on the
# one surface where the chart digests a Secret.
#
# The exposure was measured rather than argued: the digest preimage was recovered
# byte for byte and then used to predict helm's digest for three client secrets
# that had never been rendered, 3 of 3, at ~300k guesses/sec on one core. Against
# a provider-issued secret that is CONFIRMATION of a candidate an attacker
# already holds, not recovery - including against superseded values still in the
# ReplicaSet revision history. Lower bar than recovery, and the real one.
#
# WHY ARM 0 EXISTS, AND IT IS THE ONLY ARM THAT MATTERS. Arms 1 and 2 ask whether
# two digests are equal. If the two renders were IDENTICAL, the digests would be
# equal for a reason that has nothing to do with redaction, and this check would
# print PASS forever while measuring nothing. That is not hypothetical: the
# author's first run of this comparison by hand used a --set that changed a value
# to what it already was, and read the resulting "unchanged" as a defect in the
# projection. An inert arm renders exactly like a working one. So arm 0 asserts
# the two inputs really do differ, BY VALUE, in both directions, and it is a
# META-FAILURE rather than an assertion: if the inputs did not differ, nothing
# was analysed and a green line here would be a lie rather than a wrong answer.
#
# Arm 2 is the positive control for arm 1. Deleting the annotation, or hashing a
# constant, would satisfy arm 1 perfectly - and would silently break every
# settings upgrade, because settings.yaml is a subPath mount frozen for the
# container's lifetime. Arm 1 alone cannot tell "the secret is redacted" from
# "the digest is dead". Arm 2 can, and it is why both are here.
_e9_fx="$CHART/ci/values-settings-oauth.yaml"
_e9_run() { "$HELM" template t "$CHART" -f "$_e9_fx" "$@" 2>&1; }
_e9_dig() { printf '%s\n' "$1" | sed -n 's/^[[:space:]]*checksum\/settings:[[:space:]]*//p'; }
_e9_A="$(_e9_run --set-string auth.oauth.web.google.clientSecret=E9-ARM-A-SECRET)"
_e9_B="$(_e9_run --set-string auth.oauth.web.google.clientSecret=E9-ARM-B-SECRET)"
_e9_C="$(_e9_run --set-string auth.oauth.web.google.clientSecret=E9-ARM-A-SECRET --set-string hub.name=E9ArmCRename)"
# Arm 0a: each arm must render exactly one checksum/settings, non-empty. Zero
# would make every equality below vacuously true; two would make "the digest"
# ambiguous and the comparison meaningless.
for _e9_n in A B C; do
  eval "_e9_v=\"\$_e9_${_e9_n}\""
  # shellcheck disable=SC2154
  _e9_d="$(_e9_dig "$_e9_v")"
  _e9_c="$(printf '%s\n' "$_e9_d" | grep -c . || true)"
  if [ "$_e9_c" -ne 1 ]; then
    echo "META-FAILURE: E9 arm ${_e9_n} rendered ${_e9_c} checksum/settings annotations, expected exactly 1." >&2
    echo "  Nothing was analysed: the comparisons below would be over an absent or ambiguous value." >&2
    exit 2
  fi
done
_e9_dA="$(_e9_dig "$_e9_A")"; _e9_dB="$(_e9_dig "$_e9_B")"; _e9_dC="$(_e9_dig "$_e9_C")"
# Arm 0b: the inputs really differ, in both directions. Checked by value, on the
# rendered Secret, so a --set that silently failed to apply cannot pass as a
# collapse.
_e9_ina="$(printf '%s\n' "$_e9_A" | grep -cF -- 'E9-ARM-A-SECRET' || true)"
_e9_inb="$(printf '%s\n' "$_e9_B" | grep -cF -- 'E9-ARM-B-SECRET' || true)"
_e9_xa="$(printf '%s\n' "$_e9_A" | grep -cF -- 'E9-ARM-B-SECRET' || true)"
_e9_xb="$(printf '%s\n' "$_e9_B" | grep -cF -- 'E9-ARM-A-SECRET' || true)"
if [ "$_e9_ina" -ne 1 ] || [ "$_e9_inb" -ne 1 ] || [ "$_e9_xa" -ne 0 ] || [ "$_e9_xb" -ne 0 ]; then
  echo "META-FAILURE: E9's two arms were supposed to differ by exactly the client secret." >&2
  echo "  Expected A:own=1 B:own=1 A:other=0 B:other=0; got A:own=${_e9_ina} B:own=${_e9_inb} A:other=${_e9_xa} B:other=${_e9_xb}." >&2
  echo "  The --set did not land, so the two renders are not distinguishable and NOTHING WAS ANALYSED." >&2
  exit 2
fi
if [ "$_e9_dA" = "$_e9_dB" ]; then
  pass "checksum/settings is unchanged by rotating the OAuth client secret (the digest is a projection, not an oracle)"
else
  fail "checksum/settings CHANGED when only the OAuth client secret changed (${_e9_dA} -> ${_e9_dB}). The annotation is a digest over the credential again, which publishes an offline verification oracle for it to everyone with pod read access - a wider audience than the Secret's RBAC. See scion-hub.settingsChecksum."
fi
if [ "$_e9_dA" != "$_e9_dC" ]; then
  pass "checksum/settings still moves when a non-secret settings field changes (the projection did not kill the annotation)"
else
  fail "checksum/settings did NOT move when hub.name changed. The redaction has gone too far or the digest is a constant - and settings.yaml is a subPath mount the kubelet never refreshes, so every configuration upgrade would now report success and change nothing."
fi

# ---------------------------------------------------------------------------
# E10. A SHORT client secret must not be REFUSED, and must still be redacted.
# ---------------------------------------------------------------------------
#
# THIS IS A REGRESSION TEST FOR A DEFECT THIS HARNESS DID NOT FIND. The value
# half of scion-hub.settingsChecksum searches the finished projection for each
# credential it redacted, and the search is a plain substring test. A client
# secret of "def" is a substring of the word "default" in the rendered settings,
# so the chart REFUSED a perfectly valid install and told the maintainer to add
# a redaction path that does not exist. Found by an arm run for an unrelated
# reason - rendering NOTES.txt with credentials set - not by E9, which uses
# long values and could never have exhibited it.
#
# Both assertions are needed and they pull against each other. The first says
# the render is not refused; on its own, deleting the whole value check passes
# it. The second says a short secret is still absent from the digest, which is
# the property the value check exists to protect - and it is asserted the only
# way it can be from outside: two different short secrets must produce ONE
# digest. E9's arm 2 remains the control that the digest is not simply dead.
_e10_run() { "$HELM" template t "$CHART" "${BASE[@]}" --set auth.mode=oauth \
  --set-string auth.oauth.web.google.clientId=e10-client-id \
  --set-string auth.oauth.web.google.clientSecret="$1" 2>&1; }
_e10_a="$(_e10_run def)"
_e10_b="$(_e10_run ghi)"
if printf '%s\n' "$_e10_a" | grep -qE '^Error:'; then
  fail "a three-character OAuth client secret was REFUSED: $(printf '%s\n' "$_e10_a" | head -1). The value-based backstop in scion-hub.settingsChecksum is colliding with ordinary rendered text - see the floor documented there. A refusal an operator cannot act on is worse than the check being absent."
else
  pass "a short OAuth client secret renders (the value backstop's substring test does not collide with ordinary settings text)"
fi
_e10_da="$(_e9_dig "$_e10_a")"; _e10_db="$(_e9_dig "$_e10_b")"
_e10_ca="$(printf '%s\n' "$_e10_da" | grep -c . || true)"
if [ "$_e10_ca" -ne 1 ] || [ "$(printf '%s\n' "$_e10_db" | grep -c . || true)" -ne 1 ]; then
  echo "META-FAILURE: E10 did not render exactly one checksum/settings per arm (got ${_e10_ca} and $(printf '%s\n' "$_e10_db" | grep -c . || true))." >&2
  echo "  Nothing was analysed: the equality below would be over an absent value." >&2
  exit 2
fi
if [ "$_e10_da" = "$_e10_db" ]; then
  pass "two different short OAuth client secrets produce one checksum/settings (the length floor narrowed the check, not the redaction)"
else
  fail "checksum/settings differs between two SHORT client secrets (${_e10_da} -> ${_e10_db}). The redaction is length-dependent, so the oracle is restored for exactly the credentials with the least entropy to guess."
fi

# ---------------------------------------------------------------------------
# E11. The rotation restart NOTES.txt owes the operator, because E9 removed it.
# ---------------------------------------------------------------------------
#
# E9 asserts that rotating an OAuth client secret does NOT move
# checksum/settings. That is the fix, and it deletes a real behaviour: before the
# redaction, a credential rotation rolled the pods, as a side effect of the
# credential being inside the digest. The same side effect was the verification
# oracle. They were one mechanism and they are gone together.
#
# The replacement is prose - values.yaml next to auth.oauth.web.*.clientSecret,
# and NOTES.txt, which is the only one of the two an operator sees without going
# looking. scion-hub.settingsChecksum's comment says "delete either of those and
# you have deleted the behaviour, not the annotation." That was an unasserted
# claim about a file nothing tested, which is the shape of every stale comment in
# this chart's history, so it is asserted here.
#
# RENDERING NOTES.txt WITHOUT A CLUSTER. `helm template` evaluates NOTES.txt but
# never prints it, and this helm has no --notes. The technique is hack/verify.sh's
# and is copied deliberately rather than reinvented: copy the chart, rename
# templates/NOTES.txt to a .txt manifest name, --show-only that file with --debug
# (helm exits non-zero because the prose is not YAML; --debug renders it anyway).
# The copy is compared byte-for-byte against the original first - otherwise this
# section could be measuring a file that is not the chart's NOTES.
_e11_render() { # $1 = out file, rest = helm args
  local _o="$1"; shift
  local _d; _d="$(mktemp -d)"
  cp -a "$CHART" "$_d/c"
  mv "$_d/c/templates/NOTES.txt" "$_d/c/templates/zz-notes-probe.txt"
  if ! cmp -s "$CHART/templates/NOTES.txt" "$_d/c/templates/zz-notes-probe.txt"; then
    rm -rf "$_d"
    echo "META-FAILURE: E11's NOTES probe is not byte-identical to templates/NOTES.txt." >&2
    exit 2
  fi
  "$HELM" template t "$_d/c" --debug --show-only templates/zz-notes-probe.txt "$@" 2>/dev/null \
    | sed -n '/^# Source: .*zz-notes-probe\.txt$/,$p' >"$_o"
  rm -rf "$_d"
}
_e11_on="$(mktemp)"; _e11_off="$(mktemp)"
_e11_render "$_e11_on" "${BASE[@]}" --set auth.mode=oauth \
  --set-string auth.oauth.web.google.clientId=e11-client-id \
  --set-string auth.oauth.web.google.clientSecret=e11-client-secret-value
_e11_render "$_e11_off" "${BASE[@]}"
# Arm 0: BOTH renders must be non-empty and must contain a section that is in
# NOTES.txt unconditionally. Without this the "absent" assertion below is
# satisfied by a probe that rendered nothing at all, which is the standing defect
# in every absence check: a broken harness and a clean chart look identical.
for _e11_f in "$_e11_on" "$_e11_off"; do
  if [ ! -s "$_e11_f" ] || ! grep -qF 'SCHEMA_VERSION IS LOAD-BEARING' "$_e11_f"; then
    echo "META-FAILURE: E11's NOTES probe rendered nothing usable ($(wc -c <"$_e11_f") bytes, unconditional anchor $(grep -cF 'SCHEMA_VERSION IS LOAD-BEARING' "$_e11_f" || true))." >&2
    echo "  Nothing was analysed: the absence assertion below would pass against an empty file." >&2
    rm -f "$_e11_on" "$_e11_off"
    exit 2
  fi
done
if grep -qF 'ROTATING AN OAUTH CLIENT SECRET' "$_e11_on"; then
  pass "NOTES.txt tells the operator to restart after an OAuth credential rotation"
else
  fail "NOTES.txt does NOT print the rotation restart when OAuth credentials are set. checksum/settings is deliberately blind to credential changes (E9), so this paragraph is the only thing that tells an operator their rotation has not taken effect - and the pods stay green while serving the old credential."
fi
if grep -qF 'ROTATING AN OAUTH CLIENT SECRET' "$_e11_off"; then
  fail "NOTES.txt prints the OAuth rotation restart for a release that sets no OAuth credentials. It is advice the operator cannot act on, and a NOTES that warns about everything is read as warning about nothing."
else
  pass "NOTES.txt omits the OAuth rotation restart when no OAuth credentials are set (the condition is a condition, not a constant)"
fi
rm -f "$_e11_on" "$_e11_off"

# ---------------------------------------------------------------------------
# E12. The credential guard reaches the SCHEDULING fields, not just the two
# pod-spec fields somebody thought of.
# ---------------------------------------------------------------------------
#
# The pass that guarded hub.podAnnotations and hub.podLabels enumerated the
# fields an operator thinks of as carrying text, and stopped there.
# hub.nodeSelector, hub.tolerations and hub.affinity are the same disclosure
# surface - free-form operator input, rendered verbatim into a pod spec that
# anyone with pod read access can read - and all three rendered a DSN clean while
# the identical string on hub.podAnnotations was refused. Measured before the
# fix, not reasoned about.
#
# THE ENUMERATION IS THE FRAGILE PART, NOT THE MATCHER. Every one of these
# assertions passes by the guard being CALLED on a surface; none of them can tell
# you about a surface with no call. That is not a hole this file can close - a
# test can only plant into fields it knows about, which is the same blindness
# that produced the gap. It is stated so the next reader does not mistake five
# green rows for coverage of the pod spec.
#
# PLANTED VIA A VALUES FILE, NEVER --set-string. `--set-string` runs its own
# escape parser: 'Sc2\Back\Alpha' arrives as 'Sc2BackAlpha', so the chart is
# handed a DIFFERENT credential than the one written here, the guard correctly
# does not find the one we planted, and the arm records a false pass.
#
# ASSERTED ON THE ERROR STRING AND THE SOURCE NAME TOGETHER. A non-zero exit is
# not evidence that the guard fired: the first draft of this measurement was 13
# apparent confirmations that were all schema rejections, because the values
# overlay had been concatenated onto the base file and a duplicate `hub:` key
# silently replaced the base mapping. Nothing had reached a template. The clean
# arm at the end is what exposed it, and it is why that arm is here.
_e12_vals="$(mktemp)"
_e12_dsn='postgres://u:hunter2AAA@10.0.0.1/db'
_e12_arm() { # $1 = values yaml, $2 = expected source name in the failure
  local _out
  _out="$(printf '%s\n' "$1" >"$_e12_vals"; "$HELM" template t "$CHART" "${BASE[@]}" -f "$_e12_vals" 2>&1)"
  if printf '%s' "$_out" | grep -qF "don't meet the specifications of the schema"; then
    echo "META-FAILURE: E12 arm for $2 was rejected by the values schema and never reached a template." >&2
    rm -f "$_e12_vals"; exit 2
  fi
  printf '%s' "$_out" | grep -qF "embeds credentials in a URL" \
    && printf '%s' "$_out" | grep -qF "$2"
}
for _e12_case in \
  "hub.nodeSelector:hub:
  nodeSelector:
    disk: \"$_e12_dsn\"" \
  "hub.tolerations:hub:
  tolerations:
    - key: \"$_e12_dsn\"
      operator: Exists" \
  "hub.affinity:hub:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: \"$_e12_dsn\"
                operator: Exists"
do
  _e12_name="${_e12_case%%:*}"
  if _e12_arm "${_e12_case#*:}" "$_e12_name"; then
    pass "${_e12_name} refuses a credential (the scheduling fields reach the guard)"
  else
    fail "${_e12_name} renders a DSN into the pod spec without a word. The pod spec is readable by anyone with pod read access - a wider audience than the Secret's RBAC - and hub.podAnnotations refuses the identical string, so this is an unguarded surface beside a guarded one rather than a decision to allow it."
  fi
done
# The arm that caught the empty corpus. Without it, every row above is satisfied
# by a chart that refuses everything, including values an operator must be able
# to set.
printf 'hub:\n  nodeSelector:\n    disk: ssd\n  tolerations:\n    - key: dedicated\n      operator: Exists\n' >"$_e12_vals"
if "$HELM" template t "$CHART" "${BASE[@]}" -f "$_e12_vals" >/dev/null 2>&1; then
  pass "ordinary scheduling values still render (the guard above refuses credentials, not scheduling)"
else
  fail "a plain nodeSelector/toleration no longer renders. The credential guard has been widened into legitimate operator input, which is worse than the hole it closed: the hole leaked a value the operator chose to put there, this rejects a value they must set, with no override."
fi
rm -f "$_e12_vals"

# ---------------------------------------------------------------------------
# Fail closed.
# ---------------------------------------------------------------------------

# Emitted unconditionally, on every exit path, so run-all.sh can sum what
# actually ran even when this script is reporting a failure. The count check must
# not be silenced by the outcome it is meant to qualify.
echo "ASSERTIONS_EXECUTED=${executed}"

if [ "$executed" -ne "$EXPECTED_TOTAL" ]; then
  # INEQUALITY, NOT A FLOOR. A short run is a failed run; a LONG run means
  # assertions were added without committing the number, which is the same
  # defect facing the other way. Where a check counts anything, the number is
  # committed and both directions fail.
  echo "HARNESS ERROR: executed ${executed} assertions, expected exactly ${EXPECTED_TOTAL}."
  exit 2
fi

if [ "$failed" -ne 0 ]; then
  echo "FAILED ${failed}/${executed}"
  exit 1
fi

echo "PASS ${executed}/${EXPECTED_TOTAL}"
