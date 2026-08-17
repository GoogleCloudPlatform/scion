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

# Minimum values that make the chart render. All three are `required` with no default by design.
# hub.baseUrl became required in Phase 1: the hub's own fallback is a localhost URL that agents
# cannot reach and that disables the session cookie's Secure attribute, so there is no safe
# default. Before it was added here every render in this file returned empty, which section D's
# empty-render guard reported correctly rather than scoring it as zero channels.
BASE=(--set image.repository=example.invalid/scion-hub --set hub.hubId=h --set hub.baseUrl=https://h.example.invalid)

# HELD AT 26 ON PURPOSE, AND THIS SCRIPT THEREFORE EXITS 2.
#
# The true Phase 1 figure is 35: 26 as adopted, +2 kinds (ConfigMap, Secret) in
# section B, +2 files (configmap-env.yaml, secret-settings.yaml) in section C,
# +5 in section E (the signing-key flip).
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
EXPECTED_TOTAL=35

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

schema_rejects() { # $1 = --set expr, $2 = expected path in the schema error
  local out
  out="$("$HELM" template t "$CHART" "${BASE[@]}" --set "$1" 2>&1)"
  if printf '%s' "$out" | grep -q "Additional property .* is not allowed" \
     && printf '%s' "$out" | grep -q "^- $2: Additional property"; then
    pass "schema rejects unknown key ($1) at '$2'"
  else
    fail "schema did NOT reject unknown key ($1) at '$2' -- values.schema.json missing or not enforcing"
  fi
}

schema_rejects "bogusKeyNotInSchema=1" "(root)"
schema_rejects "hub.bogusNested=1"     "hub"

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
# goes short. The total below is DERIVED from this list rather than written
# beside it: a hand-listed set with a separately-maintained count is two facts
# that can disagree, and the comment asking the next person to update both is a
# request, not a mechanism.
EXPECTED_KINDS=(ConfigMap Deployment Role RoleBinding Secret Service ServiceAccount)

for k in "${EXPECTED_KINDS[@]}"; do
  if printf '%s\n' "$RENDER" | grep -qx "kind: $k"; then
    pass "render contains kind: $k"
  else
    fail "render is MISSING kind: $k -- template dropped (check .helmignore breadth)"
  fi
done

# THE TWIN, AND IT IS NOT THE LOOP AGAIN. The loop says each named kind is
# present; it is silent on anything present that is NOT named. A template that
# started emitting a second Secret, or a stray manifest from an over-broad
# range, satisfies every iteration above.
kinds="$(printf '%s\n' "$RENDER" | grep -c '^kind:')"
if [ "$kinds" -eq "${#EXPECTED_KINDS[@]}" ]; then
  pass "render emits exactly ${#EXPECTED_KINDS[@]} manifests"
else
  fail "render emits ${kinds} manifests, expected ${#EXPECTED_KINDS[@]} -- add it to EXPECTED_KINDS deliberately, or find out what it is"
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
  scion-hub/templates/secret-settings.yaml
  scion-hub/templates/service.yaml
  scion-hub/templates/serviceaccount.yaml
)

pkgdir="$(mktemp -d)"
trap 'rm -rf "$pkgdir"' EXIT

if "$HELM" package "$CHART" -d "$pkgdir" >/dev/null 2>&1; then
  listing="$(tar tzf "$pkgdir"/*.tgz | grep -v '/$' | sort)"
else
  listing=""
fi

for f in "${EXPECTED_FILES[@]}"; do
  if printf '%s\n' "$listing" | grep -qx "$f"; then
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
  printf '%s\n' "$listing" | grep -q "^scion-hub/${_d}/" && _excluded="${_excluded} ${_d}/"
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

count="$(printf '%s\n' "$listing" | grep -c '^scion-hub/')"
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
  if printf '%s\n' "$RENDER" | grep -q -- '--base-url'; then
    _chan=$((_chan + 1)); _via="${_via} argv(--base-url)"
  fi
  if printf '%s\n' "$RENDER" | grep -q 'SCION_SERVER_BASE_URL'; then
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
       | _strip_quotes | grep -q 'none of them lands anywhere'; then
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
       | _strip_quotes | grep -q 'none of them lands anywhere'; then
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
# E. The signing-key flip.  (5 assertions)
#
# WHAT THIS PROTECTS, AND IT IS NOT THE FIX.
#
# auth.requireStableSigningKey defaults to false in this release because nothing
# in this release can satisfy true: the hub resolves a stable key from a
# pre-configured key, SharedSigningSecret, a secret backend or its store
# (pkg/hub/server.go:1445), SharedSigningSecret comes only from --session-secret,
# SCION_SERVER_SESSION_SECRET or bare SESSION_SECRET
# (cmd/server_foreground.go:1452-1462), and the chart renders none of the three.
# With true and no key, pkg/hub/server.go:1634 errors, pkg/hub/server.go:1008
# makes it fatal, cmd/server_foreground.go:259 calls log.Fatalf.
#
# The failure mode this section exists for is the OPPOSITE one, and it is the
# one a check written to protect the fix will miss: the session-secret phase
# lands the Secret and nobody flips the default back. A `false` default satisfies
# every negative assertion vacuously and forever. So the LANDING limb is keyed to
# the DEFAULT RENDER, not to the permutation set, and it goes red the day the
# default render gains a source and stays red until this default reads true.
#
# Keyed to the default deliberately: an operator who supplies their own secret
# and leaves the flag false is doing something legitimate and must not trip it.
# ---------------------------------------------------------------------------

_DEFAULT_RENDER="$("$HELM" template t "$CHART" "${BASE[@]}" 2>&1)"

# THE SUBJECT IS THE RENDER WITH FULL-LINE COMMENTS REMOVED, AND THAT IS NOT
# TIDINESS - IT IS THE DIFFERENCE BETWEEN RED AND GREEN TODAY.
#
# configmap-env.yaml documents this exact mechanism in a plain YAML `#` comment,
# which means the comment RENDERS, which means the default render contains the
# string SCION_SERVER_SESSION_SECRET right now. A naive probe reads that as "the
# default render carries a session-secret source", the LANDING limb then demands
# the default be flipped to true, and the chart would be driven into precisely
# the boot failure this whole section exists to prevent - by a detector satisfied
# by a sentence ABOUT sources. Same shape as section D's quotation problem: the
# explanatory prose and the thing it describes have identical bytes.
#
# FULL-LINE comments only - first non-space character is `#`. A trailing comment
# after a value is left alone, because `#` inside a quoted YAML scalar is legal
# and stripping to end-of-line would delete real content. Under-stripping yields
# a false RED, which is the safe direction; over-stripping would yield a false
# GREEN. An env var name and an argv flag are never on a full-line comment.
#
# LOAD-BEARING TODAY, MEASURED, NOT ASSERTED. On the default render at this head:
#   with the stripper:    (no sources)
#   without the stripper: --session-secret SCION_SERVER_SESSION_SECRET SESSION_SECRET
# So the corpus is its own coverage control - remove the stripper and E5 goes red
# immediately, and its remedy text tells you to set the default to true, which is
# the log.Fatalf. A detector that recommends the harm is worse than no detector.
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
  echo "  The LANDING limb would read the chart's own documentation as a secret source" >&2
  echo "  and demand a default flip that makes every pod log.Fatalf on first boot." >&2
  exit 2
fi
# CONTROL 2 - the apparatus does not over-fire, and this is the twin that
# matters. A stripper that deleted everything passes control 1 perfectly and
# silences the real thing. Three fixtures because the three source spellings sit
# at three different indents and one of them is an argv element.
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
  printf '%s\n' "$_line" | _secret_sources | grep -q . || _c2_missed="${_c2_missed}
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
  printf '%s\n' "$_line" | _secret_sources | grep -q . || _e1_missed="${_e1_missed} [${_line}]"
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

# --- E2. NEGATIVE. The chart refuses an unsatisfiable true. -----------------
# Asserting the error TEXT, not merely a non-zero exit: several `fail`s and the
# schema also exit non-zero, so "it was rejected" alone does not prove THIS guard
# rejected it. The text asserted is the harm citation, because that is the part
# an operator needs and the part a weakened message would drop first.
_e2="$("$HELM" template t "$CHART" "${BASE[@]}" --set auth.requireStableSigningKey=true 2>&1)"
if printf '%s' "$_e2" | grep -q 'auth.requireStableSigningKey is true' \
   && printf '%s' "$_e2" | grep -q 'cmd/server_foreground.go:259'; then
  pass "chart refuses requireStableSigningKey=true with no secret source, citing the log.Fatalf"
else
  fail "chart did NOT refuse requireStableSigningKey=true, or the refusal stopped naming its harm"
  printf '%s\n' "$_e2" | sed 's/^/        | /' | head -3
fi

# --- E3. POSITIVE TWIN OF E2. The guard is not refusing everything. ---------
# config.existingSecret is the one shape where true is satisfiable: the operator
# writes their own settings.yaml and can configure server.secrets there. Without
# this assertion, a guard hardened into an unconditional refusal scores E2 green.
_e3="$("$HELM" template t "$CHART" "${BASE[@]}" --set auth.requireStableSigningKey=true \
         --set config.existingSecret=operator-settings 2>&1)"
if printf '%s\n' "$_e3" | grep -q 'SCION_REQUIRE_STABLE_SIGNING_KEY: "true"'; then
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

# --- E5. LANDING. The limb that fires when the secret arrives. --------------
_e5_src="$(printf '%s\n' "$_DEFAULT_RENDER" | _secret_sources)"
if [ -n "$_e5_src" ] && [ "$_e4_flag" = "false" ]; then
  fail "THE SESSION SECRET HAS LANDED AND THE DEFAULT WAS NOT FLIPPED."
  echo "        the default render now carries: ${_e5_src}"
  echo "        values.yaml still defaults auth.requireStableSigningKey to false, so this"
  echo "        release ships replicas that sign tokens each other reject - silently,"
  echo "        presenting as intermittent logouts rather than as a configuration error."
  echo "        SET IT TO true. This assertion stays red until you do."
# THIS ARM IS UNREACHABLE FROM THE DEFAULT RENDER TODAY AND IS KEPT ANYWAY.
# configmap-env.yaml's guard refuses true-without-existingSecret before any
# manifest is produced, so a default of true makes the render fail outright and
# E4 catches it first. The arm is here because that guard is one `{{- if }}` and
# a future phase that relaxes it - to allow true under some new secret shape -
# would otherwise silently remove the last check on this combination. Stated
# rather than deleted so nobody counts it as coverage it is not providing.
elif [ -z "$_e5_src" ] && [ "$_e4_flag" = "true" ]; then
  fail "default is true but the default render carries no session-secret source -- every pod would log.Fatalf at cmd/server_foreground.go:259 on first boot"
else
  pass "signing-key default agrees with the default render's secret sources (flag=\"${_e4_flag}\", sources=${_e5_src:-none})"
fi

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
