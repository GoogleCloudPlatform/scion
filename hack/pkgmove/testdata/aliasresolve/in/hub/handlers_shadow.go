package hub

import "io"

// Shadowed has a parameter named like the target package.
func Shadowed(w io.Writer, apierr string) error {
	return writeError(w, codeNotFound, apierr)
}
