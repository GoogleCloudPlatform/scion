package imagecheck

import (
	"context"
	"time"
)

type CheckResult struct {
	Status    string
	Source    string
	Error     string
	CheckedAt time.Time
}

type ImageChecker interface {
	Check(ctx context.Context, image string) CheckResult
}

// LocalImageExister checks whether a container image exists on the local daemon.
// Satisfied by runtime.Runtime.
type LocalImageExister interface {
	ImageExists(ctx context.Context, image string) (bool, error)
}

type Checker struct {
	local  LocalImageExister
	client HTTPClient
}

type Option func(*Checker)

func WithLocalChecker(l LocalImageExister) Option {
	return func(c *Checker) {
		c.local = l
	}
}

func WithHTTPClient(client HTTPClient) Option {
	return func(c *Checker) {
		c.client = client
	}
}

func NewChecker(opts ...Option) *Checker {
	c := &Checker{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Checker) Check(ctx context.Context, image string) CheckResult {
	now := time.Now()

	if c.local != nil {
		found, err := c.local.ImageExists(ctx, image)
		if err == nil && found {
			return CheckResult{
				Status:    "valid",
				Source:    "local",
				CheckedAt: now,
			}
		}
	}

	ref, err := parseImageRef(image)
	if err != nil {
		return CheckResult{
			Status:    "invalid",
			CheckedAt: now,
		}
	}

	return checkRemoteImage(ctx, c.client, ref, now)
}
