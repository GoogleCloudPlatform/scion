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

package areadirs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	got, err := Parse(strings.NewReader("# comment\n\narea  maintenance\n  testsupport\tinternal/hubtestutil  \n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Area, "maintenance"}, {TestSupport, "internal/hubtestutil"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
	if d := Dirs(got); !reflect.DeepEqual(d, []string{"maintenance", "internal/hubtestutil"}) {
		t.Fatalf("Dirs = %v", d)
	}
}

func TestParseRejects(t *testing.T) {
	for name, src := range map[string]string{
		"empty":         "# only a comment\n\n",
		"one field":     "area\n",
		"extra field":   "area maintenance apierr\n",
		"unknown kind":  "leaf apierr\n",
		"absolute":      "area /pkg/hub/x\n",
		"dot":           "area .\n",
		"parent":        "area ../store\n",
		"unclean":       "area authz//kernel\n",
		"trailing sep":  "area authz/\n",
		"backslash":     `area authz\kernel` + "\n",
		"duplicate dir": "area maintenance\ntestsupport maintenance\n",
	} {
		if got, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("%s: Parse(%q) = %v, want an error", name, src, got)
		}
	}
}

// TestLoadRepoList checks the checked-in list: it parses, and every listed
// directory exists under pkg/hub, so a stale entry cannot make the scanner
// self-tests plant into a directory that no longer exists.
func TestLoadRepoList(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(root, "pkg", "hub", filepath.FromSlash(e.Dir)))
		if err != nil || !info.IsDir() {
			t.Errorf("%s entry %s: not a directory under pkg/hub (%v)", e.Kind, e.Dir, err)
		}
	}
}
