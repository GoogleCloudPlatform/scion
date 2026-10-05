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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Tests for the per-run identity the hub mints, persists and sends
// (ptone/scion#2550 P1).

// runIDFixture is a store with a broker, a project and an agent row, plus a
// dispatcher over a mock broker client.
type runIDFixture struct {
	store      store.Store
	client     *mockRuntimeBrokerClient
	dispatcher *HTTPAgentDispatcher
	agent      *store.Agent
}

func newRunIDFixture(t *testing.T, name string) *runIDFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	if err := s.CreateProject(ctx, &store.Project{ID: tid("project-" + name), Name: name, Slug: name}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:       tid("broker-" + name),
		Name:     "broker-" + name,
		Slug:     "broker-" + name,
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Name:            name,
		Slug:            name,
		ProjectID:       tid("project-" + name),
		RuntimeBrokerID: tid("broker-" + name),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	client := &mockRuntimeBrokerClient{}
	return &runIDFixture{
		store:      s,
		client:     client,
		dispatcher: NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()),
		agent:      agent,
	}
}

func (f *runIDFixture) storedRunID(t *testing.T) string {
	t.Helper()
	got, err := f.store.GetAgent(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	return got.RunID
}

func requireUUID(t *testing.T, what, s string) {
	t.Helper()
	if _, err := uuid.Parse(s); err != nil {
		t.Fatalf("%s = %q, want a UUID: %v", what, s, err)
	}
}

// Create, start and restart each mint a fresh UUID run ID, persist it on
// the row and send it to the broker. The mock broker reports no run ID (an
// older broker), so the minted value is kept.
func TestRunID_CreateStartRestartMintPersistAndSend(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-lifecycle")
	seen := map[string]string{}
	check := func(op, sent string) {
		t.Helper()
		requireUUID(t, op+" run ID", sent)
		if got := f.storedRunID(t); got != sent {
			t.Fatalf("%s: stored run_id = %q, want the sent %q", op, got, sent)
		}
		if f.agent.RunID != sent {
			t.Fatalf("%s: in-memory RunID = %q, want %q", op, f.agent.RunID, sent)
		}
		if prev, dup := seen[sent]; dup {
			t.Fatalf("%s reused the run ID of %s: %q", op, prev, sent)
		}
		if sent == f.agent.LaunchID && sent != "" {
			t.Fatalf("%s: run ID must not reuse the launch ID", op)
		}
		seen[sent] = op
	}

	if _, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentCreate: %v", err)
	}
	check("create", f.client.lastCreateReq.RunID)

	if err := f.dispatcher.DispatchAgentStart(ctx, f.agent, "", false); err != nil {
		t.Fatalf("DispatchAgentStart: %v", err)
	}
	check("start", f.client.lastStartExtras.RunID)

	if err := f.dispatcher.DispatchAgentRestart(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentRestart: %v", err)
	}
	check("restart", f.client.lastRestartExtras.RunID)

	if err := f.dispatcher.DispatchAgentRestart(ctx, f.agent); err != nil {
		t.Fatalf("second DispatchAgentRestart: %v", err)
	}
	check("second restart", f.client.lastRestartExtras.RunID)
}

// Create-with-gather's first pass and finalize_env each carry a fresh run
// ID, since either may be the pass that creates the agent.
func TestRunID_GatherAndFinalizeMint(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-gather")
	if _, err := f.dispatcher.DispatchAgentCreateWithGather(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentCreateWithGather: %v", err)
	}
	first := f.client.lastCreateReq.RunID
	requireUUID(t, "gather run ID", first)
	if got := f.storedRunID(t); got != first {
		t.Fatalf("stored run_id = %q, want %q", got, first)
	}

	if _, err := f.dispatcher.DispatchFinalizeEnv(ctx, f.agent, map[string]string{"K": "v"}); err != nil {
		t.Fatalf("DispatchFinalizeEnv: %v", err)
	}
	second := f.client.lastCreateReq.RunID
	requireUUID(t, "finalize run ID", second)
	if second == first {
		t.Fatal("finalize_env reused the gather pass's run ID")
	}
	if got := f.storedRunID(t); got != second {
		t.Fatalf("stored run_id = %q, want %q", got, second)
	}
}

// Provision creates no runtime entry, so it neither mints nor sends a run
// ID and leaves the row's run ID alone.
func TestRunID_ProvisionDoesNotMint(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-provision")
	if _, err := f.store.SetAgentRunID(ctx, f.agent.ID, "existing-run"); err != nil {
		t.Fatal(err)
	}
	f.agent.RunID = "existing-run"
	if err := f.dispatcher.DispatchAgentProvision(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentProvision: %v", err)
	}
	if got := f.client.lastCreateReq.RunID; got != "" {
		t.Errorf("provision sent run ID %q, want none", got)
	}
	if got := f.storedRunID(t); got != "existing-run" {
		t.Errorf("stored run_id = %q, want it unchanged", got)
	}
}

// A start that finds the agent already running keeps the existing entry,
// and the broker reports that entry's run ID: the hub records it in place
// of the one it minted, so a later delete targets the run that exists.
func TestRunID_StartOnAlreadyRunningAdoptsExistingLabel(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-adopt")
	if _, err := f.store.SetAgentRunID(ctx, f.agent.ID, "live-run"); err != nil {
		t.Fatal(err)
	}
	f.agent.RunID = "live-run"
	f.client.startReturnResp = &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: f.agent.Slug, Name: f.agent.Slug, Phase: "running", RunID: "live-run",
	}}

	if err := f.dispatcher.DispatchAgentStart(ctx, f.agent, "", false); err != nil {
		t.Fatalf("DispatchAgentStart: %v", err)
	}
	minted := f.client.lastStartExtras.RunID
	requireUUID(t, "minted run ID", minted)
	if minted == "live-run" {
		t.Fatal("start must mint a fresh run ID, not resend the stored one")
	}
	if got := f.storedRunID(t); got != "live-run" {
		t.Errorf("stored run_id = %q, want the adopted live-run", got)
	}
	if f.agent.RunID != "live-run" {
		t.Errorf("in-memory RunID = %q, want live-run", f.agent.RunID)
	}
}

// staleResponseClient records a newer run ID on the row while the start is
// in flight (as a concurrent dispatch would), then answers with a run ID
// that differs from the one this dispatch minted.
type staleResponseClient struct {
	*mockRuntimeBrokerClient
	store   store.Store
	agentID string
}

func (c *staleResponseClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.lastStartExtras = extras
	if _, err := c.store.SetAgentRunID(ctx, c.agentID, "newer-run"); err != nil {
		return nil, err
	}
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: agentID, Name: agentID, Phase: "running", RunID: "stale-run",
	}}, nil
}

// A late broker response never overwrites a run ID that a newer dispatch
// recorded: the adoption is a compare-and-swap against the minted ID.
func TestRunID_StaleBrokerResponseDoesNotOverwriteNewerRun(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-stale")
	client := &staleResponseClient{mockRuntimeBrokerClient: f.client, store: f.store, agentID: f.agent.ID}
	d := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())

	if err := d.DispatchAgentStart(ctx, f.agent, "", false); err != nil {
		t.Fatalf("DispatchAgentStart: %v", err)
	}
	if got := f.storedRunID(t); got != "newer-run" {
		t.Errorf("stored run_id = %q, want newer-run (the stale response must not win)", got)
	}
}

// brokerEnvelope is a broker JSON error response, as runtimebroker's
// writeError produces.
func brokerEnvelope(t *testing.T, status int, code string, details map[string]interface{}) *brokerStatusError {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": code, "details": details},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &brokerStatusError{StatusCode: status, Body: string(body)}
}

// startAttempted is the marker a broker sets on a failure from inside
// Manager.Start.
func startAttempted(runID string) map[string]interface{} {
	return map[string]interface{}{api.BrokerErrorDetailStartAttempted: true, api.BrokerErrorDetailRunID: runID}
}

// startAttemptedAt is the marker plus the run the broker's runtime holds
// after the failure (api.BrokerErrorDetailCurrentRunID).
func startAttemptedAt(runID, current string) map[string]interface{} {
	d := startAttempted(runID)
	d[api.BrokerErrorDetailCurrentRunID] = current
	return d
}

// After a failed start or restart, the hub fixes the row's run ID:
//   - When the broker reports the run its runtime holds now
//     (currentRunId, DN1), the hub records it: Manager.Start can fail
//     before removing the previous entry (its run is reported, and the
//     hub adopts it) or after creating the new one (the minted run); ""
//     (an unlabelled entry, or entries of several runs) makes the next
//     delete resolve by name.
//   - Otherwise, a rejection before Manager.Start (no startAttempted
//     marker) restores the previous run ID, since the previous entry is
//     still the live one; a marked failure without currentRunId (nothing
//     is left on any runtime, or the broker's re-list failed) keeps the
//     minted ID, so a delayed delete spares a same-name agent created
//     later; an ambiguous transport
//     failure keeps it too; an older broker sends no marker, so its
//     rejections revert as before.
//   - A hand-off to another node (ErrLifecycleDeferred) restores the
//     previous run ID: the owning node mints its own (N2, round 2).
//
// The revert restores the value the row held in the database, not the
// caller's possibly stale in-memory one.
func TestRunID_FailedStartRevertsOnlyWhenNotActedOn(t *testing.T) {
	ctx := context.Background()
	const minted, previous = "<minted>", "previous-run"
	for _, tc := range []struct {
		name    string
		restart bool
		err     func(t *testing.T) error
		want    string
	}{
		{"start gate rejection (409, no marker)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusConflict, "conflict", nil)
		}, previous},
		{"start validation rejection (400, no marker)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusBadRequest, "validation_error", nil)
		}, previous},
		{"start Manager.Start failure leaving no entry, or re-list failed (500, marker only)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttempted("r"))
		}, minted},
		{"start name in use, re-list failed (409, marker only)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusConflict, "conflict", startAttempted("r"))
		}, minted},
		{"start failed before removing the previous entry (current = previous)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt("r", previous))
		}, previous},
		{"start failed leaving an unlabelled entry or several runs (current = \"\")", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt("r", ""))
		}, ""},
		{"start name in use by another run (409, current = other)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusConflict, "conflict", startAttemptedAt("r", "other-run"))
		}, "other-run"},
		{"start runtime_error from an older broker (no marker)", false, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", nil)
		}, previous},
		{"start ambiguous", false, func(*testing.T) error { return errors.New("read: connection reset") }, minted},
		{"start handed off to another node", false, func(*testing.T) error { return ErrLifecycleDeferred }, previous},
		{"restart rejection (400, no marker)", true, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusBadRequest, "validation_error", nil)
		}, previous},
		{"restart Manager.Start failure leaving no entry, or re-list failed (500, marker only)", true, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttempted("r"))
		}, minted},
		{"restart Manager.Start not found, re-list failed (404, marker only)", true, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusNotFound, "agent_not_found", startAttempted("r"))
		}, minted},
		{"restart failed before removing the previous entry (current = previous)", true, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt("r", previous))
		}, previous},
		{"restart failed leaving an unlabelled entry or several runs (current = \"\")", true, func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusNotFound, "agent_not_found", startAttemptedAt("r", ""))
		}, ""},
		{"restart ambiguous", true, func(*testing.T) error { return errors.New("read: connection reset") }, minted},
		{"restart handed off to another node", true, func(*testing.T) error { return ErrLifecycleDeferred }, previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunIDFixture(t, "runid-revert")
			if _, err := f.store.SetAgentRunID(ctx, f.agent.ID, previous); err != nil {
				t.Fatal(err)
			}
			// The caller's struct is stale: the revert must use the
			// database value.
			f.agent.RunID = "stale-struct-run"
			f.client.returnErr = tc.err(t)

			var err error
			var mintedID string
			if tc.restart {
				err = f.dispatcher.DispatchAgentRestart(ctx, f.agent)
				mintedID = f.client.lastRestartExtras.RunID
			} else {
				err = f.dispatcher.DispatchAgentStart(ctx, f.agent, "", false)
				mintedID = f.client.lastStartExtras.RunID
			}
			if err == nil {
				t.Fatal("expected the dispatch to fail")
			}
			requireUUID(t, "minted run ID", mintedID)
			want := tc.want
			if want == minted {
				want = mintedID
			}
			if got := f.storedRunID(t); got != want {
				t.Errorf("stored run_id = %q, want %q", got, want)
			}
			if f.agent.RunID != want {
				t.Errorf("in-memory RunID = %q, want %q", f.agent.RunID, want)
			}
		})
	}
}

// concurrentRunClient records a newer run ID on the row while a start or
// restart is in flight (as a concurrent dispatch would), then fails it
// with a pre-Start rejection, which would normally revert.
type concurrentRunClient struct {
	*mockRuntimeBrokerClient
	store   store.Store
	agentID string
	err     error
}

func (c *concurrentRunClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.lastStartExtras = extras
	if _, err := c.store.SetAgentRunID(ctx, c.agentID, "newer-run"); err != nil {
		return nil, err
	}
	return nil, c.err
}

func (c *concurrentRunClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	c.lastRestartExtras = extras
	if _, err := c.store.SetAgentRunID(ctx, c.agentID, "newer-run"); err != nil {
		return nil, err
	}
	return nil, c.err
}

// The revert is a compare-and-swap against the minted ID: when a newer
// dispatch recorded its run mid-flight, a rejected start or restart leaves
// that newer run in place instead of restoring the previous one.
func TestRunID_RevertDoesNotOverwriteNewerRun(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		restart bool
		current bool // the failure reports currentRunId (DN1 adopt path)
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		restart := c.restart
		name := "start"
		if restart {
			name = "restart"
		}
		failure := func(t *testing.T) error { return brokerEnvelope(t, http.StatusConflict, "conflict", nil) }
		if c.current {
			name += " adopting currentRunId"
			failure = func(t *testing.T) error {
				return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt("r", "previous-run"))
			}
		}
		t.Run(name, func(t *testing.T) {
			f := newRunIDFixture(t, "runid-revert-cas")
			if _, err := f.store.SetAgentRunID(ctx, f.agent.ID, "previous-run"); err != nil {
				t.Fatal(err)
			}
			client := &concurrentRunClient{
				mockRuntimeBrokerClient: f.client, store: f.store, agentID: f.agent.ID,
				err: failure(t),
			}
			d := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
			var err error
			if restart {
				err = d.DispatchAgentRestart(ctx, f.agent)
			} else {
				err = d.DispatchAgentStart(ctx, f.agent, "", false)
			}
			if err == nil {
				t.Fatal("expected the dispatch to fail")
			}
			if got := f.storedRunID(t); got != "newer-run" {
				t.Errorf("stored run_id = %q, want newer-run (the revert must not overwrite it)", got)
			}
		})
	}
}

// A restart whose stop failed can find the entry still running and keep
// it; the broker reports that entry's run ID in the restart response, and
// the hub adopts it (compare-and-swap against the minted ID) so a later
// delete targets the entry that exists. A response without a run ID keeps
// the minted value.
func TestRunID_RestartAdoptsBrokerRunID(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-restart-adopt")
	if _, err := f.store.SetAgentRunID(ctx, f.agent.ID, "live-run"); err != nil {
		t.Fatal(err)
	}
	f.client.restartReturnResp = &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: f.agent.Slug, Name: f.agent.Slug, Phase: "running", RunID: "live-run",
	}}
	if err := f.dispatcher.DispatchAgentRestart(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentRestart: %v", err)
	}
	minted := f.client.lastRestartExtras.RunID
	requireUUID(t, "minted run ID", minted)
	if got := f.storedRunID(t); got != "live-run" {
		t.Errorf("stored run_id = %q, want the adopted live-run", got)
	}
	if f.agent.RunID != "live-run" {
		t.Errorf("in-memory RunID = %q, want live-run", f.agent.RunID)
	}

	// An older broker's restart response carries no run ID.
	f.client.restartReturnResp = &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: f.agent.Slug, Phase: "running"}}
	if err := f.dispatcher.DispatchAgentRestart(ctx, f.agent); err != nil {
		t.Fatalf("second DispatchAgentRestart: %v", err)
	}
	if got, want := f.storedRunID(t), f.client.lastRestartExtras.RunID; got != want {
		t.Errorf("stored run_id = %q, want the minted %q", got, want)
	}
}

// failingRunIDStore fails the run ID write as a database outage would.
type failingRunIDStore struct{ store.Store }

func (failingRunIDStore) SetAgentRunID(context.Context, string, string) (string, error) {
	return "", errors.New("database is unavailable")
}

// A run ID the row cannot record must not be sent: the dispatch fails
// closed before the broker call.
func TestRunID_PersistFailureFailsDispatchClosed(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-persist-fail")
	d := NewHTTPAgentDispatcherWithClient(failingRunIDStore{f.store}, f.client, false, slog.Default())

	if err := d.DispatchAgentStart(ctx, f.agent, "", false); err == nil {
		t.Fatal("expected DispatchAgentStart to fail")
	}
	if f.client.startCalled {
		t.Error("StartAgent must not be called when the run ID was not recorded")
	}
	if _, err := d.DispatchAgentCreate(ctx, f.agent); err == nil {
		t.Fatal("expected DispatchAgentCreate to fail")
	}
	if f.client.createCalled {
		t.Error("CreateAgent must not be called when the run ID was not recorded")
	}
}

// DispatchAgentDelete sends the row's run ID, and none for a row without one.
func TestRunID_DispatchAgentDeleteSendsRunID(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-delete")

	f.agent.RunID = "run-a"
	if err := f.dispatcher.DispatchAgentDelete(ctx, f.agent, true, false, false, time.Time{}); err != nil {
		t.Fatalf("DispatchAgentDelete: %v", err)
	}
	if got := f.client.lastDeleteOpts.runID; got != "run-a" {
		t.Errorf("delete runID = %q, want run-a", got)
	}

	f.agent.RunID = ""
	if err := f.dispatcher.DispatchAgentDelete(ctx, f.agent, true, false, false, time.Time{}); err != nil {
		t.Fatalf("DispatchAgentDelete: %v", err)
	}
	if got := f.client.lastDeleteOpts.runID; got != "" {
		t.Errorf("delete runID = %q, want none for a row without a run ID", got)
	}
}

// Both transports put runId on the delete query only when set.
func TestDeleteAgentQuery_RunID(t *testing.T) {
	q, err := url.ParseQuery(deleteAgentQuery(context.Background(), "p1", DeleteAgentOptions{DeleteFiles: true, RunID: "run a&b"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Get("runId"); got != "run a&b" {
		t.Errorf("runId = %q, want %q", got, "run a&b")
	}
	if q.Get("deleteFiles") != "true" || q.Get("projectId") != "p1" {
		t.Errorf("unexpected query %v", q)
	}
	q, _ = url.ParseQuery(deleteAgentQuery(context.Background(), "p1", DeleteAgentOptions{}))
	if q.Has("runId") {
		t.Errorf("runId sent without a run ID: %v", q)
	}
}

func TestHTTPRuntimeBrokerClient_DeleteAgentSendsRunID(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()
	if err := client.DeleteAgent(context.Background(), tid("host-1"), server.URL, "a", "", DeleteAgentOptions{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if got := gotQuery.Get("runId"); got != "run-1" {
		t.Errorf("runId = %q, want run-1", got)
	}
}

func TestControlChannelBrokerClient_DeleteAgentSendsRunID(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	client := &ControlChannelBrokerClient{manager: tunnel}
	if err := client.DeleteAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", DeleteAgentOptions{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(tunnel.lastRequest.Query)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Get("runId"); got != "run-1" {
		t.Errorf("runId = %q, want run-1", got)
	}
}

// The start and restart payloads carry runId (both transports share
// applyStartExtras).
func TestApplyStartExtras_RunID(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{RunID: "run-1"})
	if payload["runId"] != "run-1" {
		t.Errorf("payload runId = %v, want run-1", payload["runId"])
	}
	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	if _, ok := payload["runId"]; ok {
		t.Error("runId sent without a run ID")
	}
}

// RestartAgent returns the broker's restart response (with the entry's
// runId) on both transports, and treats an empty body as success with no
// response, as an older broker might send.
func TestHTTPRuntimeBrokerClient_RestartAgentReturnsResponse(t *testing.T) {
	body := `{"agent":{"id":"a","runId":"run-1"},"created":false}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()
	resp, err := client.RestartAgent(context.Background(), tid("host-1"), server.URL, "a", "", nil, StartExtras{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Agent == nil || resp.Agent.RunID != "run-1" {
		t.Fatalf("resp = %+v, want agent runId run-1", resp)
	}

	body = ""
	resp, err = client.RestartAgent(context.Background(), tid("host-1"), server.URL, "a", "", nil, StartExtras{})
	if err != nil || resp != nil {
		t.Fatalf("empty body: resp = %+v, err = %v; want nil, nil", resp, err)
	}
}

func TestControlChannelBrokerClient_RestartAgentReturnsResponse(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true, status: http.StatusAccepted,
		body: []byte(`{"agent":{"id":"agent-1","runId":"run-1"},"created":false}`)}
	client := &ControlChannelBrokerClient{manager: tunnel}
	resp, err := client.RestartAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", nil, StartExtras{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Agent == nil || resp.Agent.RunID != "run-1" {
		t.Fatalf("resp = %+v, want agent runId run-1", resp)
	}

	tunnel.body = nil
	resp, err = client.RestartAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", nil, StartExtras{})
	if err != nil || resp != nil {
		t.Fatalf("empty body: resp = %+v, err = %v; want nil, nil", resp, err)
	}
}

// brokerStartAttempted reads the broker's marker from the error envelope.
func TestBrokerStartAttempted(t *testing.T) {
	if !brokerStartAttempted(brokerEnvelope(t, 500, "runtime_error", startAttempted("r"))) {
		t.Error("marked envelope not recognized")
	}
	for _, err := range []error{
		brokerEnvelope(t, 500, "runtime_error", nil),
		brokerEnvelope(t, 500, "runtime_error", map[string]interface{}{api.BrokerErrorDetailStartAttempted: "yes"}),
		&brokerStatusError{StatusCode: 500, Body: "not json"},
		errors.New("plain"),
	} {
		if brokerStartAttempted(err) {
			t.Errorf("%v: reported start attempted", err)
		}
	}
}

func TestBrokerCurrentRunID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{"run reported", brokerEnvelope(t, 500, "runtime_error", startAttemptedAt("r", "run-old")), "run-old", true},
		{"empty run reported", brokerEnvelope(t, 500, "runtime_error", startAttemptedAt("r", "")), "", true},
		{"marker only", brokerEnvelope(t, 500, "runtime_error", startAttempted("r")), "", false},
		{"wrong type", brokerEnvelope(t, 500, "runtime_error", map[string]interface{}{api.BrokerErrorDetailCurrentRunID: 7}), "", false},
		{"not json", &brokerStatusError{StatusCode: 500, Body: "not json"}, "", false},
		{"plain", errors.New("plain"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := brokerCurrentRunID(tc.err)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// reportingCreateClient answers a create with a fixed run ID for the entry,
// as a broker that found an existing entry would.
type reportingCreateClient struct {
	*mockRuntimeBrokerClient
	runID string
}

func (c *reportingCreateClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.lastCreateReq = req
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Phase: "running", RunID: c.runID}, Created: true}, nil
}

// Create adopts the run ID the broker reports for the entry, as start does.
func TestRunID_CreateAdoptsBrokerRunID(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-create-adopt")
	client := &reportingCreateClient{mockRuntimeBrokerClient: f.client, runID: "broker-run"}
	d := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
	if _, err := d.DispatchAgentCreate(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentCreate: %v", err)
	}
	requireUUID(t, "minted run ID", f.client.lastCreateReq.RunID)
	if got := f.storedRunID(t); got != "broker-run" {
		t.Errorf("stored run_id = %q, want the adopted broker-run", got)
	}
}

// beginRun's refusal while a delete holds the row reaches callers as
// store.ErrDeleteInProgress, before any broker call, and the HTTP and DM
// entries map it to 409 delete_in_progress (N3, round 3).
func TestDeleteClaimedDuringDispatch(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-claimed")
	lease := time.Now().Add(time.Minute)
	deleting := store.DeletionStateDeleting
	if _, err := f.store.UpdateAgentDeletion(ctx, f.agent.ID, store.DeletionPredicate{}, store.DeletionFields{State: &deleting, LeaseAt: &lease, BumpClaim: true}); err != nil {
		t.Fatal(err)
	}
	err := f.dispatcher.DispatchAgentStart(ctx, f.agent, "", false)
	if !errors.Is(err, store.ErrDeleteInProgress) {
		t.Fatalf("DispatchAgentStart err = %v, want ErrDeleteInProgress", err)
	}
	if err := f.dispatcher.DispatchAgentRestart(ctx, f.agent); !errors.Is(err, store.ErrDeleteInProgress) {
		t.Errorf("DispatchAgentRestart err = %v, want ErrDeleteInProgress", err)
	}
	if f.client.startCalled || f.client.restartCalled {
		t.Error("a dispatch reached the broker")
	}
	ref := deleteClaimedDuringDispatch(err, f.agent.ID)
	if ref == nil || ref.HTTPStatus != http.StatusConflict || ref.Code != ErrCodeDeleteInProgress {
		t.Fatalf("refusal = %+v, want 409 %s", ref, ErrCodeDeleteInProgress)
	}
	if dm := ref.dmError(); dm.HTTPStatus != http.StatusConflict || dm.Code != ErrCodeDeleteInProgress {
		t.Errorf("DM error = %+v", dm)
	}
	if deleteClaimedDuringDispatch(errors.New("broker down"), f.agent.ID) != nil {
		t.Error("an unrelated error maps to delete_in_progress")
	}
}

// A delete that claims an agent whose create launch is still active (the
// claim writes stopping, so the row also reads as an incomplete create) is
// reported by the dispatcher's start guard as delete_in_progress, matching
// startGate's order, with no run minted and no broker call.
func TestDeleteClaimedDuringDispatch_PrecedesIncompleteCreate(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-claimed-launch")
	created, err := f.store.GetAgent(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	created.Phase = "created"
	if err := f.store.UpdateAgent(ctx, created); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if _, err := f.store.BeginLaunch(ctx, f.agent.ID, store.LaunchKindCreate, time.Minute); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	row, err := f.store.GetAgent(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Phase = "stopping"
	if err := f.store.UpdateAgent(ctx, row); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	lease := time.Now().Add(time.Minute)
	deleting := store.DeletionStateDeleting
	if _, err := f.store.UpdateAgentDeletion(ctx, f.agent.ID, store.DeletionPredicate{}, store.DeletionFields{State: &deleting, LeaseAt: &lease, BumpClaim: true}); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.store.GetAgent(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.IsIncompleteCreate() || !fresh.DeletionHoldsRow(time.Now()) {
		t.Fatalf("precondition: incomplete=%v holds=%v", fresh.IsIncompleteCreate(), fresh.DeletionHoldsRow(time.Now()))
	}
	before := fresh.RunID

	for _, op := range []string{"start", "restart"} {
		var err error
		if op == "start" {
			err = f.dispatcher.DispatchAgentStart(ctx, fresh, "", false)
		} else {
			err = f.dispatcher.DispatchAgentRestart(ctx, fresh)
		}
		if !errors.Is(err, store.ErrDeleteInProgress) {
			t.Errorf("%s err = %v, want ErrDeleteInProgress", op, err)
		}
		if ref := deleteClaimedDuringDispatch(err, f.agent.ID); ref == nil || ref.Code != ErrCodeDeleteInProgress {
			t.Errorf("%s refusal = %+v, want %s", op, ref, ErrCodeDeleteInProgress)
		}
	}
	if f.client.startCalled || f.client.restartCalled {
		t.Error("a dispatch reached the broker")
	}
	if got := f.storedRunID(t); got != before {
		t.Errorf("run ID changed from %q to %q", before, got)
	}
}
