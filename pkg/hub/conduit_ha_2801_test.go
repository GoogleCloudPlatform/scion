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

//go:build integration

package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// --- Local helpers for #2801 broker HA rows ---

// conduitHA2801DoHTTP executes an authenticated owner HTTP request directly
// against node.Server.Handler() and returns the recorder.
func conduitHA2801DoHTTP(t *testing.T, node *conduitHANode, user *AuthenticatedUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	tok, _, _, err := node.Server.userTokenService.GenerateTokenPair(user.ID(), user.Email(), user.DisplayName(), user.Role(), ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	node.Server.Handler().ServeHTTP(rec, req)
	return rec
}

// conduitHA2801BrokerSession is a raw broker conduit session dialed directly
// to a hub node with explicit incarnation, exec_scope, and RPC handler so
// tests can control incarnation, call GoAway from the broker side, or send
// a malformed Hello without modifying the shared fake broker helper.
type conduitHA2801BrokerSession struct {
	Session   conduit.LocalSession
	Welcome   *conduitv1.Welcome
	SessionID string
}

// conduitHA2801DialBroker dials /api/v1/conduit on node with HMAC broker auth
// and the given Hello capabilities, returning the live session and Welcome.
func conduitHA2801DialBroker(
	t *testing.T,
	node *conduitHANode,
	brokerID string,
	secret []byte,
	incarnation string,
	execScope string,
	rpcHandler conduit.RPCHandler,
) (*conduitHA2801BrokerSession, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(node.Public.URL, "http") + "/api/v1/conduit"
	d := &ws.Dialer{
		URL: u,
		Header: func(context.Context) (http.Header, error) {
			req, err := http.NewRequest(http.MethodGet, node.Public.URL+"/api/v1/conduit", nil)
			if err != nil {
				return nil, err
			}
			if err := (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req); err != nil {
				return nil, err
			}
			return req.Header, nil
		},
	}

	var rpcList []string
	if rpcHandler != nil {
		rpcList = conduit.BrokerRPCs()
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		ClientVersion: "test-2801",
		Capabilities: &conduitv1.Capabilities{
			Rpc:                 rpcList,
			EndpointIncarnation: incarnation,
			ExecScope:           execScope,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, wel, err := conduit.Dial(ctx, d, conduit.Config{RPCHandler: rpcHandler}, hello)
	if err != nil {
		return nil, err
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return &conduitHA2801BrokerSession{
		Session:   ls,
		Welcome:   wel,
		SessionID: wel.GetSessionId(),
	}, nil
}

// conduitHA2801WaitLocal waits until sessionID is registered locally on node's
// relay and visible in Postgres.
func conduitHA2801WaitLocal(t *testing.T, node *conduitHANode, brokerID, sessionID string) conduit.LocalSession {
	t.Helper()
	var local conduit.LocalSession
	require.NoError(t, pollUntil(10*time.Second, func() error {
		ls, _, ok := node.Relay().Local(context.Background(), sessionID)
		if !ok {
			return fmt.Errorf("session %s not yet local on %s", sessionID, node.InstanceID)
		}
		ps, err := node.RegStore.ListPrincipalSessions(context.Background(), registry.PrincipalBroker, brokerID)
		if err != nil {
			return err
		}
		for _, v := range ps.Sessions {
			if v.Session.SessionID == sessionID {
				local = ls
				return nil
			}
		}
		return fmt.Errorf("session %s not yet visible in Postgres", sessionID)
	}))
	return local
}

// conduitHA2801ReqCounter tracks how many times each broker request ID was
// handled, plus total calls.
type conduitHA2801ReqCounter struct {
	mu      sync.Mutex
	byReqID map[string]int
	total   int
}

func newConduitHA2801ReqCounter() *conduitHA2801ReqCounter {
	return &conduitHA2801ReqCounter{byReqID: make(map[string]int)}
}

func (c *conduitHA2801ReqCounter) Inc(reqID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	c.byReqID[reqID]++
	return c.byReqID[reqID]
}

func (c *conduitHA2801ReqCounter) Count(reqID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byReqID[reqID]
}

func (c *conduitHA2801ReqCounter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *conduitHA2801ReqCounter) UniqueReqIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byReqID))
	for k := range c.byReqID {
		out = append(out, k)
	}
	return out
}

// conduitHA2801LogCapture is a thread-safe slog.Handler that records all log
// records while forwarding to an inner handler.
type conduitHA2801LogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

type conduitHA2801LogCapture struct {
	mu      sync.Mutex
	records []conduitHA2801LogRecord
	attrs   []slog.Attr
	inner   slog.Handler
}

func newConduitHA2801LogCapture(inner slog.Handler) *conduitHA2801LogCapture {
	return &conduitHA2801LogCapture{inner: inner}
}

func (h *conduitHA2801LogCapture) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *conduitHA2801LogCapture) Handle(ctx context.Context, r slog.Record) error {
	m := make(map[string]string)
	for _, a := range h.attrs {
		m[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, conduitHA2801LogRecord{
		Level: r.Level,
		Msg:   r.Message,
		Attrs: m,
	})
	h.mu.Unlock()
	if h.inner != nil && h.inner.Enabled(ctx, r.Level) {
		return h.inner.Handle(ctx, r)
	}
	return nil
}

func (h *conduitHA2801LogCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	combined := append(append([]slog.Attr(nil), h.attrs...), attrs...)
	var inner slog.Handler
	if h.inner != nil {
		inner = h.inner.WithAttrs(attrs)
	}
	return &conduitHA2801LogCaptureChild{root: h, attrs: combined, inner: inner}
}

func (h *conduitHA2801LogCapture) WithGroup(name string) slog.Handler {
	var inner slog.Handler
	if h.inner != nil {
		inner = h.inner.WithGroup(name)
	}
	return &conduitHA2801LogCaptureChild{root: h, attrs: append([]slog.Attr(nil), h.attrs...), inner: inner}
}

type conduitHA2801LogCaptureChild struct {
	root  *conduitHA2801LogCapture
	attrs []slog.Attr
	inner slog.Handler
}

func (c *conduitHA2801LogCaptureChild) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (c *conduitHA2801LogCaptureChild) Handle(ctx context.Context, r slog.Record) error {
	m := make(map[string]string)
	for _, a := range c.attrs {
		m[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	c.root.mu.Lock()
	c.root.records = append(c.root.records, conduitHA2801LogRecord{
		Level: r.Level,
		Msg:   r.Message,
		Attrs: m,
	})
	c.root.mu.Unlock()
	if c.inner != nil && c.inner.Enabled(ctx, r.Level) {
		return c.inner.Handle(ctx, r)
	}
	return nil
}

func (c *conduitHA2801LogCaptureChild) WithAttrs(attrs []slog.Attr) slog.Handler {
	combined := append(append([]slog.Attr(nil), c.attrs...), attrs...)
	var inner slog.Handler
	if c.inner != nil {
		inner = c.inner.WithAttrs(attrs)
	}
	return &conduitHA2801LogCaptureChild{root: c.root, attrs: combined, inner: inner}
}

func (c *conduitHA2801LogCaptureChild) WithGroup(name string) slog.Handler {
	var inner slog.Handler
	if c.inner != nil {
		inner = c.inner.WithGroup(name)
	}
	return &conduitHA2801LogCaptureChild{root: c.root, attrs: append([]slog.Attr(nil), c.attrs...), inner: inner}
}

func (h *conduitHA2801LogCapture) Matching(substr string) []conduitHA2801LogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []conduitHA2801LogRecord
	for _, rec := range h.records {
		if strings.Contains(rec.Msg, substr) {
			out = append(out, rec)
		}
	}
	return out
}

// conduitHA2801BrokerIncarnationMetricReader wires OTel broker incarnation
// metrics on s and returns a helper to read a counter total by broker_id.
func conduitHA2801BrokerIncarnationMetricReader(t *testing.T, s *Server) func(name, brokerID string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelConduitBrokerIncarnationMetrics(mp)
	require.NoError(t, err)
	s.SetConduitBrokerIncarnationMetrics(rec)
	return func(name, brokerID string) int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != name {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value("broker_id"); !ok || v.AsString() != brokerID {
						continue
					}
					total += dp.Value
				}
			}
		}
		return total
	}
}

// --- #2801 H Rows ---

// TestConduitHA_2801_C2_BrokerPathCrossNode validates ptone/scion#2801
// checkbox:
//
//	[ ] C2 (broker path cross-node), C12 (brokers), with evidence in acceptance.md
//
// The fake broker holds its conduit session on Hub B while broker RPCs
// (GET /logs and POST /exec) land on Hub A. Hub A resolves the broker to a
// *relay.RemoteSession on Hub B, forwards each RPC across the internal peer
// listener (incrementing Hub B's relay hop counter), and returns the broker's
// response.
func TestConduitHA_2801_C2_BrokerPathCrossNode(t *testing.T) {
	env := newConduitHAEnv(t)
	ctx := context.Background()
	env.HubA.EnableBrokerRouting(t)

	counter := newConduitHA2801ReqCounter()
	var seenSessionHeaders sync.Map
	rpcHandler := conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		envReq := envelope.RPCToRequest(req)
		counter.Inc(envReq.RequestID)
		seenSessionHeaders.Store(envReq.RequestID, req.GetHeaders()[conduit.BrokerRPCSessionHeader])
		switch {
		case strings.HasSuffix(envReq.Path, "/logs"):
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"logs":"cross-node-broker-logs"}`),
			))
		case strings.HasSuffix(envReq.Path, "/exec"):
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"output":"cross-node-broker-exec","exitCode":0}`),
			))
		default:
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusNotFound, nil, nil,
			))
		}
	})

	// Connect fake broker to Hub B only.
	_ = env.StartBroker(t, env.HubB, rpcHandler)

	// Confirm Hub A resolves the broker to a RemoteSession owned by Hub B.
	var resolved router.Resolved
	require.NoError(t, pollUntil(5*time.Second, func() error {
		var err error
		resolved, err = env.HubA.Router().Resolve(ctx, router.Request{
			Op:   router.OpStatefulRPC,
			Kind: registry.PrincipalBroker,
			ID:   env.BrokerID,
			Want: registry.Want{Capability: conduit.RPCAgentLogs},
		}, nil)
		return err
	}))
	require.False(t, resolved.Local, "Hub A must resolve the broker session as remote")
	assert.Equal(t, env.HubB.InstanceID, resolved.Record.RelayInstanceID)
	_, isRemote := resolved.Session.(*relay.RemoteSession)
	assert.True(t, isRemote, "resolved session must be *relay.RemoteSession, got %T", resolved.Session)

	hopsBeforeB := env.HubB.InternalRPCHops()
	hopsBeforeA := env.HubA.InternalRPCHops()

	// 1. Idempotent broker RPC (GET /logs) lands on Hub A.
	logsRec := conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodGet, "/api/v1/agents/"+env.Agent.ID+"/logs?tail=10", nil)
	require.Equal(t, http.StatusOK, logsRec.Code, logsRec.Body.String())
	assert.Contains(t, logsRec.Body.String(), "cross-node-broker-logs")
	assert.Equal(t, hopsBeforeB+1, env.HubB.InternalRPCHops(), "Hub B internal listener must handle 1 relayed RPC hop for logs")
	assert.Equal(t, hopsBeforeA, env.HubA.InternalRPCHops(), "Hub A internal listener must not handle an RPC hop")

	// 2. Non-idempotent broker RPC (POST /exec) lands on Hub A.
	execRec := conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodPost, "/api/v1/agents/"+env.Agent.ID+"/exec", map[string]any{
		"command": []string{"echo", "ok"},
	})
	require.Equal(t, http.StatusOK, execRec.Code, execRec.Body.String())
	assert.Contains(t, execRec.Body.String(), "cross-node-broker-exec")
	assert.Equal(t, hopsBeforeB+2, env.HubB.InternalRPCHops(), "Hub B internal listener must handle 2 relayed RPC hops total")
	assert.Equal(t, hopsBeforeA, env.HubA.InternalRPCHops())

	// Each request was handled exactly once and carried Hub B's session ID header.
	assert.Equal(t, 2, counter.Total())
	for _, reqID := range counter.UniqueReqIDs() {
		assert.Equal(t, 1, counter.Count(reqID), "request %s handled once", reqID)
		hdr, _ := seenSessionHeaders.Load(reqID)
		assert.Equal(t, resolved.Record.SessionID, hdr, "request %s signed for Hub B session", reqID)
	}
}

// TestConduitHA_2801_C20_AmbiguousAndRetrySemantics validates ptone/scion#2801
// checkbox:
//
//	[ ] C20, including: a session lost with a non-idempotent request in flight
//	    gives 502 outcome_unknown with no re-execution; an idempotent request on
//	    a lost session is retried on another session; a request not yet written
//	    is re-resolved
//
// The fake broker counts per-request-id handling:
//   - (a) non-idempotent POST /exec in flight on lost session -> 502
//     outcome_unknown, handled exactly once even with a second live session;
//   - (b) idempotent GET /logs in flight on lost session -> retried on the
//     other live session, 2 attempts and 1 success;
//   - (c) non-idempotent POST /exec whose first resolved session fails before
//     write (owner relay stale-route not_local) -> re-resolved to live session,
//     handled exactly once, 200 OK.
func TestConduitHA_2801_C20_AmbiguousAndRetrySemantics(t *testing.T) {
	t.Run("a_non_idempotent_in_flight_lost", func(t *testing.T) {
		env := newConduitHAEnv(t)
		env.HubA.EnableBrokerRouting(t)

		incarnation := "broker-c20a-" + uuid.NewString()[:8]
		counter := newConduitHA2801ReqCounter()
		entered := make(chan string, 2)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		handler := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			envReq := envelope.RPCToRequest(req)
			counter.Inc(envReq.RequestID)
			sessID := req.GetHeaders()[conduit.BrokerRPCSessionHeader]
			select {
			case entered <- sessID:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"output":"should-not-be-delivered","exitCode":0}`),
			))
		})

		// Dial two live sessions of the same broker and incarnation on Hub A
		// and Hub B so both are EpochCurrent and eligible simultaneously.
		sA, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, incarnation, "", handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, sA.SessionID)

		sB, err := conduitHA2801DialBroker(t, env.HubB, env.BrokerID, env.BrokerSecret, incarnation, "", handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubB, env.BrokerID, sB.SessionID)

		recCh := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recCh <- conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodPost, "/api/v1/agents/"+env.Agent.ID+"/exec", map[string]any{
				"command": []string{"touch", "non-idempotent"},
			})
		}()

		var activeSessionID string
		select {
		case activeSessionID = <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("non-idempotent exec never reached broker")
		}

		// Close only the session that received the in-flight request; the
		// other session stays healthy and eligible.
		var survivor conduit.LocalSession
		if activeSessionID == sA.SessionID {
			env.CloseSession(t, env.HubA, sA.SessionID)
			survivor = sB.Session
		} else {
			env.CloseSession(t, env.HubB, sB.SessionID)
			survivor = sA.Session
		}

		var rec *httptest.ResponseRecorder
		select {
		case rec = <-recCh:
		case <-time.After(10 * time.Second):
			t.Fatal("exec HTTP call did not return after in-flight session closed")
		}

		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		var body struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, ErrCodeOutcomeUnknown, body.Error.Code)
		assert.Equal(t, brokerOutcomeUnknownMessage, body.Error.Message)
		assert.Equal(t, conduit.RPCAgentExec, body.Error.Details["operation"])
		assert.Equal(t, env.BrokerID, body.Error.Details["broker_id"])
		reqID, _ := body.Error.Details["request_id"].(string)
		assert.NotEmpty(t, reqID)
		assert.NotContains(t, rec.Body.String(), "should-not-be-delivered")

		// Surviving session is still open, yet the non-idempotent request ran exactly once.
		select {
		case <-survivor.Done():
			t.Fatal("surviving broker session should remain open")
		default:
		}
		assert.Equal(t, 1, counter.Total(), "non-idempotent request must not be re-executed on the surviving session")
		assert.Equal(t, 1, counter.Count(reqID), "per-request-id count must be 1")
	})

	t.Run("b_idempotent_in_flight_lost_retried", func(t *testing.T) {
		env := newConduitHAEnv(t)
		env.HubA.EnableBrokerRouting(t)

		incarnation := "broker-c20b-" + uuid.NewString()[:8]
		counter := newConduitHA2801ReqCounter()
		var attempts, successes atomic.Int64
		firstEntered := make(chan string, 1)
		holdFirst := make(chan struct{})
		t.Cleanup(func() { close(holdFirst) })

		handler := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			envReq := envelope.RPCToRequest(req)
			counter.Inc(envReq.RequestID)
			n := attempts.Add(1)
			if n == 1 {
				firstEntered <- req.GetHeaders()[conduit.BrokerRPCSessionHeader]
				select {
				case <-holdFirst:
				case <-ctx.Done():
				}
				return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
					envReq.RequestID, http.StatusOK,
					map[string]string{"Content-Type": "application/json"},
					[]byte(`{"logs":"dropped-first-attempt"}`),
				))
			}
			successes.Add(1)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"logs":"retried-second-attempt"}`),
			))
		})

		sA, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, incarnation, "", handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, sA.SessionID)

		sB, err := conduitHA2801DialBroker(t, env.HubB, env.BrokerID, env.BrokerSecret, incarnation, "", handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubB, env.BrokerID, sB.SessionID)

		recCh := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recCh <- conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodGet, "/api/v1/agents/"+env.Agent.ID+"/logs?tail=5", nil)
		}()

		var firstSessionID string
		select {
		case firstSessionID = <-firstEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("first idempotent logs attempt never reached broker")
		}

		// Drop the session holding the first in-flight attempt; Hub A must
		// retry on the other live session.
		if firstSessionID == sA.SessionID {
			env.CloseSession(t, env.HubA, sA.SessionID)
		} else {
			env.CloseSession(t, env.HubB, sB.SessionID)
		}

		var rec *httptest.ResponseRecorder
		select {
		case rec = <-recCh:
		case <-time.After(10 * time.Second):
			t.Fatal("retried logs HTTP call did not complete")
		}

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "retried-second-attempt")
		assert.Equal(t, int64(2), attempts.Load(), "idempotent request must see 2 attempts")
		assert.Equal(t, int64(1), successes.Load(), "idempotent request must see 1 success")
		reqIDs := counter.UniqueReqIDs()
		require.Len(t, reqIDs, 1, "both attempts must carry the same request_id")
		assert.Equal(t, 2, counter.Count(reqIDs[0]), "per-request-id handling count must be 2")
	})

	t.Run("c_not_yet_written_reresolved", func(t *testing.T) {
		env := newConduitHAEnv(t)
		ctx := context.Background()
		env.HubA.EnableBrokerRouting(t)

		incarnation := "broker-c20c-" + uuid.NewString()[:8]
		counter := newConduitHA2801ReqCounter()
		handler := conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			envReq := envelope.RPCToRequest(req)
			counter.Inc(envReq.RequestID)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"output":"reresolved-after-not-written","exitCode":0}`),
			))
		})

		// Dial one real live session on Hub A.
		sLive, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, incarnation, "", handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, sLive.SessionID)

		// Insert a stale session row on Hub B's live relay generation with a
		// newer LastSeen and higher ConnectionEpoch so Hub A's router picks it
		// first. Hub B does not hold this session locally, so Hub B's internal
		// listener rejects the hop before writing anything (404 not_local).
		relays, err := env.HubB.RegStore.ListRelays(ctx)
		require.NoError(t, err)
		var hubBRelay registry.RelayInstance
		var found bool
		for _, r := range relays {
			if r.InstanceID == env.HubB.InstanceID {
				hubBRelay = r
				found = true
				break
			}
		}
		require.True(t, found, "Hub B relay row must exist in Postgres")

		staleID := "stale-not-local-" + uuid.NewString()[:8]
		_, err = env.HubB.Server.conduit.Load().registry.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
			SessionID:           staleID,
			PrincipalKind:       registry.PrincipalBroker,
			PrincipalID:         env.BrokerID,
			RelayInstanceID:     env.HubB.InstanceID,
			RelayGeneration:     hubBRelay.Generation,
			Transport:           registry.TransportWS,
			EndpointIncarnation: incarnation,
			LastSeen:            time.Now().Add(2 * time.Second),
			Capabilities: registry.Capabilities{
				RPC:                 conduit.BrokerRPCs(),
				EndpointIncarnation: incarnation,
			},
		})
		require.NoError(t, err)

		hopsBeforeB := env.HubB.InternalRPCHops()

		// Send a NON-idempotent POST /exec to Hub A: because the first attempt
		// was refused before write (not_local), Hub A must re-resolve to sLive
		// and succeed rather than returning 502 outcome_unknown.
		rec := conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodPost, "/api/v1/agents/"+env.Agent.ID+"/exec", map[string]any{
			"command": []string{"echo", "once"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "reresolved-after-not-written")

		// Confirm Hub A actually attempted the stale row on Hub B first and
		// observed Hub B's not_local stale-route answer.
		assert.Equal(t, hopsBeforeB+1, env.HubB.InternalRPCHops(), "Hub A must have tried the stale Hub B session first")
		assert.Contains(t, env.HubB.InternalStaleReasons(), "not_local")

		// Broker handled the request exactly once.
		assert.Equal(t, 1, counter.Total())
		reqIDs := counter.UniqueReqIDs()
		require.Len(t, reqIDs, 1)
		assert.Equal(t, 1, counter.Count(reqIDs[0]), "per-request-id count must be 1")
	})
}

// TestConduitHA_2801_BrokerDrainAfterGoAwayCrossNode validates ptone/scion#2801
// checkbox:
//
//	[ ] Required live row: a broker session draining after its own GoAway on a
//	    node other than the one handling the request. In-flight requests on it
//	    complete within drain_deadline, new requests re-resolve to the
//	    replacement session on the same relay, and the owner relay's session
//	    stale-route answer is exercised end to end across nodes
//
// Subtest 1 runs the full cross-node GoAway drain:
//   - Broker session s1 is on Hub B; in-flight exec RPC 1 lands on Hub A.
//   - Broker sends GoAway(DrainDeadline: 5s) on s1; Hub B receives GoAway
//     while s1's Postgres row remains non-draining.
//   - New exec RPC 2 lands on Hub A -> hits s1 on Hub B -> Hub B answers 409
//     with X-Conduit-Stale-Reason: draining -> replacement session s2 dials
//     Hub B (same relay, same incarnation) -> Hub A re-resolves RPC 2 to s2
//     on Hub B and RPC 2 succeeds 200 OK.
//   - In-flight RPC 1 is released and completes 200 OK within drain_deadline,
//     after which s1 closes cleanly.
//
// Subtest 2 confirms an in-flight non-idempotent RPC that outlives a short
// drain_deadline fails with 502 outcome_unknown when the drain deadline fires.
func TestConduitHA_2801_BrokerDrainAfterGoAwayCrossNode(t *testing.T) {
	t.Run("in_flight_completes_and_new_reresolves_to_replacement_on_same_relay", func(t *testing.T) {
		env := newConduitHAEnv(t)
		env.HubA.EnableBrokerRouting(t)

		incarnation := "broker-goaway-" + uuid.NewString()[:8]
		inFlightEntered := make(chan struct{}, 1)
		releaseInFlight := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-releaseInFlight:
			default:
				close(releaseInFlight)
			}
		})

		var s1Count, s2Count atomic.Int64
		s1Handler := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			s1Count.Add(1)
			envReq := envelope.RPCToRequest(req)
			select {
			case inFlightEntered <- struct{}{}:
			default:
			}
			select {
			case <-releaseInFlight:
			case <-ctx.Done():
			}
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"output":"in-flight-on-s1-done","exitCode":0}`),
			))
		})
		s2Handler := conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			s2Count.Add(1)
			envReq := envelope.RPCToRequest(req)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"output":"new-req-on-s2-same-relay","exitCode":0}`),
			))
		})

		s1, err := conduitHA2801DialBroker(t, env.HubB, env.BrokerID, env.BrokerSecret, incarnation, "", s1Handler)
		require.NoError(t, err)
		hubBLocalS1 := conduitHA2801WaitLocal(t, env.HubB, env.BrokerID, s1.SessionID)

		// Start in-flight RPC 1 on Hub A -> routes cross-node to s1 on Hub B.
		rpc1Done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rpc1Done <- conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodPost, "/api/v1/agents/"+env.Agent.ID+"/exec", map[string]any{
				"command": []string{"sleep", "in-flight"},
			})
		}()
		select {
		case <-inFlightEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("in-flight RPC 1 never reached s1 on Hub B")
		}

		// Broker sends GoAway on s1; wait for Hub B's relay to see it.
		require.NoError(t, s1.Session.GoAway(conduit.GoAwayOptions{DrainDeadline: 5 * time.Second}))
		select {
		case <-hubBLocalS1.GoAwayReceived():
		case <-time.After(5 * time.Second):
			t.Fatal("Hub B did not receive broker GoAway on s1")
		}

		// When Hub B's internal listener answers 409 X-Conduit-Stale-Reason: draining
		// for s1, dial replacement session s2 on the SAME relay (Hub B) before
		// returning the response to Hub A so Hub A's immediate re-resolution
		// deterministically finds s2 on Hub B.
		var sawDrainingStale atomic.Bool
		var s2SessionID atomic.Value
		var dialOnce sync.Once
		env.HubB.SetOnInternalRPC(func(sessionID string, status int, staleReason string) {
			if sessionID == s1.SessionID && status == http.StatusConflict && staleReason == string(registry.ReasonDraining) {
				sawDrainingStale.Store(true)
				dialOnce.Do(func() {
					s2, derr := conduitHA2801DialBroker(t, env.HubB, env.BrokerID, env.BrokerSecret, incarnation, "", s2Handler)
					require.NoError(t, derr)
					conduitHA2801WaitLocal(t, env.HubB, env.BrokerID, s2.SessionID)
					s2SessionID.Store(s2.SessionID)
				})
			}
		})
		t.Cleanup(func() { env.HubB.SetOnInternalRPC(nil) })

		hopsBeforeRPC2 := env.HubB.InternalRPCHops()

		// Send new non-idempotent RPC 2 to Hub A: must hit s1 -> get Hub B's
		// 409 draining stale-route answer -> re-resolve to s2 on Hub B -> 200 OK.
		rec2 := conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodPost, "/api/v1/agents/"+env.Agent.ID+"/exec", map[string]any{
			"command": []string{"echo", "after-goaway"},
		})
		require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
		assert.Contains(t, rec2.Body.String(), "new-req-on-s2-same-relay")
		assert.True(t, sawDrainingStale.Load(), "Hub B owner relay must have emitted 409 X-Conduit-Stale-Reason: draining")
		assert.Contains(t, env.HubB.InternalStaleReasons(), string(registry.ReasonDraining))
		assert.Equal(t, hopsBeforeRPC2+2, env.HubB.InternalRPCHops(), "RPC 2 must make 2 hops to Hub B (stale s1 + replacement s2)")
		assert.NotEmpty(t, s2SessionID.Load())
		assert.Equal(t, int64(1), s2Count.Load(), "replacement session s2 on Hub B must handle RPC 2 once")
		assert.Equal(t, int64(1), s1Count.Load(), "draining session s1 must not receive RPC 2")

		// Release in-flight RPC 1: it must complete cleanly within drain_deadline.
		close(releaseInFlight)
		var rec1 *httptest.ResponseRecorder
		select {
		case rec1 = <-rpc1Done:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight RPC 1 did not complete after release")
		}
		require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
		assert.Contains(t, rec1.Body.String(), "in-flight-on-s1-done")

		// Once its last in-flight RPC completes, s1 closes cleanly.
		select {
		case <-s1.Session.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("draining session s1 did not close after in-flight RPC completed")
		}
	})

	t.Run("in_flight_past_drain_deadline_returns_outcome_unknown", func(t *testing.T) {
		env := newConduitHAEnv(t)
		env.HubA.EnableBrokerRouting(t)

		incarnation := "broker-goaway-exp-" + uuid.NewString()[:8]
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		handler := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			envReq := envelope.RPCToRequest(req)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"output":"too-late","exitCode":0}`),
			))
		})

		s1, err := conduitHA2801DialBroker(t, env.HubB, env.BrokerID, env.BrokerSecret, incarnation, "", handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubB, env.BrokerID, s1.SessionID)

		rpcDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rpcDone <- conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodPost, "/api/v1/agents/"+env.Agent.ID+"/exec", map[string]any{
				"command": []string{"sleep", "past-deadline"},
			})
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("in-flight RPC never reached broker")
		}

		// Short drain_deadline (150ms): the blocked RPC exceeds it, so s1
		// closes at the deadline and Hub A answers 502 outcome_unknown.
		require.NoError(t, s1.Session.GoAway(conduit.GoAwayOptions{DrainDeadline: 150 * time.Millisecond}))

		var rec *httptest.ResponseRecorder
		select {
		case rec = <-rpcDone:
		case <-time.After(5 * time.Second):
			t.Fatal("RPC did not fail after drain_deadline expired")
		}
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeOutcomeUnknown+`"`)
	})
}

// TestConduitHA_2801_C17_BrokerIncarnation validates ptone/scion#2801
// checkbox:
//
//	[ ] C17 (T6) broker sub-cases: a broker restart that connects to a
//	    different relay makes the old process's sessions non-current and
//	    nothing routes to them; a Hello without an incarnation is refused; an
//	    incarnation flap is logged (broker_incarnation_flap); plus the
//	    shared-broker sub-case carried over from Phase 1
func TestConduitHA_2801_C17_BrokerIncarnation(t *testing.T) {
	t.Run("restart_onto_different_relay_makes_old_sessions_non_current", func(t *testing.T) {
		env := newConduitHAEnvWith(t, conduitHAOptions{
			AuthzRecheckInterval: 100 * time.Millisecond,
		})
		ctx := context.Background()
		env.HubA.EnableBrokerRouting(t)
		env.HubB.EnableBrokerRouting(t)

		var proc1Calls, proc2Calls atomic.Int64
		proc1Handler := conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			proc1Calls.Add(1)
			envReq := envelope.RPCToRequest(req)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"logs":"from-proc-1"}`),
			))
		})
		proc2Handler := conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			proc2Calls.Add(1)
			envReq := envelope.RPCToRequest(req)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(
				envReq.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"},
				[]byte(`{"logs":"from-proc-2"}`),
			))
		})

		// Old broker process (proc-1) connects to Hub B.
		oldSess, err := conduitHA2801DialBroker(t, env.HubB, env.BrokerID, env.BrokerSecret, "proc-1", "", proc1Handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubB, env.BrokerID, oldSess.SessionID)

		// Confirm routing reaches proc-1 before the restart.
		recBefore := conduitHA2801DoHTTP(t, env.HubA, env.Owner, http.MethodGet, "/api/v1/agents/"+env.Agent.ID+"/logs?tail=5", nil)
		require.Equal(t, http.StatusOK, recBefore.Code, recBefore.Body.String())
		assert.Contains(t, recBefore.Body.String(), "from-proc-1")
		require.Equal(t, int64(1), proc1Calls.Load())

		// Restarted broker process (proc-2) connects to a DIFFERENT relay (Hub A).
		newSess, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, "proc-2", "", proc2Handler)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, newSess.SessionID)

		// Immediately, proc-1's session is non-current/superseded in Postgres.
		superseded, err := env.HubB.Server.conduit.Load().registry.BrokerSessionSuperseded(ctx, env.BrokerID, oldSess.SessionID, time.Now())
		require.NoError(t, err)
		assert.True(t, superseded, "proc-1 session on Hub B must be marked superseded once proc-2 registers on Hub A")

		// Nothing routes to proc-1 from either Hub A or Hub B.
		for _, node := range []*conduitHANode{env.HubA, env.HubB} {
			rec := conduitHA2801DoHTTP(t, node, env.Owner, http.MethodGet, "/api/v1/agents/"+env.Agent.ID+"/logs?tail=5", nil)
			require.Equal(t, http.StatusOK, rec.Code, "%s: %s", node.InstanceID, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "from-proc-2", "node %s must route only to proc-2", node.InstanceID)
		}
		assert.Equal(t, int64(1), proc1Calls.Load(), "nothing may route to old proc-1 session after restart")
		assert.Equal(t, int64(2), proc2Calls.Load(), "both post-restart RPCs must route to proc-2")

		// Hub B's session recheck closes the superseded proc-1 session with
		// 4409 superseded_incarnation within AuthzRecheckInterval.
		select {
		case <-oldSess.Session.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("Hub B recheck did not close superseded proc-1 session")
		}
		var ce *conduit.CloseError
		require.ErrorAs(t, oldSess.Session.Err(), &ce, "old session err = %v", oldSess.Session.Err())
		assert.Equal(t, relay.CloseSupersededIncarnation, ce.Code)

		// proc-2 stays open.
		select {
		case <-newSess.Session.Done():
			t.Fatalf("proc-2 session closed unexpectedly: %v", newSess.Session.Err())
		default:
		}
	})

	t.Run("hello_without_incarnation_refused", func(t *testing.T) {
		env := newConduitHAEnv(t)
		ctx := context.Background()

		_, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, "", "", nil)
		require.Error(t, err, "broker Hello without endpoint_incarnation must be refused")
		var ce *conduit.CloseError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, conduit.CloseProtocolError, ce.Code)
		assert.Contains(t, ce.Reason, relay.ReasonBadHello)

		// Refusal happens before registry insert: zero session rows in Postgres.
		ps, err := env.HubA.RegStore.ListPrincipalSessions(ctx, registry.PrincipalBroker, env.BrokerID)
		require.NoError(t, err)
		assert.Empty(t, ps.Sessions, "refused Hello must not insert a session row")
	})

	t.Run("incarnation_flap_logged", func(t *testing.T) {
		prevLogger := slog.Default()
		// Pass nil inner handler: forwarding to prevLogger.Handler() when it is
		// Go's slog.defaultHandler recurses through log.Default() and deadlocks.
		capture := newConduitHA2801LogCapture(nil)
		slog.SetDefault(slog.New(capture))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		env := newConduitHAEnv(t)
		readMetric := conduitHA2801BrokerIncarnationMetricReader(t, env.HubA.Server)

		// 1. Admit proc-1 on Hub A.
		s1, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, "proc-1", "", nil)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, s1.SessionID)

		// 2. Admit proc-2 on Hub A (supersedes proc-1; first change is NOT a flap).
		s2, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, "proc-2", "", nil)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, s2.SessionID)
		select {
		case <-s1.Session.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("s1 was not closed when proc-2 was admitted")
		}
		assert.Empty(t, capture.Matching("broker_incarnation_flap"), "first incarnation change must not log a flap")
		assert.Zero(t, readMetric("scion.hub.conduit.broker_incarnation_flap", env.BrokerID))

		// 3. Return to proc-1 on Hub A within the 10m flap window -> flap logged & counted.
		s3, err := conduitHA2801DialBroker(t, env.HubA, env.BrokerID, env.BrokerSecret, "proc-1", "", nil)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubA, env.BrokerID, s3.SessionID)
		select {
		case <-s2.Session.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("s2 was not closed when proc-1 returned")
		}

		flaps := capture.Matching("broker_incarnation_flap")
		require.NotEmpty(t, flaps, "returning to a recent incarnation must log broker_incarnation_flap")
		assert.Equal(t, slog.LevelWarn, flaps[0].Level)
		assert.Equal(t, env.BrokerID, flaps[0].Attrs["broker_id"])
		assert.Equal(t, "proc-1", flaps[0].Attrs["incarnation"])
		assert.Equal(t, int64(1), readMetric("scion.hub.conduit.broker_incarnation_flap", env.BrokerID))
	})

	t.Run("shared_broker_exec_scope_mismatch_refused", func(t *testing.T) {
		env := newConduitHAEnv(t)
		ctx := context.Background()

		// Create a flat broker with a concrete runtime target (exec_scope) in Postgres.
		scopedBrokerID := tid("conduit-ha-scoped-broker")
		require.NoError(t, env.HubA.Store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:            scopedBrokerID,
			Name:          "conduit-ha-scoped-broker",
			Slug:          "conduit-ha-scoped-broker",
			Status:        store.BrokerStatusOnline,
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "cluster-alpha/ns-1", Type: "kubernetes"},
			Created:       time.Now(),
			Updated:       time.Now(),
		}))
		keyB64, err := env.HubA.Server.brokerAuthService.GenerateAndStoreSecret(ctx, scopedBrokerID)
		require.NoError(t, err)
		scopedSecret, err := base64.StdEncoding.DecodeString(keyB64)
		require.NoError(t, err)

		// A Hello presenting a different exec_scope (e.g. a second broker
		// sharing the credential for another scope) is refused with 4403.
		_, err = conduitHA2801DialBroker(t, env.HubB, scopedBrokerID, scopedSecret, "proc-wrong-scope", "cluster-beta/ns-2", nil)
		require.Error(t, err, "mismatched exec_scope must be refused")
		var ce *conduit.CloseError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, conduit.CloseForbidden, ce.Code)

		// Matching exec_scope is admitted and recorded in Postgres.
		okSess, err := conduitHA2801DialBroker(t, env.HubB, scopedBrokerID, scopedSecret, "proc-right-scope", "cluster-alpha/ns-1", nil)
		require.NoError(t, err)
		conduitHA2801WaitLocal(t, env.HubB, scopedBrokerID, okSess.SessionID)

		ps, err := env.HubA.RegStore.ListPrincipalSessions(ctx, registry.PrincipalBroker, scopedBrokerID)
		require.NoError(t, err)
		require.Len(t, ps.Sessions, 1)
		assert.Equal(t, "cluster-alpha/ns-1", ps.Sessions[0].Session.ExecScope)
	})
}
