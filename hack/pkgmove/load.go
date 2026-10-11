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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// srcFile is one Go file of the source (or target) directory.
type srcFile struct {
	Name       string // base name
	Path       string // absolute path
	Src        []byte
	AST        *ast.File
	PkgName    string
	IsTest     bool   // file name ends in _test.go
	XTest      bool   // package clause is <name>_test
	Included   bool   // matched by the build context
	Constraint string // effective build constraint ("" when none)
	CGo        bool   // imports "C"
	Moved      bool   // part of the move set
}

// goList runs `go list` in dir with the given arguments and returns stdout.
func goList(dir string, tags []string, args ...string) ([]byte, error) {
	full := []string{"list"}
	if len(tags) > 0 {
		full = append(full, "-tags="+strings.Join(tags, ","))
	}
	full = append(full, args...)
	cmd := exec.Command("go", full...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go %s: %v\n%s", strings.Join(full, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Name       string
	Export     string
	Module     *struct {
		Path      string
		Dir       string
		GoVersion string
	}
	Error *struct{ Err string }

	GoFiles, TestGoFiles, XTestGoFiles []string
	IgnoredGoFiles                     []string
	Imports, TestImports, XTestImports []string
	Deps                               []string
}

// decodeList decodes the concatenated JSON objects that `go list -json` prints.
func decodeList(out []byte) ([]listedPackage, error) {
	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// moduleInfo describes the module and import path of a directory.
type moduleInfo struct {
	ImportPath string
	PkgName    string
	ModPath    string
	ModDir     string
	GoVersion  string
}

func resolveDir(dir string, tags []string) (*moduleInfo, error) {
	out, err := goList(dir, tags, "-json=ImportPath,Name,Dir,Module", ".")
	if err != nil {
		return nil, err
	}
	pkgs, err := decodeList(out)
	if err != nil || len(pkgs) != 1 {
		return nil, fmt.Errorf("cannot resolve package in %s: %v", dir, err)
	}
	p := pkgs[0]
	if p.Module == nil {
		return nil, fmt.Errorf("%s is not inside a Go module", dir)
	}
	return &moduleInfo{ImportPath: p.ImportPath, PkgName: p.Name, ModPath: p.Module.Path, ModDir: p.Module.Dir, GoVersion: p.Module.GoVersion}, nil
}

// readDir parses every .go file of dir and classifies it against the build
// context. Files are returned sorted by name.
func readDir(fset *token.FileSet, dir string, ctx *build.Context) ([]*srcFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var files []*srcFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %v", path, err)
		}
		included, err := ctx.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("match %s: %v", path, err)
		}
		sf := &srcFile{
			Name:     name,
			Path:     path,
			Src:      src,
			AST:      f,
			PkgName:  f.Name.Name,
			IsTest:   strings.HasSuffix(name, "_test.go"),
			Included: included,
		}
		sf.XTest = sf.IsTest && strings.HasSuffix(sf.PkgName, "_test")
		sf.Constraint, err = effectiveConstraint(name, src)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "C" {
				sf.CGo = true
			}
		}
		files = append(files, sf)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

func buildContext(tags []string) *build.Context {
	ctx := build.Default
	ctx.BuildTags = append([]string(nil), tags...)
	return &ctx
}

// exportImporter imports packages from the export data that `go list -export`
// produced, with optional in-memory overrides (for packages type-checked from
// source by this tool).
type exportImporter struct {
	exports   map[string]string
	deps      map[string][]string // transitive dependencies of each listed package
	overrides map[string]*types.Package
	gc        types.Importer
}

func (imp *exportImporter) Import(path string) (*types.Package, error) {
	if p, ok := imp.overrides[path]; ok {
		return p, nil
	}
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	return imp.gc.Import(path)
}

// newExportImporter compiles export data for the given import paths (and their
// dependencies) and returns an importer that serves them.
func newExportImporter(fset *token.FileSet, dir string, tags []string, paths []string) (*exportImporter, error) {
	imp := &exportImporter{exports: map[string]string{}, deps: map[string][]string{}, overrides: map[string]*types.Package{}}
	if len(paths) > 0 {
		args := append([]string{"-e", "-export", "-deps", "-json=ImportPath,Export,Error,Deps"}, paths...)
		out, err := goList(dir, tags, args...)
		if err != nil {
			return nil, err
		}
		pkgs, err := decodeList(out)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			if p.Error != nil {
				return nil, fmt.Errorf("go list: %s: %s", p.ImportPath, p.Error.Err)
			}
			if p.Export != "" {
				imp.exports[p.ImportPath] = p.Export
			}
			imp.deps[p.ImportPath] = p.Deps
		}
	}
	imp.gc = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := imp.exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(file)
	})
	return imp, nil
}

// importsOf returns the sorted, de-duplicated import paths of files, minus
// the excluded paths and the pseudo-packages "C" and "unsafe".
func importsOf(files []*srcFile, exclude ...string) []string {
	seen := map[string]bool{"C": true, "unsafe": true}
	for _, e := range exclude {
		seen[e] = true
	}
	var out []string
	for _, f := range files {
		for _, spec := range f.AST.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// typeCheck type-checks files as package path and fails on any error.
func typeCheck(fset *token.FileSet, path string, files []*srcFile, imp types.Importer, goVersion string) (*types.Package, *types.Info, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Implicits:  map[ast.Node]types.Object{},
	}
	var errs []string
	conf := types.Config{
		Importer:    imp,
		FakeImportC: true,
		Error: func(err error) {
			if len(errs) < 20 {
				errs = append(errs, err.Error())
			}
		},
	}
	if goVersion != "" {
		conf.GoVersion = "go" + goVersion
	}
	asts := make([]*ast.File, len(files))
	for i, f := range files {
		asts[i] = f.AST
	}
	pkg, _ := conf.Check(path, fset, asts, info)
	if len(errs) > 0 {
		return nil, nil, fmt.Errorf("type-checking %s failed:\n  %s", path, strings.Join(errs, "\n  "))
	}
	return pkg, info, nil
}

// typecheckAfter type-checks the target package (alone and with its
// in-package tests) and then the source package with its in-package tests,
// importing the freshly checked target.
func typecheckAfter(cfg *Config, mod *moduleInfo, dstImport string) error {
	fset := token.NewFileSet()
	ctx := buildContext(cfg.Tags)
	dst, err := readDir(fset, cfg.DstDir, ctx)
	if err != nil {
		return err
	}
	src, err := readDir(fset, cfg.SrcDir, ctx)
	if err != nil {
		return err
	}
	var dstLib, dstAll, srcAll []*srcFile
	for _, f := range dst {
		if f.Included && !f.XTest {
			dstAll = append(dstAll, f)
			if !f.IsTest {
				dstLib = append(dstLib, f)
			}
		}
	}
	for _, f := range src {
		if f.Included && !f.XTest {
			srcAll = append(srcAll, f)
		}
	}
	all := append(append([]*srcFile(nil), dstAll...), srcAll...)
	imp, err := newExportImporter(fset, cfg.SrcDir, cfg.Tags, importsOf(all, mod.ImportPath, dstImport))
	if err != nil {
		return err
	}
	lib, _, err := typeCheck(fset, dstImport, dstLib, imp, mod.GoVersion)
	if err != nil {
		return err
	}
	if len(dstAll) != len(dstLib) {
		if _, _, err := typeCheck(fset, dstImport, dstAll, imp, mod.GoVersion); err != nil {
			return err
		}
	}
	imp.overrides[dstImport] = lib
	if _, _, err := typeCheck(fset, mod.ImportPath, srcAll, imp, mod.GoVersion); err != nil {
		return err
	}
	return nil
}
