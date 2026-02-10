// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package templatecache

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/transfer"
)

// mockTemplateService is a mock implementation of hubclient.TemplateService.
type mockTemplateService struct {
	getFunc              func(ctx context.Context, templateID string) (*hubclient.Template, error)
	requestDownloadURLs  func(ctx context.Context, templateID string) (*hubclient.DownloadResponse, error)
	downloadFileFunc     func(ctx context.Context, url string) ([]byte, error)
}

func (m *mockTemplateService) List(ctx context.Context, opts *hubclient.ListTemplatesOptions) (*hubclient.ListTemplatesResponse, error) {
	return nil, nil
}

func (m *mockTemplateService) Get(ctx context.Context, templateID string) (*hubclient.Template, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, templateID)
	}
	return nil, nil
}

func (m *mockTemplateService) Create(ctx context.Context, req *hubclient.CreateTemplateRequest) (*hubclient.CreateTemplateResponse, error) {
	return nil, nil
}

func (m *mockTemplateService) Update(ctx context.Context, templateID string, req *hubclient.UpdateTemplateRequest) (*hubclient.Template, error) {
	return nil, nil
}

func (m *mockTemplateService) Delete(ctx context.Context, templateID string) error {
	return nil
}

func (m *mockTemplateService) Clone(ctx context.Context, templateID string, req *hubclient.CloneTemplateRequest) (*hubclient.Template, error) {
	return nil, nil
}

func (m *mockTemplateService) RequestUploadURLs(ctx context.Context, templateID string, files []hubclient.FileUploadRequest) (*hubclient.UploadResponse, error) {
	return nil, nil
}

func (m *mockTemplateService) Finalize(ctx context.Context, templateID string, manifest *hubclient.TemplateManifest) (*hubclient.Template, error) {
	return nil, nil
}

func (m *mockTemplateService) RequestDownloadURLs(ctx context.Context, templateID string) (*hubclient.DownloadResponse, error) {
	if m.requestDownloadURLs != nil {
		return m.requestDownloadURLs(ctx, templateID)
	}
	return nil, nil
}

func (m *mockTemplateService) UploadFile(ctx context.Context, url string, method string, headers map[string]string, content io.Reader) error {
	return nil
}

func (m *mockTemplateService) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	if m.downloadFileFunc != nil {
		return m.downloadFileFunc(ctx, url)
	}
	return nil, nil
}

// mockHubClient is a mock implementation of hubclient.Client.
type mockHubClient struct {
	templates hubclient.TemplateService
}

func (m *mockHubClient) Agents() hubclient.AgentService                  { return nil }
func (m *mockHubClient) GroveAgents(groveID string) hubclient.AgentService { return nil }
func (m *mockHubClient) Groves() hubclient.GroveService                  { return nil }
func (m *mockHubClient) RuntimeBrokers() hubclient.RuntimeBrokerService      { return nil }
func (m *mockHubClient) Templates() hubclient.TemplateService            { return m.templates }
func (m *mockHubClient) Workspace() hubclient.WorkspaceService           { return nil }
func (m *mockHubClient) Users() hubclient.UserService                    { return nil }
func (m *mockHubClient) Env() hubclient.EnvService                       { return nil }
func (m *mockHubClient) Secrets() hubclient.SecretService                { return nil }
func (m *mockHubClient) Auth() hubclient.AuthService                     { return nil }
func (m *mockHubClient) Health(ctx context.Context) (*hubclient.HealthResponse, error) {
	return nil, nil
}

func TestHydrateSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	templateSvc := &mockTemplateService{
		getFunc: func(ctx context.Context, templateID string) (*hubclient.Template, error) {
			return &hubclient.Template{
				ID:          "tmpl-123",
				Name:        "test-template",
				ContentHash: "hash-abc",
			}, nil
		},
		requestDownloadURLs: func(ctx context.Context, templateID string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{Path: "scion-agent.yaml", URL: "file:///tmp/test1", Hash: transfer.HashBytes([]byte("harness: claude\n"))},
					{Path: "home/config.txt", URL: "file:///tmp/test2", Hash: transfer.HashBytes([]byte("config data"))},
				},
				Expires: time.Now().Add(time.Hour),
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			switch url {
			case "file:///tmp/test1":
				return []byte("harness: claude\n"), nil
			case "file:///tmp/test2":
				return []byte("config data"), nil
			}
			return nil, errors.New("unknown URL")
		},
	}

	client := &mockHubClient{templates: templateSvc}
	hydrator := NewHydrator(cache, client)

	// Hydrate template
	path, err := hydrator.Hydrate(context.Background(), "test-template")
	if err != nil {
		t.Fatalf("Hydrate() error = %v", err)
	}
	if path == "" {
		t.Error("Hydrate() returned empty path")
	}

	// Second call should use cache
	path2, err := hydrator.Hydrate(context.Background(), "test-template")
	if err != nil {
		t.Fatalf("Hydrate() second call error = %v", err)
	}
	if path2 != path {
		t.Errorf("Second Hydrate() should return cached path")
	}
}

func TestHydrateWithHash(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Pre-populate cache
	files := map[string][]byte{"test.txt": []byte("cached content")}
	contentHash := "known-hash"
	cachedPath, err := cache.Store("tmpl-999", contentHash, files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Create hydrator with mock that should not be called
	templateSvc := &mockTemplateService{
		getFunc: func(ctx context.Context, templateID string) (*hubclient.Template, error) {
			t.Error("Get() should not be called when hash matches cache")
			return nil, nil
		},
	}

	client := &mockHubClient{templates: templateSvc}
	hydrator := NewHydrator(cache, client)

	// Hydrate with known hash should use cache
	path, err := hydrator.HydrateWithHash(context.Background(), "tmpl-999", contentHash)
	if err != nil {
		t.Fatalf("HydrateWithHash() error = %v", err)
	}
	if path != cachedPath {
		t.Errorf("HydrateWithHash() should return cached path")
	}
}

func TestHydrateTemplateNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	templateSvc := &mockTemplateService{
		getFunc: func(ctx context.Context, templateID string) (*hubclient.Template, error) {
			return nil, nil // Template not found
		},
	}

	client := &mockHubClient{templates: templateSvc}
	hydrator := NewHydrator(cache, client)

	_, err = hydrator.Hydrate(context.Background(), "non-existent")
	if err == nil {
		t.Error("Hydrate() should error for non-existent template")
	}
}

func TestHydrateNoHubClient(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	hydrator := NewHydrator(cache, nil)

	_, err = hydrator.Hydrate(context.Background(), "template")
	if err == nil {
		t.Error("Hydrate() should error when hub client is nil")
	}
}

func TestIsHubConnectivityError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "HubConnectivityError",
			err:      &HubConnectivityError{Cause: errors.New("test")},
			expected: true,
		},
		{
			name:     "wrapped HubConnectivityError",
			err:      errors.New("wrapper: " + (&HubConnectivityError{Cause: errors.New("test")}).Error()),
			expected: false, // Not using errors.As, just wrapping
		},
		{
			name:     "connection refused",
			err:      errors.New("dial tcp: connection refused"),
			expected: true,
		},
		{
			name:     "no such host",
			err:      errors.New("no such host"),
			expected: true,
		},
		{
			name:     "timeout",
			err:      errors.New("request timeout"),
			expected: true,
		},
		{
			name:     "network unreachable",
			err:      errors.New("network is unreachable"),
			expected: true,
		},
		{
			name:     "regular error",
			err:      errors.New("some other error"),
			expected: false,
		},
		{
			name:     "ECONNREFUSED",
			err:      syscall.ECONNREFUSED,
			expected: true,
		},
		{
			name:     "url.Error",
			err:      &url.Error{Op: "Get", URL: "http://localhost", Err: errors.New("test")},
			expected: true,
		},
		{
			name:     "net.Error timeout",
			err:      &mockNetError{timeout: true},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsHubConnectivityError(tt.err)
			if result != tt.expected {
				t.Errorf("IsHubConnectivityError(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

type mockNetError struct {
	timeout   bool
	temporary bool
}

func (e *mockNetError) Error() string   { return "mock net error" }
func (e *mockNetError) Timeout() bool   { return e.timeout }
func (e *mockNetError) Temporary() bool { return e.temporary }

var _ net.Error = (*mockNetError)(nil)

func TestHubConnectivityError(t *testing.T) {
	cause := errors.New("connection refused")
	err := &HubConnectivityError{Cause: cause}

	// Test Error()
	if err.Error() == "" {
		t.Error("Error() should return non-empty string")
	}

	// Test Unwrap()
	if err.Unwrap() != cause {
		t.Error("Unwrap() should return the cause")
	}

	// Test errors.Is
	if !errors.Is(err, cause) {
		t.Error("errors.Is should find the cause")
	}
}

func TestPrefetchTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	callCount := 0
	templateSvc := &mockTemplateService{
		getFunc: func(ctx context.Context, templateID string) (*hubclient.Template, error) {
			callCount++
			return &hubclient.Template{
				ID:          "tmpl-prefetch",
				ContentHash: "prefetch-hash",
			}, nil
		},
		requestDownloadURLs: func(ctx context.Context, templateID string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{Path: "test.txt", URL: "mock://test", Hash: transfer.HashBytes([]byte("prefetch content"))},
				},
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			return []byte("prefetch content"), nil
		},
	}

	client := &mockHubClient{templates: templateSvc}
	hydrator := NewHydrator(cache, client)

	// Prefetch template
	err = hydrator.PrefetchTemplate(context.Background(), "tmpl-prefetch")
	if err != nil {
		t.Fatalf("PrefetchTemplate() error = %v", err)
	}

	if callCount != 1 {
		t.Errorf("Expected 1 Get call, got %d", callCount)
	}

	// Verify it's cached
	path, ok := cache.Get("tmpl-prefetch", "prefetch-hash")
	if !ok {
		t.Error("Template should be cached after prefetch")
	}
	if path == "" {
		t.Error("Cached path should not be empty")
	}
}

func TestHydrateHashMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	templateSvc := &mockTemplateService{
		getFunc: func(ctx context.Context, templateID string) (*hubclient.Template, error) {
			return &hubclient.Template{
				ID:          "tmpl-hash-mismatch",
				ContentHash: "expected-hash",
			}, nil
		},
		requestDownloadURLs: func(ctx context.Context, templateID string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{Path: "test.txt", URL: "mock://test", Hash: "wrong-hash"},
				},
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			return []byte("content with wrong hash"), nil
		},
	}

	client := &mockHubClient{templates: templateSvc}
	hydrator := NewHydrator(cache, client)

	_, err = hydrator.Hydrate(context.Background(), "tmpl-hash-mismatch")
	if err == nil {
		t.Error("Hydrate() should error on hash mismatch")
	}
}
