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
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// guardScanSubdirs lists the subpackages of pkg/hub (slash paths relative to
// pkg/hub) that source-scanning guard tests scan in addition to pkg/hub
// itself, so code moved out of package hub stays guarded. It is the single
// source of truth for those scans: when an area moves out of pkg/hub, add its
// directory here (and a pkgmove:scan-covers marker plus reason at each
// scanning site). An entry may name a directory that does not exist yet (an
// area that has not moved); guardScanDirs skips it until it exists.
var guardScanSubdirs = []string{
	"apierr",
	"internal/principal",
	"maintenance",
}

// guardScanDirs returns the directories a guard test scans for base, the
// pkg/hub directory (as an absolute or relative path): "." first, then each
// guardScanSubdirs entry that exists under base, as OS paths relative to
// base. Join each with base to read it; filepath.Join(".", name) is name, so
// keys built from the relative path stay the bare file name for pkg/hub
// itself. A missing entry is skipped; any other error (including an entry
// that is not a directory) is returned, so a guard never silently scans less
// than the list says once a directory exists.
func guardScanDirs(base string) ([]string, error) {
	dirs := []string{"."}
	for _, sub := range guardScanSubdirs {
		rel := filepath.FromSlash(sub)
		info, err := os.Stat(filepath.Join(base, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("guard scan directory %s: %w", sub, err)
		case !info.IsDir():
			return nil, fmt.Errorf("guard scan directory %s is not a directory", sub)
		}
		dirs = append(dirs, rel)
	}
	return dirs, nil
}

// mustGuardScanDirs is guardScanDirs for tests: it fails t on error.
func mustGuardScanDirs(t testing.TB, base string) []string {
	t.Helper()
	dirs, err := guardScanDirs(base)
	if err != nil {
		t.Fatalf("listing guard scan directories under %s: %v", base, err)
	}
	return dirs
}

// guardScanEntry is an fs.DirEntry whose Name is the file's slash path
// relative to the scanned base directory ("server.go",
// "internal/principal/identity.go").
type guardScanEntry struct {
	fs.DirEntry
	name string
}

func (e guardScanEntry) Name() string { return e.name }

// readGuardScanDirs is os.ReadDir(base) over every guardScanDirs directory:
// the entries of base first (named by base name, as os.ReadDir names them),
// then those of each existing guardScanSubdirs entry, named by their slash
// path relative to base. Callers that join a Name with base read the right
// file, and callers that skip directories still see only files.
func readGuardScanDirs(base string) ([]fs.DirEntry, error) {
	dirs, err := guardScanDirs(base)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, rel := range dirs {
		entries, err := os.ReadDir(filepath.Join(base, rel))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			out = append(out, guardScanEntry{DirEntry: e, name: path.Join(filepath.ToSlash(rel), e.Name())})
		}
	}
	return out, nil
}

// globGuardScanDirs is filepath.Glob(filepath.Join(base, pattern)) over every
// guardScanDirs directory: matches in base first, then in each existing
// guardScanSubdirs entry. With base "." the matches in pkg/hub are bare file
// names, exactly as filepath.Glob(pattern) returns them.
func globGuardScanDirs(base, pattern string) ([]string, error) {
	dirs, err := guardScanDirs(base)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rel := range dirs {
		matches, err := filepath.Glob(filepath.Join(base, rel, pattern))
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
	}
	return out, nil
}

// guardScanPackage returns the package name expected for files in rel, a
// directory returned by guardScanDirs: "hub" for "." and the last path
// element otherwise.
func guardScanPackage(rel string) string {
	if rel == "." {
		return "hub"
	}
	return path.Base(filepath.ToSlash(rel))
}

// enclosingFuncName returns the name of the function containing the given
// byte offset. Shared by create_message_enumeration_test.go and
// msg_containment_callsite_test.go.
func enclosingFuncName(fset *token.FileSet, f *ast.File, offset int) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Pos()).Offset
		end := fset.Position(fn.Body.End()).Offset
		if offset >= start && offset <= end {
			return fn.Name.Name
		}
	}
	return "<unknown>"
}

// findHubDir locates the pkg/hub directory relative to the test file.
// Shared by create_message_enumeration_test.go and
// msg_containment_callsite_test.go.
func findHubDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, "server.go")); err != nil {
		t.Fatalf("working directory %q does not look like pkg/hub (server.go not found)", wd)
	}
	return wd
}
