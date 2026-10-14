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
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
)

// stubAgentInfoSyncs swaps the writeAgentInfoFile sync seams for the
// duration of the test.
func stubAgentInfoSyncs(t *testing.T, fileSync func(*os.File) error, dirSync func(*os.File) error) {
	t.Helper()
	origFile, origDir, origOpen := agentInfoFileSync, agentInfoDirSync, agentInfoDirOpen
	t.Cleanup(func() {
		agentInfoFileSync, agentInfoDirSync, agentInfoDirOpen = origFile, origDir, origOpen
	})
	agentInfoFileSync, agentInfoDirSync = fileSync, dirSync
}

// recordAgentInfoDirOpens wraps the dir-open seam (already saved by
// stubAgentInfoSyncs) so the test can check which directories
// syncAgentInfoDir opened.
func recordAgentInfoDirOpens(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	open := agentInfoDirOpen
	agentInfoDirOpen = func(dir string) (*os.File, error) {
		opened = append(opened, dir)
		return open(dir)
	}
	return &opened
}

// checkAgentInfoDirOpens checks that syncAgentInfoDir opened dir once,
// through the seam.
func checkAgentInfoDirOpens(t *testing.T, opened *[]string, dir string) {
	t.Helper()
	if len(*opened) != 1 || (*opened)[0] != dir {
		t.Errorf("dir opens = %q, want exactly [%q] through agentInfoDirOpen", *opened, dir)
	}
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
	}, func(*os.File) error { return nil })

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
	if goruntime.GOOS == "windows" {
		t.Skip("Windows cannot fsync a directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, provisionedWorktreeStateFile)

	var calls int
	var syncedDir string
	var finalAtSync []byte
	stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(d *os.File) error {
		calls++
		syncedDir = d.Name()
		finalAtSync, _ = os.ReadFile(path)
		return nil
	})
	opened := recordAgentInfoDirOpens(t)

	if err := writeProvisionedWorktreeRepoRoot(dir, "/repo"); err != nil {
		t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
	}
	checkAgentInfoDirOpens(t, opened, dir)
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
	stubAgentInfoSyncs(t, func(*os.File) error { return errSync }, func(*os.File) error { return nil })

	err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644)
	if !errors.Is(err, errSync) {
		t.Fatalf("writeAgentInfoFile error = %v, want %v", err, errSync)
	}
	if errors.Is(err, ErrAgentInfoNotDurable) {
		t.Errorf("pre-rename error %v wraps ErrAgentInfoNotDurable, want it unwrapped", err)
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
// sync failure is returned wrapped in ErrAgentInfoNotDurable, with the new
// content already in place.
func TestWriteAgentInfoFileReturnsDirSyncError(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows cannot fsync a directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-info.json")
	errSync := &os.PathError{Op: "sync", Path: dir, Err: syscall.EIO}
	stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(*os.File) error { return errSync })
	opened := recordAgentInfoDirOpens(t)

	err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("writeAgentInfoFile error = %v, want it to wrap %v", err, errSync)
	}
	checkAgentInfoDirOpens(t, opened, dir)
	if !errors.Is(err, ErrAgentInfoNotDurable) {
		t.Errorf("writeAgentInfoFile error = %v, want it to wrap ErrAgentInfoNotDurable", err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"new":true}` {
		t.Errorf("final contents = %q, want the new contents (the rename landed)", got)
	}
}

// TestWriteAgentInfoFileIgnoresUnsupportedDirSync checks that a filesystem
// reporting directory fsync as unsupported does not fail the write, as in
// pkg/config's syncDir.
func TestWriteAgentInfoFileIgnoresUnsupportedDirSync(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows cannot fsync a directory")
	}
	for _, unsupported := range []error{
		syscall.EINVAL,
		syscall.ENOTSUP,
		errors.ErrUnsupported,
		&os.PathError{Op: "sync", Path: "dir", Err: syscall.EINVAL},
		fmt.Errorf("wrapped: %w", syscall.ENOTSUP),
	} {
		t.Run(unsupported.Error(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-info.json")
			var calls int
			stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(*os.File) error {
				calls++
				return unsupported
			})

			if err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644); err != nil {
				t.Fatalf("writeAgentInfoFile error = %v, want nil for an unsupported dir sync", err)
			}
			if calls != 1 {
				t.Errorf("dir sync calls = %d, want 1", calls)
			}
			if got, _ := os.ReadFile(path); string(got) != `{"new":true}` {
				t.Errorf("final contents = %q, want the new contents", got)
			}
		})
	}
}

// TestWriteAgentInfoFileSkipsDirSyncWhenOpenFails checks that a parent
// directory that cannot be opened after the rename is skipped, as in
// pkg/config's syncDir: no error, no dir sync, new content in place.
func TestWriteAgentInfoFileSkipsDirSyncWhenOpenFails(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows cannot fsync a directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-info.json")
	var syncCalls int
	stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(*os.File) error {
		syncCalls++
		return nil
	})
	var opened []string
	agentInfoDirOpen = func(d string) (*os.File, error) {
		opened = append(opened, d)
		return nil, &os.PathError{Op: "open", Path: d, Err: syscall.EACCES}
	}

	if err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644); err != nil {
		t.Fatalf("writeAgentInfoFile error = %v, want nil when the parent dir cannot be opened", err)
	}
	checkAgentInfoDirOpens(t, &opened, dir)
	if syncCalls != 0 {
		t.Errorf("dir sync calls = %d, want 0 after a failed open", syncCalls)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"new":true}` {
		t.Errorf("final contents = %q, want the new contents", got)
	}
}

// TestWriteAgentInfoFileRenameErrorNotWrapped checks that a rename failure
// (the target is a non-empty directory) is returned without
// ErrAgentInfoNotDurable, since the target was left untouched.
func TestWriteAgentInfoFileRenameErrorNotWrapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-info.json")
	if err := os.MkdirAll(filepath.Join(path, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	var dirSyncCalls int
	stubAgentInfoSyncs(t, func(f *os.File) error { return f.Sync() }, func(*os.File) error {
		dirSyncCalls++
		return nil
	})

	err := writeAgentInfoFile(path, []byte(`{"new":true}`), 0o644)
	if err == nil {
		t.Fatal("writeAgentInfoFile error = nil, want the rename failure")
	}
	if errors.Is(err, ErrAgentInfoNotDurable) {
		t.Errorf("rename error %v wraps ErrAgentInfoNotDurable, want it unwrapped", err)
	}
	if dirSyncCalls != 0 {
		t.Errorf("dir sync calls = %d, want 0 after a failed rename", dirSyncCalls)
	}
	if fi, serr := os.Stat(filepath.Join(path, "child")); serr != nil || !fi.IsDir() {
		t.Errorf("target directory changed: stat child = %v, %v", fi, serr)
	}
}
