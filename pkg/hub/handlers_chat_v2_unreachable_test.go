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

// Tests for nc-delivery-unreachable: chat v2 must not report "Delivered" for
// a message routed to a primary agent that is unreachable — soft-deleted, or
// in a lifecycle phase where the runtime broker's buffer cannot actually
// reach a container (suspended, stopping, stopped, error). These assertions
// replace repro_nc_unreachable_test.go (nc-unreachable-inv investigation),
// which pinned the buggy behaviour; here they assert the fixed behaviour.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// unreachableTestSetup creates a project and a topic whose default agent has
// the given phase (and is optionally soft-deleted), and wires the given
// dispatcher. Mirrors setupSendTest in handlers_chat_v2_test.go.
func unreachableTestSetup(t *testing.T, phase string, deleted bool, disp AgentDispatcher) (*Server, store.Store, string, *store.Agent) {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(disp)
	ctx := t.Context()
	a := &store.Agent{ID: tid("unreachable-" + phase), ProjectID: proj.ID, Name: "Unreachable", Slug: "unreachable-" + phase,
		Phase: phase, OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if deleted {
		a.DeletedAt = time.Now()
		if err := s.UpdateAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	topicID := tid("unreachable-topic-" + phase)
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "unreachable-" + phase,
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: a.Slug}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	return srv, s, topicID, a
}

// unreachableSend posts content to topicID and returns the HTTP status, the
// decoded JSON response body, and the persisted store row (nil if the
// response carried no message ID).
func unreachableSend(t *testing.T, srv *Server, s store.Store, topicID, content string) (int, map[string]any, *store.Message) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": content})
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	id, _ := resp["id"].(string)
	var m *store.Message
	if id != "" {
		m, _ = s.GetMessage(t.Context(), id)
	}
	t.Logf("HTTP %d body=%s", rec.Code, rec.Body.String())
	return rec.Code, resp, m
}

// Suspended default agent: the phase gate must persist the row failed,
// report it in the response, and never dispatch.
func TestUnreachableNC_SuspendedDefault(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "suspended", false, d)
	code, resp, m := unreachableSend(t, srv, s, topic, "hello")

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if resp["dispatchState"] != "failed" {
		t.Fatalf("expected response dispatchState=failed, got %v", resp["dispatchState"])
	}
	if resp["dispatchFailureCode"] != "agent_unreachable" {
		t.Fatalf("expected dispatchFailureCode=agent_unreachable, got %v", resp["dispatchFailureCode"])
	}
	if len(d.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches, got %d", len(d.getMessages()))
	}
}

// Stopped default agent: same as suspended.
func TestUnreachableNC_StoppedDefault(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "stopped", false, d)
	code, resp, m := unreachableSend(t, srv, s, topic, "hello")

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if resp["dispatchState"] != "failed" {
		t.Fatalf("expected response dispatchState=failed, got %v", resp["dispatchState"])
	}
	if resp["dispatchFailureCode"] != "agent_unreachable" {
		t.Fatalf("expected dispatchFailureCode=agent_unreachable, got %v", resp["dispatchFailureCode"])
	}
	if len(d.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches, got %d", len(d.getMessages()))
	}
}

// Leading @-mention of an error-phase agent (no default): the mentioned
// agent becomes the primary, so the gate applies to it too.
func TestUnreachableNC_LeadingMentionError(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(d)
	ctx := t.Context()
	a := &store.Agent{ID: tid("unreachable-m"), ProjectID: proj.ID, Name: "M", Slug: "error-m", Phase: "error",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	topicID := tid("unreachable-topic-m")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "m", CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	code, resp, m := unreachableSend(t, srv, s, topicID, "@error-m please look")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if resp["dispatchState"] != "failed" || resp["dispatchFailureCode"] != "agent_unreachable" {
		t.Fatalf("expected failed/agent_unreachable, got %v", resp)
	}
	if len(d.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches, got %d", len(d.getMessages()))
	}
}

// Deleted default agent still named on the topic: must be reported as
// unreachable, not silently turned into a human-to-human message.
func TestUnreachableNC_DeletedDefault(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "running", true, d)
	code, resp, m := unreachableSend(t, srv, s, topic, "hello")

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if len(d.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches, got %d", len(d.getMessages()))
	}
	if m == nil {
		t.Fatalf("expected message to be persisted")
	}
	if m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if m.Type == messages.TypeChat {
		t.Fatalf("expected an agent-addressed message, not a human-to-human chat message: %+v", m)
	}
	if resp["dispatchFailureCode"] != "agent_unreachable" {
		t.Fatalf("expected dispatchFailureCode=agent_unreachable, got %v", resp)
	}
}

// Running default agent: dispatches normally, and the dispatch context
// carries the persisted hub message ID (#1820) so a later buffered-delivery
// failure report can still mark the row failed.
func TestUnreachableNC_RunningDefault_Dispatched(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "running", false, d)
	code, resp, m := unreachableSend(t, srv, s, topic, "hello")

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected row dispatched, got %+v", m)
	}
	if resp["dispatchState"] != "dispatched" {
		t.Fatalf("expected response dispatchState=dispatched, got %v", resp["dispatchState"])
	}
	msgs := d.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one dispatch, got %d", len(msgs))
	}
	if msgs[0].messageID == "" {
		t.Fatalf("expected the dispatch context to carry the hub message ID (#1820)")
	}
	if msgs[0].messageID != m.ID {
		t.Fatalf("expected dispatch context message ID %q to match persisted row %q", msgs[0].messageID, m.ID)
	}
}

// A not-yet-running phase (e.g. starting) keeps today's behaviour: dispatch
// proceeds normally because a buffered message lands once the agent is up.
func TestUnreachableNC_StartingDefault_DispatchedAsToday(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "starting", false, d)
	code, _, m := unreachableSend(t, srv, s, topic, "hello")

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", code)
	}
	if m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected row dispatched (not-yet-running phases keep current behaviour), got %+v", m)
	}
	msgs := d.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one dispatch, got %d", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Review round 2 (nc-unreachable-review.md): R1, R2 tests live in
// chat-thread.test.ts / chat-message.test.ts for the frontend half, and here
// for the backend half. R3's three missing-coverage cases and the nit-1
// transient-error test also live here.
// ---------------------------------------------------------------------------

// R1: a human @mention in a topic whose default agent was deleted must still
// notify. sendUnreachableDefaultAgent used to be a near-copy of
// sendHumanToHuman that dropped fireHumanMentionNotifications entirely; it is
// now merged into sendHumanToHuman via unreachableAgentOverride so the two
// paths share one notification pipeline. This test would fail against that
// regression.
func TestUnreachableNC_DeletedDefault_HumanMentionStillNotifies(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "running", true, d)
	ctx := t.Context()

	srv.mu.RLock()
	wcs := srv.webChatStore
	srv.mu.RUnlock()
	tp, err := wcs.GetTopic(ctx, topic)
	if err != nil || tp == nil {
		t.Fatalf("GetTopic: %v", err)
	}
	projectID := tp.ProjectID

	// Create a human project member to @mention.
	humanUser := &store.User{
		ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice",
		Role: "member", Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, humanUser); err != nil {
		t.Fatal(err)
	}
	groupID := api.NewUUID()
	if err := s.CreateGroup(ctx, &store.Group{
		ID: groupID, Name: "unreachable-r1 members", Slug: "project:unreachable-r1:members",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: humanUser.ID, Role: "member",
	}); err != nil {
		t.Fatal(err)
	}
	// Create a project-scoped role binding so resolveProjectHumanMembers
	// (via ListProjectMembers) can find the human.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("GetRoleDefinitionByName: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      humanUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatal(err)
	}

	code, resp, m := unreachableSend(t, srv, s, topic, "hi @Alice, the bot is gone")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if resp["dispatchFailureCode"] != "agent_unreachable" {
		t.Fatalf("expected dispatchFailureCode=agent_unreachable, got %v", resp)
	}

	// fireHumanMentionNotifications runs in a goroutine; poll for it.
	require.Eventually(t, func() bool {
		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, humanUser.ID, false)
		return err == nil && len(notifs) == 1
	}, 2*time.Second, 20*time.Millisecond,
		"human @mention must still notify when the topic default agent was deleted (R1)")
}

// R3: a leading @mention of a running agent must override a deleted topic
// default — dispatched to the mentioned agent, not marked unreachable.
// Correctness here depends on handleConversationSend's len(plan.Agents) > 0
// check short-circuiting before the unresolvedDefaultAgent branch; this test
// pins that ordering.
func TestUnreachableNC_LeadingMentionOverridesDeletedDefault(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, deletedDefault := unreachableTestSetup(t, "running", true, d)
	ctx := t.Context()

	live := &store.Agent{ID: tid("live-b"), ProjectID: deletedDefault.ProjectID, Name: "Live B", Slug: "live-b",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, live); err != nil {
		t.Fatal(err)
	}

	code, resp, m := unreachableSend(t, srv, s, topic, "@live-b hi")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected dispatched (mention overrides the deleted default), got %+v", m)
	}
	if resp["dispatchState"] != "dispatched" {
		t.Fatalf("expected response dispatchState=dispatched, got %v", resp["dispatchState"])
	}
	if m.RecipientID != live.ID {
		t.Fatalf("expected message routed to live-b (%s), got recipient %+v", live.ID, m.Recipient)
	}
	msgs := d.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one dispatch (to live-b, not the deleted default), got %d", len(msgs))
	}
}

// R3: a DM whose peer agent is soft-deleted must be reported unreachable with
// the agent_unreachable code, and dispatch zero times. Unlike the topic
// default case, this goes through sendAgentRouted's isAgentUnreachable gate
// (not the unresolvedDefaultAgent/sendHumanToHuman override), because
// GetAgent returns soft-deleted rows for a DM peer lookup.
func TestUnreachableNC_DM_DeletedPeerAgent(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, _, proj, _ := setupSendTest(t)
	srv.SetDispatcher(d)
	ctx := t.Context()

	agent := &store.Agent{ID: tid("dm-deleted-peer"), ProjectID: proj.ID, Name: "Peer", Slug: "peer",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent.DeletedAt = time.Now()
	if err := s.UpdateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)

	code, resp, m := unreachableSend(t, srv, s, dmKey, "hello")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if resp["dispatchFailureCode"] != "agent_unreachable" {
		t.Fatalf("expected dispatchFailureCode=agent_unreachable, got %v", resp)
	}
	if len(d.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches, got %d", len(d.getMessages()))
	}
}

// R3: the sync dispatch_error branch (dispatchWithBrokerRetry fails
// synchronously after a reachable-looking primary was attempted) must report
// dispatchState=failed and dispatchFailureCode=dispatch_error in the
// response, matching the persisted row.
func TestUnreachableNC_DispatchErrorBranch_ResponseMatchesRow(t *testing.T) {
	wantReason := "broker crashed"
	d := &errorDispatcher{err: errors.New(wantReason)}
	srv, s, topic, _ := unreachableTestSetup(t, "running", false, d)

	code, resp, m := unreachableSend(t, srv, s, topic, "hello")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("expected row failed, got %+v", m)
	}
	if m.DispatchFailureReason == nil || *m.DispatchFailureReason != wantReason {
		t.Fatalf("expected persisted row reason %q, got %+v", wantReason, m.DispatchFailureReason)
	}
	if resp["dispatchState"] != "failed" {
		t.Fatalf("expected response dispatchState=failed, got %v", resp["dispatchState"])
	}
	if resp["dispatchFailureCode"] != "dispatch_error" {
		t.Fatalf("expected dispatchFailureCode=dispatch_error, got %v", resp)
	}
	if resp["dispatchFailureReason"] != wantReason {
		t.Fatalf("expected response reason %q matching the persisted row, got %v", wantReason, resp["dispatchFailureReason"])
	}
}

// transientAgentLookupStore wraps a store and injects a non-ErrNotFound error
// from GetAgentBySlug for a specific slug, to exercise the nit-1 fix: a
// transient store error resolving a topic's default agent must not be
// classified the same as "deleted".
type transientAgentLookupStore struct {
	store.Store
	failSlug string
	err      error
}

func (t *transientAgentLookupStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	if slug == t.failSlug {
		return nil, t.err
	}
	return t.Store.GetAgentBySlug(ctx, projectID, slug)
}

// errListAgentsStore wraps a store and forces ListAgents to fail, which is
// the seam resolveRoutingAgents (via listAllProjectAgents) uses to list
// project agents for mention resolution. Injecting a failure there is the
// cleanest way to force resolveRoutingAgents to return a non-nil error
// without adding a hacky new seam to production code.
type errListAgentsStore struct {
	store.Store
	err error
}

func (e *errListAgentsStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	return nil, e.err
}

// Consider 2 (review round 2): a routing-plan error (resolveRoutingAgents
// itself failing, not just resolving to zero agents) in a topic whose
// default agent is soft-deleted must not be mislabelled "Agent unreachable
// (deleted)". Before the fix, handleConversationSend gated the unreachable-
// default override only on unresolvedDefaultAgent being set, so a plan error
// in this exact topic shape reported the same "Agent unreachable (deleted)"
// outcome as an actually-unreachable default. The fix gates that override on
// planErr == nil, so a plan error now keeps the pre-existing human-to-human
// error handling instead.
func TestUnreachableNC_RoutingPlanError_DeletedDefault_NotMislabelledUnreachable(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := unreachableTestSetup(t, "running", true, d)
	srv.store = &errListAgentsStore{Store: s, err: errors.New("list agents: connection reset by peer")}

	code, resp, m := unreachableSend(t, srv, s, topic, "hello")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil {
		t.Fatalf("expected message to be persisted")
	}
	if m.Type != messages.TypeChat {
		t.Fatalf("expected a human-to-human chat message on a routing-plan error, got type=%q", m.Type)
	}
	if m.DispatchState == store.MessageDispatchFailed {
		t.Fatalf("a routing-plan error must not be persisted as failed/unreachable: %+v", m)
	}
	if resp["dispatchFailureCode"] == "agent_unreachable" {
		t.Fatalf("expected no agent_unreachable code on a routing-plan error, got %v", resp)
	}
	if want := "Agent unreachable (deleted)"; m.DispatchFailureReason != nil && *m.DispatchFailureReason == want {
		t.Fatalf("expected the routing-plan error to NOT persist %q, got %+v", want, m)
	}
}

// Nit 1 (review round 2): a transient (non-not-found) error resolving the
// topic's default agent must not be classified as "deleted" — that would
// permanently persist "Agent unreachable (deleted)" for a DB hiccup. Keep the
// pre-nc-delivery-unreachable behaviour: fall through to an ordinary
// human-to-human message.
func TestUnreachableNC_TransientDefaultLookupError_FallsThroughToHumanToHuman(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(d)
	ctx := t.Context()

	topicID := tid("unreachable-topic-transient")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "transient",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: "ghost-agent"}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	srv.store = &transientAgentLookupStore{Store: s, failSlug: "ghost-agent", err: errors.New("connection reset by peer")}

	code, resp, m := unreachableSend(t, srv, s, topicID, "hello")
	if code != http.StatusCreated {
		t.Fatalf("expected 201 (pre-change fallthrough), got %d (body=%v)", code, resp)
	}
	if m == nil {
		t.Fatalf("expected message to be persisted")
	}
	if m.Type != messages.TypeChat {
		t.Fatalf("expected a human-to-human chat message on a transient lookup error, got type=%q", m.Type)
	}
	if m.DispatchState == store.MessageDispatchFailed {
		t.Fatalf("a transient store error must not be classified as unreachable/deleted: %+v", m)
	}
	if resp["dispatchFailureCode"] == "agent_unreachable" {
		t.Fatalf("expected no agent_unreachable code on a transient lookup error, got %v", resp)
	}
}
