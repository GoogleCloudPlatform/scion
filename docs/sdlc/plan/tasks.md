# Task Breakdown — Hello-World CLI

## Task 1: Build the greeting package

**Acceptance criteria:** AC-005, AC-006, AC-007

Create `pkg/greeting/greet.go` with a `Greet(name string, now time.Time) string` function that:
- Returns `"Hello, <name>! The current time is <HH:MM:SS>."` 
- Defaults to `"World"` if name is empty

**Subtasks:**
- [ ] Implement: Write `pkg/greeting/greet.go` and `pkg/greeting/greet_test.go` together. Tests must cover: default name, custom name, empty name with fixed time injection. Use `testify/assert`.
- [ ] Review: Verify against AC-005, AC-006, AC-007. Confirm deterministic time output in tests.

## Task 2: Build the CLI command and entry point

**Acceptance criteria:** AC-001, AC-002, AC-003, AC-004

Create `cmd/hello/main.go` with a cobra root command that:
- Accepts `--name` string flag (default: "World")
- Calls `greeting.Greet()` with the flag value and `time.Now()`
- Prints the result to stdout

**Subtasks:**
- [ ] Implement: Write `cmd/hello/main.go` with cobra command wiring and integration test in `cmd/hello/main_test.go`. Tests should build the binary and run it as a subprocess to verify end-to-end output format. Use `testify/assert` and `testify/require`.
- [ ] Review: Verify against AC-001, AC-002, AC-003, AC-004. Confirm `--name` flag works, empty name defaults correctly, and names with spaces are handled.

## Task 3: Verify build and test pipeline

**Acceptance criteria:** AC-001 through AC-007

**Subtasks:**
- [ ] Implement: Run `go build ./cmd/hello/` and `go test ./pkg/greeting/ ./cmd/hello/` to confirm everything compiles and passes. Fix any issues.
- [ ] Review: Verify all 7 acceptance criteria are covered by at least one passing test. Confirm the binary runs and produces expected output.
