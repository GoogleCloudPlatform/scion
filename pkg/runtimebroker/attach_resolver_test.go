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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/require"
)

// TestHandlePTYStream_AuxRuntimeWithoutAttach_ClosesBeforeExec is the
// aux-match variant of the control-channel pre-check: the lookup is the
// REAL Server.LookupAgent (not a fake injecting Runtime), the default
// runtime supports attach, and the agent is only found on an auxiliary
// runtime whose own instance opts out. handlePTYStream must close the
// stream with 4503/session-not-ready and never invoke the runtime exec.
//
// The auxiliary runtime is registered under the path of a script that
// records every invocation: LookupAgent reports the matched map key as
// RuntimeName, which handlePTYStreamWithAgent would exec — so "never
// invoked" proves the gate fired, not just that an exec failed with the
// same close code.
func TestHandlePTYStream_AuxRuntimeWithoutAttach_ClosesBeforeExec(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		labels    map[string]string
	}{
		// projectID "" — the first-stage auxiliary match.
		{name: "first-stage aux match", projectID: "", labels: map[string]string{"scion.name": "some-agent"}},
		// project-scoped lookup of an unlabelled agent — only the
		// no-project-label fallback stage can find it.
		{name: "no-project-label fallback aux match", projectID: "proj-1", labels: map[string]string{"scion.name": "some-agent"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "exec-invoked")
			fakeRuntime := filepath.Join(dir, "fake-runtime")
			require.NoError(t, os.WriteFile(fakeRuntime, []byte("#!/bin/sh\necho \"$@\" >> '"+marker+"'\nexit 1\n"), 0o700))

			srv := New(DefaultServerConfig(), &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
			auxRT := &attachCapableTestRuntime{
				MockRuntime:    &runtime.MockRuntime{NameFunc: func() string { return "aux-fake" }},
				supportsAttach: false,
			}
			auxMgr := &mockManager{agents: []api.AgentInfo{{Name: "some-agent", ID: "cid-1", Labels: tc.labels}}}
			srv.auxiliaryRuntimesMu.Lock()
			srv.auxiliaryRuntimes[fakeRuntime] = auxiliaryRuntime{Runtime: auxRT, Manager: auxMgr}
			srv.auxiliaryRuntimesMu.Unlock()

			// Precondition: the real lookup resolves to the opted-out aux instance.
			res, err := srv.LookupAgent(context.Background(), "some-agent", tc.projectID)
			require.NoError(t, err)
			require.Equal(t, fakeRuntime, res.RuntimeName)

			brokerConn, hubConn, cleanup := newWSPair(t)
			defer cleanup()
			client := &ControlChannelClient{
				conn:        brokerConn,
				connected:   true,
				streams:     make(map[string]*StreamHandler),
				agentLookup: srv,
				log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				ctx:         context.Background(),
			}

			closedCh := make(chan ptyClassifierCloseMsg, 1)
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				var m ptyClassifierCloseMsg
				if err := hubConn.ReadJSON(&m); err == nil {
					closedCh <- m
				}
			}()
			defer func() { _ = hubConn.Close(); <-readerDone }()

			handler := &StreamHandler{streamID: "attach-aux-optout", slug: "some-agent", projectID: tc.projectID, dataCh: make(chan []byte, 1), resizeCh: make(chan [2]int, 1), closeCh: make(chan struct{})}
			client.streamMu.Lock()
			client.streams[handler.streamID] = handler
			client.streamMu.Unlock()

			done := make(chan struct{})
			go func() { defer close(done); client.handlePTYStream(handler, 80, 24) }()

			select {
			case msg := <-closedCh:
				require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, msg.Code)
				require.Equal(t, wsprotocol.CloseReasonSessionNotReady, msg.Reason)
			case <-time.After(5 * time.Second):
				t.Fatal("no stream_close received")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				close(handler.closeCh)
				t.Fatal("handlePTYStream did not return")
			}

			if b, err := os.ReadFile(marker); err == nil {
				t.Fatalf("runtime exec was invoked for an agent on an attach-opted-out auxiliary runtime (args: %q); the pre-check must close the stream before any exec", string(b))
			}
		})
	}
}

// TestBuildInfoProfiles_AuxTypeProfile_AsksCachedAuxInstance covers the
// auxiliary branch of resolveLiveRuntimeInstance: a profile whose type is
// NOT the default, but for which an auxiliary runtime instance has already
// been built and cached, must be answered by that live instance — here one
// that opts out, so Attach=&false — rather than left unknown. The default
// runtime supports attach, so a false answer can only come from the cached
// auxiliary instance.
func TestBuildInfoProfiles_AuxTypeProfile_AsksCachedAuxInstance(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
`)

	srv := &Server{
		runtime:           &runtime.MockRuntime{NameFunc: func() string { return "docker" }},
		auxiliaryRuntimes: map[string]auxiliaryRuntime{},
	}
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: &attachCapableTestRuntime{
			MockRuntime:    &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }},
			supportsAttach: false,
		},
		Manager: &mockManager{},
	}
	byName := infoProfilesByName(srv.buildInfoProfiles("docker"))

	remote, ok := byName["remote"]
	if !ok || remote.Attach == nil || *remote.Attach {
		t.Errorf("remote (aux type, cached opted-out instance) = %+v, want Attach=&false", remote)
	}
	local, ok := byName["local"]
	if !ok || local.Attach == nil || !*local.Attach {
		t.Errorf("local (default type, attach-capable default runtime) = %+v, want Attach=&true", local)
	}
}

func newOptOutTestRuntime(name string) *attachCapableTestRuntime {
	return &attachCapableTestRuntime{
		MockRuntime:    &runtime.MockRuntime{NameFunc: func() string { return name }},
		supportsAttach: false,
	}
}

// TestBuildInfoProfiles_FallbackDefaultProfile_AsksLiveInstance covers both
// synthesized-"default"-profile fallbacks of buildInfoProfiles: settings
// that fail to load, and configured profiles that are all filtered out
// (local-only runtimes on a non-local default). Either way the "default"
// profile resolves to the default runtime type, so it must carry the live
// default runtime's answer (&false for an opted-out runtime), not be left
// unknown.
func TestBuildInfoProfiles_FallbackDefaultProfile_AsksLiveInstance(t *testing.T) {
	tests := []struct {
		name     string
		settings string
	}{
		{name: "settings fail to load", settings: "profiles: [this is: not valid\n"},
		{name: "every profile filtered out", settings: `schema_version: "1"
runtimes:
  docker:
    type: docker
profiles:
  local:
    runtime: docker
  remote:
    runtime: docker
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeHomeSettings(t, tc.settings)
			srv := &Server{runtime: newOptOutTestRuntime("optout")}

			profiles := srv.buildInfoProfiles("optout")

			require.Len(t, profiles, 1)
			require.Equal(t, "default", profiles[0].Name, "precondition: this case must reach the synthesized default profile")
			require.NotNil(t, profiles[0].Attach, "default profile of an opted-out default runtime must carry an explicit Attach")
			require.False(t, *profiles[0].Attach)
		})
	}
}

// TestHandleInfo_OptedOutDefaultRuntime_ReportsBrokerWideAttachFalse pins
// that GET /api/v1/info reports the default runtime's own attach capability
// broker-wide, rather than a blanket true.
func TestHandleInfo_OptedOutDefaultRuntime_ReportsBrokerWideAttachFalse(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
`)
	srv := New(DefaultServerConfig(), &mockManager{}, newOptOutTestRuntime("optout"))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/info", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var resp BrokerInfoResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Capabilities)
	require.False(t, resp.Capabilities.Attach, "/info Capabilities.Attach must reflect the opted-out default runtime")
}

// TestSwapRuntime_PropagatesAttachCapabilityToHeartbeat covers the
// heartbeat's broker-wide Capabilities.Attach and its SwapRuntime wiring:
// before the swap the heartbeat reports attach supported; after swapping
// to a runtime that opts out, the next heartbeat must report false.
func TestSwapRuntime_PropagatesAttachCapabilityToHeartbeat(t *testing.T) {
	srv := New(DefaultServerConfig(), &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	hb := NewHeartbeatService(&mockRuntimeBrokerService{}, "test-host", time.Hour, &mockManager{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hb.SetDefaultRuntime(srv.runtime)
	srv.hubMu.Lock()
	srv.hubConnections["local"] = &HubConnection{Name: "local", Heartbeat: hb}
	srv.hubMu.Unlock()

	before := hb.buildHeartbeat(context.Background())
	require.NotNil(t, before.Capabilities)
	require.True(t, before.Capabilities.Attach, "precondition: attach-capable default runtime reports Attach=true")

	srv.SwapRuntime(newOptOutTestRuntime("optout"))

	after := hb.buildHeartbeat(context.Background())
	require.NotNil(t, after.Capabilities)
	require.False(t, after.Capabilities.Attach, "heartbeat must report the swapped-in runtime's attach opt-out")
}

// TestHubConnectionStart_HeartbeatReportsDefaultRuntimeAttachCapability
// covers the HubConnection.Start wiring: the heartbeat service it creates
// must be handed the broker's live default runtime, so an opted-out default
// runtime is reported as Capabilities.Attach=false from the first
// heartbeat, not the nil-runtime "supported" default.
func TestHubConnectionStart_HeartbeatReportsDefaultRuntimeAttachCapability(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)
	srv.runtime = newOptOutTestRuntime("docker")
	cfg := srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false
	srv.config = cfg

	srv.hubMu.RLock()
	conn, ok := srv.hubConnections["local"]
	srv.hubMu.RUnlock()
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, conn.Start(ctx, srv))

	conn.mu.RLock()
	hb := conn.Heartbeat
	conn.mu.RUnlock()
	require.NotNil(t, hb, "precondition: Start must create a heartbeat service")
	got := hb.buildHeartbeat(ctx)
	require.NotNil(t, got.Capabilities)
	require.False(t, got.Capabilities.Attach, "heartbeat created by Start must report the opted-out default runtime")
}

// TestBrokerProfile_AttachWireContract_MatchesHubclient pins the broker-side
// encoding of BrokerProfile.Attach against its hubclient mirror: &false and
// &true must encode under the same "attach" key hubclient decodes (so an
// explicit opt-out is never silently dropped in transit), and nil must be
// omitted entirely (so it decodes as unknown ⇒ supported, not false).
func TestBrokerProfile_AttachWireContract_MatchesHubclient(t *testing.T) {
	f, tr := false, true
	for _, tc := range []struct {
		name string
		in   *bool
	}{{"explicit false", &f}, {"explicit true", &tr}, {"unknown", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(BrokerProfile{Name: "p", Type: "t", Available: true, Attach: tc.in})
			require.NoError(t, err)
			var got hubclient.BrokerProfile
			require.NoError(t, json.Unmarshal(raw, &got))
			if tc.in == nil {
				require.Nil(t, got.Attach, "nil Attach must not reach the hub as an explicit value: %s", raw)
				require.NotContains(t, string(raw), `"attach"`)
				return
			}
			require.NotNil(t, got.Attach, "explicit Attach was dropped in transit: %s", raw)
			require.Equal(t, *tc.in, *got.Attach)
		})
	}
}
