package imagecheck

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type imageRef struct {
	Registry   string
	Repository string
	Tag        string
}

func parseImageRef(image string) (imageRef, error) {
	if image == "" {
		return imageRef{}, fmt.Errorf("empty image reference")
	}

	ref := image
	tag := "latest"
	if i := strings.LastIndex(ref, ":"); i > 0 {
		possibleTag := ref[i+1:]
		if !strings.Contains(possibleTag, "/") {
			tag = possibleTag
			ref = ref[:i]
		}
	}

	registry := "docker.io"
	repo := ref

	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		registry = parts[0]
		repo = parts[1]
	} else if len(parts) == 1 {
		repo = "library/" + parts[0]
	}

	return imageRef{
		Registry:   registry,
		Repository: repo,
		Tag:        tag,
	}, nil
}

func checkRemoteImage(ctx context.Context, client HTTPClient, ref imageRef, now time.Time) CheckResult {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, ref.Tag)

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return CheckResult{
			Status:    "error",
			Error:     err.Error(),
			CheckedAt: now,
		}
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))

	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{
			Status:    "error",
			Error:     err.Error(),
			CheckedAt: now,
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return CheckResult{
			Status:    "valid",
			Source:    "registry",
			CheckedAt: now,
		}
	case resp.StatusCode == http.StatusNotFound:
		return CheckResult{
			Status:    "invalid",
			CheckedAt: now,
		}
	case resp.StatusCode == http.StatusUnauthorized:
		return CheckResult{
			Status:    "error",
			Error:     "registry requires authentication",
			CheckedAt: now,
		}
	default:
		return CheckResult{
			Status:    "error",
			Error:     fmt.Sprintf("registry returned %d", resp.StatusCode),
			CheckedAt: now,
		}
	}
}
