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

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Obvious fakes; never real credentials.
const (
	fakeRejectedToken = "invalid-token"
	fakeGoodToken     = "good-token"
	fakeRawFileURL    = "https://raw.githubusercontent.com/owner/repo/0123456789abcdef0123456789abcdef01234567/skills/s/SKILL.md"
)

// seenRequest records what one request looked like when it left
// downloadSkillFile.
type seenRequest struct {
	host string
	path string
	auth string
}

// githubRawFake serves requests addressed to GitHub hosts from a local
// handler, so downloadSkillFile's real isGitHubHost check decides whether a
// credential is sent. Requests to any other host go to their own address.
type githubRawFake struct {
	srv *httptest.Server

	mu   sync.Mutex
	seen []seenRequest
}

func (f *githubRawFake) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{host: req.URL.Host, path: req.URL.Path, auth: req.Header.Get("Authorization")})
	f.mu.Unlock()

	out := req.Clone(req.Context())
	if isGitHubHost(req.URL.Hostname()) {
		target, _ := url.Parse(f.srv.URL)
		out.URL.Scheme = target.Scheme
		out.URL.Host = target.Host
		out.Host = target.Host
	}
	return http.DefaultTransport.RoundTrip(out)
}

func (f *githubRawFake) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

// installGitHubRawFake routes downloadSkillFile's GitHub-host requests to
// handler for the duration of the test.
func installGitHubRawFake(t *testing.T, handler http.HandlerFunc) *githubRawFake {
	t.Helper()
	f := &githubRawFake{srv: httptest.NewServer(handler)}
	t.Cleanup(f.srv.Close)
	prev := skillDownloadTransport
	skillDownloadTransport = f
	t.Cleanup(func() { skillDownloadTransport = prev })
	return f
}

// publicFileHandler answers like raw.githubusercontent.com does for a public
// file: authStatus when a credential is sent (GitHub rejected it), and the
// content when none is.
func publicFileHandler(authStatus int, content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(authStatus)
			return
		}
		_, _ = w.Write([]byte(content))
	}
}

func rejectedCred() gitHubCredential {
	return gitHubCredential{token: fakeRejectedToken, source: gitHubCredSourceDefault}
}

// A public file fetched with a credential GitHub rejects (401, 403 or 404)
// is downloaded by one retry, at the same URL, without the credential.
func TestDownloadSkillFile_RejectedCredentialRetriesWithoutAuth(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fake := installGitHubRawFake(t, publicFileHandler(status, "skill body"))
			dest := filepath.Join(t.TempDir(), "SKILL.md")

			if err := downloadSkillFile(context.Background(), fakeRawFileURL, dest, defaultMaxFileSize, rejectedCred()); err != nil {
				t.Fatalf("retry: downloadSkillFile: %v", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil || string(got) != "skill body" {
				t.Fatalf("content = %q, %v; want %q", got, err, "skill body")
			}

			reqs := fake.requests()
			if len(reqs) != 2 {
				t.Fatalf("requests = %d, want 2 (one with the credential, one retry without): %+v", len(reqs), reqs)
			}
			if reqs[0].auth != "Bearer "+fakeRejectedToken {
				t.Errorf("first request should carry the configured credential")
			}
			if reqs[1].auth != "" {
				t.Errorf("retry must not carry a credential, got Authorization %q", reqs[1].auth)
			}
			if reqs[1].host != "raw.githubusercontent.com" || reqs[1].path != reqs[0].path {
				t.Errorf("retry went to %s%s, want the same URL as the first request (%s%s)",
					reqs[1].host, reqs[1].path, reqs[0].host, reqs[0].path)
			}
		})
	}
}

// A file that fails with the credential and without it (a private repo, or a
// missing file) gives a typed error that says the credential was rejected,
// names its source, and never contains the credential.
func TestDownloadSkillFile_RejectedCredentialPrivateRepo(t *testing.T) {
	fake := installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	dest := filepath.Join(t.TempDir(), "SKILL.md")

	err := downloadSkillFile(context.Background(), fakeRawFileURL, dest, defaultMaxFileSize, rejectedCred())
	if err == nil {
		t.Fatal("private repo: expected an error")
	}
	var rej *gitHubCredentialRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("error = %v (%T), want *gitHubCredentialRejectedError", err, err)
	}
	if rej.AuthStatus != http.StatusNotFound || rej.AnonStatus != http.StatusNotFound {
		t.Errorf("statuses = %d/%d, want 404/404", rej.AuthStatus, rej.AnonStatus)
	}
	msg := err.Error()
	for _, want := range []string{"SKILL.md", "rejected", gitHubCredSourceDefault, "status 404", "private"} {
		if !strings.Contains(msg, want) {
			t.Errorf("private repo: error %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, fakeRejectedToken) {
		t.Errorf("private repo: error must not contain the credential: %q", msg)
	}
	if n := len(fake.requests()); n != 2 {
		t.Errorf("requests = %d, want exactly 2 (no further retries)", n)
	}
}

// A credential GitHub accepts is used as before: one request, carrying it.
// Statuses outside 401/403/404 are not retried.
func TestDownloadSkillFile_GoodCredentialNoRetry(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		fake := installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("private body"))
		})
		dest := filepath.Join(t.TempDir(), "SKILL.md")
		cred := gitHubCredential{token: fakeGoodToken, source: gitHubCredSourceDefault}
		if err := downloadSkillFile(context.Background(), fakeRawFileURL, dest, defaultMaxFileSize, cred); err != nil {
			t.Fatalf("good credential: downloadSkillFile: %v", err)
		}
		reqs := fake.requests()
		if len(reqs) != 1 {
			t.Fatalf("requests = %d, want 1", len(reqs))
		}
		if reqs[0].auth != "Bearer "+fakeGoodToken {
			t.Errorf("the request should carry the configured credential")
		}
	})
	t.Run("server error", func(t *testing.T) {
		fake := installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		dest := filepath.Join(t.TempDir(), "SKILL.md")
		err := downloadSkillFile(context.Background(), fakeRawFileURL, dest, defaultMaxFileSize, rejectedCred())
		if err == nil || !strings.Contains(err.Error(), "status 500") {
			t.Fatalf("error = %v, want the plain status 500 error", err)
		}
		var rej *gitHubCredentialRejectedError
		if errors.As(err, &rej) {
			t.Errorf("a 500 is not a rejected credential: %v", err)
		}
		if n := len(fake.requests()); n != 1 {
			t.Errorf("requests = %d, want 1 (no retry for a 500)", n)
		}
	})
}

// A non-GitHub host never gets the credential, so a 404 from it is not
// retried and stays a plain status error.
func TestDownloadSkillFile_NoRetryForNonGitHubHost(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	// Requests to srv go through the fake transport unchanged (not a GitHub
	// host), which still exercises the production client configuration.
	installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should reach the GitHub fake")
	})

	dest := filepath.Join(t.TempDir(), "SKILL.md")
	err := downloadSkillFile(context.Background(), srv.URL+"/SKILL.md", dest, defaultMaxFileSize, rejectedCred())
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("error = %v, want the plain status 404 error", err)
	}
	var rej *gitHubCredentialRejectedError
	if errors.As(err, &rej) {
		t.Errorf("no credential was sent, so the error must not blame one: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 1 {
		t.Fatalf("requests = %d, want 1 (no retry for a non-GitHub host)", len(auths))
	}
	if auths[0] != "" {
		t.Errorf("credential sent to a non-GitHub host: %q", auths[0])
	}
}

// Content fetched by the retry without credentials is still checked against
// the resolver's hashes.
func TestInstallOneSkill_RejectedCredentialRetryStillVerifiesHash(t *testing.T) {
	const want = "hello\n"
	wantHash := hashBytesAs([]byte(want), "sha256:")
	ctx := ContextWithGitHubToken(context.Background(), fakeRejectedToken)
	skill := func() ResolvedSkill {
		return ResolvedSkill{
			Name:    "s",
			URI:     "gh://owner/repo/s",
			Version: "0123456789ab",
			Files:   []ResolvedFile{{Path: "SKILL.md", URL: fakeRawFileURL, Hash: wantHash}},
		}
	}

	t.Run("tampered", func(t *testing.T) {
		installGitHubRawFake(t, publicFileHandler(http.StatusNotFound, "tampered\n"))
		_, err := installOneSkill(ctx, skill(), "s", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
			t.Fatalf("error = %v, want a hash mismatch", err)
		}
	})
	t.Run("matching", func(t *testing.T) {
		installGitHubRawFake(t, publicFileHandler(http.StatusNotFound, want))
		skillsDest := t.TempDir()
		if _, err := installOneSkill(ctx, skill(), "s", skillsDest); err != nil {
			t.Fatalf("installOneSkill: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(skillsDest, "s", "SKILL.md"))
		if err != nil || string(got) != want {
			t.Fatalf("installed content = %q, %v; want %q", got, err, want)
		}
	})
}

// When both attempts fail, the install error names the skill and the source
// of the credential that was tried, but not the credential. A skill served
// from the GitHub resolver's cache without content (the incident path) gets
// its credential, and its label, from the resolver's own lookup: the
// ?token= secret name, the GH_* convention key that matched, or the default
// GITHUB_TOKEN.
func TestInstallOneSkill_RejectedCredentialNamesSource(t *testing.T) {
	installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	skill := func(credRef string) ResolvedSkill {
		return ResolvedSkill{
			Name:                "s",
			URI:                 "gh://owner/repo/s",
			Files:               []ResolvedFile{{Path: "SKILL.md", URL: fakeRawFileURL}},
			githubCredentialRef: credRef,
		}
	}
	// installCtx is the context the agent runtime builds for the install
	// phase (runtimebroker attachSkillResolver / cmd create).
	installCtx := func(defaultToken string, creds map[string]string) context.Context {
		r := &GitHubSkillResolver{token: defaultToken, provisionCredentials: creds}
		return r.WithInstallCredentials(context.Background(), defaultToken)
	}

	tests := []struct {
		name       string
		ctx        context.Context
		skill      ResolvedSkill
		wantSource string
		token      string
	}{
		{
			name:       "hub-resolved skill, default token",
			ctx:        ContextWithGitHubToken(context.Background(), fakeRejectedToken),
			skill:      skill(""),
			wantSource: "the GITHUB_TOKEN credential",
			token:      fakeRejectedToken,
		},
		{
			name:       "cache hit, falls through to the default token",
			ctx:        installCtx(fakeRejectedToken, nil),
			skill:      skill("gh://owner/repo/s"),
			wantSource: "the GITHUB_TOKEN credential",
			token:      fakeRejectedToken,
		},
		{
			name:       "cache hit, owner key",
			ctx:        installCtx(fakeRejectedToken, map[string]string{"GH_OWNER": "invalid-owner-token"}),
			skill:      skill("gh://owner/repo/s"),
			wantSource: "the credential GH_OWNER",
			token:      "invalid-owner-token",
		},
		{
			name:       "cache hit, repo key",
			ctx:        installCtx(fakeRejectedToken, map[string]string{"GH_OWNER": "invalid-owner-token", "GH_OWNER__REPO": "invalid-repo-token"}),
			skill:      skill("gh://owner/repo/s"),
			wantSource: "the credential GH_OWNER__REPO",
			token:      "invalid-repo-token",
		},
		{
			name:       "cache hit, ?token= secret",
			ctx:        installCtx(fakeRejectedToken, map[string]string{"NAMED": "invalid-named-token"}),
			skill:      skill("gh://owner/repo/s?token=NAMED"),
			wantSource: "the credential NAMED named by ?token=",
			token:      "invalid-named-token",
		},
		{
			name: "lookup without a source label",
			ctx: ContextWithGitHubCredentialLookup(context.Background(), func(string) string {
				return "invalid-lookup-token"
			}),
			skill:      skill("gh://owner/repo/s"),
			wantSource: gitHubCredSourceResolver,
			token:      "invalid-lookup-token",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := installResolvedSkills(tc.ctx, []ResolvedSkill{tc.skill}, t.TempDir(), t.TempDir())
			var skillErr *SkillResolutionError
			if !errors.As(err, &skillErr) {
				t.Fatalf("source label: expected a *SkillResolutionError, got %T: %v", err, err)
			}
			// Message is the user-facing reason: the rejected-credential text
			// must reach it, not the fixed "install failed on the broker".
			msg := skillErr.Message
			for _, want := range []string{tc.wantSource, "rejected"} {
				if !strings.Contains(msg, want) {
					t.Errorf("source label: install error %q should contain %q", msg, want)
				}
			}
			if !strings.Contains(err.Error(), `required skill "gh://owner/repo/s"`) {
				t.Errorf("source label: error %q should name the skill", err.Error())
			}
			if strings.Contains(err.Error(), tc.token) {
				t.Errorf("source label: install error must not contain the credential: %q", err.Error())
			}
		})
	}
}

// An optional skill whose download fails with a rejected credential both
// ways is skipped, and the skip reason is the rejected-credential text; a
// required one fails with it.
func TestInstallResolvedSkills_RejectedCredentialOptionalAndRequired(t *testing.T) {
	installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ctx := ContextWithGitHubToken(context.Background(), fakeRejectedToken)
	skill := func(optional bool) ResolvedSkill {
		return ResolvedSkill{
			Name:     "s",
			URI:      "gh://owner/repo/s",
			Files:    []ResolvedFile{{Path: "SKILL.md", URL: fakeRawFileURL}},
			Optional: optional,
		}
	}

	record, skipped, err := installResolvedSkills(ctx, []ResolvedSkill{skill(true)}, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("optional and rejected: an optional skill must be skipped, got: %v", err)
	}
	if len(record.Skills) != 0 || len(skipped) != 1 {
		t.Fatalf("optional and rejected: installed %d, skipped %d; want 0 and 1", len(record.Skills), len(skipped))
	}
	if !strings.Contains(skipped[0].Reason, "rejected the GITHUB_TOKEN credential") {
		t.Errorf("optional and rejected: skip reason %q should carry the rejected-credential text", skipped[0].Reason)
	}
	if strings.Contains(skipped[0].Reason, fakeRejectedToken) {
		t.Errorf("optional and rejected: skip reason must not contain the credential: %q", skipped[0].Reason)
	}

	_, _, err = installResolvedSkills(ctx, []ResolvedSkill{skill(false)}, t.TempDir(), t.TempDir())
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("required and rejected: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if !strings.Contains(skillErr.Message, "rejected the GITHUB_TOKEN credential") {
		t.Errorf("required and rejected: message %q should carry the rejected-credential text", skillErr.Message)
	}
}

// The live incident path end to end: a gh:// skill served from the GitHub
// resolver's disk cache without content, whose credential is the owner-level
// GH_<OWNER> key, is downloaded with that credential; when GitHub rejects it
// both ways, the error names GH_<OWNER> (never its value).
func TestInstallFromDiskCacheHit_OwnerKeyCredentialLabelled(t *testing.T) {
	const (
		uri      = "gh://acme/private/s@main"
		ownerKey = "GH_ACME"
		ownerTok = "invalid-owner-token"
	)
	t.Setenv("GITHUB_TOKEN", "")
	creds := map[string]string{ownerKey: ownerTok}

	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first.putEntry(resolutionCacheKey(ghRef, ownerTok), ResolvedSkill{
		Name: "s", URI: uri,
		Files: []ResolvedFile{{Path: "SKILL.md", URL: fakeRawFileURL, Mode: "0644", Content: []byte("# skill")}},
	}, true)
	first.Flush()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// No GitHub API request is expected: the disk entry is fresh.
	server, _ := newTestGitHubServer(t)
	r := NewGitHubSkillResolverWithCredentials(fakeRejectedToken, creds, cache)
	r.httpClient = server.Client()
	r.apiBase = server.URL
	r.rawBase = server.URL + "/raw"

	fake := installGitHubRawFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	ctx := ContextWithSkillResolver(context.Background(), r)
	ctx = r.WithInstallCredentials(ctx, fakeRejectedToken)
	res, err := r.Resolve(ctx, []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
		t.Fatalf("disk hit: Resolve: err=%v result=%+v", err, res)
	}
	if res.Resolved[0].Files[0].Content != nil {
		t.Fatal("disk hit: expected a content-less disk hit")
	}

	_, _, err = installResolvedSkills(ctx, res.Resolved, t.TempDir(), t.TempDir())
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("disk hit: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if !strings.Contains(skillErr.Message, "the credential "+ownerKey) {
		t.Errorf("disk hit: message %q should name the owner-level credential %s", skillErr.Message, ownerKey)
	}
	if strings.Contains(err.Error(), ownerTok) || strings.Contains(err.Error(), fakeRejectedToken) {
		t.Errorf("disk hit: error must not contain a credential value: %q", err.Error())
	}
	reqs := fake.requests()
	if len(reqs) != 2 || reqs[0].auth != "Bearer "+ownerTok || reqs[1].auth != "" {
		t.Errorf("disk hit: want one request with the owner-level credential and one retry without, got %d requests", len(reqs))
	}
}

// A second request that fails at the transport level (timeout, cancelled
// context, connection reset, a redirect to another host) returns an error;
// it must not panic, and nothing is sent anywhere else.
func TestDownloadSkillFile_RetryTransportFailureReturnsError(t *testing.T) {
	tests := []struct {
		name string
		// anon is how the fake answers the request without credentials.
		anon func(cancel context.CancelFunc) (*http.Response, error)
	}{
		{"timeout", func(context.CancelFunc) (*http.Response, error) { return nil, context.DeadlineExceeded }},
		{"cancelled", func(cancel context.CancelFunc) (*http.Response, error) {
			cancel()
			return nil, context.Canceled
		}},
		{"connection reset", func(context.CancelFunc) (*http.Response, error) {
			return nil, errors.New("read: connection reset by peer")
		}},
		{"cross-host redirect", func(context.CancelFunc) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://elsewhere.example/SKILL.md"}},
				Body:       http.NoBody,
			}, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			var hosts []string
			prev := skillDownloadTransport
			skillDownloadTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				mu.Lock()
				hosts = append(hosts, req.URL.Host)
				mu.Unlock()
				if req.Header.Get("Authorization") != "" {
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
				}
				return tc.anon(cancel)
			})
			t.Cleanup(func() { skillDownloadTransport = prev })

			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = downloadSkillFile(ctx, fakeRawFileURL, filepath.Join(t.TempDir(), "SKILL.md"), defaultMaxFileSize, rejectedCred())
			}()
			if recovered != nil {
				t.Fatalf("retry transport failure (%s) panicked: %v", tc.name, recovered)
			}
			if err == nil {
				t.Fatalf("retry transport failure (%s): expected an error", tc.name)
			}
			if strings.Contains(err.Error(), fakeRejectedToken) {
				t.Errorf("retry transport failure: error must not contain the credential: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(hosts) != 2 || hosts[0] != "raw.githubusercontent.com" || hosts[1] != "raw.githubusercontent.com" {
				t.Errorf("retry transport failure: requests went to %v, want exactly 2 to raw.githubusercontent.com", hosts)
			}
		})
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// gitHubCredential never prints its token, for any fmt verb.
func TestGitHubCredential_FormatRedactsToken(t *testing.T) {
	c := gitHubCredential{token: fakeRejectedToken, source: gitHubCredSourceDefault}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%t"} {
		if s := fmt.Sprintf(verb, c); strings.Contains(s, fakeRejectedToken) {
			t.Errorf("credential formatted with %s contains the token: %q", verb, s)
		}
	}
}
