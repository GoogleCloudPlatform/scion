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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// okTmux is a deliver-message Runner where every tmux command succeeds.
type okTmux struct {
	mu    sync.Mutex
	calls int
}

func (o *okTmux) Run(context.Context, []string, io.Reader) (string, error) {
	o.mu.Lock()
	o.calls++
	o.mu.Unlock()
	return "", nil
}

// deliverFixture is a hub with conduit and hub.conduit_deliver on, its
// launched agent's broker, and message rows addressed to that agent.
type deliverFixture struct {
	*ptyConduitFixture
	broker string
}

func newDeliverFixture(t *testing.T) *deliverFixture {
	t.Helper()
	f := &deliverFixture{ptyConduitFixture: newPTYConduitFixture(t)}
	f.broker = f.launched.RuntimeBrokerID
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: true})
	return f
}

// msg persists a message row to the launched agent in state.
func (f *deliverFixture) msg(t *testing.T, state string) string {
	t.Helper()
	m := &store.Message{
		ID: uuid.NewString(), ProjectID: f.launched.ProjectID, Sender: "user:alice", SenderID: uuid.NewString(),
		Recipient: "agent:" + f.launched.Slug, RecipientID: f.launched.ID, Msg: "hello", Type: "instruction",
		AgentID: f.launched.ID, DispatchState: state,
	}
	require.NoError(t, f.store.CreateMessage(context.Background(), m))
	return m.ID
}

func (f *deliverFixture) state(t *testing.T, id string) string {
	t.Helper()
	m, err := f.store.GetMessage(context.Background(), id)
	require.NoError(t, err)
	return m.DispatchState
}

func deliverReq(deliveryID string, ids ...string) agentcontrol.DeliverRequest {
	text := "hello"
	return agentcontrol.DeliverRequest{
		V: 1, DeliveryID: deliveryID, MessageIDs: ids, Op: agentcontrol.OpPaste, Text: &text,
		DeadlineMS: 92000, SubmitKeys: []string{"Enter"},
	}
}

// postDeliver calls the /deliver route through the broker routing, as the
// broker identity callerBroker ("" for none).
func (f *deliverFixture) postDeliver(t *testing.T, callerBroker, agentID string, body any) (int, agentcontrol.DeliverResponse) {
	t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+f.broker+"/agents/"+agentID+"/deliver", bytes.NewReader(raw))
	if callerBroker != "" {
		r = r.WithContext(contextWithBrokerIdentity(r.Context(), NewBrokerIdentity(callerBroker)))
	}
	rr := httptest.NewRecorder()
	f.srv.handleRuntimeBrokerRoutes(rr, r)
	var resp agentcontrol.DeliverResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp
}

// capture records what reached the agent's control handler.
type capture struct {
	mu   sync.Mutex
	reqs []*conduitv1.RpcRequest
}

func (c *capture) wrap(status int, body string) func(conduit.RPCHandler) conduit.RPCHandler {
	return func(next conduit.RPCHandler) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			c.mu.Lock()
			c.reqs = append(c.reqs, req)
			c.mu.Unlock()
			if status == 0 {
				return next.HandleRPC(ctx, req)
			}
			return &conduitv1.RpcResponse{Status: int32(status), Body: []byte(body)}
		})
	}
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// startDeliverAgent connects the launched agent with deliver-message
// registered; status 0 serves the real route, anything else answers that
// status and body.
func (f *deliverFixture) startDeliverAgent(t *testing.T, status int, body string) (*capture, *okTmux) {
	t.Helper()
	c := &capture{}
	tm := &okTmux{}
	f.startControlAgentWithDeliver(t, f.public.URL, true, &control.DeliverOptions{Runner: tm}, c.wrap(status, body))
	return c, tm
}

// TestBrokerAgentDeliver_Authorization: the experiment, the broker
// identity and the agent's assignment are checked before anything else,
// each with a passing control.
func TestBrokerAgentDeliver_Authorization(t *testing.T) {
	f := newDeliverFixture(t)
	c, _ := f.startDeliverAgent(t, 0, "")
	id := f.msg(t, store.MessageDispatchDispatched)

	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true})
	code, _ := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d0", id))
	assert.Equal(t, http.StatusNotFound, code, "experiment off")
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: false, experiments.ConduitDeliver: true})
	code, _ = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d0", id))
	assert.Equal(t, http.StatusNotFound, code, "hub.conduit off")
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: true})

	code, _ = f.postDeliver(t, "", f.launched.ID, deliverReq("d0", id))
	assert.Equal(t, http.StatusForbidden, code, "no broker identity")
	code, _ = f.postDeliver(t, uuid.NewString(), f.launched.ID, deliverReq("d0", id))
	assert.Equal(t, http.StatusForbidden, code, "another broker's HMAC")

	other := *f.launched
	other.ID, other.Slug, other.RuntimeBrokerID = uuid.NewString(), "other", uuid.NewString()
	other.RunID = ""
	require.NoError(t, f.store.CreateAgent(context.Background(), &other))
	code, _ = f.postDeliver(t, f.broker, other.ID, deliverReq("d0", id))
	assert.Equal(t, http.StatusForbidden, code, "agent assigned to another broker")
	assert.Equal(t, 0, c.count(), "nothing reached the agent")
	assert.Equal(t, store.MessageDispatchDispatched, f.state(t, id))

	code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d0", id))
	require.Equal(t, http.StatusOK, code, "positive control")
	assert.Equal(t, agentcontrol.OutcomeDelivered, resp.Outcome)
}

// TestBrokerAgentDeliver_Delivered: the hub binds the rows, overwrites
// broker_id and not_after_unix_ms, calls exactly deliver-message, and
// records delivered; a same-id replay returns the stored outcome without
// calling the agent.
func TestBrokerAgentDeliver_Delivered(t *testing.T) {
	f := newDeliverFixture(t)
	c, tm := f.startDeliverAgent(t, 0, "")
	a, b := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchDispatched)
	req := deliverReq("d1", a, b)
	req.BrokerID = "forged-broker"
	req.NotAfterUnixMS = 1
	before := time.Now()
	code, resp := f.postDeliver(t, f.broker, f.launched.ID, req)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, agentcontrol.OutcomeDelivered, resp.Outcome)
	assert.False(t, resp.Replayed)
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, a))
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, b))
	assert.Positive(t, tm.calls)

	require.Equal(t, 1, c.count())
	got := c.reqs[0]
	assert.Equal(t, "/v1/control/deliver-message", got.GetPath())
	assert.Equal(t, "application/json", got.GetHeaders()["Content-Type"])
	var sent agentcontrol.DeliverRequest
	require.NoError(t, json.Unmarshal(got.GetBody(), &sent))
	assert.Equal(t, f.broker, sent.BrokerID, "broker_id overwritten by the hub")
	notAfter := time.UnixMilli(sent.NotAfterUnixMS)
	assert.WithinDuration(t, before.Add(92*time.Second), notAfter, 5*time.Second, "not_after set by the hub")

	code, resp = f.postDeliver(t, f.broker, f.launched.ID, req)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.Replayed)
	assert.Equal(t, agentcontrol.OutcomeDelivered, resp.Outcome)
	assert.Equal(t, 1, c.count(), "the replay did not call the agent")
}

// TestBrokerAgentDeliver_BindRules: rows bound to another delivery, a
// mixed set, unknown or other-agent rows and empty ids never reach the
// agent.
func TestBrokerAgentDeliver_BindRules(t *testing.T) {
	f := newDeliverFixture(t)
	c, _ := f.startDeliverAgent(t, 200, `{"outcome":"partial","stage":"submit"}`)
	a := f.msg(t, store.MessageDispatchDispatched)
	code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", a))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, agentcontrol.OutcomePartial, resp.Outcome)
	assert.Equal(t, store.MessageDispatchPartial, f.state(t, a))
	require.Equal(t, 1, c.count())

	code, resp = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d2", a))
	assert.Equal(t, http.StatusConflict, code, "another id on a final row")
	assert.Equal(t, agentcontrol.OutcomeBound, resp.Outcome)
	code, resp = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", a))
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, resp.Replayed, "same id on a final row replays")
	assert.Equal(t, agentcontrol.OutcomePartial, resp.Outcome)

	mixedOK, mixedBad := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchFailed)
	code, _ = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d3", mixedOK, mixedBad))
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, store.MessageDispatchDispatched, f.state(t, mixedOK), "the whole bind rolled back")

	code, resp = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d4", uuid.NewString()))
	assert.Equal(t, http.StatusBadRequest, code, "unknown message id")
	assert.Equal(t, agentcontrol.FieldMessageIDs, resp.Field)

	code, _ = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d5"))
	assert.Equal(t, http.StatusBadRequest, code, "empty message_ids")

	// The request cannot name a route, a path or a target.
	for _, field := range []string{"route", "path", "target", "argv"} {
		var m map[string]any
		raw, _ := json.Marshal(deliverReq("d6", mixedOK))
		_ = json.Unmarshal(raw, &m)
		m[field] = "exec-start"
		code, resp = f.postDeliver(t, f.broker, f.launched.ID, m)
		assert.Equal(t, http.StatusBadRequest, code, field)
		assert.Equal(t, agentcontrol.FieldBody, resp.Field, field)
	}
	assert.Equal(t, 1, c.count(), "no refused request reached the agent")
}

// TestBrokerAgentDeliver_PendingRowBinds (G1): an interrupt DM is delivered
// inside the broker call, while its row is still pending.
func TestBrokerAgentDeliver_PendingRowBinds(t *testing.T) {
	f := newDeliverFixture(t)
	f.startDeliverAgent(t, 0, "")
	id := f.msg(t, store.MessageDispatchPending)
	req := deliverReq("d1", id)
	req.Op = agentcontrol.OpInterruptPaste
	req.InterruptKeys = []string{"C-c"}
	code, resp := f.postDeliver(t, f.broker, f.launched.ID, req)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, agentcontrol.OutcomeDelivered, resp.Outcome)
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, id))
}

// TestBrokerAgentDeliver_NothingPastedUnbinds: every answer that proves
// nothing was pasted returns the rows to dispatched, so a retry or the
// exec path can proceed; the agent's answer is passed through.
func TestBrokerAgentDeliver_NothingPastedUnbinds(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{400, `{"outcome":"invalid","field":"text"}`},
		{404, `404 page not found`},
		{408, `{"outcome":"expired_before_paste"}`},
		{409, `{"outcome":"busy"}`},
		{413, ``},
		{422, `{"outcome":"not_delivered","stage":"target"}`},
		{429, ``},
		{501, ``},
		{503, ``},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newDeliverFixture(t)
			f.startDeliverAgent(t, tc.status, tc.body)
			id := f.msg(t, store.MessageDispatchDispatched)
			code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
			assert.Equal(t, tc.status, code)
			assert.NotEmpty(t, resp.Outcome, "the broker always gets an outcome")
			assert.Equal(t, store.MessageDispatchDispatched, f.state(t, id))
			// After the unbind the rows bind again.
			code, _ = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d2", id))
			assert.Equal(t, tc.status, code, "a retry binds again")
		})
	}
	t.Run("no session", func(t *testing.T) {
		f := newDeliverFixture(t)
		id := f.msg(t, store.MessageDispatchDispatched)
		code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Equal(t, deliverOutcomeNotReached, resp.Outcome)
		assert.Equal(t, store.MessageDispatchDispatched, f.state(t, id))
	})
	t.Run("route not advertised", func(t *testing.T) {
		f := newDeliverFixture(t)
		ca := f.startControlAgent(t, f.public.URL, true, nil) // rotate-token only
		id := f.msg(t, store.MessageDispatchDispatched)
		code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Equal(t, deliverOutcomeNotReached, resp.Outcome)
		assert.Equal(t, store.MessageDispatchDispatched, f.state(t, id))
		assert.Zero(t, ca.rpcs.Load())
	})
}

// TestBrokerAgentDeliver_MayHavePasted: in_progress, unexpected answers and
// a timeout after send leave the rows partial, never dispatched.
func TestBrokerAgentDeliver_MayHavePasted(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		wantCode   int
	}{
		{"in progress", `{"outcome":"in_progress"}`, 409, 409},
		{"500", `boom`, 500, http.StatusBadGateway},
		{"200 without outcome", `{}`, 200, http.StatusBadGateway},
		{"200 not json", `ok`, 200, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliverFixture(t)
			f.startDeliverAgent(t, tc.status, tc.body)
			id := f.msg(t, store.MessageDispatchDispatched)
			code, _ := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, store.MessageDispatchPartial, f.state(t, id))
		})
	}
	t.Run("timeout after send", func(t *testing.T) {
		orig := hubDeliverTimeout
		hubDeliverTimeout = 300 * time.Millisecond
		t.Cleanup(func() { hubDeliverTimeout = orig })
		f := newDeliverFixture(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.startControlAgentWithDeliver(t, f.public.URL, true, &control.DeliverOptions{Runner: &okTmux{}}, func(conduit.RPCHandler) conduit.RPCHandler {
			return conduit.RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return &conduitv1.RpcResponse{Status: 200}
			})
		})
		id := f.msg(t, store.MessageDispatchDispatched)
		code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
		assert.Equal(t, http.StatusGatewayTimeout, code)
		assert.Equal(t, deliverOutcomeUnknown, resp.Outcome)
		assert.Equal(t, store.MessageDispatchPartial, f.state(t, id))
	})
}

// TestBrokerAgentDeliver_StaleSweep: a row left delivering past 220 s
// becomes partial on the broker message sweep, never dispatched; a fresh
// one is left alone.
func TestBrokerAgentDeliver_StaleSweep(t *testing.T) {
	f := newDeliverFixture(t)
	ctx := context.Background()
	stale, fresh := f.msg(t, store.MessageDispatchDispatched), f.msg(t, store.MessageDispatchDispatched)
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "old", []string{stale}, time.Now().Add(-5*time.Minute))
	require.NoError(t, err)
	_, err = f.store.BindMessageDelivery(ctx, f.launched.ID, "new", []string{fresh}, time.Now())
	require.NoError(t, err)
	f.srv.brokerMessageSweepHandler()(ctx)
	assert.Equal(t, store.MessageDispatchPartial, f.state(t, stale))
	assert.Equal(t, store.MessageDispatchDelivering, f.state(t, fresh))
}

// TestBrokerMessageFailures_PartialConduitRow (G5): a broker failure report
// fails a partial conduit row (so the sender is told), never a delivered
// one; the stored outcome still replays.
func TestBrokerMessageFailures_PartialConduitRow(t *testing.T) {
	f := newDeliverFixture(t)
	c, _ := f.startDeliverAgent(t, 200, `{"outcome":"partial","stage":"submit"}`)
	partial := f.msg(t, store.MessageDispatchDispatched)
	code, _ := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", partial))
	require.Equal(t, http.StatusOK, code)

	ctx := context.Background()
	delivered := f.msg(t, store.MessageDispatchDispatched)
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d2", []string{delivered}, time.Now())
	require.NoError(t, err)
	_, err = f.store.FinishMessageDelivery(ctx, "d2", []string{delivered}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
	require.NoError(t, err)

	rr, resp := postMessageFailures(t, f.srv, f.broker, f.broker,
		messageDeliveryFailure{MessageID: partial, Reason: "partial delivery over conduit"},
		messageDeliveryFailure{MessageID: delivered, Reason: "late"})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, resp.Marked)
	assert.Equal(t, store.MessageDispatchFailed, f.state(t, partial))
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, delivered))

	code, r2 := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", partial))
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, r2.Replayed)
	assert.Equal(t, 1, c.count())
}

// TestBrokerAgentDeliver_LogsNoText: the hub's info line names sizes and
// the outcome, never the text.
func TestBrokerAgentDeliver_LogsNoText(t *testing.T) {
	f := newDeliverFixture(t)
	var buf bytes.Buffer
	f.srv.messageLog = slog.New(slog.NewJSONHandler(&buf, nil))
	f.startDeliverAgent(t, 0, "")
	id := f.msg(t, store.MessageDispatchDispatched)
	req := deliverReq("d1", id)
	text := "SENTINEL-HUB-TEXT-7353"
	req.Text = &text
	code, _ := f.postDeliver(t, f.broker, f.launched.ID, req)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, buf.String(), "message delivery over conduit")
	assert.NotContains(t, buf.String(), "SENTINEL-HUB-TEXT")
	assert.False(t, strings.Contains(buf.String(), `"Enter"`))
}

// TestBrokerAgentDeliver_ConcurrentSameIDHasOneOwner (review R1): while one
// call owns a delivery, a same-id duplicate (a re-send, or a call on
// another hub replica) gets 409 in_progress without reaching the agent, and
// can never unbind the rows. After the owner pastes, a fresh id is refused
// and the row is pasted exactly once.
func TestBrokerAgentDeliver_ConcurrentSameIDHasOneOwner(t *testing.T) {
	f := newDeliverFixture(t)
	tm := &okTmux{}
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	f.startControlAgentWithDeliver(t, f.public.URL, true, &control.DeliverOptions{Runner: tm}, func(next conduit.RPCHandler) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(ctx context.Context, r *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				close(entered)
				<-release
			} else {
				// Any later call would be a duplicate reaching the agent;
				// answer the kind of refusal that used to unbind.
				return &conduitv1.RpcResponse{Status: 503}
			}
			return next.HandleRPC(ctx, r)
		})
	})
	id := f.msg(t, store.MessageDispatchPending)
	done := make(chan int, 1)
	go func() {
		code, _ := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
		done <- code
	}()
	<-entered
	code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
	assert.Equal(t, http.StatusConflict, code, "duplicate while in flight")
	assert.Equal(t, agentcontrol.OutcomeInProgress, resp.Outcome, "an answer the broker never falls back on")
	assert.Equal(t, store.MessageDispatchDelivering, f.state(t, id), "the duplicate did not unbind")
	close(release)
	require.Equal(t, http.StatusOK, <-done)
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, id))

	code, resp = f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d2", id))
	assert.Equal(t, http.StatusConflict, code, "a fresh id must not paste a delivered row")
	assert.Equal(t, agentcontrol.OutcomeBound, resp.Outcome)
	mu.Lock()
	assert.Equal(t, 1, calls, "one call reached the agent")
	mu.Unlock()
	assert.Equal(t, 4, tm.calls, "exactly one four-step flush (has-session, load, paste, submit)")
}

// TestBrokerAgentDeliver_OwnerUnbindOnlyAfterDuplicateRefused (review R1,
// every unbinding status): when the owner's own call proves nothing was
// pasted, only the owner unbinds; a duplicate that arrived meanwhile was
// refused in_progress, and the owner's same-id retry then binds again.
func TestBrokerAgentDeliver_OwnerUnbindOnlyAfterDuplicateRefused(t *testing.T) {
	for _, status := range []int{408, 409, 413, 422, 429, 501, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newDeliverFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			body := `{"outcome":"busy"}`
			f.startControlAgentWithDeliver(t, f.public.URL, true, &control.DeliverOptions{Runner: &okTmux{}}, func(conduit.RPCHandler) conduit.RPCHandler {
				return conduit.RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
					first := false
					once.Do(func() { first = true })
					if first {
						close(entered)
						<-release
					}
					return &conduitv1.RpcResponse{Status: int32(status), Body: []byte(body)}
				})
			})
			id := f.msg(t, store.MessageDispatchDispatched)
			done := make(chan int, 1)
			go func() {
				code, _ := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
				done <- code
			}()
			<-entered
			code, resp := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d1", id))
			assert.Equal(t, http.StatusConflict, code)
			assert.Equal(t, agentcontrol.OutcomeInProgress, resp.Outcome)
			assert.Equal(t, store.MessageDispatchDelivering, f.state(t, id))
			close(release)
			assert.Equal(t, status, <-done)
			assert.Equal(t, store.MessageDispatchDispatched, f.state(t, id), "the owner unbinds")
		})
	}
}

// TestBrokerAgentDeliver_SizeLimitIs413 (review R3): text over the route
// limit, raw or JSON-encoded, and more than 64 ids are 413 too_large (the
// broker's exec fallback), never the permanent 400; nothing is bound.
func TestBrokerAgentDeliver_SizeLimitIs413(t *testing.T) {
	f := newDeliverFixture(t)
	c, _ := f.startDeliverAgent(t, 0, "")
	id := f.msg(t, store.MessageDispatchDispatched)
	raw := deliverReq("d1", id)
	big := strings.Repeat("x", agentcontrol.MaxDeliverText+1)
	raw.Text = &big
	encoded := deliverReq("d2", id)
	angle := strings.Repeat("<", 200*1024)
	encoded.Text = &angle
	ids := make([]string, 65)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	many := deliverReq("d3", ids...)
	for name, req := range map[string]agentcontrol.DeliverRequest{"raw text": raw, "encoded text": encoded, "65 ids": many} {
		code, resp := f.postDeliver(t, f.broker, f.launched.ID, req)
		assert.Equal(t, http.StatusRequestEntityTooLarge, code, name)
		assert.Equal(t, agentcontrol.OutcomeTooLarge, resp.Outcome, name)
	}
	assert.Equal(t, store.MessageDispatchDispatched, f.state(t, id))
	assert.Equal(t, 0, c.count())
}

// TestBrokerMessageFailures_NeverFailsDelivered (review R4): a failure
// report for a row the delivery finished as delivered changes nothing and
// sends no notice; one for a row still delivering wins, stores a
// replayable outcome, and the delivery's late finish loses.
func TestBrokerMessageFailures_NeverFailsDelivered(t *testing.T) {
	f := newDeliverFixture(t)
	ctx := context.Background()
	delivered := f.msg(t, store.MessageDispatchPending)
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d1", []string{delivered}, time.Now())
	require.NoError(t, err)
	_, err = f.store.FinishMessageDelivery(ctx, "d1", []string{delivered}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
	require.NoError(t, err)
	inflight := f.msg(t, store.MessageDispatchDispatched)
	_, err = f.store.BindMessageDelivery(ctx, f.launched.ID, "d2", []string{inflight}, time.Now())
	require.NoError(t, err)

	rr, resp := postMessageFailures(t, f.srv, f.broker, f.broker,
		messageDeliveryFailure{MessageID: delivered, Reason: "late"},
		messageDeliveryFailure{MessageID: inflight, Reason: "broker gave up"})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, resp.Marked)
	assert.Equal(t, 1, resp.Ignored)
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, delivered))
	assert.Equal(t, store.MessageDispatchFailed, f.state(t, inflight))

	n, err := f.store.FinishMessageDelivery(ctx, "d2", []string{inflight}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
	require.NoError(t, err)
	assert.Zero(t, n)
	code, r := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d2", inflight))
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, r.Replayed)
	assert.Equal(t, agentcontrol.OutcomePartial, r.Outcome)
}

// TestExecuteAgentDMFinalization_KeepsDelivered (review R4): the agent-DM
// failure finalization never turns a delivered row into failed, and a row
// still delivering keeps a replayable outcome.
func TestExecuteAgentDMFinalization_KeepsDelivered(t *testing.T) {
	f := newDeliverFixture(t)
	ctx := context.Background()
	delivered := f.msg(t, store.MessageDispatchPending)
	_, err := f.store.BindMessageDelivery(ctx, f.launched.ID, "d1", []string{delivered}, time.Now())
	require.NoError(t, err)
	_, err = f.store.FinishMessageDelivery(ctx, "d1", []string{delivered}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
	require.NoError(t, err)
	require.NoError(t, f.srv.markFailed(ctx, delivered, "broker said failed"))
	assert.Equal(t, store.MessageDispatchDelivered, f.state(t, delivered))

	inflight := f.msg(t, store.MessageDispatchPending)
	_, err = f.store.BindMessageDelivery(ctx, f.launched.ID, "d2", []string{inflight}, time.Now())
	require.NoError(t, err)
	require.NoError(t, f.srv.markFailed(ctx, inflight, "dispatch failed"))
	assert.Equal(t, store.MessageDispatchFailed, f.state(t, inflight))
	code, r := f.postDeliver(t, f.broker, f.launched.ID, deliverReq("d2", inflight))
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, r.Replayed)
}
