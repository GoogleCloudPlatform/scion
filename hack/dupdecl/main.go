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

// Command dupdecl fails when a package, together with its _test.go files,
// declares the same top-level identifier in two places under any of the
// given build-tag sets (`make hub-dupcheck`).
//
// CI compiles the pkg/hub tests only in shards (hack/hubshard), so two test
// files in different shards can each declare a helper of the same name: every
// shard builds, and every whole-package build (go vet ./pkg/hub/, go test
// ./pkg/hub/) fails with "redeclared in this block". This check is parse-only
// (go/parser, no type check), so it costs seconds and little memory.
//
// For each tag set, go/build (build.Context.MatchFile, this machine's
// GOOS/GOARCH) decides which files compile. Within each package clause (the
// package itself, and an external package_test as its own namespace) it
// counts top-level funcs, types, vars and consts, and methods as
// Receiver.Name. The blank identifier and init are skipped. It also reports
// an import whose local name is declared in the package block of the same
// package clause ("already declared through import of package"); the local
// name is the explicit one, else a guess from the import path (guessName:
// the last element after a trailing /vN, with a gopkg.in .vN suffix, a go-
// prefix and a -go suffix stripped; more conservative than goimports); a
// guess that is not an identifier is skipped, and a clash on a guessed name
// says so. Blank and dot imports are skipped. Not checked,
// because they need type information: dot imports, methods declared through
// an alias receiver (type A = T; func (A) m() against func (*T) m()), and
// clashes between a method and a struct field.
//
// Usage:
//
//	dupdecl [-tags LIST]... DIR
//
// Each -tags is one comma-separated tag set ("" is the default build); no
// -tags checks the default build only. Exit codes: 0 clean, 1 duplicates,
// 2 usage or parse error, or a tag set that matched no Go file.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type tagSets []string

func (t *tagSets) String() string     { return strings.Join(*t, " ") }
func (t *tagSets) Set(v string) error { *t = append(*t, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dupdecl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var sets tagSets
	flags.Var(&sets, "tags", "a comma-separated build-tag set to check (repeatable; \"\" is the default build)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: dupdecl [-tags LIST]... DIR")
		return 2
	}
	if len(sets) == 0 {
		sets = tagSets{""}
	}
	dir := flags.Arg(0)
	found := false
	for _, set := range sets {
		dups, nfiles, err := check(dir, splitTags(set))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "dupdecl: %v\n", err)
			return 2
		}
		label := set
		if label == "" {
			label = "(default)"
		}
		if len(dups) == 0 {
			_, _ = fmt.Fprintf(stdout, "dupdecl: %s -tags %s: %d files, no duplicate top-level declarations\n", dir, label, nfiles)
			continue
		}
		found = true
		for _, d := range dups {
			_, _ = fmt.Fprintf(stdout, "dupdecl: %s -tags %s: %s\n", dir, label, d)
		}
	}
	if found {
		_, _ = fmt.Fprintln(stdout, "dupdecl: a whole-package build (go vet, go test) fails with \"redeclared in this block\" or \"already declared through import\"; rename or remove one declaration")
		return 1
	}
	return 0
}

func splitTags(set string) []string {
	var tags []string
	for _, t := range strings.Split(set, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// check returns the duplicate declarations in dir under tags, one line each
// ("package hub: putWarnings declared at a_test.go:10 and b_test.go:20"),
// sorted, and the number of files compiled.
func check(dir string, tags []string) ([]string, int, error) {
	ctx := build.Default
	ctx.BuildTags = tags
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	fset := token.NewFileSet()
	// package name -> identifier -> positions
	decls := map[string]map[string][]token.Position{}
	// package name -> import local name -> positions
	imports := map[string]map[string][]token.Position{}
	// package name -> import local names guessed from an unaliased path
	guessed := map[string]map[string]bool{}
	nfiles := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		ok, err := ctx.MatchFile(dir, name)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, 0, err
		}
		nfiles++
		pkg := f.Name.Name
		if decls[pkg] == nil {
			decls[pkg] = map[string][]token.Position{}
			imports[pkg] = map[string][]token.Position{}
			guessed[pkg] = map[string]bool{}
		}
		for _, im := range f.Imports {
			name := importName(im)
			if name == "" || name == "_" || name == "." {
				continue
			}
			p := fset.Position(im.Pos())
			p.Filename = filepath.Base(p.Filename)
			imports[pkg][name] = append(imports[pkg][name], p)
			if im.Name == nil {
				guessed[pkg][name] = true
			}
		}
		add := func(id *ast.Ident, key string) {
			if id.Name == "_" {
				return
			}
			p := fset.Position(id.Pos())
			p.Filename = filepath.Base(p.Filename)
			decls[pkg][key] = append(decls[pkg][key], p)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					if d.Name.Name != "init" {
						add(d.Name, d.Name.Name)
					}
					continue
				}
				if recv := recvName(d.Recv); recv != "" {
					add(d.Name, recv+"."+d.Name.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						add(s.Name, s.Name.Name)
					case *ast.ValueSpec:
						for _, id := range s.Names {
							add(id, id.Name)
						}
					}
				}
			}
		}
	}
	var dups []string
	for pkg, m := range decls {
		for key, pos := range m {
			if len(pos) < 2 {
				continue
			}
			dups = append(dups, fmt.Sprintf("package %s: %s declared at %s", pkg, key, joinPos(pos)))
		}
	}
	for pkg, m := range imports {
		for name, ipos := range m {
			// Methods are keyed Recv.Name and never match an import name.
			dpos := decls[pkg][name]
			if len(dpos) == 0 {
				continue
			}
			line := fmt.Sprintf("package %s: %s imported at %s and declared at %s", pkg, name, firstPos(ipos, 3), joinPos(dpos))
			if guessed[pkg][name] {
				line += " (name guessed from the import path; if the package's real name differs, alias the import)"
			}
			dups = append(dups, line)
		}
	}
	sort.Strings(dups)
	if nfiles == 0 {
		return nil, 0, fmt.Errorf("%s: no Go files matched -tags %q", dir, strings.Join(tags, ","))
	}
	return dups, nfiles, nil
}

// sortedPos returns the positions as file:line, sorted by file name, then
// line number.
func sortedPos(pos []token.Position) []string {
	pos = append([]token.Position(nil), pos...)
	sort.Slice(pos, func(i, j int) bool {
		if pos[i].Filename != pos[j].Filename {
			return pos[i].Filename < pos[j].Filename
		}
		return pos[i].Line < pos[j].Line
	})
	where := make([]string, len(pos))
	for i, p := range pos {
		where[i] = fmt.Sprintf("%s:%d", p.Filename, p.Line)
	}
	return where
}

func joinPos(pos []token.Position) string {
	return strings.Join(sortedPos(pos), " and ")
}

// firstPos is joinPos limited to the first limit positions, with a count of
// the rest (a common package such as strings is imported by many files).
func firstPos(pos []token.Position, limit int) string {
	all := sortedPos(pos)
	if len(all) <= limit {
		return strings.Join(all, " and ")
	}
	return fmt.Sprintf("%s and %d more files", strings.Join(all[:limit], " and "), len(all)-limit)
}

// importName returns the name an import declares in its file block: the
// explicit name, else guessName of the path. It returns "" for an
// unparsable path or a guess that is not an identifier.
func importName(im *ast.ImportSpec) string {
	if im.Name != nil {
		return im.Name.Name
	}
	path, err := strconv.Unquote(im.Path.Value)
	if err != nil {
		return ""
	}
	return guessName(path)
}

// guessName guesses the package name of an import path without loading it:
// the last path element, after a trailing /vN element (any N, so
// google.golang.org/api/iam/v1 gives iam), with a gopkg.in .vN suffix, a
// go- prefix and a -go suffix stripped (go-jose gives jose, gax-go gives
// gax). It returns "" when the result is not an identifier, so no clash is
// reported on a name it cannot stand behind; goimports would instead cut
// the name at the first non-identifier rune (my-lib gives my).
func guessName(path string) string {
	elems := strings.Split(path, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && isVersion(last) {
		last = elems[len(elems)-2]
	}
	if i := strings.LastIndex(last, ".v"); i > 0 && strings.HasPrefix(path, "gopkg.in/") && isVersion(last[i+1:]) {
		last = last[:i]
	}
	last = strings.TrimPrefix(last, "go-")
	last = strings.TrimSuffix(last, "-go")
	if !token.IsIdentifier(last) {
		return ""
	}
	return last
}

// isVersion reports whether s is a major-version element: v0, v1, v2, ...
func isVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	_, err := strconv.ParseUint(s[1:], 10, 32)
	return err == nil
}

// recvName returns the base type name of a method receiver: T for (T),
// (*T), (T[K]) and (*T[K, V]).
func recvName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	t := fl.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}
