package hub

import (
	"io"

	"example.com/fx/apierr"
)

// errorWriter keeps a wrapper alias as a func value.
var errorWriter func(io.Writer, apierr.Code, string) error = writeError
