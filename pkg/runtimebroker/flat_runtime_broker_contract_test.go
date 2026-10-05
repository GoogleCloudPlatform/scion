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

package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Frozen group F tests of the flat Runtime Broker contract, Runtime Broker
// half (.design/flat-runtime-brokers-contract.md section 15). Every test is
// skipped until P1.2 (ptone/scion#3268) wires flat dispatch. P1.2 removes
// pendingFlatDispatch and may change only the body of the arrange helper
// newFlatInstanceTestServer; assertions and act steps must pass unchanged.
const pendingFlatDispatch = "pending ptone/scion#3268: flat dispatch not wired yet"

const (
	flatInstanceKey  = "local-docker"
	flatInstanceName = "example-docker"
	flatDaemonID     = "daemon-1"
	legacyBrokerID   = "legacy-broker-id"
)

// flatInstanceOpts selects the arrangement of a flat Runtime Broker instance.
type flatInstanceOpts struct {
	// hubInProcess: the Hub runs in the same process (co-located). False
	// means remote flat hosting, which P1 refuses.
	hubInProcess bool
	// legacyCredentials writes broker-credentials.json and a legacy
	// hub-credentials/*.json entry for legacyBrokerID.
	legacyCredentials bool
	// instanceCredentials writes runtime-brokers/<key>/hub-credentials/hub.json.
	instanceCredentials bool
	// activeProfile, when set, is the settings active_profile, naming a
	// profile whose runtime is not the instance's single target.
	activeProfile string
}

type flatInstanceFixture struct {
	srv       *Server
	mgr       *mockManager
	identity  *brokeridentity.Identity
	instances []config.V1RuntimeBrokerInstanceConfig
	globalDir string
}

// newFlatInstanceTestServer is the F-arrange helper: it builds a Runtime
// Broker server hosting one flat Docker instance. Its P1.1 body builds every
// input current code allows (isolated .scion env, settings with
// server.broker.instances, the persisted instance identity, credential
// files); P1.2 completes it by constructing the flat instance.
func newFlatInstanceTestServer(t *testing.T, opts flatInstanceOpts) *flatInstanceFixture {
	t.Helper()
	mgr := &mockManager{agents: []api.AgentInfo{{
		ID: "container-1", Name: "test-agent-1", Phase: "running", ContainerStatus: "Up 1 hour",
	}}}
	srv := newTestServerWithManager(t, mgr)
	srv.config.Host = "127.0.0.1"
	srv.config.Port = 0

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	active := ""
	if opts.activeProfile != "" {
		active = "active_profile: " + opts.activeProfile + "\nprofiles:\n  " + opts.activeProfile +
			":\n    runtime: cluster\nruntimes:\n  cluster:\n    type: kubernetes\n    namespace: agents\n"
	}
	settings := "schema_version: \"1\"\n" + active + `server:
  broker:
    enabled: true
    instances:
      - key: ` + flatInstanceKey + `
        name: ` + flatInstanceName + `
        runtime_target:
          type: docker
`
	writeFlatFixtureFile(t, filepath.Join(globalDir, "settings.yaml"), settings)
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}

	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, flatInstanceKey), flatInstanceKey,
		brokeridentity.TargetTypeDocker,
		brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeDocker, Docker: &brokeridentity.DockerScope{DaemonID: flatDaemonID}},
		[]string{legacyBrokerID})
	if err != nil {
		t.Fatal(err)
	}
	if opts.legacyCredentials {
		legacy := `{"name":"hub","brokerId":"` + legacyBrokerID + `","secretKey":"c2VjcmV0","hubEndpoint":"http://hub.invalid"}`
		writeFlatFixtureFile(t, filepath.Join(globalDir, "broker-credentials.json"), legacy)
		writeFlatFixtureFile(t, filepath.Join(globalDir, "hub-credentials", "hub.json"), legacy)
	}
	if opts.instanceCredentials {
		writeFlatFixtureFile(t, filepath.Join(brokeridentity.InstanceDir(globalDir, flatInstanceKey), "hub-credentials", "hub.json"),
			`{"name":"hub","brokerId":"`+id.RuntimeBrokerID+`","secretKey":"c2VjcmV0","hubEndpoint":"http://hub.invalid"}`)
	}
	_ = config.CheckRuntimeBrokerInstanceHosting(instances, opts.hubInProcess)

	t.Fatalf("newFlatInstanceTestServer: P1.2 (ptone/scion#3268) wires the flat instance into the Runtime Broker server")
	return &flatInstanceFixture{srv: srv, mgr: mgr, identity: id, instances: instances, globalDir: globalDir}
}

func writeFlatFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// flatCreateBody builds a create request body. extra holds P1.2 fields
// (expectedRuntimeTargetId, config.profile) as raw JSON values.
func flatCreateBody(requestID, name string, extra map[string]interface{}) string {
	body := map[string]interface{}{
		"requestId":   requestID,
		"name":        name,
		"slug":        name,
		"projectId":   "flat-project-id",
		"projectSlug": "flat-project",
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func serveFlat(srv *Server, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func decodeFlatError(t *testing.T, w *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error envelope %q: %v", w.Body.String(), err)
	}
	return resp.Error
}

func expectFlatRefusal(t *testing.T, w *httptest.ResponseRecorder, status int, code string) APIError {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	e := decodeFlatError(t, w)
	if e.Code != code {
		t.Fatalf("code = %q, want %q", e.Code, code)
	}
	if started, ok := e.Details["startAttempted"].(bool); ok && started {
		t.Fatal("a flat refusal never sets startAttempted:true")
	}
	return e
}

// expectNoCreateSideEffects asserts a refusal happened before
// beginCreateAttempt: no dispatch-attempt record or file, no launch registry
// entry, no project directory or marker, no runtime call.
func expectNoCreateSideEffects(t *testing.T, f *flatInstanceFixture, requestID string) {
	t.Helper()
	if _, ok := f.srv.dispatchAttempts[requestID]; ok {
		t.Error("a dispatch attempt was recorded")
	}
	if _, err := os.Stat(f.srv.dispatchAttemptPath(requestID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dispatch attempt file exists (stat err %v)", err)
	}
	f.srv.launchRegistry.mu.Lock()
	n := len(f.srv.launchRegistry.records)
	f.srv.launchRegistry.mu.Unlock()
	if n != 0 {
		t.Errorf("launch registry has %d records", n)
	}
	if _, err := os.Stat(filepath.Join(f.globalDir, "projects", "flat-project")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("project directory created (stat err %v)", err)
	}
	f.mgr.mu.Lock()
	starts := f.mgr.startCalls
	f.mgr.mu.Unlock()
	if starts != 0 || f.mgr.preflightCalls != 0 {
		t.Errorf("runtime called: start=%d preflight=%d", starts, f.mgr.preflightCalls)
	}
}

func TestFlatInstanceCreate_ExpectedTargetMismatchBeforeAttempt(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	const reqID = "req-flat-mismatch"
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents",
		flatCreateBody(reqID, "flat-agent", map[string]interface{}{"expectedRuntimeTargetId": "another-target"}))
	e := expectFlatRefusal(t, w, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	if e.Details["runtimeBrokerId"] != f.identity.RuntimeBrokerID ||
		e.Details["expectedRuntimeTargetId"] != "another-target" ||
		e.Details["actualRuntimeTargetId"] != f.identity.RuntimeTarget.ID {
		t.Fatalf("details = %v", e.Details)
	}
	want := api.CheckExpectedRuntimeTarget(f.identity.RuntimeBrokerID, f.identity.RuntimeTarget.ID, "another-target")
	if e.Message != want.Message() {
		t.Fatalf("message = %q, want %q", e.Message, want.Message())
	}
	expectNoCreateSideEffects(t, f, reqID)
}

func TestFlatInstanceCreate_MissingExpectedTargetRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	const reqID = "req-flat-missing"
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody(reqID, "flat-agent", nil))
	e := expectFlatRefusal(t, w, http.StatusPreconditionFailed, ErrCodeRuntimeTargetRequired)
	if e.Details["runtimeBrokerId"] != f.identity.RuntimeBrokerID {
		t.Fatalf("details = %v", e.Details)
	}
	if !strings.Contains(e.Message, "requires expectedRuntimeTargetId") || !strings.Contains(e.Message, "upgrade the Hub") {
		t.Fatalf("message = %q", e.Message)
	}
	expectNoCreateSideEffects(t, f, reqID)
}

func TestFlatInstanceCreate_NonEmptyProfileRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	const reqID = "req-flat-profile"
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody(reqID, "flat-agent", map[string]interface{}{
		"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
		"config":                  map[string]interface{}{"profile": "local"},
	}))
	e := expectFlatRefusal(t, w, http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
	if e.Details["profile"] != "local" || e.Details["runtimeBrokerId"] != f.identity.RuntimeBrokerID {
		t.Fatalf("details = %v", e.Details)
	}
	expectNoCreateSideEffects(t, f, reqID)
}

func TestFlatInstanceCreate_CheckPrecedence(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	profile := map[string]interface{}{"profile": "local"}
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"decode first", `{"name": "flat-agent", "projectId":`, http.StatusBadRequest, ErrCodeInvalidRequest},
		{"required before profile", flatCreateBody("p1", "flat-agent", map[string]interface{}{"config": profile}),
			http.StatusPreconditionFailed, ErrCodeRuntimeTargetRequired},
		{"mismatch before profile", flatCreateBody("p2", "flat-agent", map[string]interface{}{"config": profile, "expectedRuntimeTargetId": "x"}),
			http.StatusConflict, ErrCodeRuntimeTargetMismatch},
		{"profile last", flatCreateBody("p3", "flat-agent", map[string]interface{}{"config": profile, "expectedRuntimeTargetId": f.identity.RuntimeTarget.ID}),
			http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", c.body)
			expectFlatRefusal(t, w, c.status, c.code)
		})
	}
}

func TestFlatInstanceCreate_EmptyProfileIgnoresSettingsActiveProfile(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	// The settings active_profile names a Kubernetes runtime; the flat
	// instance never consults it and uses its single Docker target.
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true, activeProfile: "batch"})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-flat-empty-profile", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "config": map[string]interface{}{"template": "claude"}}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.startCalls != 1 {
		t.Fatalf("startCalls = %d, want 1", f.mgr.startCalls)
	}
	if f.mgr.lastStartOpts.Profile != "" {
		t.Fatalf("profile resolved from settings: %q", f.mgr.lastStartOpts.Profile)
	}
}

func TestFlatInstanceStart_ExpectedTargetMismatchRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start", `{"expectedRuntimeTargetId":"another-target"}`)
	e := expectFlatRefusal(t, w, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	if e.Details["actualRuntimeTargetId"] != f.identity.RuntimeTarget.ID {
		t.Fatalf("details = %v", e.Details)
	}
	if f.mgr.startCalls != 0 {
		t.Fatalf("runtime started on a mismatch: %d", f.mgr.startCalls)
	}
}

func TestFlatInstanceStart_UndecodableBodyRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	for _, body := range []string{`{"task": `, `{"expectedRuntimeTargetId": 42}`} {
		w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start", body)
		expectFlatRefusal(t, w, http.StatusBadRequest, ErrCodeInvalidRequest)
	}
	if f.mgr.startCalls != 0 {
		t.Fatalf("runtime started on an undecodable body: %d", f.mgr.startCalls)
	}
	// Unknown keys stay ignored (start/restart version-skew contract).
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start",
		`{"someFutureKey": true, "expectedRuntimeTargetId": "`+f.identity.RuntimeTarget.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("unknown keys must be accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestFlatInstanceStart_WithoutExpectedTargetUsesOnlyTarget(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.startCalls != 1 || f.mgr.lastStartOpts.Profile != "" {
		t.Fatalf("startCalls=%d profile=%q", f.mgr.startCalls, f.mgr.lastStartOpts.Profile)
	}
}

func TestFlatInstanceStart_IgnoresSavedProfile(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true, activeProfile: "batch"})
	projectDir, err := config.GetResolvedProjectDir("")
	if err != nil {
		t.Fatal(err)
	}
	writeFlatFixtureFile(t, filepath.Join(config.GetAgentHomePath(projectDir, "test-agent-1"), "agent-info.json"),
		`{"name":"test-agent-1","profile":"batch"}`)
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start",
		`{"expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.lastStartOpts.Profile != "" {
		t.Fatalf("saved profile was used: %q", f.mgr.lastStartOpts.Profile)
	}
}

func TestFlatInstanceStart_MismatchKeepsRunIDFencing(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start",
		`{"runId":"run-refused","expectedRuntimeTargetId":"another-target"}`)
	expectFlatRefusal(t, w, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	if f.mgr.startCalls != 0 {
		t.Fatal("a refused start must not reach the runtime")
	}
	// An accepted start still carries the hub-minted run ID unchanged.
	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start",
		`{"runId":"run-accepted","expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.lastStartOpts.RunID != "run-accepted" {
		t.Fatalf("run ID = %q, want run-accepted", f.mgr.lastStartOpts.RunID)
	}
}

func TestFlatInstanceCreate_FromUnawareReplicaRefused(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	// After activation by a capable Hub replica, an unaware replica sends a
	// create without expectedRuntimeTargetId.
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	const reqID = "req-flat-unaware-replica"
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents",
		flatCreateBody(reqID, "flat-agent", map[string]interface{}{"config": map[string]interface{}{"template": "claude"}}))
	expectFlatRefusal(t, w, http.StatusPreconditionFailed, ErrCodeRuntimeTargetRequired)
	expectNoCreateSideEffects(t, f, reqID)
}

func TestFlatInstanceServer_RemoteModeNeverActivates(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: false, legacyCredentials: true, instanceCredentials: true})
	if err := config.CheckRuntimeBrokerInstanceHosting(f.instances, false); err == nil {
		t.Fatal("remote flat hosting must be refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- f.srv.Start(ctx) }()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), api.ErrCodeFlatRuntimeBrokerRemoteUnsupported) {
			t.Fatalf("Start = %v, want a %s refusal", err, api.ErrCodeFlatRuntimeBrokerRemoteUnsupported)
		}
	case <-ctx.Done():
		_ = f.srv.Shutdown(context.Background())
		t.Fatal("a remote flat instance must refuse to start, not serve")
	}
	f.srv.mu.Lock()
	conns := len(f.srv.hubConnections)
	f.srv.mu.Unlock()
	if conns != 0 {
		t.Fatalf("no Hub connection may be opened, found %d", conns)
	}
	if f.mgr.startCalls != 0 {
		t.Fatal("no dispatch may be accepted")
	}
}

func TestFlatInstanceServer_LoadsNoLegacyCredentials(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true, legacyCredentials: true})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- f.srv.Start(ctx) }()
	time.Sleep(500 * time.Millisecond)

	f.srv.mu.Lock()
	for name, conn := range f.srv.hubConnections {
		if conn.BrokerID == legacyBrokerID {
			t.Errorf("Hub connection %q uses the legacy identity %s", name, legacyBrokerID)
		}
		if conn.BrokerID != "" && conn.BrokerID != f.identity.RuntimeBrokerID {
			t.Errorf("Hub connection %q uses a foreign identity %s", name, conn.BrokerID)
		}
	}
	f.srv.mu.Unlock()

	cancel()
	_ = f.srv.Shutdown(context.Background())
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop")
	}
}

// TestLegacyInstanceCreate_NonEmptyExpectedTargetRejected: a current-binary
// legacy Runtime Broker never ignores a non-empty expectedRuntimeTargetId.
func TestLegacyInstanceCreate_NonEmptyExpectedTargetRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	const reqID = "req-legacy-expected-target"
	w := serveFlat(srv, http.MethodPost, "/api/v1/agents",
		flatCreateBody(reqID, "legacy-agent", map[string]interface{}{"expectedRuntimeTargetId": "some-target"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	e := decodeFlatError(t, w)
	if e.Code != ErrCodeRuntimeTargetMismatch {
		t.Fatalf("code = %q", e.Code)
	}
	if actual, ok := e.Details["actualRuntimeTargetId"]; !ok || actual != "" {
		t.Fatalf("a legacy Runtime Broker reports an empty actual target, got %v", e.Details)
	}
	if e.Details["expectedRuntimeTargetId"] != "some-target" {
		t.Fatalf("details = %v", e.Details)
	}
	if _, ok := srv.dispatchAttempts[reqID]; ok {
		t.Error("refusal must precede the dispatch attempt")
	}
	if mgr.startCalls != 0 {
		t.Error("runtime must not be called")
	}
}
