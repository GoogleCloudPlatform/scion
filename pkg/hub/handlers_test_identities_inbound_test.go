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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Broker inbound message paths refuse test identities (F1) ---------------

func tiInboundLegacy(t *testing.T, srv *Server, projectID, agentSlug, sender string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(inboundMessageRequest{
		Topic: "scion.project." + projectID + ".agent." + agentSlug + ".messages",
		Message: &messages.StructuredMessage{Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel: "discord", Sender: sender, Recipient: "agent:" + agentSlug, Msg: "hello", Type: messages.TypeInstruction},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func tiInboundRouted(t *testing.T, srv *Server, projectID, agentSlug, sender string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(routedInboundRequest{
		ProjectID: projectID, DefaultAgent: agentSlug,
		Message: &messages.StructuredMessage{Version: messages.Version, Channel: "slack", Sender: sender, Msg: "hello", Type: messages.TypeInstruction},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound/routed", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// Both broker inbound paths refuse a live test identity, an expired one
// and a reserved-domain email with no row, with the flag on and off. The
// test identities own the project, so only the refusal can stop them.
func TestTestIdentity_BrokerInboundRefused(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	live := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	expired := tiStoreFixture(t, s, generateID(), time.Now().Add(-time.Minute))

	project := &store.Project{ID: tid("ti-inbound-project"), Name: "ti-inbound", Slug: "ti-inbound", OwnerID: live.ID, CreatedBy: live.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	for _, u := range []*store.User{live, expired} {
		msgAuthzAddProjectMember(t, s, u.ID, project.ID, project.Slug, store.GroupMemberRoleMember)
		msgAuthzGrantAgentMessage(t, s, u.ID, project.ID)
	}
	agent := &store.Agent{ID: tid("ti-inbound-agent"), Slug: "ti-inbound-agent", Name: "ti inbound", ProjectID: project.ID,
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject, StateVersion: 1, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateAgent(ctx, agent))

	for _, enabled := range []bool{true, false} {
		srv.testIdentities.enabled = enabled
		for _, email := range []string{live.Email, expired.Email, "nobody@" + store.TestFixtureEmailDomain} {
			for name, send := range map[string]func(*testing.T, *Server, string, string, string) *httptest.ResponseRecorder{
				"legacy": tiInboundLegacy, "routed": tiInboundRouted,
			} {
				rec := send(t, srv, project.ID, agent.Slug, "user:"+email)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s enabled=%v %s: %s", name, enabled, email, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "sender identity not eligible", "%s enabled=%v %s", name, enabled, email)
			}
		}
	}
	srv.testIdentities.enabled = true
	assert.True(t, emailResolvedPrincipalRefused("x@"+store.TestFixtureEmailDomain, nil))
	assert.True(t, emailResolvedPrincipalRefused("odd@example.com", &store.User{Kind: store.UserKindTestFixture}))
	assert.False(t, emailResolvedPrincipalRefused("ok@scion-test.invalid", &store.User{Email: "ok@scion-test.invalid"}))
	assert.False(t, emailResolvedPrincipalRefused("ok@example.com", nil))
}
