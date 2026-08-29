#!/usr/bin/env bash
# Negative tests for the conversation-upsert guard (C1 — Option C).
#
# Verifies that the guard correctly:
#   1. Passes with no violations (baseline)
#   2. Exempts INSERT INTO conversations with kind='group' in webchannel_store.go
#   3. Rejects INSERT INTO conversations with kind='direct' in webchannel_store.go
#   4. Rejects AddParticipant calls in webchannel_store.go (fully barred surface)
#   5. Exempts INSERT INTO conversations with kind='group' in webchannel_store_postgres.go
#   6. Rejects INSERT INTO conversations with kind='group' in a non-exempted pkg/hub file
#   7. Exempts single-line kind='group' INSERT (Case A regression)
#   8. Rejects single-line kind='direct' INSERT
#   9. Rejects kind='direct' INSERT with decoy 'group' after statement (Case C regression)
#  10. Rejects decoy 'group' after closing backtick
#  11. Rejects post-backtick content leak (kind via variable, 'group' after backtick)
#  12. Rejects 20-line cap truncation (unbounded statement with decoy 'group')
#  13. Exempts long kind='group' statement with closing backtick within window
#  14. Exempts kind='group' INSERT with 'direct' mentioned only after backtick
#
# Each test injects a synthetic pattern, runs the guard, and checks the exit code.
# Originals are always restored via trap.
set -euo pipefail

cd "$(dirname "$0")/.."

GUARD="./hack/check-conversation-upsert-guard.sh"
SQLITE="pkg/hub/webchannel_store.go"
POSTGRES="pkg/hub/webchannel_store_postgres.go"
INJECT_OTHER="pkg/hub/c1_negative_test_inject.go"

# --- Backup and cleanup ---
cp "$SQLITE" "${SQLITE}.c1bak"
cp "$POSTGRES" "${POSTGRES}.c1bak"

cleanup() {
  mv "${SQLITE}.c1bak" "$SQLITE" 2>/dev/null || true
  mv "${POSTGRES}.c1bak" "$POSTGRES" 2>/dev/null || true
  rm -f "$INJECT_OTHER"
}
trap cleanup EXIT

restore() {
  # Restore both files to their backup state before each test
  cp "${SQLITE}.c1bak" "$SQLITE"
  cp "${POSTGRES}.c1bak" "$POSTGRES"
  rm -f "$INJECT_OTHER"
}

pass=0
fail=0

run_test() {
  local desc="$1"
  local expected_exit="$2"

  set +e
  guard_output=$("$GUARD" 2>&1)
  actual_exit=$?
  set -e

  if [[ "$actual_exit" -eq "$expected_exit" ]]; then
    echo "PASS: $desc (exit $actual_exit)"
    ((pass++)) || true
  else
    echo "FAIL: $desc (expected exit $expected_exit, got exit $actual_exit)" >&2
    if [[ -n "$guard_output" ]]; then
      echo "  Guard output:" >&2
      printf '  %s\n' "$guard_output" >&2
    fi
    ((fail++)) || true
  fi

  restore
}

# --- Test 1: Baseline — guard passes with no injections ---
run_test "baseline: no violations" 0

# --- Test 2: kind='group' INSERT in webchannel_store.go — EXEMPTED (exit 0) ---
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestGroupInsertSqlite() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES (?, ?, 'group', 'native')`, id, pid)
}
INJECT
run_test "kind='group' INSERT in webchannel_store.go is exempted" 0

# --- Test 3: kind='direct' INSERT in webchannel_store.go — NOT EXEMPTED (exit 1) ---
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestDirectInsertSqlite() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		Values (?, ?, 'direct', 'native')`, id, pid)
}
INJECT
run_test "kind='direct' INSERT in webchannel_store.go is rejected" 1

# --- Test 4: AddParticipant in webchannel_store.go — NOT EXEMPTED (exit 1) ---
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestAddParticipant() {
	s.store.AddParticipant(ctx, conv.ID, userID)
}
INJECT
run_test "AddParticipant in webchannel_store.go is rejected" 1

# --- Test 5: kind='group' INSERT in webchannel_store_postgres.go — EXEMPTED (exit 0) ---
cat >>"$POSTGRES" <<'INJECT'

// c1-negative-test-inject
func c1TestGroupInsertPostgres() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES ($1, $2, 'group', 'native')`, id, pid)
}
INJECT
run_test "kind='group' INSERT in webchannel_store_postgres.go is exempted" 0

# --- Test 6: kind='group' INSERT in a NON-exempted hub file — NOT EXEMPTED (exit 1) ---
cat > "$INJECT_OTHER" <<'INJECT'
package hub

func c1TestGroupInsertOther() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES (?, ?, 'group', 'native')`, id, pid)
}
INJECT
run_test "kind='group' INSERT in non-exempted hub file is rejected" 1

# --- Test 7: Single-line kind='group' INSERT — EXEMPTED (exit 0) ---
# Regression test for Case A: the INSERT and 'group' are on the same line.
# The old 3-line-window logic looked only AFTER the INSERT line and missed this.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestSingleLineGroupInsert() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind) VALUES (?, ?, 'group')`, id, pid)
}
INJECT
run_test "single-line kind='group' INSERT is exempted (Case A fix)" 0

# --- Test 8: Single-line kind='direct' INSERT — NOT EXEMPTED (exit 1) ---
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestSingleLineDirectInsert() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind) VALUES (?, ?, 'direct')`, id, pid)
}
INJECT
run_test "single-line kind='direct' INSERT is rejected" 1

# --- Test 9: Multi-line kind='direct' INSERT with decoy 'group' — NOT EXEMPTED (exit 1) ---
# Regression test for Case C (SECURITY): a kind='direct' INSERT followed by
# a comment containing 'group' within the next 3 lines. The old window-based
# logic would wrongly exempt this because it saw 'group' in the window.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestDirectWithDecoyGroup() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES (?, ?, 'direct', 'native')`, id, pid)
	// This creates a 'group' sidebar later
}
INJECT
run_test "kind='direct' INSERT with decoy 'group' after statement is rejected (Case C fix)" 1

# --- Test 10: Decoy 'group' after closing backtick — NOT EXEMPTED (exit 1) ---
# The 'group' literal appears in a Go comment after the raw string ends.
# The statement itself has kind='direct'.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestDecoyGroupAfterBacktick() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES (?, ?, 'direct', 'native')` + "/* 'group' */", id, pid)
}
INJECT
run_test "decoy 'group' after closing backtick is rejected" 1

# --- Test 11: Post-backtick content leak — NOT EXEMPTED (exit 1) ---
# Regression test for Fix 1 (sed d→q): kind is set via a variable (no literal
# in the SQL). 'group' appears on a line AFTER the closing backtick that has
# no backtick of its own — the old `p; d` sed logic would leak it into $stmt.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestLeakedGroupAfterStatement() {
	kind := "group"
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES (?, ?, ?, 'native')`, id, pid, kind)
	// Use 'group' conversation for sidebar
}
INJECT
run_test "post-backtick 'group' on non-backtick line does not leak into statement" 1

# --- Test 12: 20-line cap truncation — NOT EXEMPTED (exit 1) ---
# Regression test for Fix 2: an INSERT statement longer than 20 lines. A decoy
# 'group' appears inside the statement (within the first 20 lines) and the real
# 'direct' is in VALUES beyond line 20. Without the fix, the truncated text
# would show 'group' (pass Check 1) and no 'direct' (pass Check 2), wrongly
# exempting the INSERT. With the fix, the missing closing backtick within the
# 20-line window causes fail-closed rejection.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestCapTruncation() {
	db.Exec(`INSERT INTO conversations (
		id,
		project_id,
		kind,
		surface,
		external_ref,
		created_at,
		updated_at,
		col8,
		col9,
		col10, -- 'group' placeholder
		col11,
		col12,
		col13,
		col14,
		col15,
		col16,
		col17,
		col18,
		col19,
		col20
	) VALUES (
		?, ?, 'direct', 'native', ?, NOW(), NOW(),
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
	// 'group' reference for documentation
}
INJECT
run_test "20-line cap truncation: unbounded statement is rejected (fail-closed)" 1

# --- Test 13: Long kind='group' statement with backtick within window — EXEMPTED (exit 0) ---
# Control test for Fix 2: a multi-line kind='group' INSERT that IS bounded
# (closing backtick within 20 lines). Proves the fail-closed path doesn't
# reject everything.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestLongGroupStatement() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface, external_ref, created_at, updated_at)
		VALUES (?, ?, 'group', 'native', ?, NOW(), NOW())`, args...)
}
INJECT
run_test "long kind='group' INSERT with closing backtick within window is exempted" 0

# --- Test 14: kind='group' INSERT with 'direct' only after backtick — EXEMPTED (exit 0) ---
# Control test for Fix 1: a valid kind='group' INSERT where 'direct' appears
# only in a comment after the closing backtick. Proves Fix 1 correctly ignores
# post-backtick content.
cat >>"$SQLITE" <<'INJECT'

// c1-negative-test-inject
func c1TestGroupWithPostBacktickDirect() {
	db.Exec(`INSERT INTO conversations (id, project_id, kind, surface)
		VALUES (?, ?, 'group', 'native')`, id, pid)
	// Also handle 'direct' conversations elsewhere
}
INJECT
run_test "kind='group' INSERT with 'direct' only after backtick is exempted" 0

# --- Summary ---
echo
echo "Results: $pass passed, $fail failed out of $((pass + fail)) tests"
if [[ "$fail" -gt 0 ]]; then
  exit 1
fi
