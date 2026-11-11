package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

type (
	Alpha = sub.Alpha
	Beta  = sub.Beta
	Delta = sub.Delta
	Gamma = sub.Gamma
)
