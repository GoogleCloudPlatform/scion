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

//go:build !windows

package provision

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckHostCloneURL(t *testing.T) {
	accepted := []string{
		"https://github.com/org/repo.git",
		"https://user@git.example.com:8443/org/repo",
		"ssh://git@github.com/org/repo.git",
		"ssh://git@[2001:db8::1]:22/org/repo.git",
		"git://git.example.com/org/repo.git",
		"git@github.com:org/repo.git",
		"github.com:org/repo.git",
		"git@[2001:db8::1]:org/repo.git",
	}
	for _, u := range accepted {
		assert.NoError(t, CheckHostCloneURL(u), u)
	}
	refused := []string{
		"",
		"/srv/repos/repo.git",
		"./repo",
		"../repo",
		"repo",
		"~/repo.git",
		"C:/repos/repo",
		`C:\repos\repo`,
		"file:///srv/repos/repo.git",
		"FILE:///srv/repos/repo.git",
		"http://github.com/org/repo.git",
		"ftp://example.com/repo.git",
		"HTTPS://github.com/org/repo.git",
		"ext::sh -c touch% /tmp/pwned",
		"fd::17",
		"https::github.com/org/repo",
		"-uhttps://github.com/org/repo",
		"--upload-pack=touch /tmp/pwned",
		" https://github.com/org/repo.git",
		"https://github.com/org/repo.git\n",
		"https://github.com/org/\x00repo",
		"ssh://-oProxyCommand=touch%20pwned/repo",
		"ssh://-oProxyCommand=x@host/repo",
		"-oProxyCommand=x:repo",
		"https:///org/repo",
		"host:",
		"/abs/path:with-colon",
		"dir/sub:path",
		"EXT::sh -c x",
		"x+y.z-1::addr",
		"https://ext::git-remote-probe.git",
	}
	for _, u := range refused {
		err := CheckHostCloneURL(u)
		require.Error(t, err, "%q must be refused", u)
		assert.True(t, errors.Is(err, ErrHostCloneURLRefused), u)
		if len(u) > 8 {
			assert.NotContains(t, err.Error(), u, "the refusal never echoes the URL")
		}
	}
}

// fakeGit puts a git on PATH that records its argv (one per line) and the
// GIT_ALLOW_PROTOCOL it ran with, then fails, and returns the record path.
func fakeGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + record + "'\n" +
		"printf 'ALLOW=%s\\n' \"$GIT_ALLOW_PROTOCOL\" >> '" + record + "'\n" +
		"printf 'TOKEN=%s\\n' \"$GITHUB_TOKEN\" >> '" + record + "'\n" +
		"printf 'SCOPED=%s\\n' \"$SCION_SCOPED_GIT_CREDENTIAL\" >> '" + record + "'\n" +
		"printf 'CFG=%s\\n' \"$GIT_CONFIG_COUNT\" >> '" + record + "'\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_ALLOW_PROTOCOL", "")
	t.Setenv(GitTokenEnv, "")
	// The token is delivered through util.ScopedCredentialHelperEnv, which carries
	// it in SCION_SCOPED_GIT_CREDENTIAL (not GITHUB_TOKEN) and installs a
	// host-scoped credential helper; clear any inherited value first.
	t.Setenv("SCION_SCOPED_GIT_CREDENTIAL", "")
	t.Setenv("GIT_CONFIG_COUNT", "")
	return record
}

func readArgv(t *testing.T, record string) (argv []string, allow string) {
	t.Helper()
	data, err := os.ReadFile(record)
	require.NoError(t, err)
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if v, ok := strings.CutPrefix(l, "ALLOW="); ok {
			allow = v
			continue
		}
		if strings.HasPrefix(l, "TOKEN=") || strings.HasPrefix(l, "SCOPED=") || strings.HasPrefix(l, "CFG=") {
			continue
		}
		argv = append(argv, l)
	}
	return argv, allow
}

// runGitClone ends the options with "--" before the URL and path, and a
// hardened clone carries HostCloneGitArgs ahead of "clone" and
// GIT_ALLOW_PROTOCOL=https:ssh:git.
func TestRunGitClone_ArgvEndsOptionsBeforeURL(t *testing.T) {
	const cloneURL = "https://git.example.com/org/repo.git"
	dest := filepath.Join(t.TempDir(), "ws")
	depth := 0

	for _, hardened := range []bool{false, true} {
		record := fakeGit(t)
		err := runGitClone(context.Background(), ProvisionInput{
			GitClone:        &api.GitCloneConfig{URL: cloneURL, Branch: "main", Depth: &depth},
			Resolved:        ResolvedWorkspace{HostPath: dest},
			HostGitHardened: hardened,
		})
		require.Error(t, err, "the fake git fails")
		argv, allow := readArgv(t, record)

		want := []string{}
		if hardened {
			want = append(want, HostCloneGitArgs()...)
		} else {
			// Union: a non-hardened clone still carries the base host-side git pins.
			want = append(want, util.HostGitArgs()...)
		}
		want = append(want, "clone", "--branch", "main", "--", cloneURL, dest)
		assert.Equal(t, want, argv, "hardened=%v", hardened)
		if hardened {
			assert.Equal(t, HostCloneProtocols, allow)
		} else {
			assert.Empty(t, allow)
		}
	}
}

// A non-hardened ProvisionShared clone still carries the base host-side git pins
// unconditionally (no repository hooks, no fsmonitor, restricted ext transport),
// so a cloned repository's config cannot change the clone's behaviour. Dropping the
// unconditional pins (the else branch in runGitClone) makes this fail.
func TestRunGitClone_NonHardenedCarriesBasePins(t *testing.T) {
	const cloneURL = "https://git.example.com/org/repo.git"
	dest := filepath.Join(t.TempDir(), "ws")
	depth := 0
	record := fakeGit(t)
	err := runGitClone(context.Background(), ProvisionInput{
		GitClone:        &api.GitCloneConfig{URL: cloneURL, Depth: &depth},
		Resolved:        ResolvedWorkspace{HostPath: dest},
		HostGitHardened: false,
	})
	require.Error(t, err, "the fake git fails")
	argv, _ := readArgv(t, record)
	joined := strings.Join(argv, " ")
	for _, pin := range []string{
		"core.hooksPath=" + os.DevNull,
		"core.fsmonitor=false",
		"protocol.ext.allow=never",
	} {
		assert.Contains(t, joined, pin, "a non-hardened clone must carry the base pin %q", pin)
	}
}

func TestHostCloneGitArgs_PinsTransports(t *testing.T) {
	args := HostCloneGitArgs()
	assert.Equal(t, hostGitArgs(), args[:len(hostGitArgs())], "starts with the host-side git settings")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "protocol.ext.allow=never",
		"protocol.allow=never", "protocol.https.allow=always", "protocol.ssh.allow=always", "protocol.git.allow=always",
	} {
		assert.Contains(t, joined, want)
	}
	assert.NotContains(t, joined, "protocol.http.allow", "plain http is not allowed")
	fileUser := strings.Index(joined, "protocol.file.allow=user")
	fileNever := strings.LastIndex(joined, "protocol.file.allow=never")
	assert.Greater(t, fileNever, fileUser, "the later file=never overrides file=user")
}

// Defence in depth below the URL check: a hardened clone of a local path or
// a file:// URL is refused by git itself and populates nothing.
func TestRunGitClone_HardenedRefusesLocalTransports(t *testing.T) {
	isolateGitConfig(t)
	bare := initBareGitRepo(t)
	depth := 0
	for _, src := range []string{bare, "file://" + bare} {
		dest := filepath.Join(t.TempDir(), "ws")
		err := runGitClone(context.Background(), ProvisionInput{
			GitClone:        &api.GitCloneConfig{URL: src, Depth: &depth},
			Resolved:        ResolvedWorkspace{HostPath: dest},
			HostGitHardened: true,
		})
		require.Error(t, err, "%s must be refused", src)
		assert.NoFileExists(t, filepath.Join(dest, "README.md"))
	}

	// The same local source clones without hardening (the existing callers).
	dest := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, runGitClone(context.Background(), ProvisionInput{
		GitClone: &api.GitCloneConfig{URL: bare, Depth: &depth},
		Resolved: ResolvedWorkspace{HostPath: dest},
	}))
	assert.FileExists(t, filepath.Join(dest, "README.md"))
}

func TestCloneTokenHost_BoundToRecordedRemote(t *testing.T) {
	for _, tc := range []struct {
		remote, cloneURL, host string
	}{
		{"github.com/org/repo", "https://github.com/org/repo.git", "github.com"},
		{"github.com/org/repo", "https://github.com/Org/Repo", "github.com"},
		{"github.com/org/repo", "https://GitHub.COM/org/repo.git/", "github.com"},
		{"https://github.com/org/repo.git", "https://github.com/org/repo.git", "github.com"},
		{"ghe.example.com/team/app", "https://ghe.example.com/team/app.git", "ghe.example.com"},
		{"gitlab.example.com:8443/group/sub/app", "https://gitlab.example.com:8443/group/sub/app.git", "gitlab.example.com:8443"},
		{"git@github.com:org/repo.git", "https://github.com/org/repo.git", "github.com"},
	} {
		host, ok := cloneTokenHost(tc.remote, tc.cloneURL)
		assert.True(t, ok, "%s / %s", tc.remote, tc.cloneURL)
		assert.Equal(t, tc.host, host, "%s / %s", tc.remote, tc.cloneURL)
	}
	for _, tc := range []struct{ remote, cloneURL string }{
		{"github.com/org/repo", "https://mirror.example.com/org/repo.git"},      // another host
		{"github.com/org/repo", "https://github.com/other/repo.git"},            // another owner
		{"github.com/org/repo", "https://github.com/org/other.git"},             // another repo
		{"github.com/org/repo", "https://github.com/org/repo/extra.git"},        // another path
		{"github.com/org/repo", "http://github.com/org/repo.git"},               // not https
		{"github.com/org/repo", "ssh://git@github.com/org/repo.git"},            // not https
		{"github.com/org/repo", "git@github.com:org/repo.git"},                  // scp-style
		{"github.com/org/repo", "https://user:pw@github.com/org/repo.git"},      // user info
		{"github.com/org/repo", "https://github.com/org/repo.git?x=1"},          // query
		{"github.com/org/repo", "https://github.com:8443/org/repo.git"},         // another port
		{"github.com:8443/org/repo", "https://github.com/org/repo.git"},         // another port
		{"", "https://github.com/org/repo.git"},                                 // no remote
		{"github.com", "https://github.com"},                                    // no repository path
		{"github.com/org/repo", "https://github.com.evil.example/org/repo.git"}, // look-alike host
	} {
		_, ok := cloneTokenHost(tc.remote, tc.cloneURL)
		assert.False(t, ok, "%s / %s must get no token", tc.remote, tc.cloneURL)
	}
}

// tlsGitServer serves bareRepo over https (dumb HTTP) at /org/<name>,
// demanding the test token, and records every request and whether any
// request presented the token.
type tlsGitServer struct {
	url      string
	host     string
	cert     *x509.Certificate
	requests atomic.Int32
	sawToken atomic.Bool
}

func newTLSGitServer(t *testing.T, bareRepo string) *tlsGitServer {
	t.Helper()
	run(t, "git", "-C", bareRepo, "update-server-info")
	files := http.StripPrefix("/org/", http.FileServer(http.Dir(filepath.Dir(bareRepo))))
	g := &tlsGitServer{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		_, pass, ok := r.BasicAuth()
		if ok && pass == testCloneToken {
			g.sawToken.Store(true)
		}
		if !ok || pass != testCloneToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	g.url = srv.URL + "/org/" + filepath.Base(bareRepo)
	g.host = u.Host
	g.cert = srv.Certificate()
	return g
}

// writeTLSCA writes cert to a PEM file and returns its path, for GIT_SSL_CAINFO.
func writeTLSCA(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	require.NoError(t, os.WriteFile(f, pemBytes, 0o644))
	return f
}

// hardenedTokenClone clones cloneURL as the hub does at create: hardened,
// with the token bound to the project's recorded remote.
func hardenedTokenClone(t *testing.T, cloneURL, recordedRemote string) (string, error) {
	t.Helper()
	depth := 0
	dest := filepath.Join(t.TempDir(), "ws")
	return dest, runGitClone(context.Background(), ProvisionInput{
		GitClone:         &api.GitCloneConfig{URL: cloneURL, Depth: &depth},
		Resolved:         ResolvedWorkspace{HostPath: dest},
		CloneToken:       testCloneToken,
		CloneTokenRemote: recordedRemote,
		HostGitHardened:  true,
	})
}

// remoteOf is the recorded-remote form of a clone URL (host/owner/repo).
func remoteOf(cloneURL string) string {
	return strings.TrimPrefix(cloneURL, "https://")
}

// The token is bound to the project's recorded remote: a clone of that
// remote (a GitHub Enterprise-style host) gets it, scoped to that host; a
// clone URL naming another host than the record (as a clone-url label can),
// or another repository on the recorded host, gets no token; and a redirect
// is not followed, so its target never sees the token.
func TestRunGitClone_TokenBoundToRecordedRemoteAndNoRedirects(t *testing.T) {
	isolateGitConfig(t)
	t.Setenv(GitTokenEnv, "")
	bare := initBareGitRepo(t)

	own := newTLSGitServer(t, bare)
	// HostGitEnv scrubs GIT_SSL_NO_VERIFY (host clones always verify TLS), so the
	// test trusts the httptest cert through GIT_SSL_CAINFO, which HostGitEnv keeps.
	// Every httptest TLS server shares the same certificate, so one CA covers all.
	t.Setenv("GIT_SSL_CAINFO", writeTLSCA(t, own.cert))

	dest, err := hardenedTokenClone(t, own.url, remoteOf(own.url))
	require.NoError(t, err, "the recorded remote's clone gets the token")
	assert.FileExists(t, filepath.Join(dest, "README.md"))
	assert.True(t, own.sawToken.Load())

	// The clone URL (a label) names another host than the record.
	other := newTLSGitServer(t, bare)
	_, err = hardenedTokenClone(t, other.url, "ghe.example.com/org/"+filepath.Base(bare))
	require.Error(t, err, "a host other than the recorded remote's gets no credential")
	assert.NotContains(t, err.Error(), testCloneToken)
	assert.False(t, other.sawToken.Load(), "the token is never offered to another host")
	assert.Positive(t, other.requests.Load(), "fixture check: git reached the server")

	// Same host as the record, another repository.
	sameHost := newTLSGitServer(t, bare)
	_, err = hardenedTokenClone(t, sameHost.url, sameHost.host+"/org/other-repo")
	require.Error(t, err, "another repository on the recorded host gets no credential")
	assert.False(t, sameHost.sawToken.Load())

	target := newTLSGitServer(t, bare)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.TrimSuffix(target.url, "/org/"+filepath.Base(bare))+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	redirectURL := redirect.URL + "/org/" + filepath.Base(bare)
	_, err = hardenedTokenClone(t, redirectURL, remoteOf(redirectURL))
	require.Error(t, err, "a redirect is not followed")
	assert.Zero(t, target.requests.Load(), "the redirect target is never contacted")
	assert.False(t, target.sawToken.Load())
}

// A clone of a non-https URL never gets the token or a credential helper,
// even when it names the recorded remote; the matching https URL gets both.
func TestRunGitClone_TokenNotUsedForNonHTTPS(t *testing.T) {
	depth := 0
	clone := func(cloneURL string) string {
		t.Helper()
		record := fakeGit(t)
		err := runGitClone(context.Background(), ProvisionInput{
			GitClone:         &api.GitCloneConfig{URL: cloneURL, Depth: &depth},
			Resolved:         ResolvedWorkspace{HostPath: filepath.Join(t.TempDir(), "ws")},
			CloneToken:       testCloneToken,
			CloneTokenRemote: "git.example.com/org/repo",
		})
		require.Error(t, err, "the fake git fails")
		data, err := os.ReadFile(record)
		require.NoError(t, err)
		return string(data)
	}
	got := clone("http://git.example.com/org/repo.git")
	assert.Contains(t, got, "SCOPED=\n", "a non-https URL gets no token")
	assert.Contains(t, got, "CFG=\n", "no credential helper")
	assert.Contains(t, got, "TOKEN=\n", "the token never rides in GITHUB_TOKEN")
	got = clone("https://git.example.com/org/repo.git")
	assert.Contains(t, got, "SCOPED="+testCloneToken+"\n", "fixture check: the matching https URL gets the token via the scoped helper env")
	assert.Contains(t, got, "CFG=2\n", "the host-scoped credential helper is installed")
	assert.Contains(t, got, "TOKEN=\n", "the token is delivered via the scoped helper, never GITHUB_TOKEN")
}
