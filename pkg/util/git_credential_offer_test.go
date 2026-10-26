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

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// credentialTraceServer is an https git endpoint that answers every request
// 401 (Basic) and records the credentials of every Authorization header it
// receives, decoded as user:password.
type credentialTraceServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
	// headers records every X-Ambient request header value.
	headers []string
	// askpassLog records every run of the GIT_ASKPASS program the server
	// sets up (it answers "askpass-secret").
	askpassLog string
}

// askpassRuns returns the logged GIT_ASKPASS prompts.
func (s *credentialTraceServer) askpassRuns() string {
	b, _ := os.ReadFile(s.askpassLog)
	return string(b)
}

func (s *credentialTraceServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// newCredentialTraceServer starts the server and points the host user's git
// config (GIT_CONFIG_GLOBAL, system config off) at its certificate. The
// config also carries an ambient credential store holding
// "ambient:ambient-secret" for the server, so a test can tell whether git
// offered the host user's own credentials.
func newCredentialTraceServer(t *testing.T) *credentialTraceServer {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for this test: %v", err)
	}
	cs := &credentialTraceServer{}
	cs.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			decoded := h
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); err == nil {
				decoded = string(b)
			}
			cs.mu.Lock()
			cs.auth = append(cs.auth, decoded)
			cs.mu.Unlock()
		}
		if v := r.Header.Values("X-Ambient"); len(v) > 0 {
			cs.mu.Lock()
			cs.headers = append(cs.headers, v...)
			cs.mu.Unlock()
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(cs.srv.Close)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cs.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "credentials")
	if err := os.WriteFile(store, []byte(strings.Replace(cs.srv.URL, "https://", "https://ambient:ambient-secret@", 1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	body := "[http]\n\tsslCAInfo = " + ca + "\n[credential]\n\thelper = store --file=" + store + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cs.askpassLog = filepath.Join(dir, "askpass.log")
	askpass := filepath.Join(dir, "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+cs.askpassLog+"'\necho askpass-secret\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", askpass)
	return cs
}

func (s *credentialTraceServer) repoURL() string { return s.srv.URL + "/org/repo.git" }

func assertNoCredentialOffered(t *testing.T, cs *credentialTraceServer) {
	t.Helper()
	if got := cs.seen(); len(got) != 0 {
		t.Errorf("git offered credentials %q; want none", got)
	}
	if runs := cs.askpassRuns(); runs != "" {
		t.Errorf("git ran GIT_ASKPASS:\n%s", runs)
	}
}

func assertOnlyTokenOffered(t *testing.T, cs *credentialTraceServer, token string) {
	t.Helper()
	got := cs.seen()
	if len(got) == 0 {
		t.Fatal("git offered no credential; want the token")
	}
	for _, a := range got {
		if !strings.HasSuffix(a, ":"+token) {
			t.Errorf("git offered %q; want only the token", a)
		}
	}
}

// The trace server's ambient credential store answers a plain clone, so the
// no-credential assertions below are not vacuous.
func TestCredentialTrace_AmbientStoreAnswersPlainClone(t *testing.T) {
	cs := newCredentialTraceServer(t)
	if err := CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", ""); err == nil {
		t.Fatal("the 401 endpoint cannot be cloned")
	}
	got := cs.seen()
	if len(got) == 0 || got[0] != "ambient:ambient-secret" {
		t.Fatalf("want the ambient store's credential offered, got %q", got)
	}
}

// GIT_ASKPASS answers a plain clone when no helper does, so the
// no-askpass assertions below are not vacuous.
func TestCredentialTrace_AskpassAnswersWithoutHelper(t *testing.T) {
	cs := newCredentialTraceServer(t)
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	ca, err := os.ReadFile(os.Getenv("GIT_CONFIG_GLOBAL"))
	if err != nil {
		t.Fatal(err)
	}
	// The same config without the credential store.
	body := strings.SplitN(string(ca), "[credential]", 2)[0]
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	if err := CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", ""); err == nil {
		t.Fatal("the 401 endpoint cannot be cloned")
	}
	if cs.askpassRuns() == "" {
		t.Fatal("want GIT_ASKPASS run")
	}
	got := cs.seen()
	if len(got) == 0 || !strings.HasSuffix(got[0], ":askpass-secret") {
		t.Fatalf("want the askpass credential offered, got %q", got)
	}
}

// With NoCredentialHelpers a clone offers neither the token nor the host user's
// credentials.
func TestCloneSharedWorkspaceHardened_NoCredentials(t *testing.T) {
	cs := newCredentialTraceServer(t)
	err := CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "project-token",
		HostGitHardening{NoCredentialHelpers: true})
	if err == nil {
		t.Fatal("the 401 endpoint cannot be cloned")
	}
	assertNoCredentialOffered(t, cs)
	if strings.Contains(err.Error(), "project-token") {
		t.Error("the error names the token")
	}
}

// Without NoCredentialHelpers a hardened clone offers the token, and only the
// token (the scoped helper resets the host user's helpers).
func TestCloneSharedWorkspaceHardened_OffersTokenOnly(t *testing.T) {
	cs := newCredentialTraceServer(t)
	if err := CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "project-token",
		HostGitHardening{}); err == nil {
		t.Fatal("the 401 endpoint cannot be cloned")
	}
	assertOnlyTokenOffered(t, cs, "project-token")
}

// With NoCredentialHelpers a pull's fetch offers neither the token nor the host
// user's credentials.
func TestPullSharedWorkspaceHardened_NoCredentials(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	cs := newCredentialTraceServer(t)
	if _, err := PullSharedWorkspaceHardened(workspace, cs.repoURL(), "project-token",
		HostGitHardening{NoCredentialHelpers: true}); err == nil {
		t.Fatal("the 401 endpoint cannot be pulled")
	}
	assertNoCredentialOffered(t, cs)
}

// Without NoCredentialHelpers a hardened pull offers the token only.
func TestPullSharedWorkspaceHardened_OffersTokenOnly(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	cs := newCredentialTraceServer(t)
	if _, err := PullSharedWorkspaceHardened(workspace, cs.repoURL(), "project-token", HostGitHardening{}); err == nil {
		t.Fatal("the 401 endpoint cannot be pulled")
	}
	assertOnlyTokenOffered(t, cs, "project-token")
}

// A hardened pull's fetch carries the caller's settings after the network
// transport allow-list, so they override it, and the caller's environment.
func TestPullSharedWorkspaceHardened_ConfigOverridesNetworkArgs(t *testing.T) {
	source, workspace, _ := sharedWorkspaceFixture(t)
	// file is refused by the caller's settings: the local source cannot be
	// fetched.
	_, err := PullSharedWorkspaceHardened(workspace, source, "", HostGitHardening{
		Config: []string{"protocol.file.allow=never"},
	})
	if err == nil {
		t.Fatal("a fetch with protocol.file.allow=never must refuse a local source")
	}
	_, err = PullSharedWorkspaceHardened(workspace, source, "", HostGitHardening{
		Env: []string{"GIT_ALLOW_PROTOCOL=https:ssh:git"},
	})
	if err == nil {
		t.Fatal("a fetch with GIT_ALLOW_PROTOCOL leaving out file must refuse a local source")
	}
	if _, err := PullSharedWorkspace(workspace, source, ""); err != nil {
		t.Fatalf("the same source pulls without the settings: %v", err)
	}
}

// newAuthedDumbHTTPRepo serves a bare copy of source over https as a dumb
// HTTP git repository at <url>/org/repo.git, answering 401 (Basic) unless a
// request carries oauth2:<token> (no check when token is empty). The host
// user's git config trusts its certificate. It returns the repository URL.
func newAuthedDumbHTTPRepo(t *testing.T, source, token string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "org", "repo.git")
	for _, args := range [][]string{
		{"clone", "-q", "--bare", source, bare},
		{"-C", bare, "repack", "-a", "-d", "-q"},
		{"-C", bare, "update-server-info"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
	files := http.FileServer(http.Dir(root))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[http]\n\tsslCAInfo = "+ca+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return srv.URL + "/org/repo.git"
}

// A hardened clone and pull with the token still authenticate to a
// repository that requires it.
func TestHardenedCloneAndPull_AuthenticateWithToken(t *testing.T) {
	source := setupGitRepo(t)
	url := newAuthedDumbHTTPRepo(t, source, "project-token")
	h := HostGitHardening{
		Config: []string{"protocol.allow=never", "protocol.file.allow=never", "protocol.https.allow=always", "protocol.http.allow=never"},
		Env:    []string{"GIT_ALLOW_PROTOCOL=https:ssh:git"},
	}
	dest := filepath.Join(t.TempDir(), "ws")
	if err := CloneSharedWorkspaceHardened(dest, url, "", "project-token", h); err != nil {
		t.Fatalf("hardened clone with the token: %v", err)
	}
	if !IsGitRepoDir(dest) {
		t.Fatal("the clone must be a repository")
	}
	if _, err := PullSharedWorkspaceHardened(dest, url, "project-token", h); err != nil {
		t.Fatalf("hardened pull with the token: %v", err)
	}
	// Without the token the same repository refuses the clone.
	if err := CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), url, "", "", h); err == nil {
		t.Fatal("the repository must require the token")
	}
}

func (s *credentialTraceServer) seenHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.headers...)
}

// useAmbientConfig replaces the host user's git config with one that
// trusts the server and holds extra, given after the [http] section.
func (s *credentialTraceServer) useAmbientConfig(t *testing.T, extra string) {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[http]\n\tsslCAInfo = "+ca+"\n"+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
}

// ambientHeaderConfig sets an X-Ambient extra header at every URL scope a
// request to the server's org/repo matches: generic, the host, and the full
// repository URL.
func (s *credentialTraceServer) ambientHeaderConfig() string {
	return "\textraHeader = X-Ambient: generic\n" +
		"[http \"" + s.srv.URL + "/\"]\n\textraHeader = X-Ambient: host\n" +
		"[http \"" + s.repoURL() + "\"]\n\textraHeader = X-Ambient: full\n"
}

// With NoCredentialHelpers no configured extra header is sent, whether it
// is generic or scoped to the host or to the full URL, on clone and pull.
// Without it all three are sent (the harness is live).
func TestNoCredentialHelpers_ResetsExtraHeadersAtEveryURLScope(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	cs := newCredentialTraceServer(t)
	cs.useAmbientConfig(t, cs.ambientHeaderConfig())

	_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "")
	got := cs.seenHeaders()
	for _, want := range []string{"generic", "host", "full"} {
		if !slices.Contains(got, want) {
			t.Fatalf("a plain clone must send the %s ambient header (harness), got %q", want, got)
		}
	}

	n := len(cs.seenHeaders())
	_ = CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "", HostGitHardening{NoCredentialHelpers: true})
	if extra := cs.seenHeaders()[n:]; len(extra) != 0 {
		t.Errorf("clone with NoCredentialHelpers sent ambient headers %q", extra)
	}
	n = len(cs.seenHeaders())
	_, _ = PullSharedWorkspaceHardened(workspace, cs.repoURL(), "", HostGitHardening{NoCredentialHelpers: true})
	if extra := cs.seenHeaders()[n:]; len(extra) != 0 {
		t.Errorf("pull with NoCredentialHelpers sent ambient headers %q", extra)
	}
}

// With NoCredentialHelpers a credential helper scoped to the host or to the
// full URL is not asked either, on clone and pull; without it, it answers.
func TestNoCredentialHelpers_ResetsURLScopedHelpers(t *testing.T) {
	for _, scope := range []string{"host", "full URL"} {
		t.Run(scope, func(t *testing.T) {
			_, workspace, _ := sharedWorkspaceFixture(t)
			cs := newCredentialTraceServer(t)
			store := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(store, []byte(strings.Replace(cs.srv.URL, "https://", "https://scoped:scoped-secret@", 1)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			url := cs.srv.URL
			if scope == "full URL" {
				url = cs.repoURL()
			}
			cs.useAmbientConfig(t, "[credential \""+url+"\"]\n\thelper = store --file="+store+"\n")

			// The checks run before the contrast: when the endpoint rejects a
			// stored credential, git erases it from the store, so a plain
			// clone first would leave nothing for the checks to find.
			_ = CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "", HostGitHardening{NoCredentialHelpers: true})
			_, _ = PullSharedWorkspaceHardened(workspace, cs.repoURL(), "", HostGitHardening{NoCredentialHelpers: true})
			if got := cs.seen(); len(got) != 0 {
				t.Errorf("NoCredentialHelpers offered %q", got)
			}
			if runs := cs.askpassRuns(); runs != "" {
				t.Errorf("git ran GIT_ASKPASS:\n%s", runs)
			}
			_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), cs.repoURL(), "", "")
			if got := cs.seen(); len(got) == 0 || got[0] != "scoped:scoped-secret" {
				t.Fatalf("a plain clone must use the scoped helper (harness), got %q", got)
			}
		})
	}
}

// With NoCredentialHelpers an https clone and pull of a repository that
// needs no credential still succeed (the CA from the host user's git config
// still applies).
func TestNoCredentialHelpers_UnauthenticatedCloneAndPullSucceed(t *testing.T) {
	source := setupGitRepo(t)
	url := newAuthedDumbHTTPRepo(t, source, "")
	h := HostGitHardening{
		Config:              []string{"protocol.allow=never", "protocol.file.allow=never", "protocol.https.allow=always", "protocol.http.allow=never"},
		Env:                 []string{"GIT_ALLOW_PROTOCOL=https:ssh:git"},
		NoCredentialHelpers: true,
	}
	dest := filepath.Join(t.TempDir(), "ws")
	if err := CloneSharedWorkspaceHardened(dest, url, "", "", h); err != nil {
		t.Fatalf("unauthenticated clone: %v", err)
	}
	if _, err := PullSharedWorkspaceHardened(dest, url, "", h); err != nil {
		t.Fatalf("unauthenticated pull: %v", err)
	}
}

// git applies no URL-scoped setting to a URL it cannot normalize (a "/../"
// segment above the root), so only the generic http.extraHeader applies
// there. With NoCredentialHelpers it is not sent either, on clone and pull;
// without it, it is.
func TestNoCredentialHelpers_ResetsGenericExtraHeaderForUnnormalizedURL(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	cs := newCredentialTraceServer(t)
	cs.useAmbientConfig(t, cs.ambientHeaderConfig())
	url := cs.srv.URL + "/../org/repo.git"

	_ = CloneSharedWorkspace(filepath.Join(t.TempDir(), "ws"), url, "", "")
	if got := cs.seenHeaders(); !slices.Contains(got, "generic") {
		t.Fatalf("a plain clone must send the generic ambient header (harness), got %q", got)
	}
	n := len(cs.seenHeaders())
	_ = CloneSharedWorkspaceHardened(filepath.Join(t.TempDir(), "ws"), url, "", "", HostGitHardening{NoCredentialHelpers: true})
	_, _ = PullSharedWorkspaceHardened(workspace, url, "", HostGitHardening{NoCredentialHelpers: true})
	if extra := cs.seenHeaders()[n:]; len(extra) != 0 {
		t.Errorf("NoCredentialHelpers sent ambient headers %q", extra)
	}
}
