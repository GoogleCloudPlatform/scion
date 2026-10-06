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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The typed not-found errors (ptone/scion#3113) keep matching their
// sentinels, keep Error() byte-identical to the previous fmt.Errorf text,
// and carry a Name with no broker path or content hash.

func TestTemplateNotFoundError_FromLookup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectPath := t.TempDir()

	t.Run("plain name", func(t *testing.T) {
		_, err := FindTemplateInProjectPath("missing-tpl", projectPath)
		var tplErr *TemplateNotFoundError
		if !errors.As(err, &tplErr) {
			t.Fatalf("error %v (%T) is not a *TemplateNotFoundError", err, err)
		}
		if !errors.Is(err, ErrTemplateNotFound) {
			t.Errorf("errors.Is(err, ErrTemplateNotFound) = false for %v", err)
		}
		if tplErr.Name != "missing-tpl" {
			t.Errorf("Name = %q, want missing-tpl", tplErr.Name)
		}
		if want := fmt.Sprintf("template %s not found: %v", "missing-tpl", ErrTemplateNotFound); err.Error() != want {
			t.Errorf("Error() = %q, want the unchanged text %q", err.Error(), want)
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "broker-only", "secret-tpl")
		_, err := FindTemplateInProjectPath(abs, projectPath)
		var tplErr *TemplateNotFoundError
		if !errors.As(err, &tplErr) || !errors.Is(err, ErrTemplateNotFound) {
			t.Fatalf("error %v does not match the typed error and the sentinel", err)
		}
		if tplErr.Name != "secret-tpl" {
			t.Errorf("Name = %q, want the base name secret-tpl", tplErr.Name)
		}
		if strings.Contains(tplErr.Name, "broker-only") || strings.Contains(tplErr.Name, string(filepath.Separator)) {
			t.Errorf("Name %q carries a path", tplErr.Name)
		}
		if want := fmt.Sprintf("template path %s not found or not a directory: %v", abs, ErrTemplateNotFound); err.Error() != want {
			t.Errorf("Error() = %q, want the unchanged text %q", err.Error(), want)
		}
	})

	t.Run("content-hash cache directory", func(t *testing.T) {
		hash := "sha256:" + strings.Repeat("ab", 32)
		abs := filepath.Join(t.TempDir(), "cache", hash)
		_, err := FindTemplateInProjectPath(abs, projectPath)
		var tplErr *TemplateNotFoundError
		if !errors.As(err, &tplErr) {
			t.Fatalf("error %v is not a *TemplateNotFoundError", err)
		}
		if tplErr.Name != "" {
			t.Errorf("Name = %q, want empty: a content hash is not a template name", tplErr.Name)
		}
	})

	t.Run("wrapped", func(t *testing.T) {
		_, inner := FindTemplateInProjectPath("missing-tpl", projectPath)
		err := fmt.Errorf("failed to load template: %w", inner)
		var tplErr *TemplateNotFoundError
		if !errors.As(err, &tplErr) || !errors.Is(err, ErrTemplateNotFound) || tplErr.Name != "missing-tpl" {
			t.Errorf("wrapped error lost the typed error or sentinel: %v", err)
		}
	})
}

func TestHarnessConfigNotFoundError_FromLookup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectPath := t.TempDir()

	_, err := FindHarnessConfigDir("missing-hc", projectPath)
	var hcErr *HarnessConfigNotFoundError
	if !errors.As(err, &hcErr) {
		t.Fatalf("error %v (%T) is not a *HarnessConfigNotFoundError", err, err)
	}
	if !errors.Is(err, ErrHarnessConfigNotFound) {
		t.Errorf("errors.Is(err, ErrHarnessConfigNotFound) = false for %v", err)
	}
	if hcErr.Name != "missing-hc" {
		t.Errorf("Name = %q, want missing-hc", hcErr.Name)
	}
	if strings.Contains(hcErr.Name, projectPath) {
		t.Errorf("Name %q carries a searched path", hcErr.Name)
	}
	want := fmt.Sprintf("harness-config %q not found (searched: %s): %v", "missing-hc", strings.Join(hcErr.Searched, ", "), ErrHarnessConfigNotFound)
	if err.Error() != want {
		t.Errorf("Error() = %q, want the unchanged text %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), filepath.Join(projectPath, harnessConfigsDirName, "missing-hc")) {
		t.Errorf("Error() lost the searched project path (diagnostics): %s", err.Error())
	}
}

// TestFriendlyTemplateName_NoHashQueryOrUserinfo: a bare 64-hex digest is
// treated like a prefixed content hash, and a URL reference never keeps its
// query string or userinfo (ptone/scion#3113 review N1/N2).
func TestFriendlyTemplateName_NoHashQueryOrUserinfo(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	cases := []struct {
		ref  string
		want string
	}{
		{hex, ""},
		{"/var/cache/scion/templates/" + hex, ""},
		{"/var/cache/scion/templates/sha256:" + hex, ""},
		{"https://host.example/a/b.tgz?token=s3cr3t", "b"},
		{"https://host.example/a/b?sig=x", "b"},
		{"https://user:pw@host.example", "host.example"},
		{"https://user:pw@host.example/a/tpl.zip?sig=x#frag", "tpl"},
		{"https://github.com/user/repo/tree/main/templates/claude?token=s3cr3t", "claude"},
		{"https://example.com/my-template.tar.gz", "my-template"},
		{"claude", "claude"},
		// Percent-encoding is kept, never decoded (review round 2).
		{"https://example.com/a%20b", "a%20b"},
		{"https://example.com/a%2Fb", "a%2Fb"},
		{"https://example.com/a%0Ab", "a%0Ab"},
		// A dot element is not a template name.
		{"https://example.com/a/b/..", "remote"},
		{"https://example.com/a/b/.", "remote"},
	}
	for _, c := range cases {
		got := FriendlyTemplateName(c.ref)
		if got != c.want {
			t.Errorf("FriendlyTemplateName(%q) = %q, want %q", c.ref, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r ") {
			t.Errorf("FriendlyTemplateName(%q) = %q contains a decoded space or line break", c.ref, got)
		}
		for _, secret := range []string{"s3cr3t", "sig", "token", "user", "pw@", hex} {
			if strings.Contains(got, secret) {
				t.Errorf("FriendlyTemplateName(%q) = %q leaks %q", c.ref, got, secret)
			}
		}
	}

	// Through the typed error, as the broker sees it.
	if n := NewTemplateNotFoundError("/var/cache/scion/templates/"+hex, "x").Name; n != "" {
		t.Errorf("TemplateNotFoundError.Name for a bare-hex cache dir = %q, want empty", n)
	}
}
