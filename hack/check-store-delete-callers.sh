#!/usr/bin/env bash
# Flags direct calls of the store methods DeleteAgent and DeleteProject
# outside an explicit allowlist of callers.
#
# CompositeStore.DeleteAgent and CompositeStore.DeleteProject remove the row
# (and, for a project, its agents) but do not deactivate delegation edges or
# service-account assignments: those rows have no foreign key to the agent,
# so they would outlive it as active rows. A caller must deactivate them
# first, in the same transaction (see the doc comments on the two methods in
# pkg/store/store.go and pkg/store/entadapter/composite.go). Agent removal
# normally goes through the delete engine (FinalizeAgentDeletion with
# hardDeleteAgentTx) and project removal through ProjectDeletionService,
# both of which do so.
#
# Every remaining caller is listed in hack/store-delete-callers-allowlist.txt
# as "<file> | <function> | <count>", with a comment saying how it meets the
# contract. <function> is the enclosing top-level function (a call in a
# closure counts for the function that contains it; "-" for a call outside
# any function). No line numbers: entries survive unrelated edits. The gate
# fails when:
#   - a (file, function) pair has more calls than its entry (or has none), or
#   - a pair has FEWER calls than its entry, or an entry matches nothing:
#     lower the count (or delete the entry) in the same change that removes
#     the call.
#
# What counts: every selector reference .DeleteAgent or .DeleteProject in a
# non-test .go file (a call, a method value such as f := st.DeleteAgent, or
# a call whose arguments continue on the next line). Not counted:
#   - comment lines, and a trailing comment after whitespace;
#   - method declarations (func (s *T) DeleteAgent(...) has no leading dot);
#   - longer identifiers (DeleteAgentOptions);
#   - a call whose first three arguments are visible on the same line, with
#     no bracket of any kind inside the first two
#     (.DeleteAgent(ctx, a, b, ...)). The store methods take exactly two
#     arguments (ctx, id), so this skips the broker, hub client and dispatch
#     methods of the same name without hiding a store call.
# LIMITATIONS: the check is textual and line-oriented. It does not see a
# call through a method expression with the receiver as an argument
# (store.Store.DeleteAgent(st, ctx, id) is skipped as a three-argument call),
# through reflection, or inside a /* */ block comment line (which is
# counted). A two-argument method of the same name on an unrelated type is
# counted and must be allowlisted with a comment saying so.
#
# Severity: SECURITY-GRADE (store delete contract)
#   Missing rg:          exit 3 (nothing analysed; see hack/lib/require-tool.sh)
#   Scan root missing:   exit 4
#   Allowlist missing:   exit 4
#   No call found:       exit 4 (the store implementation always calls itself)
#   Violations found:    exit 1
#   Clean:               exit 0
#
# Usage: hack/check-store-delete-callers.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

# sort and join must agree on collation.
export LC_ALL=C

name="check-store-delete-callers"

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool rg "$name" "ripgrep (rg)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Candidate lines: any selector reference to either method name.
pattern='\.(DeleteAgent|DeleteProject)\b'

helper_hint() {
  echo "Do not call the store's DeleteAgent or DeleteProject directly. Remove an" >&2
  echo "agent through the delete engine (FinalizeAgentDeletion with" >&2
  echo "hardDeleteAgentTx) and a project through ProjectDeletionService, which" >&2
  echo "deactivate the delegation edges and service-account assignments in the" >&2
  echo "same transaction. If a direct call is needed, deactivate those rows first" >&2
  echo "in the same transaction and add an entry, with a comment saying how, to" >&2
  echo "hack/store-delete-callers-allowlist.txt." >&2
}

# count_calls <root>
# Prints "<file>|<function> <count>" for every (file, enclosing function)
# with at least one counted reference under <root>, paths relative to <root>.
count_calls() {
  local root="$1"
  # rg exits 1 when nothing matches; the caller treats an empty result as
  # "nothing analysed".
  (cd "$root" || exit 4
   rg -l --color never -e "$pattern" --glob '*.go' --glob '!*_test.go' . 2>/dev/null || true) \
    | sed 's#^\./##' | sort | while IFS= read -r f; do
        awk -v file="$f" '
          /^func / {
            s = substr($0, 6)
            if (substr(s, 1, 1) == "(") { sub(/^\([^)]*\)[[:space:]]*/, "", s) }
            sub(/[[:space:](\[].*$/, "", s)
            fn = s
          }
          /^[[:space:]]*\/\// { next }
          {
            line = $0
            sub(/[[:space:]]+\/\/.*$/, "", line)
            while (match(line, /\.(DeleteAgent|DeleteProject)([^A-Za-z0-9_]|$)/)) {
              rest = substr(line, RSTART)
              line = substr(line, RSTART + 1)
              # Spelled out, not ([^...]*,){2}: mawk exhausts memory on the
              # bounded repeat.
              if (rest ~ /^\.(DeleteAgent|DeleteProject)\([^],(){}[]*,[^],(){}[]*,/) { continue }
              n[(fn == "" ? "-" : fn)]++
            }
          }
          END { for (k in n) print file "|" k " " n[k] }
        ' "$root/$f"
      done | sort
}

# scan <root> <allowlist> <label>
# Compares per-(file, function) counts under <root> with <allowlist>, whose
# paths are relative to <root>. Returns 0 clean, 1 violations, 4 when
# <root>/pkg/store is missing or nothing was counted.
scan() {
  local root="$1" allowlist="$2" label="$3"
  if [[ ! -d "$root/pkg/store" ]]; then
    echo "$name: scan root $root/pkg/store is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi
  if [[ ! -f "$allowlist" ]]; then
    echo "$name: allowlist $allowlist is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi

  local actual="$work/actual" expected="$work/expected"
  count_calls "$root" >"$actual"
  if [[ ! -s "$actual" ]]; then
    echo "$name: no DeleteAgent or DeleteProject reference found under $root — NOTHING WAS ANALYSED" >&2
    return 4
  fi

  # Allowlist: "<file> | <function> | <count>", '#' comments and blank lines
  # ignored. A malformed line is a parse error, reported as such (failing
  # closed) rather than as a count mismatch.
  local parsed="$work/parsed"
  if ! awk -F'|' '{ sub(/\r$/, "") }
             /^[[:space:]]*(#|$)/ { next }
             { f = $1; fn = $2; c = $3; gsub(/[[:space:]]/, "", f); gsub(/[[:space:]]/, "", fn); gsub(/[[:space:]]/, "", c)
               if (NF != 3 || f == "" || fn == "" || c !~ /^[0-9]+$/) {
                 printf "%s: allowlist parse error at line %d: want \"<file> | <function> | <count>\", got: %s\n", name, NR, $0 > "/dev/stderr"
                 bad = 1; next
               }
               print f "|" fn " " c }
             END { exit bad }' name="$name" \
    "$allowlist" >"$parsed"; then
    echo "Fix the malformed line(s) in $allowlist." >&2
    return 1
  fi
  sort "$parsed" >"$expected"

  local over under
  over="$(join -a1 -e0 -o '0,1.2,2.2' "$actual" "$expected" | awk '$2 > $3 { print "  " $1 ": " $2 " (allowlist " $3 ")" }')"
  under="$(join -a2 -e0 -o '0,1.2,2.2' "$actual" "$expected" | awk '$2 < $3 { print "  " $1 ": " $2 " (allowlist " $3 ")" }')"

  local total
  total="$(awk '{ s += $2 } END { print s + 0 }' "$actual")"
  echo "$name: analysed ${label}, ${total} direct DeleteAgent/DeleteProject reference(s)" >&2

  local rc=0
  if [[ -n "$over" ]]; then
    echo "$name: DeleteAgent/DeleteProject reference(s) outside the allowlist (file|function):" >&2
    echo "$over" >&2
    echo >&2
    helper_hint
    rc=1
  fi
  if [[ -n "$under" ]]; then
    echo "$name: allowlist is stale (fewer references than recorded, file|function):" >&2
    echo "$under" >&2
    echo "Lower the count, or delete the entry, in $allowlist." >&2
    rc=1
  fi
  return "$rc"
}

self_test() {
  local dir="$work/fixture" failed=0 rc out
  mkdir -p "$dir/pkg/store" "$dir/pkg/hub"

  cat >"$dir/pkg/store/impl.go" <<'EOF'
package store
func (c *Composite) DeleteAgent(ctx context.Context, id string) error {
	return c.inner.DeleteAgent(ctx, id)
}
EOF
  cat >"$dir/pkg/hub/a.go" <<'EOF'
package hub
func (s *Server) rollback(ctx context.Context) {
	cleanup := func() {
		_ = s.store.DeleteProject(ctx, id)
	}
	// s.store.DeleteAgent(ctx, id) in a comment is ignored
	x := 1 // s.store.DeleteAgent(ctx, id) trailing comment is ignored
	_ = c.client.DeleteAgent(ctx, brokerID, endpoint, id, projectID, opts)
	_ = d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, DeleteAgentOptions{
	})
	_ = DeleteAgentOptions{}
}
func other() {
	require.NoError(t, st.DeleteAgent(ctx, a))
	f := st.DeleteProject
	_ = st.DeleteAgent(
		ctx, id)
}
EOF
  printf 'package hub\nfunc t() { _ = st.DeleteAgent(ctx, id) }\n' >"$dir/pkg/hub/a_test.go"
  cat >"$dir/allow" <<'EOF'
# fixture
pkg/store/impl.go | DeleteAgent | 1
pkg/hub/a.go | rollback | 1
pkg/hub/a.go | other | 3
EOF

  rc=0; scan "$dir" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: allowlist match: rc=$rc (want 0)" >&2; failed=1; }

  printf 'func added() { _ = tx.DeleteAgent(ctx, id) }\n' >>"$dir/pkg/hub/a.go"
  rc=0; out="$(scan "$dir" "$dir/allow" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"pkg/hub/a.go|added: 1 (allowlist 0)"* ]] ||
    { echo "self-test FAIL: new function in allowlisted file: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/store/impl.go | DeleteAgent | 1\npkg/hub/a.go | rollback | 1\npkg/hub/a.go | other | 3\npkg/hub/a.go | added | 1\n' >"$dir/allow"
  printf 'package hub\nfunc b() { _ = s.store.DeleteProject(ctx, id) }\n' >"$dir/pkg/hub/b.go"
  rc=0; out="$(scan "$dir" "$dir/allow" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"pkg/hub/b.go|b: 1 (allowlist 0)"* ]] ||
    { echo "self-test FAIL: new file: rc=$rc (want 1)" >&2; failed=1; }
  rm "$dir/pkg/hub/b.go"

  printf 'func (s *Server) rollback2() { _ = s.store.DeleteProject(ctx, id) }\n' >>"$dir/pkg/hub/a.go"
  printf 'pkg/store/impl.go | DeleteAgent | 1\npkg/hub/a.go | rollback | 2\npkg/hub/a.go | other | 3\npkg/hub/a.go | added | 1\n' >"$dir/allow"
  rc=0; scan "$dir" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: function names must match exactly: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/store/impl.go | DeleteAgent | 1\npkg/hub/a.go | rollback | 1\npkg/hub/a.go | rollback2 | 1\npkg/hub/a.go | other | 4\npkg/hub/a.go | added | 1\n' >"$dir/allow"
  rc=0; out="$(scan "$dir" "$dir/allow" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"allowlist is stale"* ]] ||
    { echo "self-test FAIL: stale count: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/store/impl.go | DeleteAgent | 1\npkg/hub/a.go | rollback | 1\npkg/hub/a.go | rollback2 | 1\npkg/hub/a.go | other | 3\npkg/hub/a.go | added | 1\npkg/hub/gone.go | f | 1\n' >"$dir/allow"
  rc=0; scan "$dir" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: stale entry: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/store/impl.go | DeleteAgent | 1\npkg/hub/a.go | rollback | 1\npkg/hub/a.go | rollback2 | 1\npkg/hub/a.go | other | 3\npkg/hub/a.go | added | 1\n' >"$dir/allow"
  rc=0; scan "$dir" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: updated allowlist: rc=$rc (want 0)" >&2; failed=1; }

  printf 'pkg/store/impl.go | DeleteAgent 1\n' >"$dir/allow.bad"
  rc=0; out="$(scan "$dir" "$dir/allow.bad" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"allowlist parse error at line 1"* && "$out" != *"outside the allowlist"* ]] ||
    { echo "self-test FAIL: malformed allowlist: rc=$rc (want 1 with a parse error)" >&2; failed=1; }

  printf 'pkg/store/impl.go | DeleteAgent | one\n' >"$dir/allow.bad"
  rc=0; out="$(scan "$dir" "$dir/allow.bad" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"allowlist parse error at line 1"* ]] ||
    { echo "self-test FAIL: non-numeric count: rc=$rc (want 1 with a parse error)" >&2; failed=1; }

  rc=0; scan "$dir/missing" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: missing root: rc=$rc (want 4)" >&2; failed=1; }

  mkdir -p "$dir/empty/pkg/store"
  rc=0; scan "$dir/empty" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: nothing counted: rc=$rc (want 4)" >&2; failed=1; }

  if [[ $failed -ne 0 ]]; then
    return 1
  fi
  echo "$name: self-test passed" >&2
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" ]] && [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

rc=0
scan . hack/store-delete-callers-allowlist.txt "$sha" || rc=$?
if [[ $rc -eq 0 ]]; then
  echo "$name: no DeleteAgent/DeleteProject call outside the allowlist"
fi
exit "$rc"
