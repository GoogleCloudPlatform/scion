package imagecheck

import (
	"context"
	"time"
)

// LocalImageExister checks whether a container image exists on the local daemon.
// Satisfied by runtime.Runtime.
type LocalImageExister interface {
	ImageExists(ctx context.Context, image string) (bool, error)
}

func WithLocalChecker(l LocalImageExister) Option {
	return func(c *Checker) {
		c.local = l
	}
}

func checkLocalImage(ctx context.Context, local LocalImageExister, image string, now time.Time) (CheckResult, bool) {
	found, err := local.ImageExists(ctx, image)
	if err == nil && found {
		return CheckResult{
			Status:    "valid",
			Source:    "local",
			CheckedAt: now,
		}, true
	}
	return CheckResult{}, false
}
