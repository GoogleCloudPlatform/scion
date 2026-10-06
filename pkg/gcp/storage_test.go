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

package gcp

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	rfs "github.com/rclone/rclone/fs"
)

// The tests below run syncFiltered, the shared body of SyncToGCS and
// SyncFromGCS, over two local directories: rclone's local backend stands in
// for the bucket, so the real filter and sync logic run without GCS.

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runSync(t *testing.T, dst, src string) error {
	t.Helper()
	ctx := context.Background()
	srcFs, err := rfs.NewFs(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	dstFs, err := rfs.NewFs(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	return syncFiltered(ctx, dstFs, srcFs)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func assertAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("%s exists (err=%v), want absent", p, err)
	}
}

// uploadedWorkspace is a workspace as the hub uploads it for a hub-native
// project whose .scion is a directory.
var uploadedWorkspace = map[string]string{
	".scion/settings.yaml": "hub:\n  enabled: true\n",
	".scion/project-id":    "uploaded-id\n",
	"README.md":            "readme",
	"src/main.go":          "package main\n",
	"src/.scion/nested":    "nested, not an identity entry",
}

const markerContent = "project-id: local-id\nproject-name: p\nproject-slug: p\n"

// TestSyncFiltered_DownloadIntoMarkerFileProject reproduces
// ptone/scion#3575: the broker has written .scion as a marker file and the
// uploaded workspace holds .scion/ as a directory. The download must
// succeed, leave the marker file untouched and sync everything else.
func TestSyncFiltered_DownloadIntoMarkerFileProject(t *testing.T) {
	bucket, project := t.TempDir(), t.TempDir()
	writeTree(t, bucket, uploadedWorkspace)
	writeTree(t, project, map[string]string{".scion": markerContent, "stale.txt": "gone after sync"})

	if err := runSync(t, project, bucket); err != nil {
		t.Fatalf("download into a marker-file project failed: %v", err)
	}

	info, err := os.Lstat(filepath.Join(project, ".scion"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf(".scion is no longer a regular file: info=%v err=%v", info, err)
	}
	if got := readFile(t, filepath.Join(project, ".scion")); got != markerContent {
		t.Errorf("marker file changed: %q", got)
	}
	for _, rel := range []string{"README.md", "src/main.go", "src/.scion/nested"} {
		if got := readFile(t, filepath.Join(project, rel)); got != uploadedWorkspace[rel] {
			t.Errorf("%s = %q, want %q", rel, got, uploadedWorkspace[rel])
		}
	}
	// Ordinary sync semantics are unchanged: extraneous files are removed.
	assertAbsent(t, filepath.Join(project, "stale.txt"))
}

// TestSyncFiltered_DownloadIntoDirectoryProject covers the directory layout
// (git projects): the local .scion directory and its project-id are kept,
// nothing from the uploaded .scion/ is merged in, and other files sync.
func TestSyncFiltered_DownloadIntoDirectoryProject(t *testing.T) {
	bucket, project := t.TempDir(), t.TempDir()
	writeTree(t, bucket, uploadedWorkspace)
	writeTree(t, project, map[string]string{".scion/project-id": "local-id\n", ".scion/agents/a/x": "local"})

	if err := runSync(t, project, bucket); err != nil {
		t.Fatalf("download into a directory-layout project failed: %v", err)
	}
	if got := readFile(t, filepath.Join(project, ".scion/project-id")); got != "local-id\n" {
		t.Errorf("local project-id changed: %q", got)
	}
	if got := readFile(t, filepath.Join(project, ".scion/agents/a/x")); got != "local" {
		t.Errorf("local .scion content changed: %q", got)
	}
	assertAbsent(t, filepath.Join(project, ".scion/settings.yaml"))
	if got := readFile(t, filepath.Join(project, "src/main.go")); got != uploadedWorkspace["src/main.go"] {
		t.Errorf("src/main.go = %q", got)
	}
}

// TestSyncFiltered_SourceWithoutIdentityKeepsLocal checks that a source
// with no .scion does not delete the destination's own identity entry.
func TestSyncFiltered_SourceWithoutIdentityKeepsLocal(t *testing.T) {
	for _, local := range []map[string]string{
		{".scion": markerContent},
		{".scion/project-id": "local-id\n"},
	} {
		src, dst := t.TempDir(), t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "a"})
		writeTree(t, dst, local)
		if err := runSync(t, dst, src); err != nil {
			t.Fatalf("sync failed: %v", err)
		}
		for rel, want := range local {
			if got := readFile(t, filepath.Join(dst, rel)); got != want {
				t.Errorf("%s = %q, want %q", rel, got, want)
			}
		}
		if got := readFile(t, filepath.Join(dst, "a.txt")); got != "a" {
			t.Errorf("a.txt = %q", got)
		}
	}
}

// TestSyncFiltered_UploadExcludesIdentity checks the upload direction: the
// root .scion entry, file or directory, never reaches the bucket, and a
// stray .scion/ already in the bucket from an earlier upload is left inert
// (not deleted, and never downloaded; see the download tests).
func TestSyncFiltered_UploadExcludesIdentity(t *testing.T) {
	for name, local := range map[string]map[string]string{
		"marker file": {".scion": markerContent, "a.txt": "a"},
		"directory":   {".scion/project-id": "local-id\n", ".scion/settings.yaml": "x: y\n", "a.txt": "a"},
	} {
		t.Run(name, func(t *testing.T) {
			workspace, bucket := t.TempDir(), t.TempDir()
			writeTree(t, workspace, local)
			if err := runSync(t, bucket, workspace); err != nil {
				t.Fatalf("upload failed: %v", err)
			}
			assertAbsent(t, filepath.Join(bucket, ".scion"))
			if got := readFile(t, filepath.Join(bucket, "a.txt")); got != "a" {
				t.Errorf("a.txt = %q", got)
			}
		})
	}

	t.Run("stray bucket entry", func(t *testing.T) {
		workspace, bucket := t.TempDir(), t.TempDir()
		writeTree(t, workspace, map[string]string{".scion": markerContent, "a.txt": "a"})
		writeTree(t, bucket, map[string]string{".scion/settings.yaml": "old\n"})
		if err := runSync(t, bucket, workspace); err != nil {
			t.Fatalf("upload over a stray .scion/ failed: %v", err)
		}
		if got := readFile(t, filepath.Join(bucket, ".scion/settings.yaml")); got != "old\n" {
			t.Errorf("stray entry = %q, want it left alone", got)
		}
	})
}

// TestSyncFiltered_DoesNotFollowSymlinks checks that symlinks in the source
// are skipped, not followed, including a .scion symlink pointing outside
// the tree.
func TestSyncFiltered_DoesNotFollowSymlinks(t *testing.T) {
	src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, outside, map[string]string{"secret": "outside"})
	writeTree(t, src, map[string]string{"a.txt": "a"})
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(src, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, ".scion")); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, dst, src); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	for _, rel := range []string{"link", "linkdir", ".scion"} {
		assertAbsent(t, filepath.Join(dst, rel))
	}
	if got := readFile(t, filepath.Join(dst, "a.txt")); got != "a" {
		t.Errorf("a.txt = %q", got)
	}
}

// rcloneTransferImports are rclone packages that copy or sync files. Any
// non-test file importing one could move a workspace without the identity
// filter, so only the files below may import them.
var rcloneTransferImports = map[string]bool{
	"github.com/rclone/rclone/fs/sync":       true,
	"github.com/rclone/rclone/fs/operations": true,
	"github.com/rclone/rclone/cmd/bisync":    true,
	"github.com/rclone/rclone/fs/march":      true,
}

// allowedRcloneTransferFiles maps each module-relative file allowed to
// import rcloneTransferImports to the reason it is allowed.
var allowedRcloneTransferFiles = map[string]string{
	// The workspace bucket helpers; all transfers go through syncFiltered.
	"pkg/gcp/storage.go": "filtered workspace bucket sync",
	// Project sync against the hub's WebDAV endpoint, not the bucket; it
	// applies its own exclude list, which includes .scion/**.
	"pkg/projectsync/projectsync.go": "WebDAV project sync with its own .scion exclude",
	// Fetches template directories from a remote URL; not a workspace.
	"pkg/config/remote_templates.go": "remote template fetch",
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// TestWorkspaceSyncGuard_NoUnfilteredRcloneTransfers fails if a non-test
// file in the module drives an rclone transfer outside the allow list, so
// every bucket sync has to go through SyncToGCS/SyncFromGCS and therefore
// syncFiltered.
func TestWorkspaceSyncGuard_NoUnfilteredRcloneTransfers(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "extras", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if rcloneTransferImports[p] {
				if _, ok := allowedRcloneTransferFiles[rel]; !ok {
					t.Errorf("%s imports %s: bucket syncs must go through gcp.SyncToGCS/SyncFromGCS so the .scion identity entry is excluded", rel, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceSyncGuard_StorageUsesFilteredSync checks storage.go itself:
// sync.Sync is called exactly once, inside syncFiltered, and both exported
// helpers call syncFiltered.
func TestWorkspaceSyncGuard_StorageUsesFilteredSync(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "storage.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	callsIn := map[string]map[string]int{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		calls := map[string]int{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok {
					calls[x.Name+"."+fun.Sel.Name]++
				}
			case *ast.Ident:
				calls[fun.Name]++
			}
			return true
		})
		callsIn[fn.Name.Name] = calls
	}
	total := 0
	for name, calls := range callsIn {
		for callee, n := range calls {
			if strings.HasPrefix(callee, "sync.") || strings.HasPrefix(callee, "operations.") {
				total += n
				if name != "syncFiltered" || callee != "sync.Sync" {
					t.Errorf("%s calls %s; only syncFiltered may call sync.Sync", name, callee)
				}
			}
		}
	}
	if total != 1 {
		t.Errorf("found %d rclone transfer calls in storage.go, want exactly 1 (in syncFiltered)", total)
	}
	if callsIn["syncFiltered"]["identityFilteredContext"] != 1 {
		t.Error("syncFiltered must build its context with identityFilteredContext")
	}
	for _, exported := range []string{"SyncToGCS", "SyncFromGCS"} {
		if callsIn[exported]["syncFiltered"] != 1 {
			t.Errorf("%s must call syncFiltered", exported)
		}
	}
}
