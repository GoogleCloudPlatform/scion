#!/usr/bin/env bash
# verify-destroy-protection.sh — proves destroy protection actually enforces.
#
# It exists because this kind of test is easy to pass for the wrong reason,
# and a destroy-protection test that passes for the wrong reason is worse
# than no test at all: it retires the question while leaving the estate
# unprotected.
#
# What this proves, with a hub present and protection on, in ONE run:
#
#   1. gcloud sql instances delete <sql>          MUST be refused
#   2. gcloud filestore instances delete <fs>     MUST be refused
#   3. terraform apply -var deletion_protection=false
#                                                 MUST be refused (interlock)
#   4. the GKE cluster                            NOT TESTED, NOT PROTECTED
#
# Row 4 is not an omission and must never be dropped. `google_container_cluster
# .deletion_protection` is a Terraform-side guard only; container v1 has no
# deletion-protection field at all (0 of 87 Cluster properties match
# delet|protect, verified against the discovery document, with a positive
# control). Three green rows with no fourth row read as "the estate is
# protected", which is false.
#
# THE CENTRAL RULE OF THIS SCRIPT
# ------------------------------
# A non-zero exit is NOT a pass. `gcloud sql instances delete` fails with a
# non-zero exit when:
#   - deletion protection refuses it            <- the only pass
#   - the caller lacks the permission (403)     <- proves nothing
#   - the instance does not exist (404)         <- proves nothing
#   - the API is disabled, quota, VPC-SC, ...   <- proves nothing
# All of these look identical at the exit-code level. This script classifies
# the refusal TEXT and treats "refused, but not by the guard" as RED, not as a
# pass. Permission loss is not hypothetical: the operator identity running
# this script has had a permission silently revoked mid-week before, with no
# announcement, which would otherwise have masqueraded as a passing test.
#
# And the delete attempts are followed by an existence check, because the
# operation's status is a claim and the resource still being there is the
# postcondition. This whole week has been that lesson.
#
# Usage:
#   ./verify-destroy-protection.sh --selftest
#   ./verify-destroy-protection.sh --run \
#       --project P --sql-instance tfha-pg \
#       --filestore tfha-nfs --filestore-zone us-central1-a \
#       [--tf-dir /path/to/shared/root] [--expect-commit <sha>] \
#       [--out destroy-protection-report.md]
#
# Exit codes
#   0  every required row passed (row 4 is informational and never fails)
#   1  a row failed, or was INCONCLUSIVE — read the table
#   2  the harness could not run (bad args, missing tool, no credentials)
#   3  refused to act for safety
#
# --selftest needs no GCP access and no credentials. Run it first. If the
# classifier cannot tell a 403 from a protection refusal on fixtures, nothing
# this script says about the real project is worth reading.

set -uo pipefail

# Global cleanup: every mktemp'd file/dir in this script is appended to
# TEMPS, and this trap removes them all on any exit path (normal, error, or
# an early `return`/`exit` from a helper). Declaring TEMPS=() before the
# trap means "${TEMPS[@]}" is always an empty-but-defined array expansion,
# not an unset one, so it is safe under `set -u` even before anything has
# been added to it.
TEMPS=()
trap 'rm -f "${TEMPS[@]}"' EXIT

PROG="$(basename "$0")"
MODE=""
PROJECT=""
SQL_INSTANCE=""
FS_INSTANCE=""
FS_ZONE=""
TF_DIR=""
OUT=""
GKE_CLUSTER=""   # recorded in row 5 only; NEVER passed to a delete
NAME_PREFIX="tfha"
REGION="us-central1"
REAL_DESTROY=no  # attempt 4b, opt-in, and gated on 4a having refused first
EXPECT_COMMIT="" # if set, --tf-dir's HEAD must match or the run refuses

RED=0            # count of failing / inconclusive rows
SWEEP_ERRORS=0   # sweeps that could not run — blind spots, not clean results
# Initialised, not merely declared. `declare -a ROWS` leaves the array UNSET,
# and under `set -u` the first `${#ROWS[@]}` is "unbound variable". On a live
# run (2026-09-27) that exact expansion, inside a stop function, aborted
# the function before its `exit` and the run carried on past a tripped
# precondition. Selftest row "(b)" pins this.
ROWS=()          # rendered result rows

# ---------------------------------------------------------------------------
# Classifier
# ---------------------------------------------------------------------------
# classify <kind> <exit_code> <stderr_text>  ->  one of:
#   PROTECTED     the guard refused it                       (the only pass)
#   DELETED       the command SUCCEEDED                      (catastrophic)
#   DENIED        permission / auth refused it               (inconclusive)
#   NOTFOUND      the resource is not there                  (inconclusive)
#   OTHER         refused for some fourth reason             (inconclusive)
#
# kind is sql | filestore | terraform.
#
# Matching is case-insensitive and deliberately narrow. If GCP rewords a
# message, this goes red and a human looks — which is the correct outcome. A
# loose pattern that keeps passing through a reword is how a dead test survives
# for a year.
classify() {
  local kind="$1" rc="$2" txt="$3"
  local low
  low="$(printf '%s' "$txt" | tr '[:upper:]' '[:lower:]')"

  # Success is checked first and unconditionally. If the delete worked, no
  # amount of interesting stderr matters.
  if [[ "$rc" -eq 0 ]]; then
    echo DELETED; return
  fi

  # Order matters: check the disqualifiers BEFORE the pass pattern. A 403 body
  # that happens to contain the words "deletion protection" (for example, an
  # error listing the permission `cloudsql.instances.update` needed to change
  # deletion protection) must not be scored as a pass.
  case "$low" in
    *permission_denied*|*"403"*|*"does not have permission"*|\
    *"permission \""*|*"required \"cloudsql"*|*"required \"file."*|\
    *"caller does not have"*|*"insufficient authentication"*|\
    *"reauthentication"*|*"invalid_grant"*)
      echo DENIED; return ;;
  esac
  case "$low" in
    *not_found*|*"404"*|*"was not found"*|*"does not exist"*|\
    *"no such instance"*)
      echo NOTFOUND; return ;;
  esac

  case "$kind" in
    sql|filestore)
      # The one thing that counts.
      #
      # CAPTURED 2026-09-23 against tfha-pg / tfha-nfs. The live wording is NOT
      # what I had reconstructed, and the difference matters:
      #
      #   sql:       "The instance is protected. Please disable the deletion
      #               protection and try again."
      #   filestore: "... is protected from deletion: <deletion_protection_reason>"
      #
      # Filestore never says "deletion protection" at all. The old pattern
      # matched it only because OUR OWN reason string happens to contain the
      # literal `deletion_protection` — i.e. the test was passing on text the
      # operator supplies, not on text the API guarantees. Reword the reason in
      # modules/filestore and this would have gone INCONCLUSIVE with the guard
      # working perfectly. Hence "protected from deletion" below, and a selftest
      # row that strips the reason string entirely.
      case "$low" in
        *"deletion protection"*|*"deletionprotection"*|\
        *"deletion_protection"*|\
        *"protected from deletion"*|*"instance is protected"*)
          echo PROTECTED; return ;;
      esac
      ;;
    terraform)
      # The interlock is a lifecycle precondition, not an API refusal, so it
      # has a completely different shape and must not share the sql pattern.
      case "$low" in
        *"resource precondition failed"*|*"precondition failed"*|\
        *"destroy_guard"*)
          echo PROTECTED; return ;;
      esac
      ;;
    prevent_destroy)
      # Attempt 4. Different again: this is Terraform's own plan-time refusal,
      # emitted before any provider is asked to delete anything. Captured
      # verbatim from terraform 1.9.8, so unlike the API patterns this one is
      # CONFIRMED rather than reconstructed:
      #
      #   Error: Instance cannot be destroyed
      #   Resource <addr> has lifecycle.prevent_destroy set, but the plan
      #   calls for this resource to be destroyed.
      #
      # Deliberately NOT sharing the precondition pattern above: a run that
      # failed on the interlock instead of on prevent_destroy would be a
      # different (and much weaker) result wearing the same green tick.
      case "$low" in
        *"prevent_destroy"*|*"instance cannot be destroyed"*)
          echo PROTECTED; return ;;
      esac
      ;;
  esac

  echo OTHER
}

# verdict_for <classification> -> PASS | FAIL | INCONCLUSIVE
verdict_for() {
  case "$1" in
    PROTECTED) echo PASS ;;
    DELETED)   echo FAIL ;;
    *)         echo INCONCLUSIVE ;;
  esac
}

# How many pass-fixtures are still guesses rather than captured output.
# Derived by counting tagged fixture calls, so it cannot drift from them.
# Anchored on the call shape so the surrounding prose about UNCONFIRMED is not
# itself counted — a counter that counts its own documentation is the self-lint
# bug again.
unconfirmed_count() {
  grep -cE '^  t "UNCONFIRMED ' "$0" || true
}

explain() {
  case "$1" in
    PROTECTED) echo "refused by the guard, and said so" ;;
    DELETED)   echo "THE DELETE SUCCEEDED — the guard is not in effect" ;;
    DENIED)    echo "refused by IAM, not by the guard — proves nothing" ;;
    NOTFOUND)  echo "resource absent — nothing was under test" ;;
    OTHER)     echo "refused for an unrecognised reason — read the stderr" ;;
  esac
}

# ---------------------------------------------------------------------------
# Self-test
# ---------------------------------------------------------------------------
# Fixtures are real-shaped gcloud/terraform stderr. The two marked UNCONFIRMED
# are the exact strings this harness has never yet seen from the live API; on
# the first real run their actual text must be pasted back in here. Until then
# they are a best-effort guess and are labelled as such, rather than being
# quietly trusted.
do_selftest() {
  local fails=0 n=0
  t() { # t <desc> <kind> <rc> <want> <text>
    local desc="$1" kind="$2" rc="$3" want="$4" txt="$5" got
    n=$((n+1))
    got="$(classify "$kind" "$rc" "$txt")"
    if [[ "$got" == "$want" ]]; then
      printf '  ok    %-52s -> %s\n' "$desc" "$got"
    else
      printf '  FAIL  %-52s -> %s (want %s)\n' "$desc" "$got" "$want"
      fails=$((fails+1))
    fi
  }

  echo "=== classifier fixtures ==="

  # --- the passes ---
  #
  # Each pass fixture is tagged UNCONFIRMED until it has been replaced with
  # stderr captured from a real refusal. The tag is not decoration: the
  # "unconfirmed" warning printed by both --selftest and --run is DERIVED by
  # counting these tags (see unconfirmed_count), never stated separately.
  #
  # That is deliberate: the fixtures and the UNCONFIRMED marker must be
  # updated in the same change. A marker that has to be remembered will
  # eventually contradict the fixtures — a doc-comment saying "confirmed"
  # above a guess is worse than no marker, because it is actively reassuring.
  # Deriving it makes the two physically incapable of disagreeing: delete the
  # tag when you paste in the real text, and the warning updates itself.
  # CAPTURED 2026-09-23 from tfha-pg and tfha-nfs in example-project, at the one
  # moment when attempting a real delete was free: both instances existed with
  # protection on and held no hub data, so a guard failure would have cost a
  # 10-minute re-apply instead of a hub's database. Both refused; both were
  # still present afterwards.
  t "sql: deletion protection refusal (captured)" sql 1 PROTECTED \
    'ERROR: (gcloud.sql.instances.delete) HTTPError 400: The instance is protected. Please disable the deletion protection and try again. To disable deletion protection, update the instance settings with deletionProtectionEnabled set to false.'
  t "filestore: deletion protection refusal (captured)" filestore 1 PROTECTED \
    'ERROR: (gcloud.filestore.instances.delete) FAILED_PRECONDITION: instance "projects/example-project/locations/us-central1-a/instances/tfha-nfs" is protected from deletion: Shared Filestore instance; hubs read it via shared-lookup. Set deletion_protection = false to destroy.'
  # THE IMPORTANT ONE. Same refusal with our own deletion_protection_reason
  # removed, i.e. only the words the Filestore API guarantees. The previous
  # pattern failed this: it matched the literal `deletion_protection` inside a
  # string WE wrote, so the test was measuring our config, not the guard. A
  # reworded reason would have turned a working guard red.
  t "filestore: refusal minus our own reason string" filestore 1 PROTECTED \
    'ERROR: (gcloud.filestore.instances.delete) FAILED_PRECONDITION: instance "projects/example-project/locations/us-central1-a/instances/tfha-nfs" is protected from deletion: .'
  # And the sql refusal truncated to its first sentence, which is all some
  # gcloud versions print.
  t "sql: refusal, first sentence only" sql 1 PROTECTED \
    'ERROR: (gcloud.sql.instances.delete) HTTPError 400: The instance is protected.'
  # CAPTURED, not reconstructed — hence no UNCONFIRMED tag. terraform 1.9.8,
  # 2026-09-23, real refusal from a real Cloud SQL instance (example-project
  # scion-hub-db, which holds the non-postgres database "scionhub"), against
  # the destroy_guard block from configurations/shared-infra. Note it refuses
  # at PLAN time and names the offending database, which is what makes the
  # message actionable rather than merely red.
  t "terraform: interlock precondition (captured)" terraform 1 PROTECTED \
    'Error: Resource precondition failed
  on main.tf line 40, in resource "terraform_data" "destroy_guard":
  40:       condition     = var.deletion_protection || length(local.hub_dbs) == 0
    ├────────────────
    │ local.hub_dbs is list of string with 1 element
    │ var.deletion_protection is false

destroy_guard: hubs still exist on this shared infra (databases: scionhub).
Destroy every hub root first.
destroy_guard: cannot disable deletion protection while hubs exist on tfha-pg.'

  # Attempt 4. NOT tagged UNCONFIRMED: this is terraform 1.9.8's own output,
  # captured verbatim from a real refused `plan -destroy`, not reconstructed.
  # Terraform's wording is ours to pin; GCP's is not.
  t "prevent_destroy: plan-time refusal (captured)" prevent_destroy 1 PROTECTED \
    'Error: Instance cannot be destroyed

  on main.tf line 1:
   1: resource "terraform_data" "keeper" {

Resource terraform_data.keeper has lifecycle.prevent_destroy set, but the
plan calls for this resource to be destroyed. To avoid this error and
continue with the plan, either disable lifecycle.prevent_destroy or reduce
the scope of the plan using the -target option.'

  # A destroy plan that SUCCEEDS is the catastrophe for attempt 4: it means
  # prevent_destroy is absent and the estate is one keystroke from gone.
  t "prevent_destroy: a clean destroy plan is FAIL" prevent_destroy 0 DELETED \
    'Plan: 0 to add, 0 to change, 14 to destroy.'
  # Refused, but by the interlock rather than by prevent_destroy. Weaker result,
  # must not wear the same tick — this is what keeps the two kinds separate.
  t "prevent_destroy: interlock text is NOT a pass here" prevent_destroy 1 OTHER \
    'Error: Resource precondition failed
destroy_guard: hubs still exist on this shared infra'
  # ...and the converse, so the separation is pinned in both directions.
  t "terraform kind: prevent_destroy text is NOT a pass" terraform 1 OTHER \
    'Error: Instance cannot be destroyed — lifecycle.prevent_destroy set'

  # --- the disqualifiers: every one of these is a non-zero exit ---
  t "sql: 403 must NOT pass" sql 1 DENIED \
    'ERROR: (gcloud.sql.instances.delete) HTTPError 403: The client is not authorized to make this request.'
  t "filestore: PERMISSION_DENIED must NOT pass" filestore 1 DENIED \
    'ERROR: (gcloud.filestore.instances.delete) PERMISSION_DENIED: Permission file.instances.delete denied on resource'
  t "sql: 404 must NOT pass" sql 1 NOTFOUND \
    'ERROR: (gcloud.sql.instances.delete) HTTPError 404: The Cloud SQL instance does not exist.'
  t "filestore: NOT_FOUND must NOT pass" filestore 1 NOTFOUND \
    'ERROR: (gcloud.filestore.instances.delete) NOT_FOUND: Instance was not found.'
  t "expired credentials must NOT pass" sql 1 DENIED \
    'ERROR: (gcloud.sql.instances.delete) There was a problem refreshing your current auth tokens: invalid_grant'
  # I first wrote this expecting OTHER and it came back DENIED, which turned out
  # to be the fixture being wrong, not the classifier: an API-disabled error
  # really is served as a 403. DENIED is the honest label, and either way the
  # verdict is INCONCLUSIVE, so nothing downstream changes. Recording the
  # correction rather than silently editing the expectation, because "my test
  # disagreed with my code so I changed the test" is how a suite rots.
  t "API disabled reads as DENIED (still not a pass)" sql 1 DENIED \
    'ERROR: HTTPError 403: Cloud SQL Admin API has not been used in project 1 before or it is disabled.'
  t "unrecognised refusal is OTHER" filestore 1 OTHER \
    'ERROR: (gcloud.filestore.instances.delete) INTERNAL: backend error'

  # --- the adversarial one: the whole reason disqualifiers are checked first ---
  t "403 mentioning deletion protection must NOT pass" sql 1 DENIED \
    'ERROR: HTTPError 403: Permission cloudsql.instances.update is required to change deletion protection.'

  # --- catastrophe ---
  t "rc=0 is FAIL even with scary stderr" sql 0 DELETED \
    'WARNING: deletion protection was ignored'
  t "rc=0 with empty stderr" filestore 0 DELETED ''

  # --- cross-kind leakage: the sql pattern must not pass a terraform run ---
  t "tf run quoting 'deletion protection' is not a pass" terraform 1 OTHER \
    'Error: Provider produced inconsistent result: deletion protection drift'
  # ...and the terraform pattern must not pass a sql run
  t "sql run quoting 'precondition failed' is not a pass" sql 1 OTHER \
    'ERROR: (gcloud.sql.instances.delete) precondition failed on the backend'

  echo
  echo "=== verdict mapping ==="
  local c
  for c in PROTECTED:PASS DELETED:FAIL DENIED:INCONCLUSIVE \
           NOTFOUND:INCONCLUSIVE OTHER:INCONCLUSIVE; do
    local k="${c%%:*}" want="${c##*:}" got
    n=$((n+1)); got="$(verdict_for "$k")"
    if [[ "$got" == "$want" ]]; then
      printf '  ok    %-52s -> %s\n' "$k" "$got"
    else
      printf '  FAIL  %-52s -> %s (want %s)\n' "$k" "$got" "$want"
      fails=$((fails+1))
    fi
  done

  # --- self-lint: the pipefail SIGPIPE trap ---
  #
  # A literal-matching lint matches ITSELF. An earlier version of this lint
  # flagged its own comments; the first draft of this one flagged its own
  # failure message,
  # which is the same bug wearing a different hat. A lint whose own text is a
  # violation is unusable, and the temptation is to loosen the pattern until it
  # stops complaining — which is how you end up with a lint that catches
  # nothing.
  #
  # The fix is structural, not a looser pattern: assemble the forbidden literal
  # at RUNTIME so the sequence never appears in the file at all. Nothing below
  # needs an exemption, so there is no exemption to over-apply later.
  echo
  echo "=== self-lint ==="

  # Positive control for the self-lints themselves: prove the source is
  # readable BEFORE trusting anything these lints fail to find.
  #
  # Both lints below inspect "$0". If the script is piped to bash
  # (`cat verify-destroy-protection.sh | bash -s -- --selftest`, an easy thing
  # for CI to do) then "$0" is not this file and every grep over it returns
  # nothing. The two lints then behave in OPPOSITE ways:
  #
  #   - the stated-gap lint asserts a COUNT of 2, finds 0, and fails closed;
  #   - the SIGPIPE lint asserts an ABSENCE, finds 0 offenders, and prints
  #     "ok" — a vacuous pass, indistinguishable from a clean file.
  #
  # Verified by doing it: piped, the count lint goes red and the SIGPIPE lint
  # goes green. So an absence assertion over a medium that might be missing is
  # not a check at all; it is a check-shaped way of reading nothing. Same shape
  # as the 403/404 problem and as the empty-sweep-diffs-clean trap in
  # snapshot(): you cannot trust what an instrument does not find until you
  # have shown the instrument can find something.
  local src_ok=no
  if [[ -r "$0" ]] && grep -q 'THE CENTRAL RULE OF THIS SCRIPT' "$0" 2>/dev/null; then
    src_ok=yes
  fi
  n=$((n+1))
  if [[ "$src_ok" == yes ]]; then
    printf '  ok    %-52s\n' "source readable — source lints are meaningful"
  else
    # Literal "$0" in the message text below, not an expansion.
    # shellcheck disable=SC2016
    printf '  FAIL  %-52s\n' 'cannot read $0: source lints below are VACUOUS'
    printf '        run the script by path, do not pipe it into bash\n'
    fails=$((fails+1))
  fi

  local offenders forbidden
  forbidden='\|[[:space:]]*grep -'"q"
  offenders="$(grep -nE "$forbidden" "$0" | grep -vE '^[0-9]+:[[:space:]]*#' || true)"
  n=$((n+1))
  if [[ "$src_ok" != yes ]]; then
    printf '  FAIL  %-52s\n' "SIGPIPE lint SKIPPED (source unreadable), not passed"
    fails=$((fails+1))
  elif [[ -n "$offenders" ]]; then
    printf '  FAIL  piping into a quiet grep under pipefail is a SIGPIPE race:\n%s\n' "$offenders"
    fails=$((fails+1))
  else
    printf '  ok    %-52s\n' "no pipe into a quiet grep in a pipefail script"
  fi
  # The stated-gap rows must be unconditional. If someone later makes one
  # depend on a flag, the gap disappears from the results table and the
  # remaining green rows read as full coverage.
  #
  # THIS CHECK WAS BROKEN AND PASSING. The first version did:
  #     grep -q 'ROW 4 IS UNCONDITIONAL' "$0"
  # ...and the file contains that literal — inside the grep command itself. It
  # matched its own source line, so it returned ok unconditionally, including
  # after I renamed the row and the real marker was gone. Same self-matching
  # root cause as the SIGPIPE lint above, but inverted: there it produced a
  # false FAIL, which is loud and gets fixed in a minute. Here it produced a
  # false PASS, which is silent and would have survived indefinitely.
  #
  # Fix is the same shape: assemble the marker at runtime so the literal never
  # appears in the file except where it is meant to, and assert on a COUNT
  # rather than on presence, so deleting one of two rows is still caught.
  local marker want_rows got_rows
  marker='@unconditional-'"row"
  want_rows=2
  got_rows="$(grep -cF "$marker" "$0" || true)"
  n=$((n+1))
  if [[ "$got_rows" -eq "$want_rows" ]]; then
    printf '  ok    %-52s (%s)\n' "stated-gap rows still unconditional" "$got_rows"
  else
    printf '  FAIL  expected %s unconditional stated-gap rows, found %s — a gap can vanish silently\n' \
      "$want_rows" "$got_rows"
    fails=$((fails+1))
  fi

  # --- attempt 4's snapshot comparison -------------------------------------
  #
  # The "Estate unchanged" row diffs the before-snapshot against the
  # after-snapshot. Any content that necessarily differs between the two makes
  # that row red on every run. It did: the headers carried "(before)" and
  # "(after)". The selftest never caught it because snapshot() needs gcloud, so
  # the defect would have surfaced for the first time on a live run, as
  # "THE ESTATE CHANGED during attempt 4".
  #
  # So the headers are now pure functions with no arguments to get wrong, and
  # they are tested here with no credentials. cmp_snap() below is the same
  # comparison do_attempt4 performs.
  echo
  echo "=== attempt 4 snapshot comparison ==="
  cmp_snap() { # cmp_snap <a> <b> -> IDENTICAL | DIFFERS
    if diff -u --label before --label after "$1" "$2" >/dev/null 2>&1
    then echo IDENTICAL; else echo DIFFERS; fi
  }
  ts() { # ts <desc> <want> <a-body> <b-body>
    local desc="$1" want="$2" a b got
    a="$(mktemp)"; b="$(mktemp)"
    { snapshot_header; printf '%s\n' "$3"; snapshot_sweep_header "$NAME_PREFIX"; } > "$a"
    { snapshot_header; printf '%s\n' "$4"; snapshot_sweep_header "$NAME_PREFIX"; } > "$b"
    n=$((n+1)); got="$(cmp_snap "$a" "$b")"; rm -f "$a" "$b"
    if [[ "$got" == "$want" ]]; then
      printf '  ok    %-52s -> %s\n' "$desc" "$got"
    else
      printf '  FAIL  %-52s -> %s (want %s)\n' "$desc" "$got" "$want"
      fails=$((fails+1))
    fi
  }
  # The regression: two snapshots of an unchanged estate must compare equal.
  ts "unchanged estate compares IDENTICAL" IDENTICAL \
    'sql: tfha-pg' 'sql: tfha-pg'
  # Negative controls — the comparison must still detect a real partial destroy.
  ts "a resource disappearing is caught" DIFFERS \
    $'sql: tfha-pg\nrouter: tfha-nat' 'sql: tfha-pg'
  ts "a resource appearing is caught" DIFFERS \
    'sql: tfha-pg' $'sql: tfha-pg\nrouter: tfha-nat'
  # A blind sweep must not diff as clean against a working one.
  ts "sweep ERROR vs real result is caught" DIFFERS \
    'sql: ERROR — permission denied' 'sql: tfha-pg'
  # And the headers themselves must carry no before/after label.
  #
  # This is NOT redundant with the four ts() cases above, which is worth
  # knowing before someone deletes it as duplication. ts() builds both sides by
  # calling the same snapshot_header, so a label reintroduced there appears on
  # both sides and cancels out: re-adding "(before)" to snapshot_header leaves
  # all four ts() rows green and turns only this row red (verified by doing
  # it). ts() tests the comparison; this tests the content. The bug was in the
  # content.
  # No pipe into a quiet grep here: under `set -o pipefail` that is the same
  # SIGPIPE race the self-lint above checks for, and this script's own
  # self-lint flagged this very line when it was first written that way.
  # Capture, then match in the shell.
  local hdrs
  hdrs="$( { snapshot_header; snapshot_sweep_header "$NAME_PREFIX"; } )"
  n=$((n+1))
  if [[ "$hdrs" == *[Bb]efore* || "$hdrs" == *[Aa]fter* ]]; then
    printf '  FAIL  %-52s\n' "snapshot headers leak a before/after label"
    fails=$((fails+1))
  else
    printf '  ok    %-52s\n' "snapshot headers carry no before/after label"
  fi

  # --- the commit under test -------------------------------------------------
  #
  # The bug this encodes: I reviewed one commit and planned another, then filed
  # a BLOCKING bug against code that had already been fixed. Nothing lied to me
  # — my own output printed the wrong HEAD and I read past it. So the check has
  # to be in the command, and it has to fail closed.
  echo
  echo "=== commit assertion ==="
  tc() { # tc <desc> <want-result> <expect> <head>
    local desc="$1" want="$2" got
    n=$((n+1)); got="$(commit_matches "$3" "$4")"
    if [[ "$got" == "$want" ]]; then
      printf '  ok    %-52s -> %s\n' "$desc" "$got"
    else
      printf '  FAIL  %-52s -> %s (want %s)\n' "$desc" "$got" "$want"
      fails=$((fails+1))
    fi
  }
  tc "exact sha matches"            yes \
    70ddef78c0000000000000000000000000000000 70ddef78c0000000000000000000000000000000
  tc "short prefix matches full head" yes \
    70ddef78c 70ddef78c0000000000000000000000000000000
  tc "a different commit is refused" no \
    9c488c628 70ddef78c0000000000000000000000000000000
  # The one that matters most. "I could not determine the commit" must never
  # render as "the commit is the one you asked for" — same rule as trap 3.
  tc "UNKNOWN head fails closed"    no  70ddef78c UNKNOWN
  tc "empty head fails closed"      no  70ddef78c ""
  # An empty EXPECTATION is the dangerous direction: `[[ $got == * ]]` is true
  # for everything, so a dropped guard turns "nobody said which commit" into
  # "the commit is correct". No expectation is not a satisfied expectation.
  tc "empty expectation never matches" no "" 70ddef78c0000000000000000000000000000000
  # A prefix of the EXPECTATION is not a match: expecting a full sha and
  # getting a short one is an unverified claim, not a verified one.
  tc "head shorter than expectation is refused" no \
    70ddef78c0000000000000000000000000000000 70ddef78c
  # Positive control for repo_head itself. Without this, every "fails closed"
  # row above is also satisfied by a repo_head that can never find anything,
  # and the assertion would refuse every run for the wrong reason. Prove the
  # instrument can read a real HEAD before trusting its UNKNOWNs.
  local rh_dir rh
  rh_dir="$(mktemp -d)"
  if command -v git >/dev/null 2>&1 \
     && git -C "$rh_dir" init -q 2>/dev/null \
     && git -C "$rh_dir" -c user.email=s@e -c user.name=s \
          commit -q --allow-empty -m x 2>/dev/null; then
    rh="$(repo_head "$rh_dir")"
    n=$((n+1))
    if [[ "$rh" =~ ^[0-9a-f]{40}$ ]]; then
      printf '  ok    %-52s\n' "repo_head reads a real HEAD (control)"
      n=$((n+1))
      if [[ "$(commit_matches "${rh:0:8}" "$rh")" == yes ]]; then
        printf '  ok    %-52s\n' "real HEAD matches its own short prefix"
      else
        printf '  FAIL  %-52s\n' "real HEAD did not match its own short prefix"
        fails=$((fails+1))
      fi
    else
      printf '  FAIL  %-52s -> %s\n' "repo_head reads a real HEAD (control)" "$rh"
      fails=$((fails+1))
    fi
  else
    printf '  SKIP  %-52s (no git) — SKIPPED, not passed\n' "repo_head positive control"
    fails=$((fails+1))
  fi
  n=$((n+1))
  if [[ "$(repo_head "$rh_dir/definitely-not-here")" == UNKNOWN ]]; then
    printf '  ok    %-52s\n' "repo_head on a non-repo path -> UNKNOWN"
  else
    printf '  FAIL  %-52s\n' "repo_head on a non-repo path did not say UNKNOWN"
    fails=$((fails+1))
  fi
  rm -rf "$rh_dir"

  selftest_regressions

  echo
  if [[ "$fails" -eq 0 ]]; then
    echo "selftest: ${n}/${n} passed"
    local u; u="$(unconfirmed_count)"
    if [[ "$u" -gt 0 ]]; then
      echo
      echo "NOTE: ${u} PROTECTED fixture(s) still carry UNCONFIRMED wording —"
      echo "      reconstructed, not captured. On the first real run, paste the"
      echo "      live stderr in and drop the tag; this count follows the tags."
      echo "      Until then a red destroy-protection check may mean GCP reworded, not that the guard"
      echo "      is missing. Check the captured stderr before concluding."
    else
      echo
      echo "All PROTECTED fixtures are captured from live refusals."
    fi
    return 0
  fi
  echo "selftest: ${fails} of ${n} FAILED — do not trust this harness"
  return 1
}

# ---------------------------------------------------------------------------
# Real run
# ---------------------------------------------------------------------------
# attempt <kind> <label> <resource> <cmd...>
# Runs the command, classifies, then checks the POSTCONDITION: is the resource
# still there? A guard that refuses but leaves the thing deleted, or an
# "inconclusive" that turns out to have deleted it anyway, both need catching.
attempt() {
  local kind="$1" label="$2" resource="$3"; shift 3
  local err rc cls base verdict still note

  echo "--- ${label}: ${*}" >&2
  err="$("$@" 2>&1 >/dev/null)"; rc=$?
  cls="$(classify "$kind" "$rc" "$err")"
  base="$cls"
  verdict="$(verdict_for "$cls")"

  still="$("exists_${kind}" "$resource")"
  if [[ "$still" != "yes" ]]; then
    # The postcondition beats the classification, always.
    verdict=FAIL
    cls="GONE-${cls}"
  fi

  [[ "$verdict" == "PASS" ]] || RED=$((RED+1))

  note="$(explain "$base")"
  [[ "$cls" == GONE-* ]] && note="RESOURCE IS GONE after the attempt — ${note}"

  ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
    "$label" "$resource" "$cls" "$verdict" "$note")")

  {
    printf '%s\n' "### ${label} (${resource}) — ${verdict} [${cls}]"
    printf '%s\n' '```'
    printf 'exit: %s\nstill present after attempt: %s\n\n%s\n' "$rc" "$still" "$err"
    printf '%s\n\n' '```'
  } >> "$OUT"
}

exists_sql() {
  gcloud sql instances describe "$1" --project="$PROJECT" \
    --format='value(name)' >/dev/null 2>&1 && echo yes || echo no
}
exists_filestore() {
  gcloud filestore instances describe "$1" --project="$PROJECT" \
    --zone="$FS_ZONE" --format='value(name)' >/dev/null 2>&1 && echo yes || echo no
}
exists_terraform() { echo yes; }  # nothing to describe; the apply is the subject
exists_prevent_destroy() { echo yes; }  # attempt 4 verifies via the snapshot diff

# --- which commit is actually under test ------------------------------------
#
# Added after I reported a BLOCKING bug in a Terraform root that did not have
# it: I had fetched the branch and diffed against it, but never checked the
# working tree out, so the plan ran against the previous commit. The reviewer
# caught it. My own output had printed the wrong HEAD and I read past it.
#
# A destroy-protection report that names the wrong commit is the exact failure
# class this harness exists to prevent: a confident table about code nobody is
# shipping. So the SHA is asserted by the run, not remembered by the operator,
# and an unknown HEAD fails closed rather than passing quietly.
repo_head() { # repo_head <dir> -> full sha | UNKNOWN
  local d="$1" out
  [[ -n "$d" && -d "$d" ]] || { echo UNKNOWN; return; }
  out="$(git -C "$d" rev-parse HEAD 2>/dev/null)" || { echo UNKNOWN; return; }
  [[ -n "$out" ]] && echo "$out" || echo UNKNOWN
}

commit_matches() { # commit_matches <want> <got> -> yes|no
  local want="$1" got="$2"
  # No expectation, an unknown HEAD, or an empty SHA can never be a match.
  # "I could not determine the commit" must not render as "the commit is right".
  [[ -z "$want" || -z "$got" || "$got" == UNKNOWN ]] && { echo no; return; }
  [[ "$got" == "$want"* ]] && echo yes || echo no
}

# --- attempt 4 support: whole-estate snapshot -------------------------------
#
# The other three attempts check one resource each. Attempt 4's failure mode is
# different in kind: a destroy that is refused on the protected resources but
# proceeds on the plumbing around them. A per-resource existence check cannot
# see that, so attempt 4 diffs the WHOLE tfha estate before and after.
#
# Every sweep records an explicit ERROR line rather than an empty result when
# the command fails. A permission loss mid-run would otherwise produce an empty
# "before" and an empty "after", which diff as identical — a silent pass built
# out of two blind spots. Same 403/404 trap as everywhere else.
#
# The two headers below are emitted by pure functions and carry NO before/after
# label. That is the whole point: these two files are diffed AGAINST EACH OTHER,
# so anything that necessarily differs between them makes the postcondition red
# on every run, including runs where the estate is untouched. The first draft
# printed "## terraform state (before)" vs "(after)" and would have failed the
# must-pass "Estate unchanged" row on every single live run — reported as
# "THE ESTATE CHANGED during attempt 4", the most alarming false alarm this
# harness can raise, in the one check whose entire job is spotting a partial
# destroy. Identical defect to comparing an IAM policy's `etag` field: a
# churn field that changes regardless of real content makes an equality
# check permanently red (or, the mirror image, masks a real change if it's
# excluded carelessly). The label belongs on the diff, not in the content.
snapshot_header()       { printf '## terraform state\n'; }
snapshot_sweep_header() { printf '\n## gcloud sweep for %s*\n' "$1"; }

snapshot() {
  local f="$1" kind out
  : > "$f"
  {
    snapshot_header
    if [[ -n "$TF_DIR" ]]; then
      if out="$(terraform -chdir="$TF_DIR" state list 2>&1)"; then
        printf '%s\n' "$out" | sort
      else
        printf 'ERROR:terraform-state-list\n%s\n' "$out"
        SWEEP_ERRORS=$((SWEEP_ERRORS+1))
      fi
    else
      printf 'ERROR:no-tf-dir\n'
      SWEEP_ERRORS=$((SWEEP_ERRORS+1))
    fi
    snapshot_sweep_header "$NAME_PREFIX"
  } >> "$f"

  # Each kind is listed by name only and filtered client-side, so a
  # server-side filter typo cannot quietly return nothing.
  #
  # 2026-09-27: these used to be strings run through `eval`, and the
  # unquoted `--format=value(name)` is a bash syntax error under eval. EVERY
  # sweep failed on a live run, on both sides, and the "Estate unchanged"
  # row compared two identical blind spots and scored them PASS. Now each is a
  # real argv (sweep_list), with no eval and no string re-parsing.
  local kind_list="network subnet address globaladdress router sql filestore gke artifactrepo serviceaccount secret run bucket"
  local matched=0 hits
  for kind in $kind_list; do
    if out="$(sweep_list "$kind" 2>&1)"; then
      hits="$(printf '%s\n' "$out" | grep -F "$NAME_PREFIX" || true)"
      if [[ -n "$hits" ]]; then
        printf '%s\n' "$hits" | sed "s/^/${kind}: /" >> "$f"
        matched=$((matched + $(printf '%s\n' "$hits" | wc -l)))
      fi
    else
      printf '%s: ERROR — %s\n' "$kind" "$(printf '%s' "$out" | head -1)" >> "$f"
      SWEEP_ERRORS=$((SWEEP_ERRORS+1))
    fi
  done

  # A sweep that finds NOTHING is a blind instrument until proven otherwise —
  # prove the instrument can find something before trusting what it didn't
  # find. This script only runs with the shared estate present, so zero
  # ${NAME_PREFIX} resources across
  # every kind means the sweep is broken (wrong project, wrong prefix, lost
  # permission), not that the estate is empty. Two empty snapshots diff as
  # identical, which is a silent pass built out of two blind spots.
  if [[ "$matched" -eq 0 ]]; then
    printf 'ERROR:no-%s-resources-found — the sweep saw nothing; treated as blind\n' \
      "$NAME_PREFIX" >> "$f"
    SWEEP_ERRORS=$((SWEEP_ERRORS+1))
  fi
  [[ "$SWEEP_ERRORS" -eq 0 ]]
}

# sweep_list <kind>. One gcloud list per kind, as a real argv. It is kept out of
# snapshot() so the selftest can drive snapshot() against a stub gcloud.
sweep_list() {
  local p="--project=${PROJECT}"
  case "$1" in
    network)        gcloud compute networks list "$p" --format='value(name)' ;;
    subnet)         gcloud compute networks subnets list "$p" --format='value(name)' ;;
    address)        gcloud compute addresses list "$p" --format='value(name)' ;;
    globaladdress)  gcloud compute addresses list "$p" --global --format='value(name)' ;;
    router)         gcloud compute routers list "$p" --format='value(name)' ;;
    sql)            gcloud sql instances list "$p" --format='value(name)' ;;
    filestore)      gcloud filestore instances list "$p" --format='value(name)' ;;
    gke)            gcloud container clusters list "$p" --format='value(name)' ;;
    artifactrepo)   gcloud artifacts repositories list "$p" --format='value(name)' ;;
    serviceaccount) gcloud iam service-accounts list "$p" --format='value(email)' ;;
    secret)         gcloud secrets list "$p" --format='value(name)' ;;
    run)            gcloud run services list "$p" --region="${REGION}" --format='value(metadata.name)' ;;
    bucket)         gcloud storage buckets list "$p" --format='value(name)' ;;
    *)              echo "unknown sweep kind $1" >&2; return 2 ;;
  esac
}

# --- attempt 4: the direct destroy, which must abort at PLAN time ----------
#
# Two stages, and the order is the whole safety argument.
#
#   4a  terraform plan -destroy -var deletion_protection=false
#       Read-only. Cannot mutate anything. Must be REFUSED by prevent_destroy.
#
#   4b  terraform destroy -auto-approve -var deletion_protection=false
#       The real thing. Only runs if 4a refused, and only with an explicit
#       --with-real-destroy.
#
# 4b is gated on 4a because if prevent_destroy is missing or misplaced, 4a says
# so for free and 4b would DESTROY THE SHARED ESTATE to find out the same fact.
# A test that inflicts the failure it is checking for is not a test. The
# read-only probe is also the more honest assertion: what's actually required
# is plan-time refusal, and only 4a actually observes the plan phase in isolation.
#
# Note, from the verbatim refusal: Terraform's own error ends with "...or
# reduce the scope of the plan using the -target option." The refusal message
# advertises its own bypass. That is precisely why the -target case is a
# written-down residual and not an afterthought.
do_attempt4() {
  if [[ -z "$TF_DIR" ]]; then
    ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
      "Direct destroy (plan-time)" "shared root" "SKIPPED" "INCONCLUSIVE" \
      "no --tf-dir given; the direct-destroy path was not exercised")")
    RED=$((RED+1)); return
  fi

  local before after
  before="$(mktemp)"; after="$(mktemp)"
  TEMPS+=("$before" "$after")
  # Fail CLOSED on a blind before-snapshot. Without a trustworthy
  # "before" there is no way to detect a partial destroy, so the destroy
  # attempts must not run at all.
  if ! snapshot "$before"; then
    ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
      "Estate snapshot (before)" "${NAME_PREFIX}-*" "BLIND" "FAIL" \
      "the before-sweep errored or found no ${NAME_PREFIX} resources; attempt 4 NOT run, since a partial destroy would be undetectable")")
    RED=$((RED+1))
    { printf '### Estate snapshot (before) — BLIND\n\n```\n'; cat "$before"; printf '```\n\n'; } >> "$OUT"
    return
  fi

  attempt prevent_destroy "Direct destroy (plan-time)" "shared root" \
    terraform -chdir="$TF_DIR" plan -destroy -input=false \
      -var 'deletion_protection=false'
  local probe_verdict="${ROWS[-1]}"

  if [[ "$REAL_DESTROY" == "yes" ]]; then
    if [[ "$probe_verdict" == *"**PASS**"* ]]; then
      attempt prevent_destroy "Direct destroy (apply-time)" "shared root" \
        terraform -chdir="$TF_DIR" destroy -input=false -auto-approve \
          -var 'deletion_protection=false'
    else
      ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
        "Direct destroy (apply-time)" "shared root" "NOT RUN" "INCONCLUSIVE" \
        "REFUSED to run: the read-only plan probe did not show a prevent_destroy refusal, so running the real destroy could actually destroy the estate")")
      RED=$((RED+1))
    fi
  fi

  local after_ok=yes
  snapshot "$after" || after_ok=no

  # The postcondition for attempt 4. A partial destroy is the failure mode the
  # per-resource checks cannot see, so this compares the whole estate.
  #
  # IDENTICAL only counts when neither side is blind. On a live
  # run both sides were 13 ERROR lines, they diffed identical, and this row
  # said PASS.
  local diffout
  if [[ "$after_ok" != yes ]]; then
    diffout="$(diff -u --label before --label after "$before" "$after" 2>&1)"
    ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
      "Estate unchanged (before == after)" "${NAME_PREFIX}-*" "BLIND" "FAIL" \
      "the after-sweep errored or found no ${NAME_PREFIX} resources; an unchanged estate is UNPROVEN")")
    RED=$((RED+1))
  elif diffout="$(diff -u --label before --label after "$before" "$after" 2>&1)"; then
    ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
      "Estate unchanged (before == after)" "${NAME_PREFIX}-*" "IDENTICAL" "PASS" \
      "nothing was destroyed, partially or otherwise")")
  else
    ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
      "Estate unchanged (before == after)" "${NAME_PREFIX}-*" "DIFFERS" "FAIL" \
      "THE ESTATE CHANGED during attempt 4 — see the diff in the evidence file")")
    RED=$((RED+1))
  fi
  {
    printf '### Estate snapshot diff (attempt 4)\n\n'
    printf '%s\n' '```diff'
    printf '%s\n' "${diffout:-(identical)}"
    printf '%s\n\n' '```'
  } >> "$OUT"
}

do_run() {
  [[ -n "$PROJECT"      ]] || { echo "$PROG: --project is required" >&2; exit 2; }
  [[ -n "$SQL_INSTANCE" ]] || { echo "$PROG: --sql-instance is required" >&2; exit 2; }
  [[ -n "$FS_INSTANCE"  ]] || { echo "$PROG: --filestore is required" >&2; exit 2; }
  [[ -n "$FS_ZONE"      ]] || { echo "$PROG: --filestore-zone is required" >&2; exit 2; }
  command -v gcloud >/dev/null || { echo "$PROG: gcloud not found" >&2; exit 2; }
  gcloud auth print-access-token >/dev/null 2>&1 \
    || { echo "$PROG: no usable credentials" >&2; exit 2; }

  # Which commit is under test. Asserted HERE, in the command that runs the
  # test, not in a step beside it: the failure this prevents is exactly that a
  # checkout was verified in one place and executed from another.
  local head_sha="n/a"
  if [[ -n "$TF_DIR" ]]; then
    head_sha="$(repo_head "$TF_DIR")"
    if [[ -n "$EXPECT_COMMIT" ]]; then
      if [[ "$(commit_matches "$EXPECT_COMMIT" "$head_sha")" != yes ]]; then
        echo "$PROG: refusing — ${TF_DIR} is at HEAD ${head_sha}, expected ${EXPECT_COMMIT}" >&2
        echo "  A destroy-protection result attributed to the wrong commit is the" >&2
        echo "  silent-wrong-answer class this harness exists to prevent." >&2
        echo "  (An undeterminable HEAD also fails here: UNKNOWN is not a match.)" >&2
        exit 2
      fi
      echo "checkout verified: HEAD ${head_sha} matches --expect-commit ${EXPECT_COMMIT}" >&2
    elif [[ "$head_sha" == UNKNOWN ]]; then
      echo "$PROG: note — could not determine HEAD of ${TF_DIR}; the report will say UNKNOWN" >&2
    fi
  elif [[ -n "$EXPECT_COMMIT" ]]; then
    echo "$PROG: --expect-commit given without --tf-dir; there is nothing to check" >&2
    exit 2
  fi

  OUT="${OUT:-destroy-protection-report.md}"
  : > "$OUT"
  {
    printf '# Destroy-protection check, captured %s\n\n' "$(date -u +%FT%TZ)"
    # Literal backticks for markdown code formatting inside the single-quoted
    # format strings below, not command substitution; double-quoting would
    # make them live and break this.
    # shellcheck disable=SC2016
    printf 'project: `%s`  identity: `%s`\n\n' "$PROJECT" \
      "$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null)"
    # shellcheck disable=SC2016
    printf 'terraform root: `%s`  HEAD: `%s`%s\n\n' "${TF_DIR:-none}" "$head_sha" \
      "$([[ -n "$EXPECT_COMMIT" ]] && printf ' (asserted against `%s`)' "$EXPECT_COMMIT")"
    printf 'Pass condition is refusal text matching its cause. A bare non-zero\n'
    printf 'exit is RED: a 403 and a deletion-protection refusal are\n'
    printf 'indistinguishable by exit code, and scoring the former as a pass\n'
    printf 'retires the question.\n\n'
    local u; u="$(unconfirmed_count)"
    if [[ "$u" -gt 0 ]]; then
      printf '> **Caveat carried into this evidence:** %s of the pass patterns were\n' "$u"
      printf '> reconstructed rather than captured from a live refusal. A red row\n'
      printf '> below may therefore mean the API reworded its message, not that the\n'
      printf '> guard is absent. Read the stderr before concluding either way, and\n'
      printf '> update the fixtures from it.\n\n'
    fi
    printf '## Raw output\n\n'
  } >> "$OUT"

  # Guard: the destroy-protection check is only meaningful with a hub present, because the interlock is
  # "fails WHILE a hub exists". Running it on an empty instance tests nothing
  # and would produce a confident green.
  local dbs
  dbs="$(gcloud sql databases list --instance="$SQL_INSTANCE" --project="$PROJECT" \
          --format='value(name)' 2>/dev/null | grep -vx 'postgres' || true)"
  if [[ -z "$dbs" ]]; then
    echo "$PROG: refusing — no non-postgres database on ${SQL_INSTANCE}." >&2
    echo "  The destroy-protection check requires a hub present; the interlock is 'fails while a hub exists'." >&2
    echo "  With none, the interlock would pass by vacuum." >&2
    exit 3
  fi
  echo "hub databases present: $(echo "$dbs" | tr '\n' ' ')" >&2

  attempt sql       "SQL instance delete"    "$SQL_INSTANCE" \
    gcloud sql instances delete "$SQL_INSTANCE" --project="$PROJECT" --quiet
  gate_last_row
  attempt filestore "Filestore delete"       "$FS_INSTANCE" \
    gcloud filestore instances delete "$FS_INSTANCE" --project="$PROJECT" \
      --zone="$FS_ZONE" --quiet
  gate_last_row

  if [[ -n "$TF_DIR" ]]; then
    attempt terraform "Interlock (protection off)" "shared root" \
      terraform -chdir="$TF_DIR" apply -input=false -auto-approve \
        -var 'deletion_protection=false'
  else
    ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
      "Interlock (protection off)" "shared root" "SKIPPED" "INCONCLUSIVE" \
      "no --tf-dir given; the interlock was not exercised")")
    RED=$((RED+1))
  fi
  gate_last_row

  local n0=${#ROWS[@]} i
  do_attempt4
  for ((i=n0; i<${#ROWS[@]}; i++)); do
    [[ "${ROWS[i]}" == *"**PASS**"* ]] || stop 1 "attempt-4 row $((i+1)) is not PASS; no further rows attempted"
  done

  render_results
  echo
  printf '%s\n' "${ROWS[@]}"
  echo
  echo "written: ${OUT}"
  [[ "$RED" -eq 0 ]] && return 0 || return 1
}

# Stop at the FIRST failed row. do_run used to carry on after a failure, so an
# unexpected success (a real delete) on row 1 was followed by a delete
# attempt on row 2. The rule instead: any failure, STOP, attempt nothing
# further.
gate_last_row() {
  [[ ${#ROWS[@]} -gt 0 && "${ROWS[-1]}" == *"**PASS**"* ]] \
    || stop 1 "row ${#ROWS[@]} is not PASS; no further rows attempted"
}

# stop() ALWAYS exits. The report is rendered in a SUBSHELL, so an
# error in rendering (for example an unbound variable under `set -u`, which is
# exactly what killed the wrapper's stop() before its exit) can only end
# the subshell, never skip the `exit`. stop() itself touches nothing that can
# be unset.
stop() { # stop <exit-code> <reason...>
  local rc="${1:-1}"
  shift || true
  printf '%s: STOP — %s\n' "${PROG:-verify-destroy-protection}" "$*" >&2
  ( render_results "STOPPED: $*" ) 2>/dev/null || true
  exit "$rc"
}

# The two stated gaps are added HERE, when the report is rendered, so that they
# appear on every report, including a stopped one.
add_stated_gaps() {
  # @unconditional-row — GKE out-of-band delete.
  # Not a test result: the statement that no test is possible. Green rows with
  # no such row read as full coverage, which is false. Never make this
  # conditional on --gke-cluster. The selftest counts these markers.
  ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
    "GKE cluster, out-of-band delete" "${GKE_CLUSTER:-tfha-agents}" "n/a" \
    "NOT TESTED, NOT PROTECTED" \
    "container v1 has no deletion-protection field; \`prevent_destroy\` and the provider flag are Terraform-side only, so a \`gcloud container clusters delete\` is unaffected — mitigated only by IAM/org policy, outside Terraform's reach")")

  # @unconditional-row — the -target residual is recorded here rather than
  # exercised. Same rule: it is a stated gap, not a skipped test. Exercising
  # it for real would mean actually turning off protection on live shared
  # resources, which is exactly what this residual risk warns against.
  ROWS+=("$(printf '| %s | %s | %s | **%s** | %s |' \
    "\`-target\` apply flipping protection" "shared root" "n/a" \
    "NOT TESTED, NOT ENFORCED" \
    "an in-place update, not a destroy, so \`prevent_destroy\` does not apply and a \`-target\`ed plan skips the precondition; prohibited by the README runbook, not enforced in config. Deliberately not tested live")")
}

render_results() { # render_results [stop-reason]
  add_stated_gaps
  [[ -n "${OUT:-}" ]] || return 0
  {
    printf '\n## Results\n\n'
    [[ -n "${1:-}" ]] && printf '> **%s**\n\n' "$1"
    printf '| check | resource | classification | verdict | note |\n'
    printf '|---|---|---|---|---|\n'
    printf '%s\n' "${ROWS[@]}"
    printf '\nRows that must pass: 4. The last two are stated gaps, not failures.\n'
    if [[ "${SWEEP_ERRORS:-0}" -gt 0 ]]; then
      printf '\n> **%s sweep(s) were blind** (errored, or found no %s resources).\n' "$SWEEP_ERRORS" "$NAME_PREFIX"
      printf '> An identical diff across two blind spots is not evidence; the\n'
      printf '> estate row is FAIL, not PASS. See the ERROR lines in the snapshots.\n'
    fi
  } >> "$OUT"
}


# ---------------------------------------------------------------------------
# Regression rows for defects found on a real live run (2026-09-27 14:01Z).
# Each row drives the REAL functions against stub gcloud/terraform shell
# functions inside a subshell, so no credentials are needed and nothing
# leaks. Every one of these rows FAILS on harness sha f82e3315 (verified by
# running this function against that version) and passes on the fixed code.
# ---------------------------------------------------------------------------
selftest_regressions() {
  rr() { # rr <desc> <want> <got>
    n=$((n+1))
    if [[ "$3" == "$2" ]]; then
      printf '  ok    %-52s -> %s\n' "$1" "$3"
    else
      printf '  FAIL  %-52s -> %s (want %s)\n' "$1" "$3" "$2"
      fails=$((fails+1))
    fi
  }
  local got tmpd
  tmpd="$(mktemp -d)"

  echo
  echo "=== live-run regressions (a)-(d) ==="

  # (a) The sweep must actually run. With a gcloud that answers every list
  # with one tfha name, a snapshot must contain 13 tfha lines and no ERROR.
  # The eval-based version produced 13 ERROR lines (bash syntax error on the
  # unquoted `value(name)`) without ever calling gcloud.
  got="$( (
    gcloud()    { printf 'tfha-x\nnot-ours\n'; }
    terraform() { printf 'module.a.x\n'; }
    PROJECT=p REGION=r TF_DIR="$tmpd" NAME_PREFIX=tfha SWEEP_ERRORS=0
    f="$tmpd/a.snap"; snapshot "$f" >/dev/null 2>&1
    printf 'tfha=%s error=%s' "$(grep -c '^[a-z]*: tfha-x$' "$f")" "$(grep -c 'ERROR' "$f")"
  ) )"
  rr "(a) sweep runs gcloud (no eval syntax error)" "tfha=13 error=0" "$got"

  # (b) stop() exits, with the given code, even under `set -u` with ROWS
  # unset and OUT set, which is the state that killed the wrapper's
  # stop(). And ROWS is initialised, so `${#ROWS[@]}` is safe under -u.
  # "echo CONTINUED" below is deliberately unreachable in the passing case:
  # it only runs if stop() fails to exit, which is exactly the regression
  # this row checks for.
  # shellcheck disable=SC2317
  got="$( ( set -u; OUT="$tmpd/b.md"; unset ROWS; stop 7 "selftest" 2>/dev/null; echo CONTINUED ); echo "rc=$?" )"
  rr "(b) stop exits under set -u, ROWS unset" "rc=7" "$got"
  got="$( ( set -u; printf '%s' "${#ROWS[@]}" ) 2>/dev/null )"
  rr "(b) ROWS initialised (safe under set -u)" "0" "$got"

  # (c) A blind sweep must FAIL, not diff identical and pass; and a blind
  # BEFORE snapshot must stop attempt 4 from running at all.
  # (c) c1: every list errors. c2: lists succeed but find no tfha resource.
  # c3: positive control. The sweep sees tfha and 4a is refused: PASS.
  a4() { # a4 <gcloud-mode> -> "<last-verdict> plan=<called|not-called>"
    ( local mode="$1"
      gcloud() {
        case "$mode" in
          error) echo "boom" >&2; return 1 ;;
          empty) printf 'someone-else\n' ;;
          *)     printf 'tfha-x\n' ;;
        esac
      }
      terraform() {
        case " $* " in
          *" state list "*) printf 'module.a.x\n' ;;
          *" plan "*) : > "$tmpd/plan.called"
            printf 'Error: Instance cannot be destroyed\nResource x has lifecycle.prevent_destroy set\n' >&2
            return 1 ;;
          *) return 0 ;;
        esac
      }
      rm -f "$tmpd/plan.called"
      PROJECT=p REGION=r TF_DIR="$tmpd" NAME_PREFIX=tfha REAL_DESTROY=no
      OUT="$tmpd/c.md" ROWS=() RED=0 SWEEP_ERRORS=0
      do_attempt4 >/dev/null 2>&1
      printf '%s plan=%s' "$(printf '%s' "${ROWS[-1]}" | grep -oE '\*\*[A-Z]+\*\*' | tr -d '*')" \
        "$([[ -e "$tmpd/plan.called" ]] && echo called || echo not-called)" )
  }
  rr "(c) all sweeps error: FAIL, destroy not tried" "FAIL plan=not-called" "$(a4 error)"
  rr "(c) zero tfha found: FAIL, destroy not tried" "FAIL plan=not-called" "$(a4 empty)"
  rr "(c) control: sweep sees tfha, refused: PASS" "PASS plan=called" "$(a4 ok)"

  # (d) do_run stops at the FIRST failed row. Row 1 "succeeds" (a real
  # delete, the catastrophe), so the filestore delete must never be issued.
  got="$( (
    gcloud() {
      case " $* " in
        *" auth "*)                   echo me ;;
        *" databases list "*)         printf 'postgres\ntfha_h1\n' ;;
        *" sql instances delete "*)   return 0 ;;
        *" sql instances describe "*) return 1 ;;
        *" filestore instances delete "*) : > "$tmpd/fs.called"; return 1 ;;
        *)                            printf 'tfha-x\n' ;;
      esac
    }
    PROJECT=p SQL_INSTANCE=tfha-pg FS_INSTANCE=tfha-nfs FS_ZONE=z TF_DIR="" EXPECT_COMMIT=""
    OUT="$tmpd/d.md" ROWS=() RED=0
    do_run >/dev/null 2>&1
  ); echo "rc=$? filestore=$([[ -e "$tmpd/fs.called" ]] && echo called || echo not-called)" )"
  rr "(d) do_run stops after a failed row 1" "rc=1 filestore=not-called" "$got"
  got="$(grep -c 'NOT TESTED' "$tmpd/d.md" 2>/dev/null || echo 0)"
  rr "(d) stopped report still carries both stated gaps" "2" "$got"

  rm -rf "$tmpd"
}

# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --selftest)       MODE=selftest ;;
    --run)            MODE=run ;;
    --project)        PROJECT="${2:?}"; shift ;;
    --sql-instance)   SQL_INSTANCE="${2:?}"; shift ;;
    --filestore)      FS_INSTANCE="${2:?}"; shift ;;
    --filestore-zone) FS_ZONE="${2:?}"; shift ;;
    --tf-dir)         TF_DIR="${2:?}"; shift ;;
    --expect-commit)  EXPECT_COMMIT="${2:?}"; shift ;;
    --gke-cluster)    GKE_CLUSTER="${2:?}"; shift ;;
    --name-prefix)    NAME_PREFIX="${2:?}"; shift ;;
    --region)         REGION="${2:?}"; shift ;;
    --with-real-destroy) REAL_DESTROY=yes ;;
    --out)            OUT="${2:?}"; shift ;;
    -h|--help)        sed -n '2,60p' "$0"; exit 0 ;;
    *) echo "$PROG: unknown argument '$1'" >&2; exit 2 ;;
  esac
  shift
done

case "$MODE" in
  selftest) do_selftest ;;
  run)      do_run ;;
  *)        echo "$PROG: one of --selftest or --run is required" >&2; exit 2 ;;
esac
