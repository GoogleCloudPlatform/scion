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
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertTestAgentCredential records an active credential for agentID under
// jti, the way a production mint records it. Tests use it to seed a
// credential outside the dispatch path under test (e.g. a sibling agent's
// credential that must survive the test's revoke call).
func insertTestAgentCredential(t *testing.T, s store.AgentCredentialStore, agentID, projectID, jti string) {
	t.Helper()
	now := time.Now()
	cred := &store.AgentCredential{
		AgentID:      agentID,
		ProjectID:    projectID,
		TokenJTIHash: hashJTI(jti),
		IssuedAt:     now,
		ExpiresAt:    now.Add(10 * time.Hour),
	}
	if err := s.CreateAgentCredential(context.Background(), cred); err != nil {
		t.Fatalf("failed to insert test agent credential: %v", err)
	}
}

// getTestAgentCredential looks a credential back up by its plaintext jti
// (hashing it the same way the production code does) so a test can assert
// on RevokedAt/RevokeReason after exercising a dispatch or handler path.
func getTestAgentCredential(t *testing.T, s store.AgentCredentialStore, jti string) *store.AgentCredential {
	t.Helper()
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(jti))
	if err != nil {
		t.Fatalf("failed to look up test agent credential for jti %q: %v", jti, err)
	}
	return cred
}

// fakeMintingTokenGenerator implements AgentTokenGenerator for dispatcher
// tests. SignAgentToken mints a fake token and returns its credential for
// the caller to record, as production's AgentTokenService does, and
// remembers every jti it issued (in call order) so a test can look up what
// it minted afterward. GenerateAgentToken records the credential itself.
// failWith fails AuthorizeAgentToken and GenerateAgentToken.
type fakeMintingTokenGenerator struct {
	store    store.AgentCredentialStore
	jtis     []string
	failWith error
}

func (f *fakeMintingTokenGenerator) GenerateAgentToken(agentID, projectID string, ancestry []string, role AgentRole, additionalScopes []AgentTokenScope) (string, error) {
	if f.failWith != nil {
		return "", f.failWith
	}
	token, cred, err := f.SignAgentToken(AgentTokenGrant{AgentID: agentID, ProjectID: projectID, Ancestry: ancestry}, "")
	if err != nil {
		return "", err
	}
	if err := f.store.CreateAgentCredential(context.Background(), cred); err != nil {
		return "", err
	}
	return token, nil
}

// AuthorizeAgentToken is the entry point every dispatcher mint site calls.
func (f *fakeMintingTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	if f.failWith != nil {
		return AgentTokenGrant{}, f.failWith
	}
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID, Ancestry: agent.Ancestry}, nil
}

func (f *fakeMintingTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	jti := fmt.Sprintf("test-jti-%s-%d", grant.AgentID, len(f.jtis)+1)
	f.jtis = append(f.jtis, jti)
	now := time.Now()
	cred := &store.AgentCredential{
		AgentID:      grant.AgentID,
		ProjectID:    grant.ProjectID,
		TokenJTIHash: hashJTI(jti),
		RunID:        runID,
		IssuedAt:     now,
		ExpiresAt:    now.Add(10 * time.Hour),
	}
	return "fake-agent-jwt-" + jti, cred, nil
}

// lastJTI returns the most recently minted jti, for a test that only expects
// a single GenerateAgentToken call.
func (f *fakeMintingTokenGenerator) lastJTI() string {
	return f.jtis[len(f.jtis)-1]
}

// revokeFailingCredentialStore wraps a real store.Store and makes
// RevokeAgentCredentialsByAgent always fail, so a test can verify a
// revoke-store error is logged and swallowed rather than masking or
// replacing the original dispatch/handler error.
type revokeFailingCredentialStore struct {
	store.Store
	failWith error
}

func (s *revokeFailingCredentialStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, revokedBy, reason string) (int, error) {
	return 0, s.failWith
}

// TestRevokeAgentCredentialsBestEffort_CancelledParentContextStillRevokes
// covers revokeAgentCredentialsBestEffort detaching from the caller's
// context before calling the store, because a dispatch or handler
// failure path may already have an expired or cancelled context by the time
// it decides to revoke. A cancelled parent must not prevent the revoke.
func TestRevokeAgentCredentialsBestEffort_CancelledParentContextStillRevokes(t *testing.T) {
	s := createTestStore(t)

	project := &store.Project{ID: tid("revoke-besteffort-cancelled-project"), Slug: "revoke-besteffort-cancelled-project", Name: "Revoke Cancelled Ctx Project"}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	jti := "revoke-besteffort-cancelled-jti"
	insertTestAgentCredential(t, s, tid("revoke-besteffort-cancelled-agent"), project.ID, jti)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	revokeAgentCredentialsBestEffort(cancelledCtx, s, tid("revoke-besteffort-cancelled-agent"), agentCredentialRevokeReasonStartFailed)

	cred := getTestAgentCredential(t, s, jti)
	if cred.RevokedAt == nil {
		t.Fatal("expected the credential to be revoked even though the caller's context was already cancelled")
	}
	if cred.RevokeReason == nil || *cred.RevokeReason != agentCredentialRevokeReasonStartFailed {
		t.Fatalf("expected revoke reason %q, got %v", agentCredentialRevokeReasonStartFailed, cred.RevokeReason)
	}
}

// TestRevokeAgentCredentialsBestEffort_NilStore covers the best-effort
// helper being handed no credential store: it must return without
// panicking, keeping its contract of never disrupting the caller's error
// path.
func TestRevokeAgentCredentialsBestEffort_NilStore(t *testing.T) {
	revokeAgentCredentialsBestEffort(context.Background(), nil, tid("revoke-besteffort-nil-store-agent"), agentCredentialRevokeReasonCreateFailed)
}

// TestIsConfirmedNonRunningPhase is a direct table test over every
// state.Phase value, plus an empty phase and an unrecognized one, so a
// change to the allow-list is caught here even if it happens not to be
// exercised by a DispatchAgentStart-level test.
func TestIsConfirmedNonRunningPhase(t *testing.T) {
	tests := []struct {
		phase string
		want  bool
	}{
		{"", true},
		{string(state.PhaseCreated), true},
		{string(state.PhaseProvisioning), true},
		{string(state.PhaseCloning), false},
		{string(state.PhaseStarting), false},
		{string(state.PhaseRunning), false},
		{string(state.PhaseSuspended), true},
		{string(state.PhaseStopping), false},
		{string(state.PhaseStopped), true},
		{string(state.PhaseError), true},
		{"some-unknown-phase", false},
	}
	for _, tt := range tests {
		name := tt.phase
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			if got := isConfirmedNonRunningPhase(tt.phase); got != tt.want {
				t.Errorf("isConfirmedNonRunningPhase(%q) = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

// fakeTimeoutError is a minimal net.Error stand-in for a timed-out
// operation, used to build a realistic *url.Error the way the standard
// library's HTTP client would.
type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

// TestIsConfirmedStartNotActedOnError is a direct table test over the
// error classifier DispatchAgentStart's start-failure revoke relies on.
// Every "true" case here is matched by a sentinel (errStartBrokerNotConnected,
// errStartRequestNotSent) or a type (*brokerStatusError, *net.OpError with
// Op "dial"): deleting any one of those checks makes its case fail. Every
// "false" case including the error-text lookalikes near the end of the
// table is an error that merely resembles a known-safe one in its text — an
// OS-level "not connected" from a read or write that happened after the
// request was sent, or a decoded-body snippet that happens to echo matching
// words — so loosening any check from a sentinel/type match back to a text
// match makes one of these slip to true.
func TestIsConfirmedStartNotActedOnError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"broker status error, 4xx", &brokerStatusError{StatusCode: 404, Body: "not found"}, true},
		{"broker status error, 500 with runtime_error envelope", &brokerStatusError{StatusCode: 500, Body: `{"error":{"code":"runtime_error","message":"boom"}}`}, true},
		{"broker status error, 500 with non-JSON body", &brokerStatusError{StatusCode: 500, Body: "internal server error"}, false},
		{"broker status error, 504 with HTML body", &brokerStatusError{StatusCode: 504, Body: "<html><body>504 Gateway Time-out</body></html>"}, false},
		{"dial error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"manager not-connected sentinel", fmt.Errorf("broker %s not connected: %w", "b1", errStartBrokerNotConnected), true},
		{"control channel client not-connected sentinel", fmt.Errorf("broker %s not connected via control channel: %w", "b1", errStartBrokerNotConnected), true},
		{"no http endpoint, not-sent sentinel", fmt.Errorf("runtime broker %q has no HTTP endpoint configured: %w", "b1", errStartRequestNotSent), true},
		{"http marshal failure, not-sent sentinel", fmt.Errorf("failed to marshal request: %w (%w)", errors.New("boom"), errStartRequestNotSent), true},
		{"http create request failure, not-sent sentinel", fmt.Errorf("failed to create request: %w (%w)", errors.New("boom"), errStartRequestNotSent), true},
		{"http sign failure, not-sent sentinel", fmt.Errorf("failed to sign request: %w (%w)", errors.New("boom"), errStartRequestNotSent), true},
		{"control channel build-for-signing failure, not-sent sentinel", fmt.Errorf("failed to build control channel request for signing: %w (%w)", errors.New("boom"), errStartRequestNotSent), true},
		{"control channel sign failure, not-sent sentinel", fmt.Errorf("failed to sign control channel request: %w (%w)", errors.New("boom"), errStartRequestNotSent), true},
		{"control channel oversized body, not-sent sentinel", checkBodySize("POST", "/api/v1/agents/x/start", make([]byte, maxControlChannelBodySize+1)), true},
		{"http send timeout (url.Error)", &url.Error{Op: "Post", URL: "http://broker", Err: fakeTimeoutError{}}, false},
		{"wrapped deadline exceeded", fmt.Errorf("failed to send request: %w", context.DeadlineExceeded), false},
		{"control channel request timeout", fmt.Errorf("control channel request failed: %w", fmt.Errorf("request timeout after %v", 120*time.Second)), false},
		{"control channel connection closed", fmt.Errorf("control channel request failed: %w", errors.New("connection closed")), false},
		{"control channel context cancelled", fmt.Errorf("control channel request failed: %w", context.Canceled), false},
		{"http decode failure on a 2xx response", fmt.Errorf("failed to decode response: %w (body=%q)", errors.New("invalid character"), "not-json"), false},
		{"plain unclassified error", errors.New("broker hiccup"), false},
		// A typed-nil *brokerStatusError wrapped in a non-nil error makes
		// errors.As yield a nil pointer; that must read as unconfirmed.
		{"wrapped typed-nil broker status error", fmt.Errorf("start failed: %w", (*brokerStatusError)(nil)), false},
		// A read failure that happens after the request was already sent
		// must not be confused with errStartBrokerNotConnected just because
		// the OS error text also contains the words "not connected"
		// (syscall.ENOTCONN's message is "transport endpoint is not
		// connected" on Linux). Built from a real *net.OpError wrapping the
		// real syscall error, not a hand-copied string.
		{"post-send read error mentioning not connected", fmt.Errorf("failed to send request: %w", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ENOTCONN}), false},
		// Same confusion via a write failure surfaced through the control
		// channel's own request-failed wrapper.
		{"control-channel write error mentioning not connected", fmt.Errorf("control channel request failed: %w", fmt.Errorf("failed to send request: %w", &net.OpError{Op: "write", Net: "tcp", Err: syscall.ENOTCONN})), false},
		// decodeResponseWithSnippet echoes up to 256 bytes of the response
		// body into the error message; that echoed text must not be able to
		// trigger a match either.
		{"decoded response-body snippet mentioning not connected", fmt.Errorf("failed to decode response: %w (body=%q)", errors.New("invalid character"), "upstream says: not connected"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConfirmedStartNotActedOnError(tt.err); got != tt.want {
				t.Errorf("isConfirmedStartNotActedOnError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsConfirmedStartNotActedOnError_RealNotConnectedSource goes through
// the actual source of a not-connected error — ControlChannelManager.
// TunnelRequest against a broker with no registered connection — rather
// than hand-building one, so a change to that call site's wrapping (e.g.
// dropping the errStartBrokerNotConnected wrap, or changing the message)
// is caught here even if the table test above still hand-constructs a
// passing case.
func TestIsConfirmedStartNotActedOnError_RealNotConnectedSource(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	req := &wsprotocol.RequestEnvelope{Method: "POST", Path: "/api/v1/agents/x/start"}

	_, err := mgr.TunnelRequest(context.Background(), "no-such-broker", req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errStartBrokerNotConnected), "expected the real TunnelRequest not-connected error to wrap errStartBrokerNotConnected, got: %v", err)
	assert.True(t, isConfirmedStartNotActedOnError(err), "expected the real TunnelRequest not-connected error to be classified confirmed-safe")
}

// TestIsConfirmedStartNotActedOnError_RealControlChannelNotConnectedSource
// goes through ControlChannelBrokerClient.StartAgent itself (rather than
// the manager-level TunnelRequest above) against a manager with no
// registered connections, so the doRequest-level not-connected check gets
// the same real-source coverage. Dropping the errStartBrokerNotConnected
// wrap at that call site fails this test.
func TestIsConfirmedStartNotActedOnError_RealControlChannelNotConnectedSource(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewControlChannelBrokerClient(mgr, nil, false)

	_, err := c.StartAgent(context.Background(), "no-such-broker", "", "agent-1", "project-1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errStartBrokerNotConnected), "expected the real ControlChannelBrokerClient.StartAgent not-connected error to wrap errStartBrokerNotConnected, got: %v", err)
	assert.True(t, isConfirmedStartNotActedOnError(err), "expected the real not-connected error to be classified confirmed-safe")
}

// TestIsConfirmedStartNotActedOnError_RealHTTPNoEndpointSource goes through
// brokerHTTPTransport.StartAgent itself against an empty broker endpoint,
// so the doRequest-level no-endpoint check gets the same real-source
// coverage. Dropping the errStartRequestNotSent wrap at that call site
// fails this test.
func TestIsConfirmedStartNotActedOnError_RealHTTPNoEndpointSource(t *testing.T) {
	transport := newBrokerHTTPTransport(false, nil)

	_, err := transport.StartAgent(context.Background(), "broker-1", "", "agent-1", "project-1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errStartRequestNotSent), "expected the real brokerHTTPTransport.StartAgent no-endpoint error to wrap errStartRequestNotSent, got: %v", err)
	assert.True(t, isConfirmedStartNotActedOnError(err), "expected the real no-endpoint error to be classified confirmed-safe")
}

// TestIsConfirmedBrokerRejection is a direct table test over the
// status-code/body nuance isConfirmedStartNotActedOnError relies on for a
// *brokerStatusError: below 500 is always the broker's own response; 500
// counts only with a parseable runtimebroker error envelope; everything
// else (501 and up, or a 500 with an unparseable body) is left unconfirmed,
// since a reverse proxy or load balancer can return those after it has
// already forwarded the request, so they say nothing about whether the
// broker acted on it.
func TestIsConfirmedBrokerRejection(t *testing.T) {
	tests := []struct {
		name string
		err  *brokerStatusError
		want bool
	}{
		{"400 validation error", &brokerStatusError{StatusCode: 400, Body: `{"error":{"code":"validation_error","message":"bad"}}`}, true},
		{"404 not found, no body", &brokerStatusError{StatusCode: 404, Body: ""}, true},
		{"500 with runtime_error envelope", &brokerStatusError{StatusCode: 500, Body: `{"error":{"code":"runtime_error","message":"boom"}}`}, true},
		{"500 with plain text body", &brokerStatusError{StatusCode: 500, Body: "internal server error"}, false},
		{"500 with empty body", &brokerStatusError{StatusCode: 500, Body: ""}, false},
		{"502 bad gateway, no body", &brokerStatusError{StatusCode: 502, Body: ""}, false},
		{"503 with a valid envelope", &brokerStatusError{StatusCode: 503, Body: `{"error":{"code":"runtime_unavailable","message":"busy"}}`}, false},
		{"504 with an HTML body", &brokerStatusError{StatusCode: 504, Body: "<html><body>504 Gateway Time-out</body></html>"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConfirmedBrokerRejection(tt.err); got != tt.want {
				t.Errorf("isConfirmedBrokerRejection(%+v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
