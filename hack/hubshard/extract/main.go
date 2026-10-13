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

// Command extract moves test-helper declarations of a Go package verbatim
// into dedicated helper-only *_helpers_test.go files, and verifies such a
// move. It prepares pkg/hub for build-tag sharding of its test files: once
// the helpers that many test files share live in their own files, a shard
// needs only those files plus its own tests.
//
// It works on syntax only (go/parser); it never type-checks or compiles the
// package, so it is cheap to run on pkg/hub.
//
//	extract move   -dir pkg/hub -family hack/hubshard/extract/families/ids.txt
//	extract bulk   -dir pkg/hub [-by area|origin] [-origins 'handlers_*']
//	extract verify -before /tmp/hub-main -after pkg/hub
//	extract export -dir pkg/hub -to pkg/hub/internal/hubtestutil -family hack/hubshard/extract/families/hubtestutil-wave1.txt
//	extract verify-export -before /tmp/hub-main -after pkg/hub -to pkg/hub/internal/hubtestutil -family ...
//
// See README.md for the rules a move follows.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/build/constraint"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "move":
		err = runMove(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "bulk":
		err = runBulk(os.Args[2:])
	case "export":
		err = runExport(os.Args[2:])
	case "verify-export":
		err = runVerifyExport(os.Args[2:])
	case "comment-only":
		err = runCommentOnly(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "extract:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: extract move -dir DIR -family FILE [-n]\n       extract bulk -dir DIR [-by area|origin] [-origins GLOB] [-keep FILE] [-n]\n       extract verify -before DIR -after DIR\n       extract export -dir DIR -to DIR [-import PATH] [-alias FILE] -family FILE [-n]\n       extract verify-export -before DIR -after DIR -to DIR [-import PATH] [-alias FILE] -family FILE\n       extract comment-only -before DIR -after DIR")
	os.Exit(2)
}

func runMove(args []string) error {
	fs := flag.NewFlagSet("move", flag.ExitOnError)
	dir := fs.String("dir", "pkg/hub", "package directory")
	famPath := fs.String("family", "", "family spec file")
	dry := fs.Bool("n", false, "dry run: print the plan, write nothing")
	_ = fs.Parse(args)
	if *famPath == "" {
		return fmt.Errorf("-family is required")
	}
	fam, err := readFamily(*famPath)
	if err != nil {
		return err
	}
	p, err := loadPackage(*dir)
	if err != nil {
		return err
	}
	res, err := plan(p, fam)
	if err != nil {
		return err
	}
	for _, m := range res.moves {
		fmt.Printf("move %-40s %s -> %s (%s)\n", m.d.key, m.d.file.name, fam.dest, m.reason)
	}
	for _, name := range sortedKeys(res.changed) {
		if res.changed[name] == nil {
			fmt.Printf("delete %s (no declarations left)\n", name)
		}
	}
	if len(res.moves) == 0 {
		fmt.Println("nothing to move")
		return nil
	}
	if *dry {
		return nil
	}
	return apply(p, res)
}

func runBulk(args []string) error {
	fs := flag.NewFlagSet("bulk", flag.ExitOnError)
	dir := fs.String("dir", "pkg/hub", "package directory")
	by := fs.String("by", "area", "helper file per \"area\" (file-name prefix and constraint) or per \"origin\" file")
	glob := fs.String("origins", "", "only take declarations from test files whose base name matches this glob")
	dry := fs.Bool("n", false, "dry run: print the first round's plan, write nothing")
	keepPath := fs.String("keep", "", "file of declaration names bulk must never move (one per line, # comments)")
	maxIter := fs.Int("max-rounds", 50, "give up if no fixpoint after this many rounds")
	_ = fs.Parse(args)
	if *glob != "" {
		if _, err := path.Match(*glob, ""); err != nil {
			return fmt.Errorf("-origins: %w", err)
		}
	}
	if *by != "area" && *by != "origin" {
		return fmt.Errorf("-by must be area or origin, not %q", *by)
	}
	keep := map[string]bool{}
	if *keepPath != "" {
		var err error
		if keep, err = readKeep(*keepPath); err != nil {
			return err
		}
	}
	st, err := runBulkOn(*dir, *glob, *by, keep, *dry, *maxIter, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("bulk: %d round(s), %d declaration(s) moved from %d origin file(s), %d origin(s) skipped\n",
		st.iterations, st.moved, len(st.origins), len(st.skipped))
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	before := fs.String("before", "", "package directory before the move")
	after := fs.String("after", "", "package directory after the move")
	_ = fs.Parse(args)
	if *before == "" || *after == "" {
		return fmt.Errorf("-before and -after are required")
	}
	pb, err := loadPackage(*before)
	if err != nil {
		return err
	}
	pa, err := loadPackage(*after)
	if err != nil {
		return err
	}
	r := verify(pb, pa)
	r.write(os.Stdout)
	if !r.ok() {
		return fmt.Errorf("verification failed")
	}
	return nil
}

const (
	modulePath       = "github.com/GoogleCloudPlatform/scion"
	defaultAliasFile = "zz_alias_hubtestutil_test.go"
)

// defaultImportPath derives the target's import path from its directory,
// relative to the module root (the current directory).
func defaultImportPath(dir string) string {
	return modulePath + "/" + filepath.ToSlash(filepath.Clean(dir))
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dir := fs.String("dir", "pkg/hub", "source package directory")
	to := fs.String("to", "pkg/hub/internal/hubtestutil", "target package directory (non-test files)")
	imp := fs.String("import", "", "target import path (default: module path + -to)")
	alias := fs.String("alias", defaultAliasFile, "alias file written into -dir")
	famPath := fs.String("family", "", "export family file")
	dry := fs.Bool("n", false, "dry run: print the plan, write nothing")
	_ = fs.Parse(args)
	if *famPath == "" {
		return fmt.Errorf("-family is required")
	}
	if *imp == "" {
		*imp = defaultImportPath(*to)
	}
	spec, err := readExportSpec(*famPath)
	if err != nil {
		return err
	}
	p, err := loadPackage(*dir)
	if err != nil {
		return err
	}
	ep, err := planExport(p, spec, *to, *imp, *alias)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	ep.report(&b)
	_, _ = os.Stdout.Write(b.Bytes())
	if *dry {
		return nil
	}
	return ep.write(*to)
}

func runVerifyExport(args []string) error {
	fs := flag.NewFlagSet("verify-export", flag.ExitOnError)
	before := fs.String("before", "", "source package directory before the export")
	after := fs.String("after", "", "source package directory after the export")
	to := fs.String("to", "", "target package directory (default: -after's internal/hubtestutil, so archive trees need no extra flags)")
	imp := fs.String("import", "", "target import path (default: the hubtestutil import path when -to is defaulted, else module path + -to)")
	alias := fs.String("alias", defaultAliasFile, "alias file in -after")
	famPath := fs.String("family", "", "export family file")
	_ = fs.Parse(args)
	if *before == "" || *after == "" || *famPath == "" {
		return fmt.Errorf("-before, -after and -family are required")
	}
	if *to == "" {
		*to = filepath.Join(*after, "internal", "hubtestutil")
		if *imp == "" {
			*imp = modulePath + "/pkg/hub/internal/hubtestutil"
		}
	}
	if *imp == "" {
		*imp = defaultImportPath(*to)
	}
	spec, err := readExportSpec(*famPath)
	if err != nil {
		return err
	}
	pb, err := loadPackage(*before)
	if err != nil {
		return err
	}
	pa, err := loadPackage(*after)
	if err != nil {
		return err
	}
	r, err := verifyExport(pb, pa, *to, *imp, spec, *alias)
	if err != nil {
		return err
	}
	r.write(os.Stdout)
	if !r.ok() {
		return fmt.Errorf("verification failed")
	}
	return nil
}

// runCommentOnly checks that the Go files of two directories differ only in
// comments, for a hand-written comment fix on top of generated code.
func runCommentOnly(args []string) error {
	fs := flag.NewFlagSet("comment-only", flag.ExitOnError)
	before := fs.String("before", "", "directory before the change")
	after := fs.String("after", "", "directory after the change")
	_ = fs.Parse(args)
	if *before == "" || *after == "" {
		return fmt.Errorf("-before and -after are required")
	}
	diffs, err := commentOnlyDiff(*before, *after)
	if err != nil {
		return err
	}
	for _, d := range diffs {
		fmt.Println(d)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("%d file(s) differ in code, not only in comments", len(diffs))
	}
	fmt.Println("RESULT: OK (comments and whitespace only)")
	return nil
}

// readFamily parses a family spec: blank lines and #-comments are ignored,
// the first line is "dest <file>", an optional next line "build <expr>" gives
// the //go:build constraint of a new dest file, and every other line is one
// top-level name.
func readFamily(path string) (family, error) {
	f, err := os.Open(path)
	if err != nil {
		return family{}, err
	}
	defer func() { _ = f.Close() }()
	var fam family
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "dest "); ok && fam.dest == "" {
			fam.dest = strings.TrimSpace(rest)
			continue
		}
		if fam.dest == "" {
			return family{}, fmt.Errorf("%s: first entry must be \"dest <file>\"", path)
		}
		if rest, ok := strings.CutPrefix(line, "build "); ok && fam.build == "" && len(fam.names) == 0 {
			x, err := constraint.Parse("//go:build " + strings.TrimSpace(rest))
			if err != nil {
				return family{}, fmt.Errorf("%s: build: %w", path, err)
			}
			fam.build = x.String()
			continue
		}
		fam.names = append(fam.names, line)
	}
	if err := sc.Err(); err != nil {
		return family{}, err
	}
	if len(fam.names) == 0 {
		return family{}, fmt.Errorf("%s: no names", path)
	}
	return fam, nil
}

// readKeep parses a bulk keep list: one top-level name per line; blank lines
// and #-comments are ignored.
func readKeep(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	keep := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			keep[line] = true
		}
	}
	return keep, sc.Err()
}
