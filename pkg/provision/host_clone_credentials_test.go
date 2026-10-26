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
// values. The host user's git config (GIT_CONFIG_GLOBAL) trusts it and holds
// ambient credentials for it: a credential store answering
// ambient:ambient-secret, and an X-Ambient extra header at the generic,
// host and full-URL scopes. GIT_ASKPASS names a script that logs its runs.
type hostCloneTrace struct {
	srv        *httptest.Server
	mu         sync.Mutex
	auth       []string
	headers    []string
	askpassLog string
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
	body := "[http]\n\tsslCAInfo = " + ca + "\n\textraHeader = X-Ambient: generic\n" +
		"[http \"" + tr.srv.URL + "/\"]\n\textraHeader = X-Ambient: host\n" +
		"[http \"" + tr.url() + "\"]\n\textraHeader = X-Ambient: full\n" +
		"[credential]\n\thelper = store --file=" + store + "\n"
	require.NoError(t, os.WriteFile(cfg, []byte(body), 0o600))
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

// A hardened clone (the hub's shared-backend clone) of an https URL that is
// not the project's recorded remote offers no credential from the host's
// helpers, sends none of its extra headers at any URL scope, and runs no
// askpass.
func TestRunGitClone_HardenedUnboundOffersNoHostCredentials(t *testing.T) {
	for _, remote := range []string{"github.com/org/repo", ""} {
		t.Run("recorded remote "+remote, func(t *testing.T) {
			tr := newHostCloneTrace(t)
			tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: remote})
			auth, headers, askpass := tr.seen()
			assert.Empty(t, auth, "no credential may be offered")
			assert.Empty(t, headers, "no ambient extra header may be sent")
			assert.Empty(t, askpass, "askpass must not run")
		})
	}
}

// The recorded remote gets the project's token only; with no token it keeps
// the host's own git authentication (unchanged behaviour).
func TestRunGitClone_HardenedBoundUnchanged(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		tr := newHostCloneTrace(t)
		tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: tr.url(), CloneToken: "project-token"})
		auth, _, askpass := tr.seen()
		require.NotEmpty(t, auth)
		for _, a := range auth {
			assert.Equal(t, "oauth2:project-token", a)
		}
		assert.Empty(t, askpass)
	})
	t.Run("no token", func(t *testing.T) {
		tr := newHostCloneTrace(t)
		tr.clone(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: tr.url()})
		auth, _, _ := tr.seen()
		require.NotEmpty(t, auth)
		assert.Equal(t, "ambient:ambient-secret", auth[0])
	})
}

// A clone that is not hardened (broker-side provisioning) is unchanged: the
// host's helpers still answer.
func TestRunGitClone_NotHardenedUnchanged(t *testing.T) {
	tr := newHostCloneTrace(t)
	tr.clone(t, ProvisionInput{CloneTokenRemote: "github.com/org/repo"})
	auth, headers, _ := tr.seen()
	require.NotEmpty(t, auth)
	assert.Equal(t, "ambient:ambient-secret", auth[0])
	assert.NotEmpty(t, headers, "the host's extra headers still apply")
}

// For a URL git cannot normalize (a "/../" segment above the root) git
// applies only the generic http.extraHeader; the hardened unbound clone
// does not send it either.
func TestRunGitClone_HardenedUnboundUnnormalizedURL(t *testing.T) {
	tr := newHostCloneTrace(t)
	tr.cloneURL(t, ProvisionInput{HostGitHardened: true, CloneTokenRemote: "github.com/org/repo"}, tr.srv.URL+"/../org/repo.git")
	auth, headers, askpass := tr.seen()
	assert.Empty(t, auth)
	assert.Empty(t, headers)
	assert.Empty(t, askpass)
}
