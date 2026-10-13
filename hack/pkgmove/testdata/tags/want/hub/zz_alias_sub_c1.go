//go:build !no_sqlite

package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

func sqliteLimit() int {
	return sub.SqliteLimit()
}
