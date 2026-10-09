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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
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
			contains: []string{"lost its connection to the agent's runtime broker", "close code 4503: broker_disconnected", "This may be temporary", "scion attach a1"}},
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
			contains: []string{"the server ended the session", "This may be temporary"}},
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
	assert.NotContains(t, err.Error(), "This may be temporary")
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
		assert.Equal(t, retry, strings.Contains(err.Error(), "This may be temporary"), "code %d", code)
	}
}

// A close whose one automatic reconnect failed is described by the original
// close code, plus why the reconnect failed, and both stay reachable.
func TestDescribeAttachClose_ReconnectFailed(t *testing.T) {
	orig := &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYUpstreamUnavailable, Reason: "relay_restart"}
	dialErr := errors.New("connection failed with status 503: no session")
	err := describeAttachClose(&wsclient.PTYReconnectError{Close: orig, Err: dialErr}, "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "close code 4503: relay_restart")
	assert.Contains(t, err.Error(), "The automatic reconnect also failed: connection failed with status 503: no session")
	assert.Contains(t, err.Error(), "try again with: scion attach a1")
	var ce *wsclient.PTYCloseError
	require.True(t, errors.As(err, &ce))
	assert.Same(t, orig, ce)
	assert.ErrorIs(t, err, dialErr)
}

// When the automatic reconnect itself ended with a close code, the message
// and hint follow that close, and mention the close that triggered it.
func TestDescribeAttachClose_ReconnectEndedWithClose(t *testing.T) {
	orig := &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYUpstreamUnavailable, Reason: "relay_restart"}
	second := &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYSessionGone, Reason: wsprotocol.CloseReasonAgentStopped}
	err := describeAttachClose(&wsclient.PTYReconnectError{Close: orig, Err: second}, "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal session has ended")
	assert.Contains(t, err.Error(), "close code 4410: agent_stopped")
	assert.Contains(t, err.Error(), "on the automatic reconnect after close code 4503: relay_restart")
	assert.Contains(t, err.Error(), "scion resume a1 --attach")
	assert.NotContains(t, err.Error(), "This may be temporary")
	var ce *wsclient.PTYCloseError
	require.True(t, errors.As(err, &ce))
	assert.Same(t, orig, ce, "the original close stays reachable first")
}

// When the CLI stopped because too many reconnected sessions ended quickly,
// the message says so rather than offering only the generic retry text.
func TestDescribeAttachClose_ReconnectLimit(t *testing.T) {
	orig := &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYUpstreamUnavailable, Reason: "relay_restart"}
	err := describeAttachClose(&wsclient.PTYReconnectError{Close: orig, Err: wsclient.ErrPTYReconnectLimit}, "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "close code 4503: relay_restart")
	assert.Contains(t, err.Error(), "scion attach stopped after 3 automatic reconnects whose sessions each ended within a minute.")
	assert.NotContains(t, err.Error(), "automatic reconnect also failed")
	assert.ErrorIs(t, err, wsclient.ErrPTYReconnectLimit)
}

// A Hub refusal at the automatic reconnect's preflight ends the attach
// with the original close and the Hub's reason; describeAttachPreflight
// leaves it to describeAttachClose.
func TestDescribeAttachClose_ReconnectRefusedByHub(t *testing.T) {
	orig := &wsclient.PTYCloseError{Code: wsprotocol.ClosePTYUpstreamUnavailable, Reason: "relay_restart"}
	t.Run("no path", func(t *testing.T) {
		refusal := &wsclient.PTYPreflightError{Status: 503, Code: wsprotocol.ErrCodeRuntimeAttachUnsupported,
			Reason: "agent_pty_unavailable", Message: "No path to the terminal"}
		in := &wsclient.PTYReconnectError{Close: orig, Err: refusal}
		err := describeAttachPreflight(describeAttachClose(in, "a1"), "a1")
		assert.Contains(t, err.Error(), "attach to agent 'a1' ended:")
		assert.Contains(t, err.Error(), "close code 4503: relay_restart")
		assert.Contains(t, err.Error(), "The Hub refused the automatic reconnect: "+wsclient.AttachUnsupportedMessage)
		assert.Contains(t, err.Error(), "(status 503, runtime_attach_unsupported, reason agent_pty_unavailable)")
		assert.Contains(t, err.Error(), "Check the agent with: scion list")
		assert.NotContains(t, err.Error(), "try again")
		assert.ErrorIs(t, err, refusal)
	})
	t.Run("denied: same text and hint as on the first attach", func(t *testing.T) {
		refusal := &wsclient.PTYPreflightError{Status: 403, Code: "forbidden", Message: "no access"}
		in := &wsclient.PTYReconnectError{Close: orig, Err: refusal}
		err := describeAttachPreflight(describeAttachClose(in, "a1"), "a1")
		assert.Contains(t, err.Error(),
			"The Hub refused the automatic reconnect: you do not have permission to attach to this agent (status 403, forbidden).")
		assert.Contains(t, err.Error(), "Ask a project owner for attach access to agent 'a1'")
		assert.NotContains(t, err.Error(), "try again")
	})
	t.Run("empty message gets the fixed summary", func(t *testing.T) {
		for _, tc := range []struct {
			refusal *wsclient.PTYPreflightError
			want    string
		}{
			{&wsclient.PTYPreflightError{Status: 503}, "the Hub cannot attach to this agent right now (status 503)."},
			{&wsclient.PTYPreflightError{Status: 502}, "the Hub refused the attach (status 502)."},
		} {
			in := &wsclient.PTYReconnectError{Close: orig, Err: tc.refusal}
			err := describeAttachPreflight(describeAttachClose(in, "a1"), "a1")
			assert.Contains(t, err.Error(), "The Hub refused the automatic reconnect: "+tc.want)
			assert.NotContains(t, err.Error(), ":  (")
		}
	})
	t.Run("direct preflight refusal still described by describeAttachPreflight", func(t *testing.T) {
		refusal := &wsclient.PTYPreflightError{Status: 403, Code: "forbidden", Message: "no"}
		err := describeAttachPreflight(describeAttachClose(refusal, "a1"), "a1")
		assert.Contains(t, err.Error(), "cannot attach to agent 'a1'")
	})
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

// TestAttachViaHub_StoppedAgent_SuggestsResumeWithoutBrokerLookup: the CLI
// no longer refuses from the broker record, so a stopped agent on a broker
// whose runtime has no attach is told to resume (once running, it may be
// attachable through the agent path), and the broker record is never read.
func TestAttachViaHub_StoppedAgent_SuggestsResumeWithoutBrokerLookup(t *testing.T) {
	const (
		projectID = "proj-stopped-noattach"
		agentName = "stopped-noattach"
	)
	var brokerGets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects/"+projectID+"/agents/"+agentName:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: "id-2", Name: agentName, Phase: "stopped",
				Runtime: "noattach", RuntimeBrokerID: mockAttachBrokerID})
		case strings.HasPrefix(r.URL.Path, "/api/v1/runtime-brokers"):
			brokerGets.Add(1)
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scion resume "+agentName+" --attach")
	assert.EqualValues(t, 0, brokerGets.Load(), "the CLI does not read the broker record")
}

// ptyHub is a mock Hub for the full attach flow: the agent record, the
// broker record (which says attach is unsupported, and is counted), and
// the agent's /pty endpoint. A plain GET of /pty answers preflightStatus
// and preflightBody; a WebSocket upgrade is accepted, sends one data frame
// and closes 1000 (a clean detach).
type ptyHub struct {
	srv        *httptest.Server
	preflights atomic.Int32
	upgrades   atomic.Int32
	brokerGets atomic.Int32
}

func newPTYHub(t *testing.T, projectID, agentName, agentID string, preflightStatus int, preflightBody string) *ptyHub {
	t.Helper()
	h := &ptyHub{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/"+projectID+"/agents/"+agentName:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: agentID, Name: agentName, Phase: "running",
				RuntimeBrokerID: mockAttachBrokerID})
		case strings.HasPrefix(r.URL.Path, "/api/v1/runtime-brokers"):
			h.brokerGets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{ID: mockAttachBrokerID,
				Capabilities: &hubclient.BrokerCapabilities{Attach: false}})
		case r.URL.Path == "/api/v1/agents/"+agentID+"/pty" && websocket.IsWebSocketUpgrade(r):
			h.upgrades.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte("$ ")))
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(wsprotocol.ClosePTYNormal, ""), time.Now().Add(time.Second))
		case r.URL.Path == "/api/v1/agents/"+agentID+"/pty":
			h.preflights.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(preflightStatus)
			_, _ = w.Write([]byte(preflightBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// TestAttachViaHub_UnsupportedBrokerAgentPath_Attaches: an agent whose
// broker runtime has no attach, but which the Hub can reach through the
// agent path (preflight 200, path agent), is attached: the CLI does not
// refuse from the broker record, and dials after the preflight.
func TestAttachViaHub_UnsupportedBrokerAgentPath_Attaches(t *testing.T) {
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", "test-token")
	stubPlainTransport(t)

	h := newPTYHub(t, "proj-agent-path", "agent-path", "agent-path-id", http.StatusOK, `{"path":"agent"}`)
	client, err := hubclient.New(h.srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: h.srv.URL, ProjectID: "proj-agent-path"}, "agent-path")
	require.NoError(t, err)
	assert.EqualValues(t, 1, h.preflights.Load())
	assert.EqualValues(t, 1, h.upgrades.Load(), "the attach dials after the preflight")
	assert.EqualValues(t, 0, h.brokerGets.Load(), "the CLI does not read the broker record")
}

// TestAttachViaHub_NoPath_ExitsWithReasonWithoutRetry: when the Hub has no
// path to the agent's terminal, the attach ends with the Hub's reason after
// one preflight, with no WebSocket dial and no retry.
func TestAttachViaHub_NoPath_ExitsWithReasonWithoutRetry(t *testing.T) {
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", "test-token")
	stubPlainTransport(t)

	h := newPTYHub(t, "proj-no-path", "no-path", "no-path-id", http.StatusServiceUnavailable, noPathPreflightBody)
	client, err := hubclient.New(h.srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: h.srv.URL, ProjectID: "proj-no-path"}, "no-path")
	assertNoPathRefusal(t, err, "no-path")
	assert.Contains(t, err.Error(), wsclient.AttachUnsupportedMessage)
	assert.EqualValues(t, 1, h.preflights.Load(), "one preflight, no retry")
	assert.EqualValues(t, 0, h.upgrades.Load(), "no WebSocket dial")
}

// TestDescribeAttachPreflight covers the CLI's wording for preflight
// refusals: 401, 403 and 404 reuse the close-code hints, 422 says the
// agent has no runtime broker, a no-path 503 is final, another 503 is
// presented as temporary, and the "status N" detail is always kept.
func TestDescribeAttachPreflight(t *testing.T) {
	tests := []struct {
		name     string
		in       *wsclient.PTYPreflightError
		contains []string
		excludes []string
	}{
		{name: "401", in: &wsclient.PTYPreflightError{Status: 401, Code: "unauthorized", Message: "Authentication required"},
			contains: []string{"cannot attach to agent 'a1': your Hub credentials are not valid (status 401, unauthorized)", "scion hub auth login"}},
		{name: "403", in: &wsclient.PTYPreflightError{Status: 403, Code: "forbidden", Message: "no"},
			contains: []string{"you do not have permission to attach to this agent (status 403, forbidden)", "attach access to agent 'a1'"}},
		{name: "404", in: &wsclient.PTYPreflightError{Status: 404, Code: "not_found", Message: "Agent not found"},
			contains: []string{"the Hub cannot find the agent (status 404, not_found)", "scion list"}},
		{name: "422", in: &wsclient.PTYPreflightError{Status: 422, Code: "no_runtime_broker", Message: "Agent has no runtime broker"},
			contains: []string{"the agent has no runtime broker (status 422, no_runtime_broker)", "Check the agent with: scion list"}},
		{name: "no path", in: &wsclient.PTYPreflightError{Status: 503, Code: wsprotocol.ErrCodeRuntimeAttachUnsupported, Reason: "agent_pty_unavailable"},
			contains: []string{wsclient.AttachUnsupportedMessage + ", and the agent has no session that serves a terminal",
				"(status 503, runtime_attach_unsupported, reason agent_pty_unavailable)", "Check the agent with: scion list"},
			excludes: []string{"try again"}},
		{name: "other 503", in: &wsclient.PTYPreflightError{Status: 503, Code: "runtime_broker_unavailable",
			Reason: "broker_not_connected", Message: "Runtime broker not connected"},
			contains: []string{"Runtime broker not connected (status 503, runtime_broker_unavailable, reason broker_not_connected)",
				"try again with: scion attach a1"}},
		{name: "503 without a body", in: &wsclient.PTYPreflightError{Status: 503},
			contains: []string{"the Hub cannot attach to this agent right now (status 503)"},
			excludes: []string{"(503 )", "status 503, )"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := describeAttachPreflight(tc.in, "a1")
			for _, want := range tc.contains {
				assert.Contains(t, err.Error(), want)
			}
			for _, not := range tc.excludes {
				assert.NotContains(t, err.Error(), not)
			}
			assert.NotContains(t, err.Error(), "{agent}")
			assert.ErrorIs(t, err, tc.in)
		})
	}
	t.Run("other statuses unchanged", func(t *testing.T) {
		in := &wsclient.PTYPreflightError{Status: 502, Message: "bad gateway"}
		assert.Same(t, error(in), describeAttachPreflight(in, "a1"))
	})
	t.Run("other errors unchanged", func(t *testing.T) {
		in := errors.New("x")
		assert.Same(t, in, describeAttachPreflight(in, "a1"))
	})
}

// TestDescribeAttachPreflight_403KeepsUATHint: the described 403 still
// carries "status 403", so a user access token gets the scope hint.
func TestDescribeAttachPreflight_403KeepsUATHint(t *testing.T) {
	in := &wsclient.PTYPreflightError{Status: 403, Code: "forbidden", Message: "no"}
	err := attachErrorWithUATHint(describeAttachPreflight(in, "a1"), store.UATPrefix+"abc")
	assert.Contains(t, err.Error(), "may lack agent:attach")
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
