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
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
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
			if out.Held > 0 {
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
	broker         string
	disp           *realBrokerDispatcher
	fault          *conduitFaultStore
	sharedRegStore registry.Store
	sharedReg      *registry.Registry
}

func (f *holdE2EFixture) newPeerHubServer(t *testing.T, instanceID string) *Server {
	t.Helper()
	ctx := context.Background()
	srvB, err := newTestHubServer(t, ServerConfig{}, f.store)
	require.NoError(t, err)
	srvB.conduitGrants = f.srv.conduitGrants
	setHoldTestSettings(t, srvB, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: true})
	srvB.SetDispatcher(f.disp)

	internalB := httptest.NewServer(srvB.ConduitInternalHandler())
	t.Cleanup(internalB.Close)

	authB, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: f.peerSecret, SelfID: instanceID})
	require.NoError(t, err)
	require.NoError(t, srvB.StartConduitRelay(ctx, ConduitRelayOptions{
		InstanceID:       instanceID,
		InternalEndpoint: internalB.URL,
		HTTPClient:       internalB.Client(),
		PeerAuth:         authB,
		Store:            f.sharedRegStore,
		Registry:         f.sharedReg,
		Clock:            clock.NewFake(f.clock.Now()),
		sessionWriteWait: conduitFixtureWriteWait,
	}))
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srvB.shutdownConduitRelay(sctx)
	})
	return srvB
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
	var (
		fs             *conduitFaultStore
		srvPtr         atomic.Pointer[Server]
		sharedRegStore registry.Store
		sharedReg      *registry.Registry
	)
	internalA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := srvPtr.Load(); s != nil {
			s.ConduitInternalHandler().ServeHTTP(w, r)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(internalA.Close)

	pf := newPTYConduitFixtureServer(t, func(srv *Server) {
		srvPtr.Store(srv)
		fs, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *conduitFaultStore {
			return &conduitFaultStore{Store: inner}
		})
		if srv.authzService != nil {
			srv.authzService.store = fs
		}
	}, func(o *ConduitRelayOptions) {
		o.InternalEndpoint = internalA.URL
		o.HTTPClient = internalA.Client()
		sharedRegStore = o.Store
		sharedReg = o.Registry
	})
	f := &holdE2EFixture{
		ptyConduitFixture: pf,
		broker:            pf.launched.RuntimeBrokerID,
		fault:             fs,
		sharedRegStore:    sharedRegStore,
		sharedReg:         sharedReg,
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

// setRedriveLeaseTTLForTest overrides the per-agent cross-replica re-drive
// lease TTL for tests exercising mid-pass lease renewal (O-1).
func (s *Server) setRedriveLeaseTTLForTest(ttl time.Duration) {
	rt := s.holdRuntime()
	rt.mu.Lock()
	rt.leaseTTLOverride = ttl
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
		srvB := f.newPeerHubServer(t, "hub-b-srv")

		var ids []string
		for i := 0; i < 20; i++ {
			m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "multi-rep", time.Now().Add(10*time.Minute))
			ids = append(ids, m.ID)
		}

		f.connectDeliverSession(t, f.public.URL, true, nil)
		fresh, err := f.store.GetAgent(ctx, f.launched.ID)
		require.NoError(t, err)
		stB, errB := srvB.agentConduitDeliverSessionState(ctx, fresh)
		require.NoError(t, errB)
		require.Equal(t, agentDeliverSessionReady, stB, "replica B must resolve the admitted session via shared registry")

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
		var deliveredIDs []string
		for _, d := range f.disp.deliveries() {
			deliveredIDs = append(deliveredIDs, d.MessageIDs...)
		}
		assert.Equal(t, ids, deliveredIDs, "held messages must arrive in creation order across replicas")
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
		require.Eventually(t, func() bool {
			return len(f.disp.notices()) == 1
		}, 5*time.Second, 10*time.Millisecond, "sender failure notice must arrive after async expiry commits")
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
		ctx := context.Background()
		origHoldUntil := time.Now().UTC().Add(4 * time.Minute).Truncate(time.Millisecond)
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "drop-mid-redrive", origHoldUntil)

		deliverEntered := make(chan struct{})
		releaseDeliver := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseDeliver) }) }
		t.Cleanup(release)

		var dropOnce sync.Once
		f.disp.mu.Lock()
		f.disp.beforeDeliver = func(*agentcontrol.DeliverRequest) {
			dropOnce.Do(func() {
				close(deliverEntered)
				<-releaseDeliver
			})
		}
		f.disp.mu.Unlock()

		ca := f.connectDeliverSession(t, f.public.URL, true, nil)

		select {
		case <-deliverEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("re-drive never entered /deliver")
		}

		// Confirm re-drive claimed the held row to dispatched before /deliver runs,
		// so waitMessageState below cannot pass on the initial awaiting_session state.
		claimed, err := f.store.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		require.Equal(t, store.MessageDispatchDispatched, claimed.DispatchState)

		// Drop the Conduit session and wait for the Hub relay to observe the close
		// and evict the session before /deliver hits the Hub; otherwise under
		// -test.cpu=1 /deliver writes the RPC frame to a half-closed socket and
		// rightly treats the post-send close as 504 outcome=unknown (partial).
		ca.stop(t)
		require.Eventually(t, func() bool {
			st, err := f.srv.agentConduitDeliverSessionState(ctx, f.launched)
			if err != nil || st != agentDeliverSessionNone {
				return false
			}
			ps, err := f.sharedRegStore.ListPrincipalSessions(ctx, registry.PrincipalAgent, f.launched.ID)
			return err == nil && len(ps.Sessions) == 0
		}, 10*time.Second, 10*time.Millisecond, "hub relay must evict closed conduit session")
		release()

		// Broker's /deliver gets 503 no_session, reports code=awaiting_session,
		// and row returns to awaiting_session with the ORIGINAL HoldUntil!
		reheld := f.waitMessageState(t, m.ID, store.MessageDispatchAwaitingSession)
		require.NotNil(t, reheld.HoldUntil)
		assert.WithinDuration(t, origHoldUntil, *reheld.HoldUntil, 50*time.Millisecond,
			"A2-S3: re-hold after session drop must preserve original hold_until, never reset to now+10m")
		assert.Empty(t, f.disp.notices())
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
	envs := decodeAllHoldTestEnvelopes(t, pasted)
	require.NotEmpty(t, envs, "missing begin delimiter in %q", pasted)
	return envs[0]
}

func decodeAllHoldTestEnvelopes(t *testing.T, pasted string) []map[string]any {
	t.Helper()
	const begin = "---BEGIN SCION MESSAGE---\n"
	const end = "\n---END SCION MESSAGE---"
	var out []map[string]any
	rest := pasted
	for {
		i := strings.Index(rest, begin)
		if i < 0 {
			break
		}
		afterBegin := rest[i+len(begin):]
		j := strings.Index(afterBegin, end)
		require.GreaterOrEqual(t, j, 0, "missing end delimiter in %q", pasted)
		var env map[string]any
		require.NoError(t, json.Unmarshal([]byte(afterBegin[:j]), &env))
		out = append(out, env)
		rest = afterBegin[j+len(end):]
	}
	return out
}

// ---------------------------------------------------------------------------
// Round 1 review regressions (ptone/scion#7881, PR ptone/scion#8097)
// ---------------------------------------------------------------------------

func (f *holdE2EFixture) seedHeldWith(t *testing.T, sender, senderID, body string, mutate func(*store.Message)) *store.Message {
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
	if mutate != nil {
		mutate(m)
	}
	require.NoError(t, f.store.CreateMessage(ctx, m))
	did := "seed-" + m.ID
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, did, []string{m.ID}, now.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, did, []string{m.ID}, agentcontrol.ReasonNoSession))
	ok, err := f.store.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m.ID,
		AgentID:     f.launched.ID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     f.srv.heldMessagesCap(),
	})
	require.NoError(t, err)
	require.True(t, ok)
	return m
}

func postHoldBrokerJSON(t *testing.T, srv *Server, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Plugin-Name", "test-plugin")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func TestHoldA2_Round1_REQ1_SchedulerChannelRejectedAndSpoofBlocked(t *testing.T) {
	t.Run("all_external_agent_and_broker_ingress_paths_reject_scheduler_channel", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		owner, err := f.store.GetUser(ctx, f.launched.OwnerID)
		require.NoError(t, err)
		evtID := api.NewUUID()

		// 1. Public user/agent message API (reviewer's TestA2Rev_UserAPIPersistsSchedulerChannel).
		userBody := `{"structured_message":{"msg":"spoof-via-api","type":"instruction","channel":"scheduler","thread_id":"` + evtID + `"}}`
		req, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/agents/"+f.launched.ID+"/message", bytes.NewReader([]byte(userBody)))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+f.userToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		// 2. Agent outbound-message API.
		outBody := `{"recipient":"user:` + owner.Email + `","msg":"spoof-outbound","channel":" SCHEDULER "}`
		outReq, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader([]byte(outBody)))
		require.NoError(t, err)
		outReq.Header.Set("Authorization", "Bearer "+f.agentToken(t, f.agent))
		outReq.Header.Set("Content-Type", "application/json")
		outResp, err := http.DefaultClient.Do(outReq)
		require.NoError(t, err)
		_ = outResp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, outResp.StatusCode)

		// 3. Project broadcast API.
		bcastBody := `{"structured_message":{"msg":"spoof-bcast","type":"instruction","channel":"scheduler"}}`
		bcastReq, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/projects/"+f.launched.ProjectID+"/broadcast", bytes.NewReader([]byte(bcastBody)))
		require.NoError(t, err)
		bcastReq.Header.Set("Authorization", "Bearer "+f.userToken)
		bcastReq.Header.Set("Content-Type", "application/json")
		bcastResp, err := http.DefaultClient.Do(bcastReq)
		require.NoError(t, err)
		_ = bcastResp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, bcastResp.StatusCode)

		// 4. ExecuteAgentDM.
		_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
			SenderAgent:    f.agent,
			SenderIdentity: &storedAgentIdentity{agent: f.agent},
			TargetAgent:    f.launched,
			ProjectID:      f.launched.ProjectID,
			Msg:            "spoof-dm",
			Type:           messages.TypeInstruction,
			Channel:        "scheduler",
		})
		require.NotNil(t, dmErr)
		assert.Equal(t, http.StatusBadRequest, dmErr.HTTPStatus)

		// 5. Broker legacy inbound.
		recLegacy := postHoldBrokerJSON(t, f.srv, "/api/v1/broker/inbound", inboundMessageRequest{
			Topic: "scion.project." + f.launched.ProjectID + ".agent." + f.launched.Slug + ".messages",
			Message: &messages.StructuredMessage{
				Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel: "scheduler", ThreadID: evtID,
				Sender: "user:" + owner.Email, Recipient: "agent:" + f.launched.Slug,
				Msg: "spoof-broker-inbound", Type: messages.TypeInstruction,
			},
		})
		assert.Equal(t, http.StatusBadRequest, recLegacy.Code, recLegacy.Body.String())

		// 6. Broker routed inbound.
		recRouted := postHoldBrokerJSON(t, f.srv, "/api/v1/broker/inbound/routed", routedInboundRequest{
			ProjectID: f.launched.ProjectID, DefaultAgent: f.launched.Slug,
			Message: &messages.StructuredMessage{
				Version: messages.Version, Channel: "scheduler", ThreadID: evtID,
				Sender: "user:" + owner.Email, Msg: "spoof-broker-routed", Type: messages.TypeInstruction,
			},
		})
		assert.Equal(t, http.StatusBadRequest, recRouted.Code, recRouted.Body.String())

		listed, err := f.store.ListMessages(ctx, store.MessageFilter{ProjectID: f.launched.ProjectID}, store.ListOptions{Limit: 20})
		require.NoError(t, err)
		assert.Empty(t, listed.Items, "no rejected channel=scheduler request may persist a message row")
	})

	t.Run("suspended_sender_cannot_bypass_revalidation_via_channel_thread_id_or_mismatched_event", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		// Valid scheduled event targeting f.launched, plus another targeting f.agent.
		payloadTarget := `{"agentName":"` + f.launched.Slug + `","message":"legit"}`
		evtTarget := withAgentRevision(t, f.srv, store.ScheduledEvent{
			ID: api.NewUUID(), ProjectID: f.launched.ProjectID, EventType: "message",
			Payload: payloadTarget, Status: store.ScheduledEventPending, CreatedBy: f.agent.ID,
		}, f.agent.ID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evtTarget))

		payloadOther := `{"agentName":"` + f.agent.Slug + `","message":"other"}`
		evtOther := withAgentRevision(t, f.srv, store.ScheduledEvent{
			ID: api.NewUUID(), ProjectID: f.launched.ProjectID, EventType: "message",
			Payload: payloadOther, Status: store.ScheduledEventPending, CreatedBy: f.agent.ID,
		}, f.agent.ID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evtOther))

		uid := tid("req1-spoof-user")
		createTestUserWithProjectRole(t, f.store, uid, "req1-spoof@conduit.test", f.launched.ProjectID, store.ProjectRoleMember)
		ensureHubMembership(ctx, f.store, uid)

		control := f.seedHeldWith(t, "user:"+uid, uid, "control-no-spoof", nil)
		spoofChannel := f.seedHeldWith(t, "user:"+uid, uid, "spoofed-scheduler-channel", func(m *store.Message) {
			m.Channel = "scheduler"
			m.ThreadID = evtTarget.ID
		})
		senderProj := f.launched.ProjectID
		mismatchedEvt := f.seedHeldWith(t, "scheduler", "SCHEDULER", "mismatched-target-event", func(m *store.Message) {
			m.Type = messages.TypeSystem
			m.Channel = "scheduler"
			m.ThreadID = evtOther.ID
			m.ScheduledEventID = evtOther.ID
			m.SenderProjectID = &senderProj
		})

		u, err := f.store.GetUser(ctx, uid)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.store.UpdateUser(ctx, u))

		ca := f.connectDeliverSession(t, f.public.URL, true, nil)
		for _, m := range []*store.Message{control, spoofChannel, mismatchedEvt} {
			got := f.waitMessageState(t, m.ID, store.MessageDispatchFailed)
			require.NotNil(t, got.DispatchFailureReason)
			assert.Equal(t, holdFailReasonSenderUnauthorized, *got.DispatchFailureReason)
		}
		assert.Empty(t, ca.tmux.pastedTexts())
	})
}

type pausingHoldDispatcher struct {
	AgentDispatcher
	targetSlug  string
	once        sync.Once
	releaseOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (p *pausingHoldDispatcher) unblock() {
	p.releaseOnce.Do(func() {
		close(p.release)
	})
}

func (p *pausingHoldDispatcher) DispatchAgentMessage(ctx context.Context, target *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	if target.Slug == p.targetSlug {
		p.once.Do(func() {
			close(p.entered)
			<-p.release
		})
	}
	return p.AgentDispatcher.DispatchAgentMessage(ctx, target, message, interrupt, sm)
}

func TestHoldA2_Round1_REQ2_CrossReplicaBatchOrdering150(t *testing.T) {
	t.Run("second_hub_server", func(t *testing.T) { runHoldA2CrossReplicaOrdering150(t, false) })
	t.Run("second_pass_same_store", func(t *testing.T) { runHoldA2CrossReplicaOrdering150(t, true) })
}

func runHoldA2CrossReplicaOrdering150(t *testing.T, sameProcessSecondPass bool) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()
	f.srv.setHeldMessagesCapForTest(200)

	srvB := f.newPeerHubServer(t, "hub-b-req2")

	var expected []string
	for i := 0; i < 150; i++ {
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "xrep-150", time.Now().Add(10*time.Minute))
		expected = append(expected, m.ID)
	}

	pd := &pausingHoldDispatcher{
		AgentDispatcher: f.disp,
		targetSlug:      f.launched.Slug,
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	t.Cleanup(pd.unblock)
	f.srv.SetDispatcher(pd)

	f.connectDeliverSession(t, f.public.URL, true, nil)
	select {
	case <-pd.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("replica A never started dispatching batch 1")
	}

	fresh, gerr := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, gerr)
	stB, errB := srvB.agentConduitDeliverSessionState(ctx, fresh)
	require.NoError(t, errB)
	require.Equal(t, agentDeliverSessionReady, stB, "replica B must see the admitted session")

	second := srvB
	if sameProcessSecondPass {
		second = f.srv
	}
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		second.redriveAgentHeldMessagesOnce(ctx, f.launched.ID)
	}()
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("competing replica pass should return immediately on active lease")
	}
	assert.Equal(t, 0, f.disp.dispatchCount(f.launched.Slug), "competing replica must not dispatch any batch while replica A holds the per-agent lease")

	// Live arrival while lease is active queues behind the drain rather than overtaking batch 1.
	liveRes, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    fresh,
		ProjectID:      fresh.ProjectID,
		Msg:            "xrep-live-151",
		Type:           messages.TypeInstruction,
	})
	require.Nil(t, dmErr)
	liveRow, lerr := f.store.GetMessage(ctx, liveRes.MessageID)
	require.NoError(t, lerr)
	assert.Equal(t, store.MessageDispatchAwaitingSession, liveRow.DispatchState, "live send during leased drain must queue behind hold")
	expected = append(expected, liveRes.MessageID)

	pd.unblock()

	for _, id := range expected {
		f.waitMessageState(t, id, store.MessageDispatchDelivered)
	}
	var got []string
	for _, d := range f.disp.deliveries() {
		assert.LessOrEqual(t, len(d.MessageIDs), messageHoldClaimBatchSize, "each Conduit deliver-message RPC must stay within 64 IDs")
		got = append(got, d.MessageIDs...)
	}
	assert.Equal(t, expected, got, "1..151 must arrive strictly in creation order across replicas")
}

type stopOnGetUserHoldStore struct {
	store.Store
	inner   store.Store
	agentID string
	userID  string
	once    sync.Once
}

func (s *stopOnGetUserHoldStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.userID {
		var fired bool
		s.once.Do(func() {
			fired = true
			a, err := s.inner.GetAgent(ctx, s.agentID)
			if err == nil {
				a.Phase = string(state.PhaseStopped)
				_ = s.inner.UpdateAgent(ctx, a)
			}
		})
		if fired {
			return nil, errors.New("injected transient user lookup fault")
		}
	}
	return s.Store.GetUser(ctx, id)
}

type cancelAfterClaimHoldStore struct {
	store.Store
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelAfterClaimHoldStore) ClaimHeldMessages(ctx context.Context, agentID string, limit int) ([]store.Message, error) {
	claimed, err := s.Store.ClaimHeldMessages(ctx, agentID, limit)
	if err == nil && len(claimed) > 0 {
		s.once.Do(func() {
			if s.cancel != nil {
				s.cancel()
			}
		})
	}
	return claimed, err
}

func TestHoldA2_Round1_REQ3_OPT6_RefusedReholdAndTimeoutNeverOrphan(t *testing.T) {
	t.Run("refused_rehold_on_stopped_agent_fails_all_claimed_rows_with_notices", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		m1 := f.seedHeldMessage(t, "user:"+f.launched.OwnerID, f.launched.OwnerID, "orphan-1-user", time.Now().Add(10*time.Minute))
		m2 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "orphan-2-agent", time.Now().Add(10*time.Minute))

		installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *stopOnGetUserHoldStore {
			return &stopOnGetUserHoldStore{Store: inner, inner: f.store, agentID: f.launched.ID, userID: f.launched.OwnerID}
		})
		f.connectDeliverSession(t, f.public.URL, true, nil)

		got1 := f.waitMessageState(t, m1.ID, store.MessageDispatchFailed)
		got2 := f.waitMessageState(t, m2.ID, store.MessageDispatchFailed)
		require.NotNil(t, got1.DispatchFailureReason)
		require.NotNil(t, got2.DispatchFailureReason)
		assert.Equal(t, holdFailReasonAgentStopped, *got1.DispatchFailureReason)
		assert.Equal(t, holdFailReasonAgentStopped, *got2.DispatchFailureReason)
		assert.Equal(t, 0, f.disp.dispatchCount(f.launched.Slug))
		require.Len(t, f.disp.notices(), 1, "agent sender of m2 must receive DELIVERY_FAILED notice")
		assert.Contains(t, f.disp.notices()[0], holdFailReasonAgentStopped)
	})

	t.Run("pass_context_timeout_treats_claimed_rows_as_transient_and_reholds", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()

		// Connect session first when no rows are held, then seed held rows so we
		// drive one explicit pass whose context cancels right after ClaimHeldMessages.
		ca := f.connectDeliverSession(t, f.public.URL, true, nil)
		m1 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "timeout-rehold-1", time.Now().Add(10*time.Minute))
		m2 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "timeout-rehold-2", time.Now().Add(10*time.Minute))

		passCtx, passCancel := context.WithCancel(ctx)
		wrapper, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *cancelAfterClaimHoldStore {
			return &cancelAfterClaimHoldStore{Store: inner, cancel: passCancel}
		})
		f.srv.redriveAgentHeldMessagesOnce(passCtx, f.launched.ID)
		wrapper.cancel = nil

		for _, id := range []string{m1.ID, m2.ID} {
			got, err := f.store.GetMessage(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, store.MessageDispatchAwaitingSession, got.DispatchState,
				"claimed row must return to awaiting_session on pass timeout, never fail or stick in dispatched")
		}
		assert.Equal(t, 0, f.disp.dispatchCount(f.launched.Slug))
		assert.Empty(t, f.disp.notices())

		// Subsequent normal pass delivers both re-held rows coalesced into a
		// single /deliver flush in creation order (AC A2-3b, R-4).
		f.srv.redriveAgentHeldMessagesOnce(ctx, f.launched.ID)
		f.waitMessageState(t, m1.ID, store.MessageDispatchDelivered)
		f.waitMessageState(t, m2.ID, store.MessageDispatchDelivered)
		deliveries := f.disp.deliveries()
		require.Len(t, deliveries, 1, "two re-driven rows must coalesce into one /deliver flush (A2-3b)")
		assert.Equal(t, []string{m1.ID, m2.ID}, deliveries[0].MessageIDs)
		joined := strings.Join(ca.tmux.pastedTexts(), "\n\n")
		i1 := strings.Index(joined, "timeout-rehold-1")
		i2 := strings.Index(joined, "timeout-rehold-2")
		require.True(t, i1 >= 0 && i2 > i1, "pasted text must contain both messages in creation order: %q", joined)
	})
}

type recordingContextIDDispatcher struct {
	AgentDispatcher
	mu      sync.Mutex
	ctxIDs  []string
	failErr error
}

func (d *recordingContextIDDispatcher) DispatchAgentMessage(ctx context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	d.mu.Lock()
	d.ctxIDs = append(d.ctxIDs, dispatchMessageIDFromContext(ctx))
	err := d.failErr
	d.mu.Unlock()
	return err
}

func TestHoldA2_Round1_REQ4_FlagOffMatchesPhaseAForSchedulerAndBrokerInbound(t *testing.T) {
	t.Run("scheduler_message_fire_stores_no_row_with_flag_off", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		setHoldTestSettings(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: false})
		recDisp := &recordingContextIDDispatcher{AgentDispatcher: f.disp}
		f.srv.SetDispatcher(recDisp)

		payload := `{"agentName":"` + f.launched.Slug + `","message":"flag-off-sched"}`
		evt := withAgentRevision(t, f.srv, store.ScheduledEvent{
			ID: api.NewUUID(), ProjectID: f.launched.ProjectID, EventType: "message",
			Payload: payload, Status: store.ScheduledEventPending, CreatedBy: f.agent.ID,
		}, f.agent.ID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		require.NoError(t, f.srv.messageEventHandler()(ctx, evt))

		listed, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.launched.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, listed.Items, "flag off: scheduler fire must not persist a messages row")
		require.Len(t, recDisp.ctxIDs, 1)
		assert.Empty(t, recDisp.ctxIDs[0], "flag off: scheduler fire must not set dispatch message_id")
	})

	t.Run("broker_inbound_and_routed_dispatch_before_persist_with_flag_off", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		setHoldTestSettings(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: false})
		owner, err := f.store.GetUser(ctx, f.launched.OwnerID)
		require.NoError(t, err)

		recDisp := &recordingContextIDDispatcher{AgentDispatcher: f.disp, failErr: errors.New("simulated broker down")}
		f.srv.SetDispatcher(recDisp)

		recLegacy := postHoldBrokerJSON(t, f.srv, "/api/v1/broker/inbound", inboundMessageRequest{
			Topic: "scion.project." + f.launched.ProjectID + ".agent." + f.launched.Slug + ".messages",
			Message: &messages.StructuredMessage{
				Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel: "telegram", Sender: "user:" + owner.Email, Recipient: "agent:" + f.launched.Slug,
				Msg: "legacy-flag-off", Type: messages.TypeInstruction,
			},
		})
		assert.Equal(t, http.StatusBadGateway, recLegacy.Code)

		recRouted := postHoldBrokerJSON(t, f.srv, "/api/v1/broker/inbound/routed", routedInboundRequest{
			ProjectID: f.launched.ProjectID, DefaultAgent: f.launched.Slug,
			Message: &messages.StructuredMessage{
				Version: messages.Version, Channel: "telegram", Sender: "user:" + owner.Email,
				Msg: "routed-flag-off", Type: messages.TypeInstruction,
			},
		})
		assert.Equal(t, http.StatusBadGateway, recRouted.Code)

		require.Len(t, recDisp.ctxIDs, 2)
		assert.Empty(t, recDisp.ctxIDs[0], "flag off: legacy broker inbound must not set dispatch message_id")
		assert.Empty(t, recDisp.ctxIDs[1], "flag off: routed broker inbound must not set dispatch message_id")

		listed, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.launched.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, listed.Items, "flag off: failed broker-inbound dispatches must not persist pre-dispatch rows")

		// When dispatch succeeds with flag off, post-dispatch persistence still writes both rows.
		recDisp.mu.Lock()
		recDisp.failErr = nil
		recDisp.mu.Unlock()

		okLegacy := postHoldBrokerJSON(t, f.srv, "/api/v1/broker/inbound", inboundMessageRequest{
			Topic: "scion.project." + f.launched.ProjectID + ".agent." + f.launched.Slug + ".messages",
			Message: &messages.StructuredMessage{
				Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel: "telegram", Sender: "user:" + owner.Email, Recipient: "agent:" + f.launched.Slug,
				Msg: "legacy-ok", Type: messages.TypeInstruction,
			},
		})
		require.Equal(t, http.StatusOK, okLegacy.Code, okLegacy.Body.String())

		okRouted := postHoldBrokerJSON(t, f.srv, "/api/v1/broker/inbound/routed", routedInboundRequest{
			ProjectID: f.launched.ProjectID, DefaultAgent: f.launched.Slug,
			Message: &messages.StructuredMessage{
				Version: messages.Version, Channel: "telegram", Sender: "user:" + owner.Email,
				Msg: "routed-ok", Type: messages.TypeInstruction,
			},
		})
		require.Equal(t, http.StatusOK, okRouted.Code, okRouted.Body.String())

		listedAfter, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.launched.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Len(t, listedAfter.Items, 2)
	})
}

func TestHoldA2_Round1_REQ5_FlagOffStillExpiresHeldRowsWithNotice(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	// Create a second agent to test stopped-agent sweep cleanup with flag off too.
	agent2 := &store.Agent{
		ID:              tid("req5-stopped-target"),
		Name:            "req5-stopped-target",
		Slug:            "req5-stopped-target",
		ProjectID:       f.launched.ProjectID,
		RuntimeBrokerID: f.broker,
		Runtime:         "cloudrun",
		Phase:           string(state.PhaseRunning),
		RunID:           uuid.NewString(),
		OwnerID:         f.launched.OwnerID,
		CreatedBy:       f.launched.OwnerID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, agent2))

	// Hold m1 (short TTL) on f.launched and m2 (long TTL) on agent2 while flag is ON.
	m1 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "flag-off-ttl", time.Now().Add(25*time.Millisecond))
	m2 := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     agent2.ProjectID,
		AgentID:       agent2.ID,
		Sender:        "agent:" + f.agent.Slug,
		SenderID:      f.agent.ID,
		Recipient:     "agent:" + agent2.Slug,
		RecipientID:   agent2.ID,
		Msg:           "flag-off-stopped",
		Type:          messages.TypeInstruction,
		DispatchState: store.MessageDispatchDispatched,
		HoldLaunchID:  agent2.RunID,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, m2))
	_, err := f.store.BindMessageDelivery(ctx, agent2.ID, "seed-"+m2.ID, []string{m2.ID}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, "seed-"+m2.ID, []string{m2.ID}, agentcontrol.ReasonNoSession))
	ok, err := f.store.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID: m2.ID, AgentID: agent2.ID, MaxProofAge: 2 * time.Minute, HoldTTL: 10 * time.Minute, MaxHeld: 100,
	})
	require.NoError(t, err)
	require.True(t, ok)

	// Flip hub.conduit_deliver OFF, let m1 pass hold_until, and mark agent2 stopped.
	setHoldTestSettings(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: false})
	time.Sleep(45 * time.Millisecond)
	agent2.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(ctx, agent2))

	f.srv.sweepHeldMessages(ctx)

	got1 := f.waitMessageState(t, m1.ID, store.MessageDispatchFailed)
	require.NotNil(t, got1.DispatchFailureReason)
	assert.Equal(t, holdFailReasonSessionTimeout, *got1.DispatchFailureReason)

	got2 := f.waitMessageState(t, m2.ID, store.MessageDispatchFailed)
	require.NotNil(t, got2.DispatchFailureReason)
	assert.Equal(t, holdFailReasonAgentStopped, *got2.DispatchFailureReason)

	require.Len(t, f.disp.notices(), 2, "both senders must receive failure notices even with hub.conduit_deliver off")
}

func TestHoldA2_Round1_OPT1_DecisionResponsePreservesAgentLineAndMetadata(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()
	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	f.srv.SetWebChatStore(wcs)
	setHoldTestSettings(t, f.srv, map[string]bool{
		conduitExperiment:             true,
		experiments.ConduitDeliver:    true,
		experiments.Decisions:         true,
		experiments.ChatScheduledSend: true,
	})

	owner, err := f.store.GetUser(ctx, f.launched.OwnerID)
	require.NoError(t, err)

	sendDecisionPromptFromLaunched := func(rawDecisions string) struct {
		MessageID string               `json:"message_id"`
		Decisions []DecisionSendResult `json:"decisions"`
	} {
		var decs []messages.DecisionInput
		require.NoError(t, json.Unmarshal([]byte(rawDecisions), &decs))
		body, err := json.Marshal(OutboundMessageRequest{
			Recipient: "user:" + owner.Email,
			Msg:       "Please choose",
			Decisions: decs,
		})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/agents/"+f.launched.ID+"/outbound-message", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+f.agentToken(t, f.launched))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		rb, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(rb))
		var out struct {
			MessageID string               `json:"message_id"`
			Decisions []DecisionSendResult `json:"decisions"`
		}
		require.NoError(t, json.Unmarshal(rb, &out))
		return out
	}

	// 1. Single-decision prompt with an agent_interpreted answer ("no" = choice 2),
	//    then overridden by the user ("yes" = choice 1) while f.launched has no session.
	singleOut := sendDecisionPromptFromLaunched(`[{"id":"D10","question":"Ship the feature now?","choices":["yes","no"],"default":"yes"}]`)
	require.Len(t, singleOut.Decisions, 1)
	singleRowID := singleOut.Decisions[0].RowID

	srcMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
		Sender: "user:" + owner.Email, SenderID: owner.ID,
		Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID,
		Msg: "maybe not yet", Type: messages.TypeInstruction,
		DispatchState: store.MessageDispatchDelivered, CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, srcMsg))
	interpResp := &store.Message{
		ID: uuid.NewString(), ProjectID: f.launched.ProjectID, AgentID: f.launched.ID,
		Sender: "agent:" + f.launched.Slug, SenderID: f.launched.ID,
		Recipient: "user:" + owner.Email, RecipientID: owner.ID,
		Msg: "recorded interpreted no", Type: messages.TypeDecisionResponse,
		DispatchState: store.MessageDispatchDelivered, CreatedAt: time.Now().UTC(),
	}
	_, _, err = f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:        singleRowID,
		ChoiceID:             "2",
		AnsweredByUserID:     owner.ID,
		AnsweredByName:       owner.DisplayName,
		AnsweredByEmail:      owner.Email,
		Provenance:           store.DecisionProvenanceAgentInterpreted,
		InterpretedByAgentID: f.launched.ID,
		SourceMessageID:      srcMsg.ID,
		Surface:              store.DecisionSurfaceNative,
		ResponseMessage:      interpResp,
	})
	require.NoError(t, err)

	ansBody, _ := json.Marshal(map[string]any{"choice_id": "1"})
	ansReq, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/decisions/"+singleRowID+"/answer", bytes.NewReader(ansBody))
	require.NoError(t, err)
	ansReq.Header.Set("Authorization", "Bearer "+f.userToken)
	ansReq.Header.Set("Content-Type", "application/json")
	ansResp, err := http.DefaultClient.Do(ansReq)
	require.NoError(t, err)
	ansBytes, _ := io.ReadAll(ansResp.Body)
	_ = ansResp.Body.Close()
	require.Equal(t, http.StatusOK, ansResp.StatusCode, string(ansBytes))
	var singleAnsOut DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(ansBytes, &singleAnsOut))
	f.waitMessageState(t, singleAnsOut.ResponseMessageID, store.MessageDispatchAwaitingSession)

	// 2. Three-decision set prompt answered via POST /api/v1/decision-prompts/{id}/answers
	//    while f.launched is held in awaiting_session.
	setOut := sendDecisionPromptFromLaunched(`[{"id":"D234","question":"Ship the feature now?","choices":["yes","no"],"default":"yes"},{"question":"Which approach?","choices":[{"id":"a","label":"Extend"},{"id":"b","label":"New type"}]},{"id":"ship-now","question":"Deploy today?","choices":["yes","no"],"default":"no"}]`)
	require.Len(t, setOut.Decisions, 3)
	setAnsBody, _ := json.Marshal(map[string]any{
		"answers": []map[string]any{
			{"decision_row_id": setOut.Decisions[0].RowID, "choice_id": "1"},
			{"decision_row_id": setOut.Decisions[1].RowID, "choice_id": "a"},
			{"decision_row_id": setOut.Decisions[2].RowID, "choice_id": "1"},
		},
	})
	setReq, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/decision-prompts/"+setOut.MessageID+"/answers", bytes.NewReader(setAnsBody))
	require.NoError(t, err)
	setReq.Header.Set("Authorization", "Bearer "+f.userToken)
	setReq.Header.Set("Content-Type", "application/json")
	setResp, err := http.DefaultClient.Do(setReq)
	require.NoError(t, err)
	setBytes, _ := io.ReadAll(setResp.Body)
	_ = setResp.Body.Close()
	require.Equal(t, http.StatusOK, setResp.StatusCode, string(setBytes))
	var setAnsOut DecisionSetAnswerResponse
	require.NoError(t, json.Unmarshal(setBytes, &setAnsOut))
	f.waitMessageState(t, setAnsOut.ResponseMessageID, store.MessageDispatchAwaitingSession)

	// Connect Conduit session and verify both re-driven decision-response envelopes.
	ca := f.connectDeliverSession(t, f.public.URL, true, nil)
	f.waitMessageState(t, singleAnsOut.ResponseMessageID, store.MessageDispatchDelivered)
	f.waitMessageState(t, setAnsOut.ResponseMessageID, store.MessageDispatchDelivered)

	// Note: realBrokerDispatcher coalesces rapid re-driven messages in MessageBuffer,
	// so extract all ---BEGIN SCION MESSAGE--- envelopes across pasted chunks.
	var pastedEnvs []map[string]any
	for _, chunk := range ca.tmux.pastedTexts() {
		pastedEnvs = append(pastedEnvs, decodeAllHoldTestEnvelopes(t, chunk)...)
	}
	require.Len(t, pastedEnvs, 2)

	assert.Equal(t, singleAnsOut.ResponseMessageID, pastedEnvs[0]["message_id"])
	singleMsgText, _ := pastedEnvs[0]["msg"].(string)
	assert.Contains(t, singleMsgText, `[decision-response] D10 v1 ("Ship the feature now?") answer=yes (choice 1) by `+owner.Email+`, overriding interpreted answer no`)
	singleMeta, _ := pastedEnvs[0]["metadata"].(map[string]any)
	require.NotNil(t, singleMeta)
	assert.Equal(t, "D10", singleMeta[messages.MetadataDecisionKey])
	assert.Equal(t, "1", singleMeta[messages.MetadataDecisionVersion])
	assert.Equal(t, "1", singleMeta[messages.MetadataChoiceID])
	assert.Equal(t, store.DecisionProvenanceDirect, singleMeta[messages.MetadataDecisionProvenance])

	assert.Equal(t, setAnsOut.ResponseMessageID, pastedEnvs[1]["message_id"])
	setMsgText, _ := pastedEnvs[1]["msg"].(string)
	assert.Contains(t, setMsgText, "[decision-response] 3 decisions answered by "+owner.Email+" (prompt "+setOut.MessageID+")")
	assert.Contains(t, setMsgText, `D234 v1 ("Ship the feature now?") answer=yes (choice 1, recommended)`)
	assert.Contains(t, setMsgText, `1 v1 ("Which approach?") answer=Extend (choice a)`)
	assert.Contains(t, setMsgText, `ship-now v1 ("Deploy today?") answer=yes (choice 1)`)
	setMeta, _ := pastedEnvs[1]["metadata"].(map[string]any)
	require.NotNil(t, setMeta)
	assert.Equal(t, setAnsOut.BatchID, setMeta[messages.MetadataDecisionBatch])
	assert.Equal(t, "D234@1=1,1@1=a,ship-now@1=1", setMeta[messages.MetadataDecisionAnswers])
	assert.Empty(t, setMeta[messages.MetadataDecisionKey])
}

func TestHoldA2_Round2_R1_FlagOffRoutedGroupRecordsUserParticipant(t *testing.T) {
	env := setupRoutedTestEnv(t)
	setHoldTestSettings(t, env.srv, map[string]bool{
		conduitExperiment:          true,
		experiments.ConduitDeliver: false,
	})

	rec := env.doRoutedRequest(t, routedThreadPost(env, "r1-flag-off-group-thread", "hello @beta"))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	convID := latestConversationID(t, env.store, env.agent1.ID)
	conv, err := env.store.GetConversation(context.Background(), convID)
	require.NoError(t, err)
	require.Equal(t, "group", conv.Kind)

	assert.Equal(t, 1, countParticipants(t, env.store, convID, "user", env.user.ID),
		"R-1: with hub.conduit_deliver off, routed group post must still record the posting user as a participant")
	assert.Equal(t, 1, countParticipants(t, env.store, convID, "agent", env.agent1.ID))
	assert.Equal(t, 1, countParticipants(t, env.store, convID, "agent", env.agent2.ID))
}

type slowHoldDispatcher struct {
	AgentDispatcher
	targetSlug string
	perRowWait time.Duration
}

func (d *slowHoldDispatcher) DispatchAgentMessage(ctx context.Context, target *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	if target.Slug == d.targetSlug && d.perRowWait > 0 {
		time.Sleep(d.perRowWait)
	}
	return d.AgentDispatcher.DispatchAgentMessage(ctx, target, message, interrupt, sm)
}

func TestHoldA2_Round2_O1_SlowPassRenewsLeaseBlockingReplicaB(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()
	f.srv.setHeldMessagesCapForTest(150)

	// Shorten replica A's lease TTL to 300ms while batch 1 dispatch + settle
	// takes >600ms total (~5ms * 64 = 320ms dispatch + 350ms settle pause).
	// Without mid-batch and pre-settle lease renewals (O-1), replica A's 300ms
	// lease would expire mid-batch and replica B would steal batch 2 early!
	f.srv.setRedriveLeaseTTLForTest(300 * time.Millisecond)

	srvB := f.newPeerHubServer(t, "hub-b-o1")
	srvB.setRedriveLeaseTTLForTest(300 * time.Millisecond)

	var expected []string
	for i := 0; i < 100; i++ {
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "o1-slow", time.Now().Add(10*time.Minute))
		expected = append(expected, m.ID)
	}

	f.srv.SetDispatcher(&slowHoldDispatcher{
		AgentDispatcher: f.disp,
		targetSlug:      f.launched.Slug,
		perRowWait:      5 * time.Millisecond,
	})

	settleEntered := make(chan struct{})
	var settleOnce sync.Once
	f.disp.mu.Lock()
	f.disp.beforeDeliver = func(req *agentcontrol.DeliverRequest) {
		settleOnce.Do(func() {
			close(settleEntered)
			// Hold batch 1 before /deliver bind for longer than the 300ms lease TTL!
			time.Sleep(350 * time.Millisecond)
		})
	}
	f.disp.mu.Unlock()

	f.connectDeliverSession(t, f.public.URL, true, nil)

	select {
	case <-settleEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("batch 1 never entered /deliver")
	}

	// Wait 200ms into the 350ms beforeDeliver sleep: total elapsed since replica A
	// acquired its initial 300ms lease is now >520ms (> 300ms initial TTL!).
	// Because replica A renewed mid-batch AND right before waitForBatchBoundOrSettled,
	// replica B's competing pass must STILL be blocked and dispatch 0 rows from batch 2!
	time.Sleep(200 * time.Millisecond)
	srvB.redriveAgentHeldMessagesOnce(ctx, f.launched.ID)
	assert.Equal(t, 64, f.disp.dispatchCount(f.launched.Slug),
		"O-1: replica B must not claim or dispatch batch 2 during a slow batch-1 pass because replica A renewed its lease")

	for _, id := range expected {
		f.waitMessageState(t, id, store.MessageDispatchDelivered)
	}
	deliveries := f.disp.deliveries()
	require.Len(t, deliveries, 2, "100 rows must still arrive in 2 ordered batches (64 + 36)")
	assert.Equal(t, expected[:64], deliveries[0].MessageIDs)
	assert.Equal(t, expected[64:], deliveries[1].MessageIDs)
}

type rv3CountingHoldStore struct {
	store.Store
	claims          atomic.Int64
	acquires        atomic.Int64
	failAgentExpiry atomic.Bool
}

func (s *rv3CountingHoldStore) ClaimHeldMessages(ctx context.Context, agentID string, limit int) ([]store.Message, error) {
	s.claims.Add(1)
	return s.Store.ClaimHeldMessages(ctx, agentID, limit)
}

func (s *rv3CountingHoldStore) AcquireAgentRedriveLease(ctx context.Context, agentID, ownerID string, ttl time.Duration) (bool, error) {
	s.acquires.Add(1)
	return s.Store.AcquireAgentRedriveLease(ctx, agentID, ownerID, ttl)
}

func (s *rv3CountingHoldStore) ExpireHeldMessagesPastDeadlineForAgent(ctx context.Context, agentID, reason string) ([]store.Message, error) {
	if s.failAgentExpiry.Load() {
		return nil, errors.New("injected inline deadline expiry fault")
	}
	return s.Store.ExpireHeldMessagesPastDeadlineForAgent(ctx, agentID, reason)
}

func TestHoldA2_Round3_RV3R1_ExpiredUnsweptRowNoBusyLoop(t *testing.T) {
	t.Run("connect_with_expired_unswept_row_expires_inline_without_loop", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "expired-unswept", time.Now().Add(300*time.Millisecond))
		time.Sleep(600 * time.Millisecond)

		cs, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *rv3CountingHoldStore {
			return &rv3CountingHoldStore{Store: inner}
		})
		f.connectDeliverSession(t, f.public.URL, true, nil)
		time.Sleep(1 * time.Second)

		claims := cs.claims.Load()
		acq := cs.acquires.Load()
		assert.LessOrEqual(t, claims, int64(5), "RV3-R1: re-drive must not busy-loop on an expired unswept row")
		assert.LessOrEqual(t, acq, int64(5), "RV3-R1: lease acquisitions must stay bounded")

		// Inline past-deadline expiry transitions m to failed with fixed reason + sender notice immediately.
		got := f.waitMessageState(t, m.ID, store.MessageDispatchFailed)
		require.NotNil(t, got.DispatchFailureReason)
		assert.Equal(t, holdFailReasonSessionTimeout, *got.DispatchFailureReason)
		require.Len(t, f.disp.notices(), 1)
		assert.Contains(t, f.disp.notices()[0], holdFailReasonSessionTimeout)

		hasHold, err := f.store.HasAgentDeliveryHold(ctx, f.launched.ID)
		require.NoError(t, err)
		assert.False(t, hasHold, "marker must be deleted after inline expiry and lease release")

		f.srv.sweepHeldMessages(ctx)
		after := cs.claims.Load()
		time.Sleep(300 * time.Millisecond)
		assert.Equal(t, after, cs.claims.Load(), "no further claims once marker is removed")
	})

	t.Run("faulted_inline_expiry_still_does_not_busy_loop", func(t *testing.T) {
		f := newHoldE2EFixture(t)
		ctx := context.Background()
		m := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "expired-faulted", time.Now().Add(200*time.Millisecond))
		time.Sleep(400 * time.Millisecond)

		cs, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *rv3CountingHoldStore {
			w := &rv3CountingHoldStore{Store: inner}
			w.failAgentExpiry.Store(true)
			return w
		})
		f.connectDeliverSession(t, f.public.URL, true, nil)
		time.Sleep(600 * time.Millisecond)

		// Even when inline expiry fails so the marker stays present, claimedAny==false
		// prevents deferred release from self-rescheduling!
		claims := cs.claims.Load()
		assert.LessOrEqual(t, claims, int64(3), "RV3-R1 defense-in-depth: claimedAny guard must stop loop even if inline expiry faults")

		f.srv.sweepHeldMessages(ctx)
		f.waitMessageState(t, m.ID, store.MessageDispatchFailed)
	})
}

func TestHoldA2_Round3_RV3O1_LaterHoldEpisodeExtendsMarkerOrdering(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	// Episode 1: seed a held row whose hold_until expires in 500ms.
	m1 := f.seedHeldMessage(t, "agent:"+f.agent.Slug, f.agent.ID, "rv3o1-ep1", time.Now().Add(500*time.Millisecond))

	// Episode 2: hold a fresh message via /deliver no_session + /message-failures awaiting_session (HoldTTL = 10m)
	// while Episode 1's marker already exists.
	m2 := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     f.launched.ProjectID,
		AgentID:       f.launched.ID,
		Sender:        "agent:" + f.agent.Slug,
		SenderID:      f.agent.ID,
		Recipient:     "agent:" + f.launched.Slug,
		RecipientID:   f.launched.ID,
		Msg:           "rv3o1-ep2",
		Type:          messages.TypeInstruction,
		DispatchState: store.MessageDispatchDispatched,
		HoldLaunchID:  f.launched.RunID,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, m2))
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d-rv3o1", []string{m2.ID}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, "d-rv3o1", []string{m2.ID}, agentcontrol.ReasonNoSession))
	held, err := f.store.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   m2.ID,
		AgentID:     f.launched.ID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, held)

	// Wait until Episode 1's 500ms bound passes while Episode 2's 10m bound is still live.
	time.Sleep(700 * time.Millisecond)

	// Live send m3 must STILL queue behind m2 via holdOrDispatch without calling the broker!
	fresh, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	res3, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    fresh,
		ProjectID:      fresh.ProjectID,
		Msg:            "rv3o1-ep2-live-3",
		Type:           messages.TypeInstruction,
	})
	require.Nil(t, dmErr)
	assert.Equal(t, 0, f.disp.dispatchCount(f.launched.Slug), "RV3-O1: live send must still queue behind Episode 2 held row after Episode 1 bound passed")
	got3 := f.waitMessageState(t, res3.MessageID, store.MessageDispatchAwaitingSession)
	assert.Equal(t, store.MessageDispatchAwaitingSession, got3.DispatchState)

	// Connect session: m2 and m3 deliver in order, and m1 fails on deadline expiry.
	f.connectDeliverSession(t, f.public.URL, true, nil)
	f.waitMessageState(t, m2.ID, store.MessageDispatchDelivered)
	f.waitMessageState(t, res3.MessageID, store.MessageDispatchDelivered)
	f.waitMessageState(t, m1.ID, store.MessageDispatchFailed)

	var deliveredIDs []string
	for _, d := range f.disp.deliveries() {
		deliveredIDs = append(deliveredIDs, d.MessageIDs...)
	}
	assert.Equal(t, []string{m2.ID, res3.MessageID}, deliveredIDs)
}

type failPrepareHoldStore struct {
	store.Store
}

func (s *failPrepareHoldStore) PrepareOrHoldMessage(_ context.Context, _, _ string, _ int) (store.PrepareOrHoldResult, error) {
	return store.PrepareOrHoldResult{}, errors.New("raw sqlite internal constraint detail xyz")
}

func TestHoldA2_Round3_RV3N2_HoldOrDispatchErrorUsesFixedReason(t *testing.T) {
	f := newHoldE2EFixture(t)
	ctx := context.Background()

	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *failPrepareHoldStore {
		return &failPrepareHoldStore{Store: inner}
	})

	fresh, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	_, dmErr := f.srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    f.agent,
		SenderIdentity: &storedAgentIdentity{agent: f.agent},
		TargetAgent:    fresh,
		ProjectID:      fresh.ProjectID,
		Msg:            "rv3n2-prepare-fault",
		Type:           messages.TypeInstruction,
	})
	require.NotNil(t, dmErr)

	msgs, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.launched.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	require.Equal(t, store.MessageDispatchFailed, msgs.Items[0].DispatchState)
	require.NotNil(t, msgs.Items[0].DispatchFailureReason)
	assert.Equal(t, holdFailReasonPrepareFailed, *msgs.Items[0].DispatchFailureReason)
	assert.NotContains(t, *msgs.Items[0].DispatchFailureReason, "raw sqlite internal constraint detail xyz")
}
