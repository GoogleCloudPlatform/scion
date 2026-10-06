//go:build !no_sqlite

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

package hub

// Tests for the p2a-r3 review round-3 fixes (design agent-reincarnate §3.7,
// Amendment A25.3): R1 (a test for chat v2's secondary persist-failure
// branch, killing mutation M5) and O2 (folding in the reviewer's three F3
// repro tests: DM-resolution failure, agent-sender DM linkage/D-1, and a
// broker-identity sender). All tests use the real SQLite store.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// R1: chat v2 secondary persist-failure branch (mentionPersisted == false)
// ---------------------------------------------------------------------------

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

func (s *failCreateMessageForAgentStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	if s.fault.Active() && msg.AgentID == s.agentID {
		return errors.New("injected CreateMessage failure for target agent")
	}
	return s.Store.CreateMessage(ctx, msg)
}

// TestSendAgentRouted_R1_MigratingSecondaryPersistFailureIsErrorNotDeferred
// is the p2a-r3 review's R1 finding: chat v2's secondary/mention fan-out
// loop has a "persist failed -> error, never deferred" branch
// (handlers_chat_v2.go, mentionPersisted) that mirrors F1's rule on every
// other path, but had no test. Mutation M5 (`if true || mentionPersisted`)
// survived because createMessageFailStore fails the primary's own persist
// first, never reaching this branch — this test isolates the secondary's
// persist failure instead.
func TestSendAgentRouted_R1_MigratingSecondaryPersistFailureIsErrorNotDeferred(t *testing.T) {
	// Installed before createProjectOwnerRoleBinding below, whose mutation
	// audit goroutine reads srv.store (ptone/scion#3184).
	srv, s, failing, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failCreateMessageForAgentStore {
		return &failCreateMessageForAgentStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	broker := &store.RuntimeBroker{ID: tid("r1-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("r1-p"), Slug: "r1-p", Name: "r1-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("r1-primary"), Slug: "r1-primary", Name: "r1-primary", ProjectID: project.ID, Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, primary))
	second := &store.Agent{ID: tid("r1-second"), Slug: "r1-second", Name: "r1-second", ProjectID: project.ID, Phase: string(state.PhaseProvisioning), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, second))
	setReincarnationState(t, s, second, store.ReincarnationStateProvisioning)

	userID := api.NewUUID()
	owner := NewAuthenticatedUser(userID, "r1@test.com", "Owner", "member", "cli")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "r1@test.com", DisplayName: "Owner"}))
	ensureHubMembership(ctx, s, userID)
	srv.seedProjectCreatorMembership(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, userID))
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Fail CreateMessage only for the migrating secondary; the primary's
	// own persist (checked first, per the review) must still succeed so
	// execution actually reaches the secondary loop.
	failing.agentID = second.ID
	fault.Arm()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/topic:"+project.ID+"/messages", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), owner))
	rr := httptest.NewRecorder()
	mentionResults := []messages.MentionResult{{Slug: "r1-second", Status: "delivered"}}
	msgID := srv.sendAgentRouted(rr, req, "topic:"+project.ID, project.ID, owner,
		"hello @r1-second", "Owner", []*store.Agent{primary, second}, []string{"r1-second"}, mentionResults, nil, time.Now(), "", nil, false)

	require.NotEmpty(t, msgID, "the primary's own message must still be persisted; response: %d %s", rr.Code, rr.Body.String())

	assert.Equal(t, "error", mentionResults[0].Status,
		"an unpersisted migrating secondary must be reported as error, never deferred")
	assert.Contains(t, mentionResults[0].Error, "reincarnating")

	for _, d := range dispatcher.getMessages() {
		assert.NotEqual(t, "r1-second", d.agentSlug, "no dispatch call may be made to the migrating secondary")
	}
	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: second.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "no row may exist for a secondary whose persist failed")
}

// ---------------------------------------------------------------------------
// O2: strengthen F3's assertions (reviewer repros, folded in)
// ---------------------------------------------------------------------------

// rev3UpsertFailStore wraps a real store and makes conversation resolution
// fail, simulating a DM-resolution error independent of message
// persistence.
type rev3UpsertFailStore struct{ store.Store }

func (s *rev3UpsertFailStore) UpsertConversationByExternalRef(_ context.Context, _ *store.Conversation) (*store.Conversation, error) {
	return nil, errors.New("injected upsert failure")
}

// TestProcessMentions_O2_DeferredMentionDMResolutionFailure is the
// reviewer's TestRev2a3Repro_DeferredMentionDMResolutionFailure: when the
// sender<->mentioned-agent DM conversation cannot be resolved for a
// deferred mention, F1's rule applies — error, never deferred, no row
// persisted (a row with no reachable conversation would be a silent
// F3 regression).
func TestProcessMentions_O2_DeferredMentionDMResolutionFailure(t *testing.T) {
	srv, s, primary, mentioned, mctx := rev2MentionSetup(t)
	srv.store = &rev3UpsertFailStore{Store: s}
	orig := messages.NewInstruction("user:tester", "agent:"+primary.Slug, "hey @"+mentioned.Slug)
	orig.SenderID = tid("r3-dm-fail-user")

	res := srv.processMentions(mctx, []string{mentioned.Slug}, primary, orig, "", "")
	require.Len(t, res, 1)
	assert.Equal(t, "error", res[0].Status)
	// O-a (p2a-r4 review): pin that this "error" specifically came from the
	// DM-resolution branch, not some other error path in the loop.
	assert.Equal(t, "agent is reincarnating and the catch-up conversation could not be resolved", res[0].Error)

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: mentioned.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "no row may be persisted when DM resolution fails for a deferred mention")
}

// TestProcessMentions_O2_DeferredMentionAgentSenderDM is the reviewer's
// TestRev2a3Repro_DeferredMentionAgentSenderDM: strengthens F3's assertion
// from "has some conversation_id" to "is specifically the sender<->mentioned
// direct DM" (D-1: participants are exactly {sender, mentioned}, never the
// primary), for an agent sender (not just the user-sender case the original
// F3 test covered).
func TestProcessMentions_O2_DeferredMentionAgentSenderDM(t *testing.T) {
	srv, s, primary, mentioned, _ := rev2MentionSetup(t)
	ctx := context.Background()

	sender := &store.Agent{
		ID: tid("r3-sender"), Slug: "r3-sender", Name: "r3-sender",
		ProjectID: primary.ProjectID, Phase: "running", RuntimeBrokerID: primary.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))
	mentioned.MessageMode = store.MessageModeProject
	require.NoError(t, s.UpdateAgent(ctx, mentioned))

	actx := contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: sender.ID}, ProjectID: sender.ProjectID,
	}})
	orig := messages.NewInstruction("agent:r3-sender", "agent:"+primary.Slug, "hey @"+mentioned.Slug)
	orig.SenderID = sender.ID

	res := srv.processMentions(actx, []string{mentioned.Slug}, primary, orig, "", "")
	require.Len(t, res, 1)
	require.Equal(t, "deferred", res[0].Status,
		"an agent sender must reach the deferred path the same as a user sender")

	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: mentioned.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)

	conv, err := s.GetConversation(ctx, rows.Items[0].ConversationID)
	require.NoError(t, err)
	assert.Equal(t, "direct", conv.Kind, "a deferred mention's conversation must be a direct DM, not the parent group")

	parts, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, p := range parts {
		ids[p.PrincipalID] = true
	}
	assert.True(t, ids[sender.ID], "the sender must be a DM participant")
	assert.True(t, ids[mentioned.ID], "the mentioned agent must be a DM participant")
	assert.False(t, ids[primary.ID], "D-1: the primary must NOT be a participant in the mention's DM")
}

// TestProcessMentions_O2_DeferredMentionBrokerSender is the reviewer's
// TestRev2a3Repro_DeferredMentionBrokerSender: a sender identity that is
// neither a user nor an agent (e.g. a broker) must never be reported
// "deferred" for a mention. N1 (p2a-r4 review): a broker identity is
// actually denied by authorizeAgentMessage before the migration gate is
// ever reached — the defensive `authID == ""` guard in processMentions
// (handlers_agent_messaging.go) is currently unreachable by this path, not
// exercised by it. The assertion is tightened to the exact observed
// status so a future authz change that lets brokers reach the gate is
// noticed (rather than this test silently continuing to pass on some
// other non-"deferred" status).
func TestProcessMentions_O2_DeferredMentionBrokerSender(t *testing.T) {
	srv, _, primary, mentioned, _ := rev2MentionSetup(t)
	bctx := contextWithBrokerIdentity(context.Background(), NewBrokerIdentity(primary.RuntimeBrokerID))
	orig := messages.NewInstruction("system:x", "agent:"+primary.Slug, "hey @"+mentioned.Slug)

	res := srv.processMentions(bctx, []string{mentioned.Slug}, primary, orig, "", "")
	require.Len(t, res, 1)
	assert.Equal(t, "unauthorized", res[0].Status,
		"a broker sender is denied by authz before the migration gate, not by the authID==\"\" guard")
}
