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

package dag

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	d, err := Parse(strings.NewReader(`# header
leaf        apierr
leaf        internal/hubdeps   authz/authzkernel   # trailing comment

area        authz/authzkernel
area        maintenance
testsupport internal/hubtestutil
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Kind: Leaf, Rel: "apierr", Areas: []string{}, Line: 2},
		{Kind: Leaf, Rel: "internal/hubdeps", Areas: []string{"authz/authzkernel"}, Line: 3},
		{Kind: Area, Rel: "authz/authzkernel", Areas: []string{}, Line: 5},
		{Kind: Area, Rel: "maintenance", Areas: []string{}, Line: 6},
		{Kind: TestSupport, Rel: "internal/hubtestutil", Areas: []string{}, Line: 7},
	}
	if !reflect.DeepEqual(d.Entries, want) {
		t.Fatalf("entries:\n got %+v\nwant %+v", d.Entries, want)
	}
	if got := d.Rels(Area, TestSupport); !reflect.DeepEqual(got, []string{"authz/authzkernel", "internal/hubtestutil", "maintenance"}) {
		t.Errorf("Rels(area, testsupport) = %q", got)
	}
	if got := d.Rels(); len(got) != 5 {
		t.Errorf("Rels() = %q, want all 5", got)
	}
	if d.Count(Leaf) != 2 || d.Count(Area) != 2 || d.Count(TestSupport) != 1 {
		t.Errorf("counts leaf %d area %d testsupport %d", d.Count(Leaf), d.Count(Area), d.Count(TestSupport))
	}
	if e, ok := d.Lookup("maintenance"); !ok || e.Kind != Area {
		t.Errorf("Lookup(maintenance) = %+v, %v", e, ok)
	}
	if _, ok := d.Lookup("nope"); ok {
		t.Error("Lookup(nope) found an entry")
	}
}

func TestParseEmpty(t *testing.T) {
	d, err := Parse(strings.NewReader("# only comments\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Entries) != 0 {
		t.Fatalf("entries = %+v", d.Entries)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"one field", "leaf\n", "line 1: want"},
		{"unknown kind", "feature authzop\n", `unknown kind "feature"`},
		{"absolute", "leaf /abs\n", "must be a clean"},
		{"dot", "leaf .\n", "must be a clean"},
		{"dotdot", "leaf ../x\n", "must be a clean"},
		{"unclean", "leaf a//b\n", "must be a clean"},
		{"trailing slash", "leaf a/\n", "must be a clean"},
		{"backslash", "leaf a\\b\n", "must be a clean"},
		{"bad listed path", "area a ../b\n", "must be a clean"},
		{"duplicate", "leaf a\narea a\n", "line 2: a is already declared on line 1"},
		{"lists itself", "area a a\n", "a lists itself"},
		{"lists twice", "area a b b\narea b\n", "a lists b twice"},
		{"lists undeclared", "area a b\n", "a lists b, which is not declared"},
		{"lists leaf", "area a b\nleaf b\n", "a lists b, which is a leaf"},
		{"lists testsupport", "area a b\ntestsupport b\n", "a lists b, which is a testsupport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestReadFileMissing(t *testing.T) {
	if _, err := ReadFile("testdata-does-not-exist.txt"); err == nil {
		t.Fatal("no error for a missing file")
	}
}
