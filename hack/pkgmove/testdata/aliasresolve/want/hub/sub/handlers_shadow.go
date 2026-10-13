package sub

import (
	"io"

	apierr2 "example.com/fx/apierr"
)

// Shadowed has a parameter named like the target package.
func Shadowed(w io.Writer, apierr string) error {
	return apierr2.WriteError(w, apierr2.CodeNotFound, apierr)
}
