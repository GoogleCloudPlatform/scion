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

package hub

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// collectedFullPathField is the transfer.FileInfo field that holds a
// collected file's absolute path. Hub code reads collected files through an
// os.Root opened at the collected directory (transfer.ReadCollectedFile /
// OpenCollectedFile, as uploadDirBlobs and uploadResourceFiles do), never
// by that path.
const collectedFullPathField = "FullPath"

// checkCollectedFullPathSource parses one source file (src, or the file at
// path when src is nil) and returns a problem for every selector naming the
// FullPath field. The check is by name: package hub has no other field or
// method called FullPath, so any selector of that name is a read of a
// collected entry's absolute path. Composite-literal keys (FileInfo{FullPath:
// ...}) build an entry rather than read one and are not selectors, so they
// pass.
func checkCollectedFullPathSource(fset *token.FileSet, path string, src any) ([]string, error) {
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var problems []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != collectedFullPathField {
			return true
		}
		problems = append(problems, fmt.Sprintf("%s: %s reads a collected file by its absolute path (.%s); read it through an os.Root with transfer.ReadCollectedFile or transfer.OpenCollectedFile",
			fset.Position(sel.Pos()), enclosingFuncName(fset, f, fset.Position(sel.Pos()).Offset), collectedFullPathField))
		return true
	})
	return problems, nil
}

// TestCollectedFilesReadThroughRoot is a source guard: no non-test file in
// pkg/hub (or an area moved out of it) reads a collected entry's FullPath,
// so every hub reader of collected files goes through an os.Root and the
// transfer helpers, which check the entry is still a regular file of the
// collected size and hash.
func TestCollectedFilesReadThroughRoot(t *testing.T) {
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs: pkg/hub and every area moved out of it.
	files, err := globGuardScanDirs(".", "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found to scan")
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		problems, err := checkCollectedFullPathSource(fset, file, nil)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, p := range problems {
			t.Error(p)
		}
	}
}

// TestCheckCollectedFullPathSource feeds the source guard synthetic files: a
// read of FullPath is reported however it is reached, and code that reads
// through the root, or only builds an entry, passes.
func TestCheckCollectedFullPathSource(t *testing.T) {
	flagged := map[string]string{
		"os.ReadFile": `package hub
import ("os"; "github.com/GoogleCloudPlatform/scion/pkg/transfer")
func upload(fi transfer.FileInfo) { _, _ = os.ReadFile(fi.FullPath) }`,
		"os.Open of a slice element": `package hub
import ("os"; "github.com/GoogleCloudPlatform/scion/pkg/transfer")
func upload(files []transfer.FileInfo) { _, _ = os.Open(files[0].FullPath) }`,
		"assigned path": `package hub
import xfer "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func upload(j struct{ fi xfer.FileInfo }) string { p := j.fi.FullPath; return p }`,
	}
	for name, src := range flagged {
		t.Run("flagged/"+name, func(t *testing.T) {
			problems, err := checkCollectedFullPathSource(token.NewFileSet(), "flagged.go", src)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 {
				t.Fatalf("want one problem, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], "upload reads a collected file") {
				t.Errorf("problem does not name the function: %s", problems[0])
			}
		})
	}

	allowed := map[string]string{
		"read through the root": `package hub
import ("os"; "github.com/GoogleCloudPlatform/scion/pkg/transfer")
func upload(root *os.Root, fi transfer.FileInfo) { _, _ = transfer.ReadCollectedFile(root, fi) }`,
		"build an entry": `package hub
import "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func entry(p string) transfer.FileInfo { return transfer.FileInfo{Path: "a", FullPath: p} }`,
	}
	for name, src := range allowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			problems, err := checkCollectedFullPathSource(token.NewFileSet(), "allowed.go", src)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 0 {
				t.Errorf("want no problems, got %v", problems)
			}
		})
	}
}
