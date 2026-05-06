# Go Testing Standards

This document describes the standards for writing robust tests in Go.

## Table-Driven Tests
The standard pattern for testing multiple inputs/outputs for a single function.

```go
func TestAdd(t *testing.T) {
    tests := []struct {
        name     string
        a, b     int
        want     int
    }{
        {"positive", 1, 2, 3},
        {"negative", -1, -2, -3},
        {"zero", 0, 0, 0},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if got := Add(tt.a, tt.b); got != tt.want {
                t.Errorf("Add() = %v, want %v", got, tt.want)
            }
        })
    }
}
```

## Mocking and Interfaces
Prefer "Mocks by Implementation" over heavy reflection-based frameworks.

```go
type Downloader interface {
    Download(url string) ([]byte, error)
}

type MockDownloader struct {
    Res []byte
    Err error
}

func (m *MockDownloader) Download(url string) ([]byte, error) {
    return m.Res, m.Err
}
```

## Best Practices
- **Parallel Tests**: Use `t.Parallel()` to speed up test execution (ensure tests are thread-safe).
- **Cleanup**: Use `t.Cleanup(func() { ... })` for cleaning up resources like temp files or database connections.
- **Assertions**: Go's standard library doesn't have `assert` or `expect`. Use plain `if` statements and `t.Errorf`. If a project uses `testify/assert`, follow that convention.
- **Integration Tests**: Use `go test -tags=integration` to separate long-running or external-dependency tests.

## Coverage
Use `go test -coverprofile=cover.out` and `go tool cover -html=cover.out` to visualize code coverage.
