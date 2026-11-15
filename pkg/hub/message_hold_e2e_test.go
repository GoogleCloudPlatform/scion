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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTmux captures the text payloads pasted by sciontool's
// deliver-message route in execution order.
type recordingTmux struct {
	mu     sync.Mutex
	calls  int
	pasted []string
}

func (r *recordingTmux) Run(_ context.Context, argv []string, stdin io.Reader) (string, error) {
	var data []byte
	if stdin != nil {
		data, _ = io.ReadAll(stdin)
	}
	r.mu.Lock()
	r.calls++
	if slices.Contains(argv, "load-buffer") && len(data) > 0 {
		r.pasted = append(r.pasted, string(data))
	}
	r.mu.Unlock()
	if len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "session=") {
		return "session=1000\nnow=2000\ninitrun=\ninitspec=\nready=\nreadymtime=\n", nil
	}
	return "", nil
}

func (r *recordingTmux) pastedTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pasted)
}

// cloudRunTestRuntime backs agent.NewManager in end-to-end Hub+Broker+Conduit
// tests, exposing runtime.ExecCapability so Cloud Run (ExecAvailable=false)
// and Docker (ExecAvailable=true) take the exact production branches in
// pkg/agent.
type cloudRunTestRuntime struct {
	*runtime.MockRuntime
	execSupported atomic.Bool

	mu       sync.Mutex
	execCmds []string
}

func (r *cloudRunTestRuntime) ExecAvailable() bool {
	return r.execSupported.Load()
}

func (r *cloudRunTestRuntime) recordedExecs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.execCmds)
}

// realBrokerDispatcher wires a real *agent.AgentManager (including its real
// MessageBuffer debounce/coalescing and ConduitDeliverer retry loop) to the
// Hub's /deliver and /message-failures HTTP routes.
type realBrokerDispatcher struct {
	srv      *Server
	st       store.Store
	brokerID string
	rt       *cloudRunTestRuntime
	mgr      *agent.AgentManager

	omitFailureCode atomic.Bool
	labelled        atomic.Bool

	mu              sync.Mutex
	dispatches      map[string]int
	senderNotices   []string
	deliverRequests []agentcontrol.DeliverRequest
	startCalls      int
	beforeDeliver   func(req *agentcontrol.DeliverRequest)
	onStart         func(ctx context.Context, target *store.Agent)
}

func newRealBrokerDispatcher(t *testing.T, srv *Server, st store.Store, brokerID string, runtimeName string, execSupported bool) *realBrokerDispatcher {
	t.Helper()
	d := &realBrokerDispatcher{
		srv:        srv,
		st:         st,
		brokerID:   brokerID,
		dispatches: make(map[string]int),
	}
	d.labelled.Store(true)

	rtName := runtimeName
	rt := &cloudRunTestRuntime{}
	rt.execSupported.Store(execSupported)
	rt.MockRuntime = &runtime.MockRuntime{
		NameFunc: func() string { return rtName },
		ListFunc: func(ctx context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			agents, err := st.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 100})
			if err != nil {
				return nil, err
			}
			var out []api.AgentInfo
			for _, a := range agents.Items {
				if a.RuntimeBrokerID != brokerID {
					continue
				}
				if a.Phase != string(state.PhaseRunning) && a.Phase != string(state.PhaseStarting) {
					continue
				}
				labels := map[string]string{
					"scion.name":       strings.ToLower(a.Slug),
					"scion.project_id": a.ProjectID,
					"agent_id":         a.ID,
				}
				if d.labelled.Load() {
					labels[agent.LabelConduitDeliver] = "true"
				}
				out = append(out, api.AgentInfo{
					ID:          a.ID,
					ContainerID: "c-" + a.ID,
					Name:        a.Slug,
					ProjectID:   a.ProjectID,
					Phase:       string(state.PhaseRunning),
					Labels:      labels,
				})
			}
			return out, nil
		},
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
			if !rt.execSupported.Load() {
				return "", errors.New("exec unsupported on cloudrun")
			}
			rt.mu.Lock()
			rt.execCmds = append(rt.execCmds, strings.Join(cmd, " "))
			rt.mu.Unlock()
			return "", nil
		},
		ExecWithStdinFunc: func(_ context.Context, _ string, cmd []string, stdin io.Reader) (string, error) {
			if !rt.execSupported.Load() {
				return "", errors.New("exec unsupported on cloudrun")
			}
			if stdin != nil {
				_, _ = io.ReadAll(stdin)
			}
			rt.mu.Lock()
			rt.execCmds = append(rt.execCmds, strings.Join(cmd, " "))
			rt.mu.Unlock()
			return "", nil
		},
	}
	d.rt = rt
	d.mgr = agent.NewManager(rt).(*agent.AgentManager)
	d.mgr.SetConduitDeliverer(d)
	t.Cleanup(d.mgr.Close)
	return d
}

func (d *realBrokerDispatcher) DeliverMessage(ctx context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error) {
	d.mu.Lock()
	cp := *req
	cp.MessageIDs = slices.Clone(req.MessageIDs)
	d.deliverRequests = append(d.deliverRequests, cp)
	hook := d.beforeDeliver
	d.mu.Unlock()

	if hook != nil {
		hook(&cp)
	}

	raw, err := json.Marshal(req)
	if err != nil {
		return 0, agentcontrol.DeliverResponse{}, err
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+d.brokerID+"/agents/"+agentID+"/deliver", bytes.NewReader(raw))
	r = r.WithContext(contextWithBrokerIdentity(ctx, NewBrokerIdentity(d.brokerID)))
	rr := httptest.NewRecorder()
	d.srv.handleRuntimeBrokerRoutes(rr, r)
	var resp agentcontrol.DeliverResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp, nil
}

func (d *realBrokerDispatcher) postFailureReport(f hubclient.MessageFailure) messageDeliveryFailuresResponse {
	raw, _ := json.Marshal(hubclient.MessageFailuresReport{Failures: []hubclient.MessageFailure{f}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+d.brokerID+"/message-failures", bytes.NewReader(raw))
	r = r.WithContext(contextWithBrokerIdentity(context.Background(), NewBrokerIdentity(d.brokerID)))
	rr := httptest.NewRecorder()
	d.srv.handleRuntimeBrokerRoutes(rr, r)
	var out messageDeliveryFailuresResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func (d *realBrokerDispatcher) DispatchAgentCreate(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return &CreateDispatchResult{}, nil
}
func (d *realBrokerDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}
func (d *realBrokerDispatcher) DispatchAgentReprovision(context.Context, *store.Agent, ReprovisionOptions) error {
	return nil
}
func (d *realBrokerDispatcher) DispatchAgentStart(ctx context.Context, target *store.Agent, _ string, _ bool) error {
	d.mu.Lock()
	d.startCalls++
	hook := d.onStart
	d.mu.Unlock()
	if hook != nil {
		hook(ctx, target)
		return nil
	}
	// Default: simulate a resumed Cloud Run container posting its initial HTTP
	// readiness heartbeat before its Conduit WebSocket session connects.
	go func(agentID string) {
		time.Sleep(30 * time.Millisecond)
		_ = d.st.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}(target.ID)
	return nil
}
func (d *realBrokerDispatcher) DispatchAgentStop(context.Context, *store.Agent) error    { return nil }
func (d *realBrokerDispatcher) DispatchAgentRestart(context.Context, *store.Agent) error { return nil }
func (d *realBrokerDispatcher) DispatchAgentResetAuth(context.Context, *store.Agent) error {
	return nil
}
func (d *realBrokerDispatcher) DispatchAgentDelete(context.Context, *store.Agent, bool, bool, bool, time.Time) error {
	return nil
}
func (d *realBrokerDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", nil
}
func (d *realBrokerDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, nil
}
func (d *realBrokerDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	return true, nil
}
func (d *realBrokerDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return &CreateDispatchResult{}, nil
}
func (d *realBrokerDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*CreateDispatchResult, error) {
	return &CreateDispatchResult{}, nil
}

func (d *realBrokerDispatcher) DispatchAgentMessage(ctx context.Context, target *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	var deliveryText string
	if sm != nil && sm.DeliveryText != "" {
		deliveryText = sm.DeliveryText
	} else if sm != nil {
		deliveryText = messages.FormatForDelivery(sm)
	} else {
		deliveryText = message
	}

	// Sender agent (f.agent, assigned to broker-1) receives system failure
	// notices here so tests can assert exact notice counts and text.
	if target.RuntimeBrokerID != d.brokerID {
		d.mu.Lock()
		d.dispatches[target.Slug]++
		if sm != nil && sm.Type == messages.TypeSystem {
			d.senderNotices = append(d.senderNotices, sm.Msg)
		}
		d.mu.Unlock()
		return nil
	}

	d.mu.Lock()
	d.dispatches[target.Slug]++
	d.mu.Unlock()

	msgID := dispatchMessageIDFromContext(ctx)
	msgCtx := agent.WithDeliveryMessageID(ctx, msgID)
	if !interrupt && msgID != "" {
		msgCtx = agent.WithDeliveryFailureHandler(msgCtx, func(deliveryErr error) {
			f := hubclient.MessageFailure{
				MessageID: msgID,
				AgentID:   target.Slug,
				ProjectID: target.ProjectID,
				Reason:    "broker delivery failed: " + deliveryErr.Error(),
			}
			if errors.Is(deliveryErr, agent.ErrAwaitingSession) && !d.omitFailureCode.Load() {
				f.Code = hubclient.MessageFailureCodeAwaitingSession
			}
			go d.postFailureReport(f)
		})
	}

	if err := d.mgr.Message(msgCtx, target.Slug, target.ProjectID, deliveryText, interrupt); err != nil {
		if interrupt && msgID != "" && errors.Is(err, agent.ErrAwaitingSession) && !d.omitFailureCode.Load() {
			out := d.postFailureReport(hubclient.MessageFailure{
				MessageID: msgID,
				AgentID:   target.Slug,
				ProjectID: target.ProjectID,
				Code:      hubclient.MessageFailureCodeAwaitingSession,
				Reason:    "broker delivery failed: " + err.Error(),
			})
			if out.Held > 0 || out.Marked > 0 {
				return nil
			}
		}
		return err
	}
	return nil
}

func (d *realBrokerDispatcher) dispatchCount(slug string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dispatches[slug]
}

func (d *realBrokerDispatcher) resetDispatchCount(slug string) {
	d.mu.Lock()
	d.dispatches[slug] = 0
	d.mu.Unlock()
}

func (d *realBrokerDispatcher) notices() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.senderNotices)
}

func (d *realBrokerDispatcher) deliveries() []agentcontrol.DeliverRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.deliverRequests)
}

type holdE2EFixture struct {
	*ptyConduitFixture
	broker string
	disp   *realBrokerDispatcher
	fault  *conduitFaultStore
}

func setHoldTestSettings(t *testing.T, srv *Server, exps map[string]bool) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"overrides": exps})
	require.NoError(t, err)
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", raw)
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":true}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.True(t, srv.GetOperationalSettings().ConversationEnvelopeSwitch())
}

func newHoldE2EFixture(t *testing.T) *holdE2EFixture {
	t.Helper()
	var fs *conduitFaultStore
	pf := newPTYConduitFixtureServer(t, func(srv *Server) {
		fs, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *conduitFaultStore {
			return &conduitFaultStore{Store: inner}
		})
		if srv.authzService != nil {
			srv.authzService.store = fs
		}
	})
	f := &holdE2EFixture{
		ptyConduitFixture: pf,
		broker:            pf.launched.RuntimeBrokerID,
		fault:             fs,
	}
	ctx := context.Background()
	f.agent.Phase = string(state.PhaseRunning)
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))
	f.launched.Runtime = "cloudrun"
	f.launched.Phase = string(state.PhaseRunning)
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = got

	setHoldTestSettings(t, f.srv, map[string]bool{
		conduitExperiment:             true,
		experiments.ConduitDeliver:    true,
		experiments.ChatScheduledSend: true,
	})

	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	f.srv.SetEventPublisher(ep)

	f.disp = newRealBrokerDispatcher(t, f.srv, f.store, f.broker, "cloudrun", false)
	f.srv.SetDispatcher(f.disp)

	eb := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = eb.Close() })
	f.srv.StartMessageBroker(eb)
	t.Cleanup(func() {
		if p := f.srv.GetMessageBrokerProxy(); p != nil {
			p.Stop()
		}
	})
	return f
}

// setHeldMessagesCapForTest overrides the per-agent held-message cap for
// multi-batch (>100 rows) tests such as AC A2-3b's 150-message batch test.
func (s *Server) setHeldMessagesCapForTest(cap int) {
	rt := s.holdRuntime()
	rt.mu.Lock()
	rt.maxHeldOverride = cap
	rt.mu.Unlock()
}

type connectableControlAgent struct {
	tmux   *recordingTmux
	cancel context.CancelFunc
	exited chan struct{}
}

func (c *connectableControlAgent) stop(t *testing.T) {
	t.Helper()
	c.cancel()
	select {
	case <-c.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit control agent did not exit")
	}
}

func (f *holdE2EFixture) connectDeliverSession(t *testing.T, hubURL string, advertise bool, wrap func(conduit.RPCHandler) conduit.RPCHandler) *connectableControlAgent {
	t.Helper()
	guardSciontoolLog()
	fresh, err := f.store.GetAgent(context.Background(), f.launched.ID)
	require.NoError(t, err)
	f.launched = fresh

	tm := &recordingTmux{}
	var deliverOpts *control.DeliverOptions
	if advertise {
		deliverOpts = &control.DeliverOptions{Runner: tm}
	}
	ctl := control.New(control.Options{KickTokenRefresh: func() {}, Deliver: deliverOpts})
	h := control.RPCHandler(ctl)
	if wrap != nil {
		h = wrap(h)
	}
	var routes []string
	if advertise {
		routes = ctl.Routes()
	}
	tok := f.agentToken(t, f.launched)
	admitted := make(chan *conduitv1.Welcome, 4)
	a, err := sconduit.New(sconduit.Options{
		HubURL:    hubURL,
		AgentID:   f.launched.ID,
		ProjectID: f.launched.ProjectID,
		LaunchID:  f.launched.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(f.clock.Now()), now: f.clock.Now},
		NoPTY:     true,
		Session:   conduit.Config{RPCHandler: h},
		RPCRoutes: routes,
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

func (f *holdE2EFixture) waitMessageState(t *testing.T, msgID, want string) *store.Message {
	t.Helper()
	var got *store.Message
	if !assert.Eventually(t, func() bool {
		m, err := f.store.GetMessage(context.Background(), msgID)
		if err != nil {
			return false
		}
		got = m
		return m.DispatchState == want
	}, 12*time.Second, 25*time.Millisecond) {
		t.Fatalf("message %s did not reach state %q (last=%+v)", msgID, want, got)
	}
	return got
}

func (f *holdE2EFixture) seedHeldMessage(t *testing.T, sender string, senderID string, body string, holdUntil time.Time) *store.Message {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	m := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     f.launched.ProjectID,
		AgentID:       f.launched.ID,
		Sender:        sender,
		SenderID:      senderID,
		Recipient:     "agent:" + f.launched.Slug,
		RecipientID:   f.launched.ID,
		Msg:           body,
		Type:          messages.TypeInstruction,
		DispatchState: store.MessageDispatchDispatched,
		HoldLaunchID:  f.launched.RunID,
		CreatedAt:     now,
	}
	require.NoError(t, f.store.CreateMessage(ctx, m))
	deliveryID := "seed-" + m.ID
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, deliveryID, []string{m.ID}, now.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, deliveryID, []string{m.ID}, agentcontrol.ReasonNoSession))

	heldOk, err := f.store.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m.ID,
		AgentID:     f.launched.ID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     holdUntil.Sub(now),
		MaxHeld:     f.srv.heldMessagesCap(),
	})
	require.NoError(t, err)
	require.True(t, heldOk)
	got, err := f.store.GetMessage(ctx, m.ID)
	require.NoError(t, err)
	return got
}

func (f *holdE2EFixture) postReport(t *testing.T, mf hubclient.MessageFailure) messageDeliveryFailuresResponse {
	t.Helper()
	return f.disp.postFailureReport(mf)
}

// ---------------------------------------------------------------------------
// AC A2-1: No-exec Cloud Run startup hold -> OnAgentSessionAdmitted re-drive
// ---------------------------------------------------------------------------

func TestHoldA2_1_NoExecStartupHoldAndSessionAdmissionRedrive(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	// Agent container is running on no-exec Cloud Run before its Conduit session connects.
	senderIdent := &storedAgentIdentity{agent: f.agent}
	res, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: senderIdent,
		TargetAgent:    f.launched,
		ProjectID:      f.launched.ProjectID,
		Msg:            "startup instruction before conduit session",
		Type:           messages.TypeInstruction,
	})
	require.Nil(t, dmErr)
	require.NotEmpty(t, res.MessageID)

	// Broker attempts /deliver -> Hub 503 no_session (stamps proof) -> broker
	// reports code=awaiting_session -> Hub holds row in awaiting_session.
	held := f.waitMessageState(t, res.MessageID, store.MessageDispatchAwaitingSession)
	require.NotNil(t, held.HoldUntil)
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), *held.HoldUntil, 30*time.Second)
	assert.Equal(t, f.launched.RunID, held.HoldLaunchID)
	assert.Empty(t, held.NotReached, "no_session proof is consumed when entering awaiting_session")
	assert.Nil(t, held.NotReachedAt)
	hasHold, err := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
	require.NoError(t, err)
	assert.True(t, hasHold)
	assert.Empty(t, f.disp.notices(), "sender must NOT receive a failure notice while held")

	// Agent finishes connecting its Conduit session advertising deliver-message.
	ca := f.connectDeliverSession(t, f.public.URL, true, nil)

	// Session admission triggers immediate re-drive -> bind delivering -> delivered.
	delivered := f.waitMessageState(t, res.MessageID, store.MessageDispatchDelivered)
	assert.Equal(t, store.MessageDispatchDelivered, delivered.DispatchState)

	pasted := ca.tmux.pastedTexts()
	require.Len(t, pasted, 1)
	assert.Contains(t, pasted[0], "startup instruction before conduit session")
	env1 := decodeHoldTestEnvelope(t, pasted[0])
	assert.Equal(t, res.MessageID, env1["message_id"], "re-driven message must carry stored message_id in Scion envelope")
	assert.Equal(t, "agent:"+f.agent.Slug, env1["from"], "re-driven message must preserve sender attribution")
	assert.Empty(t, f.disp.notices(), "sender receives zero failure notices")

	require.Eventually(t, func() bool {
		ok, _ := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
		return !ok
	}, 3*time.Second, 20*time.Millisecond, "hold marker must clear once last held row drains")
}

// ---------------------------------------------------------------------------
// AC A2-2: Non-hold failure reasons and forged/stale proofs never hold
// ---------------------------------------------------------------------------

func TestHoldA2_2_RefusesHoldWithoutFreshHubNoSessionProof(t *testing.T) {
	t.Run("connected_session_without_deliver_message_fails_not_held", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		f.connectDeliverSession(t, f.public.URL, false, nil) // no deliver-message route

		m := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "hello", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, f.store.CreateMessage(ctx, m))

		// /deliver answers 503 not_advertised, unbinding WITHOUT stamping no_session_at.
		status, resp, err := f.disp.DeliverMessage(ctx, f.launched.ID, &agentcontrol.DeliverRequest{
			V: 1, DeliveryID: "d-not-adv", MessageIDs: []string{m.ID},
			Op: agentcontrol.OpPaste, Text: ptrString("hello"), DeadlineMS: 5000, SubmitKeys: []string{"Enter"},
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Equal(t, agentcontrol.ReasonNotAdvertised, resp.Reason)

		// Even if a faulty broker reports code=awaiting_session, Hub refuses to hold.
		out := f.postReport(t, hubclient.MessageFailure{
			MessageID: m.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
			Code: hubclient.MessageFailureCodeAwaitingSession, Reason: "broker claims awaiting_session",
		})
		assert.Equal(t, 0, out.Held)
		assert.Equal(t, 1, out.Marked)
		got, err := f.store.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	})

	t.Run("spoofed_agent_503_no_session_becomes_partial_never_held", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		f.connectDeliverSession(t, f.public.URL, true, func(conduit.RPCHandler) conduit.RPCHandler {
			return conduit.RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				return &conduitv1.RpcResponse{
					Status: http.StatusServiceUnavailable,
					Body:   []byte(`{"outcome":"not_reached","reason":"no_session"}`),
				}
			})
		})

		m := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "hello", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, f.store.CreateMessage(ctx, m))

		status, resp, err := f.disp.DeliverMessage(ctx, f.launched.ID, &agentcontrol.DeliverRequest{
			V: 1, DeliveryID: "d-spoof", MessageIDs: []string{m.ID},
			Op: agentcontrol.OpPaste, Text: ptrString("hello"), DeadlineMS: 5000, SubmitKeys: []string{"Enter"},
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Empty(t, resp.Reason, "A2-S6: agent-sent reason must be stripped by Hub")

		afterDeliver, err := f.store.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, afterDeliver.DispatchState)
		assert.Empty(t, afterDeliver.NotReached)
		assert.Nil(t, afterDeliver.NotReachedAt)

		out := f.postReport(t, hubclient.MessageFailure{
			MessageID: m.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
			Code: hubclient.MessageFailureCodeAwaitingSession, Reason: "spoofed",
		})
		assert.Equal(t, 0, out.Held)
		assert.Equal(t, 1, out.Marked)
		got, err := f.store.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	})

	t.Run("unstamped_dispatched_delivering_and_stale_proof_never_hold", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		// 1. Unstamped dispatched row (e.g. exec-delivered or fabricated report).
		unstamped := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "unstamped", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, f.store.CreateMessage(ctx, unstamped))
		out := f.postReport(t, hubclient.MessageFailure{
			MessageID: unstamped.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
			Code: hubclient.MessageFailureCodeAwaitingSession, Reason: "no proof",
		})
		assert.Equal(t, 0, out.Held)
		assert.Equal(t, 1, out.Marked)

		// 2. Currently bound delivering row.
		bound := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "bound", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, f.store.CreateMessage(ctx, bound))
		_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d-live", []string{bound.ID}, time.Now().Add(time.Minute))
		require.NoError(t, err)
		out = f.postReport(t, hubclient.MessageFailure{
			MessageID: bound.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
			Code: hubclient.MessageFailureCodeAwaitingSession, Reason: "live delivering",
		})
		assert.Equal(t, 0, out.Held)
		assert.Equal(t, 1, out.Marked)
		gotBound, err := f.store.GetMessage(ctx, bound.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, gotBound.DispatchState, "refused hold on delivering row must fall through to failed, never awaiting_session")

		// 3. Stale no_session proof (> MaxProofAge).
		stale := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "stale", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, f.store.CreateMessage(ctx, stale))
		_, err = f.store.BindMessageDelivery(ctx, f.launched.ID, "d-stale", []string{stale.ID}, time.Now().Add(time.Minute))
		require.NoError(t, err)
		require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, "d-stale", []string{stale.ID}, agentcontrol.ReasonNoSession))
		time.Sleep(30 * time.Millisecond)
		heldStale, err := f.store.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
			MessageID: stale.ID, AgentID: f.launched.ID,
			MaxProofAge: 5 * time.Millisecond, HoldTTL: 10 * time.Minute, MaxHeld: 100,
		})
		require.NoError(t, err)
		assert.False(t, heldStale)
	})
}

// ---------------------------------------------------------------------------
// AC A2-3: Active hold queues new sends across all writers & demotes interrupt
// ---------------------------------------------------------------------------

func TestHoldA2_3_ActiveHoldQueuesAllWritersAndDemotesInterrupt(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	// Seed m1 in awaiting_session (establishes AgentDeliveryHold marker).
	m1 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "msg-1-first", time.Now().Add(10*time.Minute))
	f.disp.resetDispatchCount(f.launched.Slug)

	// 1. Urgent/interrupt DM via ExecuteAgentDM.
	dmRes, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    f.launched,
		ProjectID:      f.launched.ProjectID,
		Msg:            "msg-2-urgent-dm",
		Type:           messages.TypeInstruction,
		Urgent:         true,
	})
	require.Nil(t, dmErr)

	// 2. User HTTP POST /api/v1/agents/{id}/message with interrupt=true.
	rawBody, _ := json.Marshal(map[string]any{"message": "msg-3-user-http", "interrupt": true})
	httpReq, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/agents/"+f.launched.ID+"/message", bytes.NewReader(rawBody))
	require.NoError(t, err)
	httpReq.Header.Set("Authorization", "Bearer "+f.userToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	_ = httpResp.Body.Close()
	require.Equal(t, http.StatusOK, httpResp.StatusCode)

	// 3. Broker-inbound POST /api/v1/broker/inbound with urgent=true.
	ownerUser, err := f.store.GetUser(ctx, f.launched.OwnerID)
	require.NoError(t, err)
	inboundBody, _ := json.Marshal(inboundMessageRequest{
		Topic: "scion.project." + f.launched.ProjectID + ".agent." + f.launched.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Sender:    "user:" + ownerUser.Email,
			SenderID:  ownerUser.ID,
			Recipient: "agent:" + f.launched.Slug,
			Msg:       "msg-4-broker-inbound",
			Type:      messages.TypeInstruction,
			Urgent:    true,
		},
	})
	inReq := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(inboundBody))
	inReq = inReq.WithContext(contextWithBrokerIdentity(ctx, NewBrokerIdentity(f.broker)))
	inRR := httptest.NewRecorder()
	f.srv.handleBrokerInbound(inRR, inReq)
	require.Equal(t, http.StatusOK, inRR.Code, inRR.Body.String())

	// 4. Scheduled message fire via server.go:messageEventHandler (Q5=a).
	schedPayload, _ := json.Marshal(MessageEventPayload{AgentName: f.launched.Slug, Message: "msg-5-scheduler"})
	schedEvt := withAgentRevision(t, f.srv, store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: f.launched.ProjectID, EventType: "message",
		Payload: string(schedPayload), Status: store.ScheduledEventPending, CreatedBy: f.agent.ID,
	}, f.agent.ID)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &schedEvt))
	require.NoError(t, f.srv.messageEventHandler()(ctx, schedEvt))

	// Zero broker dispatches occurred while the hold marker was active!
	assert.Equal(t, 0, f.disp.dispatchCount(f.launched.Slug), "holdOrDispatch must queue behind active hold without calling broker")

	listed, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.launched.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	require.Len(t, listed.Items, 5)
	for _, item := range listed.Items {
		assert.Equal(t, store.MessageDispatchAwaitingSession, item.DispatchState)
	}

	// Connect session: all 5 re-drive in creation order and coalesce as buffered
	// op=paste (interrupt demoted to buffered so urgent messages never overtake).
	ca := f.connectDeliverSession(t, f.public.URL, true, nil)
	f.waitMessageState(t, m1.ID, store.MessageDispatchDelivered)
	f.waitMessageState(t, dmRes.MessageID, store.MessageDispatchDelivered)
	for _, item := range listed.Items {
		f.waitMessageState(t, item.ID, store.MessageDispatchDelivered)
	}

	deliveries := f.disp.deliveries()
	require.NotEmpty(t, deliveries)
	for _, dr := range deliveries {
		assert.Equal(t, agentcontrol.OpPaste, dr.Op, "re-driven messages must use buffered op=paste, never interrupt_paste")
	}

	fullPasted := strings.Join(ca.tmux.pastedTexts(), "\n\n")
	idx1 := strings.Index(fullPasted, "msg-1-first")
	idx2 := strings.Index(fullPasted, "msg-2-urgent-dm")
	idx3 := strings.Index(fullPasted, "msg-3-user-http")
	idx4 := strings.Index(fullPasted, "msg-4-broker-inbound")
	idx5 := strings.Index(fullPasted, "msg-5-scheduler")
	require.True(t, idx1 >= 0 && idx2 > idx1 && idx3 > idx2 && idx4 > idx3 && idx5 > idx4,
		"pasted text must preserve exact creation order across all 5 writers: %q", fullPasted)
}

// ---------------------------------------------------------------------------
// AC A2-3b: Ordered 64-row batching and MessageBuffer coalescing
// ---------------------------------------------------------------------------

func TestHoldA2_3b_OrderedBatchesAndCoalescing(t *testing.T) {
	t.Run("30_held_messages_coalesce_into_single_deliver_flush", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		var expectedIDs []string
		for i := 0; i < 30; i++ {
			m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "batch30-"+uuid.NewString()[:8], time.Now().Add(10*time.Minute))
			expectedIDs = append(expectedIDs, m.ID)
		}

		f.connectDeliverSession(t, f.public.URL, true, nil)
		for _, id := range expectedIDs {
			f.waitMessageState(t, id, store.MessageDispatchDelivered)
		}

		deliveries := f.disp.deliveries()
		require.Len(t, deliveries, 1, "30 held messages within route limits must coalesce into 1 /deliver flush")
		assert.Equal(t, expectedIDs, deliveries[0].MessageIDs)
	})

	t.Run("100_held_messages_64_plus_36_waits_for_batch_1_bind", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		var expectedIDs []string
		for i := 0; i < 100; i++ {
			m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "b100", time.Now().Add(10*time.Minute))
			expectedIDs = append(expectedIDs, m.ID)
		}

		batch1Entered := make(chan struct{})
		releaseBatch1 := make(chan struct{})
		var once sync.Once
		f.disp.mu.Lock()
		f.disp.beforeDeliver = func(req *agentcontrol.DeliverRequest) {
			once.Do(func() {
				close(batch1Entered)
				<-releaseBatch1
			})
		}
		f.disp.mu.Unlock()

		f.connectDeliverSession(t, f.public.URL, true, nil)

		select {
		case <-batch1Entered:
		case <-time.After(10 * time.Second):
			t.Fatal("batch 1 never entered /deliver")
		}
		// While batch 1 has not yet bound to delivering, batch 2 (36 rows) must
		// NOT have been dispatched to the broker.
		time.Sleep(80 * time.Millisecond)
		assert.Equal(t, 64, f.disp.dispatchCount(f.launched.Slug), "batch 2 must wait until batch 1 binds/settles")
		close(releaseBatch1)

		for _, id := range expectedIDs {
			f.waitMessageState(t, id, store.MessageDispatchDelivered)
		}

		deliveries := f.disp.deliveries()
		require.Len(t, deliveries, 2, "100 held messages must flush in 2 batches (64 + 36)")
		assert.Equal(t, expectedIDs[:64], deliveries[0].MessageIDs)
		assert.Equal(t, expectedIDs[64:], deliveries[1].MessageIDs)
	})

	t.Run("150_held_messages_64_plus_64_plus_22_with_raised_cap", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		f.srv.setHeldMessagesCapForTest(200)

		var expectedIDs []string
		for i := 0; i < 150; i++ {
			m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "b150", time.Now().Add(10*time.Minute))
			expectedIDs = append(expectedIDs, m.ID)
		}

		f.connectDeliverSession(t, f.public.URL, true, nil)
		for _, id := range expectedIDs {
			f.waitMessageState(t, id, store.MessageDispatchDelivered)
		}

		deliveries := f.disp.deliveries()
		require.Len(t, deliveries, 3, "150 held messages must flush in 3 batches (64 + 64 + 22)")
		assert.Equal(t, expectedIDs[:64], deliveries[0].MessageIDs)
		assert.Equal(t, expectedIDs[64:128], deliveries[1].MessageIDs)
		assert.Equal(t, expectedIDs[128:], deliveries[2].MessageIDs)
	})
}

// ---------------------------------------------------------------------------
// AC A2-4: Multi-replica claim atomicity & peer-relay session sweep (A2-S7)
// ---------------------------------------------------------------------------

func TestHoldA2_4_MultiReplicaClaimAndCrossReplicaSessionSweep(t *testing.T) {
	t.Run("concurrent_replicas_and_sweep_deliver_each_row_once", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		// Build a second Hub replica sharing the database, registry, and broker.
		srvB, err := New(ServerConfig{}, f.store)
		require.NoError(t, err)
		srvB.conduitGrants = f.srv.conduitGrants
		setHoldTestSettings(t, srvB, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: true})
		srvB.SetDispatcher(f.disp)
		authB, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: f.peerSecret, SelfID: "hub-b-srv"})
		require.NoError(t, err)
		require.NoError(t, srvB.StartConduitRelay(ctx, ConduitRelayOptions{
			InstanceID: "hub-b-srv", PeerAuth: authB, Store: f.regStore, Registry: f.reg,
			Clock: clock.NewFake(f.clock.Now()), sessionWriteWait: conduitFixtureWriteWait,
		}))
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srvB.shutdownConduitRelay(sctx)
		})

		var ids []string
		for i := 0; i < 20; i++ {
			m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "multi-rep", time.Now().Add(10*time.Minute))
			ids = append(ids, m.ID)
		}

		f.connectDeliverSession(t, f.public.URL, true, nil)

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); f.srv.redriveAgentHeldMessagesOnce(ctx, f.launched.ID) }()
		go func() { defer wg.Done(); srvB.redriveAgentHeldMessagesOnce(ctx, f.launched.ID) }()
		go func() { defer wg.Done(); f.srv.sweepHeldMessages(ctx) }()
		wg.Wait()

		for _, id := range ids {
			f.waitMessageState(t, id, store.MessageDispatchDelivered)
		}
		assert.Equal(t, 20, f.disp.dispatchCount(f.launched.Slug), "each held message must be claimed and dispatched once across replicas")
		assert.Empty(t, f.disp.notices())
	})

	t.Run("sweep_discovers_session_admitted_on_peer_relay_hub_b", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "peer-relay-msg", time.Now().Add(10*time.Minute))

		// Connect session to peer relay hub-b: hub-a's local OnAgentSessionAdmitted
		// does NOT fire, so the row stays awaiting_session until hub-a's sweep
		// queries the cluster-wide session registry.
		peerURL := f.startPeerRelay(t, "hub-b")
		ca := f.connectDeliverSession(t, peerURL, true, nil)

		got, err := f.store.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)

		f.srv.sweepHeldMessages(ctx)
		f.waitMessageState(t, m.ID, store.MessageDispatchDelivered)
		require.Len(t, ca.tmux.pastedTexts(), 1)
		assert.Contains(t, ca.tmux.pastedTexts()[0], "peer-relay-msg")
	})
}

// ---------------------------------------------------------------------------
// AC A2-5: Expiry, stop, missing route, launch mismatch, authz re-validation,
// session drop (original hold_until), transient fault R3, corrupt row A2-S1
// ---------------------------------------------------------------------------

func TestHoldA2_5_FailurePathsRevalidationAndTransientFaultRehold(t *testing.T) {
	t.Run("ttl_expiry_marks_failed_and_notifies_sender", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "ttl-expired", time.Now().Add(25*time.Millisecond))
		time.Sleep(45 * time.Millisecond)

		f.srv.sweepHeldMessages(ctx)
		got := f.waitMessageState(t, m.ID, store.MessageDispatchFailed)
		require.NotNil(t, got.DispatchFailureReason)
		assert.Equal(t, holdFailReasonSessionTimeout, *got.DispatchFailureReason)
		require.Len(t, f.disp.notices(), 1)
		assert.Contains(t, f.disp.notices()[0], holdFailReasonSessionTimeout)

		hasHold, err := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
		require.NoError(t, err)
		assert.False(t, hasHold)
	})

	t.Run("event_driven_stop_error_status_and_membership_hold_fail_immediately", func(t *testing.T) {
		// 1. User stop HTTP handler expires held rows immediately without sweep.
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		mStop := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "stopped-msg", time.Now().Add(10*time.Minute))

		stopReq, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/agents/"+f.launched.ID+"/stop", nil)
		require.NoError(t, err)
		stopReq.Header.Set("Authorization", "Bearer "+f.userToken)
		stopResp, err := http.DefaultClient.Do(stopReq)
		require.NoError(t, err)
		_ = stopResp.Body.Close()
		require.Equal(t, http.StatusOK, stopResp.StatusCode)

		gotStop := f.waitMessageState(t, mStop.ID, store.MessageDispatchFailed)
		require.NotNil(t, gotStop.DispatchFailureReason)
		assert.Equal(t, holdFailReasonAgentStopped, *gotStop.DispatchFailureReason)
		hasHold, _ := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
		assert.False(t, hasHold)

		// 2. Agent error status update expires held rows immediately.
		f2 := newHoldE2EFixture(t)
		mErr := f2.seedHeldMessage(t, "agent:"+f2.agent.Slug, f2.agent.ID, "err-status", time.Now().Add(10*time.Minute))
		statusBody, _ := json.Marshal(map[string]any{"phase": string(state.PhaseError), "message": "container crashed"})
		stReq, err := http.NewRequest(http.MethodPost, f2.public.URL+"/api/v1/agents/"+f2.launched.ID+"/status", bytes.NewReader(statusBody))
		require.NoError(t, err)
		stReq.Header.Set("Authorization", "Bearer "+f2.userToken)
		stReq.Header.Set("Content-Type", "application/json")
		stResp, err := http.DefaultClient.Do(stReq)
		require.NoError(t, err)
		_ = stResp.Body.Close()
		require.Equal(t, http.StatusOK, stResp.StatusCode)

		gotErr := f2.waitMessageState(t, mErr.ID, store.MessageDispatchFailed)
		assert.Equal(t, holdFailReasonAgentStopped, *gotErr.DispatchFailureReason)

		// 3. Membership-loss hold expires held rows immediately via membershipLossRound.
		f3 := newHoldE2EFixture(t)
		mHold := f3.seedHeldMessage(t, "agent:"+f3.agent.Slug, f3.agent.ID, "member-loss", time.Now().Add(10*time.Minute))
		ownerRBs, err := f3.store.ListRoleBindingsForPrincipals(ctx,
			[]store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: f3.launched.OwnerID}},
			[]string{store.RoleScopeProject}, []string{f3.launched.ProjectID})
		require.NoError(t, err)
		for _, rb := range ownerRBs {
			require.NoError(t, f3.store.DeleteRoleBinding(ctx, rb.ID))
		}
		require.NoError(t, f3.srv.processMembershipLossCheck(ctx, &store.MembershipLossCheck{
			ID:        uuid.NewString(),
			UserID:    f3.launched.OwnerID,
			ProjectID: f3.launched.ProjectID,
			Trigger:   store.MembershipLossTriggerMemberRemove,
		}))
		gotHold := f3.waitMessageState(t, mHold.ID, store.MessageDispatchFailed)
		assert.Equal(t, holdFailReasonAgentStopped, *gotHold.DispatchFailureReason)
	})

	t.Run("session_without_deliver_message_fails_held_rows", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "no-route", time.Now().Add(10*time.Minute))

		f.connectDeliverSession(t, f.public.URL, false, nil)
		got := f.waitMessageState(t, m.ID, store.MessageDispatchFailed)
		require.NotNil(t, got.DispatchFailureReason)
		assert.Equal(t, holdFailReasonCannotReceiveConduit, *got.DispatchFailureReason)
		require.Len(t, f.disp.notices(), 1)
		assert.Contains(t, f.disp.notices()[0], holdFailReasonCannotReceiveConduit)
	})

	t.Run("launch_id_mismatch_and_empty_hold_launch_id_fail_closed", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		mMismatch := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "old-run", time.Now().Add(10*time.Minute))

		// Restart/rotate RunID on the agent before session connects.
		_, err := f.store.SetAgentRunID(ctx, f.launched.ID, uuid.NewString(), nil)
		require.NoError(t, err)
		f.launched, err = f.store.GetAgent(ctx, f.launched.ID)
		require.NoError(t, err)

		// Also insert a row with empty HoldLaunchID (R2: must never match!).
		mEmptyLaunch := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "empty-launch", Type: messages.TypeInstruction,
			DispatchState: store.MessageDispatchAwaitingSession,
			HoldUntil:     ptrTime(time.Now().Add(10 * time.Minute)),
			HoldLaunchID:  "",
		}
		require.NoError(t, f.store.CreateMessage(ctx, mEmptyLaunch))

		f.connectDeliverSession(t, f.public.URL, true, nil)

		got1 := f.waitMessageState(t, mMismatch.ID, store.MessageDispatchFailed)
		assert.Equal(t, holdFailReasonAgentRestarted, *got1.DispatchFailureReason)
		got2 := f.waitMessageState(t, mEmptyLaunch.ID, store.MessageDispatchFailed)
		assert.Equal(t, holdFailReasonAgentRestarted, *got2.DispatchFailureReason)
	})

	t.Run("sender_revalidation_denials_user_agent_delegation_and_role_loss", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		// 1. User who gets suspended before re-drive.
		suspUserID := tid("hold-susp-user")
		createTestUserWithProjectRole(t, f.store, suspUserID, "susp@conduit.test", f.launched.ProjectID, store.ProjectRoleMember)
		ensureHubMembership(ctx, f.store, suspUserID)
		mSuspUser := f.seedHeldMessage(t, "user:"+suspUserID, suspUserID, "from-susp", time.Now().Add(10*time.Minute))
		suspUser, err := f.store.GetUser(ctx, suspUserID)
		require.NoError(t, err)
		suspUser.Status = "suspended"
		require.NoError(t, f.store.UpdateUser(ctx, suspUser))

		// 2. Member user removed from project before re-drive.
		remUserID := tid("hold-rem-user")
		createTestUserWithProjectRole(t, f.store, remUserID, "rem@conduit.test", f.launched.ProjectID, store.ProjectRoleMember)
		ensureHubMembership(ctx, f.store, remUserID)
		mRemUser := f.seedHeldMessage(t, "user:"+remUserID, remUserID, "from-rem", time.Now().Add(10*time.Minute))
		// Revoke all role bindings for remUserID.
		rbs, err := f.store.ListRoleBindingsForPrincipals(ctx,
			[]store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: remUserID}},
			[]string{store.RoleScopeProject}, []string{f.launched.ProjectID})
		require.NoError(t, err)
		for _, rb := range rbs {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, rb.ID))
		}

		// 3. Soft-deleted sender agent.
		delAgent := &store.Agent{
			ID: tid("hold-del-sender"), Slug: "del-sender", Name: "Del Sender",
			ProjectID: f.launched.ProjectID, OwnerID: f.launched.OwnerID,
			RuntimeBrokerID: "broker-1", Phase: string(state.PhaseStopped),
			DeletedAt: time.Now().UTC(),
		}
		require.NoError(t, f.store.CreateAgent(ctx, delAgent))
		mDelAgent := f.seedHeldMessage(t, "agent:"+delAgent.Slug, delAgent.ID, "from-del-agent", time.Now().Add(10*time.Minute))

		// 4. Delegated message whose DelegationGrant was revoked before re-drive.
		mDeleg := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "from-revoked-grant", time.Now().Add(10*time.Minute))
		// Persist a row carrying a non-existent/revoked DelegationGrantID directly.
		mDelegRevoked := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			AuthorizingUserID: f.launched.OwnerID, DelegationGrantID: uuid.NewString(),
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "deleg-revoked", Type: messages.TypeInstruction,
			DispatchState: store.MessageDispatchAwaitingSession,
			HoldUntil:     ptrTime(time.Now().Add(10 * time.Minute)),
			HoldLaunchID:  f.launched.RunID,
		}
		require.NoError(t, f.store.CreateMessage(ctx, mDelegRevoked))
		// Remove the unrevoked helper row so we check only the 4 unauthorized ones + 1 valid control.
		_, _ = f.store.FailUndeliveredMessage(ctx, mDeleg.ID, "setup")

		mValid := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "still-authorized", time.Now().Add(10*time.Minute))

		ca := f.connectDeliverSession(t, f.public.URL, true, nil)

		for _, deniedID := range []string{mSuspUser.ID, mRemUser.ID, mDelAgent.ID, mDelegRevoked.ID} {
			got := f.waitMessageState(t, deniedID, store.MessageDispatchFailed)
			require.NotNil(t, got.DispatchFailureReason)
			assert.Equal(t, holdFailReasonSenderUnauthorized, *got.DispatchFailureReason)
		}
		f.waitMessageState(t, mValid.ID, store.MessageDispatchDelivered)
		require.Len(t, ca.tmux.pastedTexts(), 1)
		assert.Contains(t, ca.tmux.pastedTexts()[0], "still-authorized")
	})

	t.Run("session_drop_during_redrive_reholds_with_original_hold_until", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		origHoldUntil := time.Now().UTC().Add(4 * time.Minute).Truncate(time.Millisecond)
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "drop-mid-redrive", origHoldUntil)

		var ca *connectableControlAgent
		var dropOnce sync.Once
		f.disp.mu.Lock()
		f.disp.beforeDeliver = func(*agentcontrol.DeliverRequest) {
			dropOnce.Do(func() {
				// Drop the Conduit session right before /deliver hits the Hub!
				ca.stop(t)
			})
		}
		f.disp.mu.Unlock()

		ca = f.connectDeliverSession(t, f.public.URL, true, nil)

		// Broker's /deliver gets 503 no_session, reports code=awaiting_session,
		// and row returns to awaiting_session with the ORIGINAL HoldUntil!
		reheld := f.waitMessageState(t, m.ID, store.MessageDispatchAwaitingSession)
		require.NotNil(t, reheld.HoldUntil)
		assert.WithinDuration(t, origHoldUntil, *reheld.HoldUntil, 50*time.Millisecond,
			"A2-S3: re-hold after session drop must preserve original hold_until, never reset to now+10m")
	})

	t.Run("transient_lookup_fault_reholds_batch_tail_preserving_order", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		origUntil := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Millisecond)

		// Row 1 is from user f.owner; rows 2 & 3 are from agent f.agent.
		// Injecting failGetUser faults row 1's re-validation with a transient
		// store error (R3). Rows 2 & 3 MUST also be re-held so they never overtake row 1!
		m1 := f.seedHeldMessage(t, "user:"+f.launched.OwnerID, f.launched.OwnerID, "order-1-user", origUntil)
		m2 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "order-2-agent", origUntil)
		m3 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "order-3-agent", origUntil)

		f.fault.failGetUser.Store(true)
		ca := f.connectDeliverSession(t, f.public.URL, true, nil)

		// Wait briefly for the admission re-drive pass to run and re-hold all 3.
		time.Sleep(150 * time.Millisecond)
		assert.Equal(t, 0, f.disp.dispatchCount(f.launched.Slug), "nothing dispatched on transient lookup fault")
		for _, id := range []string{m1.ID, m2.ID, m3.ID} {
			got, err := f.store.GetMessage(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)
			require.NotNil(t, got.HoldUntil)
			assert.WithinDuration(t, origUntil, *got.HoldUntil, 50*time.Millisecond)
		}

		// Clear fault and run sweep: all 3 deliver in exact order 1 -> 2 -> 3!
		f.fault.failGetUser.Store(false)
		f.srv.sweepHeldMessages(ctx)
		for _, id := range []string{m1.ID, m2.ID, m3.ID} {
			f.waitMessageState(t, id, store.MessageDispatchDelivered)
		}
		joined := strings.Join(ca.tmux.pastedTexts(), "\n\n")
		i1 := strings.Index(joined, "order-1-user")
		i2 := strings.Index(joined, "order-2-agent")
		i3 := strings.Index(joined, "order-3-agent")
		require.True(t, i1 >= 0 && i2 > i1 && i3 > i2, "expected 1 -> 2 -> 3 order after fault recovery: %q", joined)
	})

	t.Run("unrecoverable_corrupt_row_fails_closed_never_dispatched_raw", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		// Seed a marker + a row whose Sender is agent:slug (passes authz) but
		// whose Type is an unsupported/corrupt value that storedMessageDispatch rejects!
		_ = f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "valid-control", time.Now().Add(10*time.Minute))
		corrupt := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "RAW_UNWRAPPED_ATTACK", Type: "bogus-corrupt-type",
			DispatchState: store.MessageDispatchAwaitingSession,
			HoldUntil:     ptrTime(time.Now().Add(10 * time.Minute)),
			HoldLaunchID:  f.launched.RunID,
		}
		require.NoError(t, f.store.CreateMessage(ctx, corrupt))

		ca := f.connectDeliverSession(t, f.public.URL, true, nil)
		got := f.waitMessageState(t, corrupt.ID, store.MessageDispatchFailed)
		require.NotNil(t, got.DispatchFailureReason)
		assert.Equal(t, holdFailReasonMessageUnrecoverable, *got.DispatchFailureReason)

		for _, p := range ca.tmux.pastedTexts() {
			assert.NotContains(t, p, "RAW_UNWRAPPED_ATTACK", "corrupt row must NEVER reach the agent")
		}
	})
}

// ---------------------------------------------------------------------------
// AC A2-6: Per-agent cap (A2-S5) and flapping session coalescing
// ---------------------------------------------------------------------------

func TestHoldA2_6_PerAgentCapAndFlappingSessionCoalescing(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()
	f.srv.setHeldMessagesCapForTest(4)

	var heldIDs []string
	for i := 0; i < 4; i++ {
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "cap-msg", time.Now().Add(10*time.Minute))
		heldIDs = append(heldIDs, m.ID)
	}

	// 5th message reporting awaiting_session is refused by cap and marked failed.
	m5 := &store.Message{
		ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
		Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
		Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
		Msg: "over-cap-broker", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
	}
	require.NoError(t, f.store.CreateMessage(ctx, m5))
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d-cap", []string{m5.ID}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, "d-cap", []string{m5.ID}, agentcontrol.ReasonNoSession))
	out := f.postReport(t, hubclient.MessageFailure{
		MessageID: m5.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
		Code: hubclient.MessageFailureCodeAwaitingSession, Reason: "no session",
	})
	assert.Equal(t, 0, out.Held)
	assert.Equal(t, 1, out.Marked)
	got5, err := f.store.GetMessage(ctx, m5.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got5.DispatchState)

	// 5th message via holdOrDispatch bypasses hold queue and dispatches normally to broker!
	f.disp.resetDispatchCount(f.launched.Slug)
	_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    f.launched,
		ProjectID:      f.launched.ProjectID,
		Msg:            "over-cap-live-send",
		Type:           messages.TypeInstruction,
	})
	require.Nil(t, dmErr)
	assert.Equal(t, 1, f.disp.dispatchCount(f.launched.Slug), "at cap, holdOrDispatch falls through to normal broker dispatch")

	// Rapidly fire 25 concurrent admission notifications: singleflight coalesces cleanly.
	f.connectDeliverSession(t, f.public.URL, true, nil)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.srv.onAgentConduitSessionAdmitted(f.launched.ID, []string{agentcontrol.RouteDeliverMessage})
		}()
	}
	wg.Wait()

	for _, id := range heldIDs {
		f.waitMessageState(t, id, store.MessageDispatchDelivered)
	}
}

// ---------------------------------------------------------------------------
// AC A2-7: Exec-capable runtimes (Docker/K8s) fall back to exec without hold
// ---------------------------------------------------------------------------

func TestHoldA2_7_ExecCapableRuntimeUsesExecWithoutHold(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	f.setAgentRuntime(t, "docker")
	dockerDisp := newRealBrokerDispatcher(t, f.srv, f.store, f.broker, "docker", true)
	f.srv.SetDispatcher(dockerDisp)

	res, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    f.launched,
		ProjectID:      f.launched.ProjectID,
		Msg:            "docker-exec-delivery",
		Type:           messages.TypeInstruction,
	})
	require.Nil(t, dmErr)

	require.Eventually(t, func() bool {
		return len(dockerDisp.rt.recordedExecs()) > 0
	}, 6*time.Second, 25*time.Millisecond, "exec-capable runtime must deliver via tmux exec")

	got, err := f.store.GetMessage(ctx, res.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, got.DispatchState)
	hasHold, err := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
	require.NoError(t, err)
	assert.False(t, hasHold)
}

// ---------------------------------------------------------------------------
// AC A2-8: Experiment off and legacy broker (omitting code) fail as Phase A
// ---------------------------------------------------------------------------

func TestHoldA2_8_ExperimentOffAndLegacyBrokerFailWithoutHold(t *testing.T) {
	t.Run("conduit_deliver_experiment_off", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		m := &store.Message{
			ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
			Msg: "exp-off", Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, f.store.CreateMessage(ctx, m))
		_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d-exp", []string{m.ID}, time.Now().Add(time.Minute))
		require.NoError(t, err)
		require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, "d-exp", []string{m.ID}, agentcontrol.ReasonNoSession))

		setHoldTestSettings(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: false})
		out := f.postReport(t, hubclient.MessageFailure{
			MessageID: m.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
			Code: hubclient.MessageFailureCodeAwaitingSession, Reason: "no session",
		})
		assert.Equal(t, 0, out.Held)
		assert.Equal(t, 1, out.Marked)
		got, err := f.store.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	})

	t.Run("legacy_broker_omitting_code_fails_immediately", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		f.disp.omitFailureCode.Store(true)

		res, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
			SenderAgent:    f.agent,
			SenderIdentity: &storedAgentIdentity{agent: f.agent},
			TargetAgent:    f.launched,
			ProjectID:      f.launched.ProjectID,
			Msg:            "legacy-broker-msg",
			Type:           messages.TypeInstruction,
		})
		require.Nil(t, dmErr)

		got := f.waitMessageState(t, res.MessageID, store.MessageDispatchFailed)
		assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
		hasHold, _ := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
		assert.False(t, hasHold)
	})
}

// ---------------------------------------------------------------------------
// AC A2-9: Sweeps, /deliver bind 409, failure report ignore, log privacy, JSON
// ---------------------------------------------------------------------------

func TestHoldA2_9_SweepsDeliverBindFailureReportLogPrivacyAndJSONOmission(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()
	logBuf := capturePTYLogs(t)

	// Connect session before seeding the held row so OnAgentSessionAdmitted has
	// already fired and m stays in awaiting_session while a live session exists.
	f.connectDeliverSession(t, f.public.URL, true, nil)

	const sentinel = "SUPER_SECRET_MESSAGE_BODY_A2_S6_987654"
	m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, sentinel, time.Now().Add(10*time.Minute))

	// 1. Stuck-pending + stale-delivering sweep and reconcileBroker leave awaiting_session untouched.
	f.srv.brokerMessageSweepHandler()(ctx)
	f.srv.reconcileBroker(ctx, f.broker)
	got, err := f.store.GetMessage(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)

	// 2. /deliver bind on an awaiting_session row answers 409 bound even when a session is connected!
	status, resp, err := f.disp.DeliverMessage(ctx, f.launched.ID, &agentcontrol.DeliverRequest{
		V: 1, DeliveryID: "d-409", MessageIDs: []string{m.ID},
		Op: agentcontrol.OpPaste, Text: ptrString("attempt"), DeadlineMS: 5000, SubmitKeys: []string{"Enter"},
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, agentcontrol.OutcomeBound, resp.Outcome)

	// 3. Ordinary broker failure report (code == "") leaves awaiting_session untouched.
	out := f.postReport(t, hubclient.MessageFailure{
		MessageID: m.ID, AgentID: f.launched.Slug, ProjectID: f.launched.ProjectID,
		Code: "", Reason: "stale broker report",
	})
	assert.Equal(t, 1, out.Ignored)
	assert.Equal(t, 0, out.Marked)
	got, err = f.store.GetMessage(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState)

	// 4. Internal hold/proof fields are omitted from JSON serialization (A2-S8).
	marshaled, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(marshaled), "hold_until")
	assert.NotContains(t, string(marshaled), "hold_launch_id")
	assert.NotContains(t, string(marshaled), "not_reached")

	// 5. Force expiry and verify message body NEVER appears in logs (A2-S6).
	f.srv.expireHeldMessagesForAgent(ctx, f.launched.ID, holdFailReasonSessionTimeout)
	f.waitMessageState(t, m.ID, store.MessageDispatchFailed)
	logBuf.mu.Lock()
	allLogs := logBuf.buf.String()
	logBuf.mu.Unlock()
	assert.NotContains(t, allLogs, sentinel, "message body must never be logged on hold/re-drive/expiry paths")
}

// ---------------------------------------------------------------------------
// AC A2-10: End-to-end wake + hold + re-drive on suspended Cloud Run agent
// Covers all 3 wake paths:
//   (a) server.go:messageEventHandler (Wake=true, ptone Q5=a)
//   (b) chat_scheduled_send (Wake=true)
//   (c) ExecuteAgentDM (Wake=true)
// ---------------------------------------------------------------------------

func TestHoldA2_10_SuspendedCloudRunWakeHoldAndRedrive(t *testing.T) {
	t.Run("scheduler_message_event_wake_cloudrun", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		f.launched.Phase = string(state.PhaseSuspended)
		require.NoError(t, f.store.UpdateAgent(ctx, f.launched))

		payload, _ := json.Marshal(MessageEventPayload{
			AgentName: f.launched.Slug,
			Message:   "wake-via-scheduler-event",
			Wake:      true,
		})
		evt := withAgentRevision(t, f.srv, store.ScheduledEvent{
			ID: api.NewUUID(), ProjectID: f.launched.ProjectID, EventType: "message",
			Payload: string(payload), Status: store.ScheduledEventFiring, CreatedBy: f.agent.ID,
		}, f.agent.ID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))

		// messageEventHandler wakes the suspended Cloud Run agent (HTTP status
		// becomes running before Conduit session connects) and dispatches the
		// persisted message row.
		require.NoError(t, f.srv.messageEventHandler()(ctx, evt))

		listed, err := f.store.ListMessages(ctx, store.MessageFilter{AgentID: f.launched.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		msgID := listed.Items[0].ID

		f.waitMessageState(t, msgID, store.MessageDispatchAwaitingSession)

		// Resumed container's Conduit session now connects with its new RunID!
		ca := f.connectDeliverSession(t, f.public.URL, true, nil)
		f.waitMessageState(t, msgID, store.MessageDispatchDelivered)

		pasted := ca.tmux.pastedTexts()
		require.Len(t, pasted, 1)
		assert.Contains(t, pasted[0], "wake-via-scheduler-event")
		schedEnv := decodeHoldTestEnvelope(t, pasted[0])
		assert.Equal(t, msgID, schedEnv["message_id"])
	})

	t.Run("chat_scheduled_send_wake_cloudrun", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		db := openTestMemorySQLite(t, "sqlite")
		wcs := NewWebChatStore(db, "sqlite")
		require.NoError(t, wcs.Init())
		f.srv.SetWebChatStore(wcs)

		topicID := tid("a2-10-topic")
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID:           topicID,
			ProjectID:    f.launched.ProjectID,
			Name:         "a2-10-topic",
			CreatedBy:    f.launched.OwnerID,
			CreatedAt:    time.Now().UTC(),
			DefaultAgent: f.launched.Slug,
		}))
		setTopicConversationID(t, db, f.store, topicID, f.launched.ProjectID)

		f.launched.Phase = string(state.PhaseSuspended)
		f.launched.MessageMode = store.MessageModeProject
		require.NoError(t, f.store.UpdateAgent(ctx, f.launched))

		fireAt := time.Now().UTC().Add(2 * time.Minute)
		schedBody, _ := json.Marshal(map[string]any{
			"content":         "wake-via-chat-scheduled-send",
			"fire_at":         fireAt.Format(time.RFC3339),
			"idempotency_key": "idem-a2-10",
			"wake":            true,
		})
		schedReq, err := http.NewRequest(http.MethodPost, f.public.URL+scheduledConversationPath(topicID), bytes.NewReader(schedBody))
		require.NoError(t, err)
		schedReq.Header.Set("Authorization", "Bearer "+f.userToken)
		schedReq.Header.Set("Content-Type", "application/json")
		schedResp, err := http.DefaultClient.Do(schedReq)
		require.NoError(t, err)
		defer func() { _ = schedResp.Body.Close() }()
		require.Equal(t, http.StatusCreated, schedResp.StatusCode)

		assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))

		listed, err := f.store.ListMessages(ctx, store.MessageFilter{AgentID: f.launched.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		msgID := listed.Items[0].ID

		f.waitMessageState(t, msgID, store.MessageDispatchAwaitingSession)

		ca := f.connectDeliverSession(t, f.public.URL, true, nil)
		f.waitMessageState(t, msgID, store.MessageDispatchDelivered)
		require.Len(t, ca.tmux.pastedTexts(), 1)
		assert.Contains(t, ca.tmux.pastedTexts()[0], "wake-via-chat-scheduled-send")
	})

	t.Run("execute_agent_dm_wake_cloudrun", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		f.launched.Phase = string(state.PhaseSuspended)
		require.NoError(t, f.store.UpdateAgent(ctx, f.launched))

		res, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
			SenderAgent:    f.agent,
			SenderIdentity: wakeDMSenderIdentity(f.agent, ScopeProjectRead, ScopeAgentLifecycle),
			TargetAgent:    f.launched,
			ProjectID:      f.launched.ProjectID,
			Msg:            "wake-via-agent-dm",
			Type:           messages.TypeInstruction,
			Wake:           true,
		})
		require.Nil(t, dmErr)
		require.NotEmpty(t, res.MessageID)

		f.waitMessageState(t, res.MessageID, store.MessageDispatchAwaitingSession)

		ca := f.connectDeliverSession(t, f.public.URL, true, nil)
		f.waitMessageState(t, res.MessageID, store.MessageDispatchDelivered)
		require.Len(t, ca.tmux.pastedTexts(), 1)
		assert.Contains(t, ca.tmux.pastedTexts()[0], "wake-via-agent-dm")
	})
}

func ptrString(s string) *string     { return &s }
func ptrTime(t time.Time) *time.Time { return &t }

func decodeHoldTestEnvelope(t *testing.T, pasted string) map[string]any {
	t.Helper()
	const begin = "---BEGIN SCION MESSAGE---\n"
	const end = "\n---END SCION MESSAGE---"
	i := strings.Index(pasted, begin)
	require.GreaterOrEqual(t, i, 0, "missing begin delimiter in %q", pasted)
	rest := pasted[i+len(begin):]
	j := strings.Index(rest, end)
	require.GreaterOrEqual(t, j, 0, "missing end delimiter in %q", pasted)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(rest[:j]), &out))
	return out
}
