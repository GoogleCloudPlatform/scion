# J-1 & J-2: Test Floor Assertions

**Date:** 2026-08-27
**Branch:** scion/ca-msg-em5
**Defects:** J-1 (message_deprecation_test.go), J-2 (doc_syntax_test.go)

## Problem

Both tests proved their mechanisms worked on synthetic fixtures but never
asserted the mechanism actually ran on real artifacts. Zero extractions or
zero files scanned was indistinguishable from all-correct.

## Changes

### J-2: cmd/doc_syntax_test.go — TestDocSyntax

- **Replaced `os.IsNotExist` skip with `require.NoError` hard fail** — renamed
  or moved doc files now cause an immediate test failure instead of silently
  vanishing from coverage.
- **Added `totalLines` accumulator and floor assertion** — after scanning all
  doc files, the test requires at least 9 extracted scion command lines. A drop
  means either the extractor broke or examples were deleted.

### J-1: cmd/message_deprecation_test.go — TestDeprecationWarnings_ReplacementsExist

- **Extracted standalone `findReplacementProblems` function** — quote-agnostic
  scanner that finds `scion <subcommand>` references in deprecation warning
  output and validates each resolves against rootCmd. Handles single-quoted,
  backtick-quoted, and unquoted references.
- **Rewrote main test body** to call `findReplacementProblems` and assert a
  floor of 6 replacement references (the actual count of `scion ...` commands
  in the 10 deprecation warnings).
- **Replaced `catches_nonexistent_replacement` subtest** with four targeted
  subtests exercising the extracted function: `catches_deepest_match_blind_spot`,
  `catches_backtick_quoted_unknown`, `catches_unquoted_unknown`, and
  `accepts_valid_replacement`.

### Floor value note

The architect specified a floor of 7 replacement references, but the actual
`emitDeprecationWarnings` function contains exactly 6 warnings with `scion`
command references (broadcast, all→broadcast, raw→keys, notify→notifications
subscribe, in→schedule create, at→schedule create). The remaining 4 warnings
(plain, channel, thread-id, cc) do not reference scion commands. Floor set to 6.

## Mutation Verification

### MUT-B: emitDeprecationWarning no-op
Made `emitDeprecationWarning` an empty function body.
**Result: FAIL (as expected)**
```
Error: "0" is not greater than or equal to "6"
Messages: expected at least 6 replacement references in deprecation warnings;
got 0 — the extractor may be broken or warnings were removed
```

### MUT-D: Backtick-quoted nonexistent command
Added `if cmd.Flags().Changed("wake")` branch emitting
`` use `scion agent poke` instead `` and set wake as Changed in the test.
**Result: FAIL (as expected)**
```
replacement command not found: scion agent poke
(from: Warning: --wake is deprecated: use `scion agent poke` instead)
```

### MUT-E: Nonexistent doc file paths
Renamed all four `docFiles` entries to nonexistent paths.
**Result: FAIL (as expected)**
```
Error: Received unexpected error:
  stat .../NONEXISTENT.md: no such file or directory
Messages: doc file missing: ../resources/platform_skills/scion-messaging/NONEXISTENT.md
  — update docFiles or restore the file
```

## Verification

- `go test -count=1 ./cmd/ -v -run 'TestDocSyntax|TestDeprecationWarnings'` — PASS
- `go test -count=1 ./cmd/` — PASS (full suite, 5.98s)
- All three mutation tests fail as expected and were reverted.
