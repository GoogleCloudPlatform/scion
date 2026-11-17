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
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func connectAgentDeliverSession(t *testing.T, f *holdE2EFixture, target *store.Agent) *connectableControlAgent {
	t.Helper()
	guardSciontoolLog()
	fresh, err := f.store.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)

	tm := &recordingTmux{}
	ctl := control.New(control.Options{
		KickTokenRefresh: func() {},
		Deliver:          &control.DeliverOptions{Runner: tm},
	})
	tok := f.agentToken(t, fresh)
	admitted := make(chan *conduitv1.Welcome, 4)
	a, err := sconduit.New(sconduit.Options{
		HubURL:    f.public.URL,
		AgentID:   fresh.ID,
		ProjectID: fresh.ProjectID,
		LaunchID:  fresh.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(f.clock.Now()), now: f.clock.Now},
		NoPTY:     true,
		Session:   conduit.Config{RPCHandler: control.RPCHandler(ctl)},
		RPCRoutes: ctl.Routes(),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(ctx)
	}()
	ca := &connectableControlAgent{tmux: tm, cancel: cancel, exited: exited}
	t.Cleanup(func() { ca.stop(t) })
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
	return ca
}

func postWebTopicMessage(t *testing.T, f *holdE2EFixture, topicID, content string) chatMessageResponse {
	t.Helper()
	rawBody, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/chat/conversations/"+topicID+"/messages", bytes.NewReader(rawBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(bodyBytes))
	var out chatMessageResponse
	require.NoError(t, json.Unmarshal(bodyBytes, &out))
	return out
}

func collectPastedEnvelopesByID(t *testing.T, ca *connectableControlAgent) map[string]map[string]any {
	t.Helper()
	byID := make(map[string]map[string]any)
	for _, chunk := range ca.tmux.pastedTexts() {
		for _, env := range decodeAllHoldTestEnvelopes(t, chunk) {
			if id, _ := env["message_id"].(string); id != "" {
				byID[id] = env
			}
		}
	}
	return byID
}

// TestHoldA2_WebMessageRedriveTranslatesMentionsOutbound verifies ptone/scion#8212:
// web chat messages held in awaiting_session and later re-driven must deliver
// identical agent-facing @<email> text as live delivery (both when the human
// wrote @<email> and when they wrote @<display-slug>, and across primary and
// secondary @mention recipients), without changing stored/web-display text.
func TestHoldA2_WebMessageRedriveTranslatesMentionsOutbound(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	dbProvider, ok := f.fault.Store.(interface{ DB() *sql.DB })
	require.True(t, ok && dbProvider.DB() != nil, "underlying store must expose *sql.DB")
	db := dbProvider.DB()
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	f.srv.SetWebChatStore(wcs)

	proj, err := f.store.GetProject(ctx, f.launched.ProjectID)
	require.NoError(t, err)
	_ = addProjectHuman(t, f.store, proj, "alice.smith@test.com", "Alice Smith")

	// Non-member Hub user whose @email must stay untouched in both display and agent text.
	hubPeer := &store.User{
		ID:          api.NewUUID(),
		Email:       "hub.peer@test.com",
		DisplayName: "Hub Peer",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, hubPeer))

	// Live-baseline Cloud Run agent (already has a connected Conduit session)
	// plus a secondary Cloud Run agent for @mention fan-out during hold.
	liveAgent := &store.Agent{
		ID:              api.NewUUID(),
		ProjectID:       proj.ID,
		Name:            "LiveWorker",
		Slug:            "live-worker",
		Runtime:         "cloudrun",
		RuntimeBrokerID: f.broker,
		RunID:           "run-live-1",
		Phase:           string(state.PhaseRunning),
		OwnerID:         f.launched.OwnerID,
		CreatedBy:       f.launched.OwnerID,
	}
	secondaryAgent := &store.Agent{
		ID:              api.NewUUID(),
		ProjectID:       proj.ID,
		Name:            "SecondaryReviewer",
		Slug:            "secondary-reviewer",
		Runtime:         "cloudrun",
		RuntimeBrokerID: f.broker,
		RunID:           "run-sec-1",
		Phase:           string(state.PhaseRunning),
		OwnerID:         f.launched.OwnerID,
		CreatedBy:       f.launched.OwnerID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, liveAgent))
	_, err = f.store.SetAgentRunID(ctx, liveAgent.ID, "run-live-1", nil)
	require.NoError(t, err)
	liveAgent, err = f.store.GetAgent(ctx, liveAgent.ID)
	require.NoError(t, err)

	require.NoError(t, f.store.CreateAgent(ctx, secondaryAgent))
	_, err = f.store.SetAgentRunID(ctx, secondaryAgent.ID, "run-sec-1", nil)
	require.NoError(t, err)
	secondaryAgent, err = f.store.GetAgent(ctx, secondaryAgent.ID)
	require.NoError(t, err)

	liveTopicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: liveTopicID, ProjectID: proj.ID, Name: "live-topic",
		CreatedBy: f.launched.OwnerID, CreatedAt: time.Now().UTC(), DefaultAgent: liveAgent.Slug,
	}))
	setTopicConversationID(t, db, f.store, liveTopicID, proj.ID)

	heldTopicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: heldTopicID, ProjectID: proj.ID, Name: "held-topic",
		CreatedBy: f.launched.OwnerID, CreatedAt: time.Now().UTC(), DefaultAgent: f.launched.Slug,
	}))
	setTopicConversationID(t, db, f.store, heldTopicID, proj.ID)

	// 1. Live baseline: connect session on liveAgent first and send both forms.
	liveCA := connectAgentDeliverSession(t, f, liveAgent)

	emailInput := "Coordinate with @alice.smith@test.com and @hub.peer@test.com; keep `@alice.smith@test.com`."
	wantEmailDisplay := "Coordinate with @alice-smith and @hub.peer@test.com; keep `@alice.smith@test.com`."
	slugInput := "Ping @alice-smith and > @alice-smith about deployment."

	liveEmailResp := postWebTopicMessage(t, f, liveTopicID, emailInput)
	assert.Equal(t, wantEmailDisplay, liveEmailResp.Content, "live web response displays @display-slug")
	liveSlugResp := postWebTopicMessage(t, f, liveTopicID, slugInput)
	assert.Equal(t, slugInput, liveSlugResp.Content, "live web response preserves @display-slug")

	f.waitMessageState(t, liveEmailResp.ID, store.MessageDispatchDelivered)
	f.waitMessageState(t, liveSlugResp.ID, store.MessageDispatchDelivered)

	liveEnvs := collectPastedEnvelopesByID(t, liveCA)
	liveEmailAgentMsg, _ := liveEnvs[liveEmailResp.ID]["msg"].(string)
	liveSlugAgentMsg, _ := liveEnvs[liveSlugResp.ID]["msg"].(string)
	require.Equal(t, "Coordinate with @alice.smith@test.com and @hub.peer@test.com; keep `@alice.smith@test.com`.", liveEmailAgentMsg)
	require.Equal(t, "Ping @alice.smith@test.com and > @alice.smith@test.com about deployment.", liveSlugAgentMsg)

	// 2. Held & re-driven path: f.launched and secondaryAgent have NO Conduit session yet.
	//    First message uses @email input (which sendAgentRouted rewrites to @alice-smith in row.Msg)
	//    and enters awaiting_session via /deliver 503 no_session + broker awaiting_session report.
	heldEmailResp := postWebTopicMessage(t, f, heldTopicID, emailInput)
	assert.Equal(t, wantEmailDisplay, heldEmailResp.Content, "held web response must keep @display-slug")
	f.waitMessageState(t, heldEmailResp.ID, store.MessageDispatchAwaitingSession)

	//    Second message uses @display-slug input + secondary @mention fan-out;
	//    f.launched queues it behind the active hold via holdOrDispatch, and
	//    secondaryAgent enters awaiting_session via no_session proof + report.
	fanoutSlugInput := "@" + f.launched.Slug + " ask @" + secondaryAgent.Slug + ": " + slugInput
	wantFanoutAgentMsg := "@" + f.launched.Slug + " ask @" + secondaryAgent.Slug + ": " + liveSlugAgentMsg
	heldSlugResp := postWebTopicMessage(t, f, heldTopicID, fanoutSlugInput)
	assert.Equal(t, fanoutSlugInput, heldSlugResp.Content)
	f.waitMessageState(t, heldSlugResp.ID, store.MessageDispatchAwaitingSession)

	secList, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: secondaryAgent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, secList.Items, 1)
	secMsgID := secList.Items[0].ID
	f.waitMessageState(t, secMsgID, store.MessageDispatchAwaitingSession)

	// 3. Connect Conduit sessions on both held agents -> re-drive runs.
	heldPrimaryCA := connectAgentDeliverSession(t, f, f.launched)
	heldSecondaryCA := connectAgentDeliverSession(t, f, secondaryAgent)

	f.waitMessageState(t, heldEmailResp.ID, store.MessageDispatchDelivered)
	f.waitMessageState(t, heldSlugResp.ID, store.MessageDispatchDelivered)
	f.waitMessageState(t, secMsgID, store.MessageDispatchDelivered)

	// Stored rows for web/chat display must remain untouched (@display-slug).
	storedHeldEmail, err := f.store.GetMessage(ctx, heldEmailResp.ID)
	require.NoError(t, err)
	assert.Equal(t, wantEmailDisplay, storedHeldEmail.Msg, "stored web display text must remain @display-slug after re-drive")
	storedHeldSlug, err := f.store.GetMessage(ctx, heldSlugResp.ID)
	require.NoError(t, err)
	assert.Equal(t, fanoutSlugInput, storedHeldSlug.Msg, "stored web display text must remain @display-slug after re-drive")

	// Re-driven agent-facing text must match live delivery identically (@email).
	primaryEnvs := collectPastedEnvelopesByID(t, heldPrimaryCA)
	assert.Equal(t, liveEmailAgentMsg, primaryEnvs[heldEmailResp.ID]["msg"],
		"re-driven @email web message must deliver @email to primary agent, matching live delivery")
	assert.Equal(t, wantFanoutAgentMsg, primaryEnvs[heldSlugResp.ID]["msg"],
		"re-driven @display-slug web message must deliver @email to primary agent, matching live delivery")

	secondaryEnvs := collectPastedEnvelopesByID(t, heldSecondaryCA)
	assert.Equal(t, wantFanoutAgentMsg, secondaryEnvs[secMsgID]["msg"],
		"re-driven web @mention fan-out must deliver @email to secondary agent, matching live delivery")
}

type projectMemberFaultStore struct {
	store.Store
	failListProjectMembers atomic.Bool
	faultHits              atomic.Int32
}

func (s *projectMemberFaultStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	if s.failListProjectMembers.Load() {
		s.faultHits.Add(1)
		return nil, errInjectedStoreFault
	}
	return s.Store.ListProjectMembers(ctx, projectID)
}

// TestHoldA2_WebMessageRedriveMentionBoundaries verifies non-web / agent-sent
// exclusion and fail-closed re-holding on transient member lookup errors (R3).
func TestHoldA2_WebMessageRedriveMentionBoundaries(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	pmFault, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *projectMemberFaultStore {
		return &projectMemberFaultStore{Store: inner}
	})

	dbProvider, ok := f.fault.Store.(interface{ DB() *sql.DB })
	require.True(t, ok && dbProvider.DB() != nil)
	db := dbProvider.DB()
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	f.srv.SetWebChatStore(wcs)

	proj, err := f.store.GetProject(ctx, f.launched.ProjectID)
	require.NoError(t, err)
	_ = addProjectHuman(t, f.store, proj, "alice.smith@test.com", "Alice Smith")

	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "boundary-topic",
		CreatedBy: f.launched.OwnerID, CreatedAt: time.Now().UTC(), DefaultAgent: f.launched.Slug,
	}))
	setTopicConversationID(t, db, f.store, topicID, proj.ID)

	// 1. Web message held in awaiting_session.
	webResp := postWebTopicMessage(t, f, topicID, "Check with @alice.smith@test.com")
	f.waitMessageState(t, webResp.ID, store.MessageDispatchAwaitingSession)

	// 2. Agent-sent message stamped with Channel="web" containing @alice-smith:
	//    queued behind the active hold via ExecuteAgentDM -> holdOrDispatch.
	//    Live ExecuteAgentDM never runs translateMentionsOutbound, so re-drive must not either.
	dmRes, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    f.launched,
		ProjectID:      proj.ID,
		Msg:            "Agent note mentioning @alice-smith",
		Type:           messages.TypeInstruction,
		Channel:        "web",
	})
	require.Nil(t, dmErr)
	f.waitMessageState(t, dmRes.MessageID, store.MessageDispatchAwaitingSession)

	// 3. Inject transient ListProjectMembers fault, then connect session:
	//    the web message must re-hold (never dispatch untranslated @alice-smith),
	//    and the ordered batch must re-hold subsequent rows behind it too.
	pmFault.failListProjectMembers.Store(true)
	ca := connectAgentDeliverSession(t, f, f.launched)
	require.Eventually(t, func() bool {
		if pmFault.faultHits.Load() < 1 {
			return false
		}
		mWeb, err1 := f.store.GetMessage(ctx, webResp.ID)
		mAgent, err2 := f.store.GetMessage(ctx, dmRes.MessageID)
		return err1 == nil && err2 == nil &&
			mWeb.DispatchState == store.MessageDispatchAwaitingSession &&
			mAgent.DispatchState == store.MessageDispatchAwaitingSession
	}, 5*time.Second, 20*time.Millisecond,
		"transient member lookup fault must re-hold web row and ordered batch rows")
	assert.Empty(t, ca.tmux.pastedTexts(), "no row may dispatch past a re-held web row")

	// 4. Clear fault and trigger re-drive: web message delivers @email, while
	//    agent-sent row delivers its literal @alice-smith text untouched.
	pmFault.failListProjectMembers.Store(false)
	f.srv.scheduleHeldMessageRedrive(f.launched.ID)

	f.waitMessageState(t, webResp.ID, store.MessageDispatchDelivered)
	f.waitMessageState(t, dmRes.MessageID, store.MessageDispatchDelivered)

	envs := collectPastedEnvelopesByID(t, ca)
	assert.Equal(t, "Check with @alice.smith@test.com", envs[webResp.ID]["msg"])
	assert.Equal(t, "Agent note mentioning @alice-smith", envs[dmRes.MessageID]["msg"])
}
