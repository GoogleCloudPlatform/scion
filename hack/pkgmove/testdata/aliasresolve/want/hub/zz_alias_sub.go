package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

var (
	Handle     = sub.Handle
	HandleJSON = sub.HandleJSON
	Safe       = sub.Safe
	Shadowed   = sub.Shadowed
	Typed      = sub.Typed
	Where      = sub.Where
	Writer     = sub.Writer
)
