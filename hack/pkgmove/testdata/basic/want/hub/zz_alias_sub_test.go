// Copyright 2026 Example Authors
// SPDX-License-Identifier: Apache-2.0

package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

func onlyForTests() string {
	return sub.OnlyForTests()
}
