# Go Concurrency Patterns

This document covers common patterns for managing goroutines and synchronization.

## Basic Orchestration

### Done Channel Pattern
Signal goroutines to stop processing.

```go
func worker(done <-chan struct{}) {
    for {
        select {
        case <-done:
            return
        default:
            // Do work
        }
    }
}
```

### Context for Cancellation
The idiomatic way to handle timeouts and cancellations across API boundaries.

```go
func Operation(ctx context.Context) error {
    select {
    case <-time.After(1 * time.Second):
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

## Advanced Patterns

### Worker Pool
Limit the number of concurrent tasks.

```go
func pool(jobs <-chan int, results chan<- int, workers int) {
    var wg sync.WaitGroup
    for i := 0; i < workers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for j := range jobs {
                results <- process(j)
            }
        }()
    }
    wg.Wait()
    close(results)
}
```

### Fan-In / Fan-Out
- **Fan-Out**: Multiple goroutines read from the same channel to parallelize work.
- **Fan-In**: A single goroutine multiplexes multiple input channels into one.

### Pipeline
Connect stages where each stage is a group of goroutines running the same function.

## Synchronization Primitives
- **sync.Once**: Ensure a function is called exactly once (useful for lazy initialization).
- **sync.Pool**: Reuse objects to reduce GC pressure (use with caution).
- **atomic**: Use `sync/atomic` for low-level lock-free synchronization on primitive types.
