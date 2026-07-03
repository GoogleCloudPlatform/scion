package imagecheck

import (
	"context"
	"log/slog"
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
		slog.Warn("image check: invalid image reference", "image", image, "error", err)
		return CheckResult{
			Status:    "invalid",
			Error:     err.Error(),
			CheckedAt: now,
		}
	}

	result := checkRemoteImage(ctx, c.client, ref, now)
	if result.Error != "" {
		slog.Warn("image check: remote check failed", "image", image, "registry", ref.Registry, "repo", ref.Repository, "tag", ref.Tag, "status", result.Status, "error", result.Error)
	}
	return result
}
