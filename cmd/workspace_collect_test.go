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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
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

// Workspace transfer sites that must call collectWorkspaceFiles.
var workspaceCollectRequiredSites = []string{
	"startAgentViaHub", // non-git workspace bootstrap upload
	"syncToViaHub",     // scion sync to (hub)
	"syncFromViaHub",   // scion sync from (hub): local comparison set
}

// Template and harness-config uploads collect a resource directory, not a
// workspace. They go through hubclient.CollectFiles, which applies the same
// defaults; new uses must be added here deliberately.
var workspaceCollectAllowedHubclient = map[string]bool{
	"syncTemplateToHub":         true,
	"runTemplateStatus":         true,
	"handleHubAndLocalTemplate": true,
	"syncHarnessConfigToHub":    true,
}

const (
	transferImportPath  = "github.com/GoogleCloudPlatform/scion/pkg/transfer"
	hubclientImportPath = "github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// workspaceCollectBanned lists, per import path, the collect entry points
// that only collectWorkspaceFiles (transfer) or the allow-listed resource
// uploads (hubclient.CollectFiles) may use.
var workspaceCollectBanned = map[string]map[string]bool{
	transferImportPath:  {"CollectFiles": true, "NewManifestBuilder": true, "ManifestBuilder": true},
	hubclientImportPath: {"CollectFiles": true, "NewManifestBuilder": true, "ManifestBuilder": true},
}

// checkWorkspaceCollectSource parses one non-test source file of package cmd
// and reports uses of the banned collect entry points outside the helper and
// the allow-list. Uses are matched by import path: the local name of each
// banned package is resolved from the file's imports, so an aliased import
// is checked like a default one. A dot or blank import of a banned package is
// reported outright. Functions that call the helper are added to
// callsHelper.
func checkWorkspaceCollectSource(fset *token.FileSet, filename string, src any, callsHelper map[string]bool) ([]string, error) {
	const helper = "collectWorkspaceFiles"
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var problems []string
	// Local package name -> import path, for the banned packages only.
	local := map[string]string{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if workspaceCollectBanned[path] == nil {
			continue
		}
		name := pathpkg.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." || name == "_" {
			problems = append(problems, fmt.Sprintf("%s: %q imported as %s; import it by name so the collect guard can check its uses",
				fset.Position(imp.Pos()), path, name))
			continue
		}
		local[name] = path
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
			if !ok {
				return true
			}
			path, ok := local[pkg.Name]
			if !ok || !workspaceCollectBanned[path][sel.Sel.Name] {
				return true
			}
			switch {
			case path == transferImportPath && fn == helper:
			case path == hubclientImportPath && sel.Sel.Name == "CollectFiles" && workspaceCollectAllowedHubclient[fn]:
			default:
				problems = append(problems, fmt.Sprintf("%s: %s.%s (%s) used in %q; collect workspace files with %s",
					fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name, path, fn, helper))
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
	return problems, nil
}

// TestWorkspaceCollectSitesUseHelper is a source guard: every workspace
// collect in package cmd goes through collectWorkspaceFiles. It fails if a
// non-test file uses transfer.CollectFiles or a transfer ManifestBuilder
// anywhere else (under any import name), if a hubclient collect appears
// outside the listed template/harness-config uploads, if either package is
// dot- or blank-imported, or if a known workspace transfer site stops calling
// the helper.
func TestWorkspaceCollectSitesUseHelper(t *testing.T) {
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
		problems, err := checkWorkspaceCollectSource(fset, path, nil, callsHelper)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, p := range problems {
			t.Error(p)
		}
	}
	for _, site := range workspaceCollectRequiredSites {
		if !callsHelper[site] {
			t.Errorf("%s no longer calls collectWorkspaceFiles", site)
		}
	}
}

// TestCheckWorkspaceCollectSource feeds the source guard synthetic files: a
// bypass is reported whatever name the package is imported under, and code
// that goes through the helper or the allow-list passes.
func TestCheckWorkspaceCollectSource(t *testing.T) {
	bypass := map[string]string{
		"default import": `package cmd
import "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func newSite(p string) { transfer.CollectFiles(p, nil) }
`,
		"aliased import": `package cmd
import xfer "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func newSite(p string) { xfer.CollectFiles(p, nil) }
`,
		"dot import": `package cmd
import . "github.com/GoogleCloudPlatform/scion/pkg/transfer"
func newSite(p string) { CollectFiles(p, nil) }
`,
		"blank import": `package cmd
import _ "github.com/GoogleCloudPlatform/scion/pkg/transfer"
`,
		"aliased manifest builder": `package cmd
import t2 "github.com/GoogleCloudPlatform/scion/pkg/transfer"
var b = t2.NewManifestBuilder
`,
		"aliased hubclient collect": `package cmd
import hc "github.com/GoogleCloudPlatform/scion/pkg/hubclient"
func newSite(p string) { hc.CollectFiles(p, nil) }
`,
		"hubclient dot import": `package cmd
import . "github.com/GoogleCloudPlatform/scion/pkg/hubclient"
`,
	}
	for name, src := range bypass {
		t.Run(name, func(t *testing.T) {
			problems, err := checkWorkspaceCollectSource(token.NewFileSet(), "bypass.go", src, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) == 0 {
				t.Errorf("guard did not report the bypass in:\n%s", src)
			}
		})
	}

	t.Run("clean source", func(t *testing.T) {
		src := `package cmd
import (
	xfer "github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)
type transferLike struct{}
func (transferLike) CollectFiles() {}
func collectWorkspaceFiles(p string, extra []string) ([]xfer.FileInfo, error) { return xfer.CollectFiles(p, extra) }
func syncToViaHub(p string) { collectWorkspaceFiles(p, nil); _ = xfer.BuildManifest(nil) }
func syncTemplateToHub(p string) { hubclient.CollectFiles(p, nil) }
func other() { var transfer transferLike; transfer.CollectFiles() }
`
		callsHelper := map[string]bool{}
		problems, err := checkWorkspaceCollectSource(token.NewFileSet(), "clean.go", src, callsHelper)
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 0 {
			t.Errorf("guard reported clean source: %v", problems)
		}
		if !callsHelper["syncToViaHub"] {
			t.Errorf("guard did not record syncToViaHub calling the helper: %v", callsHelper)
		}
	})
}
