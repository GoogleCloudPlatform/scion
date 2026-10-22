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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestResolveAgentID_AgentFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/project-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"slug": "agent-id-1", "name": "my-agent", "status": "running"},
					{"slug": "agent-id-2", "name": "other-agent", "status": "stopped"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	agentID, err := resolveAgentID(context.Background(), client, "project-1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agentID != "agent-id-1" {
		t.Errorf("agent ID = %q, want %q", agentID, "agent-id-1")
	}
}

func TestResolveAgentID_AgentNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/project-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"slug": "agent-id-1", "name": "other-agent", "status": "running"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = resolveAgentID(context.Background(), client, "project-1", "nonexistent-agent")
	if err == nil {
		t.Fatal("expected error for non-existent agent")
	}
	if err.Error() != "agent 'nonexistent-agent' not found in project" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestResolveAgentID_AgentNotRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/project-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"slug": "agent-id-1", "name": "my-agent", "status": "stopped"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = resolveAgentID(context.Background(), client, "project-1", "my-agent")
	if err == nil {
		t.Fatal("expected error for stopped agent")
	}
	if err.Error() != "agent 'my-agent' is not running (phase: stopped)" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestResolveLocalWorkspacePath_WorktreeExists(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "sync-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create project directory structure
	projectName := "my-project"
	projectDir := filepath.Join(tmpDir, projectName)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	// Create worktree directory
	worktreeDir := filepath.Join(tmpDir, ".scion_worktrees", projectName, "my-agent")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	// Set projectPath to the project directory
	oldProjectPath := projectPath
	projectPath = projectDir
	defer func() { projectPath = oldProjectPath }()

	workspacePath, err := resolveLocalWorkspacePath("my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if workspacePath != worktreeDir {
		t.Errorf("workspace path = %q, want %q", workspacePath, worktreeDir)
	}
}

func TestResolveLocalWorkspacePath_FallbackToCurrent(t *testing.T) {
	// Create a temporary directory without a worktree
	tmpDir, err := os.MkdirTemp("", "sync-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Set projectPath to the temp directory (no worktrees exist)
	oldProjectPath := projectPath
	projectPath = tmpDir
	defer func() { projectPath = oldProjectPath }()

	workspacePath, err := resolveLocalWorkspacePath("my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fall back to current directory
	if workspacePath != "." {
		t.Errorf("workspace path = %q, want %q", workspacePath, ".")
	}
}

func TestResolveLocalWorkspacePath_EmptyProjectPath(t *testing.T) {
	// Clear project path
	oldProjectPath := projectPath
	projectPath = ""
	defer func() { projectPath = oldProjectPath }()

	workspacePath, err := resolveLocalWorkspacePath("my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fall back to current directory
	if workspacePath != "." {
		t.Errorf("workspace path = %q, want %q", workspacePath, ".")
	}
}

func TestSyncFromResponse_ResponseParsing(t *testing.T) {
	// Test that SyncFromResponse types are correctly used
	resp := hubclient.SyncFromResponse{
		Manifest: &transfer.Manifest{
			Version:     "1.0",
			ContentHash: "sha256:abc123",
			Files: []transfer.FileInfo{
				{Path: "main.go", Size: 1024, Hash: "sha256:def456"},
			},
		},
		DownloadURLs: []transfer.DownloadURLInfo{
			{Path: "main.go", URL: "https://example.com/file", Size: 1024, Hash: "sha256:def456"},
		},
		Expires: time.Now().Add(15 * time.Minute),
	}

	if resp.Manifest == nil {
		t.Fatal("manifest should not be nil")
	}
	if len(resp.Manifest.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(resp.Manifest.Files))
	}
	if len(resp.DownloadURLs) != 1 {
		t.Errorf("expected 1 download URL, got %d", len(resp.DownloadURLs))
	}
}

func TestSyncToResponse_ResponseParsing(t *testing.T) {
	// Test that SyncToResponse types are correctly used
	resp := hubclient.SyncToResponse{
		UploadURLs: []transfer.UploadURLInfo{
			{Path: "main.go", URL: "https://example.com/upload", Method: "PUT"},
		},
		ExistingFiles: []string{"README.md"},
		Expires:       time.Now().Add(15 * time.Minute),
	}

	if len(resp.UploadURLs) != 1 {
		t.Errorf("expected 1 upload URL, got %d", len(resp.UploadURLs))
	}
	if len(resp.ExistingFiles) != 1 {
		t.Errorf("expected 1 existing file, got %d", len(resp.ExistingFiles))
	}
	if resp.ExistingFiles[0] != "README.md" {
		t.Errorf("existing file = %q, want %q", resp.ExistingFiles[0], "README.md")
	}
}

func TestSyncToFinalizeResponse_ResponseParsing(t *testing.T) {
	resp := hubclient.SyncToFinalizeResponse{
		Applied:          true,
		ContentHash:      "sha256:final123",
		FilesApplied:     10,
		BytesTransferred: 102400,
	}

	if !resp.Applied {
		t.Error("expected applied=true")
	}
	if resp.FilesApplied != 10 {
		t.Errorf("files applied = %d, want 10", resp.FilesApplied)
	}
	if resp.BytesTransferred != 102400 {
		t.Errorf("bytes transferred = %d, want 102400", resp.BytesTransferred)
	}
}

// TestSyncToViaHub_AppliesExcludeFlag checks that `scion sync to --exclude`
// patterns reach the workspace collect and are applied together with the
// default excludes: the SyncTo request lists neither the user-excluded files
// nor .git or the workspace-root .scion entry.
func TestSyncToViaHub_AppliesExcludeFlag(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceTree(t, ws,
		"main.go",
		"docs/readme.md",
		"logs/app.log",
		"build/out.bin",
		".git/config",
		".scion/settings.yaml",
	)

	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agents/agent-1/workspace/sync-to" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var req struct {
			Files []transfer.FileInfo `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotPaths = collectedPaths(req.Files)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.SyncToResponse{})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	savedExclude, savedDryRun := syncExclude, syncDryRun
	t.Cleanup(func() { syncExclude, syncDryRun = savedExclude, savedDryRun })
	syncExclude = []string{"*.log", "build/**"}
	syncDryRun = true // stop after the SyncTo request

	if err := syncToViaHub(&HubContext{Client: client, Endpoint: server.URL}, "agent-1", "agent-1", ws); err != nil {
		t.Fatalf("syncToViaHub: %v", err)
	}
	want := []string{"docs/readme.md", "main.go"}
	if !slices.Equal(gotPaths, want) {
		t.Errorf("SyncTo request files = %v, want %v", gotPaths, want)
	}
}

// Project-level sync (bare, push and pull) was removed with the hub's WebDAV
// endpoint. Each form must fail before touching a hub, as a usage error that
// points at agent sync.
func TestRunSync_ProjectModeRemoved(t *testing.T) {
	for _, args := range [][]string{nil, {"push"}, {"pull"}, {"push", "my-agent"}, {"pull", "my-agent"}} {
		err := runSync(syncCmd, args)
		if err == nil {
			t.Fatalf("scion sync %v: expected an error", args)
		}
		if !isUsageError(err) {
			t.Errorf("scion sync %v: want a usage error, got %T: %v", args, err, err)
		}
		for _, want := range []string{"project-level sync", "has been removed", "scion sync to <agent>", "scion sync from <agent>"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("scion sync %v: error %q does not mention %q", args, err.Error(), want)
			}
		}
	}

	if syncCmd.Flags().Lookup("force") != nil {
		t.Error("--force only served project bisync and should be gone")
	}
}

// TestSyncFromViaHub_NeverWritesRepositoryOrProjectConfig drives sync-from
// against a hub whose manifest (as an older broker or a tampered bucket
// could produce) lists repository metadata and project configuration next
// to ordinary files. Only the ordinary files may land in the local
// checkout; its own .git and .scion stay as they were.
func TestSyncFromViaHub_NeverWritesRepositoryOrProjectConfig(t *testing.T) {
	ws := t.TempDir()
	writeTree := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeTree(".git/config", "mine\n", 0o644)
	writeTree(".scion/settings.yaml", "mine\n", 0o644)

	remote := []string{
		"main.go",
		".git/hooks/pre-commit",
		".git/config",
		".git/refs/heads/main",
		"sub/.git/config",
		".scion/settings.yaml",
		"proj/.scion/settings.yaml",
	}
	var server *httptest.Server
	var downloaded []string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agent-1/workspace/sync-from":
			resp := hubclient.SyncFromResponse{Manifest: &transfer.Manifest{}}
			for _, p := range remote {
				resp.Manifest.Files = append(resp.Manifest.Files, transfer.FileInfo{Path: p, Size: 5, Hash: "h-" + p})
				resp.DownloadURLs = append(resp.DownloadURLs, transfer.DownloadURLInfo{Path: p, URL: server.URL + "/dl/" + p, Size: 5, Hash: "h-" + p})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/dl/"):
			downloaded = append(downloaded, strings.TrimPrefix(r.URL.Path, "/dl/"))
			_, _ = w.Write([]byte("agent"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	savedExclude, savedDryRun := syncExclude, syncDryRun
	t.Cleanup(func() { syncExclude, syncDryRun = savedExclude, savedDryRun })
	syncExclude, syncDryRun = nil, false

	if err := syncFromViaHub(&HubContext{Client: client, Endpoint: server.URL}, "agent-1", "agent-1", ws); err != nil {
		t.Fatalf("syncFromViaHub: %v", err)
	}
	if !slices.Equal(downloaded, []string{"main.go"}) {
		t.Errorf("downloaded %v, want only main.go", downloaded)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "main.go")); string(b) != "agent" {
		t.Errorf("main.go = %q", b)
	}
	for rel, want := range map[string]string{".git/config": "mine\n", ".scion/settings.yaml": "mine\n"} {
		if b, _ := os.ReadFile(filepath.Join(ws, rel)); string(b) != want {
			t.Errorf("%s = %q, want %q", rel, b, want)
		}
	}
	for _, rel := range []string{".git/hooks/pre-commit", ".git/refs/heads/main", "sub/.git/config", "proj/.scion/settings.yaml"} {
		if _, err := os.Lstat(filepath.Join(ws, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was written (err=%v)", rel, err)
		}
	}
}

func TestSplitSyncFromDownloads(t *testing.T) {
	urls := []transfer.DownloadURLInfo{{Path: "a.go"}, {Path: ".git/config"}, {Path: "x/.scion/s"}, {Path: "b/c.go"}, {Path: ".git/refs/heads/main"}}
	keep, dropped := splitSyncFromDownloads(urls)
	var got []string
	for _, u := range keep {
		got = append(got, u.Path)
	}
	if !slices.Equal(got, []string{"a.go", "b/c.go"}) || dropped != 3 {
		t.Errorf("keep=%v dropped=%d, want [a.go b/c.go] and 3", got, dropped)
	}
}

func TestOverwrittenExecutables(t *testing.T) {
	local := []transfer.FileInfo{{Path: "run.sh", Mode: "0755"}, {Path: "a.go", Mode: "0644"}, {Path: "bin/tool", Mode: "0700"}, {Path: "odd", Mode: "x"}}
	toDownload := []transfer.DownloadURLInfo{{Path: "a.go"}, {Path: "run.sh"}, {Path: "new.sh"}, {Path: "bin/tool"}, {Path: "odd"}}
	if got := overwrittenExecutables(toDownload, local); !slices.Equal(got, []string{"run.sh", "bin/tool"}) {
		t.Errorf("overwrittenExecutables = %v", got)
	}
}
