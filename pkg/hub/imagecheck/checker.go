package imagecheck

import (
	"context"
	"net/http"
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

type Checker struct {
	local  LocalImageExister
	client HTTPClient
}

type Option func(*Checker)

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
	if c.client == nil {
		c.client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return c
}

func (c *Checker) SetLocal(l LocalImageExister) {
	c.local = l
}

func (c *Checker) Check(ctx context.Context, image string) CheckResult {
	now := time.Now()

	if c.local != nil {
		if result, found := checkLocalImage(ctx, c.local, image, now); found {
			return result
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
