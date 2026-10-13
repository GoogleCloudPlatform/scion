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

package runtimebroker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestRedactCloneURL covers the userinfo shapes a clone URL can carry: a
// user:pass pair, a username-only token (the "https://TOKEN@host" form,
// which net/url's Redacted() alone would leave visible since there is no
// password to mask), a token carried in the query string or fragment
// instead of userinfo, and an unparseable URL — none of which may ever be
// logged or returned to a client raw.
func TestRedactCloneURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "user and password",
			in:   "https://user:supersecret@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "username-only token",
			in:   "https://ghp_supersecrettoken@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "token in query string",
			in:   "https://github.com/org/repo.git?access_token=supersecret",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "token in fragment",
			in:   "https://github.com/org/repo.git#access_token=supersecret",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "no credentials",
			in:   "https://github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "unparseable",
			in:   "://not a url",
			want: "<unparseable>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactCloneURL(tt.in)
			if got != tt.want {
				t.Errorf("redactCloneURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, "supersecret") {
				t.Errorf("redactCloneURL(%q) still contains the credential: %q", tt.in, got)
			}
		})
	}
}

// TestSanitizeCloneErrorText covers the two forms a clone URL can take
// inside a provisioning error: the exact raw URL this package embeds into
// its own error text, and git's own reformatted echo of the same URL in its
// stderr output (which already strips userinfo on its own, but can still
// carry the query string or fragment).
func TestSanitizeCloneErrorText(t *testing.T) {
	t.Run("userinfo and query embedded verbatim", func(t *testing.T) {
		rawURL := "https://SUPERSECRETTOKEN@127.0.0.1:1/x.git?access_token=QSECRET"
		errText := "ProvisionShared: git clone: git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "SUPERSECRETTOKEN") {
			t.Errorf("sanitized error text still contains the userinfo token: %q", got)
		}
		if strings.Contains(got, "QSECRET") {
			t.Errorf("sanitized error text still contains the query-string token: %q", got)
		}
		if !strings.Contains(got, "127.0.0.1") {
			t.Errorf("expected the host to remain in the sanitized text, got: %q", got)
		}
	})

	t.Run("a fragment embedded verbatim", func(t *testing.T) {
		rawURL := "https://127.0.0.1:1/x.git#FRAGMENTTOKEN"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "FRAGMENTTOKEN") {
			t.Errorf("sanitized error text still contains the fragment: %q", got)
		}
	})

	t.Run("a differently formatted echo carrying only the userinfo", func(t *testing.T) {
		// Isolates the userinfo strip on its own: the port differs from the
		// raw URL, so the exact raw URL is not a substring of errText at
		// all, and the wholesale exact-match replace cannot apply here —
		// only the individual userinfo strip can remove the token.
		rawURL := "https://SECRETUSER@127.0.0.1:1/x.git"
		errText := "fatal: Authentication failed for 'https://SECRETUSER@127.0.0.1/x.git'"
		if strings.Contains(errText, rawURL) {
			t.Fatalf("test setup error: errText must not contain the exact raw URL %q", rawURL)
		}
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "SECRETUSER") {
			t.Errorf("sanitized error text still contains the userinfo token from a reformatted echo: %q", got)
		}
	})

	t.Run("a differently formatted echo carrying only the fragment", func(t *testing.T) {
		// Isolates the fragment strip on its own, the same way the query
		// case above isolates the query strip: the port differs, so the
		// exact raw URL is not a substring of errText.
		rawURL := "https://127.0.0.1:1/x.git#FRAGMENTTOKEN"
		errText := "fatal: unable to access 'https://127.0.0.1/x.git#FRAGMENTTOKEN'"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "FRAGMENTTOKEN") {
			t.Errorf("sanitized error text still contains the fragment token from a reformatted echo: %q", got)
		}
	})

	t.Run("a differently formatted echo of the same URL", func(t *testing.T) {
		// git's own diagnostic output strips userinfo on its own when it
		// echoes a URL, but can still carry the query string, in a form
		// that differs from the exact raw URL string (for example a
		// trailing slash) — so a wholesale match against the raw URL alone
		// does not catch it; the query-string strip must apply on its own.
		rawURL := "https://SUPERSECRETTOKEN@127.0.0.1:1/x.git?access_token=QSECRET"
		errText := "fatal: unable to access 'https://127.0.0.1:1/x.git?access_token=QSECRET/'"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "QSECRET") {
			t.Errorf("sanitized error text still contains the query-string token from a reformatted echo: %q", got)
		}
	})

	t.Run("a percent-encoded userinfo is removed only by the exact-match replace", func(t *testing.T) {
		// url.Userinfo.String() re-escapes minimally, so a userinfo that
		// arrived percent-encoded (here "%7E" for "~") comes back out as a
		// literal "~" — a different string than the exact raw URL text.
		// The individual userinfo strip searches for that decoded form and
		// does not find it; only the wholesale exact-match replace, which
		// compares against the raw URL text as given, removes it.
		rawURL := "https://SECRET%7Etok@127.0.0.1:1/x.git"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "SECRET") {
			t.Errorf("sanitized error text still contains the percent-encoded userinfo: %q", got)
		}
	})

	t.Run("a percent-encoded fragment is removed only by the exact-match replace", func(t *testing.T) {
		// u.Fragment is the decoded form ("%41" -> "A"), which differs from
		// the raw URL's own percent-encoded text. Same reasoning as above,
		// for the fragment strip instead of the userinfo strip.
		rawURL := "https://127.0.0.1:1/x.git#FRAG%41TOK"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "FRAG%41TOK") {
			t.Errorf("sanitized error text still contains the percent-encoded fragment: %q", got)
		}
	})

	t.Run("an empty userinfo does not strip unrelated @ signs", func(t *testing.T) {
		// A URL with an empty userinfo section (e.g. "https://@host/...")
		// parses with a non-nil but empty u.User. u.User.String() is then
		// "", so the naive strip pattern would be just "@" and remove every
		// "@" in the text, not only a genuine credential.
		rawURL := "https://@example.com/r.git"
		errText := "fatal: could not read from remote repository, please check access rights: ssh git@other.example and try again"
		got := sanitizeCloneErrorText(errText, rawURL)
		if !strings.Contains(got, "git@other.example") {
			t.Errorf("sanitized error text lost an unrelated @, got: %q", got)
		}
	})

	t.Run("a userinfo of just a colon does not strip unrelated :@ text", func(t *testing.T) {
		// A URL with an empty username and an empty password separated by a
		// colon (e.g. "https://:@host/...") also parses with a non-nil
		// u.User, and u.User.String() is ":" — carrying no credential —
		// but the naive strip pattern would still remove every ":@" in the
		// text.
		rawURL := "https://:@example.com/r.git"
		errText := "unrelated text containing a:@b elsewhere"
		got := sanitizeCloneErrorText(errText, rawURL)
		if !strings.Contains(got, "a:@b") {
			t.Errorf("sanitized error text lost unrelated \":@\" text, got: %q", got)
		}
	})

	t.Run("a differently formatted echo carrying only a password", func(t *testing.T) {
		// An empty username with a non-empty password still has to be
		// stripped: the port differs, so only the individual userinfo strip
		// can remove it.
		rawURL := "https://:PWONLY@127.0.0.1:1/x.git"
		errText := "fatal: Authentication failed for 'https://:PWONLY@127.0.0.1/x.git'"
		if strings.Contains(errText, rawURL) {
			t.Fatalf("test setup error: errText must not contain the exact raw URL %q", rawURL)
		}
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "PWONLY") {
			t.Errorf("sanitized error text still contains the password from a reformatted echo: %q", got)
		}
	})

	t.Run("an unparseable URL still strips the exact raw text", func(t *testing.T) {
		rawURL := "https://user:TOKEN@host/%zz"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "TOKEN") {
			t.Errorf("sanitized error text still contains the credential from an unparseable URL: %q", got)
		}
		if strings.Contains(got, rawURL) {
			t.Errorf("sanitized error text still contains the raw URL verbatim: %q", got)
		}
	})

	t.Run("empty inputs pass through unchanged", func(t *testing.T) {
		if got := sanitizeCloneErrorText("", "https://host/r.git"); got != "" {
			t.Errorf("expected empty errText to stay empty, got %q", got)
		}
		if got := sanitizeCloneErrorText("some error", ""); got != "some error" {
			t.Errorf("expected an empty rawURL to leave errText unchanged, got %q", got)
		}
	})
}

// TestTryProvisionWorktree_FallbackFailureLogNeverContainsCredentials drives
// a fresh agent's first provisioning attempt against an unreachable URL
// carrying both a userinfo token and a query-string token through
// provision.ProvisionShared's real git clone, and captures the resulting
// slog.Warn log line next to the already-redacted clone_url attribute.
// Neither token may appear anywhere in the captured log output.
func TestTryProvisionWorktree_FallbackFailureLogNeverContainsCredentials(t *testing.T) {
	requireWorktreeGit(t)

	const secretToken = "SUPERSECRETSAUCE"
	credentialedURL := "https://" + secretToken + "@127.0.0.1:1/x.git?access_token=" + secretToken

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	opts := &api.StartOptions{}
	_, _, _ = srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: credentialedURL}},
	}, opts, map[string]string{}, srv.runtime.Name())

	logged := buf.String()
	if strings.Contains(logged, secretToken) {
		t.Errorf("provisioning-failure log must never contain the clone URL's token, got: %s", logged)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_PreExistedFailureLogNeverContainsCredentials
// is the preExisted-path counterpart: a second start for an agent whose
// worktree already exists, injected with a credentialed URL and a
// provisioning failure, must never write the clone URL's userinfo or
// query-string token to the slog.Error log line at that call site either.
//
// The injected failure here (the agent's own worktree target replaced with a
// plain file) reaches provision.ProvisionShared through the same code path
// as every other preExisted failure: since the fail-closed guard just above
// this call already requires the provisioning sentinel to exist, ProvisionShared
// always skips its own git-clone step and goes straight to ensureWorktree —
// so its returned error can never itself contain the raw clone URL here,
// only a worktree-shape or branch message. sanitizeCloneErrorText is still
// applied defensively regardless. What this test actually pins is the
// "clone_url" attribute logged alongside it: it is always the redactCloneURL
// output, never GitClone.URL itself, independent of how ProvisionShared
// fails.
func TestBuildStartContext_WorktreePerAgentStart_PreExistedFailureLogNeverContainsCredentials(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	worktreePath := sc1.Opts.Workspace
	if worktreePath == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}

	// Replace the agent's own worktree with a plain file so ensureWorktree's
	// own-path check refuses to reuse or remove it on the next call.
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worktreePath, []byte("not a worktree"), 0o644); err != nil {
		t.Fatal(err)
	}

	const secretToken = "SUPERSECRETSAUCE"
	in.Config.GitClone = &api.GitCloneConfig{URL: "https://" + secretToken + "@127.0.0.1:1/x.git?access_token=" + secretToken}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	if _, err := srv.buildStartContext(context.Background(), in); err == nil {
		t.Fatal("expected the second buildStartContext to fail")
	}

	logged := buf.String()
	if strings.Contains(logged, secretToken) {
		t.Errorf("the preExisted-failure log must never contain the clone URL's token, got: %s", logged)
	}
	if !strings.Contains(logged, `clone_url=`) {
		t.Fatalf("expected the log line to carry a clone_url attribute, got: %s", logged)
	}
}

// TestBuildStartContext_GitCloneDebugLogRedactsCredentials proves the
// git-clone-mode debug log never contains a clone URL's embedded
// credentials, whether they are a user:pass pair or a username-only token.
func TestBuildStartContext_GitCloneDebugLogRedactsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "user and password", url: "https://user:supersecret@github.com/org/repo.git"},
		{name: "username-only token", url: "https://supersecret@github.com/org/repo.git"},
		{name: "token in query string", url: "https://github.com/org/repo.git?access_token=supersecret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			cfg.Debug = true
			srv := newTestServerForStartContext(t, cfg)

			var buf bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-1",
				ProjectPath: "/some/path",
				Config: &CreateAgentConfig{
					GitClone: &api.GitCloneConfig{
						URL:    tc.url,
						Branch: "main",
					},
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatal(err)
			}

			logged := buf.String()
			if strings.Contains(logged, "supersecret") {
				t.Errorf("debug log must not contain the clone URL's credentials, got: %s", logged)
			}
			if !strings.Contains(logged, "github.com") {
				t.Errorf("expected the redacted cloneURL to still be logged, got: %s", logged)
			}
		})
	}
}
