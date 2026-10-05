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

package config

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// completeServerEnvName matches a string literal that is exactly one
// SCION_SERVER_* variable name (not the bare prefix, not prose).
var completeServerEnvName = regexp.MustCompile(`^SCION_SERVER_[A-Z0-9_]+$`)

// serverEnvLiterals returns every string literal in non-test Go code under
// cmd/ and pkg/ that is exactly a SCION_SERVER_* name, mapped to the first
// file using it. It tokenizes the source, so comments are ignored and every
// declaration or call form counts: os.Getenv("X"), parseBoolEnv("X"),
// const/var/typed/:= assignments with or without trailing comments. The
// detector's own tables (unmatched_env.go) are skipped.
func serverEnvLiterals(t *testing.T) map[string]string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	self := filepath.Join(root, "pkg", "config", "unmatched_env.go")

	out := map[string]string{}
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == self {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fset := token.NewFileSet()
		var s scanner.Scanner
		s.Init(fset.AddFile(path, -1, len(src)), src, nil, 0)
		for {
			_, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit)
			if err != nil || !completeServerEnvName.MatchString(v) {
				continue
			}
			if _, ok := out[v]; !ok {
				out[v] = rel
			}
		}
		return nil
	}
	// The hub and broker are built from cmd/ and pkg/.
	for _, dir := range []string{"cmd", "pkg"} {
		if err := filepath.WalkDir(filepath.Join(root, dir), walk); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestDirectServerEnvNames_MatchGetenvCallSites is the drift guard for
// directServerEnvNames. Every complete SCION_SERVER_* string literal in
// non-test code (an os.Getenv argument, an env-name constant, a helper-call
// argument, ...) must be on the list or otherwise accepted by the detector,
// so the startup warning never flags a name the code reads directly. Every
// listed name must still appear as such a literal outside the detector.
func TestDirectServerEnvNames_MatchGetenvCallSites(t *testing.T) {
	read := serverEnvLiterals(t)
	if len(read) == 0 {
		t.Fatal(`found no "SCION_SERVER_..." literals in cmd/ or pkg/; the scan is broken`)
	}

	noLayer1 := func(string) bool { return false }
	for name, file := range read {
		if directServerEnvNames[name] || serverEnvMatches(name, noLayer1) {
			continue
		}
		t.Errorf("%s appears in %s but is not in directServerEnvNames, so the startup warning flags it", name, file)
	}
	for name := range directServerEnvNames {
		if _, ok := read[name]; !ok {
			t.Errorf("directServerEnvNames entry %s no longer appears in non-test code; remove it", name)
		}
	}
}
