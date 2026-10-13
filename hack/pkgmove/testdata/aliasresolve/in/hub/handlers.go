package hub

import (
	"fmt"
	"io"
)

// Handle writes an error for name.
func Handle(w io.Writer, name string) error {
	if name == "" {
		return writeError(w, codeNotFound, "empty")
	}
	var c code = "custom"
	b := box[string]{V: first([]string{name})}
	return writeError(w, c, fmt.Sprint(b.V))
}

// HandleJSON writes name as JSON through a func value of the var alias.
func HandleJSON(w io.Writer, name string) error {
	write := WriteJSON
	return write(w, name)
}

// Writer returns the error writer as a value (through a wrapper alias).
func Writer() func(io.Writer, code, string) error { return writeError }

// Where reports its own call site through the var alias.
func Where() string { return callerAt() }

// Safe runs f under guard.
func Safe(f func()) error { return guard(f) }
