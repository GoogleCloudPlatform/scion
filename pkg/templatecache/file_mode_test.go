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
	ha, hb := transfer.HashBytes([]byte("a")), transfer.HashBytes([]byte("b"))
	base := []transfer.FileInfo{{Path: "a", Hash: ha, Mode: "0644"}, {Path: "b", Hash: hb, Mode: "0644"}}
	k := EntryKey(base)
	if k != transfer.ComputeContentHashV2(base) {
		t.Errorf("key = %q, want the v2 content hash of the files", k)
	}
	if again := EntryKey([]transfer.FileInfo{base[1], base[0]}); again != k {
		t.Errorf("key is not deterministic: %q vs %q", again, k)
	}
	if again := EntryKey([]transfer.FileInfo{{Path: "a", Hash: ha}, {Path: "b", Hash: hb}}); again != k {
		t.Errorf("a missing mode must key as 0644: %q vs %q", again, k)
	}
	x1 := EntryKey([]transfer.FileInfo{{Path: "a", Hash: ha, Mode: "0755"}, {Path: "b", Hash: hb, Mode: "0644"}})
	x2 := EntryKey([]transfer.FileInfo{{Path: "a", Hash: ha, Mode: "0644"}, {Path: "b", Hash: hb, Mode: "0755"}})
	renamed := EntryKey([]transfer.FileInfo{{Path: "a", Hash: ha, Mode: "0644"}, {Path: "c", Hash: hb, Mode: "0644"}})
	if x1 == k || x2 == k || x1 == x2 || renamed == k {
		t.Errorf("chmod-only and rename-only keys must differ from the base key and from each other: %q %q %q %q", k, x1, x2, renamed)
	}
	// The interim key of ptone/scion#5652 and a bare v1 content hash name
	// entries that are never reused.
	if v1 := transfer.ComputeContentHashV1(base); v1 == k {
		t.Errorf("key equals the v1 content hash %q", v1)
	}
	// The broker records a hydrated template directory's name as the
	// agent's template hash, so the key keeps the content-hash form.
	for _, key := range []string{k, x1, x2, renamed} {
		if !transfer.IsContentHash(key) {
			t.Errorf("key %q is not in content-hash form", key)
		}
		if err := validateEntryName(key); err != nil {
			t.Errorf("key %q is not a valid entry name: %v", key, err)
		}
	}
}

// memFile is one file of a mutableTemplateService version.
type memFile struct{ path, mode, content string }

// mutableTemplateService serves one template whose files the test
// replaces between hydrations, recording the hub content hash a v1 or a v2
// hub would.
type mutableTemplateService struct {
	*mockTemplateService
	hubVersion string // "v1" (a hub before ptone/scion#5658) or "v2"
	files      []memFile
	downloads  int
}

func newMutableTemplateService(hubVersion string, files []memFile) *mutableTemplateService {
	m := &mutableTemplateService{hubVersion: hubVersion, files: files}
	m.mockTemplateService = &mockTemplateService{
		getFunc: func(ctx context.Context, id string) (*hubclient.Template, error) {
			return &hubclient.Template{ID: "tpl-1", Name: "t", ContentHash: m.hubHash(), Files: m.manifest()}, nil
		},
		requestDownloadURLs: func(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
			var dl []hubclient.DownloadURLInfo
			for _, f := range m.manifest() {
				dl = append(dl, hubclient.DownloadURLInfo{Path: f.Path, URL: "mem://" + f.Path, Hash: f.Hash, Mode: f.Mode})
			}
			return &hubclient.DownloadResponse{Files: dl, ContentHash: m.hubHash()}, nil
		},
		downloadFileFunc: func(ctx context.Context, url string) ([]byte, error) {
			m.downloads++
			for _, f := range m.files {
				if "mem://"+f.path == url {
					return []byte(f.content), nil
				}
			}
			return nil, errors.New("unknown URL")
		},
	}
	return m
}

func (m *mutableTemplateService) manifest() []hubclient.TemplateFile {
	out := make([]hubclient.TemplateFile, len(m.files))
	for i, f := range m.files {
		out[i] = hubclient.TemplateFile{Path: f.path, Mode: f.mode, Hash: transfer.HashBytes([]byte(f.content))}
	}
	return out
}

func (m *mutableTemplateService) hubHash() string {
	var infos []transfer.FileInfo
	for _, f := range m.manifest() {
		infos = append(infos, transfer.FileInfo{Path: f.Path, Hash: f.Hash, Mode: f.Mode})
	}
	if m.hubVersion == "v1" {
		return transfer.ComputeContentHashV1(infos)
	}
	return transfer.ComputeContentHashV2(infos)
}

// TestResolve_RenameAndChmodOnlyChangesMissCache covers ptone/scion#5658 on
// the broker: a rename-only or chmod-only commit is never served from the
// entry of the version before it, whether the hub records the v2 content
// hash (which changes) or an older hub's v1 hash (which does not).
func TestResolve_RenameAndChmodOnlyChangesMissCache(t *testing.T) {
	base := []memFile{{"a.md", "0644", "doc"}, {"run.sh", "0644", "#!/bin/sh\n"}}
	cases := []struct {
		name string
		next []memFile
	}{
		{"chmod only", []memFile{{"a.md", "0644", "doc"}, {"run.sh", "0755", "#!/bin/sh\n"}}},
		{"rename only", []memFile{{"a.md", "0644", "doc"}, {"bin/run.sh", "0644", "#!/bin/sh\n"}}},
	}
	for _, tc := range cases {
		for _, hubVersion := range []string{"v1", "v2"} {
			t.Run(tc.name+" "+hubVersion+" hub", func(t *testing.T) {
				cache, err := New(t.TempDir(), DefaultMaxSize)
				if err != nil {
					t.Fatal(err)
				}
				svc := newMutableTemplateService(hubVersion, base)
				h := NewHydrator(cache, &mockHubClient{templates: svc})

				first, err := h.Hydrate(context.Background(), "t")
				if err != nil {
					t.Fatal(err)
				}
				before := svc.hubHash()
				svc.files = tc.next
				if hubVersion == "v1" && svc.hubHash() != before {
					t.Fatal("test premise: a v1 hub cannot see the change")
				}
				n := svc.downloads
				second, err := h.Hydrate(context.Background(), "t")
				if err != nil {
					t.Fatal(err)
				}
				if second == first || svc.downloads == n {
					t.Fatalf("%s served from the previous version's entry (path %q, downloads %d)", tc.name, second, svc.downloads-n)
				}
				for _, f := range tc.next {
					want := transfer.NormalizeMode(f.mode)
					if got := statMode(t, filepath.Join(second, filepath.FromSlash(f.path))); got != want {
						t.Errorf("%s: mode = %v, want %v", f.path, got, want)
					}
				}
				// The new version is now a cache hit.
				n = svc.downloads
				if again, err := h.Hydrate(context.Background(), "t"); err != nil || again != second || svc.downloads != n {
					t.Errorf("third hydration: path %q (want %q), err %v, downloads %d", again, second, err, svc.downloads-n)
				}
			})
		}
	}
}

// TestHydrateWithHash_OldKeyMisses covers an agent whose recorded
// TemplateHash is a key from before ptone/scion#5658 (a bare v1 content
// hash, or the interim key of ptone/scion#5652): the entry under that key
// is left in place, but hydration resolves through the hub and caches the
// files under the current key.
func TestHydrateWithHash_OldKeyMisses(t *testing.T) {
	cache, err := New(t.TempDir(), DefaultMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	svc := newMutableTemplateService("v1", []memFile{{"bin/x", "0755", "#!/bin/sh\n"}})
	oldKey := svc.hubHash()
	oldPath, err := cache.Put(oldKey, map[string][]byte{"bin/x": []byte("#!/bin/sh\n")})
	if err != nil {
		t.Fatal(err)
	}

	path, err := NewHydrator(cache, &mockHubClient{templates: svc}).HydrateWithHash(context.Background(), "tpl-1", oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if path == oldPath || svc.downloads != 1 {
		t.Fatalf("old-key entry reused (path %q, downloads %d)", path, svc.downloads)
	}
	if want := manifestEntryKey(svc.manifest()); filepath.Base(path) != want {
		t.Errorf("entry = %q, want the current key %q", filepath.Base(path), want)
	}
	if got := statMode(t, filepath.Join(path, "bin", "x")); got != 0o755 {
		t.Errorf("bin/x: mode = %v, want 0755", got)
	}
	// The old entry stays for lookups by the recorded hash until evicted.
	if got, ok := cache.Get(oldKey); !ok || got != oldPath {
		t.Errorf("old entry = (%q, %v), want (%q, true)", got, ok, oldPath)
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
