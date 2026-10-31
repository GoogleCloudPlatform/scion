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
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const contentOutsideMarker = "template-content-outside-marker"

// A single-line file value that is not a relative path inside the template
// directory is refused by both entry points, the error carries neither the
// value nor the outside content, and nothing outside is read.
func TestTemplateContentRefusesNonLocalFileValues(t *testing.T) {
	base := t.TempDir()
	tplDir := filepath.Join(base, "templates", "tpl")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.md")
	if err := os.WriteFile(outside, []byte(contentOutsideMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	tpl := &Template{Name: "tpl", Path: tplDir}
	// A template whose directory is missing: a lookup there finds nothing,
	// so without the value check the value would become inline content.
	missing := &Template{Name: "missing", Path: filepath.Join(base, "templates", "missing")}
	values := []string{
		"../../outside.md",
		"a/../../../outside.md",
		outside, // absolute
		".",
		"a/..",
	}
	for _, v := range values {
		for entry, resolve := range map[string]func() ([]byte, error){
			"chain":    func() ([]byte, error) { return ResolveContentInChain([]*Template{tpl}, v) },
			"template": func() ([]byte, error) { return tpl.ResolveContent(v) },
			"missing":  func() ([]byte, error) { return ResolveContentInChain([]*Template{missing}, v) },
		} {
			data, err := resolve()
			if err == nil {
				t.Errorf("%s %q: accepted, got %q", entry, v, data)
				continue
			}
			if !errors.Is(err, ErrTemplateContentRefused) {
				t.Errorf("%s %q: error = %v, want ErrTemplateContentRefused", entry, v, err)
			}
			if !strings.Contains(err.Error(), "must be a relative path inside the template directory") {
				t.Errorf("%s %q: error %q is not the non-local value refusal", entry, v, err)
			}
			if strings.Contains(err.Error(), v) || strings.Contains(err.Error(), base) {
				t.Errorf("%s %q: error %q carries the value or a host path", entry, v, err)
			}
			if bytes.Contains(data, []byte(contentOutsideMarker)) {
				t.Errorf("%s %q: outside content returned", entry, v)
			}
		}
	}
}

// Controls: local and nested files, a parent template's file found from the
// child, inline single-line text with no such file, multi-line inline text
// that looks like a path (never looked up), and the empty value.
func TestTemplateContentLocalAndInlineStillWork(t *testing.T) {
	parent := t.TempDir()
	child := t.TempDir()
	write := func(dir, rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(parent, "agents.md", "parent instructions")
	write(child, "prompts/system.md", "nested prompt")
	chain := []*Template{{Name: "parent", Path: parent}, {Name: "child", Path: child}}

	cases := map[string]string{
		"agents.md":                     "parent instructions",
		"prompts/system.md":             "nested prompt",
		"You are a helpful assistant.":  "You are a helpful assistant.",
		"Work in /workspace\nnot ../up": "Work in /workspace\nnot ../up",
		"/start here\nthen continue":    "/start here\nthen continue",
	}
	for value, want := range cases {
		got, err := ResolveContentInChain(chain, value)
		if err != nil {
			t.Errorf("%q: %v", value, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%q = %q, want %q", value, got, want)
		}
	}
	got, err := chain[1].ResolveContent("prompts/system.md")
	if err != nil || string(got) != "nested prompt" {
		t.Errorf("Template.ResolveContent = %q, %v", got, err)
	}
	if got, err := ResolveContentInChain(chain, ""); err != nil || got != nil {
		t.Errorf("empty value = %q, %v; want nil, nil", got, err)
	}
	if got, err := ResolveContentInChain([]*Template{{Name: "gone", Path: filepath.Join(parent, "missing")}}, "agents.md"); err != nil || string(got) != "agents.md" {
		t.Errorf("missing template dir = %q, %v; want inline", got, err)
	}
}
