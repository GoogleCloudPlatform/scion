# I-2 / I-3: Doc Syntax Parse-Check Fixes

**Date:** 2026-08-27
**Branch:** scion/ca-msg-em5
**Files changed:** `cmd/doc_syntax_test.go`

## Problem

The architect's review of S5 (Docs) found two issues in `TestDocSyntax`:

### I-2: rootCmd.Find blind spot
Cobra's `Find` returns the deepest match and leaves the remainder as args — it
does NOT error on unknown subcommands. For example, `scion schedule message`
(which doesn't exist) passes the parse check silently because Find returns
`cmd=schedule, rest=["message"], err=nil`.

### I-3: Rule 10 subtests re-implement instead of calling
The `catches_bad_command` and `catches_deny_listed_pattern` subtests
re-implemented the checking logic independently. Deleting the main-body check
wouldn't cause the subtests to fail.

## Solution

### I-3: Extract checking logic into callable functions
Extracted two functions that return problem lists:
- `findCommandProblems(lines, source) []string` — command-find + subcommand + flag validation
- `findDenyListProblems(lines, denyPatterns, source) []string` — deny-list pattern matching

Both the main body and Rule 10 subtests call the same functions. The main body
asserts problems are empty; the subtests assert problems are non-empty for
known-bad inputs.

### I-2: Detect unconsumed subcommand-like tokens
After `rootCmd.Find(args)`, if the resolved command is a **pure group**
(`HasSubCommands() && !Runnable()`) and the first unconsumed token is not a
flag, verify it matches a registered subcommand name. The `!Runnable()` guard
prevents false positives on commands like `message` that have both subcommands
and their own `RunE` (accepting positional args like recipient names).

Added `catches_unconsumed_subcommand` subtest that proves the fix is
load-bearing: `scion schedule message --in 5m` is correctly caught because
`schedule` is a pure group and `message` is not one of its subcommands.

## Verification
- `go test -count=1 ./cmd/ -v -run TestDocSyntax` — all 3 subtests pass
- `go test -count=1 ./cmd/` — full suite passes
