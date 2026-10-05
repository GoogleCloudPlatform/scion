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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// os.Getenv("SCION_SERVER_X") / os.LookupEnv("SCION_SERVER_X")
	getenvLiteralRE = regexp.MustCompile(`(?:Getenv|LookupEnv)\("(SCION_SERVER_[A-Z0-9_]+)"\)`)
	// const EnvX = "SCION_SERVER_X", read elsewhere via os.Getenv(EnvX)
	envConstRE = regexp.MustCompile(`(?m)^\s*(?:const\s+)?\w+\s*=\s*"(SCION_SERVER_[A-Z0-9_]+)"\s*$`)
	anyLiteral = regexp.MustCompile(`"(SCION_SERVER_[A-Z0-9_]+)"`)
)

// TestDirectServerEnvNames_MatchGetenvCallSites is the drift guard for
// directServerEnvNames: every SCION_SERVER_* name read with os.Getenv /
// os.LookupEnv (or declared as an env-name constant) in non-test Go code of
// this module must be on the list or otherwise matched by the detector, and
// every listed name must still appear in non-test code.
func TestDirectServerEnvNames_MatchGetenvCallSites(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}

	read := map[string]string{}  // name -> first file reading it
	literal := map[string]bool{} // every SCION_SERVER_ literal in non-test code
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, re := range []*regexp.Regexp{getenvLiteralRE, envConstRE} {
			for _, m := range re.FindAllSubmatch(data, -1) {
				if _, ok := read[string(m[1])]; !ok {
					read[string(m[1])] = rel
				}
			}
		}
		for _, m := range anyLiteral.FindAllSubmatch(data, -1) {
			literal[string(m[1])] = true
		}
		return nil
	}
	// The hub and broker are built from cmd/ and pkg/ (plus root files).
	for _, dir := range []string{"cmd", "pkg"} {
		if err := filepath.WalkDir(filepath.Join(root, dir), walk); err != nil {
			t.Fatal(err)
		}
	}
	if len(read) == 0 {
		t.Fatal("found no os.Getenv(\"SCION_SERVER_...\") call sites; the scan is broken")
	}

	noLayer1 := func(string) bool { return false }
	for name, file := range read {
		if directServerEnvNames[name] || serverEnvMatches(name, noLayer1) {
			continue
		}
		t.Errorf("%s is read directly in %s but is not in directServerEnvNames, so the startup warning flags it", name, file)
	}
	for name := range directServerEnvNames {
		if !literal[name] {
			t.Errorf("directServerEnvNames entry %s no longer appears in non-test code; remove it", name)
		}
	}
}
