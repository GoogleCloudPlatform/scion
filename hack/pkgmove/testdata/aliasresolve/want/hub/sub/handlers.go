package sub

import (
	"fmt"
	"io"

	"example.com/fx/apierr"
)

// Handle writes an error for name.
func Handle(w io.Writer, name string) error {
	if name == "" {
		return apierr.WriteError(w, apierr.CodeNotFound, "empty")
	}
	var c apierr.Code = "custom"
	b := apierr.Box[string]{V: apierr.First([]string{name})}
	return apierr.WriteError(w, c, fmt.Sprint(b.V))
}

// HandleJSON writes name as JSON through a func value of the var alias.
func HandleJSON(w io.Writer, name string) error {
	write := apierr.WriteJSON
	return write(w, name)
}

// Writer returns the error writer as a value (through a wrapper alias).
func Writer() func(io.Writer, apierr.Code, string) error { return apierr.WriteError }

// Where reports its own call site through the var alias.
func Where() string { return apierr.CallerAt() }

// Safe runs f under guard.
func Safe(f func()) error { return apierr.Guard(f) }
