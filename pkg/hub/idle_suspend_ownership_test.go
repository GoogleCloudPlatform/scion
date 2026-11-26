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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isoEnableIdleSweep(t *testing.T, srv *Server, after time.Duration) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.IdleSuspend {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	srv.mu.Lock()
	srv.config.AutoSuspendIdle = true
	srv.config.IdleSuspendAfter = after
	srv.mu.Unlock()
}

// isoSecondReplica constructs an independent Hub replica sharing s (with its
// own instanceID and empty in-process lifecycleOps / intentStops maps).
func isoSecondReplica(t *testing.T, s store.Store, disp AgentDispatcher) *Server {
	t.Helper()
	srvB, err := newTestHubServer(t, testServerConfig(), s)
	require.NoError(t, err)
	srvB.SetHubID("test-hub-id")
	waitUserScopedDataSweep(t, srvB)
	isoEnableIdleSweep(t, srvB, 15*time.Minute)
	if cb, ok := disp.(*isoCallbackDispatcher); ok && cb.st == nil {
		cb.st = s
	}
	if disp != nil {
		srvB.SetDispatcher(disp)
	}
	return srvB
}

func isoEnableSharedConduitRedrive(t *testing.T, srvA, srvB *Server) registry.Store {
	t.Helper()
	ctx := context.Background()
	regStore := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	peerSecret := make([]byte, 32)
	_, _ = rand.Read(peerSecret)

	raw, err := json.Marshal(map[string]any{
		"overrides": map[string]bool{
			conduitExperiment:          true,
			experiments.ConduitDeliver: true,
			experiments.IdleSuspend:    true,
		},
	})
	require.NoError(t, err)
	for i, srv := range []*Server{srvA, srvB} {
		if srv == nil {
			continue
		}
		instID := fmt.Sprintf("iso-relay-%d", i+1)
		srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
		fakeStore := newFakeHubSettingStore()
		fakeStore.seed("experiments", raw)
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
		_, err := ops.Refresh(ctx)
		require.NoError(t, err)
		srv.SetOperationalSettings(ops)

		internalSrv := httptest.NewServer(srv.ConduitInternalHandler())
		t.Cleanup(internalSrv.Close)

		auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: peerSecret, SelfID: instID})
		require.NoError(t, err)
		require.NoError(t, srv.StartConduitRelay(ctx, ConduitRelayOptions{
			InstanceID:       instID,
			InternalEndpoint: internalSrv.URL,
			HTTPClient:       internalSrv.Client(),
			PeerAuth:         auth,
			Store:            regStore,
			Registry:         registry.New(regStore, registry.Config{}),
			Clock:            clock.NewFake(time.Now()),
			sessionWriteWait: conduitFixtureWriteWait,
		}))
		srvRef := srv
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srvRef.shutdownConduitRelay(sctx)
		})
	}
	return regStore
}

func isoRegisterDeliverSession(t *testing.T, regStore registry.Store, relayInstanceID string, agent *store.Agent) {
	t.Helper()
	ctx := context.Background()
	relays, err := regStore.ListRelays(ctx)
	require.NoError(t, err)
	var relayGen int64
	for _, r := range relays {
		if r.InstanceID == relayInstanceID {
			relayGen = r.Generation
			break
		}
	}
	require.NotZero(t, relayGen, "relay %s must be registered", relayInstanceID)
	now := time.Now().UTC()
	_, err = regStore.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID:           api.NewUUID(),
		PrincipalKind:       registry.PrincipalAgent,
		PrincipalID:         agent.ID,
		ProjectID:           agent.ProjectID,
		RelayInstanceID:     relayInstanceID,
		RelayGeneration:     relayGen,
		Transport:           registry.TransportWS,
		EndpointIncarnation: agent.RunID,
		Capabilities: registry.Capabilities{
			RPC:                 []string{agentcontrol.RouteDeliverMessage},
			EndpointIncarnation: agent.RunID,
			IncarnationSource:   relay.IncarnationSourceLaunchID,
		},
		ConnectedAt: now,
		LastSeen:    now,
	})
	require.NoError(t, err)
}

func isoSetupIdleAgent(t *testing.T, s store.Store, name string, activity string) (*store.Project, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	proj, broker, agent := setupOnlineBrokerAgent(t, s, name)
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, store.BrokerStatusOnline))
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  proj.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		require.NoError(t, err)
	}
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	now := time.Now().UTC()
	pastGrace := now.Add(-20 * time.Minute)
	freshSeen := now.Add(-15 * time.Second)
	runID := "run-" + name
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET run_id = ?, runtime = 'docker', activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
		runID, activity, pastGrace, freshSeen, agent.ID,
	)
	require.NoError(t, err)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	loaded.AppliedConfig = &store.AgentAppliedConfig{RuntimeTarget: "docker"}
	require.NoError(t, s.UpdateAgent(ctx, loaded))
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
		activity, pastGrace, freshSeen, agent.ID,
	)
	require.NoError(t, err)
	loaded, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	return proj, broker, loaded
}

func isoClaimIdleDirectly(t *testing.T, s store.Store, agent *store.Agent, ttl time.Duration) (store.RunIntent, time.Time, time.Time) {
	t.Helper()
	ctx := context.Background()
	fresh, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, fresh.ActivitySince)
	prior, intentAt, claimUntil, err := s.ClaimIdleSuspend(ctx, fresh.ID, store.IdleSuspendCondition{
		RunID:            fresh.RunID,
		Activity:         fresh.Activity,
		ActivitySince:    *fresh.ActivitySince,
		IdleSuspendAfter: 15 * time.Minute,
		HeartbeatRecency: agentHeartbeatRecency,
		ClaimTTL:         ttl,
	})
	require.NoError(t, err)
	return prior, intentAt, claimUntil
}

func isoSeedAwaitingSessionMessage(t *testing.T, s store.Store, agent *store.Agent, msgText string) *store.Message {
	t.Helper()
	return isoSeedAwaitingSessionMessageFrom(t, s, "user:dev", DevUserID, agent, msgText)
}

func isoSeedAwaitingSessionMessageFrom(t *testing.T, s store.Store, senderLabel, senderID string, agent *store.Agent, msgText string) *store.Message {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	row := &store.Message{
		ID:            uuid.NewString(),
		AgentID:       agent.ID,
		ProjectID:     agent.ProjectID,
		Sender:        senderLabel,
		SenderID:      senderID,
		Recipient:     "agent:" + agent.Slug,
		RecipientID:   agent.ID,
		Msg:           msgText,
		Type:          messages.TypeInstruction,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     now,
	}
	require.NoError(t, s.CreateMessage(ctx, row))
	prep, err := s.PrepareOrHoldMessage(ctx, agent.ID, row.ID, 100)
	require.NoError(t, err)
	if !prep.Held {
		delivID := uuid.NewString()
		bindRes, berr := s.BindMessageDelivery(ctx, agent.ID, delivID, []string{row.ID}, now)
		require.NoError(t, berr)
		require.Equal(t, store.DeliveryBound, bindRes.Result)
		require.NoError(t, s.UnbindMessageDeliveryWithProof(ctx, delivID, []string{row.ID}, store.MessageNotReachedNoSession))
		heldOK, herr := s.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID:   row.ID,
			AgentID:     agent.ID,
			MaxProofAge: 2 * time.Minute,
			HoldTTL:     10 * time.Minute,
			MaxHeld:     100,
		})
		require.NoError(t, herr)
		require.True(t, heldOK)
	}
	got, err := s.GetMessage(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)
	return got
}

type isoCallbackDispatcher struct {
	runIntentDispatcher
	st                    store.Store
	autoReadyOnStart      bool
	finishConduitDelivery bool
	runSeq                atomic.Int64
	onStop                func(ctx context.Context, agent *store.Agent) error
	onStart               func(ctx context.Context, agent *store.Agent, task string, resume bool) error
	onRestart             func(ctx context.Context, agent *store.Agent) error
	onDelete              func(ctx context.Context, agent *store.Agent) error
	onMsg                 func(ctx context.Context, agent *store.Agent, msg string) error
	onExec                func(ctx context.Context, agent *store.Agent, command []string, timeout int) (string, int, error)
	msgs                  []string
	mu                    sync.Mutex
}

func (d *isoCallbackDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.stops.Add(1)
	if d.onStop != nil {
		return d.onStop(ctx, agent)
	}
	return nil
}

func (d *isoCallbackDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.starts.Add(1)
	beforeRunID := agent.RunID
	if d.onStart != nil {
		if err := d.onStart(ctx, agent, task, resume); err != nil {
			return err
		}
	} else {
		agent.Phase = string(state.PhaseRunning)
		agent.ContainerStatus = "running"
	}
	if d.st != nil && agent.RunID == beforeRunID {
		newRunID := fmt.Sprintf("run-started-%d", d.runSeq.Add(1))
		if _, err := d.st.SetAgentRunID(ctx, agent.ID, newRunID, nil); err == nil {
			agent.RunID = newRunID
		}
	}
	if d.autoReadyOnStart && d.st != nil {
		st := d.st
		agentID := agent.ID
		runID := agent.RunID
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				cur, err := st.GetAgent(context.Background(), agentID)
				if err != nil {
					return
				}
				if cur.Phase == string(state.PhaseStarting) && cur.Activity == "" {
					_ = st.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{
						Activity:     string(state.ActivityIdle),
						ReportSource: store.StatusReportSourceAgent,
						ReportRunID:  runID,
					})
					return
				}
				if cur.Phase == string(state.PhaseRunning) {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	return nil
}

func (d *isoCallbackDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
	beforeRunID := agent.RunID
	if d.onRestart != nil {
		if err := d.onRestart(ctx, agent); err != nil {
			return err
		}
	} else {
		agent.Phase = string(state.PhaseRunning)
		agent.ContainerStatus = "running"
	}
	if d.st != nil && agent.RunID == beforeRunID {
		newRunID := fmt.Sprintf("run-restarted-%d", d.runSeq.Add(1))
		if _, err := d.st.SetAgentRunID(ctx, agent.ID, newRunID, nil); err == nil {
			agent.RunID = newRunID
		}
	}
	return nil
}

func (d *isoCallbackDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	if d.onDelete != nil {
		return d.onDelete(ctx, agent)
	}
	return d.runIntentDispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch, softDelete, deletedAt)
}

func (d *isoCallbackDispatcher) DispatchAgentExec(ctx context.Context, agent *store.Agent, command []string, timeout int) (string, int, error) {
	if d.onExec != nil {
		return d.onExec(ctx, agent, command, timeout)
	}
	return d.runIntentDispatcher.DispatchAgentExec(ctx, agent, command, timeout)
}

func (d *isoCallbackDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	d.msgs = append(d.msgs, message)
	d.mu.Unlock()
	if d.onMsg != nil {
		if err := d.onMsg(ctx, agent, message); err != nil {
			return err
		}
	}
	if d.finishConduitDelivery && d.st != nil && structuredMsg != nil && structuredMsg.StoredMessageID != "" {
		delivID := api.NewUUID()
		if _, err := d.st.BindMessageDelivery(ctx, agent.ID, delivID, []string{structuredMsg.StoredMessageID}, time.Now().UTC().Add(time.Minute)); err == nil {
			_, _ = d.st.FinishMessageDelivery(ctx, delivID, []string{structuredMsg.StoredMessageID}, store.MessageDispatchDelivered, "")
		}
	}
	return nil
}

func (d *isoCallbackDispatcher) deliveredMessages() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.msgs))
	copy(out, d.msgs)
	return out
}

// TestIdleSuspend_MultiReplicaPostStopHeartbeatExit137CommitsSuspendedIdle pins
// Writer Audit Rows 2 & 10: a real HTTP broker heartbeat landing on Replica B
// (where Replica A's in-process lifecycleOps is absent) reports Exited (137) /
// phase=error while Replica A's idle stop is in flight; Replica B preserves
// idle_suspend_claim_until and Replica A commits phase=suspended,
// suspend_reason=idle with retained activity.
func TestIdleSuspend_MultiReplicaPostStopHeartbeatExit137CommitsSuspendedIdle(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	dispB := &isoCallbackDispatcher{}
	srvB := isoSecondReplica(t, s, dispB)

	proj, broker, agent := isoSetupIdleAgent(t, s, "mr-exit137", string(state.ActivityIdle))
	heldMsg := isoSeedAwaitingSessionMessage(t, s, agent, "held-across-exit137")

	var midHeartbeat *store.Agent
	dispA := &isoCallbackDispatcher{
		onStop: func(_ context.Context, a *store.Agent) error {
			exitCode := 137
			hb := brokerHeartbeatRequest{
				Status:    store.BrokerStatusOnline,
				Inventory: completeInventory("docker"),
				Projects: []brokerProjectHeartbeat{{
					ProjectID: proj.ID,
					Agents: []brokerAgentHeartbeat{{
						Slug:            a.Slug,
						Phase:           string(state.PhaseError),
						ContainerStatus: "Exited (137)",
						ExitCode:        &exitCode,
						RuntimeTarget:   "docker",
					}},
				}},
			}
			rec := doRequest(t, srvB, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var err error
			midHeartbeat, err = s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			return nil
		},
	}
	srvA.SetDispatcher(dispA)

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	require.NotNil(t, midHeartbeat)
	assert.Equal(t, string(state.PhaseError), midHeartbeat.Phase, "Replica B heartbeat moved phase to error")
	assert.True(t, midHeartbeat.IdleSuspendClaimActive, "Replica B heartbeat must preserve active idle claim")
	assert.Zero(t, dispB.stops.Load(), "Replica B must not dispatch a duplicate stop")

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), final.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
	assert.Equal(t, string(state.ActivityIdle), final.Activity)
	assert.Nil(t, final.IdleSuspendClaimUntil)
	assert.True(t, srvA.shouldAutoWake(final))

	gotMsg, err := s.GetMessage(ctx, heldMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchAwaitingSession, gotMsg.DispatchState,
		"held message must survive Exited 137 heartbeat and idle-suspend commit")
}

// TestIdleSuspend_OwnershipMatrix_ClaimHolderAndRollback pins Writer Audit Row 3:
// when DispatchAgentStop fails before stopping the container, Replica A reverts
// run_intent back to running (CAS on its own intentAt) and clears its claim,
// without reverting a concurrent user stop's run_intent.
func TestIdleSuspend_OwnershipMatrix_ClaimHolderAndRollback(t *testing.T) {
	t.Run("stop dispatch failure rolls back run_intent and clears claim", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		_, _, agent := isoSetupIdleAgent(t, s, "rollback-plain", string(state.ActivityIdle))

		srvA.SetDispatcher(&isoCallbackDispatcher{
			onStop: func(_ context.Context, _ *store.Agent) error {
				return fmt.Errorf("broker unreachable: %w", errStartBrokerNotConnected)
			},
		})
		srvA.agentIdleSuspendHandler()(ctx)
		srvA.waitAutoSuspendIdle()

		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent)
		assert.Nil(t, got.IdleSuspendClaimUntil)
	})

	t.Run("stop dispatch failure never reverts a newer user stop intent", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{})
		_, _, agent := isoSetupIdleAgent(t, s, "rollback-user-stop", string(state.ActivityIdle))
		u := wakeLifecycleUser(t, s, "iso-rb-user", agent.ProjectID, "agent.lifecycle", "agent.read")

		srvA.SetDispatcher(&isoCallbackDispatcher{
			onStop: func(_ context.Context, a *store.Agent) error {
				rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return fmt.Errorf("idle stop failed after user stop: %w", errStartBrokerNotConnected)
			},
		})
		srvA.agentIdleSuspendHandler()(ctx)
		srvA.waitAutoSuspendIdle()

		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase)
		assert.Equal(t, store.RunIntentStopped, got.RunIntent, "RevertRunIntent CAS must preserve newer user stopped intent")
		assert.Nil(t, got.IdleSuspendClaimUntil)
	})

	t.Run("failed user stop leaving run_intent=stopped is never turned into suspended(idle)", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		_, _, agent := isoSetupIdleAgent(t, s, "failed-user-stop-intent", string(state.ActivityIdle))
		u := wakeLifecycleUser(t, s, "iso-failed-stop-user", agent.ProjectID, "agent.lifecycle", "agent.read")

		disp := &isoCallbackDispatcher{
			onStop: func(_ context.Context, _ *store.Agent) error {
				return fmt.Errorf("broker stop failed during user stop: %w", errStartBrokerNotConnected)
			},
		}
		srvA.SetDispatcher(disp)

		// 1. User issues stop, whose broker stop fails: phase stays running, run_intent=stopped.
		rec := requestAsIdentity(t, srvA, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
		require.NotEqual(t, http.StatusOK, rec.Code)
		assert.Equal(t, int32(1), disp.stops.Load())

		afterStopFail, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.Equal(t, string(state.PhaseRunning), afterStopFail.Phase)
		require.Equal(t, store.RunIntentStopped, afterStopFail.RunIntent)

		// 2. Now broker recovers; neither the idle sweep nor direct ClaimIdleSuspend
		// may claim the stopped-intent row or convert it into auto-wakeable suspended(idle).
		disp.onStop = nil
		srvA.agentIdleSuspendHandler()(ctx)
		srvA.waitAutoSuspendIdle()
		assert.Equal(t, int32(1), disp.stops.Load(), "idle sweep must skip running row with run_intent=stopped")

		_, _, _, err = s.ClaimIdleSuspend(ctx, afterStopFail.ID, store.IdleSuspendCondition{
			RunID:            afterStopFail.RunID,
			Activity:         afterStopFail.Activity,
			ActivitySince:    *afterStopFail.ActivitySince,
			IdleSuspendAfter: 15 * time.Minute,
			HeartbeatRecency: agentHeartbeatRecency,
			ClaimTTL:         45 * time.Second,
		})
		require.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

		final, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), final.Phase)
		assert.Empty(t, final.SuspendReason)
		assert.Equal(t, store.RunIntentStopped, final.RunIntent)
	})
}

// TestIdleSuspend_CrossReplicaIntentStopBackstopPreservesClaim pins Writer Audit
// Row 4: when Replica B receives an online broker heartbeat listing the agent as
// running while Replica A holds an active idle claim (run_intent=stopped),
// Replica B's intent-stop backstop (both stopAgentsRunningWithIntentStopped and
// stopForStoppedIntent / ClaimAgentStop) yields without clearing
// idle_suspend_claim_until or dispatching a second stop, and Replica A commits
// phase=suspended, suspend_reason=idle. Once the 45s lease expires (crashed
// holder), Replica B's backstop takes over and stops the container.
func TestIdleSuspend_CrossReplicaIntentStopBackstopPreservesClaim(t *testing.T) {
	t.Run("active idle claim on Replica A makes Replica B backstop yield and commits suspended(idle)", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		dispB := &isoCallbackDispatcher{}
		srvB := isoSecondReplica(t, s, dispB)

		proj, broker, agent := isoSetupIdleAgent(t, s, "backstop-yield", string(state.ActivityIdle))

		stopEntered := make(chan struct{})
		releaseStop := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(releaseStop) }) })

		dispA := &isoCallbackDispatcher{
			onStop: func(_ context.Context, _ *store.Agent) error {
				close(stopEntered)
				<-releaseStop
				return nil
			},
		}
		srvA.SetDispatcher(dispA)

		done := make(chan struct{})
		go func() {
			defer close(done)
			srvA.agentIdleSuspendHandler()(ctx)
			srvA.waitAutoSuspendIdle()
		}()

		<-stopEntered
		mid, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.True(t, mid.IdleSuspendClaimActive)
		require.False(t, srvB.lifecycleOps.active(agent.ID), "Replica B has no in-process lifecycleOp for Replica A's stop")

		// Send a complete online heartbeat to Replica B reporting the container still running.
		hb := brokerHeartbeatRequest{
			Status:    store.BrokerStatusOnline,
			Inventory: completeInventory("docker"),
			Projects: []brokerProjectHeartbeat{{
				ProjectID: proj.ID,
				Agents: []brokerAgentHeartbeat{{
					Slug:            agent.Slug,
					Phase:           string(state.PhaseRunning),
					Activity:        string(state.ActivityIdle),
					ContainerStatus: "Up 20 minutes",
					RuntimeTarget:   "docker",
				}},
			}},
		}
		rec := doRequest(t, srvB, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Also exercise stopForStoppedIntent directly with a stale pre-claim snapshot
		// (where IdleSuspendClaimActive was false) to prove the row-locked ClaimAgentStop guard!
		staleCopy := *mid
		staleCopy.IdleSuspendClaimActive = false
		srvB.stopForStoppedIntent(ctx, staleCopy)

		afterHB, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.True(t, afterHB.IdleSuspendClaimActive, "Replica B backstop must NOT clear active idle_suspend_claim_until")
		assert.Equal(t, mid.IdleSuspendClaimUntil, afterHB.IdleSuspendClaimUntil)
		assert.Empty(t, afterHB.StartClaimID, "Replica B must not acquire StartClaimStop while idle claim is active")
		assert.Zero(t, dispB.stops.Load(), "Replica B must not dispatch a backstop stop during active idle claim")

		releaseOnce.Do(func() { close(releaseStop) })
		<-done

		final, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), final.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
		assert.Equal(t, string(state.ActivityIdle), final.Activity)
		assert.Nil(t, final.IdleSuspendClaimUntil)
	})

	t.Run("expired idle claim from crashed holder allows Replica B backstop to take over", func(t *testing.T) {
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		stopCalled := make(chan struct{}, 1)
		dispB := &isoCallbackDispatcher{
			onStop: func(_ context.Context, _ *store.Agent) error {
				select {
				case stopCalled <- struct{}{}:
				default:
				}
				return nil
			},
		}
		srvB := isoSecondReplica(t, s, dispB)

		proj, broker, agent := isoSetupIdleAgent(t, s, "backstop-expired", string(state.ActivityIdle))
		_, _, _ = isoClaimIdleDirectly(t, s, agent, 15*time.Millisecond)
		time.Sleep(35 * time.Millisecond)

		hb := brokerHeartbeatRequest{
			Status:    store.BrokerStatusOnline,
			Inventory: completeInventory("docker"),
			Projects: []brokerProjectHeartbeat{{
				ProjectID: proj.ID,
				Agents: []brokerAgentHeartbeat{{
					Slug:            agent.Slug,
					Phase:           string(state.PhaseRunning),
					Activity:        string(state.ActivityIdle),
					ContainerStatus: "Up 25 minutes",
					RuntimeTarget:   "docker",
				}},
			}},
		}
		rec := doRequest(t, srvB, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		select {
		case <-stopCalled:
		case <-time.After(3 * time.Second):
			t.Fatal("Replica B backstop did not stop container after idle claim lease expired")
		}
		assert.Equal(t, int32(1), dispB.stops.Load())
	})
}

// TestIdleSuspend_OwnershipMatrix_ReconcilersAndDrain pins Writer Audit Rows 5–9:
// queued offline stop drain (execDispatchStop), settleQueuedStops,
// compensatingStop, replaceQueuedStop, SetAgentRunID, and finalizeInDoubtDelete
// all preserve/yield to an active idle claim.
func TestIdleSuspend_OwnershipMatrix_ReconcilersAndDrain(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	dispB := &isoCallbackDispatcher{}
	srvB := isoSecondReplica(t, s, dispB)

	proj, broker, agent := isoSetupIdleAgent(t, s, "reconcilers", string(state.ActivityIdle))
	_, intentAt, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	// Row 5: execDispatchStop pinned to intentAt yields (claimQueuedStop -> ClaimAgentStop -> ErrClaimPredicate).
	stopArgsBytes, err := json.Marshal(StopDispatchArgs{IntentAt: &intentAt, RunID: agent.RunID})
	require.NoError(t, err)
	res, err := srvB.execDispatchStop(ctx, store.BrokerDispatch{
		ID:       tid("iso-qstop"),
		BrokerID: broker.ID,
		AgentID:  agent.ID,
		Op:       "stop",
		Args:     string(stopArgsBytes),
	})
	require.NoError(t, err)
	assert.Equal(t, stopSupersededResult, res)
	assert.Zero(t, dispB.stops.Load())

	// Row 6: settleQueuedStops via heartbeat when container_status=stop_queued preserves active claim.
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET container_status = ? WHERE id = ?", containerStatusStopQueued, agent.ID)
	require.NoError(t, err)
	rec := doRequest(t, srvB, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory("docker"),
		Projects:  []brokerProjectHeartbeat{{ProjectID: proj.ID}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Row 7: compensatingStop yields when ClaimAgentStop sees active idle claim.
	srvB.compensatingStop(ctx, agent, store.StartClaim{
		ID:   tid("iso-old-start-claim"),
		Kind: store.StartClaimUser,
		At:   intentAt.Add(-time.Second),
	})
	assert.Zero(t, dispB.stops.Load())

	// Row 8: replaceQueuedStop skips because cur.RunIntent == stopped.
	srvB.replaceQueuedStop(ctx, agent, "running")

	// Row 9: finalizeInDoubtDelete does nothing on a live idle-claimed row without matching DeletionClaim.
	srvB.finalizeInDoubtDelete(ctx, agent.ID, &DeleteDispatchArgs{Claim: 99})

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, after.IdleSuspendClaimActive)
	assert.Equal(t, claimUntil, *after.IdleSuspendClaimUntil)
	assert.Equal(t, store.RunIntentStopped, after.RunIntent)

	recorded, err := srvA.recordStopStatus(ctx, agent.ID, agent.RunID, "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	assert.True(t, recorded)
}

// TestIdleSuspend_PostStopContainerStatusDoesNotAbortCommit pins Writer Audit
// Row 11: sciontool POST /api/v1/agents/:id/status reports on Replica B
// (stopping, stopped with exit code, error, and stale starting) never clear an
// active idle claim and Replica A commits phase=suspended, suspend_reason=idle.
func TestIdleSuspend_PostStopContainerStatusDoesNotAbortCommit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		phase        string
		activity     string
		exitCode     *int
		wantActivity string
	}{
		{name: "stopping_thinking_clears_activity", phase: string(state.PhaseStopping), activity: string(state.ActivityThinking), wantActivity: ""},
		{name: "stopping_empty_retains_completed", phase: string(state.PhaseStopping), wantActivity: string(state.ActivityCompleted)},
		{name: "stopped_exit_143", phase: string(state.PhaseStopped), activity: string(state.ActivityCrashed), exitCode: func() *int { v := 143; return &v }(), wantActivity: string(state.ActivityCompleted)},
		{name: "error_exit_1", phase: string(state.PhaseError), activity: string(state.ActivityCrashed), exitCode: func() *int { v := 1; return &v }(), wantActivity: string(state.ActivityCompleted)},
		{name: "stale_starting", phase: string(state.PhaseStarting), wantActivity: string(state.ActivityCompleted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srvA, s := testServer(t)
			isoEnableIdleSweep(t, srvA, 15*time.Minute)
			srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{})

			_, _, agent := isoSetupIdleAgent(t, s, "status-"+tc.name, string(state.ActivityCompleted))
			tok, cred, err := srvA.agentTokenService.SignAgentToken(AgentTokenGrant{
				AgentID:   agent.ID,
				ProjectID: agent.ProjectID,
				Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
			}, agent.RunID)
			require.NoError(t, err)
			require.NoError(t, s.CreateAgentCredential(ctx, cred))

			srvA.SetDispatcher(&isoCallbackDispatcher{
				onStop: func(_ context.Context, a *store.Agent) error {
					body := map[string]any{"phase": tc.phase}
					if tc.activity != "" {
						body["activity"] = tc.activity
					}
					if tc.exitCode != nil {
						body["exitCode"] = *tc.exitCode
					}
					rec := doRequestWithAgentToken(t, srvB, http.MethodPost, "/api/v1/agents/"+a.ID+"/status", body, tok)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					mid, gerr := s.GetAgent(ctx, a.ID)
					require.NoError(t, gerr)
					assert.True(t, mid.IdleSuspendClaimActive, "agent status report %s must preserve idle claim", tc.name)
					return nil
				},
			})

			srvA.agentIdleSuspendHandler()(ctx)
			srvA.waitAutoSuspendIdle()

			final, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), final.Phase)
			assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
			assert.Equal(t, tc.wantActivity, final.Activity)
		})
	}
}

// TestIdleSuspend_OwnershipMatrix_SystemStopsAndSweeps pins Writer Audit Rows 12,
// 14, and 15: limits_exceeded stopCurrentRun, MarkStaleAgentsOffline,
// MarkStalledAgents, and stalled autoSuspendAgent on Replica B all preserve or
// yield to Replica A's active idle claim.
func TestIdleSuspend_OwnershipMatrix_SystemStopsAndSweeps(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	dispB := &isoCallbackDispatcher{}
	srvB := isoSecondReplica(t, s, dispB)

	_, _, agent := isoSetupIdleAgent(t, s, "sys-sweeps", string(state.ActivityIdle))
	_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	// Row 12: limits_exceeded stopCurrentRun yields (systemStopBlocked + IfNoActiveIdleClaim).
	assert.False(t, srvB.stopCurrentRun(ctx, agent.ID, agent.RunID, "limits_exceeded stop"))
	err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:               string(state.PhaseStopped),
		ContainerStatus:     "stopped",
		IfPhase:             string(state.PhaseRunning),
		IfRunID:             agent.RunID,
		IfNoActiveIdleClaim: true,
	})
	assert.ErrorIs(t, err, store.ErrClaimPredicate)

	// Row 14: MarkStaleAgentsOffline and MarkStalledAgents preserve active idle claim.
	future := time.Now().Add(time.Hour)
	_, err = s.MarkStaleAgentsOffline(ctx, future)
	require.NoError(t, err)
	_, err = s.MarkStalledAgents(ctx, store.StallCutoffs{Activity: future, Thinking: future}, time.Time{})
	require.NoError(t, err)

	// Row 15: stalled autoSuspendAgent (even with a stale pre-claim snapshot where
	// IdleSuspendClaimActive=false and Activity=stalled) yields via SwapSystemRunIntent.
	staleStalled := *agent
	staleStalled.IdleSuspendClaimActive = false
	staleStalled.Activity = string(state.ActivityStalled)
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET activity = ? WHERE id = ?", string(state.ActivityStalled), agent.ID)
	require.NoError(t, err)
	assert.False(t, srvB.autoSuspendAgent(ctx, dispB, &staleStalled, state.SuspendReasonStalled))
	assert.Zero(t, dispB.stops.Load())

	mid, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, mid.IdleSuspendClaimActive)
	assert.Equal(t, claimUntil, *mid.IdleSuspendClaimUntil)

	recorded, err := srvA.recordStopStatus(ctx, agent.ID, agent.RunID, "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	assert.True(t, recorded)
	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), final.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
}

// TestIdleSuspend_OwnershipMatrix_HeartbeatsAndReports pins Writer Audit Row 13:
// MarkAgentContainerMissing and MarkAgentContainerMissingIfUnchanged on Replica B
// move phase to error without clearing idle_suspend_claim_until, and Replica A
// still commits phase=suspended, suspend_reason=idle.
func TestIdleSuspend_OwnershipMatrix_HeartbeatsAndReports(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)

	_, broker, agent := isoSetupIdleAgent(t, s, "missing-cont", string(state.ActivityIdle))
	_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	marked, err := s.MarkAgentContainerMissing(ctx, agent.ID, broker.ID, time.Now().Add(time.Hour), "", missingAgentMessage)
	require.NoError(t, err)
	require.NotNil(t, marked)
	assert.Equal(t, string(state.PhaseError), marked.Phase)

	afterMissing, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, afterMissing.IdleSuspendClaimActive, "MarkAgentContainerMissing must preserve idle claim")

	recorded, err := srvA.recordStopStatus(ctx, agent.ID, agent.RunID, "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	assert.True(t, recorded)

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), final.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
}

// TestIdleSuspend_OwnershipMatrix_MembershipHoldYieldsWithoutStranding pins
// Writer Audit Row 16: when a membership-loss hold lands on Replica B while
// Replica A holds an active idle claim (even after Exited 137 moved phase to
// error so heldAgentNeedsStop is false), Replica B durably sets agent_holds,
// revokes credentials, and yields without clearing the idle claim; Replica A's
// IfIdleSuspendCommit finishes moving the row to phase=suspended with
// suspend_reason="" (so the held agent is neither stranded in error nor
// auto-wakable). If Replica A's stop fails and rolls back, retryHeldAgentStops
// stops and suspends the held agent cleanly.
func TestIdleSuspend_OwnershipMatrix_MembershipHoldYieldsWithoutStranding(t *testing.T) {
	t.Run("membership loss during active idle claim + Exited 137 settles in suspended with empty suspend_reason", func(t *testing.T) {
		ctx := context.Background()
		f := newMSFixture(t, "idlehold")
		isoEnableIdleSweep(t, f.srv, 15*time.Minute)
		dispB := &isoCallbackDispatcher{}
		srvB := isoSecondReplica(t, f.s, dispB)
		srvB.membershipService.onMembershipLoss = nil

		broker := newTestBroker(t, f.s, "ms-idle-broker")
		require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: f.projectID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))
		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		_, err := f.s.SetRunIntent(ctx, f.agentA.ID, store.RunIntentRunning)
		require.NoError(t, err)
		_, err = f.s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET runtime_broker_id = ?, run_id = 'run-ms-idle', phase = 'running', activity = 'idle', activity_since = ?, last_seen = ? WHERE id = ?",
			broker.ID, pastGrace, now.Add(-10*time.Second), f.agentA.ID,
		)
		require.NoError(t, err)

		dispA := &isoCallbackDispatcher{
			onStop: func(_ context.Context, a *store.Agent) error {
				code := 137
				require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
					Phase:              string(state.PhaseError),
					ContainerStatus:    "Exited (137)",
					ExitCode:           &code,
					ExitReason:         string(state.ExitReasonCrashed),
					Heartbeat:          true,
					ReportSource:       store.StatusReportSourceRuntime,
					ReportRunID:        a.RunID,
					GuardReincarnation: true,
				}))
				f.dropBindings(f.userID)
				require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
				srvB.drainMembershipLossChecks(ctx)
				return nil
			},
		}
		f.srv.SetDispatcher(dispA)

		f.srv.agentIdleSuspendHandler()(ctx)
		f.srv.waitAutoSuspendIdle()

		assert.True(t, f.held(f.agentA.ID), "agent_holds must be set")
		final, err := f.s.GetAgent(ctx, f.agentA.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), final.Phase, "agent must not be stranded in error")
		assert.Empty(t, final.SuspendReason, "held agent must not carry suspend_reason=idle")
		assert.False(t, f.srv.shouldAutoWake(final), "held agent must never auto-wake")
	})

	t.Run("if idle stop fails and rolls back, retryHeldAgentStops stops and suspends the held agent", func(t *testing.T) {
		ctx := context.Background()
		f := newMSFixture(t, "idlerollback")
		isoEnableIdleSweep(t, f.srv, 15*time.Minute)
		dispB := &isoCallbackDispatcher{}
		srvB := isoSecondReplica(t, f.s, dispB)
		srvB.membershipService.onMembershipLoss = nil

		broker := newTestBroker(t, f.s, "ms-rb-broker")
		require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: f.projectID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))
		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		_, err := f.s.SetRunIntent(ctx, f.agentA.ID, store.RunIntentRunning)
		require.NoError(t, err)
		_, err = f.s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET runtime_broker_id = ?, run_id = 'run-ms-rb', phase = 'running', activity = 'idle', activity_since = ?, last_seen = ? WHERE id = ?",
			broker.ID, pastGrace, now.Add(-10*time.Second), f.agentA.ID,
		)
		require.NoError(t, err)

		f.srv.SetDispatcher(&isoCallbackDispatcher{
			onStop: func(_ context.Context, _ *store.Agent) error {
				f.dropBindings(f.userID)
				require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
				srvB.drainMembershipLossChecks(ctx)
				return fmt.Errorf("idle stop failed: %w", errStartBrokerNotConnected)
			},
		})

		f.srv.agentIdleSuspendHandler()(ctx)
		f.srv.waitAutoSuspendIdle()

		// Now idle claim is cleared; retryHeldAgentStops on Replica B stops & suspends f.agentA.
		srvB.retryHeldAgentStops(ctx)
		final, err := f.s.GetAgent(ctx, f.agentA.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), final.Phase)
		assert.Empty(t, final.SuspendReason)
		assert.GreaterOrEqual(t, dispB.stops.Load(), int32(1))
	})
}

// TestIdleSuspend_UserStopDuringActiveClaimWins pins Writer Audit Row 17:
// both online user stop and offline queueOfflineStop on Replica B preempt an
// active idle claim on Replica A, leave phase=stopped, and expire held messages.
func TestIdleSuspend_UserStopDuringActiveClaimWins(t *testing.T) {
	for _, offline := range []bool{false, true} {
		label := "online"
		if offline {
			label = "offline_queue"
		}
		t.Run(label, func(t *testing.T) {
			ctx := context.Background()
			srvA, s := testServer(t)
			isoEnableIdleSweep(t, srvA, 15*time.Minute)
			dispB := &isoCallbackDispatcher{}
			srvB := isoSecondReplica(t, s, dispB)

			_, broker, agent := isoSetupIdleAgent(t, s, "user-stop-"+label, string(state.ActivityIdle))
			heldMsg := isoSeedAwaitingSessionMessage(t, s, agent, "msg-"+label)
			u := wakeLifecycleUser(t, s, "ustop-"+label, agent.ProjectID, "agent.lifecycle", "agent.read")

			srvA.SetDispatcher(&isoCallbackDispatcher{
				onStop: func(_ context.Context, a *store.Agent) error {
					if offline {
						require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, store.BrokerStatusOffline))
					}
					rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
					require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, rec.Code, rec.Body.String())
					return nil
				},
			})

			srvA.agentIdleSuspendHandler()(ctx)
			srvA.waitAutoSuspendIdle()

			final, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), final.Phase)
			assert.Empty(t, final.SuspendReason)
			assert.Nil(t, final.IdleSuspendClaimUntil)

			gotMsg, err := s.GetMessage(ctx, heldMsg.ID)
			require.NoError(t, err)
			assert.Equal(t, store.MessageDispatchFailed, gotMsg.DispatchState)
		})
	}
}

// TestIdleSuspend_UserStopAllDuringActiveClaimWins pins Writer Audit Row 18
// (and expired-claim filtering): stop-all on Replica B preempts an active idle
// claim even after Exited 137 moved phase to error and even if auto_suspend_idle
// was toggled off mid-flight, while ignoring stopped agents with expired claims.
func TestIdleSuspend_UserStopAllDuringActiveClaimWins(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	dispB := &isoCallbackDispatcher{}
	srvB := isoSecondReplica(t, s, dispB)

	proj, _, activeAgent := isoSetupIdleAgent(t, s, "stopall-active", string(state.ActivityIdle))
	_, _, expiredAgent := isoSetupIdleAgent(t, s, "stopall-expired", string(state.ActivityIdle))
	// Put expiredAgent in phase=stopped with an expired idle_suspend_claim_until in the same project.
	_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET project_id = ?, phase = 'stopped', run_intent = 'stopped', idle_suspend_claim_until = ? WHERE id = ?",
		proj.ID, time.Now().UTC().Add(-5*time.Minute), expiredAgent.ID,
	)
	require.NoError(t, err)

	srvA.SetDispatcher(&isoCallbackDispatcher{
		onStop: func(_ context.Context, a *store.Agent) error {
			code := 137
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseError),
				ContainerStatus:    "Exited (137)",
				ExitCode:           &code,
				ExitReason:         string(state.ExitReasonCrashed),
				Heartbeat:          true,
				ReportSource:       store.StatusReportSourceRuntime,
				ReportRunID:        a.RunID,
				GuardReincarnation: true,
			}))
			// Toggle auto_suspend_idle off on Replica B before stop-all: active claim must still be preempted!
			srvB.mu.Lock()
			srvB.config.AutoSuspendIdle = false
			srvB.mu.Unlock()

			rec := doRequest(t, srvB, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents/stop-all", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			return nil
		},
	})

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	assert.Equal(t, int32(1), dispB.stops.Load(), "stop-all must stop only activeAgent and skip expiredAgent")
	finalActive, err := s.GetAgent(ctx, activeAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), finalActive.Phase)
	assert.Empty(t, finalActive.SuspendReason)
}

// TestIdleSuspend_UserSuspendDuringActiveClaimWins pins Writer Audit Row 19:
// user suspend on Replica B preempts an active idle claim (even after Exited 137
// moved phase to error and even if auto_suspend_idle was toggled off), commits
// phase=suspended, suspend_reason=user, and expires held messages.
func TestIdleSuspend_UserSuspendDuringActiveClaimWins(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{})

	_, _, agent := isoSetupIdleAgent(t, s, "user-suspend-wins", string(state.ActivityIdle))
	heldMsg := isoSeedAwaitingSessionMessage(t, s, agent, "held-before-user-suspend")
	u := wakeLifecycleUser(t, s, "ususpend-user", agent.ProjectID, "agent.lifecycle", "agent.read")

	srvA.SetDispatcher(&isoCallbackDispatcher{
		onStop: func(_ context.Context, a *store.Agent) error {
			code := 137
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseError),
				ContainerStatus:    "Exited (137)",
				ExitCode:           &code,
				ExitReason:         string(state.ExitReasonCrashed),
				Heartbeat:          true,
				ReportSource:       store.StatusReportSourceRuntime,
				ReportRunID:        a.RunID,
				GuardReincarnation: true,
			}))
			srvB.mu.Lock()
			srvB.config.AutoSuspendIdle = false
			srvB.mu.Unlock()

			rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			return nil
		},
	})

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), final.Phase)
	assert.Equal(t, string(state.SuspendReasonUser), final.SuspendReason)
	assert.False(t, srvA.shouldAutoWake(final))

	gotMsg, err := s.GetMessage(ctx, heldMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotMsg.DispatchState,
		"user suspend must expire surviving awaiting_session rows")
}

// TestIdleSuspend_ConvertHubSuspendToUser_ExpiresHeldMessagesAndGuardsClaim pins
// Writer Audit Row 20: converting a resting suspended(idle) agent to
// suspended(user) expires surviving awaiting_session rows, while IfNoStartClaim
// refuses convertHubSuspendToUser with 409 if an active claim exists.
func TestIdleSuspend_ConvertHubSuspendToUser_ExpiresHeldMessagesAndGuardsClaim(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvA.SetDispatcher(&isoCallbackDispatcher{})

	_, _, agent := isoSetupIdleAgent(t, s, "convert-user", string(state.ActivityIdle))
	heldMsg := isoSeedAwaitingSessionMessage(t, s, agent, "held-until-converted")
	u := wakeLifecycleUser(t, s, "uconv-user", agent.ProjectID, "agent.lifecycle", "agent.read")

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	mid, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), mid.Phase)
	require.Equal(t, string(state.SuspendReasonIdle), mid.SuspendReason)

	// Guard check: if a wake start claim is held, convertHubSuspendToUser returns 409 Conflict.
	claim, err := s.ClaimAgentStart(ctx, agent.ID, "wake-owner", store.StartClaimWake, "", time.Minute)
	require.NoError(t, err)
	recConflict := requestAsIdentity(t, srvA, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	assert.Equal(t, http.StatusConflict, recConflict.Code, recConflict.Body.String())
	_, err = s.ReleaseAgentStart(ctx, agent.ID, claim.ID, "wake-owner")
	require.NoError(t, err)

	// Normal conversion from suspended(idle) -> suspended(user) succeeds and expires heldMsg.
	recOK := requestAsIdentity(t, srvA, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, recOK.Code, recOK.Body.String())

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), final.Phase)
	assert.Equal(t, string(state.SuspendReasonUser), final.SuspendReason)

	gotMsg, err := s.GetMessage(ctx, heldMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotMsg.DispatchState)
}

// TestIdleSuspend_UserStartDuringActiveClaimSettlesOrPreempts pins Writer Audit
// Row 21: user start (and POST /projects/:id/agents resume-in-place) on Replica B
// waits briefly for Replica A's in-flight idle suspend to commit and then
// resumes cleanly; if the claim holder is stalled past the bounded settle wait,
// ClaimAgentStart(StartClaimUser) preempts and clears the claim, minting a new
// RunID with resume=true that fences Replica A's commit.
func TestIdleSuspend_UserStartDuringActiveClaimSettlesOrPreempts(t *testing.T) {
	t.Run("normal settle wait lets idle suspend finish then resumes cleanly", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		var sawResume atomic.Bool
		srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{
			onStart: func(_ context.Context, a *store.Agent, _ string, resume bool) error {
				sawResume.Store(resume)
				a.Phase = string(state.PhaseRunning)
				a.ContainerStatus = "running"
				return nil
			},
		})

		_, _, agent := isoSetupIdleAgent(t, s, "ustart-settle", string(state.ActivityIdle))
		oldRunID := agent.RunID
		u := wakeLifecycleUser(t, s, "ustart-u1", agent.ProjectID, "agent.lifecycle", "agent.read")

		stopEntered := make(chan struct{})
		srvA.SetDispatcher(&isoCallbackDispatcher{
			onStop: func(_ context.Context, _ *store.Agent) error {
				close(stopEntered)
				time.Sleep(40 * time.Millisecond)
				return nil
			},
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			srvA.agentIdleSuspendHandler()(ctx)
			srvA.waitAutoSuspendIdle()
		}()
		<-stopEntered

		rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		<-done

		final, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.True(t, sawResume.Load(), "start after settled idle suspend must pass resume=true")
		assert.Equal(t, string(state.PhaseRunning), final.Phase)
		assert.Equal(t, store.RunIntentRunning, final.RunIntent)
		assert.Empty(t, final.SuspendReason)
		assert.NotEqual(t, oldRunID, final.RunID)
	})

	t.Run("stalled claim holder past bounded wait is preempted by StartClaimUser and POST /agents resume", func(t *testing.T) {
		ctx := context.Background()
		prevOverride := idleSuspendSettleTimeoutOverride
		idleSuspendSettleTimeoutOverride = 20 * time.Millisecond
		t.Cleanup(func() { idleSuspendSettleTimeoutOverride = prevOverride })

		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		var sawResume atomic.Bool
		srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{
			onStart: func(_ context.Context, a *store.Agent, _ string, resume bool) error {
				sawResume.Store(resume)
				a.Phase = string(state.PhaseRunning)
				a.ContainerStatus = "running"
				return nil
			},
		})

		proj, _, agent := isoSetupIdleAgent(t, s, "ustart-preempt", string(state.ActivityIdle))
		oldRunID := agent.RunID
		_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

		rec := doRequest(t, srvB, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", CreateAgentRequest{
			Name:   agent.Slug,
			Task:   "resume-via-create",
			Resume: true,
		})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
		assert.True(t, sawResume.Load(), "preempting an active idle claim must pass resume=true")

		// Late idle commit from Replica A must fail because RunID & claim changed!
		recorded, err := srvA.recordStopStatus(ctx, agent.ID, oldRunID, "auto-suspend", store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			ContainerStatus:     "stopped",
			RetainedActivity:    string(state.ActivityIdle),
			SuspendReason:       string(state.SuspendReasonIdle),
			IfIdleSuspendCommit: &claimUntil,
		})
		require.NoError(t, err)
		assert.False(t, recorded)

		final, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), final.Phase)
		assert.Equal(t, store.RunIntentRunning, final.RunIntent)
		assert.Nil(t, final.IdleSuspendClaimUntil)
		assert.NotEqual(t, oldRunID, final.RunID)
	})
}

// TestIdleSuspend_UserRestartDuringActiveClaimPreempts pins Writer Audit Row 22:
// user restart on Replica B preempts an active idle claim via
// ClaimAgentStart(StartClaimRestart), starts a new RunID, and fences Replica A.
func TestIdleSuspend_UserRestartDuringActiveClaimPreempts(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{})
	regStore := isoEnableSharedConduitRedrive(t, srvA, srvB)

	_, _, agent := isoSetupIdleAgent(t, s, "urestart", string(state.ActivityIdle))
	oldRunID := agent.RunID
	heldMsg := isoSeedAwaitingSessionMessage(t, s, agent, "held-before-restart")
	u := wakeLifecycleUser(t, s, "urestart-user", agent.ProjectID, "agent.lifecycle", "agent.read")

	srvA.SetDispatcher(&isoCallbackDispatcher{
		onStop: func(_ context.Context, a *store.Agent) error {
			rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			return nil
		},
	})

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), final.Phase)
	assert.Equal(t, store.RunIntentRunning, final.RunIntent)
	assert.Empty(t, final.SuspendReason)
	assert.Nil(t, final.IdleSuspendClaimUntil)
	assert.NotEqual(t, oldRunID, final.RunID)

	preRedrive, err := s.GetMessage(ctx, heldMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, oldRunID, preRedrive.HoldLaunchID, "restart must not rebind old-run held messages")

	isoRegisterDeliverSession(t, regStore, "iso-relay-1", final)
	srvB.redriveAgentHeldMessagesOnce(ctx, agent.ID)

	gotMsg, err := s.GetMessage(ctx, heldMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotMsg.DispatchState)
}

// TestIdleSuspend_UserDeleteDuringActiveClaimPreempts pins Writer Audit Row 23:
// DELETE /api/v1/agents/:id on Replica B preempts an active idle claim and
// removes the agent cleanly without Replica A resurrecting or stranding it.
func TestIdleSuspend_UserDeleteDuringActiveClaimPreempts(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{})

	_, _, agent := isoSetupIdleAgent(t, s, "udelete", string(state.ActivityIdle))

	srvA.SetDispatcher(&isoCallbackDispatcher{
		onStop: func(_ context.Context, a *store.Agent) error {
			rec := doRequest(t, srvB, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
			require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
			return nil
		},
	})

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	_, err := s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestIdleSuspend_ReincarnateDuringActiveClaimPreempts pins Writer Audit Row 24:
// POST /api/v1/agents/:id/reincarnate on Replica B preempts an active idle claim
// via ClaimAgentReincarnation (clearing idle_suspend_claim_until and fencing
// IfIdleSuspendCommit).
func TestIdleSuspend_ReincarnateDuringActiveClaimPreempts(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srvA, s, proj, broker := setupReincarnateTestServer(t, disp)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvB := isoSecondReplica(t, s, disp)

	agent := newReincarnateTestAgent(t, s, proj, broker, nil)
	now := time.Now().UTC()
	pastGrace := now.Add(-20 * time.Minute)
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET run_id = 'run-reinc-idle', activity = 'idle', activity_since = ?, last_seen = ? WHERE id = ?",
		pastGrace, now.Add(-10*time.Second), agent.ID,
	)
	require.NoError(t, err)

	_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	rec := doRequest(t, srvB, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	afterClaim, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Nil(t, afterClaim.IdleSuspendClaimUntil, "ClaimAgentReincarnation must clear idle_suspend_claim_until")

	// Replica A's idle-suspend commit must be refused!
	recorded, err := srvA.recordStopStatus(ctx, agent.ID, "run-reinc-idle", "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	assert.False(t, recorded)
}

// TestIdleSuspend_DeliveryRacesActiveClaimWaitsAndAutoWakes pins Writer Audit
// Row 25: a message arriving on Replica B during an active idle claim on
// Replica A skips writing last_delivery_at, waits for Replica A to commit
// phase=suspended, suspend_reason=idle, and then auto-wakes and delivers. Also
// verifies that withStartClaim with start claims off yields via swapSystemRunIntent.
func TestIdleSuspend_DeliveryRacesActiveClaimWaitsAndAutoWakes(t *testing.T) {
	ctx := context.Background()
	srvA, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	isoEnableIdleSweep(t, srvA, 15*time.Minute)

	dispB := &isoCallbackDispatcher{autoReadyOnStart: true}
	srvB := isoSecondReplica(t, s, dispB)
	t.Cleanup(srvB.waitAutoWakeIdle)

	_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
	require.NoError(t, err)
	now := time.Now().UTC()
	pastGrace := now.Add(-20 * time.Minute)
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET run_id = 'run-wake-race', activity = 'idle', activity_since = ?, last_seen = ? WHERE id = ?",
		pastGrace, now.Add(-10*time.Second), target.ID,
	)
	require.NoError(t, err)

	// Verify startClaimsEnabled()==false wake path also yields to an active claim via swapSystemRunIntent.
	unclaimedSrv := &Server{store: s}
	targetLoaded, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	_, intentAt, _ := isoClaimIdleDirectly(t, s, targetLoaded, 45*time.Second)
	werr := unclaimedSrv.withStartClaim(ctx, targetLoaded, store.StartClaimWake, nil, func(_ context.Context, _ func() error) (bool, error) {
		return false, nil
	})
	require.ErrorIs(t, werr, store.ErrIdleSuspendPrecondition)
	_, err = s.RevertRunIntent(ctx, target.ID, store.RunIntentStopped, intentAt, store.RunIntentRunning)
	require.NoError(t, err)
	require.NoError(t, s.ClearIdleSuspendClaim(ctx, target.ID))

	stopEntered := make(chan struct{})
	srvA.SetDispatcher(&isoCallbackDispatcher{
		onStop: func(_ context.Context, _ *store.Agent) error {
			close(stopEntered)
			time.Sleep(45 * time.Millisecond)
			return nil
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		srvA.agentIdleSuspendHandler()(ctx)
		srvA.waitAutoSuspendIdle()
	}()
	<-stopEntered

	staleSnap, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	staleSnap.IdleSuspendClaimActive = false
	res, dmErr := srvB.ExecuteAgentDM(ctx, awAgentDMInput(sender, staleSnap, "wake-from-replica-b"))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	<-done

	final, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), final.Phase)
	assert.Empty(t, final.SuspendReason)
	assert.Equal(t, []string{"wake-from-replica-b"}, dispB.deliveredMessages())
}

// TestIdleSuspend_HeldMessagesSurviveAndRedriveOnResumeAndAutoWake pins Writer
// Audit Row 26: messages held in awaiting_session survive ClaimIdleSuspend,
// Exited 137 post-stop heartbeat, resting phase=suspended(idle), sweepHeldMessages,
// and redriveAgentHeldMessagesOnce without waking; then rebind hold_launch_id to
// the new RunID and re-drive to delivered on BOTH manual lifecycle start resume
// and incoming-DM auto-wake (ahead of the wake message).
func TestIdleSuspend_HeldMessagesSurviveAndRedriveOnResumeAndAutoWake(t *testing.T) {
	t.Run("manual lifecycle start resume rebinds hold_launch_id and redrives held row", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		dispB := &isoCallbackDispatcher{finishConduitDelivery: true}
		srvB := isoSecondReplica(t, s, dispB)
		regStore := isoEnableSharedConduitRedrive(t, srvA, srvB)

		_, _, agent := isoSetupIdleAgent(t, s, "hold-manual-resume", string(state.ActivityIdle))
		oldRunID := agent.RunID
		u := wakeLifecycleUser(t, s, "uhold-resume", agent.ProjectID, "agent.lifecycle", "agent.read", "agent.message")
		heldMsg := isoSeedAwaitingSessionMessageFrom(t, s, "user:"+u.Email, u.ID, agent, "survive-manual-resume")

		srvA.SetDispatcher(&isoCallbackDispatcher{
			onStop: func(_ context.Context, a *store.Agent) error {
				code := 137
				require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
					Phase:              string(state.PhaseError),
					ContainerStatus:    "Exited (137)",
					ExitCode:           &code,
					ExitReason:         string(state.ExitReasonCrashed),
					Heartbeat:          true,
					ReportSource:       store.StatusReportSourceRuntime,
					ReportRunID:        a.RunID,
					GuardReincarnation: true,
				}))
				// Sweeper and redrive while idle claim is active must preserve and defer without waking.
				srvB.sweepHeldMessages(ctx)
				srvB.redriveAgentHeldMessagesOnce(ctx, a.ID)
				return nil
			},
		})

		srvA.agentIdleSuspendHandler()(ctx)
		srvA.waitAutoSuspendIdle()

		// Sweeper and redrive while resting in suspended(idle) must also preserve without waking!
		srvB.sweepHeldMessages(ctx)
		srvB.redriveAgentHeldMessagesOnce(ctx, agent.ID)
		assert.Zero(t, dispB.starts.Load(), "redrive must never wake a suspended(idle) agent")

		midMsg, err := s.GetMessage(ctx, heldMsg.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchAwaitingSession, midMsg.DispatchState)

		// Manual resume via POST /api/v1/agents/:id/start rebinds hold_launch_id to new RunID.
		rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		resumed, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.NotEqual(t, oldRunID, resumed.RunID)

		reboundMsg, err := s.GetMessage(ctx, heldMsg.ID)
		require.NoError(t, err)
		assert.Equal(t, resumed.RunID, reboundMsg.HoldLaunchID)

		isoRegisterDeliverSession(t, regStore, "iso-relay-1", resumed)
		srvB.redriveAgentHeldMessagesOnce(ctx, agent.ID)
		deliveredMsg, err := s.GetMessage(ctx, heldMsg.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDelivered, deliveredMsg.DispatchState)
		assert.Equal(t, []string{"survive-manual-resume"}, dispB.deliveredMessages())
	})

	t.Run("auto-wake on incoming DM rebinds surviving held row and redrives in FIFO order", func(t *testing.T) {
		ctx := context.Background()
		srvA, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		dispB := &isoCallbackDispatcher{autoReadyOnStart: true, finishConduitDelivery: true}
		srvB := isoSecondReplica(t, s, dispB)
		t.Cleanup(srvB.waitAutoWakeIdle)
		regStore := isoEnableSharedConduitRedrive(t, srvA, srvB)

		_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
		require.NoError(t, err)
		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET runtime = 'cloudrun', run_id = 'run-hold-autowake', activity = 'idle', activity_since = ?, last_seen = ? WHERE id = ?",
			pastGrace, now.Add(-10*time.Second), target.ID,
		)
		require.NoError(t, err)
		targetLoaded, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		heldMsg := isoSeedAwaitingSessionMessageFrom(t, s, "agent:"+sender.Slug, sender.ID, targetLoaded, "first-surviving-held")

		srvA.SetDispatcher(&isoCallbackDispatcher{})
		srvA.agentIdleSuspendHandler()(ctx)
		srvA.waitAutoSuspendIdle()

		suspendedTarget, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		require.Equal(t, string(state.PhaseSuspended), suspendedTarget.Phase)
		require.Equal(t, string(state.SuspendReasonIdle), suspendedTarget.SuspendReason)

		res, dmErr := srvB.ExecuteAgentDM(ctx, awAgentDMInput(sender, suspendedTarget, "second-wake-dm"))
		require.Nil(t, dmErr)
		require.NotNil(t, res)

		wokenTarget, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		isoRegisterDeliverSession(t, regStore, "iso-relay-1", wokenTarget)

		// Both heldMsg (rebound from old run) and res.MessageID (queued behind heldMsg on active hold)
		// now share the woken RunID; redriving delivers them in FIFO order!
		srvB.redriveAgentHeldMessagesOnce(ctx, target.ID)

		gotFirst, err := s.GetMessage(ctx, heldMsg.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDelivered, gotFirst.DispatchState)
		gotSecond, err := s.GetMessage(ctx, res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDelivered, gotSecond.DispatchState)
		assert.Equal(t, []string{"first-surviving-held", "second-wake-dm"}, dispB.deliveredMessages())
	})
}

// TestIdleSuspend_OwnershipMatrix_LaunchReportsAndReapers pins Writer Audit Row 27:
// a late failed ApplyLaunchReport after running_observed yields while an active
// idle claim is held, and RunLaunchReaperTick preserves active idle claims.
func TestIdleSuspend_OwnershipMatrix_LaunchReportsAndReapers(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)

	_, broker, agent := isoSetupIdleAgent(t, s, "launch-late-fail", string(state.ActivityIdle))
	// Simulate an async create launch that already ended as running_observed before the agent became idle.
	launchID := uuid.NewString()
	_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET launch_id = ?, launch_kind = ?, launch_state = ?, launch_end_reason = ?, launch_owner = ? WHERE id = ?",
		launchID, store.LaunchKindCreate, store.LaunchStateEnded, store.LaunchEndReasonRunningObserved, "broker-inst-1", agent.ID,
	)
	require.NoError(t, err)

	_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	// Late failed report from broker during active idle claim must NOT move phase to error!
	_, updated, err := s.ApplyLaunchReport(ctx, agent.ID, broker.ID, store.LaunchReport{
		LaunchID:   launchID,
		InstanceID: "broker-inst-1",
		Seq:        10,
		State:      "failed",
		Step:       "start",
		Message:    "late failure report",
		ErrorCode:  "runtime_failed",
		RunID:      agent.RunID,
	})
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), updated.Phase, "failedAfterRunningObservedApplies must yield to active idle claim")

	_, err = s.RunLaunchReaperTick(ctx, store.ReaperParams{
		KeepaliveInterval: 15 * time.Second,
		ReaperInterval:    15 * time.Second,
	})
	require.NoError(t, err)

	recorded, err := srvA.recordStopStatus(ctx, agent.ID, agent.RunID, "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	assert.True(t, recorded)
}

// TestIdleSuspend_OwnershipMatrix_StartClaimReaper pins Writer Audit Row 28:
// ConvertUnconfirmedToStop, RevertRunIntent(running -> stopped), and
// settleFailedProvisionedStart preserve an active idle claim.
func TestIdleSuspend_OwnershipMatrix_StartClaimReaper(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{})

	_, _, agent := isoSetupIdleAgent(t, s, "start-reaper", string(state.ActivityIdle))
	_, intentAt, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	_, err := s.ConvertUnconfirmedToStop(ctx, agent.ID, tid("unconf"), srvB.instanceID, time.Minute)
	assert.ErrorIs(t, err, store.ErrClaimPredicate)

	assert.False(t, srvB.settleFailedProvisionedStart(ctx, agent.ID, intentAt.Add(-time.Minute), "refused"))

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, after.IdleSuspendClaimActive)
	assert.Equal(t, claimUntil, *after.IdleSuspendClaimUntil)
}

// TestIdleSuspend_OwnershipMatrix_UpdateAgentAndBackfillPreserveClaim pins Writer
// Audit Row 29: UpdateAgent CAS bumps, message-only UpdateAgentStatus writes, and
// BackfillRunIntent preserve an active idle claim and allow IfIdleSuspendCommit.
func TestIdleSuspend_OwnershipMatrix_UpdateAgentAndBackfillPreserveClaim(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)

	_, _, agent := isoSetupIdleAgent(t, s, "update-backfill", string(state.ActivityIdle))
	_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	cur, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	cur.Labels = map[string]string{"updated": "during-claim"}
	require.NoError(t, s.UpdateAgent(ctx, cur))

	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Message: "pending config note",
	}))
	_, err = s.BackfillRunIntent(ctx)
	require.NoError(t, err)

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, after.IdleSuspendClaimActive)
	assert.Equal(t, claimUntil, *after.IdleSuspendClaimUntil)

	recorded, err := srvA.recordStopStatus(ctx, agent.ID, agent.RunID, "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	assert.True(t, recorded)

	// Managed agent runtimes (managed:*) do not support suspend/resume and are
	// skipped by harnessSupportsResume / the idle sweep.
	_, _, managedAgent := isoSetupIdleAgent(t, s, "managed-runtime-skip", string(state.ActivityIdle))
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET runtime = 'managed:vertex' WHERE id = ?", managedAgent.ID)
	require.NoError(t, err)
	loadedManaged, err := s.GetAgent(ctx, managedAgent.ID)
	require.NoError(t, err)
	supported, _, _ := srvA.harnessSupportsResume(ctx, loadedManaged)
	assert.False(t, supported, "managed agent runtimes must not be suspendable")
}

type isoKeysDispatcher struct {
	isoCallbackDispatcher
	keysCalls atomic.Int32
}

func (d *isoKeysDispatcher) DispatchAgentKeys(_ context.Context, _ agentkeys.Target, operationID string, _ time.Time, _ string) (agentkeys.BrokerResult, error) {
	d.keysCalls.Add(1)
	return agentkeys.BrokerResult{
		OperationID: operationID,
		Outcome:     agentkeys.OutcomeDispatched,
	}, nil
}

// TestIdleSuspend_ExecuteAgentKeysOutOfScopePreservesClaim pins Writer Audit
// Row 30: operator raw terminal input (POST /api/v1/agents/:id/keys) is out of
// scope of message delivery, neither marking last_delivery_at nor clearing an
// active idle_suspend_claim_until.
func TestIdleSuspend_ExecuteAgentKeysOutOfScopePreservesClaim(t *testing.T) {
	ctx := context.Background()
	f := newAgentKeysRouteFixture(t)
	isoEnableIdleSweep(t, f.srv, 15*time.Minute)
	disp := &isoKeysDispatcher{}
	f.srv.SetDispatcher(disp)

	now := time.Now().UTC()
	pastGrace := now.Add(-20 * time.Minute)
	_, err := f.store.SetRunIntent(ctx, f.agentInA.ID, store.RunIntentRunning)
	require.NoError(t, err)
	_, err = f.store.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET run_id = 'run-keys-idle', activity = 'idle', activity_since = ?, last_seen = ? WHERE id = ?",
		pastGrace, now.Add(-10*time.Second), f.agentInA.ID,
	)
	require.NoError(t, err)
	_, _, claimUntil := isoClaimIdleDirectly(t, f.store, f.agentInA, 45*time.Second)

	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int32(1), disp.keysCalls.Load())

	after, err := f.store.GetAgent(ctx, f.agentInA.ID)
	require.NoError(t, err)
	assert.True(t, after.IdleSuspendClaimActive)
	assert.Equal(t, claimUntil, *after.IdleSuspendClaimUntil)
	assert.Nil(t, after.LastDeliveryAt, "ExecuteAgentKeys must not stamp last_delivery_at")
}

// TestIdleSuspend_OwnershipMatrix_StartDispatchRollback pins Writer Audit
// Row 31: beginStartDispatch (Phase: starting, IfPhase: priorPhase) and
// (*startDispatch).rollback (Phase: priorPhase, IfPhase: starting, Restore: true)
// cannot overwrite or clear an active idle-suspend claim on a running row, and a
// failed resume from suspended(idle) rolls back cleanly to phase=suspended,
// suspend_reason=idle with retained activity and surviving held messages intact.
func TestIdleSuspend_OwnershipMatrix_StartDispatchRollback(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)

	// 1. Stale startDispatch rollback against an active running idle claim
	// rejects on IfPhase: starting without touching idle_suspend_claim_until.
	_, _, claimedAgent := isoSetupIdleAgent(t, s, "start-dispatch-guard", string(state.ActivityCompleted))
	_, _, claimUntil := isoClaimIdleDirectly(t, s, claimedAgent, 45*time.Second)

	staleDispatch := &startDispatch{
		s:                 srvA,
		agent:             claimedAgent,
		priorPhase:        string(state.PhaseSuspended),
		priorActivity:     string(state.ActivityCompleted),
		priorRestingPhase: string(state.PhaseSuspended),
		marked:            true,
	}
	staleDispatch.rollback(ctx)

	afterStale, err := s.GetAgent(ctx, claimedAgent.ID)
	require.NoError(t, err)
	assert.True(t, afterStale.IdleSuspendClaimActive)
	assert.Equal(t, claimUntil, *afterStale.IdleSuspendClaimUntil)
	assert.Equal(t, string(state.PhaseRunning), afterStale.Phase)

	// 2. Real failed resume from resting suspended(idle) invokes beginStartDispatch
	// (Phase: starting) and rollback (Phase: suspended, Restore: true), preserving
	// suspend_reason=idle, retained activity, and surviving awaiting_session holds.
	_, _, resumeAgent := isoSetupIdleAgent(t, s, "start-dispatch-resume-fail", string(state.ActivityCompleted))
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET runtime = 'cloudrun' WHERE id = ?", resumeAgent.ID)
	require.NoError(t, err)
	resumeAgent, err = s.GetAgent(ctx, resumeAgent.ID)
	require.NoError(t, err)
	heldRow := isoSeedAwaitingSessionMessage(t, s, resumeAgent, "held-across-failed-resume")

	srvA.SetDispatcher(&isoCallbackDispatcher{st: s})
	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()

	suspendedRow, err := s.GetAgent(ctx, resumeAgent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), suspendedRow.Phase)
	require.Equal(t, string(state.SuspendReasonIdle), suspendedRow.SuspendReason)
	require.Equal(t, string(state.ActivityCompleted), suspendedRow.Activity)

	srvA.SetDispatcher(&isoCallbackDispatcher{
		st: s,
		onStart: func(_ context.Context, _ *store.Agent, _ string, _ bool) error {
			return errors.New("broker start failed")
		},
	})
	u := wakeLifecycleUser(t, s, "iso-start-fail-user", resumeAgent.ProjectID, "agent.lifecycle", "agent.read")
	rec := requestAsIdentity(t, srvA, authUser(u), http.MethodPost, "/api/v1/agents/"+resumeAgent.ID+"/start", nil)
	require.NotEqual(t, http.StatusOK, rec.Code)

	rolledBack, err := s.GetAgent(ctx, resumeAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), rolledBack.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), rolledBack.SuspendReason)
	assert.Equal(t, string(state.ActivityCompleted), rolledBack.Activity)

	gotHeld, err := s.GetMessage(ctx, heldRow.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchAwaitingSession, gotHeld.DispatchState,
		"failed resume rollback to suspended(idle) must keep awaiting_session held row")
}

// TestIdleSuspend_SettingsToggleDuringClaimAndStaleReplica verifies Pass-6 R1:
//   - An active idle claim on the target row is always honoured even if
//     auto_suspend_idle is toggled off mid-claim (deliveries wait/auto-wake into
//     the resumed run and never dispatch into the dying container).
//   - Whenever lifecycle.idle_suspend is on, deliveries stamp last_delivery_at
//     even on a replica whose auto_suspend_idle snapshot is still false, so a
//     racing sweep on another replica aborts ClaimIdleSuspend.
//   - autoSuspendAgent re-reads auto_suspend_idle immediately before
//     ClaimIdleSuspend (after checkEphemeralWorkspaceBeforeStop) and aborts if
//     the setting turned off.
func TestIdleSuspend_SettingsToggleDuringClaimAndStaleReplica(t *testing.T) {
	t.Run("setting toggled off during active claim still waits and auto-wakes into new run", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		_, _, target := isoSetupIdleAgent(t, s, "toggle-off-mid-claim", string(state.ActivityIdle))
		_, _, sender := isoSetupIdleAgent(t, s, "toggle-off-sender", string(state.ActivityIdle))
		_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET project_id = ? WHERE id = ?", target.ProjectID, sender.ID)
		require.NoError(t, err)
		sender.ProjectID = target.ProjectID
		initialRunID := target.RunID

		stopEntered := make(chan struct{})
		releaseStop := make(chan struct{})
		var msgRunIDs []string
		var msgMu sync.Mutex

		disp := &isoCallbackDispatcher{
			st:               s,
			autoReadyOnStart: true,
			onStop: func(_ context.Context, _ *store.Agent) error {
				close(stopEntered)
				<-releaseStop
				return nil
			},
			onMsg: func(_ context.Context, a *store.Agent, _ string) error {
				msgMu.Lock()
				msgRunIDs = append(msgRunIDs, a.RunID)
				msgMu.Unlock()
				return nil
			},
		}
		srvA.SetDispatcher(disp)

		srvA.agentIdleSuspendHandler()(ctx)
		<-stopEntered

		// Admin toggles auto_suspend_idle OFF while DispatchAgentStop is in flight.
		srvA.mu.Lock()
		srvA.config.AutoSuspendIdle = false
		srvA.mu.Unlock()

		// Non-auto-wake delivery (reconcile.deliverMessage) must honour the active
		// claim and refuse rather than dispatching into the dying container.
		err = srvA.deliverMessage(ctx, &store.Message{
			ID:      uuid.NewString(),
			AgentID: target.ID,
			Msg:     "reconcile-during-toggled-off-claim",
		})
		require.Error(t, err)
		assert.Empty(t, disp.deliveredMessages(), "must never dispatch into dying container")

		// Auto-waking DM arrives while AutoSuspendIdle=false and claim is still active:
		// it must wait for suspended(idle) commit, auto-wake into a new RunID, and
		// deliver only to the resumed container.
		dmDone := make(chan error, 1)
		freshTarget, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		go func() {
			res, dmErr := srvA.ExecuteAgentDM(ctx, awAgentDMInput(sender, freshTarget, "dm-across-setting-off"))
			if dmErr != nil {
				dmDone <- errors.New(dmErr.Message)
				return
			}
			if res == nil || res.Outcome != AgentDMAccepted {
				dmDone <- fmt.Errorf("unexpected DM result: %+v", res)
				return
			}
			dmDone <- nil
		}()

		time.Sleep(30 * time.Millisecond)
		assert.Empty(t, disp.deliveredMessages(), "DM must wait while idle stop is in flight")
		close(releaseStop)

		require.NoError(t, <-dmDone)
		srvA.waitAutoSuspendIdle()

		assert.Equal(t, []string{"dm-across-setting-off"}, disp.deliveredMessages())
		msgMu.Lock()
		gotRunIDs := append([]string(nil), msgRunIDs...)
		msgMu.Unlock()
		require.Len(t, gotRunIDs, 1)
		assert.NotEqual(t, initialRunID, gotRunIDs[0], "DM must deliver to resumed run, never the dying run")
	})

	t.Run("setting toggled on while peer replica has stale off snapshot marks delivery and aborts claim", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute) // Replica A already sees auto_suspend_idle=true

		dispA := &isoCallbackDispatcher{st: s}
		srvA.SetDispatcher(dispA)
		dispB := &isoCallbackDispatcher{st: s}
		srvB := isoSecondReplica(t, s, dispB)
		// Replica B still has stale auto_suspend_idle=false (propagation lag),
		// with lifecycle.idle_suspend experiment enabled.
		srvB.mu.Lock()
		srvB.config.AutoSuspendIdle = false
		srvB.mu.Unlock()

		_, _, target := isoSetupIdleAgent(t, s, "stale-replica-mark", string(state.ActivityIdle))
		_, _, sender := isoSetupIdleAgent(t, s, "stale-replica-sender", string(state.ActivityIdle))
		_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET project_id = ? WHERE id = ?", target.ProjectID, sender.ID)
		require.NoError(t, err)
		sender.ProjectID = target.ProjectID

		// Replica A has already scanned target as an idle candidate.
		candidateSnap, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)

		// Before Replica A claims, a DM lands on Replica B (whose AutoSuspendIdle is false).
		res, dmErr := srvB.ExecuteAgentDM(ctx, awAgentDMInput(sender, target, "dm-on-stale-replica-b"))
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, []string{"dm-on-stale-replica-b"}, dispB.deliveredMessages())

		afterMark, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		require.NotNil(t, afterMark.LastDeliveryAt, "Replica B must stamp last_delivery_at while experiment is on")

		// Replica A's worker runs on its pre-delivery candidate snapshot: ClaimIdleSuspend
		// sees last_delivery_at >= activity_since and aborts without stopping the container.
		suspended := srvA.autoSuspendAgent(ctx, dispA, candidateSnap, state.SuspendReasonIdle)
		assert.False(t, suspended)
		assert.Zero(t, dispA.stops.Load())

		final, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), final.Phase)
	})

	t.Run("setting toggled off during workspace check aborts before ClaimIdleSuspend", func(t *testing.T) {
		ctx := context.Background()
		srvA, s := testServer(t)
		isoEnableIdleSweep(t, srvA, 15*time.Minute)
		_, _, target := isoSetupIdleAgent(t, s, "toggle-off-before-claim", string(state.ActivityIdle))
		_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET runtime = 'kubernetes', workspace_placement = ? WHERE id = ?",
			api.WorkspacePlacementLocal, target.ID)
		require.NoError(t, err)
		snap, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)

		var execCalled atomic.Bool
		disp := &isoCallbackDispatcher{
			st: s,
			onExec: func(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
				execCalled.Store(true)
				// Admin disables auto_suspend_idle while pre-stop workspace check is in flight.
				srvA.mu.Lock()
				srvA.config.AutoSuspendIdle = false
				srvA.mu.Unlock()
				return "scion-workspace-check commits=1 files=2\n", 0, nil
			},
		}
		srvA.SetDispatcher(disp)

		suspended := srvA.autoSuspendAgent(ctx, disp, snap, state.SuspendReasonIdle)
		assert.False(t, suspended)
		assert.True(t, execCalled.Load(), "pre-stop workspace check must have run")
		assert.Zero(t, disp.stops.Load(), "worker must re-read setting before ClaimIdleSuspend and skip stop")

		after, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), after.Phase)
		assert.Equal(t, store.RunIntentRunning, after.RunIntent)
		assert.Nil(t, after.IdleSuspendClaimUntil)
		assert.Nil(t, recordedWorkspaceAtStop(after), "aborted pre-claim workspace-at-stop annotation must be cleared")
	})
}

// TestIdleSuspend_OwnershipMatrix_PostCommitClearIdleSuspendClaim pins the
// two-step commit-then-clear lifecycle: UpdateAgentStatus(IfIdleSuspendCommit)
// commits phase=suspended, suspend_reason=idle while preserving
// idle_suspend_claim_until during post-commit credential/grant/quota release
// (so MarkAgentDelivery still reports claimActive=true and convertHubSuspendToUser
// refuses with 409), and the subsequent post-commit ClearIdleSuspendClaim
// (and autoSuspendAgent's full execution) clears idle_suspend_claim_until to nil.
func TestIdleSuspend_OwnershipMatrix_PostCommitClearIdleSuspendClaim(t *testing.T) {
	ctx := context.Background()
	srvA, s := testServer(t)
	isoEnableIdleSweep(t, srvA, 15*time.Minute)
	srvB := isoSecondReplica(t, s, &isoCallbackDispatcher{st: s})

	_, _, agent := isoSetupIdleAgent(t, s, "post-commit-clear", string(state.ActivityIdle))
	u := wakeLifecycleUser(t, s, "iso-post-commit-user", agent.ProjectID, "agent.lifecycle", "agent.read")
	_, _, claimUntil := isoClaimIdleDirectly(t, s, agent, 45*time.Second)

	recorded, err := srvA.recordStopStatus(ctx, agent.ID, agent.RunID, "auto-suspend", store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		ContainerStatus:     "stopped",
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfIdleSuspendCommit: &claimUntil,
	})
	require.NoError(t, err)
	require.True(t, recorded)

	// Immediately after IfIdleSuspendCommit, phase=suspended(idle) is committed
	// while idle_suspend_claim_until remains active until ClearIdleSuspendClaim.
	mid, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), mid.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), mid.SuspendReason)
	require.NotNil(t, mid.IdleSuspendClaimUntil, "IfIdleSuspendCommit must keep idle_suspend_claim_until set until post-commit release finishes")
	assert.True(t, mid.IdleSuspendClaimActive)

	snap, claimActive, err := s.MarkAgentDelivery(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, claimActive, "MarkAgentDelivery must continue reporting claimActive during post-commit window")
	assert.Nil(t, snap.LastDeliveryAt)

	rec := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "convertHubSuspendToUser returns 409 while post-commit claim is still active")

	// Post-commit ClearIdleSuspendClaim releases the claim, unblocking conversion and wake.
	require.NoError(t, s.ClearIdleSuspendClaim(ctx, agent.ID))
	cleared, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Nil(t, cleared.IdleSuspendClaimUntil)
	assert.False(t, cleared.IdleSuspendClaimActive)

	recOK := requestAsIdentity(t, srvB, authUser(u), http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, recOK.Code, recOK.Body.String())

	// Also verify end-to-end autoSuspendAgent(idle) clears idle_suspend_claim_until on success.
	_, _, e2eAgent := isoSetupIdleAgent(t, s, "post-commit-e2e", string(state.ActivityCompleted))
	disp := &isoCallbackDispatcher{st: s}
	srvA.SetDispatcher(disp)
	assert.True(t, srvA.autoSuspendAgent(ctx, disp, e2eAgent, state.SuspendReasonIdle))
	e2eAfter, err := s.GetAgent(ctx, e2eAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), e2eAfter.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), e2eAfter.SuspendReason)
	assert.Nil(t, e2eAfter.IdleSuspendClaimUntil, "autoSuspendAgent success path must clear idle_suspend_claim_until via ClearIdleSuspendClaim")
	assert.False(t, e2eAfter.IdleSuspendClaimActive)
}

type isoPostCommitReportRaceStore struct {
	store.Store
	fault           *storeFaultSwitch
	bypass          atomic.Bool
	targetID        string
	wantReports     int
	mu              sync.Mutex
	reportsAtStore  int
	allReportsReady chan struct{}
	commitLanded    chan struct{}
	commitOnce      sync.Once
}

func (w *isoPostCommitReportRaceStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return w.Store.(store.AdvisoryLocker).TryAdvisoryLock(ctx, key)
}

func (w *isoPostCommitReportRaceStore) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	return w.Store.(store.AdvisoryLocker).TryAdvisoryLockObject(ctx, classID, objID)
}

func (w *isoPostCommitReportRaceStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	if !w.fault.Active() || w.bypass.Load() || id != w.targetID {
		return w.Store.UpdateAgentStatus(ctx, id, su)
	}
	if su.ReportSource != "" {
		w.mu.Lock()
		w.reportsAtStore++
		if w.reportsAtStore == w.wantReports {
			close(w.allReportsReady)
		}
		w.mu.Unlock()
		<-w.commitLanded
		return w.Store.UpdateAgentStatus(ctx, id, su)
	}
	err := w.Store.UpdateAgentStatus(ctx, id, su)
	if su.IfIdleSuspendCommit != nil && su.Phase == string(state.PhaseSuspended) {
		w.commitOnce.Do(func() { close(w.commitLanded) })
	}
	return err
}

// TestIdleSuspend_PostCommitLateAgentAndRuntimeReportsKeepSuspendedIdleAndAutoWakeable
// verifies the multi-replica interaction between Phase 3's IfIdleSuspendCommit
// and ptone/scion#8256 (GoogleCloudPlatform/scion#4981): when Replica B reads
// phase=running during Replica A's container stop for both an agent shutdown
// status report (phase=stopped, "Session ended") and a broker heartbeat
// reporting stopped, and both UpdateAgentStatus writes land AFTER Replica A's
// IfIdleSuspendCommit writes phase=suspended, suspend_reason=idle, the locked
// row stays phase=suspended, suspend_reason=idle with retained activity intact,
// and an incoming DM without --wake auto-wakes the agent and delivers the DM.
func TestIdleSuspend_PostCommitLateAgentAndRuntimeReportsKeepSuspendedIdleAndAutoWakeable(t *testing.T) {
	ctx := context.Background()
	srvA, s, raceStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *isoPostCommitReportRaceStore {
		return &isoPostCommitReportRaceStore{
			Store:           inner,
			fault:           f,
			wantReports:     2,
			allReportsReady: make(chan struct{}),
			commitLanded:    make(chan struct{}),
		}
	})
	isoEnableIdleSweep(t, srvA, 15*time.Minute)

	dispB := &isoCallbackDispatcher{st: s, autoReadyOnStart: true}
	srvB := isoSecondReplica(t, s, dispB)
	installStoreFault(t, srvB, func(store.Store, *storeFaultSwitch) *isoPostCommitReportRaceStore {
		return raceStore
	})

	_, _, target := isoSetupIdleAgent(t, s, "post-commit-late-reports", string(state.ActivityIdle))
	_, _, sender := isoSetupIdleAgent(t, s, "post-commit-late-sender", string(state.ActivityThinking))
	_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET project_id = ? WHERE id = ?", target.ProjectID, sender.ID)
	require.NoError(t, err)
	sender.ProjectID = target.ProjectID
	ensureStandingRoot(t, s, target.ProjectID, DevUserID)
	for _, id := range []string{target.ID, sender.ID} {
		row, gerr := s.GetAgent(ctx, id)
		require.NoError(t, gerr)
		row.MessageMode = store.MessageModeProject
		row.Ancestry = []string{DevUserID}
		require.NoError(t, s.UpdateAgent(ctx, row))
	}
	now := time.Now().UTC()
	_, err = s.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
		string(state.ActivityIdle), now.Add(-20*time.Minute), now.Add(-10*time.Second), target.ID,
	)
	require.NoError(t, err)
	target, err = s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	sender, err = s.GetAgent(ctx, sender.ID)
	require.NoError(t, err)

	tok, cred, err := srvA.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   target.ID,
		ProjectID: target.ProjectID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
	}, target.RunID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(ctx, cred))

	raceStore.targetID = target.ID
	var reportWG sync.WaitGroup
	dispA := &isoCallbackDispatcher{
		st: s,
		onStop: func(_ context.Context, a *store.Agent) error {
			if a.ID != target.ID {
				return nil
			}
			// Both Replica B handlers read phase=running while Replica A's claim is
			// active (no local lifecycleOp on Replica B) and block at UpdateAgentStatus
			// until Replica A's IfIdleSuspendCommit writes phase=suspended(idle).
			reportWG.Add(2)
			go func() {
				defer reportWG.Done()
				rec := doRequestWithAgentToken(t, srvB, http.MethodPost, "/api/v1/agents/"+a.ID+"/status", map[string]any{
					"phase":      string(state.PhaseStopped),
					"message":    "Session ended",
					"exitReason": string(state.ExitReasonSignaled),
				}, tok)
				assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}()
			go func() {
				defer reportWG.Done()
				exitZero := 0
				rec := doRequest(t, srvB, http.MethodPost, "/api/v1/runtime-brokers/"+a.RuntimeBrokerID+"/heartbeat",
					brokerHeartbeatRequest{
						Status: store.BrokerStatusOnline,
						Projects: []brokerProjectHeartbeat{{
							ProjectID:  a.ProjectID,
							AgentCount: 1,
							Agents: []brokerAgentHeartbeat{{
								Slug:            a.Slug,
								RunID:           a.RunID,
								Phase:           string(state.PhaseStopped),
								ContainerStatus: "exited (0)",
								ExitCode:        &exitZero,
							}},
						}},
					})
				assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}()
			select {
			case <-raceStore.allReportsReady:
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for Replica B agent+runtime stopped reports to reach UpdateAgentStatus")
			}
			return nil
		},
	}
	srvA.SetDispatcher(dispA)
	fault.Arm()

	srvA.agentIdleSuspendHandler()(ctx)
	srvA.waitAutoSuspendIdle()
	reportWG.Wait()
	raceStore.bypass.Store(true)

	// Also deliver a fresh post-clear runtime stopped report directly on Replica B.
	exitZero := 0
	rec := doRequest(t, srvB, http.MethodPost, "/api/v1/runtime-brokers/"+target.RuntimeBrokerID+"/heartbeat",
		brokerHeartbeatRequest{
			Status: store.BrokerStatusOnline,
			Projects: []brokerProjectHeartbeat{{
				ProjectID:  target.ProjectID,
				AgentCount: 1,
				Agents: []brokerAgentHeartbeat{{
					Slug:            target.Slug,
					RunID:           target.RunID,
					Phase:           string(state.PhaseStopped),
					ContainerStatus: "exited (0)",
					ExitCode:        &exitZero,
				}},
			}},
		})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	settled, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), settled.Phase, "late agent+runtime stopped reports must leave phase=suspended intact")
	assert.Equal(t, string(state.SuspendReasonIdle), settled.SuspendReason, "suspend_reason=idle must remain intact")
	assert.Equal(t, string(state.ActivityIdle), settled.Activity, "retained activity=idle must remain intact")
	assert.Nil(t, settled.IdleSuspendClaimUntil)
	assert.True(t, srvB.shouldAutoWake(settled))

	// Incoming DM without --wake auto-wakes the suspended(idle) agent and delivers.
	res, dmErr := srvB.ExecuteAgentDM(ctx, awAgentDMInput(sender, settled, "wake-after-late-reports"))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, int32(1), dispB.starts.Load(), "DM must auto-wake the idle-suspended agent")
	assert.Equal(t, []string{"wake-after-late-reports"}, dispB.deliveredMessages())

	woken, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), woken.Phase)
	assert.Empty(t, woken.SuspendReason)
}
