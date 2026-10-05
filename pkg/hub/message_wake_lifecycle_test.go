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
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule pinned here: resuming a suspended agent to deliver a message requires
// the lifecycle permission that starting the agent requires.

// wakeLifecycleUser creates a hub member bound in projectID to a custom role
// holding exactly permissions.
func wakeLifecycleUser(t *testing.T, s store.Store, id, projectID string, permissions ...string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := hubMemberUser(t, s, id)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-role-" + id,
		Description: "Test role for message wake lifecycle tests",
		ScopeType:   store.RoleScopeProject,
		Permissions: permissions,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

// storedMessagesFor returns the number of persisted messages addressed to
// the agent recipientID.
func storedMessagesFor(t *testing.T, s store.Store, recipientID string) int {
	t.Helper()
	res, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: recipientID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return len(res.Items)
}

// sendWakeMessage posts a wake message to target through the route mux as u.
func sendWakeMessage(t *testing.T, srv *Server, u *store.User, target *store.Agent, msg string) int {
	t.Helper()
	rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
		map[string]interface{}{"message": msg, "wake": true})
	t.Logf("response %d: %s", rec.Code, rec.Body.String())
	return rec.Code
}

// TestMessageWake_MessageOnlyUserCannotResumeSuspendedAgent: a member with
// message permission but not lifecycle permission, sending with wake to a
// suspended agent, is refused with 403; nothing is resumed, dispatched or
// stored.
func TestMessageWake_MessageOnlyUserCannotResumeSuspendedAgent(t *testing.T) {
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	u := wakeLifecycleUser(t, s, "wake-msg-only", target.ProjectID, "agent.message", "agent.read")

	rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
		map[string]interface{}{"message": "please wake", "wake": true})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeForbidden, apiErr.Code)
	assert.Equal(t, "not permitted to resume agent "+target.Slug, apiErr.Message)

	assert.Empty(t, disp.getStartCalls(), "no resume may be dispatched")
	assert.Empty(t, disp.getMessageCalls(), "no message may be delivered")
	assert.Zero(t, storedMessagesFor(t, s, target.ID), "no message may be stored")

	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// TestMessageWake_LifecycleUserResumesAndDelivers: a member holding the
// lifecycle permission, sending with wake to a suspended agent, resumes it
// and the message is delivered.
func TestMessageWake_LifecycleUserResumesAndDelivers(t *testing.T) {
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	u := wakeLifecycleUser(t, s, "wake-lifecycle", target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()

	require.Equal(t, http.StatusOK, sendWakeMessage(t, srv, u, target, "wake and read"))

	starts := disp.getStartCalls()
	require.Len(t, starts, 1, "exactly one resume")
	assert.True(t, starts[0].Continue)
	msgs := disp.getMessageCalls()
	require.Len(t, msgs, 1, "the message is delivered")
	assert.Equal(t, "wake and read", msgs[0].Message)
	assert.Equal(t, 1, storedMessagesFor(t, s, target.ID))
}

// TestMessageWake_MessageOnlyUserDeliversToRunningAgent: with wake set and
// a running target no resume is needed, so message permission suffices.
func TestMessageWake_MessageOnlyUserDeliversToRunningAgent(t *testing.T) {
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	u := wakeLifecycleUser(t, s, "wake-msg-running", target.ProjectID, "agent.message", "agent.read")

	require.Equal(t, http.StatusOK, sendWakeMessage(t, srv, u, target, "already awake"))

	assert.Empty(t, disp.getStartCalls(), "a running agent needs no resume")
	msgs := disp.getMessageCalls()
	require.Len(t, msgs, 1, "the message is delivered")
	assert.Equal(t, "already awake", msgs[0].Message)
}

// wakeLifecycleAgentIdentity is an agent caller whose token carries exactly
// scopes.
type wakeLifecycleAgentIdentity struct {
	wakeDMTestIdentity
	scopes []AgentTokenScope
}

// wakeDMSenderIdentity returns an agent identity for sender carrying exactly
// scopes.
func wakeDMSenderIdentity(sender *store.Agent, scopes ...AgentTokenScope) *wakeLifecycleAgentIdentity {
	return &wakeLifecycleAgentIdentity{
		wakeDMTestIdentity: wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		scopes:             scopes,
	}
}

// authzClassification opts this fake into agent JWT classification, so
// authorization decisions treat it as an agent caller with its scopes.
func (i *wakeLifecycleAgentIdentity) authzClassification() (PrincipalKind, CredentialKind) {
	return PrincipalKindAgent, CredentialKindAgentJWT
}

func (i *wakeLifecycleAgentIdentity) Scopes() []AgentTokenScope { return i.scopes }
func (i *wakeLifecycleAgentIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range i.scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// TestMessageWake_AgentWithoutLifecycleCannotResumeSuspendedAgent: an agent
// caller allowed to message the target but without the lifecycle scope is
// refused the resume of a suspended target; nothing is resumed, dispatched
// or stored.
func TestMessageWake_AgentWithoutLifecycleCannotResumeSuspendedAgent(t *testing.T) {
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	ident := wakeDMSenderIdentity(sender, ScopeProjectRead)

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: ident,
		TargetAgent:    target,
		Msg:            "wake up, sibling",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeForbidden, dmErr.Code)
	assert.Equal(t, "not permitted to resume agent "+target.Slug, dmErr.Message)

	assert.Empty(t, disp.getStartCalls(), "no resume may be dispatched")
	assert.Empty(t, disp.getMessageCalls(), "no message may be delivered")
	assert.Zero(t, storedMessagesFor(t, s, target.ID), "no message may be stored")

	// Without wake, the same caller gets the suspended-target conflict.
	_, dmErr = srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: ident,
		TargetAgent:    target,
		Msg:            "no wake",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
	})
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
}
