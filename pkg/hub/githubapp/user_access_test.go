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

package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebHostForAPIBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "github.com"},
		{"https://api.github.com", "github.com"},
		{"https://api.github.com/", "github.com"},
		{"https://API.GitHub.com.", "github.com"},
		{"https://ghe.example.com/api/v3", "ghe.example.com"},
		{"https://GHE.Example.com:8443/api/v3/", "ghe.example.com:8443"},
		{"https://api.acme.ghe.com", "acme.ghe.com"},
		{"http://127.0.0.1:4567", "127.0.0.1:4567"},
		// "api.com" is not stripped to "com".
		{"https://api.com", "api.com"},
		{"://bad", ""},
		{"https://user@ghe.example.com/api/v3", ""},
	}
	for _, tt := range tests {
		if got := WebHostForAPIBaseURL(tt.in); got != tt.want {
			t.Errorf("WebHostForAPIBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCanonicalHost(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"GitHub.com", "github.com"},
		{"github.com.", "github.com"},
		{"github.com.:443", "github.com:443"},
		{"github.com:8443", "github.com:8443"},
		{"[::1]", "[::1]"},
		{"[::1]:22", "[::1]:22"},
		{".", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := CanonicalHost(tt.in); got != tt.want {
			t.Errorf("CanonicalHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCheckUserRepoAccess(t *testing.T) {
	var gotAuth, gotPath string
	status := http.StatusOK
	body := `{"full_name":"acme/widgets","permissions":{"admin":false,"maintain":false,"push":true,"triage":true,"pull":true}}`
	rateLimited := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if rateLimited {
			w.Header().Set("X-RateLimit-Remaining", "0")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()

	t.Run("push access", func(t *testing.T) {
		access, err := CheckUserRepoAccess(ctx, srv.URL, "user-token", "acme", "widgets")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !access.Push || !access.Pull || access.Admin {
			t.Errorf("unexpected access %+v", access)
		}
		if gotAuth != "Bearer user-token" {
			t.Errorf("Authorization = %q, want the user's own token", gotAuth)
		}
		if gotPath != "/repos/acme/widgets" {
			t.Errorf("path = %q", gotPath)
		}
	})

	t.Run("not found is a definitive denial", func(t *testing.T) {
		status, body = http.StatusNotFound, `{"message":"Not Found"}`
		_, err := CheckUserRepoAccess(ctx, srv.URL, "user-token", "acme", "widgets")
		if !errors.Is(err, ErrRepoAccessDenied) {
			t.Fatalf("err = %v, want ErrRepoAccessDenied", err)
		}
	})

	t.Run("bad token is a definitive denial", func(t *testing.T) {
		status, body = http.StatusUnauthorized, `{"message":"Bad credentials"}`
		_, err := CheckUserRepoAccess(ctx, srv.URL, "user-token", "acme", "widgets")
		if !errors.Is(err, ErrRepoAccessDenied) {
			t.Fatalf("err = %v, want ErrRepoAccessDenied", err)
		}
	})

	t.Run("rate limit is inconclusive, not access and not a denial", func(t *testing.T) {
		status, body, rateLimited = http.StatusForbidden, `{"message":"API rate limit exceeded"}`, true
		defer func() { rateLimited = false }()
		_, err := CheckUserRepoAccess(ctx, srv.URL, "user-token", "acme", "widgets")
		if err == nil || errors.Is(err, ErrRepoAccessDenied) {
			t.Fatalf("err = %v, want an inconclusive (non-denial) error", err)
		}
	})

	t.Run("server error is inconclusive", func(t *testing.T) {
		status, body = http.StatusBadGateway, `oops`
		_, err := CheckUserRepoAccess(ctx, srv.URL, "user-token", "acme", "widgets")
		if err == nil || errors.Is(err, ErrRepoAccessDenied) {
			t.Fatalf("err = %v, want an inconclusive (non-denial) error", err)
		}
	})

	t.Run("missing permissions object is a denial", func(t *testing.T) {
		status, body = http.StatusOK, `{"full_name":"acme/widgets"}`
		_, err := CheckUserRepoAccess(ctx, srv.URL, "user-token", "acme", "widgets")
		if !errors.Is(err, ErrRepoAccessDenied) {
			t.Fatalf("err = %v, want ErrRepoAccessDenied", err)
		}
	})

	t.Run("empty token never calls GitHub", func(t *testing.T) {
		gotAuth = "unset"
		_, err := CheckUserRepoAccess(ctx, srv.URL, "", "acme", "widgets")
		if !errors.Is(err, ErrRepoAccessDenied) {
			t.Fatalf("err = %v, want ErrRepoAccessDenied", err)
		}
		if gotAuth != "unset" {
			t.Error("GitHub was called without a user token")
		}
	})
}

func TestRemoteOwnerRepo(t *testing.T) {
	tests := []struct {
		remote, host        string
		wantOwner, wantRepo string
		wantOK              bool
	}{
		{"https://github.com/Acme/Widgets.git", "github.com", "Acme", "Widgets", true},
		{"https://github.com/acme/widgets", "github.com", "acme", "widgets", true},
		{"git@github.com:acme/widgets.git", "github.com", "acme", "widgets", true},
		{"ssh://git@github.com/acme/widgets.git", "github.com", "acme", "widgets", true},
		{"github.com/acme/widgets", "github.com", "acme", "widgets", true},
		{"https://GitHub.COM/acme/widgets/", "github.com", "acme", "widgets", true},
		{"https://x-access-token:abc@github.com/acme/widgets.git", "github.com", "acme", "widgets", true},
		{"https://ghe.example.com/acme/widgets.git", "ghe.example.com", "acme", "widgets", true},

		// Host mismatch: never associate or mint.
		{"https://gitlab.com/acme/widgets.git", "github.com", "", "", false},
		{"https://other.example/acme/widgets.git", "github.com", "", "", false},
		{"https://github.com.other.example/acme/widgets", "github.com", "", "", false},
		{"https://notgithub.com/acme/widgets", "github.com", "", "", false},
		{"https://github.com:8443/acme/widgets", "github.com", "", "", false},
		{"git@gitlab.com:acme/widgets.git", "github.com", "", "", false},
		{"https://github.com/acme/widgets", "ghe.example.com", "", "", false},
		{"https://github.com/acme/widgets", "", "", "", false},

		// ssh:// remotes compare the hostname only; the SSH port is ignored.
		{"ssh://git@ghe.example.com:2222/acme/widgets.git", "ghe.example.com", "acme", "widgets", true},
		{"ssh://git@GHE.Example.com.:2222/acme/widgets.git", "ghe.example.com", "acme", "widgets", true},
		{"ssh://git@ghe.example.com:2222/acme/widgets.git", "ghe.example.com:8443", "acme", "widgets", true},
		{"ssh://git@github.com:22/acme/widgets.git", "github.com", "acme", "widgets", true},
		{"ssh://git@other.example.com:2222/acme/widgets.git", "ghe.example.com", "", "", false},
		{"ssh://git@ghe.example.com.other.example:2222/acme/widgets.git", "ghe.example.com", "", "", false},
		// The host is the one actually connected to: userinfo (up to the
		// last "@") and scp-form paths cannot name the configured host.
		{"ssh://git@ghe.example.com@other.example/acme/widgets.git", "ghe.example.com", "", "", false},
		{"ssh://git@ghe.example.com@other.example:2222/acme/widgets.git", "ghe.example.com", "", "", false},
		{"ssh://ghe.example.com:2222@other.example/acme/widgets.git", "ghe.example.com", "", "", false},
		{"ssh://git@other.example/ghe.example.com/acme/widgets.git", "ghe.example.com", "", "", false},
		{"git@ghe.example.com@other.example:acme/widgets.git", "ghe.example.com", "", "", false},
		{"git@other.example:ghe.example.com/widgets.git", "ghe.example.com", "", "", false},
		// scp form "git@host:path": the host before ":" is connected to; a
		// path that is not exactly owner/repo never matches.
		{"git@ghe.example.com:other", "ghe.example.com", "", "", false},
		{"git@ghe.example.com:other/acme/widgets.git", "ghe.example.com", "", "", false},
		{"git@ghe.example.com:acme/widgets.git", "ghe.example.com", "acme", "widgets", true},
		// HTTPS keeps the port as part of the host: a non-default port
		// never matches a configured host without it, and vice versa.
		{"https://ghe.example.com:2222/acme/widgets.git", "ghe.example.com", "", "", false},
		{"https://ghe.example.com:8443/acme/widgets.git", "ghe.example.com", "", "", false},
		{"https://ghe.example.com/acme/widgets.git", "ghe.example.com:8443", "", "", false},
		{"https://ghe.example.com:8443/acme/widgets.git", "ghe.example.com:8443", "acme", "widgets", true},
		{"https://ghe.example.com@other.example/acme/widgets.git", "ghe.example.com", "", "", false},

		// Path must be exactly owner/repo.
		{"https://github.com/acme", "github.com", "", "", false},
		{"https://github.com/acme/widgets/tree/main", "github.com", "", "", false},
		{"", "github.com", "", "", false},
	}
	for _, tt := range tests {
		owner, repo, ok := RemoteOwnerRepo(tt.remote, tt.host)
		if ok != tt.wantOK || owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("RemoteOwnerRepo(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.remote, tt.host, owner, repo, ok, tt.wantOwner, tt.wantRepo, tt.wantOK)
		}
	}
}

func TestHostWithoutPort(t *testing.T) {
	for in, want := range map[string]string{
		"ghe.example.com:2222": "ghe.example.com",
		"ghe.example.com":      "ghe.example.com",
		"[::1]:22":             "[::1]",
		"[::1]":                "[::1]",
	} {
		if got := hostWithoutPort(in); got != want {
			t.Errorf("hostWithoutPort(%q) = %q, want %q", in, got, want)
		}
	}
}
