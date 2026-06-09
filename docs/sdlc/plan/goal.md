# Goal: hello-sdlc CLI Tool

## Original Goal

Build a simple Go CLI tool called `hello-sdlc` that accepts a `--name` flag and prints `Hello, <name>! Built by the SDLC pipeline.` with a default of `World`. Include unit tests. This is a Stage 1 validation of the SDLC multi-agent pipeline.

## App Type Classification

**CLI_TOOL** — standalone command-line tool with flag parsing and formatted output.

## Research Summary

### Codebase Patterns

- **Module**: `github.com/GoogleCloudPlatform/scion` (Go 1.26.1)
- **CLI structure**: Existing CLIs live under `cmd/` with `main.go` entrypoints (e.g., `cmd/scion/main.go`, `cmd/sciontool/main.go`)
- **Test framework**: `testing` stdlib + `github.com/stretchr/testify/assert` for assertions
- **License**: Apache 2.0 — all source files require the standard Google LLC copyright header
- **Package pattern**: Entrypoint packages under `cmd/<tool>/` with `package main`

### Design Decisions

- **Standalone binary**: `hello-sdlc` should be a new `cmd/hello-sdlc/main.go` following existing patterns
- **Flag parsing**: Use Go stdlib `flag` package — no need for cobra/pflag for a single-flag tool
- **Output format**: Exact string `Hello, <name>! Built by the SDLC pipeline.\n` to stdout
- **Default**: `--name` defaults to `World`
- **Testability**: Extract greeting logic into a pure function that returns a string, test that function
