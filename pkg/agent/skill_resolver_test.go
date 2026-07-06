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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

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
	skills := []ResolvedSkill{
		{
			Name: "scion",
			URI:  "skill://scion/core/scion@^1.0",
		},
		{
			Name: "custom",
			URI:  "skill://project/custom@latest",
			As:   "scion", // same dest name
		},
	}

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	_, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	if err == nil {
		t.Fatal("expected error for duplicate destination")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("error should mention conflict, got: %v", err)
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
	err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, defaultMaxFileSize)
	// 127.0.0.1 is localhost, so HTTP is allowed
	if err != nil {
		t.Errorf("expected HTTP to localhost to be allowed, got: %v", err)
	}

	// Non-localhost HTTP should fail
	err = downloadSkillFile(context.Background(), "http://example.com/file", dest, defaultMaxFileSize)
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
	err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, 50) // 50 byte limit
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
	err := downloadSkillFile(context.Background(), srv.URL+"/file", dest, defaultMaxFileSize)
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
