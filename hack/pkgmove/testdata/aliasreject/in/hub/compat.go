package hub

import (
	"io"

	"example.com/fx/apierr"
)

// notFound is a hand-written wrapper (not in a pkgmove alias file).
func notFound(w io.Writer, msg string) error { return apierr.WriteError(w, apierr.CodeNotFound, msg) }

// hook is a hand-written var that forwards to a func (it may be a hook).
var hook = apierr.WriteJSON

// writeJSON has the forwarding shape, but is hand-written.
func writeJSON(w io.Writer, s string) error { return apierr.WriteJSON(w, s) }
