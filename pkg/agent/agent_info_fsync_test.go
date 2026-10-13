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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubAgentInfoSyncs swaps the writeAgentInfoFile sync seams for the
// duration of the test.
func stubAgentInfoSyncs(t *testing.T, fileSync func(*os.File) error, dirSync func(string) error) {
	t.Helper()
	origFile, origDir := agentInfoFileSync, agentInfoDirSync
	t.Cleanup(func() {
		agentInfoFileSync, agentInfoDirSync = origFile, origDir
	})
	agentInfoFileSync, agentInfoDirSync = fileSync, dirSync
}

// TestWriteAgentInfoFileSyncsContentsBeforeRename checks that the temp
// file's contents are synced, once, before it is renamed over the final
// path (#4821).
func TestWriteAgentInfoFileSyncsContentsBeforeRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-info.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"new":true}`)

	var calls int
	var syncedName string
	var finalAtSync []byte
	stubAgentInfoSyncs(t, func(f *os.File) error {
		calls++
		syncedName = f.Name()
		finalAtSync, _ = os.ReadFile(path)
		return f.Sync()
	}, func(string) error { return nil })

	if err := writeAgentInfoFile(path, data, 0o644); err != nil {
		t.Fatalf("writeAgentInfoFile: %v", err)
	}
	if calls != 1 {
		t.Fatalf("file sync calls = %d, want 1", calls)
	}
	if filepath.Dir(syncedName) != dir || !strings.HasPrefix(filepath.Base(syncedName), ".agent-info.json.tmp-") {
		t.Errorf("synced %q, want the temp file in %q", syncedName, dir)
	}
	if string(finalAtSync) != `{"old":true}` {
		t.Errorf("final path at sync time = %q, want the old contents (sync must precede the rename)", finalAtSync)
	}
	if got, _ := os.ReadFile(path); string(got) != string(data) {
		t.Errorf("final contents = %q, want %q", got, data)
	}
}

// TestWriteAgentInfoFileSyncsParentDirAfterRename checks that the parent
// directory is synced, once, after the rename has landed (#4821).
func TestWriteAgentInfoFileSyncsParentDirAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, provisionedWorktreeStateFile)

	var calls int
	var syncedDir string
	var finalAtSync []byte
	stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(d string) error {
		calls++
		syncedDir = d
		finalAtSync, _ = os.ReadFile(path)
		return nil
	})

	if err := writeProvisionedWorktreeRepoRoot(dir, "/repo"); err != nil {
		t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
	}
	if calls != 1 {
		t.Fatalf("dir sync calls = %d, want 1", calls)
	}
	if syncedDir != dir {
		t.Errorf("synced dir %q, want %q", syncedDir, dir)
	}
	if string(finalAtSync) != `{"repoRoot":"/repo"}` {
		t.Errorf("final path at dir sync = %q, want the new contents (dir sync must follow the rename)", finalAtSync)
	}
}

// TestWriteAgentInfoFileReturnsFileSyncError checks that a content sync
// failure is returned, the final file is left untouched, and the temp file
// is removed.
func TestWriteAgentInfoFileReturnsFileSyncError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-info.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	errSync := errors.New("injected file sync failure")
	stubAgentInfoSyncs(t, func(*os.File) error { return errSync }, func(string) error { return nil })

	err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644)
	if !errors.Is(err, errSync) {
		t.Fatalf("writeAgentInfoFile error = %v, want %v", err, errSync)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"old":true}` {
		t.Errorf("final contents = %q, want the old contents", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the final file (temp file left behind)", len(entries))
	}
}

// TestWriteAgentInfoFileReturnsDirSyncError checks that a parent directory
// sync failure is returned.
func TestWriteAgentInfoFileReturnsDirSyncError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-info.json")
	errSync := errors.New("injected dir sync failure")
	stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(string) error { return errSync })

	err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644)
	if !errors.Is(err, errSync) {
		t.Fatalf("writeAgentInfoFile error = %v, want %v", err, errSync)
	}
}
