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

package writesonread

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSetAndDisabled(t *testing.T) {
	IsolateForTest(t)
	if Disabled() {
		t.Fatal("switch on after IsolateForTest")
	}
	Set()
	if !Disabled() {
		t.Fatal("switch off after Set")
	}
}

// TestIsolateForTest_ClearsAfterTest checks that a test which sets the
// switch does not hand it to the next one: IsolateForTest's cleanup turns
// it off when the subtest ends.
func TestIsolateForTest_ClearsAfterTest(t *testing.T) {
	t.Run("sets", func(t *testing.T) {
		IsolateForTest(t)
		Set()
	})
	if Disabled() {
		t.Fatal("the switch is still on after the subtest that set it ended")
	}
}

// TestIsolateForTest_NestedRestoresParent checks that a nested isolate
// restores the parent's value when the subtest ends.
func TestIsolateForTest_NestedRestoresParent(t *testing.T) {
	IsolateForTest(t)
	Set()
	t.Run("nested", func(t *testing.T) {
		IsolateForTest(t)
		if Disabled() {
			t.Fatal("the nested isolate did not turn the switch off")
		}
	})
	if !Disabled() {
		t.Fatal("the nested isolate cleared the parent's Set")
	}
}

// isolateCallRE matches a call of IsolateForTest, qualified or not.
var isolateCallRE = regexp.MustCompile(`\bIsolateForTest\(`)

// TestIsolateForTest_NoProductionCallers checks that only tests call
// IsolateForTest: no non-test Go file in the module calls it (its own file
// holds only the declaration), and in cmd only server_preflight_test.go
// does (runPreflightForTest and the root-hook test).
func TestIsolateForTest_NoProductionCallers(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	self, err := filepath.Abs("writesonread.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	var callers []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", ".scion":
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // another module (extras/*)
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n := len(isolateCallRE.FindAllIndex(b, -1))
		switch {
		case path == self:
			// Its own file: only the declaration.
			if n != 1 {
				callers = append(callers, fmt.Sprintf("%s (%d matches, want only the declaration)", path, n))
			}
		case strings.HasSuffix(path, "_test.go"):
			// Tests: in cmd, only through runPreflightForTest and the hook
			// test, both in server_preflight_test.go.
			rel, _ := filepath.Rel(root, path)
			if n > 0 && strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) && filepath.Base(path) != "server_preflight_test.go" {
				callers = append(callers, path+" (a cmd test outside server_preflight_test.go)")
			}
		default:
			scanned++
			if n > 0 {
				callers = append(callers, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files under %s; the walk is not looking at the module", scanned, root)
	}
	if len(callers) > 0 {
		t.Errorf("IsolateForTest is for tests only (in cmd, only through server_preflight_test.go), but these call it: %v", callers)
	}
}
