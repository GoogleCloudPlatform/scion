// Copyright 2026 Example Authors
// SPDX-License-Identifier: Apache-2.0

package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

type (
	Pair[K comparable, V any] = sub.Pair[K, V]
	widget                    = sub.Widget
)

const (
	maxItems = sub.MaxItems
)

var (
	guard = sub.Guard
)

func Exported() *sub.Widget {
	return sub.Exported()
}

func Map[T any](p0 []T, p1 func(T) T) []T {
	return sub.Map[T](p0, p1)
}

func helper(p0 int, p1 ...string) (int, error) {
	return sub.Helper(p0, p1...)
}

func newWidget(p0 string) *sub.Widget {
	return sub.NewWidget(p0)
}
