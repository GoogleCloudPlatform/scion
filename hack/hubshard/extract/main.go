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
//	extract verify -before /tmp/hub-main -after pkg/hub
//
// See README.md for the rules a move follows.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/build/constraint"
	"os"
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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "extract:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: extract move -dir DIR -family FILE [-n]\n       extract verify -before DIR -after DIR")
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
