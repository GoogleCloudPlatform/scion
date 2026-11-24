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
	"strings"
	"testing"
)

func TestSplitCommonFiles(t *testing.T) {
	// A non-helper common file with a shared helper, a private store.Store
	// wrapper, a private Test* function, a blank-identifier assertion, and a
	// cross-file-referenced Test* function: only the unreferenced private
	// declarations move to the generated split file, with //line directives
	// and pruned imports.
	tree := map[string]string{
		"main_test.go": header + `import "testing"

func TestMain(m *testing.M) {
	sharedHelper()
	_ = TestReferenced
	m.Run()
}
`,
		"shared_test.go": "//go:build !no_sqlite\n\n" + header + `import (
	"fmt"
	"strings"
	"testing"
	"example.com/store"
)

var _ = strings.TrimSpace

func sharedHelper() string {
	return sharedInternal()
}

func sharedInternal() string {
	return strings.TrimSpace("ok")
}

func TestReferenced(t *testing.T) {
	t.Log("referenced by TestMain")
}

type privateFakeStore struct {
	store.Store
}

func (p *privateFakeStore) Custom() string {
	return fmt.Sprintf("custom")
}

func TestPrivateOnly(t *testing.T) {
	var f privateFakeStore
	if f.Custom() == "" {
		t.Fatal("empty")
	}
}
`,
		"other_test.go": header + `import "testing"

func TestOther(t *testing.T) {}
`,
	}

	root, dir := writeFiles(t, tree)
	args := baseArgs(root, dir, "-n", "2")
	files, _ := overlayFiles(t, args)

	commonSrc, ok := files["shared_test.go"]
	if !ok {
		t.Fatalf("expected trimmed shared_test.go in overlay; got %v", sortedKeys(files))
	}
	splitName := splitPrefix + "shared_test.go"
	splitSrc, ok := files[splitName]
	if !ok {
		t.Fatalf("expected %s in overlay; got %v", splitName, sortedKeys(files))
	}

	// Common piece keeps shared helpers, blank var, and referenced Test*,
	// drops privateFakeStore, TestPrivateOnly, and unused imports (fmt, store).
	for _, want := range []string{"//go:build !no_sqlite", "func sharedHelper()", "func sharedInternal()", "func TestReferenced(", "var _ = strings.TrimSpace", `"strings"`, `"testing"`, "//line shared_test.go:"} {
		if !strings.Contains(commonSrc, want) {
			t.Errorf("common piece missing %q:\n%s", want, commonSrc)
		}
	}
	for _, forbid := range []string{"privateFakeStore", "TestPrivateOnly", `"fmt"`, `"example.com/store"`, "!hubshard"} {
		if strings.Contains(commonSrc, forbid) {
			t.Errorf("common piece should not contain %q:\n%s", forbid, commonSrc)
		}
	}

	// Split piece gets privateFakeStore + method + TestPrivateOnly + its
	// imports (fmt, testing, store), tagged with !no_sqlite && (!hubshard || hubshard_K),
	// and drops strings and shared helpers.
	for _, want := range []string{"//go:build !no_sqlite && (!hubshard || hubshard_", "type privateFakeStore struct", "func (p *privateFakeStore) Custom()", "func TestPrivateOnly(", `"fmt"`, `"testing"`, `"example.com/store"`, "//line shared_test.go:"} {
		if !strings.Contains(splitSrc, want) {
			t.Errorf("split piece missing %q:\n%s", want, splitSrc)
		}
	}
	for _, forbid := range []string{"func sharedHelper()", "func TestReferenced(", `"strings"`} {
		if strings.Contains(splitSrc, forbid) {
			t.Errorf("split piece should not contain %q:\n%s", forbid, splitSrc)
		}
	}

	// Parity, -locate, and -skip must all pass on the split tree.
	var out bytes.Buffer
	if err := run(append(args, "-parity"), &out); err != nil {
		t.Fatalf("-parity failed:\n%s\nerr: %v", out.String(), err)
	}
	out.Reset()
	if err := run(append(args, "-locate", "TestPrivateOnly", "TestReferenced"), &out); err != nil {
		t.Fatalf("-locate failed: %v", err)
	}
	if !strings.Contains(out.String(), "TestPrivateOnly ") || !strings.Contains(out.String(), "TestReferenced 1\n") {
		t.Errorf("unexpected -locate output:\n%s", out.String())
	}
	// Shard 2's skip pattern must include TestReferenced (common) and NOT
	// TestPrivateOnly (sharded).
	out.Reset()
	if err := run(append(args, "-skip", "2"), &out); err != nil {
		t.Fatalf("-skip failed: %v", err)
	}
	if !strings.Contains(out.String(), "TestReferenced") || strings.Contains(out.String(), "TestPrivateOnly") {
		t.Errorf("unexpected -skip 2 output: %q", out.String())
	}
}

func TestStoreEmbedWeightBalancing(t *testing.T) {
	// Two files with store.Store embeds (tiny line count) plus several
	// ordinary files: the two embed-heavy files must land in different shards.
	tree := map[string]string{
		"embed1_test.go": header + "import \"example.com/store\"\ntype e1 struct { store.Store }\n",
		"embed2_test.go": header + "import \"example.com/store\"\ntype e2 struct { store.Store }\n",
		"plain1_test.go": header + strings.Repeat("// line\n", 500) + "func TestP1() {}\n",
		"plain2_test.go": header + strings.Repeat("// line\n", 500) + "func TestP2() {}\n",
	}
	root, dir := writeFiles(t, tree)
	files, _ := overlayFiles(t, baseArgs(root, dir, "-n", "2"))
	as := shardOfOverlay(t, files)
	if as["embed1_test.go"] == as["embed2_test.go"] {
		t.Fatalf("embed1 and embed2 landed in the same shard %d; want balanced across shards", as["embed1_test.go"])
	}
}
