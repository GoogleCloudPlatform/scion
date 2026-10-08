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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubAttachTerminal makes requireAttachTerminal see (or not see) a TTY for
// the duration of the test.
func stubAttachTerminal(t *testing.T, isTTY bool) {
	t.Helper()
	orig := attachTerminalCheck
	attachTerminalCheck = func() bool { return isTTY }
	t.Cleanup(func() { attachTerminalCheck = orig })
}

// stubAttachSession replaces the Hub PTY dial with fn and records the agent
// ID it was asked to attach.
func stubAttachSession(t *testing.T, fn func() error) *string {
	t.Helper()
	var gotID string
	orig := attachToAgentFn
	attachToAgentFn = func(_ context.Context, _, _, id string, _ ...wsclient.AttachOption) error {
		gotID = id
		return fn()
	}
	t.Cleanup(func() { attachToAgentFn = orig })
	return &gotID
}

// stubPlainTransport makes resolveAttachOptions report no transport auth.
func stubPlainTransport(t *testing.T) {
	t.Helper()
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	t.Cleanup(func() { resolveAttachTransportFn = orig })
}

// --- #3306: no TTY ---

func TestAttachCmd_NoTTY_FailsFast(t *testing.T) {
	stubAttachTerminal(t, false)
	origSilence := attachCmd.SilenceUsage
	t.Cleanup(func() { attachCmd.SilenceUsage = origSilence })

	err := attachCmd.RunE(attachCmd, []string{"some-agent"})
	require.ErrorIs(t, err, errAttachNeedsTerminal)
	assert.Contains(t, err.Error(), "attach requires an interactive terminal")
	assert.True(t, attachCmd.SilenceUsage, "a no-TTY failure is not a usage error")
}

func TestRunAgent_AttachNoTTY_FailsBeforeStarting(t *testing.T) {
	restore := saveAttachTestState()
	defer restore()
	stubAttachTerminal(t, false)
	attach = true

	// projectPath points nowhere useful: if the TTY check did not run first,
	// RunAgent would fail later with a different error (or try to start).
	origProjectPath := projectPath
	projectPath = t.TempDir()
	t.Cleanup(func() { projectPath = origProjectPath })

	err := RunAgent(startCmd, []string{"some-agent"}, false)
	require.ErrorIs(t, err, errAttachNeedsTerminal)

	err = RunAgent(resumeCmd, []string{"some-agent"}, true)
	require.ErrorIs(t, err, errAttachNeedsTerminal)
}

func TestStdioAreTerminals(t *testing.T) {
	assert.True(t, stdioAreTerminals(true, true))
	assert.False(t, stdioAreTerminals(true, false), "stdout captured (e.g. piped to tee) must fail")
	assert.False(t, stdioAreTerminals(false, true), "no TTY on stdin must fail")
	assert.False(t, stdioAreTerminals(false, false))
}

func TestRequireAttachTerminal_WithTTY(t *testing.T) {
	stubAttachTerminal(t, true)
	assert.NoError(t, requireAttachTerminal())
}

// --- #3307: not-running hint ---

func TestNotRunningAttachHint(t *testing.T) {
	tests := []struct {
		phase string
		want  string
	}{
		{"stopped", "scion resume a1 --attach"},
		{"suspended", "scion resume a1 --attach"},
		{"stopping", "scion resume a1 --attach"},
		{"error", "scion logs a1"},
		{"created", "scion start a1 --attach"},
		{"", "scion start a1 --attach"},
	}
	for _, tc := range tests {
		t.Run(tc.phase, func(t *testing.T) {
			assert.Contains(t, notRunningAttachHint(tc.phase, "a1"), tc.want)
		})
	}
}

func TestAttachViaHub_StoppedAgent_SuggestsResume(t *testing.T) {
	const (
		projectID = "proj-stopped"
		agentName = "stopped-agent"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects/"+projectID+"/agents/"+agentName {
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: "id-1", Name: agentName, Phase: "stopped"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not running (phase: stopped)")
	assert.Contains(t, err.Error(), "scion resume stopped-agent --attach")
	assert.NotContains(t, err.Error(), "scion start")
}

// --- #3305: close codes ---

func TestDescribeAttachClose(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		reason   string
		contains []string
	}{
		{name: "broker disconnected is retryable", code: wsprotocol.ClosePTYUpstreamUnavailable, reason: wsprotocol.CloseReasonBrokerDisconnected,
			contains: []string{"lost its connection to the agent's runtime broker", "close code 4503: broker_disconnected", "does not reconnect automatically", "scion attach a1"}},
		{name: "agent stopped suggests resume", code: wsprotocol.ClosePTYSessionGone, reason: wsprotocol.CloseReasonAgentStopped,
			contains: []string{"terminal session has ended", "close code 4410: agent_stopped", "scion resume a1 --attach"}},
		{name: "agent not found", code: wsprotocol.ClosePTYAgentNotFound,
			contains: []string{"cannot find the agent", "scion list"}},
		{name: "abnormal drop is retryable", code: wsprotocol.ClosePTYAbnormal,
			contains: []string{"dropped without a close message", "close code 1006", "scion attach a1"}},
		{name: "auth required", code: wsprotocol.ClosePTYAuthRequired,
			contains: []string{"credentials are no longer valid", "scion hub auth login"}},
		{name: "forbidden", code: wsprotocol.ClosePTYForbidden,
			contains: []string{"permission", "agent 'a1'"}},
		{name: "upstream timeout", code: wsprotocol.ClosePTYUpstreamTimeout,
			contains: []string{"did not start the session in time", "scion attach a1"}},
		{name: "input overflow asks for smaller pastes", code: 1009, reason: "input_overflow",
			contains: []string{"input was too large for the session", "close code 1009: input_overflow", "Paste in smaller chunks", "scion attach a1"}},
		{name: "unknown application code is terminal", code: 4999, reason: "new_reason",
			contains: []string{"the server ended the session", "close code 4999: new_reason", "scion list"}},
		{name: "unknown retryable code", code: 1014,
			contains: []string{"the server ended the session", "does not reconnect automatically"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &wsclient.PTYCloseError{Code: tc.code, Reason: tc.reason}
			err := describeAttachClose(in, "a1")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "attach to agent 'a1' ended: ")
			for _, want := range tc.contains {
				assert.Contains(t, err.Error(), want)
			}
			assert.NotContains(t, err.Error(), "{agent}")
			assert.NotContains(t, err.Error(), "%!")
			var ce *wsclient.PTYCloseError
			require.True(t, errors.As(err, &ce), "the original close error must stay reachable")
			assert.Equal(t, tc.code, ce.Code)
		})
	}
}

// The broker's input-overflow close (1009) gets an actionable message, and
// stays terminal: the CLI must not suggest the session will recover on its
// own by retrying the same paste.
func TestDescribeAttachClose_InputOverflowIsActionableAndTerminal(t *testing.T) {
	require.Equal(t, wsprotocol.DispositionTerminal, wsprotocol.ClassifyPTYClose(ptyCloseInputTooLarge))
	err := describeAttachClose(&wsclient.PTYCloseError{Code: ptyCloseInputTooLarge, Reason: "input_overflow"}, "a1")
	require.Error(t, err)
	assert.Equal(t, "attach to agent 'a1' ended: the input was too large for the session "+
		"(pasted faster than the agent could read it) (close code 1009: input_overflow)\n\n"+
		"Paste in smaller chunks, then reattach with: scion attach a1", err.Error())
	assert.NotContains(t, err.Error(), "does not reconnect automatically")
}

func TestDescribeAttachClose_HintFollowsClassifier(t *testing.T) {
	// Rows without their own hint get the hint for their ClassifyPTYClose
	// disposition, so the table never disagrees with the classifier.
	for code, msg := range ptyCloseMessages {
		if msg.Hint != "" {
			continue
		}
		err := describeAttachClose(&wsclient.PTYCloseError{Code: code}, "a1")
		retry := wsprotocol.ClassifyPTYClose(code) == wsprotocol.DispositionRetry
		assert.Equal(t, retry, strings.Contains(err.Error(), "does not reconnect automatically"), "code %d", code)
	}
}

func TestDescribeAttachClose_OtherErrorsUnchanged(t *testing.T) {
	in := errors.New("connection failed with status 403: forbidden")
	assert.Same(t, in, describeAttachClose(in, "a1"))
	assert.NoError(t, describeAttachClose(nil, "a1"))
}

// --- #3307 / #3305: start -a and attach share one flow ---

func TestAttachViaHub_CloseCodeIsDescribed(t *testing.T) {
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", "test-token")
	stubPlainTransport(t)
	gotID := stubAttachSession(t, func() error {
		return &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYSessionGone, Reason: wsprotocol.CloseReasonAgentStopped}
	})

	srv := newAttachMockHubServer(t, "proj-cc", "cc-agent", "cc-uuid", "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-cc"}, "cc-agent")
	require.Error(t, err)
	assert.Equal(t, "cc-uuid", *gotID)
	assert.Contains(t, err.Error(), "attach to agent 'cc-agent' ended")
	assert.Contains(t, err.Error(), "scion resume cc-agent --attach")
}

// TestStartAgentViaHub_Attach_CloseCodeIsDescribed covers start -a and
// resume -a: both go through attachHubSession, so a close code is described
// the same way scion attach describes it.
func TestStartAgentViaHub_Attach_CloseCodeIsDescribed(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "start -a"
		if resume {
			name = "resume -a"
		}
		t.Run(name, func(t *testing.T) {
			clearAppTokenSources(t)
			t.Setenv("SCION_HUB_TOKEN", "test-token")
			stubPlainTransport(t)
			gotID := stubAttachSession(t, func() error {
				return &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYUpstreamUnavailable, Reason: wsprotocol.CloseReasonBrokerDisconnected}
			})

			restore := saveAttachTestState()
			defer restore()
			attach = true
			templateName = ""
			labelFlags = nil
			runtimeBrokerID = ""
			harnessConfigFlag = ""
			harnessAuthFlag = ""

			srv := newStartAgentMockHubServer(t, "proj-start-cc", "start-cc-agent", "start-cc-uuid", "")
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			err = startAgentViaHub(nil, &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-start-cc"}, "start-cc-agent", "", resume, nil)
			require.Error(t, err)
			assert.Equal(t, "start-cc-uuid", *gotID)
			assert.Contains(t, err.Error(), "attach to agent 'start-cc-agent' ended")
			assert.Contains(t, err.Error(), "close code 4503: broker_disconnected")
			assert.Contains(t, err.Error(), "scion attach start-cc-agent")
		})
	}
}

// TestAttachViaHub_StoppedUnsupportedAgent_ReportsUnsupportedFirst: a stopped
// agent on a runtime that can never be attached gets the unsupported error,
// not a hint to resume it first.
func TestAttachViaHub_StoppedUnsupportedAgent_ReportsUnsupportedFirst(t *testing.T) {
	const (
		projectID = "proj-stopped-noattach"
		agentName = "stopped-noattach"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/" + projectID + "/agents/" + agentName:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: "id-2", Name: agentName, Phase: "stopped",
				Runtime: "noattach", RuntimeBrokerID: mockAttachBrokerID})
		case "/api/v1/runtime-brokers/" + mockAttachBrokerID:
			_ = json.NewEncoder(w).Encode(mockAttachBroker("noattach"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Equal(t, "attach is not supported for agents on the noattach runtime", err.Error())
}

// TestAttachViaHub_GateRunsOnce: scion attach runs the capability gate before
// the phase check and tells attachHubSession not to repeat the broker lookup.
func TestAttachViaHub_GateRunsOnce(t *testing.T) {
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", "test-token")
	stubPlainTransport(t)
	stubAttachSession(t, func() error { return nil })

	const (
		projectID = "proj-gate-once"
		agentName = "gate-once"
	)
	brokerGets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/" + projectID + "/agents/" + agentName:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: "id-3", Name: agentName, Phase: "running", RuntimeBrokerID: mockAttachBrokerID})
		case "/api/v1/runtime-brokers/" + mockAttachBrokerID:
			brokerGets++
			_ = json.NewEncoder(w).Encode(mockAttachBroker(""))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	require.NoError(t, attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName))
	assert.Equal(t, 1, brokerGets)
}

func TestAttachHubSession_CleanDetachReturnsNil(t *testing.T) {
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", "test-token")
	stubPlainTransport(t)
	stubAttachSession(t, func() error { return nil })

	err := attachHubSession(context.Background(), &HubContext{Endpoint: "http://hub.invalid"}, hubAttachTarget{Name: "a1", ID: "id-1"})
	assert.NoError(t, err)
}

// --- #3311: local not-found message ---

// fakeEmptyRuntimeBinary writes a runtime binary that prints out for every
// call, standing in for a runtime whose container list is empty.
func fakeEmptyRuntimeBinary(t *testing.T, out string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-runtime")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '"+out+"'\n"), 0o700))
	return bin
}

// TestLocalAttachError_RealRuntimeErrors feeds each local runtime's own
// Attach "not found" error (produced by the real runtime code against an
// empty container list) through localAttachError, so a runtime rewording
// its error fails this test instead of silently hiding the friendly message.
func TestLocalAttachError_RealRuntimeErrors(t *testing.T) {
	const id = "c0ffee"
	tests := []struct {
		name string
		rt   runtime.Runtime
	}{
		{"docker", &runtime.DockerRuntime{Command: fakeEmptyRuntimeBinary(t, "")}},
		{"podman", &runtime.PodmanRuntime{Command: fakeEmptyRuntimeBinary(t, "[]")}},
		{"apple container", &runtime.AppleContainerRuntime{Command: fakeEmptyRuntimeBinary(t, "[]")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rtErr := tc.rt.Attach(context.Background(), id)
			require.Error(t, rtErr)
			got := localAttachError(rtErr, id, "a1", "proj")
			assert.Contains(t, got.Error(), "agent 'a1' not found in project 'proj'",
				"runtime error %q was not recognised as not-found", rtErr)
		})
	}
}

func TestLocalAttachError(t *testing.T) {
	const id = "c0ffee"
	tests := []struct {
		name     string
		in       error
		friendly bool
	}{
		// Current wording from pkg/runtime docker.go/podman.go/apple_container.go.
		{"container not found", fmt.Errorf("agent '%s' container not found, it may have exited and been removed", id), true},
		{"pod not found", fmt.Errorf("agent '%s' pod not found, it may have been deleted", id), true},
		{"plain not found", fmt.Errorf("agent '%s' not found", id), true},
		{"other ID", fmt.Errorf("agent '%s' container not found, it may have exited and been removed", "other"), false},
		{"not running", fmt.Errorf("agent '%s' is not running (status: exited)", id), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := localAttachError(tc.in, id, "a1", "proj")
			if tc.friendly {
				assert.Contains(t, got.Error(), "agent 'a1' not found in project 'proj'")
			} else {
				assert.Same(t, tc.in, got)
			}
		})
	}
	assert.NoError(t, localAttachError(nil, id, "a1", "proj"))
}
