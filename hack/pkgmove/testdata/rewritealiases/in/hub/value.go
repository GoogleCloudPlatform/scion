package hub

import "io"

// errorWriter keeps a wrapper alias as a func value.
var errorWriter func(io.Writer, code, string) error = writeError
