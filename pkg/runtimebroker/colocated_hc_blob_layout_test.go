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

package runtimebroker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// blobHCService stubs the harness-config calls a co-located broker makes:
// metadata, signed download URLs, and the download itself.
type blobHCService struct {
	hubclient.HarnessConfigService
	hc        *hubclient.HarnessConfig
	content   []byte
	downloads int
}

func (s *blobHCService) Get(context.Context, string) (*hubclient.HarnessConfig, error) {
	return s.hc, nil
}

func (s *blobHCService) RequestDownloadURLs(context.Context, string) (*hubclient.DownloadResponse, error) {
	return &hubclient.DownloadResponse{
		Files: []hubclient.DownloadURLInfo{{
			Path: "config.yaml", URL: "https://hub.invalid/blob", Hash: transfer.HashBytes(s.content), Size: int64(len(s.content)),
		}},
		ContentHash: s.hc.ContentHash,
	}, nil
}

func (s *blobHCService) DownloadFile(context.Context, string) ([]byte, error) {
	s.downloads++
	return s.content, nil
}

type blobHCHubClient struct {
	hubclient.Client
	hcs hubclient.HarnessConfigService
}

func (c *blobHCHubClient) HarnessConfigs() hubclient.HarnessConfigService { return c.hcs }

// TestHydrateHarnessConfig_BlobLayoutMissesDirectRead is the co-located
// acceptance of ptone/scion#4219 part 2: a blob-layout harness-config's
// storage path (<slug path>.<id>) is never a directory, only its .blobs
// sibling is, so a co-located broker's direct read misses and the
// harness-config is hydrated through the hub's download URLs, cached under
// the content hash the download response names. A legacy row whose tree is
// on disk keeps resolving directly.
func TestHydrateHarnessConfig_BlobLayoutMissesDirectRead(t *testing.T) {
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, LocalPath: t.TempDir(), Bucket: "local"})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	legacyPath := storage.HarnessConfigStoragePath("", "global", "", "shared-hc")
	legacyDir := stor.ObjectFSPath(legacyPath)
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), []byte("harness: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blobPath := legacyPath + ".hc-blob"
	blobsDir := stor.ObjectFSPath(blobPath + ".blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := []byte("harness: claude\nimage: blob:1\n")
	hash := transfer.ComputeContentHash([]transfer.FileInfo{{Path: "config.yaml", Hash: transfer.HashBytes(content)}})
	if err := os.WriteFile(filepath.Join(blobsDir, transfer.HashBytes(content)[len(transfer.HashPrefix):]), content, 0o644); err != nil {
		t.Fatal(err)
	}

	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	srv := newTestServer(t)
	connFor := func(svc *blobHCService) *HubConnection {
		client := &blobHCHubClient{hcs: svc}
		return &HubConnection{
			Name:         "local",
			IsColocated:  true,
			LocalStorage: stor,
			HubClient:    client,
			HCResolver:   templatecache.NewHarnessConfigResolver(cache, client),
		}
	}

	blobSvc := &blobHCService{
		hc:      &hubclient.HarnessConfig{ID: "hc-blob", Slug: "shared-hc", Scope: "global", StoragePath: blobPath, ContentHash: hash},
		content: content,
	}
	cfg := &CreateAgentConfig{HarnessConfig: "shared-hc", HarnessConfigID: "hc-blob", HarnessConfigHash: hash}
	path, err := srv.hydrateHarnessConfig(context.Background(), cfg, connFor(blobSvc))
	if err != nil {
		t.Fatalf("hydrateHarnessConfig (blob row) failed: %v", err)
	}
	if path == "" || path == legacyDir || path == stor.ObjectFSPath(blobPath) {
		t.Fatalf("blob-layout row must miss the direct read and hydrate, got %q", path)
	}
	if blobSvc.downloads != 1 {
		t.Errorf("expected one hydration download, got %d", blobSvc.downloads)
	}
	got, err := os.ReadFile(filepath.Join(path, "config.yaml"))
	if err != nil || string(got) != string(content) {
		t.Errorf("hydrated config.yaml = %q, %v; want %q", got, err, content)
	}

	// The same dispatch without a hash takes the resolveLocalResource path:
	// it misses as well.
	cfg = &CreateAgentConfig{HarnessConfig: "shared-hc", HarnessConfigID: "hc-blob"}
	if path, err = srv.hydrateHarnessConfig(context.Background(), cfg, connFor(blobSvc)); err != nil || path == "" || path == legacyDir {
		t.Fatalf("blob-layout row via resolveLocalResource: path=%q err=%v", path, err)
	}

	legacySvc := &blobHCService{
		hc:      &hubclient.HarnessConfig{ID: "hc-legacy", Slug: "shared-hc", Scope: "global", StoragePath: legacyPath, ContentHash: "sha256:legacy"},
		content: content,
	}
	cfg = &CreateAgentConfig{HarnessConfig: "shared-hc", HarnessConfigID: "hc-legacy", HarnessConfigHash: "sha256:legacy"}
	path, err = srv.hydrateHarnessConfig(context.Background(), cfg, connFor(legacySvc))
	if err != nil {
		t.Fatalf("hydrateHarnessConfig (legacy row) failed: %v", err)
	}
	if path != legacyDir {
		t.Errorf("legacy row should resolve directly to %q, got %q", legacyDir, path)
	}
	if legacySvc.downloads != 0 {
		t.Errorf("legacy direct read must not download, got %d downloads", legacySvc.downloads)
	}
}
