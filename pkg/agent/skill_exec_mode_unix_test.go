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

//go:build !windows

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// execSkillFixture is a registry skill as the Hub serves it after
// ptone/scion#5652: a SKILL.md and a script, each with a mode.
type execSkillFixture struct {
	files      map[string][]byte
	modes      map[string]string // as the Hub sends them; "" = legacy hub
	bundleHash string
	downloads  atomic.Int32
	srv        *httptest.Server
}

func newExecSkillFixture(t *testing.T, modes map[string]string) *execSkillFixture {
	t.Helper()
	fx := &execSkillFixture{
		files: map[string][]byte{
			"SKILL.md":       []byte("# exec skill\n"),
			"scripts/run.sh": []byte("#!/bin/sh\necho ok\n"),
		},
		modes: modes,
	}
	var infos []transfer.FileInfo
	for p, c := range fx.files {
		infos = append(infos, transfer.FileInfo{Path: p, Hash: transfer.HashBytes(c)})
	}
	fx.bundleHash = transfer.ComputeContentHash(infos)
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.downloads.Add(1)
		c, ok := fx.files[r.URL.Path[1:]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(c)
	}))
	t.Cleanup(fx.srv.Close)
	return fx
}

// resolved returns the skill the way the broker gets it: a Hub resolve
// response converted by hubResolveResponseToResult.
func (fx *execSkillFixture) resolved(t *testing.T) ResolvedSkill {
	t.Helper()
	const uri = "skill://scion/core/exec-skill@1.0.0"
	var files []hubclient.DownloadURLInfo
	for _, p := range []string{"SKILL.md", "scripts/run.sh"} {
		files = append(files, hubclient.DownloadURLInfo{
			Path: p,
			URL:  fx.srv.URL + "/" + p,
			Size: int64(len(fx.files[p])),
			Hash: transfer.HashBytes(fx.files[p]),
			Mode: fx.modes[p],
		})
	}
	res := hubResolveResponseToResult([]api.SkillReference{{URI: uri}}, &hubclient.ResolveSkillsResponse{
		Resolved: []hubclient.ResolvedSkill{{
			URI: uri, Name: "exec-skill", ResolvedVersion: "1.0.0",
			ContentHash: fx.bundleHash, Files: files,
		}},
	})
	if len(res.Resolved) != 1 {
		t.Fatalf("resolved %d skills, want 1", len(res.Resolved))
	}
	return res.Resolved[0]
}

func assertFileModes(t *testing.T, dir string, want map[string]os.FileMode) {
	t.Helper()
	for p, m := range want {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != m || fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s: mode %v, want %04o", p, fi.Mode(), m)
		}
	}
}

func TestHubSkillResolve_CarriesFileMode(t *testing.T) {
	fx := newExecSkillFixture(t, map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755"})
	skill := fx.resolved(t)
	got := map[string]string{}
	for _, f := range skill.Files {
		got[f.Path] = f.Mode
	}
	if got["scripts/run.sh"] != "0755" || got["SKILL.md"] != "0644" {
		t.Fatalf("modes not carried from the resolve response: %v", got)
	}
}

func TestInstallOneSkill_ExecScriptRoundTrips(t *testing.T) {
	fx := newExecSkillFixture(t, map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755"})
	skill := fx.resolved(t)
	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithSkillCache(context.Background(), cache)
	want := map[string]os.FileMode{"SKILL.md": 0o644, "scripts/run.sh": 0o755}

	// First install downloads, installs with the recorded modes, and
	// caches the skill with them.
	dest1 := t.TempDir()
	if _, err := installOneSkill(ctx, skill, "exec-skill", dest1); err != nil {
		t.Fatal(err)
	}
	assertFileModes(t, filepath.Join(dest1, "exec-skill"), want)

	key := skillCacheKey(skill)
	if key == fx.bundleHash || !transfer.IsContentHash(key) {
		t.Fatalf("cache key %q: want a canonical key distinct from the bundle hash", key)
	}
	cached, hit := cache.Get(key)
	if !hit {
		t.Fatal("skill not cached under its exec-aware key")
	}
	assertFileModes(t, cached, want)

	// Second install is served from the cache and keeps the modes.
	before := fx.downloads.Load()
	dest2 := t.TempDir()
	entry, err := installOneSkill(ctx, skill, "exec-skill", dest2)
	if err != nil {
		t.Fatal(err)
	}
	if fx.downloads.Load() != before {
		t.Fatal("second install downloaded again; want a cache hit")
	}
	assertFileModes(t, filepath.Join(dest2, "exec-skill"), want)
	// The record keeps the Hub's bundle hash, not the cache key.
	if entry.ContentHash != fx.bundleHash {
		t.Fatalf("entry ContentHash %q, want the bundle hash %q", entry.ContentHash, fx.bundleHash)
	}
}

func TestInstallOneSkill_StaleCacheEntryNotReusedForExecSkill(t *testing.T) {
	fx := newExecSkillFixture(t, map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755"})
	skill := fx.resolved(t)
	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// An entry cached before modes were carried: same content, same
	// bundle hash, every file 0644.
	if _, err := cache.Put(fx.bundleHash, fx.files); err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithSkillCache(context.Background(), cache)

	dest := t.TempDir()
	if _, err := installOneSkill(ctx, skill, "exec-skill", dest); err != nil {
		t.Fatal(err)
	}
	if fx.downloads.Load() == 0 {
		t.Fatal("stale 0644 cache entry was served; want a fresh download")
	}
	assertFileModes(t, filepath.Join(dest, "exec-skill"),
		map[string]os.FileMode{"SKILL.md": 0o644, "scripts/run.sh": 0o755})
}

func TestInstallOneSkill_LegacyHubWithoutModesIs0644(t *testing.T) {
	// A Hub from before ptone/scion#5652 sends no modes.
	fx := newExecSkillFixture(t, map[string]string{})
	skill := fx.resolved(t)
	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithSkillCache(context.Background(), cache)

	dest := t.TempDir()
	if _, err := installOneSkill(ctx, skill, "exec-skill", dest); err != nil {
		t.Fatal(err)
	}
	assertFileModes(t, filepath.Join(dest, "exec-skill"),
		map[string]os.FileMode{"SKILL.md": 0o644, "scripts/run.sh": 0o644})
	// No executable file: the key stays the bare bundle hash, so entries
	// cached before this change remain valid.
	if key := skillCacheKey(skill); key != fx.bundleHash {
		t.Fatalf("cache key %q, want the bundle hash %q", key, fx.bundleHash)
	}
	if _, hit := cache.Get(fx.bundleHash); !hit {
		t.Fatal("skill without executables not cached under the bundle hash")
	}
}

func TestInstallOneSkill_NeverCarriesSpecialBits(t *testing.T) {
	fx := newExecSkillFixture(t, map[string]string{"SKILL.md": "1666", "scripts/run.sh": "6777"})
	skill := fx.resolved(t)
	dest := t.TempDir()
	if _, err := installOneSkill(context.Background(), skill, "exec-skill", dest); err != nil {
		t.Fatal(err)
	}
	assertFileModes(t, filepath.Join(dest, "exec-skill"),
		map[string]os.FileMode{"SKILL.md": 0o644, "scripts/run.sh": 0o755})
}

func TestInstallOneSkill_PrefetchedContentKeepsMode(t *testing.T) {
	// Resolvers that carry content (writeSkillFileContent path) get the
	// same mode handling as downloads.
	fx := newExecSkillFixture(t, map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755"})
	skill := fx.resolved(t)
	for i := range skill.Files {
		skill.Files[i].Content = fx.files[skill.Files[i].Path]
		skill.Files[i].URL = ""
	}
	dest := t.TempDir()
	if _, err := installOneSkill(context.Background(), skill, "exec-skill", dest); err != nil {
		t.Fatal(err)
	}
	if fx.downloads.Load() != 0 {
		t.Fatal("pre-fetched content was downloaded again")
	}
	assertFileModes(t, filepath.Join(dest, "exec-skill"),
		map[string]os.FileMode{"SKILL.md": 0o644, "scripts/run.sh": 0o755})
}
