---
name: go-expert
description: Expert Go developer guidance. Use when Gemini CLI needs to write, refactor, or optimize Go code, ensuring it follows idiomatic patterns, robust concurrency, and comprehensive testing standards.
---

# Go Expert

This skill provides procedural knowledge and best practices for developing high-quality, idiomatic Go software.

## Core Philosophies

- **Simplicity Over Complexity**: Prefer clear, readable code over clever or "magic" solutions.
- **Composition Over Inheritance**: Utilize interfaces and embedding to build modular systems.
- **Separation of Concerns**: Decouple data ingestion and domain logic from persistence and infrastructure.
- **Errors as Values**: Handle errors explicitly; do not use panics for flow control.
- **Share Memory by Communicating**: Use channels for coordination between goroutines when appropriate.

## Strategic Workflows

### 1. Architectural Standards

Maintain clear boundaries between different layers of the application:
- **Service/Domain Layer**: Contains business logic. It should be independent of any specific database or transport (HTTP/gRPC). It interacts with persistence through interfaces.
- **Repository/Persistence Layer**: Handles database transactions and queries. It implements the interfaces defined by the domain layer.
- **Ingestion/Transport Layer**: Handles incoming data (API requests, CLI inputs, message queues). It decodes/sanitizes data and delegates to the service layer.

**Rule**: Never mix data ingestion (e.g., parsing an HTTP request) and database transactions in the same function. Logic that "decides" what to do should be separated from logic that "does" the DB work.

### 2. Code Implementation & Refactoring

When writing or refactoring Go code, adhere to "Effective Go" principles:
- **Naming**: Use `camelCase` for private symbols, `PascalCase` for public ones. Short, descriptive names for local variables (e.g., `i` for index, `r` for reader).
- **Documentation**: ALWAYS write appropriate `go-doc` comments for every exported (public) package, variable, constant, function, and type. The comment must be a complete sentence that begins with the name of the symbol it describes.
- **Interfaces**: Define small, focused interfaces (e.g., `io.Reader`). Accept interfaces, return concrete types.
- **Structs**: Use pointers for large structs or when mutability is required; otherwise, use values.

For detailed idioms, see [references/idioms.md](references/idioms.md).

### 3. Concurrency & Coordination

Go's concurrency primitives are powerful but require careful management:
- **Goroutine Leaks**: Always ensure goroutines have a clear exit path (e.g., using `context.Context` or closing channels).
- **Race Conditions**: Use `sync.Mutex` or `sync.RWMutex` for simple shared state, or channels for complex orchestration.
- **WaitGroups**: Use `sync.WaitGroup` to wait for a collection of goroutines to finish.

For common concurrency patterns, see [references/concurrency.md](references/concurrency.md).

### 4. Testing & Validation

Testing is a first-class citizen in Go:
- **Table-Driven Tests**: Use slices of anonymous structs to define test cases for repetitive logic.
- **Subtests**: Use `t.Run` to isolate test cases and provide clear failure messages.
- **Mocks**: Prefer creating minimal implementations of interfaces over complex mocking frameworks.
- **Benchmarking**: Use `func BenchmarkXxx(b *testing.B)` to measure performance.

For testing best practices, see [references/testing.md](references/testing.md).

### 5. Dependency & Config Management

- Use `go mod tidy` to clean up `go.mod` and `go.sum`.
- **Configuration**: Use a centralized `Config` struct (see `config/config.go`). Config is typically loaded from a TOML-formatted `.env` file.
- Minimize external dependencies; leverage the standard library whenever possible.

For project-specific patterns, see [references/project_patterns.md](references/project_patterns.md).

## Tools and Environment

- **Logging**: Prefer structured logging (JSON) for performance metrics and request tracking. Use `logrus` or standard `fmt` for serialized output. See `shared/logging.go`.
- **Formatting**: Always run `go fmt` or `goimports` on save.
- **Linting**: Use `golangci-lint` for comprehensive static analysis.
- **Static Check**: Use `go vet` to catch common mistakes.
