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

package util

import "testing"

func varKeyValue(vars []gitConfigVar) map[string]string {
	m := map[string]string{}
	for _, v := range vars {
		m[v.Key()] = v.Value
	}
	return m
}

func TestParseGitConfigFile(t *testing.T) {
	src := "" +
		"# a comment\n" +
		"[core]\n" +
		"\trepositoryformatversion = 0\n" +
		"\tbare = false ; inline comment\n" +
		"[remote \"origin\"]\n" +
		"\turl = https://github.com/org/repo.git\n" +
		"[filter \"a=b\"]\n" +
		"\tsmudge = touch x\n" +
		"[branch \"feature/x\"] merge = refs/heads/feature/x\n" +
		"[user]\n" +
		"\tname = \"Jane  Doe\"\n" +
		"\tquote = a\\\"b\n" +
		"\tcont = one\\\n" +
		"two\n"
	vars, err := parseGitConfigFile([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := varKeyValue(vars)
	want := map[string]string{
		"core.repositoryformatversion": "0",
		"core.bare":                    "false",
		"remote.origin.url":            "https://github.com/org/repo.git",
		"filter.a=b.smudge":            "touch x",
		"branch.feature/x.merge":       "refs/heads/feature/x",
		"user.name":                    "Jane  Doe",
		"user.quote":                   "a\"b",
		"user.cont":                    "onetwo",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(vars) != len(want) {
		t.Errorf("parsed %d vars, want %d: %#v", len(vars), len(want), got)
	}
}

// A subsection containing '=' keeps the '=' in the subsection, so the filter
// driver is checked like any other key.
func TestParseGitConfigFile_SubsectionWithEquals(t *testing.T) {
	vars, err := parseGitConfigFile([]byte("[filter \"a=b\"]\n\tsmudge = evil\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 1 || vars[0].Section != "filter" || vars[0].Subsection != "a=b" || vars[0].Name != "smudge" {
		t.Errorf("parsed %#v", vars)
	}
}

func TestParseGitConfigFile_Errors(t *testing.T) {
	for _, src := range []string{
		"name = value\n",           // variable before any section
		"[core\n",                  // unterminated header
		"[core \"x\n",              // unterminated subsection
		"[core]\n\tkey = \"open\n", // unterminated quote
		"[core]\n\tkey = a\\q\n",   // invalid escape
		"[core]\n\t= value\n",      // missing name
		"[core]\n\tkey value\n",    // missing '='
	} {
		if _, err := parseGitConfigFile([]byte(src)); err == nil {
			t.Errorf("parse(%q) = nil error, want an error (fail closed)", src)
		}
	}
}

// Boolean variables (no value) default to "true".
func TestParseGitConfigFile_Boolean(t *testing.T) {
	vars, err := parseGitConfigFile([]byte("[core]\n\tbare\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 1 || vars[0].Key() != "core.bare" || vars[0].Value != "true" {
		t.Errorf("parsed %#v", vars)
	}
}

// Legacy [section.subsection] lower-cases the subsection; quoted keeps case.
func TestParseGitConfigFile_SubsectionCase(t *testing.T) {
	vars, err := parseGitConfigFile([]byte("[branch.Feature]\n\tx = 1\n[branch \"Feature\"]\n\ty = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if vars[0].Subsection != "feature" {
		t.Errorf("legacy subsection = %q, want lower-cased", vars[0].Subsection)
	}
	if vars[1].Subsection != "Feature" {
		t.Errorf("quoted subsection = %q, want case kept", vars[1].Subsection)
	}
}

// An empty quoted subsection [x ""] is distinct from [x].
func TestParseGitConfigFile_EmptyQuotedSubsection(t *testing.T) {
	vars, err := parseGitConfigFile([]byte("[x \"\"]\n\ty = 1\n[x]\n\ty = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 {
		t.Fatalf("parsed %d vars, want 2: %#v", len(vars), vars)
	}
	if !vars[0].HasSubsection || vars[0].Subsection != "" || vars[0].Key() != "x..y" {
		t.Errorf("[x \"\"] y: HasSubsection=%v sub=%q key=%q, want true/\"\"/x..y", vars[0].HasSubsection, vars[0].Subsection, vars[0].Key())
	}
	if vars[1].HasSubsection || vars[1].Key() != "x.y" {
		t.Errorf("[x] y: HasSubsection=%v key=%q, want false/x.y", vars[1].HasSubsection, vars[1].Key())
	}
}
