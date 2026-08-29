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
#   - The Option C exemption is textual: it checks for the literal string
#     'group' in a 3-line window after the INSERT line. A kind value supplied
#     through a variable (e.g., kindVar instead of 'group') defeats the
#     exemption check and will be rejected by the guard even if the variable
#     always holds "group" at runtime. This is deliberate — the guard cannot
#     verify runtime values, so it requires the literal.
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
# mints kind='group' (Option C). The kind literal is on the VALUES line
# (the line after the INSERT line), not on the INSERT line itself.
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
  # read a window of 3 lines after the INSERT line and check for 'group'
  # literal. If found, the site is exempted. If not (e.g., kind='direct'
  # or a variable), the site is still a violation.
  violations=""
  while IFS= read -r match; do
    file="${match%%:*}"
    rest="${match#*:}"
    lineno="${rest%%:*}"

    # Exemption applies ONLY to these two files in pkg/hub/
    if [[ "$file" = ./pkg/hub/webchannel_store.go || "$file" = ./pkg/hub/webchannel_store_postgres.go ]]; then
      # Read 3 lines after the INSERT line. House style: column list on
      # the INSERT line, VALUES (including kind) on the next line. A 3-line
      # window covers multi-line value lists and indentation variants.
      window=$(sed -n "$((lineno+1)),$((lineno+3))p" "$file")
      if printf '%s\n' "$window" | grep -q "'group'"; then
        continue  # Exempted: kind='group' literal found
      fi
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
