package sub

import (
	"errors"
	"os"
)

// home reads the environment at init; a staying initialiser sets it.
var home = os.Getenv("FX_HOME")

// errBad is built by a pure constructor.
var errBad = errors.New("bad")

// Home returns the value seen at init.
func Home() string { return home }

// ErrBad returns the sentinel.
func ErrBad() error { return errBad }
