package sub

import (
	"io"

	"example.com/fx/apierr"
)

// Typed already imports the alias target package.
func Typed(w io.Writer) error {
	var c apierr.Code = apierr.CodeNotFound
	return apierr.WriteError(w, c, "typed")
}
