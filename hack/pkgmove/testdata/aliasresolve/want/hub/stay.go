package hub

import "io"

// Serve stays and calls the moved handlers.
func Serve(w io.Writer) error { return Handle(w, "served") }
