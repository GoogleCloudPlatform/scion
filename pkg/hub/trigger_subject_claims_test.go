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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type blockingGatherBrokerClient struct {
	*headlessGatherBrokerClient
	onBeforeCreate func(req *RemoteCreateAgentRequest) error
}

func (b *blockingGatherBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	if b.onBeforeCreate != nil {
		if err := b.onBeforeCreate(req); err != nil {
			return nil, nil, err
		}
	}
	return b.headlessGatherBrokerClient.CreateAgentWithGather(ctx, brokerID, endpoint, req)
}

func TestDispatchAgentTrigger_SubjectModeSkip_TwoReplicasConcurrency(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s2-p1c2-race")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "claims-race-ep")
	createReq := CreateTriggerRequest{
		Name:   "claims-race-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.pr_opened"}},
		Subject: &TriggerSubjectSpec{
			Mode: store.TriggerSubjectModeSkip,
		},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionDispatchAgent,
			Task:          "Handle PR ${event.subject}",
			AgentName:     "w-${event.id}",
			UntrustedVars: []string{"event.subject"},
		},
	}
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID), createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))
	assert.Equal(t, store.TriggerSubjectModeSkip, trigResp.Subject.Mode)

	// Hold the first create inside the fake broker until the second replica has
	// started handling its event so both replicas contend on the same subject.
	var createCalls atomic.Int32
	enterCreate := make(chan struct{}, 4)
	releaseCreate := make(chan struct{})
	blocker := &blockingGatherBrokerClient{
		headlessGatherBrokerClient: f.client,
		onBeforeCreate: func(_ *RemoteCreateAgentRequest) error {
			n := createCalls.Add(1)
			enterCreate <- struct{}{}
			if n == 1 {
				<-releaseCreate
			}
			return nil
		},
	}
	disp := NewHTTPAgentDispatcherWithClient(f.sched.store, blocker, false, slog.Default())
	f.sched.srv.SetDispatcher(disp)

	// Post two distinct deliveries with the SAME subject ("pr-42").
	rec1 := f.postSignedWebhook(t, ep, "del-race-1", "pr_opened", "pr-42", []byte(`{"n":1}`))
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
	rec2 := f.postSignedWebhook(t, ep, "del-race-2", "pr_opened", "pr-42", []byte(`{"n":2}`))
	require.Equal(t, http.StatusAccepted, rec2.Code, rec2.Body.String())

	// Build two independent trigger.Dispatcher replicas over the same hub store/server.
	dispA, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store: f.sched.store,
		Core:  serverTriggerCore{srv: f.sched.srv},
	})
	require.NoError(t, err)
	dispB, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store: f.sched.store,
		Core:  serverTriggerCore{srv: f.sched.srv},
	})
	require.NoError(t, err)

	dqStore, ok := f.sched.store.(store.DurableQueueStore)
	require.True(t, ok, "store must implement DurableQueueStore")

	// Claim both queue items separately (one per replica) and run them concurrently.
	now := time.Now().UTC()
	itemsA, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "replica-a", now, now.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, itemsA, 1)
	itemsB, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "replica-b", now, now.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, itemsB, 1)

	toDQItem := func(r store.DurableQueueRow) durablequeue.Item {
		return durablequeue.Item{
			Seq:       r.Seq,
			Kind:      r.Kind,
			DedupKey:  r.DedupKey,
			ProjectID: r.ProjectID,
			Payload:   []byte(r.Payload),
			Attempts:  r.Attempts,
		}
	}

	var wg sync.WaitGroup
	var resA, resB durablequeue.Result
	wg.Add(2)
	go func() {
		defer wg.Done()
		resA = dispA.HandleItem(ctx, toDQItem(itemsA[0]), nil)
	}()
	go func() {
		defer wg.Done()
		resB = dispB.HandleItem(ctx, toDQItem(itemsB[0]), nil)
	}()

	// Wait until one replica enters broker create, then allow a brief window for
	// the competing replica to hit Claim and skip, then unblock broker create.
	select {
	case <-enterCreate:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first replica to reach broker create")
	}
	time.Sleep(100 * time.Millisecond)
	close(releaseCreate)
	wg.Wait()

	assert.True(t, resA.Done, "replica A result: %+v", resA)
	assert.True(t, resB.Done, "replica B result: %+v", resB)
	completedA, err := dqStore.CompleteDurableItem(ctx, itemsA[0].Seq, "replica-a")
	require.NoError(t, err)
	require.True(t, completedA)
	completedB, err := dqStore.CompleteDurableItem(ctx, itemsB[0].Seq, "replica-b")
	require.NoError(t, err)
	require.True(t, completedB)

	assert.Equal(t, int32(1), createCalls.Load(), "exactly one agent must be dispatched across two replicas")

	fires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trigResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires.Items, 2)

	var firedCount, skippedCount int
	var createdAgentID string
	for _, fire := range fires.Items {
		switch fire.Status {
		case store.TriggerFireStatusFired:
			firedCount++
			createdAgentID = fire.ResultRef
			assert.Equal(t, fire.ResultRef, fire.TargetAgentID)
		case store.TriggerFireStatusSkipped:
			skippedCount++
			assert.Equal(t, "subject busy", fire.Error)
		default:
			t.Fatalf("unexpected fire status %q (err=%q)", fire.Status, fire.Error)
		}
	}
	assert.Equal(t, 1, firedCount)
	assert.Equal(t, 1, skippedCount)
	require.NotEmpty(t, createdAgentID)

	// Active claim is bound to the created agent.
	claim, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "pr-42")
	require.NoError(t, err)
	assert.Equal(t, store.TriggerSubjectClaimStateActive, claim.State)
	assert.Equal(t, createdAgentID, claim.AgentID)
	assert.NotNil(t, claim.BoundAt)

	// A different subject ("pr-43") on the same trigger dispatches independently.
	rec3 := f.postSignedWebhook(t, ep, "del-race-3", "pr_opened", "pr-43", []byte(`{"n":3}`))
	require.Equal(t, http.StatusAccepted, rec3.Code, rec3.Body.String())
	f.drainOneTriggerEvent(t)
	assert.Equal(t, int32(2), createCalls.Load(), "independent subject must dispatch normally")
}

func TestDispatchAgentTrigger_SubjectModeSkip_ReclaimOnTerminalAndAgentDeletion(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s2-p1c2-reclaim")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "claims-reclaim-ep")
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "claims-reclaim-trigger",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.pr_sync"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "Sync PR",
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))

	// 1. First event dispatches agent A1.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-1", "pr_sync", "pr-99", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)

	claim1, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "pr-99")
	require.NoError(t, err)
	agentA1ID := claim1.AgentID
	require.NotEmpty(t, agentA1ID)

	// 2. While A1 is running, a second event is skipped "subject busy".
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-2", "pr_sync", "pr-99", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)

	fires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trigResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires.Items, 2)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires.Items[0].Status)
	assert.Equal(t, "subject busy", fires.Items[0].Error)

	// 3. Move A1 to terminal phase (stopped); next event lazily releases via Live and dispatches A2.
	ag1, err := f.sched.store.GetAgent(ctx, agentA1ID)
	require.NoError(t, err)
	ag1.Phase = string(state.PhaseStopped)
	require.NoError(t, f.sched.store.UpdateAgent(ctx, ag1))

	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-3", "pr_sync", "pr-99", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)

	claim2, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "pr-99")
	require.NoError(t, err)
	agentA2ID := claim2.AgentID
	require.NotEmpty(t, agentA2ID)
	assert.NotEqual(t, agentA1ID, agentA2ID, "stopped agent's claim must be re-claimed for A2")

	// 4. Delete A2 through the hub agent deletion API (runAgentDeletion tail step releases claim).
	delRec := f.doJSON(t, userCtx, http.MethodDelete, fmt.Sprintf("/api/v1/agents/%s?force=true", agentA2ID), nil)
	require.Contains(t, []int{http.StatusNoContent, http.StatusOK, http.StatusAccepted}, delRec.Code, delRec.Body.String())

	// Claim is released by the deletion tail step without needing another event.
	require.Eventually(t, func() bool {
		_, gErr := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "pr-99")
		return errors.Is(gErr, store.ErrNotFound)
	}, 5*time.Second, 25*time.Millisecond, "agent deletion must release active subject claim")

	// 5. Next event immediately re-claims and dispatches A3.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-4", "pr_sync", "pr-99", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)

	claim3, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "pr-99")
	require.NoError(t, err)
	assert.NotEmpty(t, claim3.AgentID)
	assert.NotEqual(t, agentA2ID, claim3.AgentID)
}

func TestDispatchAgentTrigger_SubjectModeSkip_ReleaseOnCreateFailureAndRenderRefusal(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s2-p1c2-fail")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "claims-fail-ep")
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "claims-fail-trigger",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.build_run"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				Task:          "Run build ${event.payload.job}",
				UntrustedVars: []string{"event.payload.job"},
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))

	// 1. Render refusal after Claim releases the claim.
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-refuse", "build_run", "sub-fail", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)
	_, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "sub-fail")
	assert.ErrorIs(t, err, store.ErrNotFound, "claim must be released after template render refusal")

	// 2. Broker create failure releases the claim (even though Bind ran in commitAgentCreate tx).
	f.client.mu.Lock()
	f.client.returnErr = errors.New("broker exploded")
	f.client.mu.Unlock()

	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-fail", "build_run", "sub-fail", []byte(`{"job":"ci"}`)).Code)
	f.drainOneTriggerEvent(t)
	_, err = f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "sub-fail")
	assert.ErrorIs(t, err, store.ErrNotFound, "claim must be released after create/dispatch failure")

	// 3. Subsequent event succeeds once broker recovers.
	f.client.mu.Lock()
	f.client.returnErr = nil
	f.client.mu.Unlock()
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "del-ok", "build_run", "sub-fail", []byte(`{"job":"ci"}`)).Code)
	f.drainOneTriggerEvent(t)
	claim, err := f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigResp.ID, "sub-fail")
	require.NoError(t, err)
	assert.NotEmpty(t, claim.AgentID)
}

func TestDispatchAgentTrigger_SubjectModeNew_NoClaimRowsAndMessageSkipUnchanged(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "s2-p1c2-new")
	userCtx := f.userAuth(t, f.sched.creator)

	ep := f.createEndpoint(t, userCtx, "claims-new-ep")

	// 1. Mode "new" dispatches multiple agents for the same subject and never writes claim rows.
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "mode-new-trigger",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.ping"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeNew},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "Handle ping",
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var trigNew TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigNew))

	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "new-1", "ping", "same-sub", []byte(`{}`)).Code)
	require.Equal(t, http.StatusAccepted, f.postSignedWebhook(t, ep, "new-2", "ping", "same-sub", []byte(`{}`)).Code)
	f.drainOneTriggerEvent(t)

	fires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trigNew.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires.Items, 2)
	assert.Equal(t, store.TriggerFireStatusFired, fires.Items[0].Status)
	assert.Equal(t, store.TriggerFireStatusFired, fires.Items[1].Status)

	_, err = f.sched.store.GetActiveTriggerSubjectClaim(ctx, trigNew.ID, "same-sub")
	assert.ErrorIs(t, err, store.ErrNotFound, "mode new must never create claim rows")

	// 2. Message trigger with mode "skip" keeps Stage 1 authoring refusal, and
	// PATCHing a dispatch_agent(skip) trigger to action=message is also refused.
	targetAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "msg-target",
		Name:      "msg-target",
		ProjectID: f.sched.proj.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, targetAgent))

	recMsg := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "msg-skip-refused",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.ping"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: targetAgent.ID,
				Message: "hello",
			},
		})
	assert.Equal(t, http.StatusBadRequest, recMsg.Code, recMsg.Body.String())
	assert.Contains(t, recMsg.Body.String(), `subject_mode \"skip\" is not yet supported`)

	// Create a dispatch_agent trigger with skip, then try to PATCH action to message -> refused.
	recDisp := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:    "disp-skip-patch-test",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:   &TriggerMatchSpec{Types: []string{"webhook.other"}},
			Subject: &TriggerSubjectSpec{Mode: store.TriggerSubjectModeSkip},
			Action: TriggerActionSpec{
				Type: store.TriggerActionDispatchAgent,
				Task: "hi",
			},
		})
	require.Equal(t, http.StatusCreated, recDisp.Code, recDisp.Body.String())
	var trigDisp TriggerResponse
	require.NoError(t, json.Unmarshal(recDisp.Body.Bytes(), &trigDisp))

	adminUser := hubMemberUser(t, f.sched.store, api.NewUUID())
	grantProjectRole(t, f.sched.store, adminUser.ID, f.sched.proj.ID, store.ProjectRoleAdmin)
	adminCtx := f.userAuth(t, adminUser)

	patchRec := f.doJSON(t, adminCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s", f.sched.proj.ID, trigDisp.ID),
		map[string]any{
			"action": map[string]any{
				"type":    store.TriggerActionMessage,
				"agentId": targetAgent.ID,
				"message": "switched",
			},
		})
	assert.Equal(t, http.StatusBadRequest, patchRec.Code, patchRec.Body.String())
	assert.Contains(t, patchRec.Body.String(), `subject_mode \"skip\" is not yet supported`)
}
