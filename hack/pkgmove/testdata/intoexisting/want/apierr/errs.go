package apierr

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
)

// code is an API error code.
type Code string

const CodeNotFound Code = "not_found"

// box holds one value.
type Box[T any] struct{ V T }

// writeError writes an API error.
func WriteError(w io.Writer, c Code, msg string) error {
	_, err := fmt.Fprintf(w, "%s: %s", c, msg)
	return err
}

// first returns the first element.
func First[T any](xs []T) T { return xs[0] }

// WriteJSON writes a JSON string value.
func WriteJSON(w io.Writer, s string) error {
	_, err := fmt.Fprintf(w, "%q", s)
	return err
}

// guard runs f and turns a panic into an error.
func Guard(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	f()
	return nil
}

// callerAt reports the file and line of its caller.
func CallerAt() string {
	_, file, line, _ := runtime.Caller(1)
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

// errBox is embedded by staying types.
type ErrBox struct{ Msg string }

func (e ErrBox) Text() string { return e.Msg }
