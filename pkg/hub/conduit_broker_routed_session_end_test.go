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

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Session end (design §3.5 "Ambiguous results and session end"): the
// sessions' Call results are scripted, so each end of a request is
// produced exactly. The router stand-in applies router.Do's rule for
// stale routes (exclude the session, re-resolve, at most
// router.MaxReResolves times); the real router is exercised by the
// fixture tests in conduit_broker_routed_test.go.

// scriptedSession answers every Call with resp or err.
type scriptedSession struct {
	resp  *conduitv1.RpcResponse
	err   error
	calls int
}

func (s *scriptedSession) Call(context.Context, *conduitv1.RpcRequest) (*conduitv1.RpcResponse, error) {
	s.calls++
	return s.resp, s.err
}

func (s *scriptedSession) OpenStream(context.Context, *conduitv1.StreamOpen) (conduit.Stream, error) {
	return nil, errors.New("scripted session: no streams")
}

func (s *scriptedSession) Info() conduit.SessionInfo { return conduit.SessionInfo{} }

func (s *scriptedSession) Close() error { return nil }

// scriptedRouter resolves to its sessions in order, applying router.Do's
// stale-route rule.
type scriptedRouter struct{ sessions []*scriptedSession }

func (r *scriptedRouter) Do(ctx context.Context, req router.Request, fn func(context.Context, router.Resolved) error) error {
	excluded := map[int]bool{}
	for attempt := 0; ; attempt++ {
		idx := -1
		for i := range r.sessions {
			if !excluded[i] {
				idx = i
				break
			}
		}
		if idx < 0 {
			return router.ErrNoSession
		}
		err := fn(ctx, router.Resolved{
			Session: r.sessions[idx],
			Record:  registry.SessionRecord{SessionID: fmt.Sprintf("s%d", idx), PrincipalKind: registry.PrincipalBroker, PrincipalID: req.ID},
			Want:    req.Want,
		})
		if !errors.Is(err, relay.ErrStaleRoute) {
			return err
		}
		if attempt >= router.MaxReResolves {
			return fmt.Errorf("%w (after %d re-resolutions): %w", router.ErrNoSession, attempt, err)
		}
		excluded[idx] = true
	}
}

func scriptedTunnel(sessions ...*scriptedSession) *routedBrokerTunnel {
	r := &scriptedRouter{sessions: sessions}
	return &routedBrokerTunnel{
		router:    func() brokerRPCRouter { return r },
		enabled:   func() bool { return true },
		execScope: func(context.Context, string) (string, error) { return "", nil },
	}
}

var (
	errNotWritten = fmt.Errorf("%w: %w", conduit.ErrRequestNotSent, conduit.ErrSessionClosed)
	errLostAfter  = fmt.Errorf("%w: %w", conduit.ErrSessionClosed, errors.New("transport lost"))
)

func answered() *scriptedSession {
	return &scriptedSession{resp: &conduitv1.RpcResponse{Status: http.StatusOK, Body: []byte("served")}}
}

func tunnelOnce(t *testing.T, tun *routedBrokerTunnel, op string, idempotent bool) (*wsprotocol.ResponseEnvelope, error) {
	t.Helper()
	return tun.TunnelRequest(withBrokerRPC(context.Background(), op, idempotent), "broker-1",
		wsprotocol.NewRequestEnvelope("req-1", http.MethodPost, "/api/v1/agents/a/exec", "", nil, nil))
}

// TestRoutedBrokerRPC_SessionEnd: each way a request can meet its
// session's end.
func TestRoutedBrokerRPC_SessionEnd(t *testing.T) {
	t.Run("not written is re-resolved", func(t *testing.T) {
		for _, idempotent := range []bool{true, false} {
			s1, s2 := &scriptedSession{err: errNotWritten}, answered()
			resp, err := tunnelOnce(t, scriptedTunnel(s1, s2), conduit.RPCAgentExec, idempotent)
			require.NoError(t, err, "idempotent=%v", idempotent)
			assert.Equal(t, "served", string(resp.Body))
			assert.Equal(t, 1, s1.calls)
			assert.Equal(t, 1, s2.calls)
		}
	})
	t.Run("not written anywhere takes the fallback", func(t *testing.T) {
		s1, s2 := &scriptedSession{err: errNotWritten}, &scriptedSession{err: errNotWritten}
		_, err := tunnelOnce(t, scriptedTunnel(s1, s2), conduit.RPCAgentExec, false)
		require.ErrorIs(t, err, errBrokerNotRouted)
		assert.True(t, routedBrokerFallback(err))
	})
	t.Run("written idempotent is retried on another session", func(t *testing.T) {
		s1, s2 := &scriptedSession{err: errLostAfter}, answered()
		resp, err := tunnelOnce(t, scriptedTunnel(s1, s2), conduit.RPCAgentLogs, true)
		require.NoError(t, err)
		assert.Equal(t, "served", string(resp.Body))
		assert.Equal(t, 1, s1.calls)
		assert.Equal(t, 1, s2.calls)
	})
	t.Run("written exec is outcome unknown", func(t *testing.T) {
		s1, s2 := &scriptedSession{err: errLostAfter}, answered()
		_, err := tunnelOnce(t, scriptedTunnel(s1, s2), conduit.RPCAgentExec, false)
		var ou *brokerOutcomeUnknownError
		require.ErrorAs(t, err, &ou)
		assert.Equal(t, conduit.RPCAgentExec, ou.Operation)
		assert.Equal(t, "broker-1", ou.BrokerID)
		assert.Equal(t, "req-1", ou.RequestID)
		assert.False(t, routedBrokerFallback(err), "never retried on the fallback")
		assert.Equal(t, 0, s2.calls, "never retried on another session")

		rec := httptest.NewRecorder()
		require.True(t, writeBrokerOutcomeUnknown(rec, fmt.Errorf("dispatch: %w", err)))
		assert.Equal(t, http.StatusBadGateway, rec.Code)
		assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeOutcomeUnknown+`"`)
	})
	t.Run("idempotent budget exhausted is 503 unavailable", func(t *testing.T) {
		s1, s2, s3 := &scriptedSession{err: errLostAfter}, &scriptedSession{err: errLostAfter}, &scriptedSession{err: errLostAfter}
		_, err := tunnelOnce(t, scriptedTunnel(s1, s2, s3), conduit.RPCAgentLogs, true)
		require.ErrorIs(t, err, errBrokerRoutedUnavailable)
		assert.False(t, routedBrokerFallback(err))
		assert.Equal(t, []int{1, 1, 1}, []int{s1.calls, s2.calls, s3.calls}, "one call per session, within the budget")

		rec := httptest.NewRecorder()
		require.True(t, writeBrokerRoutedUnavailable(rec, fmt.Errorf("dispatch: %w", err)))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeUnavailable+`"`)
	})
	t.Run("caller cancel ends the request without a retry", func(t *testing.T) {
		s1, s2 := &scriptedSession{err: context.Canceled}, answered()
		tun := scriptedTunnel(s1, s2)
		ctx, cancel := context.WithCancel(withBrokerRPC(context.Background(), conduit.RPCAgentLogs, true))
		cancel()
		_, err := tun.TunnelRequest(ctx, "broker-1", wsprotocol.NewRequestEnvelope("req-1", http.MethodGet, "/x", "", nil, nil))
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 0, s2.calls)
	})
}
