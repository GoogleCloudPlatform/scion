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
	Hash      string
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
	if IsBareImageName(image) {
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

// IsBareImageName returns true when the image reference has no explicit
// registry prefix (no '.' or ':' in the first path component before any '/').
func IsBareImageName(image string) bool {
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

type ImageEntityStatus struct {
	Exists         bool   `json:"exists"`
	Image          string `json:"image,omitempty"`
	Hash           string `json:"hash,omitempty"`
	NewerThanLocal bool   `json:"newer_than_local,omitempty"`
}

type ThreeWayImageResult struct {
	LocalShort       ImageEntityStatus `json:"local_short"`
	LocalLong        ImageEntityStatus `json:"local_long"`
	Remote           ImageEntityStatus `json:"remote"`
	ResolvedImage    string            `json:"resolved_image"`
	ResolutionSource string            `json:"resolution_source"`
}

// LocalImageIDer is an optional extension of LocalImageExister that can
// return the image ID (hash) for a local image.
type LocalImageIDer interface {
	ImageID(ctx context.Context, image string) (string, error)
}

func (c *Checker) CheckAll(ctx context.Context, shortImage, longImage string) ThreeWayImageResult {
	var result ThreeWayImageResult
	result.LocalShort.Image = shortImage
	result.LocalLong.Image = longImage
	result.Remote.Image = longImage

	if c.local != nil {
		if shortImage != "" {
			shortExists, _ := c.local.ImageExists(ctx, shortImage)
			result.LocalShort.Exists = shortExists
			if shortExists {
				if ider, ok := c.local.(LocalImageIDer); ok {
					if id, err := ider.ImageID(ctx, shortImage); err == nil {
						result.LocalShort.Hash = id
					}
				}
			}
		}

		if longImage != "" {
			longExists, _ := c.local.ImageExists(ctx, longImage)
			result.LocalLong.Exists = longExists
			if longExists {
				if ider, ok := c.local.(LocalImageIDer); ok {
					if id, err := ider.ImageID(ctx, longImage); err == nil {
						result.LocalLong.Hash = id
					}
				}
			}
		}
	}

	if longImage != "" {
		ref, err := parseImageRef(longImage)
		if err == nil {
			remoteResult := checkRemoteImage(ctx, c.client, ref, time.Now())
			if remoteResult.Status == "valid" {
				result.Remote.Exists = true
				result.Remote.Hash = remoteResult.Hash
				if result.LocalLong.Exists && result.Remote.Hash != "" && result.LocalLong.Hash != "" {
					result.Remote.NewerThanLocal = result.Remote.Hash != result.LocalLong.Hash
				}
			}
		}
	}

	switch {
	case result.LocalShort.Exists:
		result.ResolvedImage = shortImage
		result.ResolutionSource = "local_short"
	case result.LocalLong.Exists:
		result.ResolvedImage = longImage
		result.ResolutionSource = "local_long"
	case result.Remote.Exists:
		result.ResolvedImage = longImage
		result.ResolutionSource = "remote"
	default:
		result.ResolvedImage = longImage
		if shortImage != "" {
			result.ResolvedImage = shortImage
		}
		result.ResolutionSource = "none"
	}

	return result
}
