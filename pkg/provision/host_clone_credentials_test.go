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

package provision

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostCloneTrace is an https endpoint answering every request 401 (Basic)
// that records decoded Authorization credentials and X-Ambient header
// values. GIT_SSL_CAINFO trusts it. The host user's git config
// (GIT_CONFIG_GLOBAL) holds ambient credentials for it: a credential store
// answering ambient:ambient-secret, and an X-Ambient extra header at the
// generic, host and full-URL scopes. GIT_ASKPASS names a script that logs
// its runs.
type hostCloneTrace struct {
	srv        *httptest.Server
	mu         sync.Mutex
	auth       []string
	headers    []string
	askpassLog string
	// reqs counts every request, so a no-credential assertion can first show
	// git reached the server.
	reqs int
}

func newHostCloneTrace(t *testing.T) *hostCloneTrace {
	t.Helper()
	_, err := exec.LookPath("git")
	require.NoError(t, err, "git is required for this test")
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv(GitTokenEnv, "")
	tr := &hostCloneTrace{}
	tr.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.mu.Lock()
		tr.reqs++
		if h := r.Header.Get("Authorization"); h != "" {
			decoded := h
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); err == nil {
				decoded = string(b)
			}
			tr.auth = append(tr.auth, decoded)
		}
		tr.headers = append(tr.headers, r.Header.Values("X-Ambient")...)
		tr.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(tr.srv.Close)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tr.srv.Certificate().Raw}), 0o600))
	store := filepath.Join(dir, "credentials")
	require.NoError(t, os.WriteFile(store, []byte(strings.Replace(tr.srv.URL, "https://", "https://ambient:ambient-secret@", 1)+"\n"), 0o600))
	cfg := filepath.Join(dir, "gitconfig")
	body := "[http]\n\textraHeader = X-Ambient: generic\n" +
		"[http \"" + tr.srv.URL + "/\"]\n\textraHeader = X-Ambient: host\n" +
		"[http \"" + tr.url() + "\"]\n\textraHeader = X-Ambient: full\n" +
		"[credential]\n\thelper = store --file=" + store + "\n"
	require.NoError(t, os.WriteFile(cfg, []byte(body), 0o600))
	t.Setenv("GIT_SSL_CAINFO", ca)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tr.askpassLog = filepath.Join(dir, "askpass.log")
	askpass := filepath.Join(dir, "askpass")
	require.NoError(t, os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+tr.askpassLog+"'\necho askpass-secret\n"), 0o700))
	t.Setenv("GIT_ASKPASS", askpass)
	return tr
}

func (tr *hostCloneTrace) url() string { return tr.srv.URL + "/org/repo.git" }

func (tr *hostCloneTrace) clone(t *testing.T, in ProvisionInput) {
	t.Helper()
	tr.cloneURL(t, in, tr.url())
}

func (tr *hostCloneTrace) cloneURL(t *testing.T, in ProvisionInput, url string) {
	t.Helper()
	in.GitClone = &api.GitCloneConfig{URL: url}
	in.Resolved = ResolvedWorkspace{HostPath: filepath.Join(t.TempDir(), "dest")}
	require.Error(t, runGitClone(context.Background(), in), "the 401 endpoint cannot be cloned")
}

func (tr *hostCloneTrace) seen() (auth, headers []string, askpass string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	b, _ := os.ReadFile(tr.askpassLog)
	return append([]string(nil), tr.auth...), append([]string(nil), tr.headers...), string(b)
}

// assertNothingOffered fails t if the trace saw any credential, ambient
// header or askpass run.
func (tr *hostCloneTrace) assertNothingOffered(t *testing.T) {
	t.Helper()
	tr.mu.Lock()
	reqs := tr.reqs
	tr.mu.Unlock()
	require.Positive(t, reqs, "git never reached the trace server; a no-credential assertion would be vacuous")
	auth, headers, askpass := tr.seen()
	assert.Empty(t, auth, "no credential may be offered")
	assert.Empty(t, headers, "no ambient extra header may be sent")
	assert.Empty(t, askpass, "askpass must not run")
}

// Control: plain host git offers the trace's ambient credential and sends
// its extra headers, so the no-credential assertions below are not vacuous.
func TestHostCloneTrace_PlainGitOffersAmbient(t *testing.T) {
	tr := newHostCloneTrace(t)
	cmd := exec.Command("git", "clone", "-q", "--", tr.url(), filepath.Join(t.TempDir(), "dest"))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = cmd.Run()
	auth, headers, _ := tr.seen()
	require.NotEmpty(t, auth)
	assert.Equal(t, "ambient:ambient-secret", auth[0])
	assert.Subset(t, headers, []string{"generic", "host", "full"})
}

// A hardened clone (the hub's shared-backend clone) with no token offered
// offers no credential from the host's helpers, sends none of its extra
// headers at any URL scope, and runs no askpass, whether or not the URL is
// the project's recorded remote. The environment git runs with, captured at
// exec, is the remote layer with no credential helper in effect.
func TestRunGitClone_HardenedUnboundOffersNoHostCredentials(t *testing.T) {
	for _, tc := range []struct{ name, remote string }{
		{"other recorded remote", "github.com/org/repo"},
		{"no recorded remote", ""},
		{"bound recorded remote, no token", "self"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newHostCloneTrace(t)
			remote := tc.remote
			if remote == "self" {
				remote = tr.url()
			}
			in := ProvisionInput{HostGitHardened: true, CloneTokenRemote: remote}
			tr.clone(t, in)
			tr.assertNothingOffered(t)

			env := captureCloneEnv(t, in, tr.url())
			checkRemoteLayerEnv(t, env, "", "")
		})
	}
}

// The recorded remote gets the project's token only.
func TestRunGitClone_HardenedBoundOffersTokenOnly(t *testing.T) {
	tr := newHostCloneTrace(t)
	tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: tr.url(), CloneToken: "project-token"})
	auth, headers, askpass := tr.seen()
	require.NotEmpty(t, auth)
	for _, a := range auth {
		assert.Equal(t, "oauth2:project-token", a)
	}
	assert.Empty(t, headers)
	assert.Empty(t, askpass)
}

// A clone that is not hardened (broker-side provisioning, and `sciontool
// provision` in the Kubernetes init container) offers no host credential
// either.
func TestRunGitClone_NotHardenedOffersNoHostCredentials(t *testing.T) {
	tr := newHostCloneTrace(t)
	in := ProvisionInput{CloneTokenRemote: "github.com/org/repo"}
	tr.clone(t, in)
	tr.assertNothingOffered(t)
	checkRemoteLayerEnv(t, captureCloneEnv(t, in, tr.url()), "", "")
}

// For a URL git cannot normalize (a "/../" segment above the root) git
// applies only the generic http.extraHeader; the hardened unbound clone
// does not send it either.
func TestRunGitClone_HardenedUnboundUnnormalizedURL(t *testing.T) {
	tr := newHostCloneTrace(t)
	tr.cloneURL(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: "github.com/org/repo"}, tr.srv.URL+"/../org/repo.git")
	tr.assertNothingOffered(t)
}
