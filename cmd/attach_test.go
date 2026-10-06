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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransportSource implements transportauth.TokenSource for testing.
type fakeTransportSource struct {
	token  string
	expiry time.Time
}

func (f *fakeTransportSource) Token() (string, error) {
	if f.token == "" {
		return "", fmt.Errorf("no transport token")
	}
	return f.token, nil
}
func (f *fakeTransportSource) SetToken(token string, expiry time.Time) {
	f.token = token
	f.expiry = expiry
}
func (f *fakeTransportSource) Expiry() time.Time { return f.expiry }

// clearAppTokenSources clears all env-var and credential sources for app tokens,
// leaving getHubAccessToken() returning "". It also points to an empty tmpDir
// for credential storage so that no OAuth token is present.
func clearAppTokenSources(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	t.Cleanup(func() { credentials.SetCredentialsPath(origPath) })

	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("HOME", tmpDir)
}

// mockAttachBrokerID is the fixed runtime broker ID the mock Hub servers in
// this file put on agent records, and the ID they serve a matching
// GET /api/v1/runtime-brokers/{id} response under — exercising the CLI's
// broker-metadata attach check (attachSupportedByBroker) the same way a real
// Hub round-trip would, rather than a name-based shortcut.
const mockAttachBrokerID = "test-broker-1"

// mockAttachBroker returns a minimal hubclient.RuntimeBroker whose
// broker-wide Capabilities.Attach reflects whether runtimeType supports
// attach. These fixtures set no agent profile, so attachSupportedByBroker
// falls back to this broker-wide value rather than a per-profile one. Only
// "noattach" is unsupported today.
func mockAttachBroker(runtimeType string) hubclient.RuntimeBroker {
	return hubclient.RuntimeBroker{
		ID: mockAttachBrokerID,
		Capabilities: &hubclient.BrokerCapabilities{
			Attach: runtimeType != "noattach",
		},
	}
}

// newAttachMockHubServer creates a mock Hub server that handles the agent GET
// request needed by attachViaHub(), plus the runtime-broker GET that backs
// its attach-capability check. The agent is returned in the "running" phase
// with the given runtime string (use "" for a normal non-managed agent).
func newAttachMockHubServer(t *testing.T, projectID, agentName, agentID, runtime string) *httptest.Server {
	t.Helper()

	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	brokerPath := "/api/v1/runtime-brokers/" + mockAttachBrokerID

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			agent := hubclient.Agent{
				ID:              agentID,
				Name:            agentName,
				Phase:           "running",
				Runtime:         runtime,
				RuntimeBrokerID: mockAttachBrokerID,
			}
			_ = json.NewEncoder(w).Encode(agent)
		case r.Method == http.MethodGet && r.URL.Path == brokerPath:
			_ = json.NewEncoder(w).Encode(mockAttachBroker(runtime))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAttachCmd_RuntimeErrorSilencesUsage is a regression test for
// ptone/scion#2089: a runtime attach failure (agent not found, in this case —
// standing in for any post-argument-validation failure such as an abnormal
// session close) must set SilenceUsage on the command so Execute does not
// print the cobra usage block after it, while the error itself is still
// returned so the exit code stays non-zero.
func TestAttachCmd_RuntimeErrorSilencesUsage(t *testing.T) {
	origNoHub := noHub
	origProjectPath := projectPath
	origSilenceUsage := attachCmd.SilenceUsage
	defer func() {
		noHub = origNoHub
		projectPath = origProjectPath
		attachCmd.SilenceUsage = origSilenceUsage
	}()

	noHub = true
	projectPath = t.TempDir() // no agents/ dir present -> deterministic "not found"
	attachCmd.SilenceUsage = false
	stubAttachTerminal(t, true)

	err := attachCmd.RunE(attachCmd, []string{"does-not-exist"})

	require.Error(t, err, "expected a runtime error for a nonexistent agent")
	assert.Contains(t, err.Error(), "not found")
	assert.True(t, attachCmd.SilenceUsage,
		"a runtime attach error must silence usage so Execute does not print it")
}

// TestAttachCmd_BadArgsDoesNotSilenceUsage is the counterpart regression test:
// a bad-arguments invocation (wrong arg count) is rejected by cobra's Args
// validator before RunE ever runs, so it must not silence usage — real
// argument errors should still show the usage block.
func TestAttachCmd_BadArgsDoesNotSilenceUsage(t *testing.T) {
	origSilenceUsage := attachCmd.SilenceUsage
	defer func() { attachCmd.SilenceUsage = origSilenceUsage }()

	attachCmd.SilenceUsage = false

	err := attachCmd.Args(attachCmd, []string{})

	require.Error(t, err, "expected an args error for a missing <agent> argument")
	assert.False(t, attachCmd.SilenceUsage,
		"a bad-arguments error must leave usage enabled")
}

// TestAttachSupportedByBroker_ProfileAttachFalse_ReturnsFalse verifies that
// attachSupportedByBroker honors a named profile's own Attach=false, even
// though the broker-wide Capabilities.Attach on this fixture is true — a
// false result here can only come from the profile-level match, not the
// broker-wide fallback.
func TestAttachSupportedByBroker_ProfileAttachFalse_ReturnsFalse(t *testing.T) {
	falseVal := false
	const brokerID = "broker-profile-false"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-brokers/"+brokerID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{
			ID: brokerID,
			Profiles: []hubclient.BrokerProfile{
				{Name: "noattach-prof", Type: "noattach", Attach: &falseVal},
			},
			Capabilities: &hubclient.BrokerCapabilities{Attach: true},
		})
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL}

	supported, unreadable := attachSupportedByBroker(context.Background(), hubCtx, brokerID, "noattach-prof")
	if supported {
		t.Error("attachSupportedByBroker supported = true, want false for a profile with Attach=false")
	}
	if unreadable {
		t.Error("attachSupportedByBroker unreadable = true, want false: the record was read successfully")
	}
}

// TestAttachSupportedByBroker_ProfileFieldAbsent_ReturnsTrue verifies the
// missing-field-⇒-supported default at the profile level: a profile entry
// present in the broker's list, but whose JSON carries no "attach" key at
// all (an older broker's exact wire shape), is treated as supported. The
// response body here is raw JSON rather than a hubclient.BrokerProfile{}
// literal, so this proves the *bool json:"attach,omitempty" decode itself
// leaves Attach nil, not just that a Go zero value happens to be nil.
func TestAttachSupportedByBroker_ProfileFieldAbsent_ReturnsTrue(t *testing.T) {
	const brokerID = "broker-profile-absent"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-brokers/"+brokerID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"profiles":[{"name":"old-prof","type":"docker","available":true}]}`, brokerID)
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL}

	supported, unreadable := attachSupportedByBroker(context.Background(), hubCtx, brokerID, "old-prof")
	if !supported {
		t.Error(`attachSupportedByBroker supported = false, want true for a profile whose JSON omits "attach" entirely`)
	}
	if unreadable {
		t.Error("attachSupportedByBroker unreadable = true, want false: the record was read successfully")
	}
}

// TestAttachSupportedByBroker_NoBrokerID_ReturnsTrueWithoutCallingHub proves
// the missing-broker-ID default fires without making any Hub call:
// hubCtx.Client is nil here, so calling it would panic.
func TestAttachSupportedByBroker_NoBrokerID_ReturnsTrueWithoutCallingHub(t *testing.T) {
	hubCtx := &HubContext{Client: nil}
	supported, unreadable := attachSupportedByBroker(context.Background(), hubCtx, "", "any-profile")
	if !supported {
		t.Error("attachSupportedByBroker supported = false, want true when runtimeBrokerID is empty")
	}
	if unreadable {
		t.Error("attachSupportedByBroker unreadable = true, want false when there is nothing to read")
	}
}

// newAttachGateFallbackServer builds a mock Hub server for exercising the
// CLI's point-GET-then-LIST-fallback attach gate end to end through
// attachViaHub: the agent GET matches newAttachMockHubServer's shape, the
// runtime-broker point-GET always answers 403 (a hub-member principal can be
// denied the point-GET yet still see the broker on LIST — LIST is
// deliberately left with its own, wider read scope rather than this test
// widening anything), and the LIST response is
// either listBrokers or a 403 when listFails is set. The returned *bool
// flips true if the PTY WebSocket path is ever requested, so callers can
// prove either that attachViaHub refused before dialing, or that it
// proceeded to dial.
func newAttachGateFallbackServer(t *testing.T, projectID, agentName, agentID, brokerID string, listBrokers []hubclient.RuntimeBroker, listFails bool) (*httptest.Server, *bool) {
	t.Helper()
	dialed := false
	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	brokerPath := "/api/v1/runtime-brokers/" + brokerID
	ptyPath := "/api/v1/agents/" + agentID + "/pty"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID:              agentID,
				Name:            agentName,
				Phase:           "running",
				RuntimeBrokerID: brokerID,
			})
		case r.Method == http.MethodGet && r.URL.Path == brokerPath:
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers":
			if listFails {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": listBrokers})
		case r.URL.Path == ptyPath:
			dialed = true
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &dialed
}

// TestAttachViaHub_PointGETForbidden_ListShowsAttachFalse_RefusesPreDial
// covers a hub-member principal: the point-GET returns
// 403, the LIST fallback succeeds and shows the broker with attach=false,
// and attachViaHub must refuse before ever dialing the PTY WebSocket.
func TestAttachViaHub_PointGETForbidden_ListShowsAttachFalse_RefusesPreDial(t *testing.T) {
	clearAppTokenSources(t)
	attachTransportFakeAllowsDial(t)

	const (
		projectID = "proj-gate-1"
		agentName = "gate-agent-1"
		agentID   = "agent-uuid-gate-1"
		brokerID  = "gate-broker-1"
	)
	srv, dialed := newAttachGateFallbackServer(t, projectID, agentName, agentID, brokerID,
		[]hubclient.RuntimeBroker{{ID: brokerID, Capabilities: &hubclient.BrokerCapabilities{Attach: false}}}, false)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	err = attachViaHub(hubCtx, agentName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attach is not supported")
	assert.False(t, *dialed, "the PTY WebSocket must never be dialed when the gate refuses")
}

// TestAttachViaHub_PointGETForbidden_ListAlsoFails_ProceedsToDial covers a
// principal without broker-record read access (e.g. a UAT): neither the
// point-GET nor the LIST fallback can produce the broker record (both 403),
// which is this client's read access, not the runtime's own attach support.
// attachViaHub must
// treat that as unknown and proceed to dial rather than refuse client-side
// — the broker's own dial-path gate stays authoritative for a runtime that
// genuinely doesn't support attach.
func TestAttachViaHub_PointGETForbidden_ListAlsoFails_ProceedsToDial(t *testing.T) {
	clearAppTokenSources(t)
	attachTransportFakeAllowsDial(t)

	const (
		projectID = "proj-gate-2"
		agentName = "gate-agent-2"
		agentID   = "agent-uuid-gate-2"
		brokerID  = "gate-broker-2"
	)
	srv, dialed := newAttachGateFallbackServer(t, projectID, agentName, agentID, brokerID, nil, true)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	err = attachViaHub(hubCtx, agentName)
	require.Error(t, err, "expected a WS dial error — the gate must not have refused pre-dial")
	assert.NotContains(t, err.Error(), "attach is not supported")
	assert.NotContains(t, err.Error(), "cannot determine whether this agent's runtime supports attach")
	assert.True(t, *dialed, "the PTY WebSocket must be dialed when the broker record is unreadable — the broker's own gate is authoritative")
}

// TestAttachViaHub_PointGETForbidden_ListShowsAttachTrue_PassesGate covers
// the case where the LIST fallback shows the broker as attach-capable:
// attachViaHub must proceed past the gate and reach the WebSocket dial
// stage (which then fails because the mock server doesn't handle WebSocket
// upgrades) — that failure mode is what proves the gate was cleared.
func TestAttachViaHub_PointGETForbidden_ListShowsAttachTrue_PassesGate(t *testing.T) {
	clearAppTokenSources(t)
	attachTransportFakeAllowsDial(t)

	const (
		projectID = "proj-gate-3"
		agentName = "gate-agent-3"
		agentID   = "agent-uuid-gate-3"
		brokerID  = "gate-broker-3"
	)
	srv, dialed := newAttachGateFallbackServer(t, projectID, agentName, agentID, brokerID,
		[]hubclient.RuntimeBroker{{ID: brokerID, Capabilities: &hubclient.BrokerCapabilities{Attach: true}}}, false)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	err = attachViaHub(hubCtx, agentName)
	require.Error(t, err, "expected a WS dial error — the gate should have been cleared")
	assert.NotContains(t, err.Error(), "attach is not supported")
	assert.NotContains(t, err.Error(), "cannot determine whether this agent's runtime supports attach")
	assert.True(t, *dialed, "the PTY WebSocket should have been dialed once the gate passed")
}

// attachTransportFakeAllowsDial installs the fake resolveAttachTransportFn
// every test in this file that needs to reach the WebSocket dial stage
// uses, restoring the original via t.Cleanup. Without it, a broken gate
// would fail at the token gate instead of ever reaching a dial, leaving any
// "no dial happened" assertion true regardless of whether the gate under
// test actually fired.
func attachTransportFakeAllowsDial(t *testing.T) {
	t.Helper()
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return &fakeTransportSource{token: "fake-oidc-token", expiry: time.Now().Add(time.Hour)}, transportauth.HeaderProxyAuthorization, nil
	}
	t.Cleanup(func() { resolveAttachTransportFn = orig })
}

// TestAttachViaHub_PointGETForbidden_ListHasMultipleBrokers_MatchesByID pins
// that the LIST fallback matches the target broker by ID rather than, say,
// taking the first or last entry in the response: the target sits in the
// MIDDLE of the list, with a decoy showing attach=true on each side. A
// match that ignored ID and returned the first broker, or one that
// returned the last, would both read a decoy's attach=true and proceed to
// dial; only the correct by-ID match reads the target's attach=false and
// refuses before ever dialing.
func TestAttachViaHub_PointGETForbidden_ListHasMultipleBrokers_MatchesByID(t *testing.T) {
	clearAppTokenSources(t)
	attachTransportFakeAllowsDial(t)

	const (
		projectID     = "proj-gate-multi-1"
		agentName     = "gate-agent-multi-1"
		agentID       = "agent-uuid-gate-multi-1"
		brokerID      = "gate-broker-multi-1-target"
		decoyBeforeID = "gate-broker-multi-1-decoy-before"
		decoyAfterID  = "gate-broker-multi-1-decoy-after"
	)
	srv, dialed := newAttachGateFallbackServer(t, projectID, agentName, agentID, brokerID,
		[]hubclient.RuntimeBroker{
			{ID: decoyBeforeID, Capabilities: &hubclient.BrokerCapabilities{Attach: true}},
			{ID: brokerID, Capabilities: &hubclient.BrokerCapabilities{Attach: false}},
			{ID: decoyAfterID, Capabilities: &hubclient.BrokerCapabilities{Attach: true}},
		}, false)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	err = attachViaHub(hubCtx, agentName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attach is not supported")
	assert.False(t, *dialed, "a decoy broker listed before or after the target must not be mistaken for it")
}

// TestAttachViaHub_PointGETForbidden_ListHasMultipleBrokers_TargetTrueAmongFalseDecoys
// is the mirror case: a decoy showing attach=false is listed first, and
// the target — matching agent.RuntimeBrokerID — shows attach=true. A
// match that ignored ID (or returned the first broker) would read the
// decoy's attach=false and refuse; the correct match reads the target's
// attach=true and proceeds to dial.
func TestAttachViaHub_PointGETForbidden_ListHasMultipleBrokers_TargetTrueAmongFalseDecoys(t *testing.T) {
	clearAppTokenSources(t)
	attachTransportFakeAllowsDial(t)

	const (
		projectID = "proj-gate-multi-2"
		agentName = "gate-agent-multi-2"
		agentID   = "agent-uuid-gate-multi-2"
		brokerID  = "gate-broker-multi-2-target"
		decoyID   = "gate-broker-multi-2-decoy"
	)
	srv, dialed := newAttachGateFallbackServer(t, projectID, agentName, agentID, brokerID,
		[]hubclient.RuntimeBroker{
			{ID: decoyID, Capabilities: &hubclient.BrokerCapabilities{Attach: false}},
			{ID: brokerID, Capabilities: &hubclient.BrokerCapabilities{Attach: true}},
		}, false)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	err = attachViaHub(hubCtx, agentName)
	require.Error(t, err, "expected a WS dial error — the gate should have been cleared")
	assert.NotContains(t, err.Error(), "attach is not supported")
	assert.True(t, *dialed, "the target's own attach=true must win over a false decoy")
}

// TestAttachViaHub_PointGETForbidden_ListSucceedsWithoutTarget_ProceedsToDial
// covers a successful LIST response that simply never includes the target
// broker at all — a decoy with attach=true is present, so a match that
// ignored ID entirely (or matched the first/only entry) would read it as
// supported for the wrong reason. The target being absent from LIST is
// still "unreadable" (nothing to read FOR THIS agent's broker, whatever the
// reason), so attachViaHub must proceed to dial rather than refuse
// client-side. Reaching this path through attachViaHub (rather than calling
// findRuntimeBrokerByIDViaList directly) is what pins the caller's own
// broker == nil check in attachSupportedByBroker: without it,
// attachSupportedFromBrokerRecord would be called with a nil broker and
// panic.
func TestAttachViaHub_PointGETForbidden_ListSucceedsWithoutTarget_ProceedsToDial(t *testing.T) {
	clearAppTokenSources(t)
	attachTransportFakeAllowsDial(t)

	const (
		projectID = "proj-gate-absent"
		agentName = "gate-agent-absent"
		agentID   = "agent-uuid-gate-absent"
		brokerID  = "gate-broker-absent-target"
		decoyID   = "gate-broker-absent-decoy"
	)
	srv, dialed := newAttachGateFallbackServer(t, projectID, agentName, agentID, brokerID,
		[]hubclient.RuntimeBroker{
			{ID: decoyID, Capabilities: &hubclient.BrokerCapabilities{Attach: true}},
		}, false)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	err = attachViaHub(hubCtx, agentName)
	require.Error(t, err, "expected a WS dial error — the gate must not have refused pre-dial")
	assert.NotContains(t, err.Error(), "attach is not supported")
	assert.NotContains(t, err.Error(), "cannot determine whether this agent's runtime supports attach")
	assert.True(t, *dialed, "the PTY WebSocket must be dialed when the target is absent from a successful LIST — the broker's own gate is authoritative")
}

// TestFindRuntimeBrokerByIDViaList_PagesToSecondPage_SendsCursor pins that
// the LIST fallback actually follows pagination: the target broker isn't on
// the first page, so a second request must be made, and it must carry the
// cursor the first page returned rather than an empty one.
func TestFindRuntimeBrokerByIDViaList_PagesToSecondPage_SendsCursor(t *testing.T) {
	const (
		brokerID = "paged-broker-target"
		decoyID  = "paged-broker-decoy"
	)
	var requests []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-brokers" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests = append(requests, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"brokers":    []hubclient.RuntimeBroker{{ID: decoyID}},
				"nextCursor": "page-2-cursor",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"brokers": []hubclient.RuntimeBroker{{ID: brokerID, Capabilities: &hubclient.BrokerCapabilities{Attach: false}}},
		})
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL}

	broker, err := findRuntimeBrokerByIDViaList(context.Background(), hubCtx, brokerID)
	require.NoError(t, err)
	require.NotNil(t, broker)
	assert.Equal(t, brokerID, broker.ID)

	require.Len(t, requests, 2, "expected exactly two LIST requests: one per page")
	assert.Empty(t, requests[0].Get("cursor"), "the first request must not carry a cursor")
	assert.Equal(t, "page-2-cursor", requests[1].Get("cursor"),
		"the second request must carry the first page's nextCursor")
}

// TestFindRuntimeBrokerByIDViaList_SendsProjectID pins that the LIST
// fallback scopes its request to hubCtx.ProjectID when known, rather than
// listing every broker the caller can see.
func TestFindRuntimeBrokerByIDViaList_SendsProjectID(t *testing.T) {
	const (
		brokerID  = "scoped-broker"
		projectID = "proj-scoped-1"
	)
	var gotProjectID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProjectID = r.URL.Query().Get("projectId")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"brokers": []hubclient.RuntimeBroker{{ID: brokerID}},
		})
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	_, err = findRuntimeBrokerByIDViaList(context.Background(), hubCtx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, projectID, gotProjectID)
}

// TestFindRuntimeBrokerByIDViaList_RepeatedCursor_StopsAfterTwoRequests pins
// the repeated-cursor guard directly: every page carries the same
// nextCursor and never contains the target. The first request has no
// cursor at all; the second carries the repeated cursor and is where the
// guard must fire, stopping the loop rather than requesting a third page
// forever.
func TestFindRuntimeBrokerByIDViaList_RepeatedCursor_StopsAfterTwoRequests(t *testing.T) {
	const brokerID = "cursor-loop-target"
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"brokers":    []hubclient.RuntimeBroker{{ID: "cursor-loop-decoy"}},
			"nextCursor": "same",
		})
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL}

	broker, err := findRuntimeBrokerByIDViaList(context.Background(), hubCtx, brokerID)
	require.NoError(t, err)
	assert.Nil(t, broker)
	assert.Equal(t, 2, requestCount, "must stop once a cursor repeats rather than requesting forever")
}

// TestFindRuntimeBrokerByIDViaList_FreshCursorEveryPage_StopsAtPageCap pins
// the page-count cap directly: the server hands out a distinct, never-seen
// cursor on every page (so the repeated-cursor guard never fires) and never
// contains the target. The loop must still stop, at exactly
// findRuntimeBrokerListMaxPages requests, rather than following the
// endlessly-advancing cursor forever.
func TestFindRuntimeBrokerByIDViaList_FreshCursorEveryPage_StopsAtPageCap(t *testing.T) {
	const brokerID = "cursor-advance-target"
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"brokers":    []hubclient.RuntimeBroker{{ID: "cursor-advance-decoy"}},
			"nextCursor": fmt.Sprintf("cursor-%d", requestCount),
		})
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL}

	broker, err := findRuntimeBrokerByIDViaList(context.Background(), hubCtx, brokerID)
	require.NoError(t, err)
	assert.Nil(t, broker)
	assert.Equal(t, findRuntimeBrokerListMaxPages, requestCount, "must stop at the page cap rather than following an endlessly-advancing cursor")
}

// TestResolveAttachTransport_PlainMode verifies that resolveAttachTransport returns
// a nil TokenSource when no transport auth is configured (plain / dev / local hub).
// This is the base case; the plain-mode invariant requires an app token in this case.
func TestResolveAttachTransport_PlainMode(t *testing.T) {
	// Ensure all transport auth env vars are unset.
	t.Setenv("SCION_TRANSPORT_TOKEN", "")
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", "")
	t.Setenv("SCION_TRANSPORT_AUDIENCE", "")
	t.Setenv("SCION_HUB_OIDC_AUDIENCE", "")
	t.Setenv("SCION_METADATA_MODE", "")

	// Override the GCE-detection function so we don't depend on the test host
	// being on GCP.
	origIsOnGCE := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = func() bool { return false }
	defer func() { transportauth.IsOnGCEFunc = origIsOnGCE }()

	// Use a temp dir with no settings.yaml to simulate a plain project.
	tmpDir := t.TempDir()
	origProjectPath := projectPath
	projectPath = tmpDir
	defer func() { projectPath = origProjectPath }()

	src, mode, err := resolveAttachTransport()

	require.NoError(t, err, "plain mode should not error")
	assert.Nil(t, src, "plain mode should return nil TokenSource")
	assert.Equal(t, transportauth.HeaderAuthorization, mode)
}

// TestResolveAttachTransport_IAPMode verifies that resolveAttachTransport returns
// a non-nil TokenSource when transport auth is configured via SCION_TRANSPORT_TOKEN.
// This simulates the hub-injected IAP token present inside an agent container.
func TestResolveAttachTransport_IAPMode(t *testing.T) {
	// A minimal three-part JWT-shaped value; ParseTokenExpiry falls back to
	// DefaultTTL on any parse error, so we don't need a valid signature.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", "")
	t.Setenv("SCION_TRANSPORT_TOKEN", "header.payload.sig")
	t.Setenv("SCION_TRANSPORT_MODE", "iap")

	src, mode, err := resolveAttachTransport()

	require.NoError(t, err, "IAP mode should not error")
	require.NotNil(t, src, "IAP mode should return a non-nil TokenSource")
	assert.Equal(t, transportauth.HeaderProxyAuthorization, mode,
		"iap transport mode should yield HeaderProxyAuthorization")
}

// TestAttachViaHub_PlainMode_EmptyToken_RequiresAppToken is a regression test for
// the plain-mode invariant: when no transport auth is configured, an empty app
// token must still return the "no access token found for Hub" error. The fix
// must not relax this requirement for the plain case.
func TestAttachViaHub_PlainMode_EmptyToken_RequiresAppToken(t *testing.T) {
	clearAppTokenSources(t)

	// Stub resolveAttachTransportFn to return nil (plain mode — no transport auth).
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-plain-123"
		agentName = "test-agent"
		agentID   = "agent-uuid-plain"
	)

	srv := newAttachMockHubServer(t, projectID, agentName, agentID, "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
	}

	err = attachViaHub(hubCtx, agentName)

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no access token found for Hub"),
		"plain mode with empty token should return 'no access token found for Hub', got: %v", err)
}

// TestAttachViaHub_IAPMode_EmptyToken_PassesGate is the primary regression test for
// issue #851: scion attach fails under IAP proxy-auth. When a transport source is
// present (IAP mode), an empty application-level token must no longer abort the
// attach attempt. The function should proceed past the token gate and reach the
// WebSocket dial stage (which will fail with a transport error, not a token error).
func TestAttachViaHub_IAPMode_EmptyToken_PassesGate(t *testing.T) {
	clearAppTokenSources(t)

	// Stub resolveAttachTransportFn to return a fake IAP transport source.
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return &fakeTransportSource{
			token:  "fake-oidc-token",
			expiry: time.Now().Add(1 * time.Hour),
		}, transportauth.HeaderProxyAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-iap-456"
		agentName = "iap-agent"
		agentID   = "agent-uuid-iap"
	)

	srv := newAttachMockHubServer(t, projectID, agentName, agentID, "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
	}

	err = attachViaHub(hubCtx, agentName)

	// The function is expected to fail at the WebSocket dial step (the mock HTTP
	// server does not handle WebSocket upgrades) — that confirms the gate was
	// cleared and the connection attempt was reached. Use require.Error so the
	// assert.NotContains below is not silently skipped when err is nil.
	require.Error(t, err, "expected a WS dial error — gate should have been cleared in IAP mode")
	assert.NotContains(t, err.Error(), "no access token found for Hub",
		"IAP mode with transport source should pass the token gate; got: %v", err)
}

// newStartAgentMockHubServer creates a minimal mock Hub server sufficient for
// exercising the attach path of startAgentViaHub() (site 2, the ready: label).
// It handles the suspend-check GET, project GET (git-remote display), agent
// CREATE POST, and the polling GET — all of which are reached before the token
// gate in the non-workspace-upload code path. The polling GET reports the
// given agentRuntime (use "" for a normal non-managed, non-noattach agent).
func newStartAgentMockHubServer(t *testing.T, projectID, agentName, agentID, agentRuntime string) *httptest.Server {
	t.Helper()
	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	agentsPath := "/api/v1/projects/" + projectID + "/agents"
	projectGetPath := "/api/v1/projects/" + projectID
	brokerPath := "/api/v1/runtime-brokers/" + mockAttachBrokerID

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodGet && r.URL.Path == projectGetPath:
			// Git-remote display: return a project with no GitRemote to suppress output.
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "test"})

		case r.Method == http.MethodGet && r.URL.Path == brokerPath:
			_ = json.NewEncoder(w).Encode(mockAttachBroker(agentRuntime))

		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			// Suspend check (pre-create) and polling (post-create): return running.
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID:              agentID,
				Name:            agentName,
				Phase:           "running",
				Runtime:         agentRuntime,
				RuntimeBrokerID: mockAttachBrokerID,
			})

		case r.Method == http.MethodPost && r.URL.Path == agentsPath:
			// Create: return a minimal response with no UploadURLs and no EnvGather
			// so the workspace-upload branch (site 1) is skipped.
			_ = json.NewEncoder(w).Encode(hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{
					ID:   agentID,
					Name: agentName,
					// Created is zero-value → hubsync watermark update is skipped.
				},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// saveAttachTestState saves the package-level variables that startAgentViaHub
// reads, and returns a function that restores them. It also shortens the
// launch-wait poll interval and fallback budget so a test whose agent never
// reaches running fails within seconds instead of minutes.
func saveAttachTestState() func() {
	origPoll, origFallback := launchPollInterval, launchWaitFallback
	launchPollInterval, launchWaitFallback = 10*time.Millisecond, 5*time.Second
	origAttach := attach
	origTemplate := templateName
	origBranch := branch
	origWorkspace := workspace
	origBroker := runtimeBrokerID
	origHConfig := harnessConfigFlag
	origHAuth := harnessAuthFlag
	origNoNotify := startNoNotify
	origLabels := labelFlags
	return func() {
		attach = origAttach
		templateName = origTemplate
		branch = origBranch
		workspace = origWorkspace
		runtimeBrokerID = origBroker
		harnessConfigFlag = origHConfig
		harnessAuthFlag = origHAuth
		startNoNotify = origNoNotify
		labelFlags = origLabels
		launchPollInterval, launchWaitFallback = origPoll, origFallback
	}
}

// TestStartAgentViaHub_Site2_PlainMode_EmptyToken_RequiresAppToken directly
// calls startAgentViaHub() with attach=true and exercises site 2 (the ready:
// label in the main polling path). In plain mode (nil transport source) with an
// empty app token the function must return "no access token found for Hub",
// confirming the invariant is preserved in the real function path.
//
// Site 1 (workspace-upload path, ~common.go:1013) is not directly exercised
// here because it requires Hub-supplied UploadURLs and a Workspace.FinalizeSyncTo
// response — complexity that exceeds the scope of a unit test. The gate logic at
// site 1 is structurally identical to site 2 and was verified by code review.
func TestStartAgentViaHub_Site2_PlainMode_EmptyToken_RequiresAppToken(t *testing.T) {
	clearAppTokenSources(t)

	restore := saveAttachTestState()
	defer restore()
	attach = true
	templateName = ""
	labelFlags = nil
	runtimeBrokerID = ""
	harnessConfigFlag = ""
	harnessAuthFlag = ""

	// Stub resolveAttachTransportFn to return nil (plain mode — no transport auth).
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-start-plain-789"
		agentName = "start-plain-agent"
		agentID   = "start-plain-uuid"
	)

	srv := newStartAgentMockHubServer(t, projectID, agentName, agentID, "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
		// ProjectPath is empty → workspace scan and hubsync calls are skipped.
	}

	err = startAgentViaHub(nil, hubCtx, agentName, "", false, nil)

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no access token found for Hub"),
		"plain mode with empty token should reach 'no access token found for Hub' in startAgentViaHub site 2; got: %v", err)
}

// TestStartAgentViaHub_Site2_NoAttachAgent_ReturnsExplicitError directly
// calls startAgentViaHub() with attach=true on an agent whose runtime is
// "noattach" and exercises site 2 (the ready: label in the main polling
// path). A noattach agent started with `-a` must fail with the same fixed,
// explicit, non-zero-exit error as `scion attach` itself, and never reach the
// WebSocket dial step — this is the same silent-failure bug on a sibling
// code path.
func TestStartAgentViaHub_Site2_NoAttachAgent_ReturnsExplicitError(t *testing.T) {
	clearAppTokenSources(t)

	restore := saveAttachTestState()
	defer restore()
	attach = true
	templateName = ""
	labelFlags = nil
	runtimeBrokerID = ""
	harnessConfigFlag = ""
	harnessAuthFlag = ""

	const (
		projectID = "proj-start-noattach-321"
		agentName = "start-noattach-agent"
		agentID   = "start-noattach-uuid"
	)

	srv := newStartAgentMockHubServer(t, projectID, agentName, agentID, "noattach")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
		// ProjectPath is empty → workspace scan and hubsync calls are skipped.
	}

	err = startAgentViaHub(nil, hubCtx, agentName, "", false, nil)

	require.Error(t, err)
	const wantMsg = "attach is not supported for agents on the noattach runtime"
	assert.Equal(t, wantMsg, err.Error(), "noattach start -a must fail with the fixed message, got: %v", err)
}

// TestStartAgentViaHub_Site1_NoAttachAgent_ReturnsExplicitError mirrors
// TestStartAgentViaHub_Site2_NoAttachAgent_ReturnsExplicitError, but drives
// startAgentViaHub() through the workspace-upload branch (site 1, ~common.go
// :1104) instead of the ready: label (site 2): a non-git project directory
// with one file makes startAgentViaHub collect workspaceFiles, the mock
// Create response returns a matching UploadURLs entry so the upload+finalize
// path runs, and the post-finalize polling GET reports Runtime: "noattach".
// The same fixed, explicit, non-zero-exit error must be returned, and the
// WebSocket dial step must never be reached.
func TestStartAgentViaHub_Site1_NoAttachAgent_ReturnsExplicitError(t *testing.T) {
	clearAppTokenSources(t)

	restore := saveAttachTestState()
	defer restore()
	attach = true
	templateName = ""
	labelFlags = nil
	runtimeBrokerID = ""
	harnessConfigFlag = ""
	harnessAuthFlag = ""

	// A non-git project directory with one file, so the workspace-scan in
	// startAgentViaHub collects it into workspaceFiles — the precondition for
	// the create request to carry WorkspaceFiles and for a Hub response with
	// UploadURLs to route into the workspace-upload branch.
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "hello.txt"), []byte("hi"), 0644))
	scionDir := filepath.Join(projectDir, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0755))

	const (
		projectID = "proj-start-noattach-site1"
		agentName = "start-noattach-site1-agent"
		agentID   = "start-noattach-site1-uuid"
	)

	// A file:// upload URL short-circuits transfer.Client.UploadFile into a
	// local file write, so the test doesn't need an HTTP PUT handler too.
	uploadDest := filepath.Join(tmpDir, "uploaded", "hello.txt")

	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	agentsPath := "/api/v1/projects/" + projectID + "/agents"
	projectGetPath := "/api/v1/projects/" + projectID
	brokerPath := "/api/v1/runtime-brokers/" + mockAttachBrokerID
	// startAgentViaHub finalizes against resp.Agent.Slug, falling back to
	// agentName when Slug is unset — the mock Create response below leaves
	// Slug unset, so the finalize call lands on agentName.
	finalizePath := "/api/v1/agents/" + agentName + "/workspace/sync-to/finalize"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodGet && r.URL.Path == projectGetPath:
			// Git-remote display: return a project with no GitRemote to suppress output.
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "test"})

		case r.Method == http.MethodPost && r.URL.Path == agentsPath:
			// Create: return UploadURLs for the one collected file, routing
			// startAgentViaHub into the workspace-upload branch (site 1).
			_ = json.NewEncoder(w).Encode(hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{ID: agentID, Name: agentName},
				UploadURLs: []transfer.UploadURLInfo{
					{Path: "hello.txt", URL: "file://" + uploadDest, Method: "PUT"},
				},
			})

		case r.Method == http.MethodPost && r.URL.Path == finalizePath:
			_ = json.NewEncoder(w).Encode(hubclient.SyncToFinalizeResponse{Applied: true, FilesApplied: 1})

		case r.Method == http.MethodGet && r.URL.Path == brokerPath:
			_ = json.NewEncoder(w).Encode(mockAttachBroker("noattach"))

		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			// Suspend check (pre-create) and post-finalize polling both hit
			// this path; report running with a noattach runtime so the
			// polling loop's running-phase branch is exercised.
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID:              agentID,
				Name:            agentName,
				Phase:           "running",
				Runtime:         "noattach",
				RuntimeBrokerID: mockAttachBrokerID,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:      client,
		Endpoint:    srv.URL,
		ProjectID:   projectID,
		ProjectPath: scionDir,
	}

	err = startAgentViaHub(nil, hubCtx, agentName, "", false, nil)

	require.Error(t, err)
	const wantMsg = "attach is not supported for agents on the noattach runtime"
	assert.Equal(t, wantMsg, err.Error(), "noattach start -a via the workspace-upload path must fail with the fixed message, got: %v", err)
}

// TestAttachViaHub_NoAttachAgent_ReturnsExplicitError verifies that attach on
// an agent whose runtime is "noattach" fails with a fixed, explicit,
// non-zero-exit error and never reaches the WebSocket dial step. It also
// checks that the error text carries no infra-specific details (only the
// fixed message is present).
func TestAttachViaHub_NoAttachAgent_ReturnsExplicitError(t *testing.T) {
	clearAppTokenSources(t)

	const (
		projectID = "proj-noattach-123"
		agentName = "noattach-agent"
		agentID   = "agent-uuid-noattach"
	)

	srv := newAttachMockHubServer(t, projectID, agentName, agentID, "noattach")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
	}

	err = attachViaHub(hubCtx, agentName)

	require.Error(t, err)
	const wantMsg = "attach is not supported for agents on the noattach runtime"
	assert.Equal(t, wantMsg, err.Error(), "noattach attach must fail with the fixed message, got: %v", err)

	// No infra-specific details (namespaces, actor names, node/pod
	// names, URLs, image refs) may leak into the user-facing message.
	for _, leak := range []string{"namespace", "://", projectID, agentID, srv.URL} {
		assert.NotContains(t, err.Error(), leak, "error message must not leak infra detail %q", leak)
	}
}

// TestAttachViaHub_DockerAgent_UnaffectedByNoAttachCheck verifies that an
// agent with a non-noattach, non-managed runtime (e.g. "docker") is
// unaffected by the noattach guard and follows the existing path — reaching
// the WebSocket dial step and failing there (the mock server doesn't
// implement a WS upgrade), exactly as it did before the noattach check was
// added.
func TestAttachViaHub_DockerAgent_UnaffectedByNoAttachCheck(t *testing.T) {
	clearAppTokenSources(t)

	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return &fakeTransportSource{
			token:  "fake-oidc-token",
			expiry: time.Now().Add(1 * time.Hour),
		}, transportauth.HeaderProxyAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-docker-123"
		agentName = "docker-agent"
		agentID   = "agent-uuid-docker"
	)

	srv := newAttachMockHubServer(t, projectID, agentName, agentID, "docker")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
	}

	err = attachViaHub(hubCtx, agentName)

	require.Error(t, err, "expected a WS dial error — the docker path must reach the dial step unchanged")
	assert.NotContains(t, err.Error(), "attach is not supported for agents on the noattach runtime",
		"docker agent must not be rejected by the noattach guard, got: %v", err)
	// Pin that the phase and token gates were actually passed and the
	// WebSocket dial itself was reached (pkg/wsclient/pty.go:123/125), not
	// some other, earlier failure that happens to also be non-nil.
	assert.Contains(t, err.Error(), "connection failed",
		"docker agent should fail at the WS dial step, got: %v", err)
}

// TestStartAgentViaHub_Site2_IAPMode_EmptyToken_PassesGate directly calls
// startAgentViaHub() with attach=true and exercises site 2 (the ready: label in
// the main polling path). With a transport source present (IAP mode) and an empty
// app token the function must NOT return "no access token found for Hub" — it
// should clear the gate and fail at the WebSocket dial, confirming that issue #851
// is fixed in the startAgentViaHub() path too.
//
// Site 1 (workspace-upload path) is not directly exercised here — see the comment
// on TestStartAgentViaHub_Site2_PlainMode_EmptyToken_RequiresAppToken for why.
func TestStartAgentViaHub_Site2_IAPMode_EmptyToken_PassesGate(t *testing.T) {
	clearAppTokenSources(t)

	restore := saveAttachTestState()
	defer restore()
	attach = true
	templateName = ""
	labelFlags = nil
	runtimeBrokerID = ""
	harnessConfigFlag = ""
	harnessAuthFlag = ""

	// Stub resolveAttachTransportFn to return a fake IAP transport source.
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return &fakeTransportSource{
			token:  "fake-oidc-token",
			expiry: time.Now().Add(1 * time.Hour),
		}, transportauth.HeaderProxyAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-start-iap-abc"
		agentName = "start-iap-agent"
		agentID   = "start-iap-uuid"
	)

	srv := newStartAgentMockHubServer(t, projectID, agentName, agentID, "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
		// ProjectPath is empty → workspace scan and hubsync calls are skipped.
	}

	err = startAgentViaHub(nil, hubCtx, agentName, "", false, nil)

	// The function is expected to fail at the WebSocket dial step (the mock HTTP
	// server does not handle WebSocket upgrades) — that confirms the gate was
	// cleared and the connection attempt was reached. Use require.Error so the
	// assert.NotContains below is not silently skipped when err is nil.
	require.Error(t, err, "expected a WS dial error — gate should have been cleared in IAP mode")
	assert.NotContains(t, err.Error(), "no access token found for Hub",
		"IAP mode with transport source should pass the token gate in startAgentViaHub site 2; got: %v", err)
}

// TestAttachErrorWithUATHint pins ptone/scion#2122's 403 hint: it fires only
// for a UAT credential (scion_pat_ prefix) on an actual 403, and never
// changes the underlying error or wraps it for any other status/credential
// combination.
func TestAttachErrorWithUATHint(t *testing.T) {
	forbidden := fmt.Errorf("connection failed with status %d: forbidden", http.StatusForbidden)
	notFound := fmt.Errorf("connection failed with status %d: agent not found", http.StatusNotFound)

	t.Run("UAT credential with 403 gets the hint", func(t *testing.T) {
		err := attachErrorWithUATHint(forbidden, "scion_pat_abc123")
		require.Error(t, err)
		assert.ErrorIs(t, err, forbidden)
		assert.Contains(t, err.Error(), "agent:attach")
		assert.Contains(t, err.Error(), "hub token scopes")
	})

	t.Run("session credential with 403 is unchanged", func(t *testing.T) {
		err := attachErrorWithUATHint(forbidden, "some-session-token")
		assert.Equal(t, forbidden, err)
	})

	t.Run("UAT credential with a non-403 error is unchanged", func(t *testing.T) {
		err := attachErrorWithUATHint(notFound, "scion_pat_abc123")
		assert.Equal(t, notFound, err)
	})

	t.Run("nil error stays nil", func(t *testing.T) {
		assert.NoError(t, attachErrorWithUATHint(nil, "scion_pat_abc123"))
	})
}
