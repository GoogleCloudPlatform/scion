package imagecheck

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type mockLocalChecker struct {
	exists bool
	err    error
}

func (m *mockLocalChecker) ImageExists(_ context.Context, _ string) (bool, error) {
	return m.exists, m.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newMockHTTPClient(statusCode int, err error) HTTPClient {
	if err != nil {
		return &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, err
			}),
		}
	}
	return &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: statusCode,
				Body:       http.NoBody,
			}, nil
		}),
	}
}

func TestChecker_ValidLocal(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: true}),
	)
	result := c.Check(context.Background(), "scion-claude:latest")
	if result.Status != "valid" {
		t.Errorf("expected status valid, got %s", result.Status)
	}
	if result.Source != "local" {
		t.Errorf("expected source local, got %s", result.Source)
	}
}

func TestChecker_ValidRemote(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: false}),
		WithHTTPClient(newMockHTTPClient(http.StatusOK, nil)),
	)
	result := c.Check(context.Background(), "ghcr.io/myorg/scion-claude:latest")
	if result.Status != "valid" {
		t.Errorf("expected status valid, got %s", result.Status)
	}
	if result.Source != "registry" {
		t.Errorf("expected source registry, got %s", result.Source)
	}
}

func TestChecker_Invalid404(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: false}),
		WithHTTPClient(newMockHTTPClient(http.StatusNotFound, nil)),
	)
	result := c.Check(context.Background(), "ghcr.io/myorg/nonexistent:latest")
	if result.Status != "invalid" {
		t.Errorf("expected status invalid, got %s", result.Status)
	}
}

func TestChecker_ErrorNetwork(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: false}),
		WithHTTPClient(newMockHTTPClient(0, fmt.Errorf("connection refused"))),
	)
	result := c.Check(context.Background(), "ghcr.io/myorg/scion-claude:latest")
	if result.Status != "error" {
		t.Errorf("expected status error, got %s", result.Status)
	}
	if !strings.Contains(result.Error, "connection refused") {
		t.Errorf("expected error to contain 'connection refused', got %s", result.Error)
	}
}

func TestChecker_ErrorUnauthorized(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: false}),
		WithHTTPClient(newMockHTTPClient(http.StatusUnauthorized, nil)),
	)
	result := c.Check(context.Background(), "ghcr.io/myorg/scion-claude:latest")
	if result.Status != "error" {
		t.Errorf("expected status error, got %s", result.Status)
	}
	if !strings.Contains(result.Error, "auth") {
		t.Errorf("expected auth-related error message, got %s", result.Error)
	}
}

func TestChecker_LocalFoundSkipsRemote(t *testing.T) {
	remoteChecked := false
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			remoteChecked = true
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: true}),
		WithHTTPClient(client),
	)
	result := c.Check(context.Background(), "ghcr.io/myorg/scion-claude:latest")
	if result.Status != "valid" {
		t.Errorf("expected status valid, got %s", result.Status)
	}
	if result.Source != "local" {
		t.Errorf("expected source local, got %s", result.Source)
	}
	if remoteChecked {
		t.Error("remote check should not have been called when image found locally")
	}
}

func TestChecker_NoLocalFallsToRemote(t *testing.T) {
	c := NewChecker(
		WithHTTPClient(newMockHTTPClient(http.StatusOK, nil)),
	)
	result := c.Check(context.Background(), "ghcr.io/myorg/scion-claude:latest")
	if result.Status != "valid" {
		t.Errorf("expected status valid, got %s", result.Status)
	}
	if result.Source != "registry" {
		t.Errorf("expected source registry, got %s", result.Source)
	}
}

func TestChecker_BareImageNoLocal(t *testing.T) {
	remoteChecked := false
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			remoteChecked = true
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	c := NewChecker(WithHTTPClient(client))
	result := c.Check(context.Background(), "cogo:latest")
	if result.Status != "unknown" {
		t.Errorf("expected status unknown for bare image without local checker, got %s", result.Status)
	}
	if remoteChecked {
		t.Error("remote check should not be called for bare image names")
	}
}

func TestChecker_BareImageLocalNotFound(t *testing.T) {
	remoteChecked := false
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			remoteChecked = true
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: false}),
		WithHTTPClient(client),
	)
	result := c.Check(context.Background(), "cogo:latest")
	if result.Status != "unknown" {
		t.Errorf("expected status unknown for bare image not found locally, got %s", result.Status)
	}
	if remoteChecked {
		t.Error("remote check should not be called for bare image names")
	}
}

func TestChecker_BareImageLocalFound(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: true}),
	)
	result := c.Check(context.Background(), "cogo:latest")
	if result.Status != "valid" {
		t.Errorf("expected status valid, got %s", result.Status)
	}
	if result.Source != "local" {
		t.Errorf("expected source local, got %s", result.Source)
	}
}

func TestChecker_BareImageLocalError(t *testing.T) {
	c := NewChecker(
		WithLocalChecker(&mockLocalChecker{exists: false, err: fmt.Errorf("podman machine not running")}),
	)
	result := c.Check(context.Background(), "cogo:latest")
	if result.Status != "error" {
		t.Errorf("expected status error for bare image with local checker error, got %s", result.Status)
	}
	if !strings.Contains(result.Error, "podman machine not running") {
		t.Errorf("expected error to contain runtime failure details, got %s", result.Error)
	}
}

func TestIsBareImageName(t *testing.T) {
	tests := []struct {
		image string
		bare  bool
	}{
		{"cogo:latest", true},
		{"cogo", true},
		{"myorg/myimage:v2", true},
		{"ghcr.io/myorg/scion-claude:latest", false},
		{"us-docker.pkg.dev/proj/repo/img:v1", false},
		{"localhost:5000/myimage:latest", false},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := IsBareImageName(tt.image); got != tt.bare {
				t.Errorf("IsBareImageName(%q) = %v, want %v", tt.image, got, tt.bare)
			}
		})
	}
}

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		input    string
		registry string
		repo     string
		tag      string
	}{
		{"scion-claude:latest", "docker.io", "library/scion-claude", "latest"},
		{"ghcr.io/myorg/scion-claude:latest", "ghcr.io", "myorg/scion-claude", "latest"},
		{"us-docker.pkg.dev/proj/repo/img:v1", "us-docker.pkg.dev", "proj/repo/img", "v1"},
		{"scion-claude", "docker.io", "library/scion-claude", "latest"},
		{"myorg/myimage:v2", "docker.io", "myorg/myimage", "v2"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref, err := parseImageRef(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ref.Registry != tt.registry {
				t.Errorf("registry: got %q, want %q", ref.Registry, tt.registry)
			}
			if ref.Repository != tt.repo {
				t.Errorf("repo: got %q, want %q", ref.Repository, tt.repo)
			}
			if ref.Tag != tt.tag {
				t.Errorf("tag: got %q, want %q", ref.Tag, tt.tag)
			}
		})
	}
}

func TestParseImageRef_Empty(t *testing.T) {
	_, err := parseImageRef("")
	if err == nil {
		t.Error("expected error for empty image reference")
	}
}
