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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// These tests pin the hub half of ptone/scion#2748: every existing-agent
// broker request carries the agent's recorded runtime type, inside the
// signed request, on both transports; and a broker's retryable 503 for an
// unavailable runtime reaches the caller as a 503 with Retry-After.

// rrRecordingBroker is an httptest broker behind the real broker HMAC
// verifier. It records, per route, the recorded runtime parameter of every
// request that passed signature verification.
type rrRecordingBroker struct {
	mu   sync.Mutex
	seen map[string][]string // route suffix -> runtime param values ("<absent>" if missing)
}

func (b *rrRecordingBroker) record(route string, q url.Values) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := "<absent>"
	if q.Has(api.RecordedRuntimeQueryParam) {
		v = q.Get(api.RecordedRuntimeQueryParam)
	}
	b.seen[route] = append(b.seen[route], v)
}

func (b *rrRecordingBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Path[strings.LastIndex(r.URL.Path, "/"):]
	if r.Method == http.MethodDelete {
		route = "DELETE"
	}
	b.record(route, r.URL.Query())
	w.Header().Set("Content-Type", "application/json")
	switch route {
	case "/has-prompt":
		_, _ = io.WriteString(w, `{"hasPrompt":false}`)
	case "/exec":
		_, _ = io.WriteString(w, `{"output":"","exitCode":0}`)
	case "/logs":
		_, _ = io.WriteString(w, "log")
	case "/keys":
		var req agentkeys.BrokerRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{OperationID: req.OperationID, Outcome: agentkeys.OutcomeDispatched})
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func TestRecordedRuntime_DispatchSendsSignedParamOverHTTP(t *testing.T) {
	cases := []struct {
		runtime string
		want    string
	}{
		{"kubernetes", "kubernetes"},
		{"docker", "docker"},
		{"", "<absent>"},
		{ManagedRuntimePrefix + "backend", "<absent>"},
	}
	for _, tc := range cases {
		t.Run("runtime="+tc.runtime, func(t *testing.T) {
			ctx := context.Background()
			secret := []byte("recorded-runtime-hub-test-secret")
			rb := &rrRecordingBroker{seen: map[string][]string{}}
			verifier := runtimebroker.NewBrokerAuthMiddleware(runtimebroker.BrokerAuthConfig{
				Enabled: true, MaxClockSkew: 5 * time.Minute, SecretKey: secret,
			})
			ts := httptest.NewServer(verifier.Middleware(rb))
			t.Cleanup(ts.Close)

			s := createTestStore(t)
			project := &store.Project{ID: tid("rr-project"), Name: "rr", Slug: "rr"}
			require.NoError(t, s.CreateProject(ctx, project))
			broker := &store.RuntimeBroker{ID: tid("rr-broker"), Name: "rr", Slug: "rr", Endpoint: ts.URL, Status: store.BrokerStatusOnline}
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
			require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
				BrokerID: broker.ID, SecretKey: secret, Algorithm: "hmac-sha256", Status: store.BrokerSecretStatusActive,
			}))

			d := NewHTTPAgentDispatcherWithClient(s, NewAuthenticatedBrokerClient(s, false), false, slog.Default())
			d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
			agent := &store.Agent{ID: tid("rr-agent"), Name: "w", Slug: "w", ProjectID: project.ID, RuntimeBrokerID: broker.ID, Runtime: tc.runtime}

			require.NoError(t, d.DispatchAgentStop(ctx, agent))
			require.NoError(t, d.DispatchAgentRestart(ctx, agent))
			require.NoError(t, d.DispatchAgentResetAuth(ctx, agent))
			require.NoError(t, d.DispatchAgentDelete(ctx, agent, true, false, false, time.Time{}))
			require.NoError(t, d.DispatchAgentMessage(ctx, agent, "hi", false, nil))
			_, err := d.DispatchAgentLogs(ctx, agent, 10)
			require.NoError(t, err)
			_, _, err = d.DispatchAgentExec(ctx, agent, []string{"true"}, 5)
			require.NoError(t, err)
			_, err = d.DispatchCheckAgentPrompt(ctx, agent)
			require.NoError(t, err)
			_, err = d.DispatchAgentKeys(ctx, agentkeys.Target{
				RuntimeBrokerID: broker.ID, AgentID: agent.ID, AgentSlug: agent.Slug, ProjectID: project.ID, Runtime: tc.runtime,
			}, "op-1", time.Now().Add(10*time.Second), "C-c")
			require.NoError(t, err)

			for _, route := range []string{"/stop", "/restart", "/reset-auth", "DELETE", "/message", "/logs", "/exec", "/has-prompt", "/keys"} {
				got := rb.seen[route]
				if len(got) != 1 || got[0] != tc.want {
					t.Errorf("%s: recorded runtime params %v, want [%s] (a request missing here failed signature verification or was not sent)", route, got, tc.want)
				}
			}
		})
	}
}

// capturingSigner records the raw query of every request it signs.
type capturingSigner struct {
	queries []string
}

func (s *capturingSigner) Sign(_ context.Context, req *http.Request, _ string) error {
	s.queries = append(s.queries, req.URL.RawQuery)
	req.Header.Set("X-Scion-Signature", "sig")
	return nil
}

func TestRecordedRuntime_ControlChannelSendsSignedParam(t *testing.T) {
	calls := map[string]func(ctx context.Context, c *ControlChannelBrokerClient){
		"stop": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_ = c.StopAgent(ctx, "b", "", "w", "p")
		},
		"restart": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_, _ = c.RestartAgent(ctx, "b", "", "w", "p", nil, StartExtras{})
		},
		"reset-auth": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_ = c.ResetAuthAgent(ctx, "b", "", "w", "p", "tok", "")
		},
		"delete": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_ = c.DeleteAgent(ctx, "b", "", "w", "p", DeleteAgentOptions{DeleteFiles: true, RunID: "run-1"})
		},
		"message": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_ = c.MessageAgent(ctx, "b", "", "w", "p", "hi", false, nil)
		},
		"logs": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_, _ = c.GetAgentLogs(ctx, "b", "", "w", "p", 10)
		},
		"logs-no-tail": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_, _ = c.GetAgentLogs(ctx, "b", "", "w", "", 0)
		},
		"exec": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_, _, _ = c.ExecAgent(ctx, "b", "", "w", "p", []string{"true"}, 5)
		},
		"has-prompt": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_, _ = c.CheckAgentPrompt(ctx, "b", "", "w", "p")
		},
		"keys": func(ctx context.Context, c *ControlChannelBrokerClient) {
			_, _ = c.ExecuteKeys(ctx, "b", "", "w", agentkeys.BrokerRequest{
				ProjectID: "p", AgentID: "a", OperationID: "op", ExecuteBefore: time.Now().Add(time.Minute), Keys: "C-c",
			})
		},
	}
	for name, call := range calls {
		for _, rt := range []string{"kubernetes", ""} {
			t.Run(name+"/runtime="+rt, func(t *testing.T) {
				tunnel := &mockControlChannelTunnel{connected: true}
				signer := &capturingSigner{}
				c := &ControlChannelBrokerClient{manager: tunnel, signer: signer}

				call(withRecordedRuntime(context.Background(), rt), c)

				require.NotNil(t, tunnel.lastRequest, "no request tunneled")
				q, err := url.ParseQuery(tunnel.lastRequest.Query)
				require.NoError(t, err)
				if rt == "" {
					if q.Has(api.RecordedRuntimeQueryParam) {
						t.Errorf("runtime param sent without a recorded runtime: %q", tunnel.lastRequest.Query)
					}
				} else if got := q.Get(api.RecordedRuntimeQueryParam); got != rt {
					t.Errorf("runtime param = %q, want %q (query %q)", got, rt, tunnel.lastRequest.Query)
				}
				// A delete carries its run (ptone/scion#2550) beside the
				// recorded runtime.
				if name == "delete" && q.Get("runId") != "run-1" {
					t.Errorf("delete runId = %q, want run-1 (query %q)", q.Get("runId"), tunnel.lastRequest.Query)
				}
				// The query the broker replays is the query that was signed.
				require.Len(t, signer.queries, 1)
				if signer.queries[0] != tunnel.lastRequest.Query {
					t.Errorf("signed query %q != tunneled query %q", signer.queries[0], tunnel.lastRequest.Query)
				}
			})
		}
	}
}

const brokerRuntimeUnavailableBody = `{"error":{"code":"runtime_unavailable","message":"runtime \"kubernetes\" is not available on this broker; retry later or check the broker's runtime configuration"}}`

func TestRecordedRuntime_ControlChannelKeepsRetryAfter(t *testing.T) {
	tunnel := &mockControlChannelTunnel{
		connected: true,
		status:    http.StatusServiceUnavailable,
		headers:   map[string]string{"Retry-After": "30"},
		body:      []byte(brokerRuntimeUnavailableBody),
	}
	c := &ControlChannelBrokerClient{manager: tunnel}

	err := c.StopAgent(context.Background(), "b", "", "w", "p")

	var se *brokerStatusError
	require.True(t, errors.As(err, &se), "error %v is not a brokerStatusError", err)
	require.Equal(t, "30", se.RetryAfter)
	require.True(t, isBrokerRuntimeUnavailable(err))
}

// logsErrClient is mockRuntimeBrokerClient with GetAgentLogs honouring
// returnErr, which the shared mock does not.
type logsErrClient struct {
	*mockRuntimeBrokerClient
}

func (c logsErrClient) GetAgentLogs(context.Context, string, string, string, string, int) (string, error) {
	return "", c.returnErr
}

func setupRuntimeUnavailableAgent(t *testing.T, suffix string, brokerErr error) (*Server, store.Store, *store.Agent, *mockRuntimeBrokerClient) {
	t.Helper()
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, suffix, "running")
	agent.Runtime = "kubernetes"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	mockClient := &mockRuntimeBrokerClient{returnErr: brokerErr}
	d := NewHTTPAgentDispatcherWithClient(s, logsErrClient{mockClient}, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	srv.SetDispatcher(d)
	return srv, s, agent, mockClient
}

func runtimeUnavailableErr() error {
	return &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: brokerRuntimeUnavailableBody, RetryAfter: "30"}
}

func requireRelayed503(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "30", rec.Header().Get("Retry-After"))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.Equal(t, brokerCodeRuntimeUnavailable, body.Error.Code)
	require.Contains(t, body.Error.Message, `"kubernetes" is not available`)
	require.Contains(t, body.Error.Message, "retry later or check the broker's runtime configuration")
}

func TestRecordedRuntime_HubRelaysBroker503(t *testing.T) {
	cases := []struct {
		name, method, action string
		body                 interface{}
	}{
		{"stop", http.MethodPost, "/stop", nil},
		{"suspend", http.MethodPost, "/suspend", nil},
		{"restart", http.MethodPost, "/restart", nil},
		{"exec", http.MethodPost, "/exec", map[string]interface{}{"command": []string{"true"}}},
		{"logs", http.MethodGet, "/logs", nil},
		{"message", http.MethodPost, "/message", map[string]interface{}{"message": "hi"}},
		{"delete", http.MethodDelete, "", nil},
		{"reset-auth", http.MethodPost, "/reset-auth", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, agent, mockClient := setupRuntimeUnavailableAgent(t, "relay-"+tc.name, runtimeUnavailableErr())

			rec := doRequest(t, srv, tc.method, "/api/v1/agents/"+agent.ID+tc.action, tc.body)

			requireRelayed503(t, rec)
			if tc.name == "restart" && mockClient.startCalled {
				t.Error("restart started the agent after the broker reported its runtime unavailable")
			}
			if tc.name == "delete" {
				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err, "hub record must remain after a non-force delete failed")
				require.NotNil(t, got)
			}
		})
	}
}

// TestRecordedRuntime_BulkResetAuthMarksRuntimeUnavailable pins that the
// admin bulk reset-auth reports a broker's runtime_unavailable answer with
// that code on the agent's failed entry, so the caller can tell it is
// retryable.
func TestRecordedRuntime_BulkResetAuthMarksRuntimeUnavailable(t *testing.T) {
	srv, _, agent, _ := setupRuntimeUnavailableAgent(t, "bulk-reset-auth", runtimeUnavailableErr())

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/agents/reset-auth-all", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Failed []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"failed"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	var found bool
	for _, f := range body.Failed {
		if f.ID == agent.ID {
			found = true
			require.Equal(t, brokerCodeRuntimeUnavailable, f.Code)
		}
	}
	require.True(t, found, "agent missing from failed entries: %s", rec.Body.String())
}

// startErrClient is mockRuntimeBrokerClient with StartAgent failing and
// every other call succeeding.
type startErrClient struct {
	*mockRuntimeBrokerClient
	startErr error
}

func (c startErrClient) StartAgent(context.Context, string, string, string, string, string, string, string, string, string, string, map[string]string, []ResolvedSecret, *api.ScionConfig, []api.SharedDir, bool, bool, StartExtras) (*RemoteAgentResponse, error) {
	c.startCalled = true
	return nil, c.startErr
}

// TestRecordedRuntime_RestartStartLegFailureRecordsStopped pins that when a
// restart's stop leg succeeds and its start leg fails, the hub records the
// agent as stopped the way an explicit stop does (phase, container status,
// exposed ports cleared, status event published) rather than leaving its
// pre-restart state in place, and still relays the start leg's
// runtime_unavailable answer.
func TestRecordedRuntime_RestartStartLegFailureRecordsStopped(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "restart-start-fails", "running")
	agent.Runtime = "kubernetes"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, startErrClient{mockClient, runtimeUnavailableErr()}, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	srv.SetDispatcher(d)
	require.NoError(t, s.UpdateAgentExposedPorts(context.Background(), agent.ID,
		[]store.ExposedPort{{Port: 4000, Label: "web", ExposedAt: time.Now()}}))
	ep := &trackingEventPublisher{}
	srv.events = ep

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)

	requireRelayed503(t, rec)
	require.True(t, mockClient.stopCalled, "stop leg was not dispatched")
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStopped), got.Phase)
	require.Equal(t, "stopped", got.ContainerStatus)
	require.Empty(t, got.ExposedPorts, "exposed ports must be cleared as an explicit stop does")
	var published bool
	for _, a := range ep.publishedAgents() {
		if a.ID == agent.ID && a.Phase == string(state.PhaseStopped) {
			published = true
		}
	}
	require.True(t, published, "no stopped status event was published")
}

// TestRecordedRuntime_DeleteRuntimeUnavailableRollsBack pins the delete
// engine's handling of a broker's runtime_unavailable answer: the delete is
// rolled back (prior phase restored, failed with code runtime_unavailable),
// the caller and a joiner of that delete get a 503 with the broker's
// Retry-After (a joiner with no remembered value gets the default), and a
// retry once the runtime is available deletes the agent.
func TestRecordedRuntime_DeleteRuntimeUnavailableRollsBack(t *testing.T) {
	brokerErr := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: brokerRuntimeUnavailableBody, RetryAfter: "7"}
	srv, s, agent, mockClient := setupRuntimeUnavailableAgent(t, "delete-rollback", brokerErr)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "7", rec.Header().Get("Retry-After"), "the broker's Retry-After is passed through")
	require.Contains(t, rec.Body.String(), brokerCodeRuntimeUnavailable)
	require.True(t, mockClient.deleteCalled)

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err, "hub record must remain after a non-force delete failed")
	require.Equal(t, "running", got.Phase, "the prior phase is restored")
	require.Equal(t, store.DeletionStateFailed, got.DeletionState)
	require.Equal(t, store.DeletionCodeRuntimeUnavailable, got.DeletionCode)

	// A joiner of that delete answers the same 503 and Retry-After.
	joined := httptest.NewRecorder()
	require.True(t, srv.resolveJoinFromRow(joined, ctx, agent.ID, got.DeletionClaim))
	require.Equal(t, http.StatusServiceUnavailable, joined.Code, joined.Body.String())
	require.Equal(t, "7", joined.Header().Get("Retry-After"))
	require.Contains(t, joined.Body.String(), brokerCodeRuntimeUnavailable)

	// The remembered value belongs to that claim only: a later delete of the
	// same agent does not inherit it.
	require.Empty(t, deletionRetryAfterFor(agent.ID, got.DeletionClaim+1))

	// A joiner on a hub process that did not classify the failure has no
	// remembered value and sends the default.
	deletionRetryAfter.Delete(agent.ID)
	joined = httptest.NewRecorder()
	require.True(t, srv.resolveJoinFromRow(joined, ctx, agent.ID, got.DeletionClaim))
	require.Equal(t, http.StatusServiceUnavailable, joined.Code, joined.Body.String())
	require.Equal(t, defaultBrokerRuntimeRetryAfter, joined.Header().Get("Retry-After"))

	// The runtime is registered again: a retry deletes the agent, and the
	// completed delete drops the agent's remembered Retry-After.
	deletionRetryAfter.Store(agent.ID, deletionRetryAfterEntry{claim: got.DeletionClaim, value: "7"})
	mockClient.returnErr = nil
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = s.GetAgent(ctx, agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, remembered := deletionRetryAfter.Load(agent.ID)
	require.False(t, remembered, "a completed delete must drop the remembered Retry-After")
}

// TestRecordedRuntime_ForceDeleteRemovesHubRecord pins that force=true still
// removes the hub record when the broker answers 503 for an unavailable
// runtime.
func TestRecordedRuntime_ForceDeleteRemovesHubRecord(t *testing.T) {
	srv, s, agent, mockClient := setupRuntimeUnavailableAgent(t, "force-delete", runtimeUnavailableErr())

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"?force=true", nil)

	require.Less(t, rec.Code, 300, rec.Body.String())
	require.True(t, mockClient.deleteCalled, "force delete should still try the broker")
	_, err := s.GetAgent(context.Background(), agent.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestRecordedRuntime_OtherBroker503StaysGatewayError pins that only the
// broker's runtime_unavailable 503 is relayed; other broker errors keep the
// existing 502.
func TestRecordedRuntime_OtherBroker503StaysGatewayError(t *testing.T) {
	other := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: `{"error":{"code":"something_else","message":"x"}}`, RetryAfter: "30"}
	srv, _, agent, _ := setupRuntimeUnavailableAgent(t, "other-503", other)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
}

func TestWriteBrokerRuntimeUnavailable_RetryAfter(t *testing.T) {
	for in, want := range map[string]string{"30": "30", "5": "5", "": "30", "0": "30", "-1": "30", "soon": "30", " 12 ": "12"} {
		rec := httptest.NewRecorder()
		err := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: brokerRuntimeUnavailableBody, RetryAfter: in}
		require.True(t, writeBrokerRuntimeUnavailable(rec, err, "kubernetes"))
		require.Equal(t, want, rec.Header().Get("Retry-After"), "broker Retry-After %q", in)
	}
	rec := httptest.NewRecorder()
	require.False(t, writeBrokerRuntimeUnavailable(rec, errors.New("boom"), "kubernetes"))
	require.Equal(t, http.StatusOK, rec.Code, "nothing may be written for other errors")
}

// TestRecordedRuntime_ExecuteAgentKeysPassesAgentRuntime pins that the keys
// route hands the agent's recorded runtime to the dispatcher, which sends it
// to the broker (see TestRecordedRuntime_DispatchSendsSignedParamOverHTTP).
func TestRecordedRuntime_ExecuteAgentKeysPassesAgentRuntime(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	f.agentInA.Runtime = "kubernetes"
	require.NoError(t, f.store.UpdateAgent(context.Background(), f.agentInA))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, d.callCount())
	require.Equal(t, "kubernetes", d.lastReq.target.Runtime)
}
