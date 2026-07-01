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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// mockHarnessConfigService is a mock implementation of
// hubclient.HarnessConfigService. Only the three methods the Resolver touches
// (Get, RequestDownloadURLs, DownloadFile) are wired to func fields; the rest
// satisfy the interface as no-ops.
type mockHarnessConfigService struct {
	getFunc             func(ctx context.Context, id string) (*hubclient.HarnessConfig, error)
	requestDownloadURLs func(ctx context.Context, id string) (*hubclient.DownloadResponse, error)
	downloadFileFunc    func(ctx context.Context, url string) ([]byte, error)
}

func (m *mockHarnessConfigService) List(ctx context.Context, opts *hubclient.ListHarnessConfigsOptions) (*hubclient.ListHarnessConfigsResponse, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) Get(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, id)
	}
	return nil, nil
}

func (m *mockHarnessConfigService) Create(ctx context.Context, req *hubclient.CreateHarnessConfigRequest) (*hubclient.CreateHarnessConfigResponse, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) Update(ctx context.Context, id string, req *hubclient.UpdateHarnessConfigRequest) (*hubclient.HarnessConfig, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) Delete(ctx context.Context, id string) error { return nil }

func (m *mockHarnessConfigService) Reimport(ctx context.Context, id string, sourceURL string) (*hubclient.ReimportHarnessConfigResponse, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) RequestUploadURLs(ctx context.Context, id string, files []hubclient.FileUploadRequest) (*hubclient.UploadResponse, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) Finalize(ctx context.Context, id string, manifest *hubclient.HarnessConfigManifest) (*hubclient.HarnessConfig, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) RequestDownloadURLs(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
	if m.requestDownloadURLs != nil {
		return m.requestDownloadURLs(ctx, id)
	}
	return nil, nil
}

func (m *mockHarnessConfigService) UploadFile(ctx context.Context, url string, method string, headers map[string]string, content io.Reader) error {
	return nil
}

func (m *mockHarnessConfigService) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	if m.downloadFileFunc != nil {
		return m.downloadFileFunc(ctx, url)
	}
	return nil, nil
}

func (m *mockHarnessConfigService) UploadFilesMultipart(ctx context.Context, id string, files []hubclient.FileInfo) error {
	return nil
}

func (m *mockHarnessConfigService) ReadFile(ctx context.Context, id, filePath string) ([]byte, error) {
	return nil, nil
}

func (m *mockHarnessConfigService) Validate(ctx context.Context, id string) (*hubclient.ValidationReport, error) {
	return nil, nil
}

var _ hubclient.HarnessConfigService = (*mockHarnessConfigService)(nil)

// TestHarnessConfigResolveSuccess exercises the end-to-end download path for a
// harness-config: metadata → download URLs → download+verify → cache, plus a
// second call that must hit the content-addressed cache.
func TestHarnessConfigResolveSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	settingsBody := []byte("model: claude\n")
	memoryBody := []byte("remember things")
	dlCalls := 0

	hcSvc := &mockHarnessConfigService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			return &hubclient.HarnessConfig{
				ID:          "hc-123",
				Name:        "test-hc",
				Harness:     "claude",
				ContentHash: "hc-hash-abc",
			}, nil
		},
		requestDownloadURLs: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{Path: "settings.json", URL: "file:///tmp/hc1", Hash: transfer.HashBytes(settingsBody)},
					{Path: "memory/CLAUDE.md", URL: "file:///tmp/hc2", Hash: transfer.HashBytes(memoryBody)},
				},
				Expires: time.Now().Add(time.Hour),
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			dlCalls++
			switch url {
			case "file:///tmp/hc1":
				return settingsBody, nil
			case "file:///tmp/hc2":
				return memoryBody, nil
			}
			return nil, errors.New("unknown URL")
		},
	}

	client := &mockHubClient{harnessConfigs: hcSvc}
	resolver := NewHarnessConfigResolver(cache, client)

	path, err := resolver.Resolve(context.Background(), "test-hc")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if path == "" {
		t.Fatal("Resolve() returned empty path")
	}
	if dlCalls != 2 {
		t.Errorf("expected 2 file downloads, got %d", dlCalls)
	}

	// Second call should hit the content-addressed cache: no further downloads.
	path2, err := resolver.Resolve(context.Background(), "test-hc")
	if err != nil {
		t.Fatalf("Resolve() second call error = %v", err)
	}
	if path2 != path {
		t.Errorf("second Resolve() should return cached path: got %q want %q", path2, path)
	}
	if dlCalls != 2 {
		t.Errorf("second Resolve() should not re-download, total downloads = %d", dlCalls)
	}
}

// TestHarnessConfigResolveWithHash verifies the fast cache path short-circuits
// before any Hub round-trip when the hash is already cached.
func TestHarnessConfigResolveWithHash(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	contentHash := "hc-known-hash"
	cachedPath, err := cache.Put(contentHash, map[string][]byte{"settings.json": []byte("cached")})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	hcSvc := &mockHarnessConfigService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			t.Error("Get() should not be called when hash matches cache")
			return nil, nil
		},
	}

	client := &mockHubClient{harnessConfigs: hcSvc}
	resolver := NewHarnessConfigResolver(cache, client)

	path, err := resolver.ResolveWithHash(context.Background(), "hc-999", contentHash)
	if err != nil {
		t.Fatalf("ResolveWithHash() error = %v", err)
	}
	if path != cachedPath {
		t.Errorf("ResolveWithHash() should return cached path: got %q want %q", path, cachedPath)
	}
}

// TestHarnessConfigResolveNotFound checks the not-found path (Get returns nil).
func TestHarnessConfigResolveNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	hcSvc := &mockHarnessConfigService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			return nil, nil
		},
	}

	client := &mockHubClient{harnessConfigs: hcSvc}
	resolver := NewHarnessConfigResolver(cache, client)

	if _, err := resolver.Resolve(context.Background(), "missing"); err == nil {
		t.Error("Resolve() should error for non-existent harness-config")
	}
}

// TestHarnessConfigResolveHashMismatch verifies integrity checking rejects a
// downloaded file whose content does not match the advertised hash.
func TestHarnessConfigResolveHashMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	hcSvc := &mockHarnessConfigService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			return &hubclient.HarnessConfig{ID: "hc-bad", ContentHash: "expected"}, nil
		},
		requestDownloadURLs: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{
				Files: []hubclient.DownloadURLInfo{
					{Path: "settings.json", URL: "mock://hc", Hash: "wrong-hash"},
				},
			}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			return []byte("content with wrong hash"), nil
		},
	}

	client := &mockHubClient{harnessConfigs: hcSvc}
	resolver := NewHarnessConfigResolver(cache, client)

	if _, err := resolver.Resolve(context.Background(), "hc-bad"); err == nil {
		t.Error("Resolve() should error on hash mismatch")
	}
}

// TestHarnessConfigResolveNoHubClient verifies a resolver built with a nil hub
// client reports the not-configured error rather than panicking.
func TestHarnessConfigResolveNoHubClient(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	resolver := NewHarnessConfigResolver(cache, nil)

	if _, err := resolver.Resolve(context.Background(), "hc"); err == nil {
		t.Error("Resolve() should error when hub client is nil")
	}
}
