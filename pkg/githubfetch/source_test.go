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

package githubfetch

import (
	"errors"
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	valid := []struct {
		name      string
		raw       string
		wantOwner string
		wantRepo  string
		wantRef   string
		wantPath  string
		wantAmb   []string
	}{
		{
			name:      "repo root empty ref",
			raw:       "https://github.com/acme/repo",
			wantOwner: "acme",
			wantRepo:  "repo",
		},
		{
			name:      "repo root with .git suffix",
			raw:       "https://github.com/acme/repo.git",
			wantOwner: "acme",
			wantRepo:  "repo",
		},
		{
			name:      "single segment branch",
			raw:       "https://github.com/acme/repo/tree/main",
			wantOwner: "acme",
			wantRepo:  "repo",
			wantRef:   "main",
		},
		{
			name:      "branch plus path is ambiguous",
			raw:       "https://github.com/acme/repo/tree/main/templates/foo",
			wantOwner: "acme",
			wantRepo:  "repo",
			wantRef:   "main",
			wantPath:  "templates/foo",
			wantAmb:   []string{"main", "templates", "foo"},
		},
		{
			name:      "slashed branch plus path is ambiguous",
			raw:       "https://github.com/acme/repo/tree/feat/x/y/templates",
			wantOwner: "acme",
			wantRepo:  "repo",
			wantRef:   "feat",
			wantPath:  "x/y/templates",
			wantAmb:   []string{"feat", "x", "y", "templates"},
		},
		{
			name:      "full 40-hex SHA plus path is unambiguous",
			raw:       "https://github.com/acme/repo/tree/" + sha + "/templates/foo",
			wantOwner: "acme",
			wantRepo:  "repo",
			wantRef:   sha,
			wantPath:  "templates/foo",
		},
		{
			name:      "explicit port 443",
			raw:       "https://github.com:443/acme/repo/tree/v1.2.3",
			wantOwner: "acme",
			wantRepo:  "repo",
			wantRef:   "v1.2.3",
		},
	}

	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseURL(%q) error = %v", tc.raw, err)
			}
			if got.Owner != tc.wantOwner || got.Repo != tc.wantRepo || got.Ref != tc.wantRef || got.Path != tc.wantPath {
				t.Fatalf("ParseURL(%q) = %+v, want owner=%q repo=%q ref=%q path=%q",
					tc.raw, got, tc.wantOwner, tc.wantRepo, tc.wantRef, tc.wantPath)
			}
			if len(got.RefAmbiguous) != len(tc.wantAmb) {
				t.Fatalf("RefAmbiguous = %v, want %v", got.RefAmbiguous, tc.wantAmb)
			}
			for i := range tc.wantAmb {
				if got.RefAmbiguous[i] != tc.wantAmb[i] {
					t.Fatalf("RefAmbiguous[%d] = %q, want %q", i, got.RefAmbiguous[i], tc.wantAmb[i])
				}
			}
		})
	}

	invalid := []string{
		"http://github.com/acme/repo/tree/main/t",
		"https://user:secret_tok@github.com/acme/repo/tree/main/t",
		"https://secret_tok@github.com/acme/repo/tree/main/t",
		"https://example.com/acme/repo/tree/main/t",
		"https://other.github.com/acme/repo/tree/main/t",
		"https://github.com.example.com/acme/repo/tree/main/t",
		"https://github.com:8443/acme/repo/tree/main/t",
		"https://github.com/acme/repo/archive/refs/heads/main.tar.gz",
		"https://github.com/acme/repo/blob/main/README.md",
		"https://github.com/acme/repo/releases/tag/v1",
		"https://github.com/acme/repo/tree/main/t?token=secret_tok",
		"https://github.com/acme/repo/tree/main/t#frag",
		"https://github.com/acme/repo/tree/main/../../etc/passwd",
		"https://github.com/acme/repo/tree",
		"https://github.com/acme",
		"https://github.com/acme/repo/not-tree/main",
	}

	for _, raw := range invalid {
		t.Run("invalid/"+raw, func(t *testing.T) {
			_, err := ParseURL(raw)
			if err == nil {
				t.Fatalf("ParseURL(%q) succeeded, want error", raw)
			}
			var ghe *Error
			if !errors.As(err, &ghe) || ghe.Kind != KindInvalidSource {
				t.Fatalf("ParseURL(%q) error = %v, want KindInvalidSource", raw, err)
			}
			if strings.Contains(err.Error(), "secret_tok") || strings.Contains(err.Error(), "example.com") {
				t.Fatalf("ParseURL(%q) leaked input in error: %q", raw, err.Error())
			}
		})
	}
}

func TestEndpointAndCacheKeys(t *testing.T) {
	ep := DefaultEndpoint()
	hosts := ep.AllowedHosts()
	wantHosts := []string{"api.github.com", "github.com", "codeload.github.com", "raw.githubusercontent.com"}
	if len(hosts) != len(wantHosts) {
		t.Fatalf("AllowedHosts = %v, want %v", hosts, wantHosts)
	}
	for i := range wantHosts {
		if hosts[i] != wantHosts[i] {
			t.Errorf("AllowedHosts[%d] = %q, want %q", i, hosts[i], wantHosts[i])
		}
	}

	const sha = "0123456789abcdef0123456789abcdef01234567"
	src := Source{Owner: "Acme", Repo: "Repo", Path: "templates/MyApp"}
	if got := ep.RawURL(src, sha, "hooks/start.sh"); got != "https://raw.githubusercontent.com/Acme/Repo/"+sha+"/templates/MyApp/hooks/start.sh" {
		t.Errorf("RawURL = %q", got)
	}
	if got := ep.TarballURL(src, sha); got != "https://api.github.com/repos/Acme/Repo/tarball/"+sha {
		t.Errorf("TarballURL = %q", got)
	}
	if got := ep.ArchiveURL(src, sha); got != "https://github.com/Acme/Repo/archive/"+sha+".tar.gz" {
		t.Errorf("ArchiveURL = %q", got)
	}

	rk := RefCacheKey("API.GITHUB.COM", "Acme", "Repo", "Feat/X", "app:42")
	if rk != "ref:api.github.com/acme/repo@Feat/X#app:42" {
		t.Errorf("RefCacheKey = %q", rk)
	}
	tk := TreeCacheKey("API.GITHUB.COM", "Acme", "Repo", sha, "Templates/App", "")
	if tk != "tree:api.github.com/acme/repo@"+sha+":Templates/App#anon" {
		t.Errorf("TreeCacheKey = %q", tk)
	}
}
