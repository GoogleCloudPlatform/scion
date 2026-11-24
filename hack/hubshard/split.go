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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// Splitting non-helper common test files in the overlay.
//
// When a regular test file (not *_helpers_test.go) declares a helper used by
// another file, file-level analysis marks the whole file common so every shard
// compiles it. That pulls all of the file's Test* functions and file-private
// types (especially structs embedding store.Store, each of which generates
// ~1,800 wrapper methods costing ~50 MB of cmd/compile RSS) into every shard.
//
// To avoid that fixed memory tax without moving declarations in the tree,
// splitCommonFiles partitions each non-helper common file at top-level
// declaration granularity inside the overlay:
//   - the original file name stays untagged (common) and keeps only the
//     declarations referenced by other files, init/TestMain, blank-identifier
//     assertions, methods on external types, and their intra-file closure;
//   - everything else (unreferenced Test* functions and file-private helpers
//     and types) moves into a generated sharded file
//     zz_hubshard_split_<orig>, preserving the original GOOS/GOARCH suffix and
//     build constraint, with //line directives so line numbers and file names
//     in test output match the source.

const splitPrefix = "zz_hubshard_split_"

// isSplitFile reports whether name is a generated split test file.
func isSplitFile(name string) bool {
	return strings.HasPrefix(name, splitPrefix) && strings.HasSuffix(name, "_test.go")
}

// splitOrigin returns the original file name for a generated split file, or ""
// if name is not a split file.
func splitOrigin(name string) string {
	if !isSplitFile(name) {
		return ""
	}
	return strings.TrimPrefix(name, splitPrefix)
}

type splitResult struct {
	origName   string
	splitName  string
	commonSrc  []byte
	commonInfo *fileInfo
	splitInfo  *fileInfo // untagged; rewrite adds (!hubshard || hubshard_K)
}

// splitCommonFiles partitions non-helper common files into a trimmed common
// piece and a sharded piece. Returns a map keyed by original file name.
func splitCommonFiles(dir string, files map[string]*fileInfo, an *analysis) (map[string]*splitResult, error) {
	// Precompute the set of unresolved identifiers and selector names
	// referenced by each file, and how many files reference each symbol.
	identUsers := map[string]int{}
	selUsers := map[string]int{}
	for _, f := range files {
		for id := range f.idents {
			identUsers[id]++
		}
		for sel := range f.selectors {
			selUsers[sel]++
		}
	}

	out := map[string]*splitResult{}
	for _, name := range sortedNames(files) {
		if !an.common[name] || isHelperFile(name) {
			continue
		}
		f := files[name]
		sp, err := splitOneCommonFile(dir, f, identUsers, selUsers)
		if err != nil {
			return nil, fmt.Errorf("split %s: %w", name, err)
		}
		if sp != nil {
			out[name] = sp
		}
	}
	return out, nil
}

func splitOneCommonFile(dir string, f *fileInfo, identUsers, selUsers map[string]int) (*splitResult, error) {
	// Skip files with //go:embed or cgo, which have special comment semantics.
	if bytes.Contains(f.src, []byte("//go:embed")) {
		return nil, nil
	}

	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, f.name, f.src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var imports []*ast.ImportSpec
	var decls []ast.Decl
	for _, d := range af.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			for _, sp := range gd.Specs {
				is := sp.(*ast.ImportSpec)
				if p, err := strconv.Unquote(is.Path.Value); err == nil && p == "C" {
					return nil, nil
				}
				imports = append(imports, is)
			}
			continue
		}
		decls = append(decls, d)
	}
	if len(decls) == 0 {
		return nil, nil
	}

	// Map each top-level declared name and receiver method to its decl index.
	declOf := map[string]int{}
	methodsOf := map[string][]int{}
	declTypes := make([][]string, len(decls))
	declRecv := make([]string, len(decls))
	keep := make([]bool, len(decls))

	usedOutside := func(sym string) bool {
		n := identUsers[sym]
		if f.idents[sym] {
			n--
		}
		return n > 0
	}
	selOutside := func(sym string) bool {
		n := selUsers[sym]
		if f.selectors[sym] {
			n--
		}
		return n > 0
	}

	// First pass: register all top-level non-method declarations in this file.
	for i, d := range decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				continue
			}
			name := d.Name.Name
			if name == "init" || name == "TestMain" || name == "_" {
				keep[i] = true
				continue
			}
			declOf[name] = i
			if usedOutside(name) {
				keep[i] = true
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					name := sp.Name.Name
					declOf[name] = i
					declTypes[i] = append(declTypes[i], name)
					if usedOutside(name) {
						keep[i] = true
					}
				case *ast.ValueSpec:
					for _, id := range sp.Names {
						if id.Name == "_" {
							keep[i] = true
							continue
						}
						declOf[id.Name] = i
						if usedOutside(id.Name) {
							keep[i] = true
						}
					}
				}
			}
		}
	}

	// Second pass: register methods and tie methods on external types to common.
	for i, d := range decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		recv := recvTypeName(fd.Recv.List[0].Type)
		declRecv[i] = recv
		if _, localType := declOf[recv]; localType {
			methodsOf[recv] = append(methodsOf[recv], i)
		} else if recv == "" || selOutside(fd.Name.Name) || usedOutside(recv) {
			keep[i] = true
		} else {
			// Method on a type declared outside this file (e.g. a pkg/hub
			// type): keep in common so any shard calling it sees the method.
			keep[i] = true
		}
	}

	// Collect all identifier names appearing inside each declaration.
	refs := make([]map[string]bool, len(decls))
	for i, d := range decls {
		r := map[string]bool{}
		ast.Inspect(d, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				r[id.Name] = true
			}
			return true
		})
		refs[i] = r
	}

	// Intra-file closure: anything referenced by a kept declaration stays kept.
	for changed := true; changed; {
		changed = false
		for i := range decls {
			if !keep[i] {
				continue
			}
			for name := range refs[i] {
				if j, ok := declOf[name]; ok && !keep[j] {
					keep[j] = true
					changed = true
				}
			}
			for _, t := range declTypes[i] {
				for _, j := range methodsOf[t] {
					if !keep[j] {
						keep[j] = true
						changed = true
					}
				}
			}
			if recv := declRecv[i]; recv != "" {
				if j, ok := declOf[recv]; ok && !keep[j] {
					keep[j] = true
					changed = true
				}
			}
		}
	}

	var keptDecls, splitDecls []ast.Decl
	for i, d := range decls {
		if keep[i] {
			keptDecls = append(keptDecls, d)
		} else {
			splitDecls = append(splitDecls, d)
		}
	}
	if len(splitDecls) == 0 {
		return nil, nil
	}

	commonSrc := renderSplitPiece(f, fset, imports, keptDecls, true)
	splitSrc := renderSplitPiece(f, fset, imports, splitDecls, false)

	splitName := splitPrefix + f.name
	commonInfo, err := parseFile(filepath.Join(dir, f.name), commonSrc)
	if err != nil {
		return nil, fmt.Errorf("parse common piece: %w", err)
	}
	splitInfo, err := parseFile(filepath.Join(dir, splitName), splitSrc)
	if err != nil {
		return nil, fmt.Errorf("parse split piece: %w", err)
	}

	return &splitResult{
		origName:   f.name,
		splitName:  splitName,
		commonSrc:  commonSrc,
		commonInfo: commonInfo,
		splitInfo:  splitInfo,
	}, nil
}

// renderSplitPiece builds a valid Go source file containing the selected
// declarations sliced verbatim from f.src, preceded by //line directives so
// file names and line numbers in test output match f.name.
func renderSplitPiece(f *fileInfo, fset *token.FileSet, imports []*ast.ImportSpec, decls []ast.Decl, keepBlankImports bool) []byte {
	usedPkgs := map[string]bool{}
	for _, d := range decls {
		ast.Inspect(d, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil {
					usedPkgs[id.Name] = true
				}
			}
			return true
		})
	}

	var keptImports []*ast.ImportSpec
	for _, is := range imports {
		if is.Name != nil {
			switch is.Name.Name {
			case "_":
				if keepBlankImports {
					keptImports = append(keptImports, is)
				}
				continue
			case ".":
				keptImports = append(keptImports, is)
				continue
			default:
				if usedPkgs[is.Name.Name] {
					keptImports = append(keptImports, is)
				}
				continue
			}
		}
		p, _ := strconv.Unquote(is.Path.Value)
		if usedPkgs[guessImportName(p)] {
			keptImports = append(keptImports, is)
		}
	}

	var buf bytes.Buffer
	if f.constraint != nil {
		fmt.Fprintf(&buf, "//go:build %s\n\n", f.constraint.String())
	}
	fmt.Fprintf(&buf, "// Code generated by hack/hubshard (split from %s); DO NOT EDIT.\n\n", f.name)
	fmt.Fprintf(&buf, "package %s\n\n", f.pkgName)
	if len(keptImports) > 0 {
		buf.WriteString("import (\n")
		for _, is := range keptImports {
			if is.Name != nil {
				fmt.Fprintf(&buf, "\t%s %s\n", is.Name.Name, is.Path.Value)
			} else {
				fmt.Fprintf(&buf, "\t%s\n", is.Path.Value)
			}
		}
		buf.WriteString(")\n\n")
	}

	for _, d := range decls {
		startPos := d.Pos()
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil {
				startPos = d.Doc.Pos()
			}
		case *ast.GenDecl:
			if d.Doc != nil {
				startPos = d.Doc.Pos()
			}
		}
		start := fset.Position(startPos)
		end := fset.Position(d.End())
		fmt.Fprintf(&buf, "//line %s:%d\n", f.name, start.Line)
		buf.Write(f.src[start.Offset:end.Offset])
		buf.WriteString("\n\n")
	}
	return buf.Bytes()
}
