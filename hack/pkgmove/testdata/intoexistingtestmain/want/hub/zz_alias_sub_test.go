package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

type (
	policy = sub.Policy
)

const (
	maxRules = sub.MaxRules
)

func newPolicy(p0 string, p1 bool) *sub.Policy {
	return sub.NewPolicy(p0, p1)
}
