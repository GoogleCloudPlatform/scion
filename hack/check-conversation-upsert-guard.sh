#!/usr/bin/env bash
# Guard: no conversation row may be minted outside the messaging layer
# (pkg/messaging/) and the store layer (pkg/store/).
#
# The property this guard enforces is: "no conversation is minted outside the
# messaging layer." It is NOT a function-name check — it is an enumeration of
# every code path that can INSERT a row into the conversations table or modify
# the participant listing index.
#
# Conversation-minting surface (enumerated 2026-08-27, Option C added 2026-08-29):
#
#   Go method calls (must only appear in pkg/messaging/ and pkg/store/):
#     1.  UpsertConversationByExternalRef — the primary resolve-or-create path
#     2a. CreateConversation              — direct INSERT (no production callers
#                                           today, but the method is public)
#     2b. AddParticipant                  — modifies the participant listing
#                                           index; an unguarded call outside
#                                           the resolve flow corrupts
#                                           conversation visibility
#
#   Ent builder (must only appear in pkg/store/):
#     3. .Conversation.Create()         — raw ent builder; only used inside
#                                         pkg/store/entadapter/conversation_store.go
#
#   Raw SQL INSERT INTO conversations (must only appear in pkg/store/):
#     4. INSERT [OR IGNORE|OR REPLACE] INTO ["]conversations["]
#                                       — case-insensitive match for the full
#                                         INSERT family. SQLite uses OR IGNORE
#                                         for idempotent inserts; Postgres uses
#                                         ON CONFLICT ... DO NOTHING (same line).
#
#   EXEMPTION (Option C, tranche C): pkg/hub/webchannel_store.go and
#   pkg/hub/webchannel_store_postgres.go may contain raw INSERT INTO
#   conversations when the statement mints kind='group'. These files perform
#   an atomic topic+conversation dual-write inside an explicit tx; the store
#   methods take ctx, not tx, so they cannot participate in the webchat
#   transaction. Surfaces 1, 2a, 2b, and 3 remain fully barred in pkg/hub.
#
# Test files (*_test.go) are excluded: test fixtures legitimately call store
# methods to set up state. The guard protects production code paths.
#
# Enumeration method: grep -rn for each pattern across all .go files, then
# subtract the allowed packages. If a new minting surface is added to the
# store interface, it must be added to this guard.
#
# LIMITATIONS
# This guard is textual and line-oriented. It does NOT detect:
#   - SQL split across lines (e.g., "INSERT INTO\n    conversations ...")
#   - A table name supplied through a format verb or variable
#     (e.g., fmt.Sprintf("INSERT INTO %s ...", tbl))
# Both are low-risk in practice: every existing INSERT site in this codebase
# puts "INSERT INTO conversations" on a single line (house style), and no
# site constructs the table name dynamically. But a green gate from this
# script guarantees only that the enumerated textual patterns are absent
# outside the allowed packages — it is not a proof that no mint path exists.
#   - The Option C exemption is textual: it extracts the SQL statement
#     (from the INSERT line through the closing backtick of the Go raw
#     string literal) and requires 'group' to appear in that text while
#     rejecting any other kind literal ('direct', 'channel', 'support').
#     A kind value supplied through a variable (e.g., kindVar instead of
#     'group') defeats the exemption check and will be rejected by the
#     guard even if the variable always holds "group" at runtime. This is
#     deliberate — the guard cannot verify runtime values, so it requires
#     the literal.
#
# Note: the default fallback for unknown conversation kinds uses
# requireParticipant, making the participant table an ACL for any future
# kind someone forgets to case. The guard therefore protects both
# listing-index integrity and, indirectly, access control for unhandled kinds.
#
# EXIT CODES
#   0  no violations found
#   1  violations found
set -euo pipefail

cd "$(dirname "$0")/.."

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

rc=0

# --- Check 1: Go method calls that mint conversations ---
# UpsertConversationByExternalRef, CreateConversation, and AddParticipant
# must only be called from pkg/messaging/ and pkg/store/.
# Pattern uses POSIX ERE (-E flag). Do not rewrite with BRE alternation (\|)
# or word-boundary (\b) — those are GNU extensions that silently match nothing
# on BSD/macOS grep, causing the guard to report false-clean.
grep -rEn 'UpsertConversationByExternalRef|\.CreateConversation\(|\.AddParticipant\(' \
  --include='*.go' \
  --exclude='*_test.go' \
  . \
  | grep -v '^./pkg/messaging/' \
  | grep -v '^./pkg/store/' \
  | grep -v '^./vendor/' \
  >"$tmp" || true

if [[ -s "$tmp" ]]; then
  echo "Conversation-minting method called outside pkg/messaging/ and pkg/store/:" >&2
  cat "$tmp" >&2
  echo >&2
  echo "Direct calls to UpsertConversationByExternalRef, CreateConversation," >&2
  echo "and AddParticipant are not allowed outside pkg/messaging/ and pkg/store/." >&2
  echo "Use the messaging package's resolution helpers" >&2
  echo "(ResolveOrCreateConversationByKey, etc.)." >&2
  rc=1
fi

# --- Check 2: Ent builder conversation creation ---
# .Conversation.Create() and .Conversation.CreateBulk() must only appear
# inside pkg/store/ (where the ent adapter lives). pkg/ent/ is excluded
# because it contains auto-generated code from the ent framework.
# Pattern uses POSIX ERE (-E flag). Do not rewrite with BRE alternation (\|)
# or word-boundary (\b) — those are GNU extensions that silently match nothing
# on BSD/macOS grep, causing the guard to report false-clean.
: >"$tmp"
grep -rEn '\.Conversation\.Create(Bulk)?([^a-zA-Z]|$)' \
  --include='*.go' \
  --exclude='*_test.go' \
  . \
  | grep -v '^./pkg/store/' \
  | grep -v '^./pkg/ent/' \
  | grep -v '^./vendor/' \
  >"$tmp" || true

if [[ -s "$tmp" ]]; then
  echo "Ent conversation builder used outside pkg/store/:" >&2
  cat "$tmp" >&2
  echo >&2
  echo ".Conversation.Create() must only be used inside pkg/store/entadapter/." >&2
  rc=1
fi

# --- Check 3: Raw SQL INSERT INTO conversations ---
# Matches the full INSERT family: INSERT INTO, INSERT OR IGNORE INTO,
# INSERT OR REPLACE INTO, with optional quoting on the table name, and
# case-insensitive to catch any casing variant.
# Allowed in pkg/store/ unconditionally.
# Allowed in pkg/hub/webchannel_store{,_postgres}.go ONLY when the INSERT
# mints kind='group' (Option C). The kind literal must appear in the
# same SQL statement (bounded by the Go raw string backtick delimiter).
# Pattern uses POSIX ERE (-E flag). Do not rewrite with BRE alternation (\|)
# or word-boundary (\b) — those are GNU extensions that silently match nothing
# on BSD/macOS grep, causing the guard to report false-clean.
: >"$tmp"
grep -rEni 'INSERT[[:space:]]+(OR[[:space:]]+[A-Z]+[[:space:]]+)?INTO[[:space:]]+"?conversations"?' \
  --include='*.go' \
  --exclude='*_test.go' \
  . \
  | grep -v '^./pkg/store/' \
  | grep -v '^./vendor/' \
  >"$tmp" || true

if [[ -s "$tmp" ]]; then
  # Option C exemption: for matches in the two webchannel_store files,
  # extract the SQL statement that contains the INSERT and verify that
  # 'group' appears in it (and NO other kind literal such as 'direct').
  #
  # Statement extraction: all exempted INSERT sites live inside Go raw
  # string literals (backtick-delimited). From the INSERT line, we
  # collect text through the first line that contains a closing backtick,
  # or up to 20 lines (safety cap). The INSERT line itself is included
  # so single-line statements are correctly handled.
  violations=""
  while IFS= read -r match; do
    file="${match%%:*}"
    rest="${match#*:}"
    lineno="${rest%%:*}"

    # Normalise the path: strip leading "./" if present, so the
    # comparison works regardless of how grep formats the prefix.
    # The normalised value is compared against a fixed allowlist —
    # any format change causes the exemption to NOT apply (fail-closed).
    norm_file="${file#./}"

    # Exemption applies ONLY to these two files in pkg/hub/
    if [[ "$norm_file" = "pkg/hub/webchannel_store.go" || "$norm_file" = "pkg/hub/webchannel_store_postgres.go" ]]; then
      # Extract the SQL statement: from the INSERT keyword through the
      # closing backtick (end of Go raw string literal), cap at 20 lines.
      # On the first line, strip everything before INSERT so the opening
      # backtick of the Go raw string is removed — otherwise a single-line
      # INSERT (where both backticks are on the same line) would be
      # truncated at the opening backtick instead of the closing one.
      end=$((lineno + 20))
      stmt=$(sed -n "${lineno},${end}p" "$file")
      # Strip prefix before INSERT on the first line (removes opening `)
      stmt=$(printf '%s\n' "$stmt" | sed '1s/^[^Ii]*INSERT/INSERT/I')
      # Check for closing backtick — if absent, statement exceeds the
      # 20-line window and we cannot verify its kind. Refuse to exempt
      # (fail-closed).
      # shellcheck disable=SC2016  # backtick is a literal match target (Go raw string delimiter)
      if ! printf '%s\n' "$stmt" | grep -q '`'; then
        violations="${violations}${match}"$'\n'
        continue
      fi

      # Truncate at the first closing backtick (`) and stop processing
      # (q quits sed). This prevents text after the statement (comments,
      # next func, subsequent lines) from influencing the kind check.
      stmt=$(printf '%s\n' "$stmt" | sed '/`/{ s/`.*//; q; }' | head -21)

      # Check 1: 'group' must appear in the statement text.
      if ! printf '%s\n' "$stmt" | grep -q "'group'"; then
        violations="${violations}${match}"$'\n'
        continue
      fi
      # Check 2: no OTHER kind literal (e.g., 'direct') in the statement.
      if printf '%s\n' "$stmt" | grep -qE "'(direct|channel|support)'"; then
        violations="${violations}${match}"$'\n'
        continue
      fi
      continue  # Exempted: kind='group' confirmed, no conflicting kinds
    fi
    violations="${violations}${match}"$'\n'
  done <"$tmp"

  if [[ -n "$violations" ]]; then
    printf 'Raw SQL INSERT INTO conversations outside allowed packages:\n' >&2
    printf '%s' "$violations" >&2
    echo >&2
    printf 'Raw SQL conversation inserts are only allowed in pkg/store/.\n' >&2
    printf "Exception: pkg/hub/webchannel_store{,_postgres}.go may INSERT\n" >&2
    printf "with kind='group' (Option C — see script header).\n" >&2
    rc=1
  fi
fi

if [[ "$rc" -ne 0 ]]; then
  exit 1
fi

echo "check-conversation-upsert-guard: no violations"
