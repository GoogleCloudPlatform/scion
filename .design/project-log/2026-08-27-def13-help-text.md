# DEF-13: Document conversation-reference grammar in `scion message --help`

**Date:** 2026-08-27
**Agent:** dev-def13
**Branch:** scion/ca-msg-em8
**Commit:** 2e6178ee

## Problem

The `scion message` deprecation warnings for `--channel` and `--thread-id`
directed users to "use @<agent-name>" but the `--help` text never defined
that form, nor any of the other conversation-reference forms accepted by
`messaging.ParseReference` (`@<email>`, `conv:<uuid>`, `#<thread>`).

## Changes

### cmd/message.go

1. **Help text**: Added four new recipient forms to the Recipients section
   of `messageCmd.Long`:
   - `@<agent-name>` — preferred form for sending to agents
   - `@<email>` — global DM by email
   - `conv:<uuid>` — annotated as "not yet supported — errors"
   - `#<thread>` — annotated as "not yet supported — errors"

2. **Example**: Added `scion message @my-agent "Please review the PR"` to
   the Examples section. No `conv:` or `#` examples (deny-listed by policy).

3. **Deprecation table refactor**: Extracted hardcoded
   `emitDeprecationWarning` calls into a package-level
   `deprecationReplacements` table. `emitDeprecationWarnings` now iterates
   the table. This enables tests to assert that deprecation messages
   referencing conversation forms point to documented help text.

### cmd/message_help_test.go (new)

- `TestMessageHelpCoversAllRefForms`: table-driven test with 4 entries
  validated against `messaging.ParseReference`.
- Tripwire assertion: `require.Equal(t, 4, int(messaging.RefThread))` fails
  if a new `ReferenceKind` is added without updating the table.
- `catches_missing_form` subtest proves the assertion mechanism is
  load-bearing.
- `deprecation_warnings_reference_documented_forms` subtest iterates
  `deprecationReplacements` and asserts every message referencing a
  conversation form (`@<`, `conv:`, `#<`) has that form in `Long`.

### cmd/doc_syntax_test.go

- Added `cobra_help_deny_list` subtest: walks the entire cobra command tree,
  extracts `scion ...` lines from `Long` and `Example` fields, and runs
  `findDenyListProblems` against them. Catches anyone adding `conv:` or `#`
  to help-text examples.

## Verification

- `go test ./cmd/ -run TestMessageHelpCoversAllRefForms -v` — PASS (6/6 subtests)
- `go test ./cmd/ -run TestDocSyntax -v` — PASS (4/4 subtests)
- `go build ./cmd/scion/` — clean
