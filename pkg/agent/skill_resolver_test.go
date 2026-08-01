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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// collectHandler is a slog.Handler that collects records for test assertions.
type collectHandler struct {
	mu      sync.Mutex
	records *[]slog.Record
}

func (h *collectHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *collectHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r)
	return nil
}
func (h *collectHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(_ string) slog.Handler      { return h }

// mockResolver implements SkillResolver for testing.
type mockResolver struct {
	resolved []ResolvedSkill
	errors   []ResolveError
	err      error
}

func (m *mockResolver) Resolve(_ context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &ResolveResult{
		Resolved: m.resolved,
		Errors:   m.errors,
	}, nil
}

func TestContextWithSkillResolver(t *testing.T) {
	ctx := context.Background()
	if got := SkillResolverFromContext(ctx); got != nil {
		t.Fatal("expected nil resolver from empty context")
	}

	resolver := &mockResolver{}
	ctx = ContextWithSkillResolver(ctx, resolver)
	if got := SkillResolverFromContext(ctx); got == nil {
		t.Fatal("expected non-nil resolver from context")
	}
}

func TestResolvedSkill_DestName(t *testing.T) {
	tests := []struct {
		name    string
		as      string
		want    string
		wantErr bool
	}{
		{"scion", "", "scion", false},
		{"scion", "my-scion", "my-scion", false},
		{"scion", "INVALID", "", true},
		{"scion", "-bad-", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name+"/"+tc.as, func(t *testing.T) {
			rs := &ResolvedSkill{Name: tc.name, As: tc.as}
			got, err := rs.DestName()
			if (err != nil) != tc.wantErr {
				t.Errorf("DestName() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("DestName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateFilePath(t *testing.T) {
	valid := []string{
		"SKILL.md",
		"scripts/analyze.sh",
		"a/b/c.txt",
		"file.txt",
	}
	for _, path := range valid {
		if err := validateFilePath(path); err != nil {
			t.Errorf("validateFilePath(%q) unexpected error: %v", path, err)
		}
	}

	invalid := []struct {
		path string
		desc string
	}{
		{"", "empty"},
		{"../etc/passwd", "path traversal"},
		{"foo/../../bar", "path traversal in middle"},
		{"/absolute/path", "absolute path"},
		{"foo\\bar", "backslash"},
		{string([]byte{'f', 'o', 'o', 0, 'b', 'a', 'r'}), "NUL byte"},
		{"CON", "reserved name CON"},
		{"PRN.txt", "reserved name PRN with extension"},
		{"NUL", "reserved name NUL"},
	}
	for _, tc := range invalid {
		t.Run(tc.desc, func(t *testing.T) {
			if err := validateFilePath(tc.path); err == nil {
				t.Errorf("validateFilePath(%q) expected error for %s", tc.path, tc.desc)
			}
		})
	}
}

func TestInstallResolvedSkills_Success(t *testing.T) {
	// Set up an httptest server to serve file content
	content := []byte("# My Skill\nThis is a test skill.")
	contentHash := transfer.HashBytes(content)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	// Compute bundle hash
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: contentHash},
	})

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	skills := []ResolvedSkill{
		{
			Name:    "test-skill",
			URI:     "skill://scion/core/test-skill@1.0",
			Version: "1.0.0",
			Hash:    bundleHash,
			Files: []ResolvedFile{
				{
					Path: "SKILL.md",
					URL:  srv.URL + "/SKILL.md",
					Hash: contentHash,
					Size: int64(len(content)),
				},
			},
		},
	}

	// Use the test server's client (with TLS config)
	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	record, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills() error: %v", err)
	}

	// Verify file was installed
	installed := filepath.Join(skillsDest, "test-skill", "SKILL.md")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("failed to read installed file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("installed content = %q, want %q", string(data), string(content))
	}

	// Verify record
	if len(record.Skills) != 1 {
		t.Fatalf("expected 1 skill in record, got %d", len(record.Skills))
	}
	if record.Skills[0].Name != "test-skill" {
		t.Errorf("record name = %q, want %q", record.Skills[0].Name, "test-skill")
	}
	if record.Skills[0].ContentHash != bundleHash {
		t.Errorf("record hash = %q, want %q", record.Skills[0].ContentHash, bundleHash)
	}
}

func TestInstallResolvedSkills_HashMismatch(t *testing.T) {
	content := []byte("actual content")
	wrongHash := "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	skills := []ResolvedSkill{
		{
			Name:    "bad-hash",
			URI:     "skill://scion/core/bad-hash@1.0",
			Version: "1.0.0",
			Hash:    "sha256:bundlehash",
			Files: []ResolvedFile{
				{
					Path: "SKILL.md",
					URL:  srv.URL + "/SKILL.md",
					Hash: wrongHash,
				},
			},
		},
	}

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err == nil {
		t.Fatal("expected error for hash mismatch")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("error should mention hash mismatch, got: %v", err)
	}

	// Verify staging directory was cleaned up (no .skill-staging- dirs remain)
	entries, _ := os.ReadDir(skillsDest)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingDirPrefix) {
			t.Errorf("staging directory %q was not cleaned up", e.Name())
		}
	}
}

func TestInstallResolvedSkills_PathTraversal(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("malicious content"))
	}))
	defer srv.Close()

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	skills := []ResolvedSkill{
		{
			Name:    "evil-skill",
			URI:     "skill://scion/core/evil-skill@1.0",
			Version: "1.0.0",
			Files: []ResolvedFile{
				{
					Path: "../../../etc/passwd",
					URL:  srv.URL + "/file",
					Hash: "sha256:doesntmatter",
				},
			},
		},
	}

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Errorf("error should mention traversal, got: %v", err)
	}
}

func TestInstallResolvedSkills_DuplicateDestination(t *testing.T) {
	// After the collision resolution change, duplicate destinations are resolved
	// via scope-based precedence instead of producing a hard error.
	content := []byte("# Winner Skill")
	contentHash := transfer.HashBytes(content)
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: contentHash},
	})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	skills := []ResolvedSkill{
		{
			Name:  "scion",
			URI:   "skill://scion/core/scion@^1.0",
			Scope: "template",
			Hash:  bundleHash,
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: contentHash},
			},
		},
		{
			Name:  "custom",
			URI:   "skill://project/custom@latest",
			As:    "scion", // same dest name
			Scope: "project",
			Hash:  bundleHash,
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: contentHash},
			},
		},
	}

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	record, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("expected success after collision resolution, got error: %v", err)
	}

	// Project scope wins over template scope.
	if len(record.Skills) != 1 {
		t.Fatalf("expected 1 installed skill, got %d", len(record.Skills))
	}
	if record.Skills[0].URI != "skill://project/custom@latest" {
		t.Errorf("expected project-scope skill to win, got URI %q", record.Skills[0].URI)
	}

	// Collision should be recorded.
	if len(record.Collisions) != 1 {
		t.Fatalf("expected 1 collision entry, got %d", len(record.Collisions))
	}
	c := record.Collisions[0]
	if c.DestName != "scion" {
		t.Errorf("collision destName = %q, want %q", c.DestName, "scion")
	}
	if c.WinnerURI != "skill://project/custom@latest" {
		t.Errorf("collision winner = %q, want project skill", c.WinnerURI)
	}
	if c.DroppedURI != "skill://scion/core/scion@^1.0" {
		t.Errorf("collision dropped = %q, want template skill", c.DroppedURI)
	}
}

func TestDownloadSkillFile_HTTPSOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("content"))
	}))
	defer srv.Close()

	// The httptest server uses HTTP, not HTTPS, and is not localhost from URL perspective
	// but the URL will be http://127.0.0.1:PORT which is localhost
	dest := filepath.Join(t.TempDir(), "test.txt")
	err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, defaultMaxFileSize, "")
	// 127.0.0.1 is localhost, so HTTP is allowed
	if err != nil {
		t.Errorf("expected HTTP to localhost to be allowed, got: %v", err)
	}

	// Non-localhost HTTP should fail
	err = downloadSkillFile(context.Background(), "http://example.com/file", dest, defaultMaxFileSize, "")
	if err == nil {
		t.Fatal("expected error for non-HTTPS non-localhost URL")
	}
	if !strings.Contains(err.Error(), "HTTPS required") {
		t.Errorf("error should mention HTTPS required, got: %v", err)
	}
}

func TestDownloadSkillFile_SizeLimit(t *testing.T) {
	// Serve content larger than the limit
	bigContent := strings.Repeat("x", 100)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(bigContent))
	}))
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	dest := filepath.Join(t.TempDir(), "test.txt")
	err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, 50, "") // 50 byte limit
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("error should mention size limit, got: %v", err)
	}
}

func TestDownloadSkillFile_CrossHostRedirect(t *testing.T) {
	// Set up two servers, first redirects to second
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("content"))
	}))
	defer other.Close()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/file", http.StatusFound)
	}))
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	dest := filepath.Join(t.TempDir(), "test.txt")
	err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, defaultMaxFileSize, "")
	if err == nil {
		t.Fatal("expected error for cross-host redirect")
	}
	if !strings.Contains(err.Error(), "cross-host redirect") {
		t.Errorf("error should mention cross-host redirect, got: %v", err)
	}
}

func TestMockResolver(t *testing.T) {
	resolver := &mockResolver{
		resolved: []ResolvedSkill{
			{Name: "test", URI: "skill://scion/core/test@1.0", Version: "1.0.0"},
		},
		errors: []ResolveError{
			{URI: "skill://scion/core/missing@1.0", Code: "not_found", Message: "skill not found"},
		},
	}

	ctx := context.Background()
	result, err := resolver.Resolve(ctx, nil, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if len(result.Resolved) != 1 {
		t.Errorf("expected 1 resolved, got %d", len(result.Resolved))
	}
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(result.Errors))
	}
}

func TestMockResolver_Error(t *testing.T) {
	resolver := &mockResolver{err: fmt.Errorf("connection refused")}

	_, err := resolver.Resolve(context.Background(), nil, ResolveOpts{})
	if err == nil {
		t.Fatal("expected error from resolver")
	}
}

func TestCollectRequiredSkillURIs(t *testing.T) {
	refs := []api.SkillReference{
		{URI: "skill://scion/core/scion@^1.0"},
		{URI: "skill://scion/core/optional@latest", Optional: true},
		{URI: "skill://scion/core/required@1.0"},
	}
	got := collectRequiredSkillURIs(refs)
	if len(got) != 2 {
		t.Fatalf("expected 2 required URIs, got %d", len(got))
	}
}

func TestFindRefByURI(t *testing.T) {
	refs := []api.SkillReference{
		{URI: "skill://scion/core/scion@^1.0"},
		{URI: "skill://scion/core/other@latest", Optional: true},
	}

	got := findRefByURI(refs, "skill://scion/core/other@latest")
	if got == nil {
		t.Fatal("expected to find ref")
	}
	if !got.Optional {
		t.Error("expected found ref to be optional")
	}

	got = findRefByURI(refs, "skill://scion/core/missing@1.0")
	if got != nil {
		t.Error("expected nil for missing URI")
	}
}

func TestWriteResolutionRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".scion", "resolved-skills.json")

	record := &SkillResolutionRecord{
		ResolvedAt: "2026-06-11T00:00:00Z",
		Resolver:   "mock",
		Skills: []SkillResolutionEntry{
			{
				URI:             "skill://scion/core/test@1.0",
				Name:            "test",
				ResolvedVersion: "1.0.0",
				ContentHash:     "sha256:abc123",
				Source:          "registry",
			},
		},
	}

	if err := writeResolutionRecord(path, record); err != nil {
		t.Fatalf("writeResolutionRecord() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read record: %v", err)
	}
	if !strings.Contains(string(data), "test") {
		t.Error("record should contain skill name")
	}
}

func TestInstallResolvedSkills_WithAsRename(t *testing.T) {
	content := []byte("# Renamed Skill")
	contentHash := transfer.HashBytes(content)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: contentHash},
	})

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	skills := []ResolvedSkill{
		{
			Name:    "original-name",
			URI:     "skill://scion/core/original-name@1.0",
			As:      "custom-name",
			Version: "1.0.0",
			Hash:    bundleHash,
			Files: []ResolvedFile{
				{
					Path: "SKILL.md",
					URL:  srv.URL + "/SKILL.md",
					Hash: contentHash,
				},
			},
		},
	}

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills() error: %v", err)
	}

	// Verify installed under the "As" name
	if _, err := os.Stat(filepath.Join(skillsDest, "custom-name", "SKILL.md")); err != nil {
		t.Errorf("expected file at custom-name/SKILL.md, got error: %v", err)
	}
	// Verify NOT installed under original name
	if _, err := os.Stat(filepath.Join(skillsDest, "original-name")); !os.IsNotExist(err) {
		t.Error("expected original-name dir to not exist")
	}
}

func TestInstallResolvedSkills_NestedFiles(t *testing.T) {
	content1 := []byte("# Skill")
	content2 := []byte("#!/bin/bash\necho hello")
	hash1 := transfer.HashBytes(content1)
	hash2 := transfer.HashBytes(content2)

	callCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if strings.Contains(r.URL.Path, "SKILL") {
			_, _ = w.Write(content1)
		} else {
			_, _ = w.Write(content2)
		}
	}))
	defer srv.Close()

	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: hash1},
		{Path: "scripts/run.sh", Hash: hash2},
	})

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	skills := []ResolvedSkill{
		{
			Name:    "nested-skill",
			URI:     "skill://scion/core/nested-skill@1.0",
			Version: "1.0.0",
			Hash:    bundleHash,
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: hash1},
				{Path: "scripts/run.sh", URL: srv.URL + "/scripts/run.sh", Hash: hash2},
			},
		},
	}

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills() error: %v", err)
	}

	// Verify nested file was created
	data, err := os.ReadFile(filepath.Join(skillsDest, "nested-skill", "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("failed to read nested file: %v", err)
	}
	if string(data) != string(content2) {
		t.Errorf("nested file content = %q, want %q", string(data), string(content2))
	}
}

func TestInstallResolvedSkills_BundleHashMismatch(t *testing.T) {
	content := []byte("# Skill Content")
	contentHash := transfer.HashBytes(content)
	wrongBundleHash := "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	skills := []ResolvedSkill{
		{
			Name:    "bundle-mismatch",
			URI:     "skill://scion/core/bundle-mismatch@1.0",
			Version: "1.0.0",
			Hash:    wrongBundleHash,
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: contentHash},
			},
		},
	}

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err == nil {
		t.Fatal("expected error for bundle hash mismatch")
	}
	if !strings.Contains(err.Error(), "bundle hash mismatch") {
		t.Errorf("error should mention bundle hash mismatch, got: %v", err)
	}
}

func TestEnumerateLocalSkills(t *testing.T) {
	agentHome := t.TempDir()
	skillsDir := ".claude/skills"
	skillsPath := filepath.Join(agentHome, skillsDir)

	// Create some local skill directories
	_ = os.MkdirAll(filepath.Join(skillsPath, "local-skill-1"), 0755)
	_ = os.MkdirAll(filepath.Join(skillsPath, "local-skill-2"), 0755)
	// Hidden dirs should be excluded
	_ = os.MkdirAll(filepath.Join(skillsPath, ".staging-temp"), 0755)
	// Files should be excluded
	_ = os.WriteFile(filepath.Join(skillsPath, "README.md"), []byte("test"), 0644)

	entries := enumerateLocalSkills(agentHome, skillsDir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 local skills, got %d", len(entries))
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
		if e.Source != "local" {
			t.Errorf("expected source 'local', got %q", e.Source)
		}
	}
	if !names["local-skill-1"] || !names["local-skill-2"] {
		t.Errorf("expected local-skill-1 and local-skill-2, got %v", names)
	}
}

func TestEnumerateLocalSkills_NonExistentDir(t *testing.T) {
	entries := enumerateLocalSkills(t.TempDir(), ".claude/skills")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for non-existent dir, got %d", len(entries))
	}
}

func TestInstallResolvedSkills_OverridesExistingLocalSkill(t *testing.T) {
	content := []byte("# Updated Skill")
	contentHash := transfer.HashBytes(content)
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: contentHash},
	})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	// Pre-create a local skill that will be overridden
	_ = os.MkdirAll(filepath.Join(skillsDest, "my-skill"), 0755)
	_ = os.WriteFile(filepath.Join(skillsDest, "my-skill", "SKILL.md"), []byte("# Old"), 0644)

	skills := []ResolvedSkill{
		{
			Name:    "my-skill",
			URI:     "skill://scion/core/my-skill@1.0",
			Version: "1.0.0",
			Hash:    bundleHash,
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: contentHash},
			},
		},
	}

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills() error: %v", err)
	}

	// Verify the new content replaced the old
	data, err := os.ReadFile(filepath.Join(skillsDest, "my-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("failed to read installed file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("expected updated content, got %q", string(data))
	}
}

func TestInstallResolvedSkills_EmptySkillsList(t *testing.T) {
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	record, err := installResolvedSkills(context.Background(), nil, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills(nil) error: %v", err)
	}
	if len(record.Skills) != 0 {
		t.Errorf("expected empty skills in record, got %d", len(record.Skills))
	}
}

// TestInstallOneSkill_GitBlobHashes covers the shape of a gh:// skill resolved
// by the Hub: the Hub never downloads file bytes, so it publishes git blob
// object IDs rather than sha256 digests, and the bundle hash is a digest over
// those. Both the per-file and the bundle check must succeed.
func TestInstallOneSkill_GitBlobHashes(t *testing.T) {
	const body = "hello\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	blobHash := transfer.GitBlobHashBytes([]byte(body))
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: blobHash},
	})

	skill := ResolvedSkill{
		Name:    "gh-skill",
		URI:     "gh://owner/repo/skills/gh-skill",
		Version: "abc123def456",
		Hash:    bundleHash,
		Files: []ResolvedFile{
			{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: blobHash},
		},
	}

	skillsDest := t.TempDir()
	entry, err := installOneSkill(context.Background(), skill, "gh-skill", skillsDest)
	if err != nil {
		t.Fatalf("installOneSkill: %v", err)
	}

	installed := filepath.Join(skillsDest, "gh-skill", "SKILL.md")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("reading installed file: %v", err)
	}
	if string(got) != body {
		t.Errorf("installed content = %q, want %q", got, body)
	}

	// The recorded hash stays in the format the resolver published, so the
	// bundle hash recomputed from it still matches what the Hub sent.
	if len(entry.Files) != 1 || entry.Files[0].Hash != blobHash {
		t.Errorf("expected recorded hash %s, got %+v", blobHash, entry.Files)
	}
}

// TestInstallOneSkill_GitBlobHashMismatch verifies that git-blob-format hashes
// are actually enforced, not merely tolerated.
func TestInstallOneSkill_GitBlobHashMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered content"))
	}))
	defer srv.Close()

	skill := ResolvedSkill{
		Name: "gh-skill",
		URI:  "gh://owner/repo/skills/gh-skill",
		Files: []ResolvedFile{
			// Git blob ID of "hello\n", which is not what the server serves.
			{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: "ce013625030ba8dba906f756967f9e9ca394464a"},
		},
	}

	_, err := installOneSkill(context.Background(), skill, "gh-skill", t.TempDir())
	if err == nil {
		t.Fatal("expected a hash mismatch error")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("error should mention hash mismatch, got: %v", err)
	}
}

// TestInstallOneSkill_EmptyHashSkipsVerification documents that a resolver may
// omit a per-file hash; the file installs and is recorded with a sha256 digest.
func TestInstallOneSkill_EmptyHashSkipsVerification(t *testing.T) {
	const body = "no hash published"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	skill := ResolvedSkill{
		Name:  "loose-skill",
		URI:   "gh://owner/repo/skills/loose-skill",
		Files: []ResolvedFile{{Path: "SKILL.md", URL: srv.URL + "/SKILL.md"}},
	}

	skillsDest := t.TempDir()
	if _, err := installOneSkill(context.Background(), skill, "loose-skill", skillsDest); err != nil {
		t.Fatalf("installOneSkill: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(skillsDest, "loose-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading installed file: %v", err)
	}
	if string(got) != body {
		t.Errorf("installed content = %q, want %q", got, body)
	}
}

func TestHashFileAs(t *testing.T) {
	const body = "hello\n"
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	gitID := transfer.GitBlobHashBytes([]byte(body))
	sha := transfer.HashBytes([]byte(body))

	// A git-blob expectation selects git-blob hashing.
	got, err := hashFileAs(path, gitID)
	if err != nil {
		t.Fatalf("hashFileAs: %v", err)
	}
	if got != gitID {
		t.Errorf("hashFileAs with git blob expectation = %s, want %s", got, gitID)
	}

	// A sha256 expectation, and an absent expectation, both select sha256.
	for _, expected := range []string{sha, ""} {
		got, err := hashFileAs(path, expected)
		if err != nil {
			t.Fatalf("hashFileAs(%q): %v", expected, err)
		}
		if got != sha {
			t.Errorf("hashFileAs(%q) = %s, want %s", expected, got, sha)
		}
	}
}

func TestIsGitHubHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"api.github.com", true},
		{"raw.githubusercontent.com", true},
		{"objects.githubusercontent.com", true},
		{"GitHub.com", true},
		{"storage.googleapis.com", false},
		{"example.com", false},
		{"", false},
		// Look-alikes must not match: the suffix check is on a dotted
		// boundary, so these are rejected.
		{"github.com.evil.test", false},
		{"notgithub.com", false},
		{"evilgithubusercontent.com", false},
	}

	for _, tc := range cases {
		if got := isGitHubHost(tc.host); got != tc.want {
			t.Errorf("isGitHubHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestDownloadSkillFile_TokenNotSentToNonGitHubHost guards the credential
// boundary: a Hub response naming an arbitrary host must not cause the
// broker's GitHub token to be handed to that host.
func TestDownloadSkillFile_TokenNotSentToNonGitHubHost(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("content"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "test.txt")
	if err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, defaultMaxFileSize, "secret-token"); err != nil {
		t.Fatalf("downloadSkillFile: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("token leaked to non-GitHub host: Authorization = %q", gotAuth)
	}
}

func TestGitHubTokenFromContext(t *testing.T) {
	if got := GitHubTokenFromContext(context.Background()); got != "" {
		t.Errorf("expected no token on a bare context, got %q", got)
	}
	ctx := ContextWithGitHubToken(context.Background(), "ghp_example")
	if got := GitHubTokenFromContext(ctx); got != "ghp_example" {
		t.Errorf("GitHubTokenFromContext = %q, want %q", got, "ghp_example")
	}
}

// --- deduplicateByDestName tests ---

func TestDeduplicateByDestName_ProjectWinsOverTemplate(t *testing.T) {
	skills := []ResolvedSkill{
		{Name: "my-skill", URI: "skill://scion/core/my-skill@1.0", Scope: "template"},
		{Name: "my-skill", URI: "gh://org/repo/skills/my-skill", Scope: "project"},
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	if len(deduped) != 1 {
		t.Fatalf("expected 1 skill after dedup, got %d", len(deduped))
	}
	if deduped[0].URI != "gh://org/repo/skills/my-skill" {
		t.Errorf("expected project-scope skill to win, got URI %q", deduped[0].URI)
	}

	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	c := collisions[0]
	if c.WinnerScope != "project" {
		t.Errorf("winner scope = %q, want %q", c.WinnerScope, "project")
	}
	if c.DroppedScope != "template" {
		t.Errorf("dropped scope = %q, want %q", c.DroppedScope, "template")
	}
}

func TestDeduplicateByDestName_SameScopeLaterWins(t *testing.T) {
	skills := []ResolvedSkill{
		{Name: "my-skill", URI: "skill://scion/core/my-skill@1.0", Scope: "template"},
		{Name: "my-skill", URI: "skill://scion/core/my-skill@2.0", Scope: "template"},
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	if len(deduped) != 1 {
		t.Fatalf("expected 1 skill after dedup, got %d", len(deduped))
	}
	// Later entry (index 1) should win on equal scope.
	if deduped[0].URI != "skill://scion/core/my-skill@2.0" {
		t.Errorf("expected later entry to win on same scope, got URI %q", deduped[0].URI)
	}

	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	if collisions[0].DroppedURI != "skill://scion/core/my-skill@1.0" {
		t.Errorf("expected earlier entry to be dropped, got %q", collisions[0].DroppedURI)
	}
}

func TestDeduplicateByDestName_NoCollision(t *testing.T) {
	skills := []ResolvedSkill{
		{Name: "skill-a", URI: "skill://scion/core/skill-a@1.0", Scope: "template"},
		{Name: "skill-b", URI: "skill://scion/core/skill-b@1.0", Scope: "project"},
		{Name: "skill-c", URI: "gh://org/repo/skills/skill-c", Scope: "hub"},
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	if len(deduped) != 3 {
		t.Fatalf("expected 3 skills (no collision), got %d", len(deduped))
	}
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions, got %d", len(collisions))
	}

	// Verify order is preserved.
	if deduped[0].Name != "skill-a" || deduped[1].Name != "skill-b" || deduped[2].Name != "skill-c" {
		t.Errorf("order not preserved: got %q, %q, %q", deduped[0].Name, deduped[1].Name, deduped[2].Name)
	}
}

func TestDeduplicateByDestName_CollisionRecordedInRecord(t *testing.T) {
	content := []byte("# Skill")
	contentHash := transfer.HashBytes(content)
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: contentHash},
	})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	skills := []ResolvedSkill{
		{
			Name: "shared-name", URI: "skill://hub/shared-name@1.0", Scope: "hub",
			Hash: bundleHash, Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: contentHash},
			},
		},
		{
			Name: "shared-name", URI: "skill://project/shared-name@2.0", Scope: "project",
			Hash: bundleHash, Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: contentHash},
			},
		},
	}

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	record, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("installResolvedSkills() error: %v", err)
	}

	if len(record.Collisions) != 1 {
		t.Fatalf("expected 1 collision in record, got %d", len(record.Collisions))
	}
	c := record.Collisions[0]
	if c.DestName != "shared-name" {
		t.Errorf("collision destName = %q, want %q", c.DestName, "shared-name")
	}
	if c.WinnerURI != "skill://project/shared-name@2.0" {
		t.Errorf("collision winner = %q, want project skill", c.WinnerURI)
	}
	if c.DroppedURI != "skill://hub/shared-name@1.0" {
		t.Errorf("collision dropped = %q, want hub skill", c.DroppedURI)
	}
}

func TestDeduplicateByDestName_OptionalLoserUsesDebugLevel(t *testing.T) {
	// When the dropped skill is optional, the collision log should be at Debug
	// level, not Warn. Capture slog output to verify.
	var records []slog.Record
	handler := &collectHandler{records: &records}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	skills := []ResolvedSkill{
		{Name: "my-skill", URI: "skill://scion/core/my-skill@1.0", Scope: "hub", Optional: true},
		{Name: "my-skill", URI: "skill://scion/core/my-skill@2.0", Scope: "project"},
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	if len(deduped) != 1 {
		t.Fatalf("expected 1 skill after dedup, got %d", len(deduped))
	}
	if deduped[0].URI != "skill://scion/core/my-skill@2.0" {
		t.Errorf("expected project-scope skill to win, got URI %q", deduped[0].URI)
	}
	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	if collisions[0].DroppedURI != "skill://scion/core/my-skill@1.0" {
		t.Errorf("expected optional skill to be dropped, got %q", collisions[0].DroppedURI)
	}

	// Verify that the collision was logged at Debug level, not Warn.
	var foundCollisionLog bool
	for _, r := range records {
		if strings.Contains(r.Message, "collision resolved") {
			foundCollisionLog = true
			if r.Level != slog.LevelDebug {
				t.Errorf("expected Debug level for optional loser collision log, got %v", r.Level)
			}
		}
	}
	if !foundCollisionLog {
		t.Error("expected a collision log record, found none")
	}
}

func TestDeduplicateByDestName_FullPrecedenceOrder(t *testing.T) {
	// Verify the full precedence chain: project > template > user > hub > platform > ""
	scopes := []string{"", "platform", "hub", "user", "template", "project"}

	for i := 0; i < len(scopes)-1; i++ {
		lower := scopes[i]
		higher := scopes[i+1]
		t.Run(fmt.Sprintf("%s_beats_%s", higher, lower), func(t *testing.T) {
			skills := []ResolvedSkill{
				{Name: "test-skill", URI: "skill://lower/test-skill@1.0", Scope: lower},
				{Name: "test-skill", URI: "skill://higher/test-skill@2.0", Scope: higher},
			}

			deduped, collisions := deduplicateByDestName(context.Background(), skills)

			if len(deduped) != 1 {
				t.Fatalf("expected 1 skill, got %d", len(deduped))
			}
			if deduped[0].Scope != higher {
				t.Errorf("expected scope %q to win, got %q", higher, deduped[0].Scope)
			}
			if len(collisions) != 1 {
				t.Fatalf("expected 1 collision, got %d", len(collisions))
			}
			if collisions[0].WinnerScope != higher {
				t.Errorf("winner scope = %q, want %q", collisions[0].WinnerScope, higher)
			}
		})
	}
}

func TestDeduplicateByDestName_DestNameErrorPassthrough(t *testing.T) {
	// A skill with an invalid DestName (e.g. "INVALID" fails ValidateSkillName)
	// must not be silently dropped. It should pass through the dedup and surface
	// as an error during install.
	skills := []ResolvedSkill{
		{Name: "good-skill", URI: "skill://scion/core/good-skill@1.0", Scope: "template"},
		{Name: "INVALID", URI: "skill://scion/core/bad@1.0", Scope: "project"}, // invalid name
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	// Both should be in the output: good-skill via the winners map, bad via passthrough.
	if len(deduped) != 2 {
		t.Fatalf("expected 2 skills (including passthrough), got %d", len(deduped))
	}
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions, got %d", len(collisions))
	}

	// Verify the invalid skill is in the output so installResolvedSkills can surface the error.
	foundInvalid := false
	for _, s := range deduped {
		if s.Name == "INVALID" {
			foundInvalid = true
		}
	}
	if !foundInvalid {
		t.Error("expected invalid-name skill to pass through dedup, but it was dropped")
	}

	// Verify installResolvedSkills surfaces the error for the invalid skill.
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")
	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err == nil {
		t.Fatal("expected error for skill with invalid DestName")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error should mention invalid destination name, got: %v", err)
	}
}

func TestDeduplicateByDestName_ThreeWayCollision(t *testing.T) {
	// Three skills collide on the same DestName. The highest scope should win,
	// and two collision entries should be recorded.
	skills := []ResolvedSkill{
		{Name: "shared", URI: "skill://hub/shared@1.0", Scope: "hub"},
		{Name: "shared", URI: "skill://template/shared@2.0", Scope: "template"},
		{Name: "shared", URI: "skill://project/shared@3.0", Scope: "project"},
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	if len(deduped) != 1 {
		t.Fatalf("expected 1 skill after three-way dedup, got %d", len(deduped))
	}
	if deduped[0].URI != "skill://project/shared@3.0" {
		t.Errorf("expected project-scope skill to win three-way collision, got URI %q", deduped[0].URI)
	}
	if deduped[0].Scope != "project" {
		t.Errorf("expected scope %q, got %q", "project", deduped[0].Scope)
	}

	// Two collisions should be recorded (hub→template, then template→project).
	if len(collisions) != 2 {
		t.Fatalf("expected 2 collision entries for three-way, got %d", len(collisions))
	}
	// First collision: template beats hub.
	if collisions[0].WinnerURI != "skill://template/shared@2.0" || collisions[0].DroppedURI != "skill://hub/shared@1.0" {
		t.Errorf("first collision: winner=%q dropped=%q, want template over hub",
			collisions[0].WinnerURI, collisions[0].DroppedURI)
	}
	// Second collision: project beats template.
	if collisions[1].WinnerURI != "skill://project/shared@3.0" || collisions[1].DroppedURI != "skill://template/shared@2.0" {
		t.Errorf("second collision: winner=%q dropped=%q, want project over template",
			collisions[1].WinnerURI, collisions[1].DroppedURI)
	}
}

func TestDeduplicateByDestName_AsAlias(t *testing.T) {
	// Skills with explicit As aliases that don't collide should pass through.
	skills := []ResolvedSkill{
		{Name: "my-skill", URI: "skill://scion/core/my-skill@1.0", Scope: "template"},
		{Name: "my-skill", URI: "gh://org/repo/skills/my-skill", Scope: "project", As: "my-skill-alt"},
	}

	deduped, collisions := deduplicateByDestName(context.Background(), skills)

	if len(deduped) != 2 {
		t.Fatalf("expected 2 skills (alias avoids collision), got %d", len(deduped))
	}
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions with alias, got %d", len(collisions))
	}
}
