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

package harnesses

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// minNodeTestMajor is the first Node release with the built-in test runner
// (node --test) that the harness JS tests use.
const minNodeTestMajor = 18

// TestHarnessNodeUnit runs every *.test.mjs file under harnesses/ (for
// example the opencode scion-bridge plugin test) with Node's built-in test
// runner, from the test file's own directory.
func TestHarnessNodeUnit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH; skipping harness JS unit tests")
	}
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Skipf("node --version failed (%v); skipping harness JS unit tests", err)
	}
	version := strings.TrimSpace(string(out))
	major, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0])
	if err != nil || major < minNodeTestMajor {
		t.Skipf("node %s lacks the built-in test runner (need >= %d); skipping harness JS unit tests", version, minNodeTestMajor)
	}

	var tests []string
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".test.mjs") {
			tests = append(tests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk harnesses: %v", err)
	}
	if len(tests) == 0 {
		t.Fatal("no harnesses/**/*.test.mjs files found")
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			cmd := exec.Command(node, "--test", filepath.Base(path))
			cmd.Dir = filepath.Dir(path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("node --test %s failed:\n%s", path, out)
			}
			t.Logf("node --test output:\n%s", out)
		})
	}
}
