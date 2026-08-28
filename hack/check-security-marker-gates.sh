#!/usr/bin/env bash
# Guard: security symbols from B5, #1322, #1338, and #1347 must remain in the
# handler files where they enforce authorization and identity invariants.
#
# This is the regression guard for the security fixtures that messaging-v2
# (scion/messaging-v2) would revert. The branch pre-dates B5 (#1343), #1322,
# #1347, and #1338 (DEF-31). A naive merge or cherry-pick from v2 silently
# deletes sender-identity derivation, DM ownership checks, agent authorization,
# and default-agent validation.
#
# WHAT THIS CHECKS
#
# Each gate row asserts that a named symbol appears a specific number of times
# inside a named enclosing function. Function names survive line drift; the
# assertion never pins an absolute line number.
#
# Gate categories:
#
#   REQUIRED — calls and definitions. The check runs or exists. A missing
#   REQUIRED site means the check no longer runs or no longer exists; that is
#   the revert. Gate HARD FAILS.
#
#   AUDIT — calls whose absence removes the only record of a security decision.
#   On a path that fails silently by design (no 403, the denied action is simply
#   dropped), the log carries the entire evidentiary weight. Gate HARD FAILS.
#   Distinguished from REQUIRED so the failure report can say "the denial is now
#   unrecorded" rather than "the check is gone."
#
#   INFORMATIONAL — doc-comment mentions. Prose, not behavior. Gate REPORTS
#   (printed as a notice) but does NOT fail. A gate that fails on a doc-comment
#   reword gets overridden without reading by the third occurrence.
#
# Gate rows:
#
#   authenticatedSender in handlers_agent_messaging.go:
#     REQUIRED: handleAgentMessage x1, handleGroupMessage x2,
#               handleProjectBroadcast x1, func definition x1
#     INFORMATIONAL: doc comment x1
#
#   validateDefaultAgent in handlers_chat_v2.go:
#     REQUIRED: handleCreateThread x1, handleTopicPatch x1, func definition x1
#     INFORMATIONAL: doc comments x3
#
#   ActionAttach in handlers_agent_messaging.go:
#     REQUIRED: handleProjectBroadcast x1
#
#   ActionAttach in handlers_chat_v2.go:
#     REQUIRED: sendAgentRouted x2 (s.authorize + CheckAccess)
#     AUDIT: sendAgentRouted x1 (logAuthzDenial — silent-denial path, no 403)
#
#   COMPOSITE: handleProjectBroadcast in handlers_agent_messaging.go must
#   contain BOTH authenticatedSender AND ActionAttach.
#
# EXIT CODES
#   0  all gates pass
#   1  one or more REQUIRED or AUDIT gates failed
set -euo pipefail

cd "$(dirname "$0")/.."

rc=0
notices=""

# ---------------------------------------------------------------------------
# count_in_func FILE FUNC_NAME SYMBOL
#
# Counts occurrences of SYMBOL inside the body of the named top-level function
# in FILE. Uses awk brace-depth tracking from the func declaration line.
#
# For "func definition" checks, use count_func_def instead.
# ---------------------------------------------------------------------------
count_in_func() {
  local file="$1" func_name="$2" symbol="$3"
  awk -v fname="$func_name" -v sym="$symbol" '
    BEGIN { in_func = 0; in_sig = 0; depth = 0; count = 0; done = 0 }

    # Match top-level func declaration. Handles both free functions and methods.
    # Matches: func foo(, func (s *Server) foo(
    # The opening brace may be on the same line or on a continuation line
    # (multi-line signatures).
    /^func / {
      if (done) next
      # Extract the function name (strip receiver if present)
      name = $0
      sub(/^func[[:space:]]+/, "", name)
      sub(/^\([^)]*\)[[:space:]]*/, "", name)
      sub(/[[:space:]]*\(.*$/, "", name)
      if (name == fname) {
        # Check if the opening brace is on this line
        if ($0 ~ /\{[[:space:]]*$/) {
          in_func = 1
          depth = 1
        } else {
          # Multi-line signature — wait for the opening brace
          in_sig = 1
        }
        next
      }
    }

    # Multi-line signature continuation: look for the opening brace
    in_sig && !done {
      if ($0 ~ /\{[[:space:]]*$/) {
        in_func = 1
        in_sig = 0
        depth = 1
        next
      }
      next
    }

    in_func && !done {
      # Count braces
      opn = $0; gsub(/[^{]/, "", opn)
      cls = $0; gsub(/[^}]/, "", cls)
      depth += length(opn) - length(cls)

      # Count symbol occurrences on this line, but skip comment lines
      if ($0 !~ /^[[:space:]]*\/\//) {
        line = $0
        n = gsub(sym, "&", line)
        count += n
      }

      if (depth <= 0) {
        done = 1
      }
    }

    END { print count }
  ' "$file"
}

# ---------------------------------------------------------------------------
# count_func_def FILE SYMBOL
#
# Counts lines matching "func SYMBOL(" or "func (receiver) SYMBOL(" at the
# top level. This detects the function definition itself.
# ---------------------------------------------------------------------------
count_func_def() {
  local file="$1" symbol="$2"
  local count
  count=$(grep -cE "^func[[:space:]]+(\\([^)]*\\)[[:space:]]+)?${symbol}\\(" "$file" 2>/dev/null) || count=0
  echo "$count"
}

# ---------------------------------------------------------------------------
# count_comments FILE SYMBOL
#
# Counts comment lines (// ...) mentioning the symbol. Used for INFORMATIONAL.
# ---------------------------------------------------------------------------
count_comments() {
  local file="$1" symbol="$2"
  local count
  count=$(grep -c "^[[:space:]]*//.*${symbol}" "$file" 2>/dev/null) || count=0
  echo "$count"
}

# ---------------------------------------------------------------------------
# assert_required DESCRIPTION FILE FUNC SYMBOL EXPECTED
# ---------------------------------------------------------------------------
assert_required() {
  local desc="$1" file="$2" func="$3" symbol="$4" expected="$5"
  local actual
  actual="$(count_in_func "$file" "$func" "$symbol")"
  if [[ "$actual" -ne "$expected" ]]; then
    echo "FAIL [REQUIRED] $desc" >&2
    echo "  expected $symbol x$expected in $func ($file), found x$actual" >&2
    rc=1
  fi
}

# ---------------------------------------------------------------------------
# assert_funcdef DESCRIPTION FILE SYMBOL EXPECTED
# ---------------------------------------------------------------------------
assert_funcdef() {
  local desc="$1" file="$2" symbol="$3" expected="$4"
  local actual
  actual="$(count_func_def "$file" "$symbol")"
  if [[ "$actual" -ne "$expected" ]]; then
    echo "FAIL [REQUIRED] $desc" >&2
    echo "  expected func $symbol definition x$expected in $file, found x$actual" >&2
    rc=1
  fi
}

# ---------------------------------------------------------------------------
# assert_audit DESCRIPTION FILE FUNC SYMBOL EXPECTED
# ---------------------------------------------------------------------------
assert_audit() {
  local desc="$1" file="$2" func="$3" symbol="$4" expected="$5"
  local actual
  actual="$(count_in_func "$file" "$func" "$symbol")"
  if [[ "$actual" -ne "$expected" ]]; then
    echo "FAIL [AUDIT] $desc" >&2
    echo "  expected $symbol x$expected in $func ($file), found x$actual" >&2
    echo "  This is a silent-denial path — logAuthzDenial is the ONLY record of the denial." >&2
    rc=1
  fi
}

# ---------------------------------------------------------------------------
# assert_informational DESCRIPTION FILE SYMBOL EXPECTED
# ---------------------------------------------------------------------------
assert_informational() {
  local desc="$1" file="$2" symbol="$3" expected="$4"
  local actual
  actual="$(count_comments "$file" "$symbol")"
  if [[ "$actual" -lt "$expected" ]]; then
    notices="${notices}NOTICE [INFORMATIONAL] $desc: expected $symbol in ≥$expected doc comments in $file, found $actual
"
  fi
}

# ===== Gate assertions =====

HAM="pkg/hub/handlers_agent_messaging.go"
HCV="pkg/hub/handlers_chat_v2.go"

# --- authenticatedSender in handlers_agent_messaging.go ---

# REQUIRED: 4 call sites + 1 definition
assert_required \
  "authenticatedSender in handleAgentMessage (B5 — DM key derivation)" \
  "$HAM" handleAgentMessage authenticatedSender 1

assert_required \
  "authenticatedSender in handleGroupMessage (B5 — per-agent and per-user DM resolution)" \
  "$HAM" handleGroupMessage authenticatedSender 2

assert_required \
  "authenticatedSender in handleProjectBroadcast (B5 — broadcast self-skip)" \
  "$HAM" handleProjectBroadcast authenticatedSender 1

assert_funcdef \
  "authenticatedSender function definition (B5 — must exist)" \
  "$HAM" authenticatedSender 1

# INFORMATIONAL: 1 doc comment
assert_informational \
  "authenticatedSender doc comment" \
  "$HAM" authenticatedSender 1

# --- validateDefaultAgent in handlers_chat_v2.go ---

# REQUIRED: 2 call sites + 1 definition
assert_required \
  "validateDefaultAgent in handleCreateThread (DEF-31 — topic creation)" \
  "$HCV" handleCreateThread validateDefaultAgent 1

assert_required \
  "validateDefaultAgent in handleTopicPatch (DEF-31 — topic update)" \
  "$HCV" handleTopicPatch validateDefaultAgent 1

assert_funcdef \
  "validateDefaultAgent function definition (DEF-31 — must exist)" \
  "$HCV" validateDefaultAgent 1

# INFORMATIONAL: 3 doc comments
assert_informational \
  "validateDefaultAgent doc comments" \
  "$HCV" validateDefaultAgent 3

# --- ActionAttach in handlers_agent_messaging.go ---

# REQUIRED: 1 call in handleProjectBroadcast
assert_required \
  "ActionAttach in handleProjectBroadcast (#1347 — project broadcast authorization)" \
  "$HAM" handleProjectBroadcast ActionAttach 1

# --- ActionAttach in handlers_chat_v2.go ---

# REQUIRED: 2 calls in sendAgentRouted (s.authorize + CheckAccess)
# AUDIT: 1 call in sendAgentRouted (logAuthzDenial — silent denial path)
# Total ActionAttach occurrences in sendAgentRouted = 3
assert_required \
  "ActionAttach authorize + CheckAccess in sendAgentRouted (#1347 — agent attach authorization)" \
  "$HCV" sendAgentRouted ActionAttach 3

# The AUDIT check: logAuthzDenial specifically, within sendAgentRouted
assert_audit \
  "logAuthzDenial(ActionAttach) in sendAgentRouted (#1347 — silent mention-denial audit trail)" \
  "$HCV" sendAgentRouted logAuthzDenial 1

# --- COMPOSITE GATE: handleProjectBroadcast ---
# This single function carries authenticatedSender (B5) AND ActionAttach (#1347).
# messaging-v2 reverts BOTH. A regression here costs sender-identity derivation
# and project authorization simultaneously.

composite_auth="$(count_in_func "$HAM" handleProjectBroadcast authenticatedSender)"
composite_attach="$(count_in_func "$HAM" handleProjectBroadcast ActionAttach)"

if [[ "$composite_auth" -lt 1 ]] || [[ "$composite_attach" -lt 1 ]]; then
  echo "FAIL [COMPOSITE] handleProjectBroadcast must contain BOTH authenticatedSender AND ActionAttach" >&2
  echo "  authenticatedSender: found x$composite_auth (need ≥1)" >&2
  echo "  ActionAttach: found x$composite_attach (need ≥1)" >&2
  echo "  This function is the highest-value anchor: a single regression costs" >&2
  echo "  sender-identity derivation AND project authorization simultaneously." >&2
  rc=1
fi

# --- Print results ---

if [[ -n "$notices" ]]; then
  echo "$notices"
fi

if [[ "$rc" -eq 0 ]]; then
  echo "check-security-marker-gates: all gates pass"
else
  echo >&2
  echo "check-security-marker-gates: FAILED — see above" >&2
fi

exit "$rc"
