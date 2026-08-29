# WS-5: Doc Syntax Parse-Check Test

**Date:** 2026-08-27
**Agent:** dev-ws5-parsecheck
**Task:** S5 WS-5 — Parse-check fenced `scion` examples against cobra tree

## What was done

Created `cmd/doc_syntax_test.go` (~90 lines) that:

1. **Extracts** every `scion ...` command line from fenced code blocks in four
   documentation files (messaging skill, messaging user docs, CLI reference,
   glossary).
2. **Parse-checks** each extracted line against the real cobra command tree using
   `rootCmd.Find()` + `cmd.ParseFlags()`, ensuring documented commands and flags
   exist in the shipped binary.
3. **Deny-list** checks that no fenced code block contains gated forms
   (`conv:` thread references, `#` thread references) that are not yet
   implemented (DEF-5).
4. **Rule 10 compliance** — two subtests prove the test catches errors:
   - `catches_bad_command`: a temp file with `scion nonexistent-command` is
     correctly rejected.
   - `catches_deny_listed_pattern`: a temp file with `scion message conv:abc123`
     is correctly flagged.

## Skip rules applied

Lines are skipped when they contain:
- `<` / `>` (documentation placeholders)
- `$` (shell interpolations)
- `[flags]` or `...` (usage patterns)
- `scion help` (cobra built-in, not discoverable via `Find`)
- Comment lines starting with `#`

## Known limitation

Parse-checking validates that commands and flags exist. It does NOT verify that
a command does what the prose says it does. This is documented in a comment at
the top of the test file.

## Test result

```
=== RUN   TestDocSyntax
=== RUN   TestDocSyntax/catches_bad_command
=== RUN   TestDocSyntax/catches_deny_listed_pattern
--- PASS: TestDocSyntax (0.00s)
    --- PASS: TestDocSyntax/catches_bad_command (0.00s)
    --- PASS: TestDocSyntax/catches_deny_listed_pattern (0.00s)
PASS
```
