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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func writeWorkspaceTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func collectedPaths(files []transfer.FileInfo) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// TestCollectWorkspaceFiles_ExcludesRootDotScion runs a real collect over a
// temp workspace in both .scion layouts and with several spellings of the
// root path: the root .scion entry is dropped, nested ones are kept.
func TestCollectWorkspaceFiles_ExcludesRootDotScion(t *testing.T) {
	kept := []string{
		"main.go",
		"foo/.scion",
		"bar/.scion/settings.yaml",
		".scionrc",
		"logs/app.log",
	}
	layouts := map[string][]string{
		"marker file": {".scion"},
		"directory":   {".scion/project-id", ".scion/settings.yaml", ".scion/templates/x/y.md"},
	}

	parent := t.TempDir()
	t.Chdir(parent)

	for layout, rootEntries := range layouts {
		name := strings.ReplaceAll(layout, " ", "-")
		abs := filepath.Join(parent, name)
		writeWorkspaceTree(t, abs, append(append([]string{}, rootEntries...), kept...)...)

		roots := map[string]string{
			"absolute":                abs,
			"absolute trailing slash": abs + string(filepath.Separator),
			"relative":                name,
			"leading dot":             "." + string(filepath.Separator) + name,
			"leading dot trailing":    "." + string(filepath.Separator) + name + string(filepath.Separator),
			"unclean":                 filepath.Join(name, "foo") + string(filepath.Separator) + "..",
		}
		for form, root := range roots {
			t.Run(layout+"/"+form, func(t *testing.T) {
				files, err := collectWorkspaceFiles(root, nil)
				if err != nil {
					t.Fatalf("collectWorkspaceFiles(%q): %v", root, err)
				}
				want := slices.Sorted(slices.Values(kept))
				if got := collectedPaths(files); !slices.Equal(got, want) {
					t.Errorf("collectWorkspaceFiles(%q) = %v, want %v", root, got, want)
				}
			})
		}
	}

	t.Run("extra patterns add to the defaults", func(t *testing.T) {
		files, err := collectWorkspaceFiles(filepath.Join(parent, "directory"), []string{"*.log"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{".scionrc", "bar/.scion/settings.yaml", "foo/.scion", "main.go"}
		if got := collectedPaths(files); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// TestCollectWorkspaceFiles_FailsClosed checks that the helper refuses to
// return the root .scion entry even if the default excludes no longer drop
// it.
func TestCollectWorkspaceFiles_FailsClosed(t *testing.T) {
	saved := transfer.DefaultExcludePatterns
	t.Cleanup(func() { transfer.DefaultExcludePatterns = saved })
	transfer.DefaultExcludePatterns = nil

	for layout, entries := range map[string][]string{
		"marker file": {".scion", "main.go"},
		"directory":   {".scion/project-id", "main.go"},
	} {
		t.Run(layout, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkspaceTree(t, dir, entries...)
			files, err := collectWorkspaceFiles(dir, nil)
			if err == nil {
				t.Fatalf("expected an error, got files %v", collectedPaths(files))
			}
			if files != nil {
				t.Errorf("expected no files on error, got %v", collectedPaths(files))
			}
		})
	}
}

// TestWorkspaceCollectSitesUseHelper is a source guard: every workspace
// collect in package cmd goes through collectWorkspaceFiles. It fails if a
// non-test file calls transfer.CollectFiles or builds a transfer
// ManifestBuilder anywhere else, if a hubclient collect appears outside the
// listed template/harness-config uploads, or if a known workspace transfer
// site stops calling the helper.
func TestWorkspaceCollectSitesUseHelper(t *testing.T) {
	const helper = "collectWorkspaceFiles"
	// Workspace transfer sites that must call the helper.
	requiredSites := []string{
		"startAgentViaHub", // non-git workspace bootstrap upload
		"syncToViaHub",     // scion sync to (hub)
		"syncFromViaHub",   // scion sync from (hub): local comparison set
	}
	// Template and harness-config uploads collect a resource directory, not a
	// workspace. They go through hubclient.CollectFiles, which applies the
	// same defaults; new uses must be added here deliberately.
	allowedHubclientCollect := map[string]bool{
		"syncTemplateToHub":         true,
		"runTemplateStatus":         true,
		"handleHubAndLocalTemplate": true,
		"syncHarnessConfigToHub":    true,
	}
	banned := map[string]map[string]bool{
		"transfer":  {"CollectFiles": true, "NewManifestBuilder": true, "ManifestBuilder": true},
		"hubclient": {"CollectFiles": true, "NewManifestBuilder": true, "ManifestBuilder": true},
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	callsHelper := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		// Package-level declarations (outside any function) count as an
		// enclosing function named "".
		check := func(fn string, root ast.Node) {
			ast.Inspect(root, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == helper {
						callsHelper[fn] = true
					}
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || !banned[pkg.Name][sel.Sel.Name] {
					return true
				}
				switch {
				case pkg.Name == "transfer" && fn == helper:
				case pkg.Name == "hubclient" && sel.Sel.Name == "CollectFiles" && allowedHubclientCollect[fn]:
				default:
					t.Errorf("%s: %s.%s used in %q; collect workspace files with %s",
						fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name, fn, helper)
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				check(fd.Name.Name, fd)
			} else {
				check("", decl)
			}
		}
	}
	for _, site := range requiredSites {
		if !callsHelper[site] {
			t.Errorf("%s no longer calls %s", site, helper)
		}
	}
}
