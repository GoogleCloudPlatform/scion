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

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// Credential values the backstop clone must never send. agentToken is the
// agent's resolved GITHUB_TOKEN (createSharedPlainAgentForURL); the others
// are the broker host's ambient git authentication set up below.
const (
	backstopAgentToken    = "tok-123"
	backstopEnvToken      = "broker-env-token"
	backstopStoreSecret   = "ambient-store-secret"
	backstopHelperSecret  = "ambient-helper-secret"
	backstopHeaderSecret  = "ambient-header-secret"
	backstopAskpassSecret = "ambient-askpass-secret"
)

// backstopCredentialServer is an https git endpoint that requires
// credentials (every request gets 401 Basic) and records, per request, the
// request URI and every header, so a test can check that no credential of
// any form reached it.
type backstopCredentialServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []string // "<method> <request URI>\n<header: value>..." per request
	auth     []string // Authorization values, Basic decoded
}

func (s *backstopCredentialServer) seen() (requests, auth []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...), append([]string(nil), s.auth...)
}

// The shared-plain backstop clone (provisionSharedPlainWorkspace, then the
// real provision.ProvisionShared and its hardened host-side git clone) of a
// private https repository offers no credential of any form, although the
// agent carries a git token, the broker process has GITHUB_TOKEN set and the
// broker host's git configuration holds a credential store, a credential
// helper, extra headers at every URL scope and an askpass program: the
// server sees no Authorization header, no credential in the request URI or
// any header, no helper or askpass runs, and the start fails closed (a
// server error, no sentinel, no container start).
//
// This pins the combination of the backstop passing no token and no token
// remote (ptone/scion#4290) with the hardened clone's no-credential-helper
// environment for an unbound https URL (util.NoCredentialHelperEnv): giving
// the backstop the agent's token and its URL as the token remote again, or
// dropping the no-credential-helper environment, makes it fail.
func TestCreateAgent_SharedPlainBackstopOffersNoCredential(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for this test: %v", err)
	}
	f := newSharedRootFixture(t, true)
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv(provision.GitTokenEnv, backstopEnvToken)

	cs := &backstopCredentialServer{}
	cs.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(r.Method + " " + r.RequestURI + "\n")
		for k, vs := range r.Header {
			for _, v := range vs {
				b.WriteString(k + ": " + v + "\n")
			}
		}
		cs.mu.Lock()
		cs.requests = append(cs.requests, b.String())
		for _, h := range r.Header.Values("Authorization") {
			decoded := h
			if raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); err == nil {
				decoded = string(raw)
			}
			cs.auth = append(cs.auth, decoded)
		}
		cs.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(cs.srv.Close)
	cloneURL := cs.srv.URL + "/org/repo.git"

	// The broker host's ambient git authentication, all for this server.
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	writeBackstopFile(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cs.srv.Certificate().Raw})), 0o600)
	store := filepath.Join(dir, "credentials")
	writeBackstopFile(t, store, strings.Replace(cs.srv.URL, "https://", "https://ambient:"+backstopStoreSecret+"@", 1)+"\n", 0o600)
	helperLog := filepath.Join(dir, "helper.log")
	helper := filepath.Join(dir, "helper")
	writeBackstopFile(t, helper, "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+helperLog+"'\n"+
		"[ \"$1\" = get ] && { echo username=helper; echo password="+backstopHelperSecret+"; }\nexit 0\n", 0o700)
	askpassLog := filepath.Join(dir, "askpass.log")
	askpass := filepath.Join(dir, "askpass")
	writeBackstopFile(t, askpass, "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+askpassLog+"'\necho "+backstopAskpassSecret+"\n", 0o700)
	gitconfig := filepath.Join(dir, "gitconfig")
	writeBackstopFile(t, gitconfig, "[http]\n\tsslCAInfo = "+ca+"\n\textraHeader = X-Ambient: generic-"+backstopHeaderSecret+"\n"+
		"[http \""+cs.srv.URL+"/\"]\n\textraHeader = X-Ambient: host-"+backstopHeaderSecret+"\n"+
		"[http \""+cloneURL+"\"]\n\textraHeader = X-Ambient: full-"+backstopHeaderSecret+"\n"+
		"[credential]\n\thelper = store --file="+store+"\n\thelper = "+helper+"\n", 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", askpass)

	w := createSharedPlainAgentForURL(t, f, cloneURL)

	requests, auth := cs.seen()
	if len(requests) == 0 {
		t.Fatalf("fixture check: the backstop clone never reached the credential-requiring server (create = %d %s)", w.Code, w.Body.String())
	}
	if len(auth) != 0 {
		t.Errorf("the server saw Authorization credentials %q, want none", auth)
	}
	secrets := []string{backstopAgentToken, backstopEnvToken, backstopStoreSecret, backstopHelperSecret, backstopHeaderSecret, backstopAskpassSecret}
	for i, req := range requests {
		if strings.Contains(req, "X-Ambient:") {
			t.Errorf("request %d carried an ambient extra header:\n%s", i, req)
		}
		for _, s := range secrets {
			if strings.Contains(req, s) {
				t.Errorf("request %d carried credential %q:\n%s", i, s, req)
			}
		}
	}
	if b, _ := os.ReadFile(helperLog); len(b) != 0 {
		t.Errorf("the host's credential helper ran (%q), want never asked", b)
	}
	if b, _ := os.ReadFile(askpassLog); len(b) != 0 {
		t.Errorf("askpass ran (%q), want never", b)
	}

	if w.Code < 500 {
		t.Fatalf("create = %d %s, want the provisioning failure", w.Code, w.Body.String())
	}
	// The client sees only a generic server error (the provisioning error is
	// logged by the broker, not returned), so its text is not asserted.
	body := w.Body.String()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Errorf("credential %q appears in the response: %s", s, body)
		}
	}
	if f.mgr.startCalled {
		t.Error("the agent must not start without its workspace")
	}
	if _, err := os.Stat(filepath.Join(f.root, "provision", ".scion-provisioned")); !os.IsNotExist(err) {
		t.Errorf("no sentinel after a failed clone (err %v)", err)
	}
}

func writeBackstopFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
