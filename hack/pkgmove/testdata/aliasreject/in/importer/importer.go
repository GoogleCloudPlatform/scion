// Package importer overrides an exported var alias of hub.
package importer

import (
	"io"

	"example.com/fx/hub"
)

func init() {
	hub.WriteJSON = func(w io.Writer, s string) error { return nil }
}
