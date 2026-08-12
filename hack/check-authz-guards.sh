#!/usr/bin/env bash
# Flags authorization checks that live inside an identity-kind guard with no
# compensating deny for the other identity kinds.
#
# This is the regression guard for ptone/scion#591. The bug was that ~24 handler
# sites wrote their authorization gate as:
#
#     if u := GetUserIdentityFromContext(ctx); u != nil {
#         decision := s.authzService.CheckAccess(ctx, u, res, action)
#         if !decision.Allowed { /* 403 */ }
#     }
#     // no else — agents and brokers skip the whole gate, silently
#
# agentIdentityWrapper implements AgentIdentity but not UserIdentity, and
# brokerIdentityImpl implements neither, so GetUserIdentityFromContext returns
# nil for both and the guard is skipped. Use s.authorize()/s.authorizeMsg()
# (pkg/hub/authorize.go), which is fail-closed for every identity kind.
#
# WHAT THIS CHECKS, AND WHY NOT SOMETHING SIMPLER
#
# It deliberately does NOT flag "uses GetUserIdentityFromContext". Roughly 25
# sites use that getter legitimately, for attribution (CreatedBy/UpdatedBy,
# message sender, list filtering) — see handlers_github_app.go:121. A check that
# fired on all of them would be suppressed within a week.
#
# The signal is the *shape of the guard*, not the name of the getter. A guard is
# reported only when all three hold:
#
#   1. It opens with an identity-kind test, in either syntactic form:
#          if <ident> := GetUserIdentityFromContext(ctx); <ident> != nil {
#          if <ident>, ok := <x>.(UserIdentity); ok {
#   2. Its body performs an authorization decision — authzService.CheckAccess,
#      requireAdmin, or a hand-rolled Role() == / != "admin" comparison.
#   3. Nothing catches the identity kinds that fail the test: the block has no
#      `else`, and it does not end in an unconditional return/continue/break/
#      panic (which would mean the code after the block is the fall-through
#      deny — the shape authorizeProjectImport and handlers_env_secrets.go use).
#
# Attribution guards fail (2). Guards with `} else { Forbidden(w); return }`
# fail (3). Only the bypass shape is reported.
#
# A third shape is reported unconditionally, because it has no benign reading —
# an explicit allow when the caller is not a user:
#
#     userIdent := GetUserIdentityFromContext(ctx)
#     if userIdent == nil {
#         return true    // "not a user, so allow"
#     }
#
# That was checkBrokerDispatchAccess and canDispatchToBroker before #591.
#
# WHAT IT CANNOT CHECK
#
# A handler with no authorization at all — handleUpdateGitHubApp before #591,
# createProjectAgent, getGroupMember — has no guard to key on and is invisible
# to any lexical rule. That gap is the route-authz manifest, issue #598.
#
# So a green run from this script is NOT a statement that the codebase is
# authorized correctly, and it must not be read as one. It says only that nobody
# has reintroduced the specific mis-shaped-guard idiom below; a route that never
# checks anything in the first place passes silently. Verifying that every route
# has an authorization check is a separate problem and is tracked in #598.
#
# Run `hack/check-authz-guards.sh --self-test` to exercise the classifier
# against a fixture covering every verdict above.
#
# EXIT CODES
#
#   0  analysed, no violations
#   1  analysed, violations found — the list is on stderr
#   2  COULD NOT ANALYSE (rg missing, no candidate files) — nothing was examined
#
# 2 is separate from 1 on purpose. Both fail a build, which is the point: a run
# that examined no source must not be indistinguishable from a clean one to
# anything reading only the exit code, or this check becomes the thing it exists
# to prevent — a guard that never fires. But the two mean opposite things to
# whoever reads the log. 1 is a security finding against named code; 2 is a
# broken environment and accuses nobody. Note this differs from the exit-0-on-
# missing-rg convention in hack/check-project-compat-literals.sh; the difference
# is deliberate, because a formatting check that silently skips costs a reformat
# later, while an authorization check that silently skips ships a bypass.
set -euo pipefail

# Classifier. Walks each candidate guard with a brace-depth counter so the
# verdict depends on the block's real extent rather than on a fixed indentation
# level, and so composite literals in the body (Resource{...}) do not confuse it.
read -r -d '' classifier <<'AWK' || true
function is_opener(line) {
  return (line ~ /^[[:space:]]*(\} else )?if [A-Za-z_][A-Za-z0-9_]* := GetUserIdentityFromContext\(.*\); [A-Za-z_][A-Za-z0-9_]* != nil \{[[:space:]]*$/ ||
          line ~ /^[[:space:]]*(\} else )?if [A-Za-z_][A-Za-z0-9_]*, ok := [^;]*\.\(UserIdentity\); ok \{[[:space:]]*$/)
}
function open_block(line) {
  in_block = 1
  start = FNR
  opener = line
  sub(/^[[:space:]]+/, "", opener)
  depth = 1
  has_authz = 0
  last_body = ""
}
function count(line, re,   tmp) { tmp = line; return gsub(re, "&", tmp) }

FNR == 1 { in_block = 0; in_nil_block = 0; nil_var = ""; cur_func = "?" }

# Track the enclosing top-level function so findings can be named rather than
# merely located. Line numbers move whenever anything above them is edited; a
# function name does not, which makes both the allowlist entries and the
# reviewer checklist stable across unrelated changes. Only column-0 `func`
# matches, so closures inside a body do not clobber the name.
/^func / {
  fname = $0
  sub(/^func[[:space:]]+/, "", fname)
  sub(/^\([^)]*\)[[:space:]]*/, "", fname)
  sub(/[[:space:]]*\(.*$/, "", fname)
  cur_func = fname
}

{
  # Shape 3: an explicit "not a user, so allow" early return. Tracked separately
  # because it is a statement pair, not a guarded block.
  if (!in_block && !in_nil_block) {
    if ($0 ~ /^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*[[:space:]]*:?=[[:space:]]*GetUserIdentityFromContext\(/) {
      nil_var = $0
      sub(/^[[:space:]]*/, "", nil_var)
      sub(/[[:space:]]*:?=.*$/, "", nil_var)
      next
    }
    if (nil_var != "") {
      # Tolerate blank lines and comments between the assignment and the test.
      if ($0 ~ /^[[:space:]]*$/ || $0 ~ /^[[:space:]]*\/\//) next
      if ($0 ~ ("^[[:space:]]*if[[:space:]]+" nil_var "[[:space:]]*==[[:space:]]*nil[[:space:]]*\\{[[:space:]]*$")) {
        pending_nil = nil_var
        pending_nil_line = FNR
        nil_var = ""
        in_nil_block = 1
        nil_depth = 1
        nil_last = ""
        next
      }
      nil_var = ""
    }
  }

  if (in_nil_block) {
    nil_depth += count($0, "\\{") - count($0, "\\}")
    if (nil_depth <= 0) {
      if (nil_last ~ /^[[:space:]]*return[[:space:]]+true[[:space:]]*$/) {
        printf "%s:%d: %s(): if %s == nil { ... return true } (fail-open on non-user caller)\n",
          FILENAME, pending_nil_line, cur_func, pending_nil
      }
      in_nil_block = 0
      next
    }
    if ($0 !~ /^[[:space:]]*$/ && $0 !~ /^[[:space:]]*\/\//) nil_last = $0
    next
  }

  if (in_block) {
    depth += count($0, "\\{") - count($0, "\\}")
    if (depth <= 0) {
      # This line closes the guard. Report only if an authorization decision was
      # made inside and nothing catches the other identity kinds.
      if (has_authz &&
          $0 !~ /\}[[:space:]]*else\b/ &&
          last_body !~ /^[[:space:]]*(return|continue|break|panic\()/) {
        printf "%s:%d: %s(): %s\n", FILENAME, start, cur_func, opener
      }
      in_block = 0
      # A `} else if <ident>, ok := ...` line closes one guard and opens the
      # next, so fall through to opener detection rather than skipping the line.
      if (is_opener($0)) open_block($0)
      next
    }
    if ($0 ~ /authzService\.CheckAccess/ ||
        $0 ~ /requireAdmin\(/ ||
        $0 ~ /\.Role\(\)[[:space:]]*[!=]=[[:space:]]*"admin"/) {
      has_authz = 1
    }
    if ($0 !~ /^[[:space:]]*$/ && $0 !~ /^[[:space:]]*\/\//) last_body = $0
    next
  }

  if (is_opener($0)) open_block($0)
}
AWK

# --------------------------------------------------------------------------
# Self-test: every verdict the classifier is responsible for, in one fixture.
# A `// WANT` marker sits on the line directly above a guard that must be
# flagged; `// WANT-NOT` above one that must not be. The attribution case is the
# canonical pair with the first case — same getter, opposite verdict — because
# keying on the getter name instead of the guard shape is the specific way this
# check would go wrong.
# --------------------------------------------------------------------------
if [[ "${1:-}" == "--self-test" ]]; then
  fixture_dir="$(mktemp -d)"
  fixture="$fixture_dir/fixture.go"
  cat >"$fixture" <<'FIXTURE'
package fixture

// Authorization gate inside an identity-kind guard, no else. The #591 shape.
func (s *Server) gate(w http.ResponseWriter, r *http.Request) {
	// WANT
	if user := GetUserIdentityFromContext(ctx); user != nil {
		decision := s.authzService.CheckAccess(ctx, user, agentResource(a), ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	}
	doTheThing()
}

// Attribution only — handlers_github_app.go:121. Same getter as the gate above,
// but it makes no authorization decision. Flagging this is the failure mode
// this check exists to avoid.
func (s *Server) attribution(w http.ResponseWriter, r *http.Request) {
	userID := ""
	// WANT-NOT
	if user := GetUserIdentityFromContext(ctx); user != nil {
		userID = user.ID()
	}
	s.setGitHubAppSecret(ctx, key, value, desc, userID)
}

// Type-assertion form of the same gate (idiom 2), no else. The composite
// literal in the body must not confuse the brace counter.
func (s *Server) assertGate(w http.ResponseWriter, r *http.Request) {
	// WANT
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type: "project",
			ID:   project.ID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// Compensated by an else branch.
func (s *Server) withElse(w http.ResponseWriter, r *http.Request) {
	// WANT-NOT
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, res, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}
	doTheThing()
}

// Compensated by a fall-through deny after an unconditional return — the
// authorizeProjectImport / handlers_env_secrets.go shape.
func (s *Server) fallThrough(w http.ResponseWriter, r *http.Request) bool {
	// WANT-NOT
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, res, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return false
		}
		return true
	}
	Forbidden(w)
	return false
}

// Hand-rolled admin comparison rather than CheckAccess, no else.
func (s *Server) handRolled(w http.ResponseWriter, r *http.Request) {
	// WANT
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		if userIdent.Role() != "admin" && broker.CreatedBy != userIdent.ID() {
			Forbidden(w)
			return
		}
	}
	doTheThing()
}

// Shape 3 — explicit allow for non-user callers.
func (s *Server) failOpen(ctx context.Context) bool {
	userIdent := GetUserIdentityFromContext(ctx)
	// WANT
	if userIdent == nil {
		return true
	}
	return s.authzService.CheckAccess(ctx, userIdent, res, ActionRead).Allowed
}

// Same shape, but fail-closed.
func (s *Server) failClosed(ctx context.Context) bool {
	userIdent := GetUserIdentityFromContext(ctx)
	// WANT-NOT
	if userIdent == nil {
		return false
	}
	return s.authzService.CheckAccess(ctx, userIdent, res, ActionRead).Allowed
}

// Shape 3 with a comment between the assignment and the nil test. Still the
// same fail-open; the classifier must not be shaken off by the gap.
func (s *Server) failOpenCommented(ctx context.Context) bool {
	userIdent := GetUserIdentityFromContext(ctx)

	// Brokers and agents do not carry a user identity.
	// WANT
	if userIdent == nil {
		return true
	}
	return s.authzService.CheckAccess(ctx, userIdent, res, ActionRead).Allowed
}

// Nil branch mentions `return true` but does not end in an unconditional one:
// the block's own last statement denies. Not the fail-open shape.
func (s *Server) failClosedAfterBranch(ctx context.Context) bool {
	userIdent := GetUserIdentityFromContext(ctx)
	// WANT-NOT
	if userIdent == nil {
		if isInternalCaller(ctx) {
			return true
		}
		return false
	}
	return s.authzService.CheckAccess(ctx, userIdent, res, ActionRead).Allowed
}
FIXTURE

  want="$(grep -n '^[[:space:]]*// WANT$' "$fixture" | cut -d: -f1 | awk '{print $1 + 1}')"
  got="$(awk "$classifier" "$fixture" | cut -d: -f2)"

  if [[ "$want" == "$got" ]]; then
    echo "check-authz-guards self-test: PASS ($(grep -c '// WANT$' "$fixture") flagged, $(grep -c '// WANT-NOT$' "$fixture") correctly ignored)"
    rm -rf "$fixture_dir"
    exit 0
  fi
  echo "check-authz-guards self-test: FAIL" >&2
  echo "expected flagged lines:" >&2
  echo "$want" >&2
  echo "actual flagged lines:" >&2
  echo "$got" >&2
  rm -rf "$fixture_dir"
  exit 1
fi

cd "$(dirname "$0")/.."

# Report which tree was actually analysed, on every path.
#
# A stale checkout runs the tip's script against old source and produces output
# that is well-formed, plausible, and wrong — over-reporting looks exactly like a
# developer having missed work, and under-reporting looks like a clean build.
# Neither is detectable from the output itself, so the output has to carry its
# own provenance. Deciding whether this sha is the branch tip stays with the
# person acting on it: comparing against a remote would need either network I/O
# or a possibly-stale ref, which relocates the same lie somewhere harder to see.
provenance() {
  local sha
  if ! sha="$(git rev-parse --short HEAD 2>/dev/null)"; then
    # Covers both "not a worktree" and "git is not on PATH" — the caller cannot
    # tell those apart from here, so do not claim to.
    printf '(git unavailable or not a worktree)'
    return
  fi
  if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
    printf '%s-dirty' "$sha"
  else
    printf '%s' "$sha"
  fi
}

if ! command -v rg >/dev/null 2>&1; then
  echo "check-authz-guards: ripgrep (rg) not found — NOTHING WAS ANALYSED (skipped, not clean)" >&2
  exit 2
fi

# Narrow to files that mention the getter or the assertion at all. The
# classifier does the real work; this just keeps it off the other ~1500 Go files.
mapfile -t candidate_files < <(
  rg -l --glob '*.go' --glob '!**/*_test.go' \
    -e 'GetUserIdentityFromContext\(' \
    -e '\.\(UserIdentity\); ok \{' \
    cmd pkg extras 2>/dev/null || true
)

if [[ ${#candidate_files[@]} -eq 0 ]]; then
  # In this repo the getter always matches something: ~25 attribution sites use
  # it legitimately and are deliberately not being converted. Zero candidates
  # therefore means the scan ran somewhere unexpected — wrong cwd, empty or
  # partial checkout — not that the codebase is clean. If every last caller is
  # ever removed for real, fix this script deliberately rather than silencing it.
  echo "check-authz-guards: analysed $(provenance) — no candidate files matched, NOTHING WAS ANALYSED (skipped, not clean)" >&2
  exit 2
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

awk "$classifier" "${candidate_files[@]}" >"$tmp" || true

if [[ ! -s "$tmp" ]]; then
  echo "check-authz-guards: analysed $(provenance), no violations"
  exit 0
fi

# Anchored allowlist. Every entry must carry a comment stating why the guard is
# safe despite its shape. An empty allowlist is the acceptance criterion for
# #591 Part 1 — do not add entries to make a red build green. A new hit is
# either a real bypass (fix the call site with s.authorize) or a classifier
# false positive (fix this script, and extend the self-test fixture above).
#
# If an entry ever does become necessary, anchor it on file + enclosing function
# + guard shape, NOT on the line number. Line numbers move whenever anything
# above them is edited, and a stale line-anchored entry fails in the dangerous
# direction: it can drift onto an unrelated site and mask a real bypass that was
# never reviewed. Findings are printed as `file:line: function(): guard` for
# exactly this reason. Note that a function may contain more than one guard of
# the same shape (addGroupMember does), so file + function is not always unique;
# where it is not, say so in the entry's comment rather than assuming it pins
# one site.
allowed_paths=(
  # (intentionally empty)
  "^$"
)

allowlist="$(printf '%s\n' "${allowed_paths[@]}" | sed 's/\$$/:/' | paste -sd '|' -)"

violations="$(grep -Ev "$allowlist" "$tmp" || true)"
if [[ -n "$violations" ]]; then
  echo "check-authz-guards: analysed $(provenance) — confirm this is the branch tip before acting on the list below" >&2
  echo >&2
  echo "Authorization check inside an identity-kind guard with no fail-closed branch:" >&2
  echo "$violations" >&2
  echo >&2
  echo "These guards are skipped entirely for agent and broker callers, which is" >&2
  echo "the bypass class of ptone/scion#591. Replace the guard with the fail-closed" >&2
  echo "helper:" >&2
  echo >&2
  echo "    if !s.authorize(w, r, agentResource(agent), ActionDelete) { return }" >&2
  echo >&2
  echo "See pkg/hub/authorize.go. If a guard is genuinely safe in this shape, add" >&2
  echo "it to allowed_paths in this script with a comment explaining why." >&2
  exit 1
fi

echo "check-authz-guards: analysed $(provenance), no violations"
