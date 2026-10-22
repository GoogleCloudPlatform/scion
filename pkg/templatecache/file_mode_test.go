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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Tests for carrying the executable bit into the broker's cache
// (ptone/scion#5652).

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func TestEntryKey(t *testing.T) {
	const h = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if got := EntryKey(h, nil); got != h {
		t.Errorf("no modes: key = %q, want the bare content hash", got)
	}
	if got := EntryKey(h, map[string]os.FileMode{"a": 0o644, "b": 0o644}); got != h {
		t.Errorf("no executable file: key = %q, want the bare content hash", got)
	}
	x1 := EntryKey(h, map[string]os.FileMode{"a": 0o755, "b": 0o644})
	x2 := EntryKey(h, map[string]os.FileMode{"a": 0o644, "b": 0o755})
	if x1 == h || x2 == h || x1 == x2 {
		t.Errorf("executable keys must differ from the bare hash and from each other: %q %q", x1, x2)
	}
	if again := EntryKey(h, map[string]os.FileMode{"b": 0o644, "a": 0o755}); again != x1 {
		t.Errorf("key is not deterministic: %q vs %q", again, x1)
	}
	if err := validateEntryName(x1); err != nil {
		t.Errorf("executable key is not a valid entry name: %v", err)
	}
	// The broker records a hydrated template directory's name as the
	// agent's template hash, so the key keeps the content-hash form.
	for _, k := range []string{x1, x2} {
		if !transfer.IsContentHash(k) {
			t.Errorf("executable key %q is not in content-hash form", k)
		}
	}
}

// modeTemplateService returns a template service whose one template has
// the given metadata manifest and download entries; each file's content is
// its path.
func modeTemplateService(contentHash string, meta []hubclient.TemplateFile, dl []hubclient.DownloadURLInfo, downloads *int) *mockTemplateService {
	for i := range dl {
		dl[i].URL = "mem://" + dl[i].Path
		dl[i].Hash = transfer.HashBytes([]byte(dl[i].Path))
	}
	for i := range meta {
		meta[i].Hash = transfer.HashBytes([]byte(meta[i].Path))
	}
	return &mockTemplateService{
		getFunc: func(ctx context.Context, id string) (*hubclient.Template, error) {
			return &hubclient.Template{ID: "tpl-1", Name: "t", ContentHash: contentHash, Files: meta}, nil
		},
		requestDownloadURLs: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{Files: dl, ContentHash: contentHash}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			*downloads++
			for _, f := range dl {
				if f.URL == url {
					return []byte(f.Path), nil
				}
			}
			return nil, errors.New("unknown URL")
		},
	}
}

const modeTestHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// TestResolve_Modes covers the modes a hydrated file gets from a hub that
// sends them and from one that does not.
func TestResolve_Modes(t *testing.T) {
	tests := []struct {
		name string
		meta []hubclient.TemplateFile
		dl   []hubclient.DownloadURLInfo
		want map[string]os.FileMode
	}{
		{
			name: "hub sends modes",
			meta: []hubclient.TemplateFile{{Path: "bin/x", Mode: "0755"}, {Path: "a.md", Mode: "0644"}},
			dl:   []hubclient.DownloadURLInfo{{Path: "bin/x", Mode: "0755"}, {Path: "a.md", Mode: "0644"}},
			want: map[string]os.FileMode{"bin/x": 0o755, "a.md": 0o644},
		},
		{
			name: "setuid and group bits are dropped",
			meta: []hubclient.TemplateFile{{Path: "bin/s", Mode: "4755"}, {Path: "w", Mode: "0666"}},
			dl:   []hubclient.DownloadURLInfo{{Path: "bin/s", Mode: "4755"}, {Path: "w", Mode: "0666"}},
			want: map[string]os.FileMode{"bin/s": 0o755, "w": 0o644},
		},
		{
			name: "old hub: modes come from the metadata manifest",
			meta: []hubclient.TemplateFile{{Path: "bin/x", Mode: "0755"}, {Path: "a.md", Mode: "0644"}},
			dl:   []hubclient.DownloadURLInfo{{Path: "bin/x"}, {Path: "a.md"}},
			want: map[string]os.FileMode{"bin/x": 0o755, "a.md": 0o644},
		},
		{
			name: "legacy entries without a mode are 0644",
			meta: []hubclient.TemplateFile{{Path: "bin/x"}, {Path: "a.md"}},
			dl:   []hubclient.DownloadURLInfo{{Path: "bin/x"}, {Path: "a.md"}},
			want: map[string]os.FileMode{"bin/x": 0o644, "a.md": 0o644},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, err := New(t.TempDir(), DefaultMaxSize)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			client := &mockHubClient{templates: modeTemplateService(modeTestHash, tt.meta, tt.dl, &n)}
			path, err := NewHydrator(cache, client).Hydrate(context.Background(), "t")
			if err != nil {
				t.Fatal(err)
			}
			for p, want := range tt.want {
				if got := statMode(t, filepath.Join(path, filepath.FromSlash(p))); got != want {
					t.Errorf("%s: mode = %v, want %v", p, got, want)
				}
			}
			// The fast path finds the entry it wrote.
			again, err := NewHydrator(cache, client).Hydrate(context.Background(), "t")
			if err != nil || again != path || n != len(tt.dl) {
				t.Errorf("second hydration: path %q (want %q), err %v, downloads %d (want %d)", again, path, err, n, len(tt.dl))
			}
		})
	}
}

// TestResolve_StaleModelessEntryNotReused pins that an entry cached before
// modes were carried (every file 0644, under the bare content hash) is not
// served for a template with an executable file.
func TestResolve_StaleModelessEntryNotReused(t *testing.T) {
	cache, err := New(t.TempDir(), DefaultMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := cache.Put(modeTestHash, map[string][]byte{"bin/x": []byte("bin/x")})
	if err != nil {
		t.Fatal(err)
	}

	n := 0
	client := &mockHubClient{templates: modeTemplateService(modeTestHash,
		[]hubclient.TemplateFile{{Path: "bin/x", Mode: "0755"}},
		[]hubclient.DownloadURLInfo{{Path: "bin/x", Mode: "0755"}}, &n)}
	path, err := NewHydrator(cache, client).Hydrate(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if path == stale || n != 1 {
		t.Fatalf("stale 0644 entry reused (path %q, downloads %d)", path, n)
	}
	if got := statMode(t, filepath.Join(path, "bin", "x")); got != 0o755 {
		t.Errorf("bin/x: mode = %v, want 0755", got)
	}
}

// TestHarnessConfigResolve_ExecutableProvisionScript covers a
// harness-config bundle with an executable provision script.
func TestHarnessConfigResolve_ExecutableProvisionScript(t *testing.T) {
	cache, err := New(t.TempDir(), DefaultMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/usr/bin/env python3\n")
	cfg := []byte("harness: pi\n")
	svc := &mockHarnessConfigService{
		getFunc: func(ctx context.Context, id string) (*hubclient.HarnessConfig, error) {
			return &hubclient.HarnessConfig{ID: "hc-1", ContentHash: modeTestHash, Files: []hubclient.TemplateFile{
				{Path: "provision.py", Hash: transfer.HashBytes(script), Mode: "0755"},
				{Path: "config.yaml", Hash: transfer.HashBytes(cfg), Mode: "0644"},
			}}, nil
		},
		requestDownloadURLs: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			return &hubclient.DownloadResponse{ContentHash: modeTestHash, Files: []hubclient.DownloadURLInfo{
				{Path: "provision.py", URL: "mem://p", Hash: transfer.HashBytes(script), Mode: "0755"},
				{Path: "config.yaml", URL: "mem://c", Hash: transfer.HashBytes(cfg), Mode: "0644"},
			}}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			if url == "mem://p" {
				return script, nil
			}
			return cfg, nil
		},
	}
	path, err := NewHarnessConfigResolver(cache, &mockHubClient{harnessConfigs: svc}).Resolve(context.Background(), "hc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := statMode(t, filepath.Join(path, "provision.py")); got != 0o755 {
		t.Errorf("provision.py: mode = %v, want 0755", got)
	}
	if got := statMode(t, filepath.Join(path, "config.yaml")); got != 0o644 {
		t.Errorf("config.yaml: mode = %v, want 0644", got)
	}
}
