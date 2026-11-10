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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// pendingDropRecorder wraps a secret backend and records the references
// passed to DropPending.
type pendingDropRecorder struct {
	secret.SecretBackend
	dropped []string
}

func (b *pendingDropRecorder) DropPending(ctx context.Context, ref string) error {
	b.dropped = append(b.dropped, ref)
	return b.SecretBackend.DropPending(ctx, ref)
}

// pendingFixture is a start-delivery fixture (owner user U, project P)
// with agent A in P and agent B in another project Q, both created by U
// with recorded principal edges, so their user-scope reads are admitted.
// The fixture's AgentID and Token are agent A's.
type pendingFixture struct {
	*sdFixture
	drops *pendingDropRecorder

	ProjectB    string
	AgentB      string
	TokenB      string
	ProjectName string
}

func newPendingFixture(t *testing.T, name string) *pendingFixture {
	t.Helper()
	f := &pendingFixture{sdFixture: newSDFixture(t, name)}
	f.drops = &pendingDropRecorder{SecretBackend: f.Server.secretBackend}
	f.Server.SetSecretBackend(f.drops)
	f.disp.SetSecretBackend(f.drops)
	ctx := context.Background()

	f.AgentID, f.Token = f.direct(t, "a", true)

	p, err := f.Store.GetProject(ctx, f.ProjectID)
	require.NoError(t, err)
	f.ProjectName = p.Name

	f.ProjectB = tid("project-b-" + name)
	require.NoError(t, f.Store.CreateProject(ctx, &store.Project{
		ID: f.ProjectB, Name: "other-" + name, Slug: "proj-b-" + name, Created: time.Now(), Updated: time.Now(),
	}))
	createDCUser(t, f.Store, f.UserID, name+"@test.com", f.ProjectB, store.ProjectRoleOwner)
	f.AgentB = tid("agent-b-" + name)
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: f.AgentB, Slug: "agent-b-" + name, Name: "Agent B " + name,
		ProjectID: f.ProjectB, Phase: "running", StateVersion: 1,
		CreatedBy: f.UserID, OwnerID: f.UserID, Ancestry: []string{f.UserID},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		Created:       time.Now(), Updated: time.Now(),
	}))
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentB, store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		func(e *store.DelegationEdge) { e.ScopeID = f.ProjectB })
	f.TokenB = mintRecordedMaterialToken(t, f.Server, f.Store, f.AgentB, f.ProjectB, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID})
	return f
}

// agentPut writes key as an agent through the agent route.
func agentPut(t *testing.T, srv *Server, agentID, token, key string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAgentToken(t, srv, http.MethodPut, "/api/v1/agents/"+agentID+"/secrets/"+key, body, token)
}

func userScopeWrite(value string, force bool) AgentSetSecretRequest {
	allow := true
	return AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte(value)),
		Type:  store.SecretTypeFile, Target: "~/.config/tool/creds.json",
		Scope: store.ScopeUser, Force: force, AllowProgeny: &allow,
	}
}

func decodePendingResponse(t *testing.T, rec *httptest.ResponseRecorder) AgentPendingSecretResponse {
	t.Helper()
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp AgentPendingSecretResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// ownerRequest makes a request as user userID on the given credential kind
// (an interactive session unless kind says otherwise).
func ownerRequest(t *testing.T, srv *Server, ident UserIdentity, kind CredentialKind, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), ident)
	ctx = context.WithValue(ctx, userContextKey{}, ident)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: kind})
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func sessionUser(id string) UserIdentity {
	return NewAuthenticatedUser(id, id+"@test.com", "User "+id, "member", "web")
}

func (f *pendingFixture) owner() UserIdentity { return sessionUser(f.UserID) }

func (f *pendingFixture) listPending(t *testing.T, ident UserIdentity) []PendingSecretView {
	t.Helper()
	rec := ownerRequest(t, f.Server, ident, CredentialKindInteractive, http.MethodGet, "/api/v1/pending-secrets", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListPendingSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.PendingSecrets
}

func (f *pendingFixture) pendingRow(t *testing.T, id string) *store.PendingSecret {
	t.Helper()
	p, err := f.Store.GetPendingSecret(context.Background(), id)
	require.NoError(t, err)
	return p
}

func (f *pendingFixture) heldValue(t *testing.T, p *store.PendingSecret) string {
	t.Helper()
	v, err := f.Server.secretBackend.ReadPending(context.Background(), p.ValueRef)
	require.NoError(t, err)
	return string(v)
}

// TestAgentPendingSecret_UserScopeWriteIsPending: a user-scope agent write
// returns 202 with the pending state, records a pending row with the
// writer's provenance and the expiry, and writes nothing to the secrets
// table, with or without force.
func TestAgentPendingSecret_UserScopeWriteIsPending(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run("force="+boolStr(force), func(t *testing.T) {
			f := newPendingFixture(t, "pend-write-"+boolStr(force))
			before := time.Now()
			resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-1", force)))
			assert.Equal(t, "TOOL_AUTH", resp.Key)
			assert.Equal(t, store.ScopeUser, resp.Scope)
			assert.Equal(t, "pending", resp.State)
			assert.NotEmpty(t, resp.PendingID)
			assert.False(t, resp.ReplacesExisting)
			assert.Equal(t, "awaiting user approval", resp.Message)

			_, err := f.Server.secretBackend.GetMeta(context.Background(), "TOOL_AUTH", store.ScopeUser, f.UserID)
			assert.ErrorIs(t, err, store.ErrNotFound, "nothing is written to the secrets table")

			p := f.pendingRow(t, resp.PendingID)
			assert.Equal(t, f.UserID, p.ScopeID)
			assert.Equal(t, store.ScopeUser, p.Scope)
			assert.Equal(t, store.SecretTypeFile, p.SecretType)
			assert.Equal(t, "~/.config/tool/creds.json", p.Target)
			assert.True(t, p.AllowProgeny)
			assert.Equal(t, len("test-value-1"), p.ValueLen)
			assert.Equal(t, f.AgentID, p.AgentID)
			assert.Equal(t, f.getAgent(t).Slug, p.AgentSlug)
			assert.Equal(t, f.ProjectID, p.ProjectID)
			assert.Equal(t, f.ProjectName, p.ProjectName)
			assert.False(t, p.Created.Before(before.Truncate(time.Second)))
			assert.True(t, p.Created.Equal(p.Updated))
			assert.True(t, p.ExpiresAt.Equal(p.Created.Add(store.PendingSecretTTL)), "expires 7 days after the write")
			assert.True(t, resp.ExpiresAt.Equal(p.ExpiresAt))
			assert.Equal(t, "test-value-1", f.heldValue(t, p))
		})
	}
}

// TestAgentPendingSecret_ProvenanceFromTokenOnly: provenance fields in the
// request body are ignored; the row carries the token's agent, project and
// owner and the server's clock.
func TestAgentPendingSecret_ProvenanceFromTokenOnly(t *testing.T) {
	f := newPendingFixture(t, "pend-prov")
	body := map[string]interface{}{
		"value":       base64.StdEncoding.EncodeToString([]byte("test-value-1")),
		"scope":       "user",
		"scopeId":     "someone-else",
		"agentId":     "forged-agent",
		"agentSlug":   "forged-slug",
		"projectId":   "forged-project",
		"projectName": "forged-name",
		"created":     "2020-01-01T00:00:00Z",
		"updated":     "2020-01-01T00:00:00Z",
		"expiresAt":   "2099-01-01T00:00:00Z",
		"createdBy":   "forged-creator",
	}
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "PROV_KEY", body))
	p := f.pendingRow(t, resp.PendingID)
	assert.Equal(t, f.UserID, p.ScopeID)
	assert.Equal(t, f.AgentID, p.AgentID)
	assert.Equal(t, f.getAgent(t).Slug, p.AgentSlug)
	assert.Equal(t, f.ProjectID, p.ProjectID)
	assert.Equal(t, f.ProjectName, p.ProjectName)
	assert.WithinDuration(t, time.Now(), p.Created, time.Minute)
	assert.True(t, p.ExpiresAt.Equal(p.Created.Add(store.PendingSecretTTL)))
}

// TestAgentPendingSecret_OtherAgentRefused: agent B's write of a name agent
// A holds pending is refused with 409, force or not. A's row, value and ID
// stay as they were, and the response names neither A nor A's project.
func TestAgentPendingSecret_OtherAgentRefused(t *testing.T) {
	f := newPendingFixture(t, "pend-other")
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-a", false)))
	dropsBefore := len(f.drops.dropped)

	for _, force := range []bool{false, true} {
		rec := agentPut(t, f.Server, f.AgentB, f.TokenB, "TOOL_AUTH", userScopeWrite("test-value-b", force))
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeSecretPendingHeldByOtherAgent, errResp.Error.Code)
		body := rec.Body.String()
		agentA := f.getAgent(t)
		for _, leak := range []string{f.AgentID, agentA.Slug, agentA.Name, f.ProjectID, f.ProjectName, resp.PendingID} {
			assert.NotContains(t, body, leak, "the refusal does not reveal the other writer")
		}
	}

	p := f.pendingRow(t, resp.PendingID)
	assert.Equal(t, f.AgentID, p.AgentID)
	assert.Equal(t, "test-value-a", f.heldValue(t, p))
	rows, err := f.Store.ListPendingSecretsByOwner(context.Background(), store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	// B's held values were released; A's was not.
	for _, ref := range f.drops.dropped[dropsBefore:] {
		assert.NotEqual(t, p.ValueRef, ref)
	}
	assert.Len(t, f.drops.dropped[dropsBefore:], 2)
}

// TestAgentPendingSecret_SameAgentReplaces: the writer replaces its own
// pending write with or without force: new ID, created and expiry kept,
// updated set, the replaced held value released after the write.
func TestAgentPendingSecret_SameAgentReplaces(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run("force="+boolStr(force), func(t *testing.T) {
			f := newPendingFixture(t, "pend-same-"+boolStr(force))
			first := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-1", false)))
			old := f.pendingRow(t, first.PendingID)
			time.Sleep(10 * time.Millisecond)

			second := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-2", force)))
			assert.NotEqual(t, first.PendingID, second.PendingID)
			_, err := f.Store.GetPendingSecret(context.Background(), first.PendingID)
			assert.ErrorIs(t, err, store.ErrNotFound)

			p := f.pendingRow(t, second.PendingID)
			assert.True(t, p.Created.Equal(old.Created), "created is kept")
			assert.True(t, p.Updated.After(old.Updated), "updated is set")
			assert.True(t, p.ExpiresAt.Equal(old.ExpiresAt), "the expiry is not extended")
			assert.True(t, second.ExpiresAt.Equal(old.ExpiresAt))
			assert.Equal(t, "test-value-2", f.heldValue(t, p))
			assert.Contains(t, f.drops.dropped, old.ValueRef, "the replaced value is released")
		})
	}
}

// TestAgentPendingSecret_ReplacesApprovedSecret: a write to a name with an
// approved user secret returns 202 replacesExisting (no 409, force or not),
// records the approved row seen, and the approved value stays delivered.
func TestAgentPendingSecret_ReplacesApprovedSecret(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run("force="+boolStr(force), func(t *testing.T) {
			f := newPendingFixture(t, "pend-repl-"+boolStr(force))
			ctx := context.Background()
			_, meta, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
				Name: "TOOL_AUTH", Value: "test-value-approved", SecretType: store.SecretTypeEnvironment, Target: "TOOL_AUTH",
				Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
			})
			require.NoError(t, err)

			resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-new", force)))
			assert.True(t, resp.ReplacesExisting)
			p := f.pendingRow(t, resp.PendingID)
			assert.Equal(t, meta.ID, p.ReplacesSecretID)
			assert.Equal(t, meta.Version, p.ReplacesVersion)

			got, err := f.Server.secretBackend.Get(ctx, "TOOL_AUTH", store.ScopeUser, f.UserID)
			require.NoError(t, err)
			assert.Equal(t, "test-value-approved", got.Value)
			assert.Equal(t, meta.Version, got.Version)
			code, body := readUserSecretAs(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH")
			require.Equal(t, http.StatusOK, code, body)
			var read AgentGetSecretResponse
			require.NoError(t, json.Unmarshal([]byte(body), &read))
			assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("test-value-approved")), read.Value)
		})
	}
}

func readUserSecretAs(t *testing.T, srv *Server, agentID, token, key string) (int, string) {
	t.Helper()
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/"+key+"?scope=user", nil, token)
	return rec.Code, rec.Body.String()
}

func listAgentSecretKeys(t *testing.T, srv *Server, agentID, token, scope string) []string {
	t.Helper()
	path := "/api/v1/agents/" + agentID + "/secrets"
	if scope != "" {
		path += "?scope=" + scope
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp AgentListSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	keys := make([]string, 0, len(resp.Secrets))
	for _, s := range resp.Secrets {
		keys = append(keys, s.Key)
	}
	return keys
}

// TestAgentPendingSecret_PendingLooksAbsent: for every agent-visible read,
// a pending name answers exactly as a name that does not exist.
func TestAgentPendingSecret_PendingLooksAbsent(t *testing.T) {
	f := newPendingFixture(t, "pend-absent")
	ctx := context.Background()
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "PENDKEY", userScopeWrite("test-value-1", false)))

	for _, who := range []struct{ id, token string }{{f.AgentID, f.Token}, {f.AgentB, f.TokenB}} {
		pc, pb := readUserSecretAs(t, f.Server, who.id, who.token, "PENDKEY")
		ac, ab := readUserSecretAs(t, f.Server, who.id, who.token, "ABSENT1")
		assert.Equal(t, ac, pc)
		assert.Equal(t, http.StatusNotFound, pc)
		assert.Equal(t, strings.ReplaceAll(ab, "ABSENT1", "PENDKEY"), pb, "by-name read: identical responses")

		for _, scope := range []string{"", store.ScopeUser} {
			assert.NotContains(t, listAgentSecretKeys(t, f.Server, who.id, who.token, scope), "PENDKEY", "list scope %q", scope)
		}
	}

	disp := f.disp
	for _, id := range []string{f.AgentID, f.AgentB} {
		agent, err := f.Store.GetAgent(ctx, id)
		require.NoError(t, err)
		resolved, _, err := disp.resolveSecrets(ctx, agent)
		require.NoError(t, err)
		for _, r := range resolved {
			assert.NotEqual(t, "PENDKEY", r.Name, "start delivery")
		}
		got, err := disp.resolveAsNeededForKeys(ctx, agent, []string{"PENDKEY", "ABSENT1"}, nil)
		require.NoError(t, err)
		assert.Empty(t, got, "as-needed and env-gather resolution")
	}

	resolved, err := f.Server.secretBackend.Resolve(ctx, f.UserID, f.ProjectID, "", &secret.ResolveOpts{
		AgentAncestry: []string{f.UserID}, AuthzCheck: func(secret.SecretMeta) bool { return true },
	})
	require.NoError(t, err)
	for _, r := range resolved {
		assert.NotEqual(t, "PENDKEY", r.Name, "backend Resolve")
	}
}

// TestAgentPendingSecret_ExpiredRowIsGone: an expired pending row never
// yields the other-agent refusal; the write deletes it and starts fresh,
// and the expired row's held value is released. The owner's list, approve
// and reject treat an expired row as gone.
func TestAgentPendingSecret_ExpiredRowIsGone(t *testing.T) {
	f := newPendingFixture(t, "pend-expired")
	ctx := context.Background()
	ref, err := f.Server.secretBackend.HoldPending(ctx, "x", []byte("test-value-old"))
	require.NoError(t, err)
	stale, err := f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		Key: "TOOL_AUTH", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: f.UserID,
		ValueRef: ref, ValueLen: 14, AgentID: f.AgentID, AgentSlug: "a", ProjectID: f.ProjectID, ProjectName: "p",
	}, time.Now().Add(-store.PendingSecretTTL-time.Hour))
	require.NoError(t, err)

	// The owner sees nothing and cannot decide on it.
	assert.Empty(t, f.listPending(t, f.owner()))
	for _, action := range []string{"approve", "reject"} {
		rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
			"/api/v1/pending-secrets/"+stale.Secret.ID+"/"+action, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", action, rec.Body.String())
	}

	// Another agent's write is not refused.
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentB, f.TokenB, "TOOL_AUTH", userScopeWrite("test-value-new", false)))
	_, err = f.Store.GetPendingSecret(ctx, stale.Secret.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Contains(t, f.drops.dropped, ref, "the expired row's held value is released")
	p := f.pendingRow(t, resp.PendingID)
	assert.Equal(t, f.AgentB, p.AgentID)
	assert.WithinDuration(t, time.Now(), p.Created, time.Minute)
	assert.True(t, p.ExpiresAt.Equal(p.Created.Add(store.PendingSecretTTL)))
}

// TestPendingSecretsAPI_ListApproveReject: the owner lists metadata only,
// approves (writing the user-scope secret with the owner as creator and
// the agent as updater, progeny optionally turned off) and rejects.
func TestPendingSecretsAPI_ListApproveReject(t *testing.T) {
	f := newPendingFixture(t, "pend-api")
	ctx := context.Background()
	a := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_A", userScopeWrite("test-value-a", false)))
	b := decodePendingResponse(t, agentPut(t, f.Server, f.AgentB, f.TokenB, "KEY_B", userScopeWrite("test-value-b", false)))

	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet, "/api/v1/pending-secrets", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "test-value")
	assert.NotContains(t, rec.Body.String(), "enc:v")
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "valueref")
	list := f.listPending(t, f.owner())
	require.Len(t, list, 2)
	assert.Equal(t, a.PendingID, list[0].ID)
	assert.Equal(t, "KEY_A", list[0].Key)
	assert.Equal(t, f.AgentID, list[0].AgentID)
	assert.Equal(t, f.ProjectID, list[0].ProjectID)
	assert.Equal(t, f.ProjectName, list[0].ProjectName)
	assert.Equal(t, "pending", list[0].State)
	assert.Equal(t, len("test-value-a"), list[0].ValueLen)
	assert.Equal(t, f.AgentB, list[1].AgentID)

	// Progeny can be turned off, never on.
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+a.PendingID+"/approve", ApprovePendingSecretRequest{AllowProgeny: boolPtr(false)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var decision PendingSecretDecisionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decision))
	assert.Equal(t, "approved", decision.State)

	got, err := f.Server.secretBackend.Get(ctx, "KEY_A", store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, "test-value-a", got.Value)
	assert.Equal(t, store.ScopeUser, got.Scope)
	assert.Equal(t, store.SecretTypeFile, got.SecretType)
	assert.Equal(t, "~/.config/tool/creds.json", got.Target)
	assert.False(t, got.AllowProgeny)
	assert.Equal(t, f.UserID, got.CreatedBy)
	assert.Equal(t, "agent:"+f.AgentID, got.UpdatedBy)
	_, err = f.Store.GetPendingSecret(ctx, a.PendingID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A second decision on the same ID finds nothing.
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+a.PendingID+"/approve", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Reject leaves the secrets table alone.
	bRow := f.pendingRow(t, b.PendingID)
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+b.PendingID+"/reject", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = f.Server.secretBackend.GetMeta(ctx, "KEY_B", store.ScopeUser, f.UserID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.Store.GetPendingSecret(ctx, b.PendingID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Contains(t, f.drops.dropped, bRow.ValueRef)
	assert.Empty(t, f.listPending(t, f.owner()))
}

// TestPendingSecretsAPI_ApproveCannotTurnProgenyOn: approval writes the
// row the agent asked for; progeny can only be turned off.
func TestPendingSecretsAPI_ApproveCannotTurnProgenyOn(t *testing.T) {
	f := newPendingFixture(t, "pend-progeny")
	body := userScopeWrite("test-value-1", false)
	body.AllowProgeny = boolPtr(false)
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_P", body))
	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+resp.PendingID+"/approve", ApprovePendingSecretRequest{AllowProgeny: boolPtr(true)})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	f.pendingRow(t, resp.PendingID)
}

// TestPendingSecretsAPI_Refusals: list, approve and reject serve the
// owning user's interactive session only. Agents, user access tokens and
// federated callers are refused; another user, hub admins included, sees
// nothing and cannot decide.
func TestPendingSecretsAPI_Refusals(t *testing.T) {
	f := newPendingFixture(t, "pend-refuse")
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_R", userScopeWrite("test-value-1", false)))
	paths := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/pending-secrets"},
		{http.MethodPost, "/api/v1/pending-secrets/" + resp.PendingID + "/approve"},
		{http.MethodPost, "/api/v1/pending-secrets/" + resp.PendingID + "/reject"},
	}
	refusedCodes := []int{http.StatusUnauthorized, http.StatusForbidden}

	for _, p := range paths {
		rec := doRequestWithAgentToken(t, f.Server, p.method, p.path, nil, f.Token)
		assert.Contains(t, refusedCodes, rec.Code, "agent %s %s: %s", p.method, p.path, rec.Body.String())

		rec = ownerRequest(t, f.Server, f.owner(), CredentialKindUAT, p.method, p.path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "user access token %s %s: %s", p.method, p.path, rec.Body.String())

		fed := NewFederatedUserIdentity("https://issuer.example", f.UserID, f.UserID+"@test.com", "Fed", "member", nil)
		rec = ownerRequest(t, f.Server, fed, CredentialKindFederation, p.method, p.path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "federated %s %s: %s", p.method, p.path, rec.Body.String())
	}

	others := map[string]UserIdentity{
		"other user": sessionUser(tid("pend-refuse-other")),
		"hub admin":  NewAuthenticatedUser(tid("pend-refuse-admin"), "admin@test.com", "Admin", "admin", "web"),
	}
	for name, ident := range others {
		assert.Empty(t, f.listPending(t, ident), "%s sees no pending secret of the owner", name)
		for _, action := range []string{"approve", "reject"} {
			rec := ownerRequest(t, f.Server, ident, CredentialKindInteractive, http.MethodPost,
				"/api/v1/pending-secrets/"+resp.PendingID+"/"+action, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", name, action, rec.Body.String())
		}
	}
	// The dev credential is a session-class credential of the dev user,
	// which is a hub admin; it too sees only its own pending secrets.
	rec := doRequest(t, f.Server, http.MethodPost, "/api/v1/pending-secrets/"+resp.PendingID+"/approve", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The row is untouched.
	f.pendingRow(t, resp.PendingID)
	_, err := f.Server.secretBackend.GetMeta(context.Background(), "KEY_R", store.ScopeUser, f.UserID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestAgentPendingSecret_ProjectScopeStoredDirectly: project-scope agent
// writes go to the secrets table and record no pending row.
func TestAgentPendingSecret_ProjectScopeStoredDirectly(t *testing.T) {
	f := newPendingFixture(t, "pend-project")
	rec := agentPut(t, f.Server, f.AgentID, f.Token, "PROJ_KEY", AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("test-value-1")), Scope: store.ScopeProject,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = agentPut(t, f.Server, f.AgentID, f.Token, "PROJ_KEY", AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("test-value-2")), Scope: store.ScopeProject,
	})
	require.Equal(t, http.StatusConflict, rec.Code, "an existing project secret needs force: %s", rec.Body.String())
	rec = agentPut(t, f.Server, f.AgentID, f.Token, "PROJ_KEY", AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("test-value-2")), Scope: store.ScopeProject, Force: true,
	})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got, err := f.Server.secretBackend.Get(context.Background(), "PROJ_KEY", store.ScopeProject, f.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, "test-value-2", got.Value)
	assert.Empty(t, f.listPending(t, f.owner()))
}

// TestPendingSecret_EndToEnd: an agent captures a user credential; it is
// pending and not delivered when an agent of the user starts; the owner
// approves; the next start delivers it. A second agent's write of the
// same name while it is pending is refused.
func TestPendingSecret_EndToEnd(t *testing.T) {
	f := newSDFixture(t, "pend-e2e")
	ctx := context.Background()
	writer, writerToken := f.direct(t, "e2e-writer", true)
	starter, starterToken := f.direct(t, "e2e-starter", true)
	other, otherToken := f.direct(t, "e2e-other", true)

	const key = "E2E_TOOL_TOKEN"
	// delivered reports what a start of the starter agent delivers of key:
	// injected at start (resolveSecrets) or, for the default as-needed
	// mode a capture uses, resolved on request (resolveAsNeededForKeys,
	// as-needed fetches and env-gather) and read at runtime.
	delivered := func() (string, bool) {
		agent, err := f.Store.GetAgent(ctx, starter)
		require.NoError(t, err)
		resolved, _, err := f.disp.resolveSecrets(ctx, agent)
		require.NoError(t, err)
		for _, r := range resolved {
			if r.Name == key {
				return r.Value, true
			}
		}
		got, err := f.disp.resolveAsNeededForKeys(ctx, agent, []string{key}, nil)
		require.NoError(t, err)
		if v, ok := got[key]; ok {
			code, body := readUserSecretAs(t, f.Server, starter, starterToken, key)
			require.Equal(t, http.StatusOK, code, "the runtime read agrees with as-needed delivery: %s", body)
			return v, true
		}
		code, body := readUserSecretAs(t, f.Server, starter, starterToken, key)
		require.Equal(t, http.StatusNotFound, code, "the runtime read agrees: %s", body)
		return "", false
	}

	// 1. Capture: the harness writes the credential at user scope.
	allow := true
	rec := doRequestWithAgentToken(t, f.Server, http.MethodPut, "/api/v1/agents/"+writer+"/secrets/"+key, AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("test-value-e2e")),
		Scope: store.ScopeUser, AllowProgeny: &allow,
	}, writerToken)
	resp := decodePendingResponse(t, rec)
	assert.Equal(t, "pending", resp.State)

	// 2. Not delivered at start.
	_, ok := delivered()
	assert.False(t, ok, "a pending secret is not delivered at start")

	// 3. Another agent's write of the same name is refused.
	rec = doRequestWithAgentToken(t, f.Server, http.MethodPut, "/api/v1/agents/"+other+"/secrets/"+key, AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("test-value-other")),
		Scope: store.ScopeUser, AllowProgeny: &allow, Force: true,
	}, otherToken)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// 4. The owner approves.
	rec = ownerRequest(t, f.Server, sessionUser(f.UserID), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+resp.PendingID+"/approve", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 5. Delivered at the next start, with the captured value.
	value, ok := delivered()
	require.True(t, ok, "the approved secret is delivered at the next start")
	assert.Equal(t, "test-value-e2e", value)
}

// TestPendingSecretsAPI_SecretNamedPendingReachable: the pending secret
// endpoints have their own prefix, so a user secret named "pending" is
// reachable on every verb of the secrets route.
func TestPendingSecretsAPI_SecretNamedPendingReachable(t *testing.T) {
	f := newPendingFixture(t, "pend-name")
	owner := f.owner()
	path := "/api/v1/secrets/pending"

	rec := ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodPut, path, SetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("test-value-1")), Scope: store.ScopeUser,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got store.Secret
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "pending", got.Key)
	assert.Equal(t, store.ScopeUser, got.Scope)

	rec = ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodPatch, path,
		map[string]interface{}{"description": "named pending"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The pending list is a separate endpoint and lists nothing.
	assert.Empty(t, f.listPending(t, owner))

	rec = ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodDelete, path, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
	_, err := f.Server.secretBackend.GetMeta(context.Background(), "pending", store.ScopeUser, f.UserID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Unknown pending-secret paths and methods are refused, not routed to
	// the secrets handler.
	rec = ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodPost, "/api/v1/pending-secrets", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	rec = ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodPost, "/api/v1/pending-secrets/x/other", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = ownerRequest(t, f.Server, owner, CredentialKindInteractive, http.MethodGet, "/api/v1/pending-secrets/x/approve", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// approvedSecretSet writes the owner's approved user secret key directly.
func (f *pendingFixture) approvedSecretSet(t *testing.T, key, value string) *secret.SecretMeta {
	t.Helper()
	_, meta, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
		Name: key, Value: value, SecretType: store.SecretTypeEnvironment, Target: key,
		Scope: store.ScopeUser, ScopeID: f.UserID, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)
	return meta
}

func (f *pendingFixture) decide(t *testing.T, id, action string) *httptest.ResponseRecorder {
	t.Helper()
	return ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+id+"/"+action, nil)
}

// TestPendingSecretsAPI_ApproveChecksExistingSecret: approval swaps the
// value in only while the approved secret of the same name is the one
// recorded when the agent wrote the pending secret. Otherwise it answers
// 409 secret_pending_existing_changed, writes nothing, keeps the pending
// secret and its held value.
func TestPendingSecretsAPI_ApproveChecksExistingSecret(t *testing.T) {
	cases := []struct {
		name string
		// before runs before the agent write, after runs after it.
		before, after func(t *testing.T, f *pendingFixture)
		// wantValue is the approved value after the decision ("" when
		// there is no approved secret).
		wantValue string
		wantCode  int
	}{
		{
			name:      "no existing secret then or now",
			wantValue: "test-value-new", wantCode: http.StatusOK,
		},
		{
			name:      "existing secret unchanged",
			before:    func(t *testing.T, f *pendingFixture) { f.approvedSecretSet(t, "TOOL_AUTH", "test-value-old") },
			wantValue: "test-value-new", wantCode: http.StatusOK,
		},
		{
			name:      "secret created after the write",
			after:     func(t *testing.T, f *pendingFixture) { f.approvedSecretSet(t, "TOOL_AUTH", "test-value-owner") },
			wantValue: "test-value-owner", wantCode: http.StatusConflict,
		},
		{
			name:      "existing secret changed after the write",
			before:    func(t *testing.T, f *pendingFixture) { f.approvedSecretSet(t, "TOOL_AUTH", "test-value-old") },
			after:     func(t *testing.T, f *pendingFixture) { f.approvedSecretSet(t, "TOOL_AUTH", "test-value-owner") },
			wantValue: "test-value-owner", wantCode: http.StatusConflict,
		},
		{
			name:   "existing secret deleted after the write",
			before: func(t *testing.T, f *pendingFixture) { f.approvedSecretSet(t, "TOOL_AUTH", "test-value-old") },
			after: func(t *testing.T, f *pendingFixture) {
				require.NoError(t, f.Server.secretBackend.Delete(context.Background(), "TOOL_AUTH", store.ScopeUser, f.UserID))
			},
			wantValue: "", wantCode: http.StatusConflict,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t, "pend-cas-"+string(rune('a'+i)))
			ctx := context.Background()
			if tc.before != nil {
				tc.before(t, f)
			}
			resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-new", false)))
			row := f.pendingRow(t, resp.PendingID)
			if tc.after != nil {
				tc.after(t, f)
			}

			rec := f.decide(t, resp.PendingID, "approve")
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			got, err := f.Server.secretBackend.Get(ctx, "TOOL_AUTH", store.ScopeUser, f.UserID)
			if tc.wantValue == "" {
				assert.ErrorIs(t, err, store.ErrNotFound, "nothing is written")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantValue, got.Value)
			}
			if tc.wantCode == http.StatusOK {
				_, err := f.Store.GetPendingSecret(ctx, resp.PendingID)
				assert.ErrorIs(t, err, store.ErrNotFound)
				assert.Contains(t, f.drops.dropped, row.ValueRef)
				return
			}
			var body struct {
				Error struct {
					Code    string                 `json:"code"`
					Message string                 `json:"message"`
					Details map[string]interface{} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, ErrCodeSecretPendingExistingChanged, body.Error.Code)
			assert.Contains(t, body.Error.Message, "Reject it")
			assert.NotContains(t, rec.Body.String(), "test-value")
			kept := f.pendingRow(t, resp.PendingID)
			assert.Equal(t, row.ValueRef, kept.ValueRef, "the pending secret is kept with its held value")
			assert.Equal(t, "test-value-new", f.heldValue(t, kept))
			assert.NotContains(t, f.drops.dropped, row.ValueRef)

			// The owner can still reject it.
			require.Equal(t, http.StatusOK, f.decide(t, resp.PendingID, "reject").Code)
			assert.Contains(t, f.drops.dropped, row.ValueRef)
		})
	}
}

// TestPendingSecretsAPI_ConcurrentApproveAndReject: of an approval and a
// rejection of the same pending secret sent together, exactly one
// succeeds and the other answers 404; the outcome matches the answer.
func TestPendingSecretsAPI_ConcurrentApproveAndReject(t *testing.T) {
	f := newPendingFixture(t, "pend-race")
	ctx := context.Background()
	for round := 0; round < 10; round++ {
		key := "RACE_KEY_" + string(rune('A'+round))
		resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, key, userScopeWrite("test-value-race", false)))

		codes := map[string]int{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, action := range []string{"approve", "reject"} {
			wg.Add(1)
			go func(action string) {
				defer wg.Done()
				rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
					"/api/v1/pending-secrets/"+resp.PendingID+"/"+action, nil)
				mu.Lock()
				codes[action] = rec.Code
				mu.Unlock()
			}(action)
		}
		wg.Wait()

		_, err := f.Server.secretBackend.GetMeta(ctx, key, store.ScopeUser, f.UserID)
		switch {
		case codes["approve"] == http.StatusOK:
			assert.Equal(t, http.StatusNotFound, codes["reject"], "round %d", round)
			assert.NoError(t, err, "round %d: approved", round)
		case codes["reject"] == http.StatusOK:
			assert.Equal(t, http.StatusNotFound, codes["approve"], "round %d", round)
			assert.ErrorIs(t, err, store.ErrNotFound, "round %d: rejected", round)
		default:
			t.Fatalf("round %d: neither decision succeeded: %v", round, codes)
		}
		_, err = f.Store.GetPendingSecret(ctx, resp.PendingID)
		assert.ErrorIs(t, err, store.ErrNotFound, "round %d", round)
	}
}

// setFailingBackend fails every Set after running onSet.
type setFailingBackend struct {
	secret.SecretBackend
	onSet func()
}

func (b *setFailingBackend) Set(context.Context, *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	if b.onSet != nil {
		b.onSet()
	}
	return false, nil, errors.New("secret backend unavailable")
}

// TestPendingSecretsAPI_ApproveWriteFailurePutsBack: when the approved
// secret cannot be written, the claimed pending secret is put back with
// its ID and held value, so the owner can try again. When the key was
// taken meanwhile (the same agent wrote a replacement), the claimed value
// is released and the newer pending secret stays.
func TestPendingSecretsAPI_ApproveWriteFailurePutsBack(t *testing.T) {
	t.Run("put back", func(t *testing.T) {
		f := newPendingFixture(t, "pend-putback")
		resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-1", false)))
		row := f.pendingRow(t, resp.PendingID)
		f.Server.SetSecretBackend(&setFailingBackend{SecretBackend: f.drops})

		rec := f.decide(t, resp.PendingID, "approve")
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		kept := f.pendingRow(t, resp.PendingID)
		assert.Equal(t, *row, *kept, "the pending secret is back as it was")
		assert.NotContains(t, f.drops.dropped, row.ValueRef)

		f.Server.SetSecretBackend(f.drops)
		require.Equal(t, http.StatusOK, f.decide(t, resp.PendingID, "approve").Code)
		got, err := f.Server.secretBackend.Get(context.Background(), "TOOL_AUTH", store.ScopeUser, f.UserID)
		require.NoError(t, err)
		assert.Equal(t, "test-value-1", got.Value)
	})
	t.Run("key taken meanwhile", func(t *testing.T) {
		f := newPendingFixture(t, "pend-putback-clash")
		resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-1", false)))
		row := f.pendingRow(t, resp.PendingID)
		var newer AgentPendingSecretResponse
		f.Server.SetSecretBackend(&setFailingBackend{SecretBackend: f.drops, onSet: func() {
			f.Server.SetSecretBackend(f.drops)
			newer = decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-2", false)))
			f.Server.SetSecretBackend(&setFailingBackend{SecretBackend: f.drops})
		}})

		rec := f.decide(t, resp.PendingID, "approve")
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		_, err := f.Store.GetPendingSecret(context.Background(), resp.PendingID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Contains(t, f.drops.dropped, row.ValueRef, "the claimed value is released")
		f.Server.SetSecretBackend(f.drops)
		assert.Equal(t, "test-value-2", f.heldValue(t, f.pendingRow(t, newer.PendingID)))
	})
}

// ownerWebRequest sends a request through the hub's full handler chain
// with the owner's web session token, the extra headers and a raw body.
func (f *pendingFixture) ownerWebRequest(t *testing.T, srv *Server, method, path, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	tok, _, err := srv.userTokenService.GenerateAccessToken(f.UserID, f.UserID+"@test.com", "Owner", "member", ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestPendingSecretsAPI_DecisionRefusesCrossSiteRequests: in every auth
// mode, approve and reject refuse a request a browser sent from another
// site (403) and a browser request whose body is not JSON (415), change
// nothing, and send no CORS headers. A same-origin JSON request and a
// request without browser headers (the CLI) are served.
func TestPendingSecretsAPI_DecisionRefusesCrossSiteRequests(t *testing.T) {
	// httptest requests carry Host example.com.
	crossSite := map[string]map[string]string{
		"foreign origin":                {"Origin": "https://attacker.test"},
		"null origin":                   {"Origin": "null"},
		"sec-fetch-site cross-site":     {"Sec-Fetch-Site": "cross-site"},
		"sibling host origin":           {"Origin": "https://other.example.com"},
		"same-site from a sibling host": {"Sec-Fetch-Site": "same-site", "Origin": "https://other.example.com"},
		"unknown sec-fetch-site":        {"Sec-Fetch-Site": "something-else"},
	}
	f := newPendingFixture(t, "pend-xsite")
	ctx := context.Background()
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-1", false)))
	row := f.pendingRow(t, resp.PendingID)
	unchanged := func(t *testing.T, what string) {
		t.Helper()
		kept, err := f.Store.GetPendingSecret(ctx, resp.PendingID)
		require.NoError(t, err, what)
		assert.Equal(t, row.ValueRef, kept.ValueRef, what)
		_, err = f.Server.secretBackend.GetMeta(ctx, "TOOL_AUTH", store.ScopeUser, f.UserID)
		assert.ErrorIs(t, err, store.ErrNotFound, what)
		assert.NotContains(t, f.drops.dropped, row.ValueRef, what)
	}
	noCORS := func(t *testing.T, rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		for name := range rec.Header() {
			assert.False(t, strings.HasPrefix(name, "Access-Control-Allow-"), "%s: %s header sent", what, name)
		}
	}

	for _, action := range []string{"approve", "reject"} {
		path := "/api/v1/pending-secrets/" + resp.PendingID + "/" + action
		for name, h := range crossSite {
			what := action + " " + name
			rec := f.ownerWebRequest(t, f.Server, http.MethodPost, path, "application/json", "{}", h)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", what, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "test-value")
			noCORS(t, rec, what)
			unchanged(t, what)
		}
		for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
			what := action + " same-origin body " + ct
			rec := f.ownerWebRequest(t, f.Server, http.MethodPost, path, ct, "{}", map[string]string{"Origin": "https://example.com"})
			assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code, "%s: %s", what, rec.Body.String())
			noCORS(t, rec, what)
			unchanged(t, what)
		}
	}

	// A same-origin JSON approval is served, without CORS headers.
	rec := f.ownerWebRequest(t, f.Server, http.MethodPost, "/api/v1/pending-secrets/"+resp.PendingID+"/approve",
		"application/json", "{}", map[string]string{"Origin": "https://example.com", "Sec-Fetch-Site": "same-origin"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	noCORS(t, rec, "same-origin approve")

	// A request without browser headers (the CLI) is served too.
	resp = decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH_2", userScopeWrite("test-value-2", false)))
	rec = f.ownerWebRequest(t, f.Server, http.MethodPost, "/api/v1/pending-secrets/"+resp.PendingID+"/reject", "", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestPendingSecretsAPI_CrossSiteRefusedInProxyMode: behind an
// authenticating proxy the full middleware chain refuses a cross-site
// decision as well.
func TestPendingSecretsAPI_CrossSiteRefusedInProxyMode(t *testing.T) {
	f := newPendingFixture(t, "pend-xsite-proxy")
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "TOOL_AUTH", userScopeWrite("test-value-1", false)))
	f.Server.authConfig.AuthMode = proxyAuthMode
	t.Cleanup(func() { f.Server.authConfig.AuthMode = "" })
	for _, action := range []string{"approve", "reject"} {
		rec := f.ownerWebRequest(t, f.Server, http.MethodPost, "/api/v1/pending-secrets/"+resp.PendingID+"/"+action,
			"application/json", "{}", map[string]string{"Origin": "https://attacker.test"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", action, rec.Body.String())
	}
	f.pendingRow(t, resp.PendingID)
	_, err := f.Server.secretBackend.GetMeta(context.Background(), "TOOL_AUTH", store.ScopeUser, f.UserID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestPendingSecret_ProjectDeletionCascade: deleting a project deletes
// the pending secrets its agents wrote and releases their held values;
// pending secrets from other projects stay. Deleting the writing agent
// keeps its pending secret, still approvable.
func TestPendingSecret_ProjectDeletionCascade(t *testing.T) {
	f := newPendingFixture(t, "pend-proj-del")
	ctx := context.Background()
	a := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_A", userScopeWrite("test-value-a", false)))
	b := decodePendingResponse(t, agentPut(t, f.Server, f.AgentB, f.TokenB, "KEY_B", userScopeWrite("test-value-b", false)))
	aRow := f.pendingRow(t, a.PendingID)
	bRow := f.pendingRow(t, b.PendingID)

	// Deleting the writer agent B keeps its pending secret.
	require.NoError(t, f.Store.DeleteAgent(ctx, f.AgentB))
	assert.Equal(t, bRow.ValueRef, f.pendingRow(t, b.PendingID).ValueRef)

	rec := doRequest(t, f.Server, http.MethodDelete, "/api/v1/projects/"+f.ProjectID+"?force=true", nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
	_, err := f.Store.GetPendingSecret(ctx, a.PendingID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the deleted project's pending secret is deleted")
	assert.Contains(t, f.drops.dropped, aRow.ValueRef, "its held value is released")
	assert.NotContains(t, f.drops.dropped, bRow.ValueRef)

	// B's pending secret, from the other project and a deleted writer,
	// is still approvable.
	require.Equal(t, http.StatusOK, f.decide(t, b.PendingID, "approve").Code)
	got, err := f.Server.secretBackend.Get(ctx, "KEY_B", store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, "test-value-b", got.Value)
}

// seedPendingSecret holds value with b and records a pending secret of
// ownerID for it.
func seedPendingSecret(t *testing.T, s store.Store, b secret.SecretBackend, ownerID, key, value string) *store.PendingSecret {
	t.Helper()
	ctx := context.Background()
	id := api.NewUUID()
	ref, err := b.HoldPending(ctx, id, []byte(value))
	require.NoError(t, err)
	res, err := s.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		ID: id, Key: key, SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: ownerID,
		ValueRef: ref, ValueLen: len(value), AgentID: "agent-1", AgentSlug: "agent-1", ProjectID: "project-1", ProjectName: "project-1",
	}, time.Now())
	require.NoError(t, err)
	return res.Secret
}

// TestPendingSecret_UserDeletionCascade: removing a deleted user's data
// deletes their pending secrets and releases the held values; the
// startup sweep finds a missing user that has only pending secrets; other
// users' pending secrets stay. With no secret backend the rows are kept
// for a later sweep.
func TestPendingSecret_UserDeletionCascade(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	drops := &pendingDropRecorder{SecretBackend: secret.NewLocalBackend(s, "test-hub-id", "test-secret")}
	srv.SetSecretBackend(drops)

	gone := tid("pend-user-gone")
	swept := tid("pend-user-swept")
	live := newActiveMember(t, s, "pend-user-live", "pend-live@test.com")
	goneRow := seedPendingSecret(t, s, drops, gone, "KEY_1", "test-value-1")
	sweptRow := seedPendingSecret(t, s, drops, swept, "KEY_2", "test-value-2")
	liveRow := seedPendingSecret(t, s, drops, live.ID, "KEY_3", "test-value-3")

	assert.True(t, srv.removeUserScopedData(ctx, gone))
	_, err := s.GetPendingSecret(ctx, goneRow.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Contains(t, drops.dropped, goneRow.ValueRef)

	removed, _, err := srv.sweepOrphanedUserScopedData(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the user with only a pending secret is found")
	_, err = s.GetPendingSecret(ctx, sweptRow.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Contains(t, drops.dropped, sweptRow.ValueRef)

	_, err = s.GetPendingSecret(ctx, liveRow.ID)
	require.NoError(t, err, "a live user's pending secret stays")
	assert.NotContains(t, drops.dropped, liveRow.ValueRef)

	// No secret backend: the row is kept and the removal reports it.
	kept := seedPendingSecret(t, s, drops, gone, "KEY_4", "test-value-4")
	srv.SetSecretBackend(nil)
	assert.False(t, srv.removeUserScopedData(ctx, gone))
	_, err = s.GetPendingSecret(ctx, kept.ID)
	require.NoError(t, err)
}
