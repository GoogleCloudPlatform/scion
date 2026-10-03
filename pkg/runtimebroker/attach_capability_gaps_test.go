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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/require"
)

// TestHandlePTYStream_RuntimeWithoutAttach_ClosesBeforeExec covers the
// control-channel pre-check in handlePTYStream: when the matched runtime
// instance opts out of attach (runtime.HasAttachSupport == false), the
// stream is closed with 4501/attach_unsupported and the runtime exec is
// NEVER invoked. The close code alone cannot prove the gate fired (a failed
// exec is classified the same way), so RuntimeName points at a script that
// records every invocation; the assertion is that it was never run.
func TestHandlePTYStream_RuntimeWithoutAttach_ClosesBeforeExec(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "exec-invoked")
	fakeRuntime := filepath.Join(dir, "fake-runtime")
	require.NoError(t, os.WriteFile(fakeRuntime, []byte("#!/bin/sh\necho \"$@\" >> '"+marker+"'\nexit 1\n"), 0o700))

	lookup := &fixedAgentLookup{result: &AgentLookupResult{
		ContainerID: "cid-1",
		RuntimeName: fakeRuntime,
		ExecUser:    "scion",
		Runtime: &attachCapableTestRuntime{
			MockRuntime:    &runtime.MockRuntime{NameFunc: func() string { return "opted-out" }},
			supportsAttach: false,
		},
	}}

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{
		conn:        brokerConn,
		connected:   true,
		streams:     make(map[string]*StreamHandler),
		agentLookup: lookup,
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

	handler := &StreamHandler{streamID: "attach-optout", slug: "some-agent", dataCh: make(chan []byte, 1), resizeCh: make(chan [2]int, 1), closeCh: make(chan struct{})}
	client.streamMu.Lock()
	client.streams[handler.streamID] = handler
	client.streamMu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); client.handlePTYStream(handler, 80, 24) }()

	select {
	case msg := <-closedCh:
		require.Equal(t, wsprotocol.ClosePTYAttachUnsupported, msg.Code)
		require.Equal(t, wsprotocol.CloseReasonAttachUnsupported, msg.Reason)
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
		t.Fatalf("runtime exec was invoked for an attach-opted-out runtime (args: %q); the pre-check must close the stream before any exec", string(b))
	}
}

// newFallbackOnlyAuxServer builds a broker whose default runtime supports
// attach, and whose only match for "some-agent" is an UNLABELLED agent on
// an auxiliary runtime that opts out of attach. For a project-scoped
// lookup, the primary stage filters the unlabelled agent out
// (agentsForProject), so it can only be found by the no-project-label
// fallback stage.
func newFallbackOnlyAuxServer() (*Server, *attachCapableTestRuntime) {
	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	auxMgr := &mockManager{agents: []api.AgentInfo{{Name: "some-agent", ID: "cid-1", Labels: map[string]string{"scion.name": "some-agent"}}}}
	auxRT := &attachCapableTestRuntime{
		MockRuntime:    &runtime.MockRuntime{NameFunc: func() string { return "aux-fake" }},
		supportsAttach: false,
	}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["aux-fake"] = auxiliaryRuntime{Runtime: auxRT, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()
	return srv, auxRT
}

// TestLookupAgent_NoProjectLabelFallback_AuxMatch_SetsRuntimeToAuxInstance
// verifies AgentLookupResult.Runtime is the matched auxiliary instance on
// the no-project-label fallback stage. A missed assignment there would
// leave Runtime as the (attach-capable) default and fail open.
func TestLookupAgent_NoProjectLabelFallback_AuxMatch_SetsRuntimeToAuxInstance(t *testing.T) {
	srv, auxRT := newFallbackOnlyAuxServer()

	result, err := srv.LookupAgent(context.Background(), "some-agent", "proj-1")
	require.NoError(t, err)
	require.Equal(t, "aux-fake", result.RuntimeName)
	if result.Runtime != auxRT {
		t.Fatalf("result.Runtime = %T(%p), want the matched auxiliary instance %p", result.Runtime, result.Runtime, auxRT)
	}
	if runtime.HasAttachSupport(result.Runtime) {
		t.Fatal("HasAttachSupport(result.Runtime) = true for an agent matched on an opted-out auxiliary runtime via the fallback stage")
	}
}

// TestHandleAgentAttach_NoProjectLabelFallback_AuxRuntimeUnsupported_Rejects
// is the end-to-end form of the above through the direct-connect handler.
func TestHandleAgentAttach_NoProjectLabelFallback_AuxRuntimeUnsupported_Rejects(t *testing.T) {
	srv, _ := newFallbackOnlyAuxServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/some-agent/attach?projectId=proj-1", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	srv.handleAgentAttach(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 for a fallback-stage match on an opted-out auxiliary runtime, got %d: %s", rec.Code, rec.Body.String())
	}
	if resp := decodeErrorResponse(t, rec); resp.Error.Code != ErrCodeRuntimeAttachUnsupported {
		t.Errorf("expected code %q, got %q", ErrCodeRuntimeAttachUnsupported, resp.Error.Code)
	}
}
