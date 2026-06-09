# Tasks — tempconv

## Task 1: Core conversion logic and unit tests
**Acceptance criteria:** AC-001, AC-002, AC-003, AC-004, AC-005, AC-006, AC-009, AC-010
**Subtasks:**
- [ ] Implement: Create `cmd/tempconv/convert.go` with conversion functions for all scale pairs, absolute zero validation, and scale normalization. Create `cmd/tempconv/convert_test.go` with table-driven tests covering all conversion paths, edge cases, and error conditions.
- [ ] Review: verify against AC-001 through AC-006, AC-009, AC-010

## Task 2: CLI entry point, flag parsing, and integration tests
**Acceptance criteria:** AC-001, AC-006, AC-007, AC-008
**Subtasks:**
- [ ] Implement: Create `cmd/tempconv/main.go` with flag parsing (--from, --to, positional value), error handling for missing/invalid args, and formatted output. Create integration-level tests for CLI argument handling.
- [ ] Review: verify against AC-007, AC-008, and end-to-end AC-001, AC-006
