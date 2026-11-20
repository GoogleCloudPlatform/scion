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

// Regression tests for ptone/scion#8517: answering (single, set/batch,
// override) or dismissing a decision prompt must auto-wake an auto-suspended
// asking agent (suspend_reason idle or stalled under lifecycle.idle_suspend)
// and deliver the decision-response once, without waking user-suspended,
// held, or confirmed-only targets.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type decisionWakeMsg struct {
	agentID    string
	message    string
	interrupt  bool
	structured *messages.StructuredMessage
}

type decisionWakeDispatcher struct {
	wakeTrackingDispatcher
	mu   sync.Mutex
	msgs []decisionWakeMsg
}

func (d *decisionWakeDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	if err := d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, sm); err != nil {
		return err
	}
	d.mu.Lock()
	d.msgs = append(d.msgs, decisionWakeMsg{
		agentID:    agent.ID,
		message:    message,
		interrupt:  interrupt,
		structured: sm,
	})
	d.mu.Unlock()
	return nil
}

func (d *decisionWakeDispatcher) delivered() []decisionWakeMsg {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]decisionWakeMsg, len(d.msgs))
	copy(out, d.msgs)
	return out
}

type decisionAutoWakeFixture struct {
	*scheduledSendFixture
	disp *decisionWakeDispatcher
}

func newDecisionAutoWakeFixture(t *testing.T, idleSuspendOn bool) *decisionAutoWakeFixture {
	t.Helper()
	f := newScheduledSendFixture(t)
	ctx := context.Background()

	setExperimentOverrides(t, f.srv, map[string]bool{
		experiments.Decisions:   true,
		experiments.IdleSuspend: idleSuspendOn,
	})

	disp := &decisionWakeDispatcher{}
	f.srv.SetDispatcher(disp)

	broker := &store.RuntimeBroker{
		ID:     tid("dec-wake-broker"),
		Name:   "Decision Wake Broker",
		Slug:   "dec-wake-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	f.agent.RuntimeBrokerID = broker.ID
	f.agent.Phase = string(state.PhaseRunning)
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	t.Cleanup(func() { f.srv.autoWakeWork.wg.Wait() })
	return &decisionAutoWakeFixture{scheduledSendFixture: f, disp: disp}
}

func (f *decisionAutoWakeFixture) suspendAgent(t *testing.T, reason state.SuspendReason) {
	t.Helper()
	require.NoError(t, f.store.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(reason),
	}))
	got, err := f.store.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	*f.agent = *got
}

func (f *decisionAutoWakeFixture) readyOnStart(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() { close(done); wg.Wait() })
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
			a, err := f.store.GetAgent(context.Background(), f.agent.ID)
			if err == nil && a.Phase == string(state.PhaseStarting) {
				_ = f.store.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
					Phase:    string(state.PhaseRunning),
					Activity: "idle",
				})
			}
		}
	}()
}

func (f *decisionAutoWakeFixture) seedPrompt(t *testing.T, rawDecisions string, createdAt time.Time) (*store.Message, *store.DecisionPromptResult) {
	t.Helper()
	var inputs []messages.DecisionInput
	require.NoError(t, json.Unmarshal([]byte(rawDecisions), &inputs))
	decs, err := messages.NormalizeDecisions("decisions", inputs)
	require.NoError(t, err)
	conv, err := f.store.GetConversationByExternalRef(context.Background(), "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		Sender:         "agent:" + f.agent.Slug,
		SenderID:       f.agent.ID,
		Recipient:      "user:" + f.alice.Email,
		RecipientID:    f.alice.ID,
		ConversationID: conv.ID,
		AgentID:        f.agent.ID,
		Channel:        "web",
		ThreadID:       f.topicID,
		CreatedAt:      createdAt,
		DispatchState:  store.MessageDispatchDispatched,
	}
	res, err := f.store.CreateDecisionPrompt(context.Background(), &store.DecisionPromptInput{
		Message:        msg,
		Intro:          "Please decide",
		AskerAgentID:   f.agent.ID,
		AskerAgentName: f.agent.Slug,
		Decisions:      decs,
	})
	require.NoError(t, err)
	return msg, res
}

func (f *decisionAutoWakeFixture) recordInterpreted(t *testing.T, sourceMsgID string, answers ...InterpretedAnswerInput) {
	t.Helper()
	body, err := json.Marshal(RecordInterpretedRequest{
		SourceMessageID: sourceMsgID,
		Answers:         answers,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/decisions/interpreted", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.handleAgentDecisionRoutes(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
}

func TestDecisionAnswer_AutoWakesHubSuspendedAgent(t *testing.T) {
	for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
		t.Run(string(reason), func(t *testing.T) {
			f := newDecisionAutoWakeFixture(t, true)
			f.readyOnStart(t)

			_, promptRes := f.seedPrompt(t, `[{"id":"W1","question":"Proceed?","choices":["yes","no"],"default":"yes"}]`, time.Now().UTC())
			f.suspendAgent(t, reason)

			rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+promptRes.Decisions[0].RowID+"/answer", map[string]any{
				"choice_id": "1",
			})
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
			var out DecisionAnswerResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
			require.NotEmpty(t, out.ResponseMessageID)

			f.srv.autoWakeWork.wg.Wait()

			require.Len(t, f.disp.getStartCalls(), 1, "expected one auto-wake start")
			assert.True(t, f.disp.getStartCalls()[0].Continue, "auto-wake must resume existing session")

			delivered := f.disp.delivered()
			require.Len(t, delivered, 1, "expected exactly one decision-response delivery")
			assert.Equal(t, f.agent.ID, delivered[0].agentID)
			require.NotNil(t, delivered[0].structured)
			assert.Equal(t, messages.TypeDecisionResponse, delivered[0].structured.Type)
			assert.Equal(t, "W1", delivered[0].structured.Metadata[messages.MetadataDecisionKey])
			assert.Equal(t, "1", delivered[0].structured.Metadata[messages.MetadataChoiceID])

			stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
			require.NoError(t, err)
			assert.Equal(t, store.MessageDispatchDispatched, stored.DispatchState)
			assert.Nil(t, stored.DispatchFailureReason)
		})
	}
}

func TestDecisionSetAnswer_AutoWakesHubSuspendedAgent(t *testing.T) {
	f := newDecisionAutoWakeFixture(t, true)
	f.readyOnStart(t)

	promptMsg, promptRes := f.seedPrompt(t, `[
		{"id":"D1","question":"First?","choices":["yes","no"],"default":"yes"},
		{"id":"D2","question":"Second?","choices":[{"id":"a","label":"Alpha"},{"id":"b","label":"Beta"}]}
	]`, time.Now().UTC())
	f.suspendAgent(t, state.SuspendReasonStalled)

	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+promptMsg.ID+"/answers", map[string]any{
		"answers": []map[string]any{
			{"decision_row_id": promptRes.Decisions[0].RowID, "choice_id": "1"},
			{"decision_row_id": promptRes.Decisions[1].RowID, "choice_id": "a"},
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out DecisionSetAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.NotEmpty(t, out.ResponseMessageID)

	f.srv.autoWakeWork.wg.Wait()

	require.Len(t, f.disp.getStartCalls(), 1, "expected one auto-wake start for set answer")
	delivered := f.disp.delivered()
	require.Len(t, delivered, 1, "expected exactly one set decision-response delivery")
	require.NotNil(t, delivered[0].structured)
	assert.Equal(t, messages.TypeDecisionResponse, delivered[0].structured.Type)
	assert.Equal(t, out.BatchID, delivered[0].structured.Metadata[messages.MetadataDecisionBatch])
	assert.Equal(t, "D1@1=1,D2@1=a", delivered[0].structured.Metadata[messages.MetadataDecisionAnswers])

	stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, stored.DispatchState)
}

func TestDecisionDismiss_AutoWakesHubSuspendedAgent(t *testing.T) {
	f := newDecisionAutoWakeFixture(t, true)
	f.readyOnStart(t)

	oldTime := time.Now().UTC().Add(-10 * time.Minute)
	f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(10 * time.Minute) }
	promptMsg, _ := f.seedPrompt(t, `[{"id":"DM1","question":"Still needed?","choices":["yes","no"]}]`, oldTime)
	f.suspendAgent(t, state.SuspendReasonIdle)

	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+promptMsg.ID+"/dismiss", map[string]any{
		"reason": "no longer needed",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out DismissDecisionPromptResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.True(t, out.Delivered)
	assert.Equal(t, DismissDeliveryDelivered, out.DeliveryStatus)

	f.srv.autoWakeWork.wg.Wait()

	require.Len(t, f.disp.getStartCalls(), 1, "expected one auto-wake start for dismiss")
	delivered := f.disp.delivered()
	require.Len(t, delivered, 1, "expected exactly one dismiss decision-response delivery")
	require.NotNil(t, delivered[0].structured)
	assert.Equal(t, messages.TypeDecisionResponse, delivered[0].structured.Type)
	assert.Equal(t, messages.ChoiceIDDismissed, delivered[0].structured.Metadata[messages.MetadataChoiceID])

	stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, stored.DispatchState)
}

func TestDecisionAnswer_DoesNotAutoWakeUserSuspendedHeldOrExperimentOff(t *testing.T) {
	t.Run("user_suspended", func(t *testing.T) {
		f := newDecisionAutoWakeFixture(t, true)
		f.readyOnStart(t)
		_, promptRes := f.seedPrompt(t, `[{"id":"U1","question":"Q?","choices":["yes","no"]}]`, time.Now().UTC())
		f.suspendAgent(t, state.SuspendReasonUser)

		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+promptRes.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		var out DecisionAnswerResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))

		f.srv.autoWakeWork.wg.Wait()
		assert.Empty(t, f.disp.getStartCalls(), "user-suspended agent must not auto-wake")
		assert.Empty(t, f.disp.delivered())

		stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, stored.DispatchState)
	})

	t.Run("experiment_off", func(t *testing.T) {
		f := newDecisionAutoWakeFixture(t, false)
		f.readyOnStart(t)
		_, promptRes := f.seedPrompt(t, `[{"id":"E1","question":"Q?","choices":["yes","no"]}]`, time.Now().UTC())
		f.suspendAgent(t, state.SuspendReasonIdle)

		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+promptRes.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		var out DecisionAnswerResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))

		f.srv.autoWakeWork.wg.Wait()
		assert.Empty(t, f.disp.getStartCalls(), "must not auto-wake when lifecycle.idle_suspend is off")
		assert.Empty(t, f.disp.delivered())

		stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, stored.DispatchState)
	})

	t.Run("held_agent", func(t *testing.T) {
		f := newDecisionAutoWakeFixture(t, true)
		f.readyOnStart(t)
		_, promptRes := f.seedPrompt(t, `[{"id":"H1","question":"Q?","choices":["yes","no"]}]`, time.Now().UTC())
		f.suspendAgent(t, state.SuspendReasonIdle)
		_, err := f.store.CreateAgentHolds(context.Background(), []*store.AgentHold{{
			AgentID:           f.agent.ID,
			ProjectID:         f.project.ID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   f.bob.ID,
			Trigger:           store.MembershipLossTriggerMemberRemove,
		}})
		require.NoError(t, err)

		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+promptRes.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		var out DecisionAnswerResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))

		f.srv.autoWakeWork.wg.Wait()
		assert.Empty(t, f.disp.getStartCalls(), "held agent must not auto-wake")
		assert.Empty(t, f.disp.delivered())

		stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, stored.DispatchState)
	})
}

func TestDecisionAnswer_InterpretedConfirmNoWakeOverrideWakes(t *testing.T) {
	f := newDecisionAutoWakeFixture(t, true)
	f.readyOnStart(t)
	ctx := context.Background()

	promptTime := time.Now().UTC().Add(-2 * time.Minute)
	_, prompt1 := f.seedPrompt(t, `[{"id":"IC1","question":"Confirm me?","choices":["yes","no"]}]`, promptTime)
	_, prompt2 := f.seedPrompt(t, `[{"id":"IO1","question":"Override me?","choices":["yes","no"]}]`, promptTime)

	conv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	srcMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		Sender:         "user:" + f.bob.Email,
		SenderID:       f.bob.ID,
		Recipient:      "agent:" + f.agent.Slug,
		RecipientID:    f.agent.ID,
		Msg:            "yes to both",
		Type:           "instruction",
		AgentID:        f.agent.ID,
		Channel:        "web",
		ThreadID:       f.topicID,
		ConversationID: conv.ID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      promptTime.Add(time.Minute),
	}
	require.NoError(t, f.store.CreateMessage(ctx, srcMsg))

	f.recordInterpreted(t, srcMsg.ID,
		InterpretedAnswerInput{Key: "IC1", ChoiceID: "1"},
		InterpretedAnswerInput{Key: "IO1", ChoiceID: "1"},
	)

	f.suspendAgent(t, state.SuspendReasonIdle)

	// Confirming an interpreted answer delivers nothing and must NOT wake.
	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+prompt1.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	f.srv.autoWakeWork.wg.Wait()
	assert.Empty(t, f.disp.getStartCalls(), "confirming an interpreted answer must not wake the asking agent")
	assert.Empty(t, f.disp.delivered())

	// Overriding an interpreted answer DOES deliver and MUST wake.
	rr = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+prompt2.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "2"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	f.srv.autoWakeWork.wg.Wait()
	require.Len(t, f.disp.getStartCalls(), 1, "overriding an interpreted answer must wake the asking agent")
	delivered := f.disp.delivered()
	require.Len(t, delivered, 1)
	assert.Contains(t, delivered[0].message, "overriding interpreted answer")
}

func TestDecisionAnswer_AutoWakeFailureMarksResponseFailed(t *testing.T) {
	f := newDecisionAutoWakeFixture(t, true)
	f.disp.startReturnErr = errors.New("broker unavailable")

	_, promptRes := f.seedPrompt(t, `[{"id":"F1","question":"Proceed?","choices":["yes","no"]}]`, time.Now().UTC())
	f.suspendAgent(t, state.SuspendReasonIdle)

	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+promptRes.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))

	f.srv.autoWakeWork.wg.Wait()

	assert.Empty(t, f.disp.delivered())
	stored, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, stored.DispatchState)
	require.NotNil(t, stored.DispatchFailureReason)
	assert.Contains(t, *stored.DispatchFailureReason, "auto-wake failed:")
}

func TestDecisionAnswer_CoalescesConcurrentAutoWakesNoDuplicates(t *testing.T) {
	f := newDecisionAutoWakeFixture(t, true)
	f.readyOnStart(t)

	_, p1 := f.seedPrompt(t, `[{"id":"C1","question":"First?","choices":["yes","no"]}]`, time.Now().UTC())
	_, p2 := f.seedPrompt(t, `[{"id":"C2","question":"Second?","choices":["yes","no"]}]`, time.Now().UTC())
	f.suspendAgent(t, state.SuspendReasonStalled)

	rr1 := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+p1.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr1.Code, "body: %s", rr1.Body.String())
	rr2 := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+p2.Decisions[0].RowID+"/answer", map[string]any{"choice_id": "2"})
	require.Equal(t, http.StatusOK, rr2.Code, "body: %s", rr2.Body.String())

	f.srv.autoWakeWork.wg.Wait()

	assert.Len(t, f.disp.getStartCalls(), 1, "concurrent answers to the same suspended agent must share one wake")
	delivered := f.disp.delivered()
	require.Len(t, delivered, 2, "each answer must be delivered exactly once")
	keys := []string{
		delivered[0].structured.Metadata[messages.MetadataDecisionKey],
		delivered[1].structured.Metadata[messages.MetadataDecisionKey],
	}
	assert.ElementsMatch(t, []string{"C1", "C2"}, keys)
}
