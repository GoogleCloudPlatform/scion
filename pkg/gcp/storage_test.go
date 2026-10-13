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
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The tests below run syncLocalToRemote and syncRemoteToLocal, the bodies
// of SyncToGCS and SyncFromGCS, over two local directories: rclone's local
// backend stands in for the bucket, so the real filter and sync logic run
// without GCS.

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

// download and upload run the helpers behind SyncFromGCS and SyncToGCS,
// with a local directory in place of the bucket remote. The local side is
// opened exactly as in production.
func download(t *testing.T, bucket, local string) error {
	t.Helper()
	return syncRemoteToLocal(context.Background(), bucket, local)
}

func upload(t *testing.T, local, bucket string) error {
	t.Helper()
	return syncLocalToRemote(context.Background(), local, bucket)
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

	if err := download(t, bucket, project); err != nil {
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

	if err := download(t, bucket, project); err != nil {
		t.Fatalf("download into a directory-layout project failed: %v", err)
	}
	if got := readFile(t, filepath.Join(project, ".scion/project-id")); got != "local-id\n" {
		t.Errorf("local project-id changed: %q", got)
	}
	if got := readFile(t, filepath.Join(project, ".scion/agents/a/x")); got != "local" {
		t.Errorf("local .scion content changed: %q", got)
	}
	assertAbsent(t, filepath.Join(project, ".scion/settings.yaml"))
	for _, rel := range []string{"src/main.go", "src/.scion/nested"} {
		if got := readFile(t, filepath.Join(project, rel)); got != uploadedWorkspace[rel] {
			t.Errorf("%s = %q, want %q", rel, got, uploadedWorkspace[rel])
		}
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
		if err := download(t, src, dst); err != nil {
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
// SyncToGCS path does not upload the root .scion entry, file or directory,
// while a nested .scion is uploaded as ordinary content; and a
// stray .scion/ already in the bucket from an earlier upload is left inert
// (not deleted, and never downloaded; see the download tests).
func TestSyncFiltered_UploadExcludesIdentity(t *testing.T) {
	for name, local := range map[string]map[string]string{
		"marker file": {".scion": markerContent, "a.txt": "a", "sub/.scion/x": "nested"},
		"directory":   {".scion/project-id": "local-id\n", ".scion/settings.yaml": "x: y\n", "a.txt": "a", "sub/.scion/x": "nested"},
	} {
		t.Run(name, func(t *testing.T) {
			workspace, bucket := t.TempDir(), t.TempDir()
			writeTree(t, workspace, local)
			if err := upload(t, workspace, bucket); err != nil {
				t.Fatalf("upload failed: %v", err)
			}
			assertAbsent(t, filepath.Join(bucket, ".scion"))
			// Only the root entry is excluded: a nested .scion is ordinary
			// content and is uploaded.
			for _, rel := range []string{"a.txt", "sub/.scion/x"} {
				if got := readFile(t, filepath.Join(bucket, rel)); got != local[rel] {
					t.Errorf("%s = %q, want %q", rel, got, local[rel])
				}
			}
		})
	}

	t.Run("stray bucket entry", func(t *testing.T) {
		workspace, bucket := t.TempDir(), t.TempDir()
		writeTree(t, workspace, map[string]string{".scion": markerContent, "a.txt": "a"})
		writeTree(t, bucket, map[string]string{".scion/settings.yaml": "old\n"})
		if err := upload(t, workspace, bucket); err != nil {
			t.Fatalf("upload over a stray .scion/ failed: %v", err)
		}
		if got := readFile(t, filepath.Join(bucket, ".scion/settings.yaml")); got != "old\n" {
			t.Errorf("stray entry = %q, want it left alone", got)
		}
	})
}

// TestSyncFiltered_DoesNotFollowSymlinks checks that symlinks in a local
// workspace are skipped, not followed, including a .scion symlink pointing
// outside the tree. It runs the helpers' own local Fs construction, so a
// change there that follows links (for example a copy_links option on the
// local remote) fails it. The storage guard also rejects link options in
// storage.go, which covers the GCS remote string.
func TestSyncFiltered_DoesNotFollowSymlinks(t *testing.T) {
	for name, run := range map[string]func(t *testing.T, src, dst string) error{
		"upload":   func(t *testing.T, src, dst string) error { return upload(t, src, dst) },
		"download": func(t *testing.T, src, dst string) error { return download(t, src, dst) },
	} {
		t.Run(name, func(t *testing.T) {
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
			if err := run(t, src, dst); err != nil {
				t.Fatalf("sync failed: %v", err)
			}
			for _, rel := range []string{"link", "linkdir", ".scion"} {
				assertAbsent(t, filepath.Join(dst, rel))
			}
			if got := readFile(t, filepath.Join(dst, "a.txt")); got != "a" {
				t.Errorf("a.txt = %q", got)
			}
		})
	}
}

// TestSyncFiltered_FailedSyncDoesNotPoisonLaterSyncs checks that a sync
// error does not carry over to later syncs in the same process. rclone
// refuses to delete extraneous files once its stats record an error, so
// with shared stats every sync after a failure failed too.
func TestSyncFiltered_FailedSyncDoesNotPoisonLaterSyncs(t *testing.T) {
	// A file/directory mismatch on an ordinary path still fails the sync
	// (fail-closed): the bucket has dir/ as a directory, the local side
	// has dir as a file.
	bucket, bad := t.TempDir(), t.TempDir()
	writeTree(t, bucket, map[string]string{"dir/f.txt": "f"})
	writeTree(t, bad, map[string]string{"dir": "a file"})
	if err := download(t, bucket, bad); err == nil {
		t.Fatal("sync onto a file/directory mismatch succeeded, want an error")
	}

	// A clean sync afterwards must succeed, including its deletions.
	good := t.TempDir()
	writeTree(t, good, map[string]string{"stale.txt": "gone after sync", "old/x": "gone too"})
	if err := download(t, bucket, good); err != nil {
		t.Fatalf("sync after an earlier failed sync: %v", err)
	}
	if got := readFile(t, filepath.Join(good, "dir/f.txt")); got != "f" {
		t.Errorf("dir/f.txt = %q", got)
	}
	assertAbsent(t, filepath.Join(good, "stale.txt"))
	assertAbsent(t, filepath.Join(good, "old"))
}

// rcloneTransferImports are rclone packages that copy or sync files. Any
// non-test file importing one could run an rclone transfer to or from the
// bucket without the identity filter, so only the files below may import
// them. Transfers that do not use rclone (signed-URL uploads, direct
// storage-client writes) are outside this guard.
var rcloneTransferImports = map[string]bool{
	"github.com/rclone/rclone/fs/sync":       true,
	"github.com/rclone/rclone/fs/operations": true,
	"github.com/rclone/rclone/cmd/bisync":    true,
	"github.com/rclone/rclone/fs/march":      true,
}

// allowedRcloneTransferFiles maps each module-relative file allowed to
// import rcloneTransferImports to the reason it is allowed.
var allowedRcloneTransferFiles = map[string]string{
	// The workspace bucket helpers; their transfers all go through syncFiltered.
	"pkg/gcp/storage.go": "filtered workspace bucket sync",
	// Syncs a project with the hub's WebDAV endpoint, never with the
	// workspace bucket. (Its own unanchored ".scion/**" exclude drops
	// .scion directories at any depth, not a root .scion marker file.)
	"pkg/projectsync/projectsync.go": "hub WebDAV project sync, not the workspace bucket",
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
// file in the module imports an rclone transfer package outside the allow
// list, so every rclone transfer to or from the bucket has to go through
// SyncToGCS/SyncFromGCS and therefore syncFiltered.
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
					t.Errorf("%s imports %s: rclone bucket transfers must go through gcp.SyncToGCS/SyncFromGCS so the .scion identity entry is excluded", rel, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// storageSyncViolations parses src (the contents of storage.go) and
// returns every way it departs from the filtered-sync rules:
//   - an rclone transfer package is imported, under its own name or an
//     alias, and any reference through it other than the single sync.Sync
//     call in syncFiltered appears, including a function value held in a
//     local or package-level variable; a dot import is rejected outright,
//     since its references cannot be attributed;
//   - syncFiltered does not build its context with identityFilteredContext;
//   - the helpers do not route through syncFiltered;
//   - anything mentions symlink handling options (copy_links, links,
//     CopyLinks, ...), which would make rclone follow or translate links.
func storageSyncViolations(src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "storage.go", src, 0)
	if err != nil {
		return nil, err
	}
	var problems []string

	// Local names of the rclone transfer packages, mapped to their last
	// path element, so an aliased import is matched like the plain one.
	transferPkgs := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !rcloneTransferImports[p] {
			continue
		}
		base := p[strings.LastIndex(p, "/")+1:]
		local := base
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch local {
		case "_":
			continue
		case ".":
			problems = append(problems, "dot import of "+p+" is not allowed")
			continue
		}
		transferPkgs[local] = base
	}

	// Every reference to a transfer package counts, not just calls: a
	// function value such as "do := sync.Sync" or a package-level
	// "var doSync = sync.Sync" would otherwise reach rclone unfiltered.
	// The whole file is walked, so declarations outside functions are
	// covered too. The only allowed reference is the callee of the single
	// sync.Sync call in syncFiltered.
	callees := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				callees[sel] = true
			}
		}
		return true
	})
	callsIn := map[string]map[string]int{}
	total := 0
	for _, decl := range f.Decls {
		fnName := ""
		var body ast.Node = decl
		if fn, ok := decl.(*ast.FuncDecl); ok {
			fnName = fn.Name.Name
			if fn.Body == nil {
				continue
			}
			body = fn.Body
		}
		calls := map[string]int{}
		ast.Inspect(body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				x, ok := v.X.(*ast.Ident)
				if !ok {
					return true
				}
				base, ok := transferPkgs[x.Name]
				if !ok {
					return true
				}
				total++
				ref := x.Name + "." + v.Sel.Name
				switch {
				case fnName == "":
					problems = append(problems, "a package-level declaration references "+ref+"; only syncFiltered may call sync.Sync")
				case !callees[v]:
					problems = append(problems, fnName+" references "+ref+" as a value; only syncFiltered may call sync.Sync")
				case fnName != "syncFiltered" || base+"."+v.Sel.Name != "sync.Sync" || x.Name != "sync":
					problems = append(problems, fnName+" calls "+ref+"; only syncFiltered may call sync.Sync")
				}
			case *ast.CallExpr:
				switch fun := v.Fun.(type) {
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok {
						calls[x.Name+"."+fun.Sel.Name]++
					}
				case *ast.Ident:
					calls[fun.Name]++
				}
			}
			return true
		})
		if fnName != "" {
			callsIn[fnName] = calls
		}
	}
	if total != 1 {
		problems = append(problems, "found "+strconv.Itoa(total)+" rclone transfer references, want exactly 1 (the sync.Sync call in syncFiltered)")
	}
	if callsIn["syncFiltered"]["identityFilteredContext"] != 1 {
		problems = append(problems, "syncFiltered must build its context with identityFilteredContext")
	}
	for _, helper := range []string{"syncLocalToRemote", "syncRemoteToLocal"} {
		if callsIn[helper]["syncFiltered"] != 1 {
			problems = append(problems, helper+" must call syncFiltered")
		}
	}
	if callsIn["SyncToGCS"]["syncLocalToRemote"] != 1 {
		problems = append(problems, "SyncToGCS must call syncLocalToRemote")
	}
	if callsIn["SyncFromGCS"]["syncRemoteToLocal"] != 1 {
		problems = append(problems, "SyncFromGCS must call syncRemoteToLocal")
	}

	// Symlink options: rclone's local backend follows links with
	// copy_links (-L) and translates them with links (-l). Neither may
	// appear in a string, identifier or field name.
	ast.Inspect(f, func(n ast.Node) bool {
		var text string
		switch v := n.(type) {
		case *ast.BasicLit:
			text = v.Value
		case *ast.Ident:
			text = v.Name
		default:
			return true
		}
		if strings.Contains(strings.ToLower(text), "link") {
			problems = append(problems, "storage.go mentions "+text+"; symlinks must not be followed or translated")
		}
		return true
	})
	return problems, nil
}

// TestWorkspaceSyncGuard_StorageUsesFilteredSync checks storage.go itself:
// sync.Sync is called exactly once, inside syncFiltered, both helpers
// route through syncFiltered, and no symlink option is set.
func TestWorkspaceSyncGuard_StorageUsesFilteredSync(t *testing.T) {
	src, err := os.ReadFile("storage.go")
	if err != nil {
		t.Fatal(err)
	}
	problems, err := storageSyncViolations(string(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestWorkspaceSyncGuard_StorageGuardCatchesBypasses feeds the storage
// guard edited copies of storage.go, each with one way around the filter,
// and checks that every one is reported.
func TestWorkspaceSyncGuard_StorageGuardCatchesBypasses(t *testing.T) {
	b, err := os.ReadFile("storage.go")
	if err != nil {
		t.Fatal(err)
	}
	orig := string(b)
	const syncImport = `"github.com/rclone/rclone/fs/sync"`
	const helperBody = "return syncLocalToRemote(ctx, localPath, gcsRemote(bucketName, prefix))"
	edit := func(t *testing.T, s, old, new string) string {
		t.Helper()
		if !strings.Contains(s, old) {
			t.Fatalf("storage.go no longer contains %q; update this test", old)
		}
		return strings.Replace(s, old, new, 1)
	}
	// A second, unfiltered transfer in SyncToGCS, reached through an import.
	extraCall := func(t *testing.T, s, pkg string) string {
		return edit(t, s, helperBody, "src, _ := fs.NewFs(ctx, localPath)\n\tdst, _ := fs.NewFs(ctx, gcsRemote(bucketName, prefix))\n\t_ = "+pkg+".Sync(ctx, dst, src, false)\n\t"+helperBody)
	}
	// The download helper's tail, where a function-value bypass is added.
	const downloadTail = "fmt.Printf(\"Syncing %s to %s via rclone\\n\", remote, localPath)\n\n\treturn syncFiltered(ctx, dstFs, srcFs)"
	// Each case lists the problems its bypass must produce, so an
	// unrelated violation cannot make a case pass on its own.
	cases := map[string]struct {
		mutate func(t *testing.T) string
		want   []string
	}{
		"extra plain call": {
			mutate: func(t *testing.T) string {
				return extraCall(t, orig, "sync")
			},
			want: []string{"SyncToGCS calls sync.Sync; only syncFiltered may call sync.Sync"},
		},
		"aliased import": {
			mutate: func(t *testing.T) string {
				s := edit(t, orig, syncImport, syncImport+"\n\trsync "+syncImport)
				return extraCall(t, s, "rsync")
			},
			want: []string{"SyncToGCS calls rsync.Sync; only syncFiltered may call sync.Sync"},
		},
		"aliased operations import": {
			mutate: func(t *testing.T) string {
				s := edit(t, orig, syncImport, syncImport+"\n\tops \"github.com/rclone/rclone/fs/operations\"")
				return edit(t, s, helperBody, "_ = ops.CopyFile\n\tops.Purge(ctx, nil, \"\")\n\t"+helperBody)
			},
			want: []string{
				"SyncToGCS references ops.CopyFile as a value; only syncFiltered may call sync.Sync",
				"SyncToGCS calls ops.Purge; only syncFiltered may call sync.Sync",
			},
		},
		"dot import": {
			mutate: func(t *testing.T) string {
				return edit(t, orig, syncImport, ". \"github.com/rclone/rclone/fs/operations\"\n\t"+syncImport)
			},
			want: []string{"dot import of github.com/rclone/rclone/fs/operations is not allowed"},
		},
		"helper skips syncFiltered": {
			mutate: func(t *testing.T) string {
				return edit(t, orig, "fmt.Printf(\"Syncing %s to %s via rclone\\n\", localPath, remote)\n\n\treturn syncFiltered(ctx, dstFs, srcFs)",
					"fmt.Printf(\"Syncing %s to %s via rclone\\n\", localPath, remote)\n\n\treturn sync.Sync(ctx, dstFs, srcFs, false)")
			},
			want: []string{
				"syncLocalToRemote calls sync.Sync; only syncFiltered may call sync.Sync",
				"syncLocalToRemote must call syncFiltered",
			},
		},
		"local function value": {
			mutate: func(t *testing.T) string {
				return edit(t, orig, downloadTail, "do := sync.Sync\n\t_ = do(ctx, dstFs, srcFs, false)\n\t"+downloadTail)
			},
			want: []string{"syncRemoteToLocal references sync.Sync as a value; only syncFiltered may call sync.Sync"},
		},
		"package-level function value": {
			mutate: func(t *testing.T) string {
				s := edit(t, orig, "// syncStatsSeq numbers", "var doSync = sync.Sync\n\n// syncStatsSeq numbers")
				return edit(t, s, downloadTail, "_ = doSync(ctx, dstFs, srcFs, false)\n\t"+downloadTail)
			},
			want: []string{"a package-level declaration references sync.Sync; only syncFiltered may call sync.Sync"},
		},
		"copy_links on the local remote": {
			mutate: func(t *testing.T) string {
				return edit(t, orig, "srcFs, err := fs.NewFs(ctx, localPath)", "srcFs, err := fs.NewFs(ctx, \":local,copy_links=true:\"+localPath)")
			},
			want: []string{"storage.go mentions \":local,copy_links=true:\"; symlinks must not be followed or translated"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			problems, err := storageSyncViolations(tc.mutate(t))
			if err != nil {
				t.Fatalf("mutated storage.go does not parse: %v", err)
			}
			for _, want := range tc.want {
				if !slices.Contains(problems, want) {
					t.Errorf("guard did not report %q; got %q", want, problems)
				}
			}
		})
	}
}
