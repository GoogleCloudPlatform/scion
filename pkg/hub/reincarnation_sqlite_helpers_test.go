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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setReincarnationState sets an agent's ReincarnationState via UpdateAgent.
// CreateAgent's ent mapping does not persist this field (see
// entadapter/agent_store.go: SetReincarnationState is wired only into
// UpdateAgent's mutation builder), so every test that needs a migrating
// agent must create it first and set this separately.
func setReincarnationState(t *testing.T, s store.Store, a *store.Agent, rs string) {
	t.Helper()
	a.ReincarnationState = rs
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

// rev2MentionSetup mirrors the reviewer's rev2a2MentionSetup helper: a
// primary agent, a migrating mentioned agent, and an admin identity in
// context (pierces per-mention authorization so these tests focus on the
// migration gate, not message-mode setup).
func rev2MentionSetup(t *testing.T) (*Server, store.Store, *store.Agent, *store.Agent, context.Context) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("rvm-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("rvm-p"), Slug: "rvm-p", Name: "rvm-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("rvm-primary"), Slug: "rvm-primary", Name: "rvm-primary", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, primary))
	mentioned := &store.Agent{ID: tid("rvm-target"), Slug: "rvm-target", Name: "rvm-target", ProjectID: project.ID, Phase: string(state.PhaseStarting), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, mentioned))
	setReincarnationState(t, s, mentioned, store.ReincarnationStateStarting)
	srv.SetDispatcher(&brokerMockDispatcher{})
	admin := NewAuthenticatedUser(tid("rvm-admin"), "rvm-admin@test.com", "Admin", "admin", "cli")
	return srv, s, primary, mentioned, contextWithIdentity(ctx, admin)
}

// failCreateMessageForAgentStore wraps a real store and fails CreateMessage
// only for messages addressed to one specific agent, leaving every other
// persist (e.g. the primary's, or any other secondary's) unaffected. Needed
// because createMessageFailStore fails universally, which trips the chat v2
// primary's own persist check (:1496-1500, per p2a-r3 review) before the
// secondary loop is ever reached.
type failCreateMessageForAgentStore struct {
	store.Store
	fault   *storeFaultSwitch // nil: always active
	agentID string
}

// assertBothParticipants asserts that conversationID has exactly the two
// named principals as active participants (order-independent), and that
// GetConversationsForPrincipal for the recipient agent returns it — the
// concrete regression report-7-gteam-2a F1 observed: `conversation list`
// returning [] for a conversation the recipient was a genuine party to.
func assertBothParticipants(t *testing.T, s store.Store, conversationID string, aKind, aID, bKind, bID string) {
	t.Helper()
	ctx := context.Background()

	parts, err := s.ListParticipants(ctx, conversationID)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, p := range parts {
		got[p.PrincipalKind+":"+p.PrincipalID] = true
	}
	assert.True(t, got[aKind+":"+aID], "expected participant %s:%s, got %v", aKind, aID, got)
	assert.True(t, got[bKind+":"+bID], "expected participant %s:%s, got %v", bKind, bID, got)

	// Discoverability: whichever side is an agent must be able to find the
	// conversation via the principal-keyed listing query (F1's root cause
	// citation of GetConversationsForPrincipal).
	for _, side := range []struct{ kind, id string }{{aKind, aID}, {bKind, bID}} {
		if side.kind != "agent" {
			continue
		}
		convs, err := s.GetConversationsForPrincipal(ctx, "agent", side.id)
		require.NoError(t, err)
		found := false
		for _, c := range convs {
			if c.ID == conversationID {
				found = true
				break
			}
		}
		assert.True(t, found, "GetConversationsForPrincipal(agent, %s) must list conversation %s", side.id, conversationID)
	}
}

// getUserErrStore wraps a real store and makes GetUser return a non-NotFound
// error for one specific ID, simulating a transient store failure during
// peer resolution — distinct from a ghost ID, which returns store.ErrNotFound
// and is already covered by the shapes above.
type getUserErrStore struct {
	store.Store
	failID string
}

func (s *failCreateMessageForAgentStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	if s.fault.Active() && msg.AgentID == s.agentID {
		return errors.New("injected CreateMessage failure for target agent")
	}
	return s.Store.CreateMessage(ctx, msg)
}

func (s *getUserErrStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.failID {
		return nil, errors.New("injected store error")
	}
	return s.Store.GetUser(ctx, id)
}
