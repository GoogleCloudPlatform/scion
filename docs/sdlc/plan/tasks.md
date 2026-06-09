# Tasks: hello-sdlc

## Task 1: Build hello-sdlc CLI with greeting logic and flag parsing

**Acceptance criteria:** AC-001, AC-002, AC-003, AC-004

**Description:** Create `cmd/hello-sdlc/main.go` with a `Greet(name string) string` function that returns the formatted greeting, and a `main()` that parses `--name` (default `"World"`) and prints the result. Write unit tests for `Greet()` covering all four acceptance criteria.

**Files to create:**
- `cmd/hello-sdlc/main.go` — entrypoint with flag parsing + `Greet()` function
- `cmd/hello-sdlc/main_test.go` — unit tests for `Greet()` covering AC-001 through AC-004

**Subtasks:**
- [ ] Implement: Create main.go with `Greet()` function and `--name` flag parsing; create main_test.go with table-driven tests for default name, custom name, empty name, and name with spaces
- [ ] Review: Verify against AC-001, AC-002, AC-003, AC-004 — run `go test ./cmd/hello-sdlc/...` and `go build ./cmd/hello-sdlc/` to confirm all pass

## Task 2: Validate end-to-end behavior

**Acceptance criteria:** AC-001, AC-002

**Description:** Build the binary and run it manually (or via test script) to confirm the exact output format matches the spec for both default and custom name scenarios.

**Subtasks:**
- [ ] Implement: Build binary with `go build -o hello-sdlc ./cmd/hello-sdlc/` and run smoke tests: `./hello-sdlc` and `./hello-sdlc --name Alice`
- [ ] Review: Verify stdout output matches exact strings from AC-001 and AC-002
