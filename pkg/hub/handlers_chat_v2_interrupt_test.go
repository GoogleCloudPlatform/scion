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

// Tests for the optional "interrupt" flag on the chat v2 conversation send
// endpoint (POST /api/v1/chat/conversations/{key}/messages). The primary and
// every @mention secondary carry the flag when running, as in routed inbound,
// which shares the same routing planner. A recipient that is not running gets
// an ordinary (buffered) dispatch, recipients whose dispatch is skipped
// (unreachable or reincarnating) ignore it, and human-to-human sends ignore
// it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func interruptTestAgent(t *testing.T, s store.Store, projectID, slug, phase string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:        tid("interrupt-" + slug),
		ProjectID: projectID,
		Name:      slug,
		Slug:      slug,
		Phase:     phase,
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(t.Context(), a); err != nil {
		t.Fatalf("CreateAgent(%s): %v", slug, err)
	}
	return a
}

// interruptBrokerDispatcher mimics the runtime broker: a non-interrupt send
// is accepted (the broker buffers it until the agent is up), while an
// interrupt send is delivered immediately and fails synchronously when the
// agent is not running, or when the agent's slug is listed in failInterrupt.
// Every attempt is recorded, including failed ones.
type interruptBrokerDispatcher struct {
	brokerMockDispatcher
	failInterrupt map[string]bool
}

func (d *interruptBrokerDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	_ = d.brokerMockDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
	if interrupt && (state.Phase(agent.Phase) != state.PhaseRunning || d.failInterrupt[agent.Slug]) {
		return fmt.Errorf("agent '%s' not found or not running", agent.Slug)
	}
	return nil
}

// interruptTopicSend creates a topic whose default agent is primarySlug,
// posts body to it and returns the decoded response.
func interruptTopicSend(t *testing.T, srv *Server, wcs WebChatStore, s store.Store, db *sql.DB, projectID, primarySlug string, body map[string]interface{}) chatMessageResponse {
	t.Helper()
	topicID := tid("interrupt-topic")
	if err := wcs.CreateTopic(t.Context(), WebChatTopic{
		ID:           topicID,
		ProjectID:    projectID,
		Name:         "interrupt-topic",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: primarySlug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, projectID)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp chatMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func interruptMentionResult(t *testing.T, resp chatMessageResponse, slug string) messages.MentionResult {
	t.Helper()
	for _, mr := range resp.Mentions {
		if mr.Slug == slug {
			return mr
		}
	}
	t.Fatalf("no mention result for %s in %+v", slug, resp.Mentions)
	return messages.MentionResult{}
}

func interruptMentionRow(t *testing.T, s store.Store, agentID string) store.Message {
	t.Helper()
	rows, err := s.ListMessages(t.Context(), store.MessageFilter{AgentID: agentID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(rows.Items) != 1 {
		t.Fatalf("expected 1 mention row for %s, got %d", agentID, len(rows.Items))
	}
	return rows.Items[0]
}

func dispatchesBySlug(dispatched []brokerDispatchedMsg) map[string]brokerDispatchedMsg {
	out := make(map[string]brokerDispatchedMsg, len(dispatched))
	for _, d := range dispatched {
		out[d.agentSlug] = d
	}
	return out
}

// Topic default agent is the primary and a second agent is @mentioned:
// interrupt reaches both dispatches when requested and neither otherwise,
// matching routed inbound's propagation to secondary mention recipients.
func TestChatV2Interrupt_TopicDefaultPrimaryAndMention(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
		want bool
	}{
		{"interrupt true", map[string]interface{}{"content": "stop and look @int-second", "interrupt": true}, true},
		{"interrupt false", map[string]interface{}{"content": "stop and look @int-second", "interrupt": false}, false},
		{"interrupt absent", map[string]interface{}{"content": "stop and look @int-second"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, wcs, proj, db := setupSendTest(t)
			dispatcher := &brokerMockDispatcher{}
			srv.SetDispatcher(dispatcher)

			primary := interruptTestAgent(t, s, proj.ID, "int-primary", "running")
			interruptTestAgent(t, s, proj.ID, "int-second", "running")

			topicID := tid("interrupt-topic")
			if err := wcs.CreateTopic(t.Context(), WebChatTopic{
				ID:           topicID,
				ProjectID:    proj.ID,
				Name:         "interrupt-topic",
				CreatedBy:    "dev",
				CreatedAt:    time.Now().UTC(),
				DefaultAgent: primary.Slug,
			}); err != nil {
				t.Fatalf("CreateTopic: %v", err)
			}
			setTopicConversationID(t, db, s, topicID, proj.ID)

			rec := doRequest(t, srv, http.MethodPost,
				"/api/v1/chat/conversations/"+topicID+"/messages", tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
			}

			got := dispatchesBySlug(dispatcher.getMessages())
			p, ok := got["int-primary"]
			if !ok {
				t.Fatalf("primary not dispatched; got %v", got)
			}
			if p.interrupt != tc.want {
				t.Errorf("primary interrupt = %v, want %v", p.interrupt, tc.want)
			}
			sec, ok := got["int-second"]
			if !ok {
				t.Fatalf("mention secondary not dispatched; got %v", got)
			}
			if sec.interrupt != tc.want {
				t.Errorf("mention secondary interrupt = %v, want %v", sec.interrupt, tc.want)
			}
		})
	}
}

// Agent DM: the DM's agent is the primary and receives the interrupt.
func TestChatV2Interrupt_AgentDMPrimary(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := interruptTestAgent(t, s, proj.ID, "int-dm-agent", "running")
	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages",
		map[string]interface{}{"content": "stop", "interrupt": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if !dispatched[0].interrupt {
		t.Error("agent DM primary dispatch must carry interrupt=true")
	}
}

// A leading @mention makes that agent the primary; it receives the interrupt.
func TestChatV2Interrupt_LeadingMentionPrimary(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	interruptTestAgent(t, s, proj.ID, "int-lead", "running")

	topicID := tid("interrupt-lead-topic")
	if err := wcs.CreateTopic(t.Context(), WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "interrupt-lead-topic",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]interface{}{"content": "@int-lead stop now", "interrupt": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	got := dispatchesBySlug(dispatcher.getMessages())
	p, ok := got["int-lead"]
	if !ok {
		t.Fatalf("leading-mention primary not dispatched; got %v", got)
	}
	if !p.interrupt {
		t.Error("leading-mention primary dispatch must carry interrupt=true")
	}
}

// Human-to-human DM: interrupt is accepted and ignored — nothing is dispatched.
func TestChatV2Interrupt_HumanDMIgnored(t *testing.T) {
	srv, _, _, _, _ := setupSendTest(t)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	key := "dm:user:" + DevUserID + ":user:" + tid("interrupt-peer")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages",
		map[string]interface{}{"content": "hi there", "interrupt": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(dispatcher.getMessages()); n != 0 {
		t.Fatalf("human DM must not dispatch to any agent, got %d dispatches", n)
	}
}

// Unreachable primary: interrupt is ignored — the row is marked failed and
// nothing is dispatched, same as without the flag. This guards against an
// interrupt bypassing the phase gate (an "interrupt forces delivery"
// regression would dispatch into a suspended agent).
func TestChatV2Interrupt_UnreachableAgentIgnored(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := interruptTestAgent(t, s, proj.ID, "int-suspended", "suspended")
	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages",
		map[string]interface{}{"content": "stop", "interrupt": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(dispatcher.getMessages()); n != 0 {
		t.Fatalf("unreachable agent must not be dispatched, got %d dispatches", n)
	}
}

// Reincarnating primary: interrupt is ignored — the send is deferred for
// catch-up and nothing is dispatched into the migrating container.
func TestChatV2Interrupt_ReincarnatingPrimaryDeferred(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := interruptTestAgent(t, s, proj.ID, "int-reincarnating", string(state.PhaseProvisioning))
	setReincarnationState(t, s, agent, store.ReincarnationStateProvisioning)
	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages",
		map[string]interface{}{"content": "stop", "interrupt": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(dispatcher.getMessages()); n != 0 {
		t.Fatalf("reincarnating agent must not be dispatched, got %d dispatches", n)
	}

	var resp chatMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DispatchState != store.MessageDispatchDeferred {
		t.Errorf("response dispatchState = %q, want %q", resp.DispatchState, store.MessageDispatchDeferred)
	}
	stored, err := s.GetMessage(t.Context(), resp.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if stored.DispatchState != store.MessageDispatchDeferred {
		t.Errorf("stored dispatchState = %q, want %q", stored.DispatchState, store.MessageDispatchDeferred)
	}
}

// A secondary that is not running yet (or is suspended) must not be sent an
// interrupt: the broker would deliver it immediately and fail, losing the
// message. It gets the ordinary non-interrupt dispatch, which the broker
// buffers, while the running primary is still interrupted.
func TestChatV2Interrupt_NonRunningMentionNotInterrupted(t *testing.T) {
	for _, phase := range []string{
		string(state.PhaseStarting),
		string(state.PhaseProvisioning),
		string(state.PhaseSuspended),
	} {
		t.Run(phase, func(t *testing.T) {
			srv, s, wcs, proj, db := setupSendTest(t)
			dispatcher := &interruptBrokerDispatcher{}
			srv.SetDispatcher(dispatcher)

			interruptTestAgent(t, s, proj.ID, "int-primary", "running")
			second := interruptTestAgent(t, s, proj.ID, "int-second", phase)

			resp := interruptTopicSend(t, srv, wcs, s, db, proj.ID, "int-primary",
				map[string]interface{}{"content": "stop and look @int-second", "interrupt": true})

			got := dispatchesBySlug(dispatcher.getMessages())
			if p, ok := got["int-primary"]; !ok || !p.interrupt {
				t.Errorf("running primary must be dispatched with interrupt=true; got %+v", got)
			}
			sec, ok := got["int-second"]
			if !ok {
				t.Fatalf("non-running secondary not dispatched; got %+v", got)
			}
			if sec.interrupt {
				t.Errorf("non-running (%s) secondary dispatched with interrupt=true; want a buffered non-interrupt dispatch", phase)
			}
			if mr := interruptMentionResult(t, resp, "int-second"); mr.Status != "delivered" {
				t.Errorf("mention status = %q (%s), want delivered", mr.Status, mr.Error)
			}
			if row := interruptMentionRow(t, s, second.ID); row.DispatchState != store.MessageDispatchDispatched {
				t.Errorf("mention row dispatchState = %q, want %q", row.DispatchState, store.MessageDispatchDispatched)
			}
		})
	}
}

// A running secondary whose interrupt dispatch fails synchronously is
// reported as failed: the stored mention row is failed and the response's
// mention status is error, not delivered.
func TestChatV2Interrupt_MentionDispatchFailureReported(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	dispatcher := &interruptBrokerDispatcher{failInterrupt: map[string]bool{"int-second": true}}
	srv.SetDispatcher(dispatcher)

	primary := interruptTestAgent(t, s, proj.ID, "int-primary", "running")
	second := interruptTestAgent(t, s, proj.ID, "int-second", "running")

	resp := interruptTopicSend(t, srv, wcs, s, db, proj.ID, "int-primary",
		map[string]interface{}{"content": "stop and look @int-second", "interrupt": true})

	if sec, ok := dispatchesBySlug(dispatcher.getMessages())["int-second"]; !ok || !sec.interrupt {
		t.Fatalf("running secondary must get an interrupt dispatch attempt; got %+v", dispatcher.getMessages())
	}
	mr := interruptMentionResult(t, resp, "int-second")
	if mr.Status != "error" || mr.Error == "" {
		t.Errorf("mention result = %+v, want status error with a reason", mr)
	}
	row := interruptMentionRow(t, s, second.ID)
	if row.DispatchState != store.MessageDispatchFailed {
		t.Errorf("mention row dispatchState = %q, want %q", row.DispatchState, store.MessageDispatchFailed)
	}
	if resp.DispatchState != store.MessageDispatchDispatched {
		t.Errorf("primary dispatchState = %q, want %q", resp.DispatchState, store.MessageDispatchDispatched)
	}

	// Only actually-dispatched agents become group participants: the
	// primary is recorded, the failed secondary is not.
	var convID string
	if err := db.QueryRowContext(t.Context(),
		"SELECT conversation_id FROM webchat_topic WHERE project_id = ? AND default_agent = ?",
		proj.ID, "int-primary").Scan(&convID); err != nil {
		t.Fatalf("lookup topic conversation: %v", err)
	}
	participants, err := s.ListParticipants(t.Context(), convID)
	if err != nil {
		t.Fatalf("ListParticipants: %v", err)
	}
	var primaryRecorded bool
	for _, p := range participants {
		if p.PrincipalKind != "agent" {
			continue
		}
		if p.PrincipalID == second.ID {
			t.Errorf("failed secondary %s recorded as a group participant", second.ID)
		}
		if p.PrincipalID == primary.ID {
			primaryRecorded = true
		}
	}
	if !primaryRecorded {
		t.Errorf("primary %s not recorded as a group participant; got %+v", primary.ID, participants)
	}
}

// A reincarnating secondary ignores interrupt: it is deferred for catch-up
// and nothing is dispatched into the migrating container, while the running
// primary is still interrupted.
func TestChatV2Interrupt_ReincarnatingMentionDeferred(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	interruptTestAgent(t, s, proj.ID, "int-primary", "running")
	second := interruptTestAgent(t, s, proj.ID, "int-second", "running")
	setReincarnationState(t, s, second, store.ReincarnationStatePending)

	resp := interruptTopicSend(t, srv, wcs, s, db, proj.ID, "int-primary",
		map[string]interface{}{"content": "stop and look @int-second", "interrupt": true})

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 || dispatched[0].agentSlug != "int-primary" || !dispatched[0].interrupt {
		t.Fatalf("expected exactly one interrupt dispatch to int-primary, got %+v", dispatched)
	}
	if mr := interruptMentionResult(t, resp, "int-second"); mr.Status != "deferred" {
		t.Errorf("mention status = %q, want deferred", mr.Status)
	}
	if row := interruptMentionRow(t, s, second.ID); row.DispatchState != store.MessageDispatchDeferred {
		t.Errorf("mention row dispatchState = %q, want %q", row.DispatchState, store.MessageDispatchDeferred)
	}
}

// A primary that is not running yet must not be sent an interrupt: it gets
// the ordinary non-interrupt dispatch, which the broker buffers until the
// agent comes up, instead of an immediate delivery that fails.
func TestChatV2Interrupt_StartingPrimaryBuffered(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	dispatcher := &interruptBrokerDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := interruptTestAgent(t, s, proj.ID, "int-starting", string(state.PhaseStarting))
	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages",
		map[string]interface{}{"content": "stop", "interrupt": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].interrupt {
		t.Error("starting primary dispatched with interrupt=true; want a buffered non-interrupt dispatch")
	}

	var resp chatMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DispatchState != store.MessageDispatchDispatched || resp.DispatchFailureCode != "" {
		t.Errorf("response dispatchState = %q (code %q), want %q", resp.DispatchState, resp.DispatchFailureCode, store.MessageDispatchDispatched)
	}
	stored, err := s.GetMessage(t.Context(), resp.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if stored.DispatchState != store.MessageDispatchDispatched {
		t.Errorf("stored dispatchState = %q, want %q", stored.DispatchState, store.MessageDispatchDispatched)
	}
}
