package hub

// Aliases for symbols moved to
// example.com/fx/apierr
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"io"

	"example.com/fx/apierr"
)

type (
	errBox = apierr.ErrBox
)

var (
	WriteJSON = apierr.WriteJSON
)

func first[T any](p0 []T) T {
	return apierr.First[T](p0)
}

func writeError(p0 io.Writer, p1 apierr.Code, p2 string) error {
	return apierr.WriteError(p0, p1, p2)
}
