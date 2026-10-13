package hub

import (
	"io"

	"example.com/fx/apierr"
)

// Typed already imports the alias target package.
func Typed(w io.Writer) error {
	var c apierr.Code = codeNotFound
	return writeError(w, c, "typed")
}
