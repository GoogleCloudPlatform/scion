package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

func httpStatus(p0 string) int {
	return sub.HTTPStatus(p0)
}

func httpStatusText(p0 string) string {
	return sub.HttpStatusText(p0)
}

func newClient(p0 string) sub.APIClient {
	return sub.NewClient(p0)
}
