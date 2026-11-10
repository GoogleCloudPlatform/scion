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

package rebuildpreflight

import (
	"errors"
	"strings"
	"testing"
)

const (
	hex40 = "0123456789abcdef0123456789abcdef01234567"
	hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// Every rejected name fails for the rule it breaks: the error names that
// rule, so a check that is removed shows even where a later rule would
// still reject the name for another reason.
func TestValidBranch_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		reason string
	}{
		{"empty", "", "it is empty"},
		{"too long", strings.Repeat("a", 201), "longer than 200 characters"},
		{"leading dash", "-x", "does not start with a letter or digit"},
		{"leading dot", ".x", "does not start with a letter or digit"},
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
			err := ValidBranch(tc.branch)
			if err == nil {
				t.Fatalf("ValidBranch(%q) = nil, want an error", tc.branch)
			}
			if !errors.Is(err, ErrInvalidBranch) {
				t.Errorf("error %q does not wrap ErrInvalidBranch", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("ValidBranch(%q) = %q, want the reason %q", tc.branch, err, tc.reason)
			}
			if !strings.HasSuffix(err.Error(), "; "+BranchRule) {
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
		hex40[:39], hex40 + "0", // not full-length object names
	} {
		if err := ValidBranch(b); err != nil {
			t.Errorf("ValidBranch(%q) = %v, want nil", b, err)
		}
	}
}

// The error never quotes the rejected name: it may be any client text.
func TestValidBranch_ErrorDoesNotEchoTheName(t *testing.T) {
	const name = "evil name<script>"
	err := ValidBranch(name)
	if err == nil || strings.Contains(err.Error(), name) {
		t.Fatalf("ValidBranch(%q) = %v; want an error that does not quote the name", name, err)
	}
}

// BranchRule, the text of the 400, names every rule ValidBranch checks.
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
		if !strings.Contains(BranchRule, rule) {
			t.Errorf("BranchRule does not name the rule %q:\n%s", rule, BranchRule)
		}
	}
}

func TestValidCommit(t *testing.T) {
	for _, sha := range []string{hex40, hex64} {
		if err := ValidCommit(sha); err != nil {
			t.Errorf("ValidCommit(%q) = %v, want nil", sha, err)
		}
	}
	for _, sha := range []string{
		"",
		hex40[:39],
		hex40 + "0",
		hex64[:63],
		hex64 + "0",
		strings.ToUpper(hex40),
		"g" + hex40[1:],
		"-" + hex40[1:],
		"main",
	} {
		err := ValidCommit(sha)
		if err == nil {
			t.Errorf("ValidCommit(%q) = nil, want an error", sha)
			continue
		}
		if !errors.Is(err, ErrInvalidCommit) {
			t.Errorf("ValidCommit(%q) error %q does not wrap ErrInvalidCommit", sha, err)
		}
	}
}
