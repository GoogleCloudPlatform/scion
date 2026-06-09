# Goal: Hello-World CLI Tool

## Original Goal

Build a simple hello-world CLI tool in Go that prints a greeting with the current time. The tool should accept an optional `--name` flag to personalize the greeting (default: "World"). Write comprehensive tests. This is a Stage 1 validation test of the SDLC multi-agent pipeline.

## App Type Classification

`CLI_TOOL`

## Research Summary

### Codebase Patterns Observed

- **Module**: `github.com/GoogleCloudPlatform/scion` (Go 1.26.1)
- **CLI framework**: `github.com/spf13/cobra` — all commands defined as `cobra.Command` vars in `cmd/` package
- **Test framework**: `github.com/stretchr/testify` (assert + require)
- **Entry points**: Each binary has a `cmd/<name>/main.go` with `package main` importing `cmd.Execute()`
- **Command registration**: Commands use `init()` to call `rootCmd.AddCommand()`
- **Test isolation**: Tests save/restore package-level state; use `t.TempDir()` and `t.Setenv()`

### Design Decisions

1. **Standalone binary**: New entry point at `cmd/hello/main.go` with its own `main()` — keeps this tool independent from the scion CLI while following the project's binary layout convention.
2. **Greeting package**: Core greeting logic in `pkg/greeting/` to separate business logic from CLI wiring, enabling unit testing without cobra.
3. **Testable time**: Inject a `timeNow` function (defaulting to `time.Now`) so tests produce deterministic output without mocking the clock globally.
4. **Output format**: `Hello, <Name>! The current time is <HH:MM:SS>.` — simple, unambiguous, parseable in tests.
