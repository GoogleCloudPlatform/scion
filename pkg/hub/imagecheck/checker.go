package imagecheck

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

	var localErr error
	if c.local != nil {
		if result, found, err := checkLocalImage(ctx, c.local, image, now); found {
			return result
		} else if err != nil {
			localErr = err
		}
	}

	// Bare image names (no registry prefix) are local-only by convention.
	// Without a local checker we cannot determine availability, so return
	// "unknown" rather than probing a remote registry that will 401.
	if isBareImageName(image) {
		result := CheckResult{
			Status:    "unknown",
			CheckedAt: now,
		}
		if localErr != nil {
			result.Status = "error"
			result.Error = fmt.Sprintf("container runtime error: %v", localErr)
		}
		return result
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

// isBareImageName returns true when the image reference has no explicit
// registry prefix (no '.' or ':' in the first path component before any '/').
func isBareImageName(image string) bool {
	ref := image
	if i := strings.LastIndex(ref, ":"); i > 0 {
		possibleTag := ref[i+1:]
		if !strings.Contains(possibleTag, "/") {
			ref = ref[:i]
		}
	}
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) < 2 {
		return true
	}
	return !strings.Contains(parts[0], ".") && !strings.Contains(parts[0], ":")
}
