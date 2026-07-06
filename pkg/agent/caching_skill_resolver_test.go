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

package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestCachingSkillResolver_DelegatesToInner(t *testing.T) {
	cache, _ := templatecache.New(t.TempDir(), 0)
	inner := &mockResolver{
		resolved: []ResolvedSkill{
			{Name: "test-skill", Version: "1.0.0", Hash: "sha256:abc"},
		},
	}

	csr := NewCachingSkillResolver(inner, cache)
	result, err := csr.Resolve(context.Background(), []api.SkillReference{{URI: "test"}}, ResolveOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resolved) != 1 || result.Resolved[0].Name != "test-skill" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCachingSkillResolver_InjectsCache(t *testing.T) {
	cache, _ := templatecache.New(t.TempDir(), 0)

	var capturedCtx context.Context
	inner := &ctxCapturingResolver{
		inner:   &mockResolver{resolved: []ResolvedSkill{{Name: "s", Hash: "h"}}},
		capture: func(ctx context.Context) { capturedCtx = ctx },
	}

	csr := NewCachingSkillResolver(inner, cache)
	_, err := csr.Resolve(context.Background(), nil, ResolveOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if SkillCacheFromContext(capturedCtx) == nil {
		t.Fatal("expected cache in context passed to inner resolver")
	}
}

func TestCachingSkillResolver_ResolverName(t *testing.T) {
	cache, _ := templatecache.New(t.TempDir(), 0)

	inner := &mockResolver{}
	csr := NewCachingSkillResolver(inner, cache)
	if got := csr.ResolverName(); got != "unknown" {
		t.Fatalf("expected 'unknown', got %q", got)
	}

	namedInner := &namedMockResolver{name: "hub"}
	csr2 := NewCachingSkillResolver(namedInner, cache)
	if got := csr2.ResolverName(); got != "hub" {
		t.Fatalf("expected 'hub', got %q", got)
	}
}

func TestCachingSkillResolver_PropagatesErrors(t *testing.T) {
	cache, _ := templatecache.New(t.TempDir(), 0)
	inner := &mockResolver{err: fmt.Errorf("connection refused")}

	csr := NewCachingSkillResolver(inner, cache)
	_, err := csr.Resolve(context.Background(), nil, ResolveOpts{})
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("expected inner error, got %v", err)
	}
}

func TestSkillCacheContext(t *testing.T) {
	ctx := context.Background()
	if got := SkillCacheFromContext(ctx); got != nil {
		t.Fatal("expected nil cache from empty context")
	}

	cache, _ := templatecache.New(t.TempDir(), 0)
	ctx = ContextWithSkillCache(ctx, cache)
	if got := SkillCacheFromContext(ctx); got == nil {
		t.Fatal("expected non-nil cache from context")
	}
}

func TestInstallOneSkill_CacheHit(t *testing.T) {
	cacheDir := t.TempDir()
	cache, _ := templatecache.New(cacheDir, 0)

	content := []byte("# My Skill\nversion: 1.0.0\n")
	fileHash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: fileHash},
	})

	// Pre-populate cache
	_, _ = cache.Put(bundleHash, map[string][]byte{"SKILL.md": content})

	ctx := ContextWithSkillCache(context.Background(), cache)

	skillsDest := t.TempDir()
	skill := ResolvedSkill{
		Name:    "cached-skill",
		URI:     "skill://scion/core/cached-skill@1.0.0",
		Version: "1.0.0",
		Hash:    bundleHash,
		Files: []ResolvedFile{
			{Path: "SKILL.md", Hash: fileHash},
		},
	}

	entry, err := installOneSkill(ctx, skill, "cached-skill", skillsDest)
	if err != nil {
		t.Fatal(err)
	}

	// Verify file was installed from cache
	installed := filepath.Join(skillsDest, "cached-skill", "SKILL.md")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("failed to read installed file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q", got)
	}

	if entry.Name != "cached-skill" {
		t.Fatalf("unexpected entry name: %s", entry.Name)
	}
	if entry.Source != "registry" {
		t.Fatalf("unexpected source: %s", entry.Source)
	}
}

func TestInstallOneSkill_CacheMissPopulatesCache(t *testing.T) {
	cacheDir := t.TempDir()
	cache, _ := templatecache.New(cacheDir, 0)

	content := []byte("# Skill Content\n")
	fileHash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: fileHash},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	ctx := ContextWithSkillCache(context.Background(), cache)

	skillsDest := t.TempDir()
	skill := ResolvedSkill{
		Name:    "new-skill",
		URI:     "skill://scion/core/new-skill@1.0.0",
		Version: "1.0.0",
		Hash:    bundleHash,
		Files: []ResolvedFile{
			{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: fileHash, Size: int64(len(content))},
		},
	}

	_, err := installOneSkill(ctx, skill, "new-skill", skillsDest)
	if err != nil {
		t.Fatal(err)
	}

	// Verify cache was populated
	cachedPath, hit := cache.Get(bundleHash)
	if !hit {
		t.Fatal("expected cache to be populated after download")
	}

	cachedContent, err := os.ReadFile(filepath.Join(cachedPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("failed to read cached file: %v", err)
	}
	if string(cachedContent) != string(content) {
		t.Fatalf("cached content mismatch: got %q", cachedContent)
	}
}

func TestInstallOneSkill_NoCacheStillWorks(t *testing.T) {
	content := []byte("# No Cache Skill\n")
	fileHash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: fileHash},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	ctx := context.Background() // no cache in context

	skillsDest := t.TempDir()
	skill := ResolvedSkill{
		Name:    "no-cache-skill",
		URI:     "skill://scion/core/no-cache-skill@1.0.0",
		Version: "1.0.0",
		Hash:    bundleHash,
		Files: []ResolvedFile{
			{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: fileHash, Size: int64(len(content))},
		},
	}

	entry, err := installOneSkill(ctx, skill, "no-cache-skill", skillsDest)
	if err != nil {
		t.Fatal(err)
	}

	installed := filepath.Join(skillsDest, "no-cache-skill", "SKILL.md")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("failed to read installed file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q", got)
	}
	if entry.ContentHash != bundleHash {
		t.Fatalf("unexpected content hash: %s", entry.ContentHash)
	}
}

func TestInstallOneSkill_SecondInstallUsesCacheFromFirstInstall(t *testing.T) {
	cacheDir := t.TempDir()
	cache, _ := templatecache.New(cacheDir, 0)

	content := []byte("# Repeated Skill\n")
	fileHash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: fileHash},
	})

	downloadCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCount++
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	ctx := ContextWithSkillCache(context.Background(), cache)

	skill := ResolvedSkill{
		Name:    "repeat-skill",
		URI:     "skill://scion/core/repeat-skill@1.0.0",
		Version: "1.0.0",
		Hash:    bundleHash,
		Files: []ResolvedFile{
			{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: fileHash, Size: int64(len(content))},
		},
	}

	// First install: downloads from server
	skillsDest1 := t.TempDir()
	_, err := installOneSkill(ctx, skill, "repeat-skill", skillsDest1)
	if err != nil {
		t.Fatal(err)
	}
	if downloadCount != 1 {
		t.Fatalf("expected 1 download, got %d", downloadCount)
	}

	// Second install: should use cache (no additional downloads)
	skillsDest2 := t.TempDir()
	_, err = installOneSkill(ctx, skill, "repeat-skill", skillsDest2)
	if err != nil {
		t.Fatal(err)
	}
	if downloadCount != 1 {
		t.Fatalf("expected still 1 download after cache hit, got %d", downloadCount)
	}

	// Verify second install has correct content
	got, err := os.ReadFile(filepath.Join(skillsDest2, "repeat-skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch on second install: got %q", got)
	}
}

func TestTruncHash(t *testing.T) {
	if got := truncHash("sha256:abc123def456ghi789"); got != "sha256:abc123def" {
		t.Fatalf("unexpected truncation: %q", got)
	}
	if got := truncHash("short"); got != "short" {
		t.Fatalf("unexpected truncation for short input: %q", got)
	}
}

// --- test helpers ---

type ctxCapturingResolver struct {
	inner   SkillResolver
	capture func(context.Context)
}

func (r *ctxCapturingResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	r.capture(ctx)
	return r.inner.Resolve(ctx, refs, opts)
}

type namedMockResolver struct {
	mockResolver
	name string
}

func (r *namedMockResolver) ResolverName() string { return r.name }
