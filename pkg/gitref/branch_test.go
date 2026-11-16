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

package gitref_test

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/gitref"
)

const (
	hex40 = "0123456789abcdef0123456789abcdef01234567"
	hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestStdlibOnlyImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var prodFiles int
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		prodFiles++
		f, err := parser.ParseFile(fset, filepath.Join(".", ent.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(imp, ".") {
				t.Errorf("pkg/gitref/%s imports non-stdlib package %q", ent.Name(), imp)
			}
		}
	}
	if prodFiles == 0 {
		t.Fatal("expected production .go files in pkg/gitref")
	}
}

func TestValidBranch_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		reason string
	}{
		{"empty", "", "it is empty"},
		{"too long", strings.Repeat("a", 201), "longer than 200 characters"},
		{"leading dash", "-x", "does not start with a letter or digit"},
		{"upload pack flag", "--upload-pack=x", "does not start with a letter or digit"},
		{"leading dot", ".x", "does not start with a letter or digit"},
		{"dot dot leading", "../x", "contains '..'"},
		{"leading slash", "/x", "does not start with a letter or digit"},
		{"dot dot", "a..b", "contains '..'"},
		{"dot dot component", "x/../y", "contains '..'"},
		{"at brace", "a@{1}", "contains '@{'"},
		{"at brace leading", "@{-1}", "contains '@{'"},
		{"double slash", "a//b", "contains '//'"},
		{"space", "a b", "a character other than"},
		{"tilde", "a~1", "a character other than"},
		{"colon", "a:b", "a character other than"},
		{"non-ascii", "brånch", "a character other than"},
		{"trailing slash", "a/", "ends with '/', '.' or '.lock'"},
		{"trailing dot", "a.", "ends with '/', '.' or '.lock'"},
		{"trailing lock", "x.lock", "ends with '/', '.' or '.lock'"},
		{"component starts with dot", "a/.b", "a path component starts with '.' or ends with '.lock'"},
		{"component ends with lock", "a.lock/b", "a path component starts with '.' or ends with '.lock'"},
		{"HEAD", "HEAD", "'HEAD' is reserved"},
		{"40 hex", hex40, "ambiguous with a commit"},
		{"64 hex", hex64, "ambiguous with a commit"},
		{"40 upper hex", strings.ToUpper(hex40), "ambiguous with a commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gitref.ValidBranch(tc.branch)
			if err == nil {
				t.Fatalf("ValidBranch(%q) = nil, want an error", tc.branch)
			}
			if !errors.Is(err, gitref.ErrInvalidBranch) {
				t.Errorf("error %q does not wrap ErrInvalidBranch", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("ValidBranch(%q) = %q, want the reason %q", tc.branch, err, tc.reason)
			}
			if !strings.HasSuffix(err.Error(), "; "+gitref.BranchRule) {
				t.Errorf("error %q does not end with the allowed form", err)
			}
		})
	}
}

func TestValidBranch_Accepts(t *testing.T) {
	for _, b := range []string{
		"main", "release/1.2", "feature_x-y", "a", "A1", "v1.2.3",
		"user/topic/sub", "head", "detached", "Detached", "detached-x", "x.locks",
		strings.Repeat("a", 200),
		hex40[:39], hex40 + "0",
	} {
		if err := gitref.ValidBranch(b); err != nil {
			t.Errorf("ValidBranch(%q) = %v, want nil", b, err)
		}
	}
}

func TestValidBranch_ErrorDoesNotEchoTheName(t *testing.T) {
	const name = "evil name<script>"
	err := gitref.ValidBranch(name)
	if err == nil || strings.Contains(err.Error(), name) {
		t.Fatalf("ValidBranch(%q) = %v; want an error that does not quote the name", name, err)
	}
}

func TestBranchRule_NamesEveryRule(t *testing.T) {
	for _, rule := range []string{
		"1-200 characters",
		"letters, digits, '.', '_', '/' and '-'",
		"start with a letter or digit",
		"'..'",
		"'@{'",
		"'//'",
		"not end with '/', '.' or '.lock'",
		"path component that starts with '.' or ends with '.lock'",
		"'HEAD'",
		"40- or 64-character hex string",
	} {
		if !strings.Contains(gitref.BranchRule, rule) {
			t.Errorf("BranchRule does not name the rule %q:\n%s", rule, gitref.BranchRule)
		}
	}
}
