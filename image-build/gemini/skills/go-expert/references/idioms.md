# Go Idioms and Best Practices

This document outlines idiomatic patterns for Go development.

## Documentation (go-doc)

ALWAYS write appropriate `go-doc` comments for **every** exported (public) variable, constant, function, method, and type. 

- The comment must be a complete sentence that begins with the name of the symbol it describes.
- Package-level documentation should be placed directly above the `package` declaration.

```go
// UserStore handles database operations for the User model.
// It encapsulates the underlying database driver.
type UserStore struct {
    db *sql.DB
}

// DefaultTimeout is the standard duration to wait before canceling a request.
const DefaultTimeout = 30 * time.Second

// Save inserts or updates a user in the database.
// It returns an error if the underlying database transaction fails.
func (s *UserStore) Save(ctx context.Context, user *User) error {
    // ...
}
```

## Error Handling

### Wrapping Errors
Use `%w` with `fmt.Errorf` to wrap errors, allowing callers to inspect them with `errors.Is` or `errors.As`.

```go
if err != nil {
    return fmt.Errorf("decompressing data: %w", err)
}
```

### Sentinels vs Error Types
- Use **Sentinels** (e.g., `var ErrNotFound = errors.New("not found")`) for simple comparisons.
- Use **Custom Error Types** (structs implementing `Error() string`) when you need to attach extra data.

## Structs and Receivers

### Pointer vs Value Receivers
- **Pointer Receivers**: Use if the method needs to modify the receiver, or if the struct is large (to avoid copying).
- **Value Receivers**: Use for small, immutable structs or simple data containers.
- **Consistency**: Don't mix receiver types for a single struct.

## Functional Options Pattern
Use for constructors with many optional parameters.

```go
type Server struct {
    port int
    timeout time.Duration
}

type Option func(*Server)

func WithPort(p int) Option {
    return func(s *Server) { s.port = p }
}

func NewServer(opts ...Option) *Server {
    s := &Server{port: 8080}
    for _, opt := range opts {
        opt(s)
    }
    return s
}
```

## Slice Tricks
- **Pre-allocation**: Use `make([]T, 0, capacity)` if you know the size beforehand to avoid re-allocations.
- **Filtering in-place**:
```go
b := a[:0]
for _, x := range a {
    if keep(x) {
        b = append(b, x)
    }
}
```
