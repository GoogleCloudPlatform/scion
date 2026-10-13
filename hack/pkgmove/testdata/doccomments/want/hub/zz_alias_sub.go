package hub

// Aliases for symbols moved to
// example.com/fx/hub/sub
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"example.com/fx/hub/sub"
)

const (
	maxRetries = sub.MaxRetries
)

var (
	Helper = sub.Helper
)

func newJob(p0 string) *sub.Job {
	return sub.NewJob(p0)
}

func run() string {
	return sub.Run()
}

func writeError(p0 string) string {
	return sub.WriteError(p0)
}
