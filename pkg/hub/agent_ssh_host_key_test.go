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

//go:build !no_sqlite && (!hubshard || hubshard_3)

package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// sshHostKeyFixture is an agent with a current run, and a host key line.
type sshHostKeyFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	agent   *store.Agent
	key     string
}

func newSSHHostKeyFixture(t *testing.T, conduitOn bool) sshHostKeyFixture {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	setConduitExperiment(t, srv, conduitOn)
	a := newReincarnateTestAgent(t, s, project, broker, nil)
	_, err := s.SetAgentRunID(context.Background(), a.ID, "run-current", nil)
	require.NoError(t, err)
	return sshHostKeyFixture{srv: srv, store: s, project: project, agent: a, key: ed25519KeyLine(t)}
}

// agentClaimsCtx is a request context for the agent's own token, with
// run claim runID ("" for a token without one).
func agentClaimsCtx(ctx context.Context, a *store.Agent, runID string, scopes ...AgentTokenScope) context.Context {
	claims := &AgentTokenClaims{Claims: jwt.Claims{Subject: a.ID}, ProjectID: a.ProjectID, Scopes: scopes, RunID: runID}
	ctx = context.WithValue(ctx, agentContextKey{}, claims)
	return contextWithIdentity(ctx, &agentIdentityWrapper{claims})
}

// postHostKey posts {"sshHostKey": key} as the agent and returns the code.
func (f sshHostKeyFixture) postHostKey(t *testing.T, claimRunID, header, key string) int {
	t.Helper()
	body, err := json.Marshal(map[string]string{"sshHostKey": key})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", bytes.NewReader(body))
	if header != "" {
		req.Header.Set(AgentRunIDHeader, header)
	}
	req = req.WithContext(agentClaimsCtx(req.Context(), f.agent, claimRunID, ScopeAgentStatusUpdate))
	rec := httptest.NewRecorder()
	f.srv.updateAgentStatus(rec, req, f.agent.ID)
	return rec.Code
}

// getHostKey reads the agent as itself and returns the decoded body.
func (f sshHostKeyFixture) getAgentJSON(t *testing.T) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+f.agent.ID, nil)
	req = req.WithContext(agentClaimsCtx(req.Context(), f.agent, "run-current", ScopeProjectRead))
	rec := httptest.NewRecorder()
	f.srv.getAgent(rec, req, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (f sshHostKeyFixture) storedKey(t *testing.T) (key, runID string) {
	t.Helper()
	a, err := f.store.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	return a.SSHHostKey, a.SSHHostKeyRunID
}

// TestSSHHostKey_BoundToCurrentRun: the reported host key is bound to the
// reporting agent's current run, for tokens with and without a run claim.
func TestSSHHostKey_BoundToCurrentRun(t *testing.T) {
	for _, tc := range []struct {
		name          string
		claim, header string
		stored        bool
	}{
		{"matching claim, no header", "run-current", "", true},
		{"matching claim and header", "run-current", "run-current", true},
		{"claim and header differ", "run-current", "run-other", false},
		{"older-run claim with the current run's header", "run-old", "run-current", false},
		{"older-run claim", "run-old", "", false},
		{"no claim, header is the current run", "", "run-current", true},
		{"no claim, header is an older run", "", "run-old", false},
		{"no claim, no header", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSHHostKeyFixture(t, true)
			require.Equal(t, http.StatusOK, f.postHostKey(t, tc.claim, tc.header, f.key))
			key, runID := f.storedKey(t)
			if !tc.stored {
				assert.Empty(t, key)
				assert.Empty(t, runID)
				return
			}
			assert.Equal(t, f.key, key)
			assert.Equal(t, "run-current", runID)
		})
	}
}

// TestSSHHostKey_ServedOnGetAgent: a recorded key round-trips to the
// single-agent GET under the exact JSON name sshHostKey, and is no longer
// served once the agent moves to another run.
func TestSSHHostKey_ServedOnGetAgent(t *testing.T) {
	f := newSSHHostKeyFixture(t, true)
	assert.NotContains(t, f.getAgentJSON(t), "sshHostKey", "nothing recorded yet")

	require.Equal(t, http.StatusOK, f.postHostKey(t, "run-current", "run-current", f.key))
	got := f.getAgentJSON(t)
	assert.Equal(t, f.key, got["sshHostKey"])
	for k := range got {
		if strings.EqualFold(k, "sshhostkey") || strings.EqualFold(k, "ssh_host_key") {
			assert.Equal(t, "sshHostKey", k, "the JSON field name is pinned")
		}
	}

	_, err := f.store.SetAgentRunID(context.Background(), f.agent.ID, "run-next", nil)
	require.NoError(t, err)
	assert.NotContains(t, f.getAgentJSON(t), "sshHostKey", "a key from another run is not served")
}

// TestSSHHostKey_FieldName pins the wire name of the response field.
func TestSSHHostKey_FieldName(t *testing.T) {
	b, err := json.Marshal(AgentWithCapabilities{Agent: store.Agent{ID: "a"}, SSHHostKey: "ssh-ed25519 AAAA"})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"sshHostKey":"ssh-ed25519 AAAA"`)
	var back AgentWithCapabilities
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, "ssh-ed25519 AAAA", back.SSHHostKey)
}

// TestSSHHostKey_ConduitOff: with the conduit experiment off, a reported
// key is not stored, and a stored key is not served.
func TestSSHHostKey_ConduitOff(t *testing.T) {
	f := newSSHHostKeyFixture(t, false)
	require.Equal(t, http.StatusOK, f.postHostKey(t, "run-current", "run-current", f.key))
	key, _ := f.storedKey(t)
	assert.Empty(t, key, "not stored with conduit off")

	res, err := f.store.SetAgentSSHHostKey(context.Background(), f.agent.ID, "run-current", f.key)
	require.NoError(t, err)
	require.Equal(t, store.SSHHostKeyWritten, res)
	assert.NotContains(t, f.getAgentJSON(t), "sshHostKey", "not served with conduit off")
}

// TestSSHHostKey_Ignored: a malformed key, a certificate, a non-ed25519
// key, a key with options, private key material, an oversized value, and
// a caller other than the agent itself (another agent, or a user with
// update access) store nothing.
func TestSSHHostKey_Ignored(t *testing.T) {
	f := newSSHHostKeyFixture(t, true)
	for _, bad := range []string{
		"not a key",
		sshCertLine(t),
		rsaKeyLine(t),
		`command="true" ` + f.key,
		f.key + "\n" + f.key,
		"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----",
		f.key + strings.Repeat(" x", 3000),
	} {
		require.Equal(t, http.StatusOK, f.postHostKey(t, "run-current", "", bad))
		key, _ := f.storedKey(t)
		assert.Empty(t, key, "%.30q stored", bad)
	}

	// Another agent of the same project reporting for this one is refused
	// by the handler before anything is read.
	other := *f.agent
	other.ID = "11111111-1111-1111-1111-111111111111"
	body, err := json.Marshal(map[string]string{"sshHostKey": f.key})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", bytes.NewReader(body))
	req = req.WithContext(agentClaimsCtx(req.Context(), &other, "run-current", ScopeAgentStatusUpdate))
	rec := httptest.NewRecorder()
	f.srv.updateAgentStatus(rec, req, f.agent.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	key, _ := f.storedKey(t)
	assert.Empty(t, key)

	// A user with update access may post the agent's status, but the
	// host key is recorded only from the agent itself.
	updater := newReincarnateAuthzUser(t, f.store, "ssh-host-key-updater")
	grantPermissionViaRoleBinding(t, f.store, updater.ID, "agent.update", store.RoleScopeProject, f.project.ID)
	updaterIdent := userIdentityFor(updater)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", bytes.NewReader(body))
	req.Header.Set(AgentRunIDHeader, "run-current")
	req = req.WithContext(contextWithIdentity(req.Context(), updaterIdent))
	rec = httptest.NewRecorder()
	f.srv.updateAgentStatus(rec, req, f.agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	key, _ = f.storedKey(t)
	assert.Empty(t, key)
}

// TestSSHHostKey_FirstKeyPerRunKept: the first host key reported for a run
// is kept; a repeat of the same key succeeds; a different key for the same
// run is not recorded and the first is still served; a new run accepts a
// new key.
func TestSSHHostKey_FirstKeyPerRunKept(t *testing.T) {
	f := newSSHHostKeyFixture(t, true)
	other := ed25519KeyLine(t)
	require.Equal(t, http.StatusOK, f.postHostKey(t, "run-current", "", f.key))
	require.Equal(t, http.StatusOK, f.postHostKey(t, "run-current", "", f.key))
	key, run := f.storedKey(t)
	assert.Equal(t, f.key, key, "a repeat of the same key keeps it")
	assert.Equal(t, "run-current", run)

	require.Equal(t, http.StatusOK, f.postHostKey(t, "run-current", "", other))
	key, _ = f.storedKey(t)
	assert.Equal(t, f.key, key, "a different key for the same run is not recorded")
	assert.Equal(t, f.key, f.getAgentJSON(t)["sshHostKey"], "the first key is still served")

	_, err := f.store.SetAgentRunID(context.Background(), f.agent.ID, "run-next", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, f.postHostKey(t, "run-next", "", other))
	key, run = f.storedKey(t)
	assert.Equal(t, other, key, "a new run accepts a new key")
	assert.Equal(t, "run-next", run)
}

// ed25519KeyLine is a fresh ed25519 public key line.
func ed25519KeyLine(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sp, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}

// rsaKeyLine is a fresh RSA public key line.
func rsaKeyLine(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	sp, err := ssh.NewPublicKey(&k.PublicKey)
	require.NoError(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}

// sshCertLine is an ed25519 host certificate line.
func sshCertLine(t *testing.T) string {
	t.Helper()
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ca, err := ssh.NewSignerFromKey(caPriv)
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sp, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	cert := &ssh.Certificate{Key: sp, CertType: ssh.HostCert, ValidPrincipals: []string{"agent"}, ValidBefore: ssh.CertTimeInfinity}
	require.NoError(t, cert.SignCert(rand.Reader, ca))
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
}

// TestNormalizeSSHHostKey: one public key line, re-encoded without a
// comment.
func TestNormalizeSSHHostKey(t *testing.T) {
	f := newSSHHostKeyFixture(t, true)
	got, ok := normalizeSSHHostKey(f.key + " agent@host")
	require.True(t, ok)
	assert.Equal(t, f.key, got)
	_, ok = normalizeSSHHostKey("")
	assert.False(t, ok)
}
