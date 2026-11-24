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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reuseTrackingBrokerClient extends headlessGatherBrokerClient with hooks for
// blocking creates, counting/recording delivered messages, and simulating
// suspended-agent wake starts.
type reuseTrackingBrokerClient struct {
	*headlessGatherBrokerClient
	onBeforeCreate func(req *RemoteCreateAgentRequest) error

	msgMu       sync.Mutex
	sentMsgs    []reuseSentMsg
	startCalls  []string
	storeForUpd store.Store
}

type reuseSentMsg struct {
	AgentID string
	Message string
	Event   json.RawMessage
}

func (b *reuseTrackingBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	if b.onBeforeCreate != nil {
		if err := b.onBeforeCreate(req); err != nil {
			return nil, nil, err
		}
	}
	return b.headlessGatherBrokerClient.CreateAgentWithGather(ctx, brokerID, endpoint, req)
}

func (b *reuseTrackingBrokerClient) MessageAgent(_ context.Context, _, _, agentID, _, message string, _ bool, structuredMsg *messages.StructuredMessage) error {
	b.msgMu.Lock()
	defer b.msgMu.Unlock()
	var rawEv json.RawMessage
	if structuredMsg != nil {
		rawEv = structuredMsg.Event
	}
	b.sentMsgs = append(b.sentMsgs, reuseSentMsg{
		AgentID: agentID,
		Message: message,
		Event:   rawEv,
	})
	return nil
}

func (b *reuseTrackingBrokerClient) StartAgent(ctx context.Context, _, _, agentID, projectID, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, _ StartExtras) (*RemoteAgentResponse, error) {
	b.msgMu.Lock()
	b.startCalls = append(b.startCalls, agentID)
	b.msgMu.Unlock()
	if b.storeForUpd != nil {
		ag, err := b.storeForUpd.GetAgent(ctx, agentID)
		if err != nil || ag == nil {
			ag, err = b.storeForUpd.GetAgentBySlug(ctx, projectID, agentID)
		}
		if err == nil && ag != nil {
			ag.Phase = string(state.PhaseRunning)
			ag.Activity = string(state.ActivityWaitingForInput)
			_ = b.storeForUpd.UpdateAgent(ctx, ag)
		}
	}
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:     agentID,
			Phase:  string(state.PhaseRunning),
			Status: string(state.PhaseRunning),
		},
	}, nil
}

func (b *reuseTrackingBrokerClient) messages() []reuseSentMsg {
	b.msgMu.Lock()
	defer b.msgMu.Unlock()
	out := make([]reuseSentMsg, len(b.sentMsgs))
	copy(out, b.sentMsgs)
	return out
}

func (b *reuseTrackingBrokerClient) starts() []string {
	b.msgMu.Lock()
	defer b.msgMu.Unlock()
	out := make([]string, len(b.startCalls))
	copy(out, b.startCalls)
	return out
}

func TestTriggerG4a_Reuse_TwoConcurrentDrainersAndWakeSuspended(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-reuse")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "reuse-ep")
	createReq := CreateTriggerRequest{
		Name:   "reuse-pr-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.pr_comment"}},
		Subject: &TriggerSubjectSpec{
			Mode: store.TriggerSubjectModeReuse,
		},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionDispatchAgent,
			Task:          "Initial task for ${event.subject}",
			AgentName:     "pr-reuse-${event.id}",
			ReuseMessage:  "Follow-up on ${event.subject}: ${event.attributes.note?}",
			UntrustedVars: []string{"event.subject", "event.attributes.note"},
		},
	}
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID), createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))
	assert.Equal(t, store.TriggerSubjectModeReuse, trigResp.Subject.Mode)

	var createCalls atomic.Int32
	enterCreate := make(chan struct{}, 4)
	releaseCreate := make(chan struct{})
	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
		onBeforeCreate: func(_ *RemoteCreateAgentRequest) error {
			n := createCalls.Add(1)
			enterCreate <- struct{}{}
			if n == 1 {
				<-releaseCreate
			}
			return nil
		},
	}
	disp := NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default())
	f.sched.srv.SetDispatcher(disp)

	// 1. Two concurrent drainers receive events for the SAME subject -> exactly 1 agent created.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-reuse-1", "pr_comment", "pr-42", []byte(`{"n":1}`)).Code)
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-reuse-2", "pr_comment", "pr-42", []byte(`{"n":2}`)).Code)

	now := time.Now().UTC().Truncate(time.Second)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, clock)
	dispA, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)
	dispB, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	dqStore := f.sched.store.(store.DurableQueueStore)
	claimNow := time.Now().UTC()
	itemsA, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "replica-a", claimNow, claimNow.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, itemsA, 1)
	itemsB, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "replica-b", claimNow, claimNow.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, itemsB, 1)

	toItem := func(r store.DurableQueueRow) durablequeue.Item {
		return durablequeue.Item{
			Seq: r.Seq, Kind: r.Kind, DedupKey: r.DedupKey,
			ProjectID: r.ProjectID, Payload: []byte(r.Payload), Attempts: r.Attempts,
		}
	}

	aDone := make(chan durablequeue.Result, 1)
	bDone := make(chan durablequeue.Result, 1)
	go func() { aDone <- dispA.HandleItem(ctx, toItem(itemsA[0]), nil) }()

	// Wait until replica A has bound the claim (in provisioning phase) and entered broker create.
	select {
	case <-enterCreate:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first replica to enter broker create")
	}

	// Start replica B while A's agent is in flight (provisioning): per Stage 3 §6.2
	// rev 1.16, B defers delivery non-terminally with RetryAt instead of failing wake.
	go func() { bDone <- dispB.HandleItem(ctx, toItem(itemsB[0]), nil) }()
	var resB durablequeue.Result
	select {
	case resB = <-bDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for second replica to defer")
	}
	assert.False(t, resB.Done, "in-flight target agent must defer B via RetryAt")
	assert.Equal(t, clock().Add(trigger.ReuseDeferBackoff), resB.RetryAt)

	firesBDeferred, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		TriggerID: trigResp.ID,
		EventID:   itemsB[0].DedupKey,
	}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesBDeferred.Items, 1)
	assert.Equal(t, store.TriggerFireStatusDeferred, firesBDeferred.Items[0].Status)
	deferredFireID := firesBDeferred.Items[0].ID

	close(releaseCreate)
	var resA durablequeue.Result
	select {
	case resA = <-aDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first replica to finish")
	}

	assert.True(t, resA.Done)
	_, _ = dqStore.CompleteDurableItem(ctx, itemsA[0].Seq, "replica-a")
	_, _ = dqStore.CompleteDurableItem(ctx, itemsB[0].Seq, "replica-b")

	assert.Equal(t, int32(1), createCalls.Load(), "exactly one agent must be created across two concurrent drainers")

	firesA, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		TriggerID: trigResp.ID,
		EventID:   itemsA[0].DedupKey,
	}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesA.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, firesA.Items[0].Status)

	subjectKey := "webhook:" + ep.ID + "/pr-42"
	claim, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	agentID := claim.AgentID
	require.NotEmpty(t, agentID)

	// Mark A's agent running (launch complete), then advance clock past RetryAt and
	// re-handle B: B's deferred fire promotes in place and delivers to A's agent.
	ag, err := f.sched.store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	ag.Phase = string(state.PhaseRunning)
	ag.Activity = string(state.ActivityWaitingForInput)
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag))

	advanceClock(trigger.ReuseDeferBackoff)
	resB2 := dispB.HandleItem(ctx, toItem(itemsB[0]), nil)
	assert.True(t, resB2.Done)
	assert.Zero(t, resB2.RetryAt)
	assert.Equal(t, int32(1), createCalls.Load(), "re-handling deferred B must reuse A's agent")

	firesBDelivered, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		TriggerID: trigResp.ID,
		EventID:   itemsB[0].DedupKey,
	}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesBDelivered.Items, 1)
	assert.Equal(t, deferredFireID, firesBDelivered.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, firesBDelivered.Items[0].Status)

	msgsAfterB := tracker.messages()
	require.Len(t, msgsAfterB, 1, "deferred B event must be delivered to A's agent once running")
	assert.Equal(t, ag.Slug, msgsAfterB[0].AgentID)
	assert.Contains(t, msgsAfterB[0].Message, "Follow-up on "+subjectKey)

	// 2. Later event on the same subject -> delivered as wake message to the existing agent.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-reuse-3", "pr_comment", "pr-42", []byte(`{"n":3}`)).Code)
	f.drainOneTriggerEvent(t)

	assert.Equal(t, int32(1), createCalls.Load(), "reuse must not create a second agent")
	msgs := tracker.messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, ag.Slug, msgs[1].AgentID)
	assert.Contains(t, msgs[1].Message, "Follow-up on "+subjectKey)
	assert.NotEmpty(t, msgs[1].Event)

	// 3. Suspend the agent -> next event on the same subject wakes it and delivers the message.
	ag.Phase = string(state.PhaseSuspended)
	ag.Activity = ""
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag))

	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-reuse-4", "pr_comment", "pr-42", []byte(`{"n":4}`)).Code)
	f.drainOneTriggerEvent(t)

	assert.Equal(t, int32(1), createCalls.Load(), "suspended agent must be woken, not replaced")
	assert.Equal(t, []string{ag.Slug}, tracker.starts(), "suspended agent must be woken via StartAgent")
	msgs = tracker.messages()
	require.Len(t, msgs, 3)
	assert.Equal(t, ag.Slug, msgs[2].AgentID)
}

func TestTriggerG4a_Reuse_TerminalSoftDeletedAndCreateFailure(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-reclaim")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "reclaim-ep")
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "reuse-reclaim-trigger",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.pr_sync"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:         store.TriggerActionDispatchAgent,
				Task:         "Sync PR",
				ReuseMessage: "Updated PR",
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))

	var failNext atomic.Bool
	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
		onBeforeCreate: func(_ *RemoteCreateAgentRequest) error {
			if failNext.Swap(false) {
				return errors.New("simulated broker failure")
			}
			return nil
		},
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	subjectKey := "webhook:" + ep.ID + "/pr-77"

	// 1. Broker create failure marks the agent terminal (error) so Live lazily
	// releases the claim and the next event dispatches cleanly.
	failNext.Store(true)
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-fail-1", "pr_sync", "pr-77", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)

	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, nil)
	liveAfterFail, err := claims.Live(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	assert.Nil(t, liveAfterFail, "failed create must leave no live claim")
	_, err = f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	assert.ErrorIs(t, err, store.ErrNotFound, "claim must be released after create failure")

	// 2. Next event succeeds and binds agent A1.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-ok-2", "pr_sync", "pr-77", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)
	claim1, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	agentA1 := claim1.AgentID
	require.NotEmpty(t, agentA1)

	// 3. Mark A1 terminal (stopped) -> claim released, new agent A2 dispatched.
	ag1, err := f.sched.store.GetAgent(ctx, agentA1)
	require.NoError(t, err)
	ag1.Phase = string(state.PhaseStopped)
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag1))

	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-term-3", "pr_sync", "pr-77", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)
	claim2, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	agentA2 := claim2.AgentID
	require.NotEmpty(t, agentA2)
	assert.NotEqual(t, agentA1, agentA2, "terminal agent must release claim and dispatch new agent")

	// 4. Soft-delete A2 -> claim released, new agent A3 dispatched.
	ag2, err := f.sched.store.GetAgent(ctx, agentA2)
	require.NoError(t, err)
	ag2.DeletedAt = time.Now().UTC()
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag2))

	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-softdel-4", "pr_sync", "pr-77", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)
	claim3, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	agentA3 := claim3.AgentID
	require.NotEmpty(t, agentA3)
	assert.NotEqual(t, agentA2, agentA3, "soft-deleted agent must release claim and dispatch new agent")
}

func TestTriggerG4a_Reuse_AuthoringRequiresWakeAuthority(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-wake-auth")
	f.sched.srv.authzService.mintDevAuthOverride = false
	owner := &store.User{
		ID:          uuid.NewString(),
		Email:       "wake-auth-owner@example.com",
		DisplayName: "Wake Auth Owner",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.CreateUser(ctx, owner))
	require.NoError(t, f.sched.srv.createProjectOwnerRoleBinding(ctx, f.sched.proj.ID, owner.ID))
	userCtx := f.userAuth(t, owner)
	ep := f.createEndpoint(t, userCtx, "wake-auth-ep")

	// Create an agent caller with AgentRoleBaseline (so its stored ceiling lacks
	// agent.lifecycle).
	agNoWake := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "ag-no-wake",
		Name:          "ag-no-wake",
		ProjectID:     f.sched.proj.ID,
		Phase:         string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleBaseline)},
		OwnerID:       f.sched.creator.ID,
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, agNoWake))

	noWakeIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agNoWake.ID},
		ProjectID: agNoWake.ProjectID,
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}, // deliberately omits ScopeAgentLifecycle
	}}
	noWakeCtx := contextWithIdentity(ctx, noWakeIdent)
	noWakeCtx = contextWithCredentialContext(noWakeCtx, CredentialContext{Kind: CredentialKindAgentJWT})

	// Agent owns an endpoint so source resolution succeeds.
	agEP := f.createEndpoint(t, noWakeCtx, "ag-nowake-ep")

	// 1. Authoring reuse without agent.lifecycle in ceiling -> 400 "reuse requires wake authority".
	rec := f.doJSON(t, noWakeCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "reuse-no-wake",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: agEP.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: agNoWake.ID,
				Message: "hi",
			},
		})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "reuse requires wake authority")

	// 2. UAT without agent:lifecycle scope -> 400 "reuse requires wake authority" on create, PATCH, and resume.
	baseUser := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web")
	projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.sched.proj.ID}
	uatNoLifecycle := scheduleTestUAT(t, baseUser, projBoundary,
		"trigger:create", "trigger:update", "trigger:read", "webhook_endpoint:read", "agent:message")
	uatCtx := contextWithIdentity(ctx, uatNoLifecycle)
	uatCtx = contextWithCredentialContext(uatCtx, credentialContextForIdentity(uatNoLifecycle))

	recUAT := f.doJSON(t, uatCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "uat-reuse-no-wake",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: agNoWake.ID,
				Message: "hi",
			},
		})
	require.Equal(t, http.StatusBadRequest, recUAT.Code, recUAT.Body.String())
	assert.Contains(t, recUAT.Body.String(), "reuse requires wake authority")

	// 2b. UAT whose hop ceiling includes agent:lifecycle succeeds authoring a reuse trigger.
	uatWithLifecycle := scheduleTestUAT(t, baseUser, projBoundary,
		"trigger:create", "trigger:update", "trigger:read", "webhook_endpoint:read", "agent:message", "agent:lifecycle")
	uatWakeCtx := contextWithIdentity(ctx, uatWithLifecycle)
	uatWakeCtx = contextWithCredentialContext(uatWakeCtx, credentialContextForIdentity(uatWithLifecycle))
	recUATOK := f.doJSON(t, uatWakeCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "uat-reuse-with-wake",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: agNoWake.ID,
				Message: "hi",
			},
		})
	require.Equal(t, http.StatusCreated, recUATOK.Code, recUATOK.Body.String())

	// Create a valid trigger with mode=new via session user, then try to PATCH to reuse via UAT without lifecycle.
	recNew := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "patch-to-reuse",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeNew},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: agNoWake.ID,
				Message: "hi",
			},
		})
	require.Equal(t, http.StatusCreated, recNew.Code, recNew.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(recNew.Body.Bytes(), &created))

	reuseMode := store.TriggerSubjectModeReuse
	recPatch := f.doJSON(t, uatCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, created.ID),
		UpdateTriggerRequest{Subject: &TriggerSubjectPatchSpec{Mode: &reuseMode}})
	require.Equal(t, http.StatusBadRequest, recPatch.Code, recPatch.Body.String())
	assert.Contains(t, recPatch.Body.String(), "reuse requires wake authority")

	// 3. Session user (EffectCeilingPrincipal) succeeds creating reuse trigger;
	// pausing it and attempting to resume via UAT without agent:lifecycle fails with 400.
	recOK := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "reuse-with-wake",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: agNoWake.ID,
				Message: "hi",
			},
		})
	require.Equal(t, http.StatusCreated, recOK.Code, recOK.Body.String())
	var reuseTrig TriggerResponse
	require.NoError(t, json.Unmarshal(recOK.Body.Bytes(), &reuseTrig))

	recPause := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s/pause", f.sched.proj.ID, reuseTrig.ID), nil)
	require.Equal(t, http.StatusOK, recPause.Code, recPause.Body.String())

	recResume := f.doJSON(t, uatCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s/resume", f.sched.proj.ID, reuseTrig.ID), nil)
	require.Equal(t, http.StatusBadRequest, recResume.Code, recResume.Body.String())
	assert.Contains(t, recResume.Body.String(), "reuse requires wake authority")
}

func TestTriggerG4a_CoalesceWindow_APIRoundTripAndValidation(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-coalesce-api")
	owner := &store.User{
		ID:          uuid.NewString(),
		Email:       "coalesce-owner@example.com",
		DisplayName: "Coalesce Owner",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.CreateUser(ctx, owner))
	require.NoError(t, f.sched.srv.createProjectOwnerRoleBinding(ctx, f.sched.proj.ID, owner.ID))
	userCtx := f.userAuth(t, owner)
	ep := f.createEndpoint(t, userCtx, "coalesce-api-ep")

	target := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "target-coalesce",
		Name:      "target-coalesce",
		ProjectID: f.sched.proj.ID,
		Phase:     string(state.PhaseRunning),
		OwnerID:   f.sched.creator.ID,
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, target))

	// Invalid coalesce_window values -> 400.
	for _, bad := range []string{"11m", "-5s", "500ms", "not-a-duration"} {
		rec := f.doJSON(t, userCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:    "bad-coalesce-" + bad,
				Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Subject: &TriggerSubjectSpec{CoalesceWindow: bad},
				Action:  TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: target.ID, Message: "hi"},
			})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "coalesce_window=%q must return 400", bad)
	}

	// Valid create with 30s -> GET returns "30s".
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "good-coalesce",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip, CoalesceWindow: "30s"},
			Action:  TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: target.ID, Message: "hi"},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "30s", created.Subject.CoalesceWindow)

	// PATCH to 2m -> returns "2m0s".
	twoMin := "2m"
	recPatch := f.doJSON(t, userCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, created.ID),
		UpdateTriggerRequest{Subject: &TriggerSubjectPatchSpec{CoalesceWindow: &twoMin}})
	require.Equal(t, http.StatusOK, recPatch.Code, recPatch.Body.String())
	var patched TriggerResponse
	require.NoError(t, json.Unmarshal(recPatch.Body.Bytes(), &patched))
	assert.Equal(t, "2m0s", patched.Subject.CoalesceWindow)

	// PATCH to 0s -> disables coalescing (omitted in response).
	zero := "0s"
	recClear := f.doJSON(t, userCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, created.ID),
		UpdateTriggerRequest{Subject: &TriggerSubjectPatchSpec{CoalesceWindow: &zero}})
	require.Equal(t, http.StatusOK, recClear.Code, recClear.Body.String())
	var cleared TriggerResponse
	require.NoError(t, json.Unmarshal(recClear.Body.Bytes(), &cleared))
	assert.Empty(t, cleared.Subject.CoalesceWindow)
}

func TestTriggerG4a_StaleReplayAfterDedupWindow(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-stale-replay")
	setTriggerExperimentsForG1bTest(t, f.sched.srv, true, true)

	// Configure GitHub App webhook secret + slug (leaving APIBaseURL default so
	// githubAppWebHost() resolves "github.com" for https://github.com/octo/repo.git).
	const (
		appID         int64 = 4242
		instID        int64 = 8801
		webhookSecret       = "g4a-stale-replay-secret"
	)
	f.sched.srv.mu.Lock()
	f.sched.srv.config.GitHubAppConfig.AppID = appID
	f.sched.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
	f.sched.srv.config.GitHubAppConfig.WebhookSecret = webhookSecret
	f.sched.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.sched.srv.mu.Unlock()

	// Create active GitHub installation and associate the fixture project via hub admin.
	sysAdmin := createAssocAdmin(t, f.sched.store, "g4a-stale-sys-admin")
	createAssocInstallation(t, f.sched.store, instID, "octo", "octo/repo")
	instCopy := instID
	f.sched.proj.GitRemote = "https://github.com/octo/repo.git"
	f.sched.proj.GitHubInstallationID = &instCopy
	require.NoError(t, f.sched.store.UpdateProject(ctx, f.sched.proj))
	setAdminBinding(t, f.sched.store, f.sched.proj, instID, "github.com/octo/repo", sysAdmin.ID)
	f.sched.srv.invalidateGitHubAssociation(f.sched.proj.ID)

	// Ensure creator has hub-admin so github_trigger.comment authorizes cleanly.
	hubAdminRole, err := f.sched.store.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.sched.store.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: hubAdminRole.ID,
		PrincipalType:    "user",
		PrincipalID:      f.sched.creator.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	// Target agent for the message trigger.
	msgAgent := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "stale-msg-target",
		Name:            "stale-msg-target",
		ProjectID:       f.sched.proj.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseRunning),
		OwnerID:         f.sched.creator.ID,
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, msgAgent))

	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	userCtx := f.userAuth(t, f.sched.creator)

	// Author three GitHub triggers on issue_comment.created: dispatch_agent, github.comment, message.
	createGHTrigger := func(name string, action TriggerActionSpec) TriggerResponse {
		rec := f.doJSON(t, userCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   name,
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
				Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
				Action: action,
			})
		require.Equal(t, http.StatusCreated, rec.Code, "create %s: %s", name, rec.Body.String())
		var out TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	trDispatch := createGHTrigger("gh-stale-dispatch", TriggerActionSpec{
		Type: store.TriggerActionDispatchAgent,
		Task: "Handle comment",
	})
	trComment := createGHTrigger("gh-stale-comment", TriggerActionSpec{
		Type: store.TriggerActionGitHubComment,
		Body: "Ack comment",
	})
	trMessage := createGHTrigger("gh-stale-message", TriggerActionSpec{
		Type:    store.TriggerActionMessage,
		AgentID: msgAgent.ID,
		Message: "Received comment",
	})

	// Build a captured signed GitHub issue_comment.created delivery whose signed
	// comment.created_at is 8 days ago (simulating replay after the 7-day dedup window
	// has purged the original event row).
	eightDaysAgo := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	rawBody, err := json.Marshal(map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id": instID,
		},
		"repository": map[string]any{
			"id":             12345,
			"full_name":      "octo/repo",
			"name":           "repo",
			"private":        false,
			"default_branch": "main",
			"html_url":       "https://github.com/octo/repo",
			"owner":          map[string]any{"login": "octo", "id": 1, "type": "Organization"},
		},
		"sender": map[string]any{
			"login": "alice",
			"id":    99,
			"type":  "User",
		},
		"issue": map[string]any{
			"number":             5,
			"title":              "Test issue",
			"state":              "open",
			"html_url":           "https://github.com/octo/repo/issues/5",
			"author_association": "OWNER",
			"user":               map[string]any{"login": "alice", "id": 99, "type": "User"},
			"created_at":         eightDaysAgo.Format(time.RFC3339),
			"updated_at":         eightDaysAgo.Format(time.RFC3339),
		},
		"comment": map[string]any{
			"id":                 7001,
			"body":               "replayed old comment",
			"html_url":           "https://github.com/octo/repo/issues/5#issuecomment-7001",
			"author_association": "OWNER",
			"user":               map[string]any{"login": "alice", "id": 99, "type": "User"},
			"created_at":         eightDaysAgo.Format(time.RFC3339),
			"updated_at":         eightDaysAgo.Format(time.RFC3339),
		},
	})
	require.NoError(t, err)

	// Seed an old processed event with the same dedup key and purge it (>7 days old)
	// to prove the dedup row is gone before the replay arrives.
	origEvID := uuid.NewString()
	require.NoError(t, f.sched.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         origEvID,
		ProjectID:  f.sched.proj.ID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   fmt.Sprintf("%d", instID),
		Type:       "github.issue_comment.created",
		Subject:    "github:octo/repo#5",
		DedupKey:   "00000000-0000-0000-0000-000000000001/" + f.sched.proj.ID,
		OccurredAt: eightDaysAgo,
		ReceivedAt: eightDaysAgo,
		CreatedAt:  eightDaysAgo,
		Status:     store.TriggerEventStatusProcessed,
	}))
	purgedEv, _, err := f.sched.store.PurgeOldTriggerHistory(ctx, time.Now().UTC().Add(-trigger.HistoryRetention))
	require.NoError(t, err)
	assert.Equal(t, 1, purgedEv, "original dedup row must be purged after 7-day retention")

	// Replay the captured signed delivery now (8 days after occurred_at).
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	_, _ = mac.Write(rawBody)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "00000000-0000-0000-0000-000000000001")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", fmt.Sprintf("%d", appID))
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	f.sched.srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Drain the replayed event.
	f.drainOneTriggerEvent(t)

	// Verify: dispatch_agent refused "stale event", github.comment refused "stale event", message fired!
	assert.Empty(t, f.client.calls, "replayed stale delivery must NOT dispatch an agent")
	require.Len(t, tracker.messages(), 1, "message rule must still receive stale events")
	assert.Equal(t, msgAgent.Slug, tracker.messages()[0].AgentID)

	for _, trID := range []string{trDispatch.ID, trComment.ID} {
		fires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			ProjectID: f.sched.proj.ID,
			TriggerID: trID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, fires.Items, 1)
		assert.Equal(t, store.TriggerFireStatusRefused, fires.Items[0].Status)
		assert.Equal(t, trigger.ErrFireStaleEvent, fires.Items[0].Error)
	}

	msgFires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trMessage.ID,
	}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, msgFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, msgFires.Items[0].Status)
}

func TestTriggerG4a_ReuseWithCoalesceWindow(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-reuse-coalesce")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "reuse-coalesce-ep")

	// github.pr_updated shape: dispatch_agent + subject.mode=reuse + coalesce_window=30s.
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "pr-updated-shape",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.pr_sync"}},
			Subject: &TriggerSubjectSpec{
				Mode:           store.TriggerSubjectModeReuse,
				CoalesceWindow: "30s",
			},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				Task:          "Review ${event.id}",
				ReuseMessage:  "Updated ${event.subject} event=${event.id}",
				UntrustedVars: []string{"event.subject"},
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))

	var createCalls atomic.Int32
	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
		onBeforeCreate: func(_ *RemoteCreateAgentRequest) error {
			createCalls.Add(1)
			return nil
		},
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(f.sched.store, f.sched.store, clock)
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.sched.store,
		Core:          serverTriggerCore{srv: f.sched.srv},
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	dqStore := f.sched.store.(store.DurableQueueStore)
	claimNext := func(t *testing.T) durablequeue.Item {
		t.Helper()
		rows, cErr := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "worker-1", time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(2*time.Hour), 1)
		require.NoError(t, cErr)
		require.Len(t, rows, 1)
		_, _ = dqStore.CompleteDurableItem(ctx, rows[0].Seq, "worker-1")
		return durablequeue.Item{
			Seq: rows[0].Seq, Kind: rows[0].Kind, DedupKey: rows[0].DedupKey,
			ProjectID: rows[0].ProjectID, Payload: []byte(rows[0].Payload), Attempts: 1,
		}
	}

	// Burst 1: two synchronize events within 30s -> sha-1 coalesced, sha-2 dispatches 1 agent.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-rc-1", "pr_sync", "pr-99", []byte(`{"after":"sha-1"}`)).Code)
	it1 := claimNext(t)
	res1 := disp.HandleItem(ctx, it1, nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(30*time.Second), res1.RetryAt)

	now = now.Add(5 * time.Second)
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-rc-2", "pr_sync", "pr-99", []byte(`{"after":"sha-2"}`)).Code)
	it2 := claimNext(t)
	res2 := disp.HandleItem(ctx, it2, nil)
	assert.False(t, res2.Done)
	assert.Equal(t, now.Add(30*time.Second), res2.RetryAt)
	assert.Equal(t, int32(0), createCalls.Load())

	// Window elapses -> it2 promotes and creates 1 agent with sha-2.
	now = now.Add(30 * time.Second)
	res2Due := disp.HandleItem(ctx, it2, nil)
	assert.True(t, res2Due.Done)
	assert.Equal(t, int32(1), createCalls.Load())

	subjectKey := "webhook:" + ep.ID + "/pr-99"
	claim, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	require.NotEmpty(t, claim.AgentID)

	ag, err := f.sched.store.GetAgent(ctx, claim.AgentID)
	require.NoError(t, err)
	ag.Phase = string(state.PhaseRunning)
	ag.Activity = string(state.ActivityWaitingForInput)
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag))

	// Burst 2: two more synchronize events on the same subject within 30s ->
	// sha-3 coalesced, sha-4 promotes and delivers 1 reuse wake message (no second agent).
	now = now.Add(5 * time.Second)
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-rc-3", "pr_sync", "pr-99", []byte(`{"after":"sha-3"}`)).Code)
	it3 := claimNext(t)
	res3 := disp.HandleItem(ctx, it3, nil)
	assert.False(t, res3.Done)
	assert.Equal(t, now.Add(30*time.Second), res3.RetryAt)

	now = now.Add(5 * time.Second)
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-rc-4", "pr_sync", "pr-99", []byte(`{"after":"sha-4"}`)).Code)
	it4 := claimNext(t)
	res4 := disp.HandleItem(ctx, it4, nil)
	assert.False(t, res4.Done)
	assert.Equal(t, now.Add(30*time.Second), res4.RetryAt)
	assert.Empty(t, tracker.messages(), "reuse message must wait for coalesce window")

	now = now.Add(30 * time.Second)
	res4Due := disp.HandleItem(ctx, it4, nil)
	assert.True(t, res4Due.Done)
	assert.Equal(t, int32(1), createCalls.Load(), "reuse+coalesce must not create a second agent")

	msgs := tracker.messages()
	require.Len(t, msgs, 1, "only latest coalesced event delivers reuse message")
	assert.Equal(t, ag.Slug, msgs[0].AgentID)
	assert.Contains(t, msgs[0].Message, "event="+it4.DedupKey)
	assert.NotContains(t, msgs[0].Message, "event="+it3.DedupKey)

	// Confirm superseded rows (it1 and it3) are skipped "coalesced".
	for _, evID := range []string{it1.DedupKey, it3.DedupKey} {
		fRes, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			TriggerID: trigResp.ID,
			EventID:   evID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, fRes.Items, 1)
		assert.Equal(t, store.TriggerFireStatusSkipped, fRes.Items[0].Status)
		assert.Equal(t, trigger.ErrFireCoalesced, fRes.Items[0].Error)
	}
}

func TestTriggerG4a_Reuse_PatchedToMessageRunsOwnAction(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s3-g4a-reuse-patch-msg")
	owner := &store.User{
		ID:          uuid.NewString(),
		Email:       "reuse-patch-owner@example.com",
		DisplayName: "Reuse Patch Owner",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, f.sched.store.CreateUser(ctx, owner))
	require.NoError(t, f.sched.srv.createProjectOwnerRoleBinding(ctx, f.sched.proj.ID, owner.ID))
	userCtx := f.userAuth(t, owner)
	ep := f.createEndpoint(t, userCtx, "reuse-patch-msg-ep")

	var createCalls atomic.Int32
	tracker := &reuseTrackingBrokerClient{
		headlessGatherBrokerClient: f.client,
		storeForUpd:                f.sched.store,
		onBeforeCreate: func(_ *RemoteCreateAgentRequest) error {
			createCalls.Add(1)
			return nil
		},
	}
	f.sched.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.sched.store, tracker, false, slog.Default()))

	// Create a separate running agent that the PATCHed message action will target.
	targetAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "patch-msg-target",
		Name:            "patch-msg-target",
		ProjectID:       f.sched.proj.ID,
		RuntimeBrokerID: f.sched.broker.ID,
		Phase:           string(state.PhaseRunning),
		Activity:        string(state.ActivityWaitingForInput),
		OwnerID:         f.sched.creator.ID,
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, targetAg))

	// 1. Create a dispatch_agent + reuse trigger.
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "reuse-then-patch-msg",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.opened"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
			Action: TriggerActionSpec{
				Type:         store.TriggerActionDispatchAgent,
				Task:         "handle ${event.id}",
				ReuseMessage: "follow-up ${event.id}",
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))

	// 2. First event dispatches an agent and binds a live subject claim.
	recW1 := f.postSignedWebhook(t, ep, "del-patch-msg-1", "opened", "sub-42", []byte(`{"n":1}`))
	require.Equal(t, http.StatusAccepted, recW1.Code, recW1.Body.String())
	f.drainOneTriggerEvent(t)
	require.Equal(t, int32(1), createCalls.Load())

	subjectKey := "webhook:" + ep.ID + "/sub-42"
	claim, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, subjectKey)
	require.NoError(t, err)
	require.NotEmpty(t, claim.AgentID)

	// Mark the claimed agent suspended so any unintended wake would call StartAgent.
	oldAg, err := f.sched.store.GetAgent(ctx, claim.AgentID)
	require.NoError(t, err)
	oldAg.Phase = string(state.PhaseSuspended)
	oldAg.Activity = ""
	require.NoError(t, f.sched.store.UpdateAgent(ctx, oldAg))

	// 3. PATCH the trigger from dispatch_agent to message while keeping subject.mode=reuse.
	recPatch := f.doJSON(t, userCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, trigResp.ID),
		UpdateTriggerRequest{
			Action: &TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: targetAg.ID,
				Message: "own message action for ${event.id}",
			},
		})
	require.Equal(t, http.StatusOK, recPatch.Code, recPatch.Body.String())

	// 4. Next event on the same subject must run the message action against targetAg,
	// and must NOT wake or message oldAg.
	recW2 := f.postSignedWebhook(t, ep, "del-patch-msg-2", "opened", "sub-42", []byte(`{"n":2}`))
	require.Equal(t, http.StatusAccepted, recW2.Code, recW2.Body.String())
	f.drainOneTriggerEvent(t)

	assert.Empty(t, tracker.starts(), "old claimed agent must not be woken")
	msgs := tracker.messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, targetAg.Slug, msgs[0].AgentID)
	assert.Contains(t, msgs[0].Message, "own message action")
}
