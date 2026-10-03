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

	if err := f.dispatcher.DispatchAgentCreate(ctx, f.agent); err != nil {
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

	if err := f.dispatcher.DispatchFinalizeEnv(ctx, f.agent, map[string]string{"K": "v"}); err != nil {
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
	if err := f.store.SetAgentRunID(ctx, f.agent.ID, "existing-run"); err != nil {
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
	if err := f.store.SetAgentRunID(ctx, f.agent.ID, "live-run"); err != nil {
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
	if err := c.store.SetAgentRunID(ctx, c.agentID, "newer-run"); err != nil {
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

// When the broker confirms it never acted on a start or restart, the hub
// restores the previous run ID: the previous entry is still the live one.
// An ambiguous failure keeps the minted ID, since the broker may have
// created the entry.
func TestRunID_FailedStartRevertsOnlyWhenNotActedOn(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		restart    bool
		err        error
		wantRevert bool
	}{
		{"start rejected", false, &brokerStatusError{StatusCode: http.StatusConflict, Body: "conflict"}, true},
		{"start ambiguous", false, errors.New("read: connection reset"), false},
		{"restart rejected", true, &brokerStatusError{StatusCode: http.StatusBadRequest, Body: "bad"}, true},
		{"restart ambiguous", true, errors.New("read: connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunIDFixture(t, "runid-revert")
			if err := f.store.SetAgentRunID(ctx, f.agent.ID, "previous-run"); err != nil {
				t.Fatal(err)
			}
			f.agent.RunID = "previous-run"
			f.client.returnErr = tc.err

			var err error
			var minted string
			if tc.restart {
				err = f.dispatcher.DispatchAgentRestart(ctx, f.agent)
				minted = f.client.lastRestartExtras.RunID
			} else {
				err = f.dispatcher.DispatchAgentStart(ctx, f.agent, "", false)
				minted = f.client.lastStartExtras.RunID
			}
			if err == nil {
				t.Fatal("expected the dispatch to fail")
			}
			requireUUID(t, "minted run ID", minted)
			want := minted
			if tc.wantRevert {
				want = "previous-run"
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

// failingRunIDStore fails the run ID write as a database outage would.
type failingRunIDStore struct{ store.Store }

func (failingRunIDStore) SetAgentRunID(context.Context, string, string) error {
	return errors.New("database is unavailable")
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
	if err := d.DispatchAgentCreate(ctx, f.agent); err == nil {
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
