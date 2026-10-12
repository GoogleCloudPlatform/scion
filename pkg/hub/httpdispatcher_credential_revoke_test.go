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

// This file covers ptone/scion#1956: the Hub must revoke the agent
// credential it minted for a create or launch dispatch that then fails,
// mirroring the delete and suspend paths (handlers_agents_core.go,
// handlers_agent_lifecycle.go). See agent_credential_revoke.go for the
// shared helper and the DispatchAgentStart LaunchError guard it documents.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBrokerForRevoke(t *testing.T, s store.Store, idName string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:       tid(idName),
		Name:     idName,
		Slug:     idName,
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_RevokesCredentialOnFailure(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-create-fail-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker boom")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-create-fail-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-create-fail-project"),
		RuntimeBrokerID: broker.ID,
	}

	_, err := dispatcher.DispatchAgentCreate(ctx, agent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker boom", "the original dispatch error must not be masked by the revoke")

	require.Len(t, gen.jtis, 1, "expected exactly one credential to have been minted")
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	require.NotNil(t, cred.RevokedAt, "credential must be revoked after a create failure")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
}

// TestHTTPAgentDispatcher_DispatchAgentCreate_NoTokenGeneratedSkipsRevoke
// covers the create path: buildCreateRequest tolerates a
// GenerateAgentToken failure and carries on without a credential, so this call
// issued no credential. A later create failure must not revoke-by-agent —
// a credential seeded before this dispatch (e.g. still active from an
// earlier successful create/start) must survive.
func TestHTTPAgentDispatcher_DispatchAgentCreate_NoTokenGeneratedSkipsRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-create-notoken-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker boom")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore, failWith: errors.New("mint failed")}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-create-notoken-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-create-notoken-project"),
		RuntimeBrokerID: broker.ID,
	}
	insertTestAgentCredential(t, memStore, agent.ID, agent.ProjectID, "revoke-create-notoken-preexisting-jti")

	_, err := dispatcher.DispatchAgentCreate(ctx, agent)
	require.Error(t, err)
	assert.Empty(t, gen.jtis, "the generator failed, so no credential should have been minted")

	cred := getTestAgentCredential(t, memStore, "revoke-create-notoken-preexisting-jti")
	assert.Nil(t, cred.RevokedAt, "a create that minted no credential must not revoke the agent's pre-existing one on failure")
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_SuccessDoesNotRevokeCredential(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-create-ok-host")

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-create-ok-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-create-ok-project"),
		RuntimeBrokerID: broker.ID,
	}

	_, createErr := dispatcher.DispatchAgentCreate(ctx, agent)
	require.NoError(t, createErr)

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "a successful create must not revoke the credential it minted")
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_RevokeStoreErrorDoesNotMaskDispatchError(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-create-storeerr-host")
	wrapped := &revokeFailingCredentialStore{Store: memStore, failWith: errors.New("revoke store down")}

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker boom")}
	dispatcher := NewHTTPAgentDispatcherWithClient(wrapped, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-create-storeerr-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-create-storeerr-project"),
		RuntimeBrokerID: broker.ID,
	}

	_, err := dispatcher.DispatchAgentCreate(ctx, agent)
	require.Error(t, err, "a revoke-store failure must not turn a real dispatch failure into success")
	assert.Contains(t, err.Error(), "broker boom", "the original dispatch error must survive a revoke-store error untouched")
}

func TestHTTPAgentDispatcher_DispatchAgentCreateWithGather_RevokesCredentialOnError(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-gather-fail-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker unavailable")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-gather-fail-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-gather-fail-project"),
		RuntimeBrokerID: broker.ID,
	}

	_, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker unavailable")

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	require.NotNil(t, cred.RevokedAt)
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
}

// TestHTTPAgentDispatcher_DispatchAgentCreateWithGather_NeedsMissingDoesNotRevokeAtDispatcherLevel
// pins the split documented in DispatchAgentCreateWithGather's defer: a 202
// "needs more env" response is returned as a value, not an error, so the
// dispatcher itself must not revoke — the caller (handlers_agents_core.go)
// decides whether that is a failure and revokes explicitly when it is.
func TestHTTPAgentDispatcher_DispatchAgentCreateWithGather_NeedsMissingDoesNotRevokeAtDispatcherLevel(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-gather-needs-host")

	mockClient := &mockRuntimeBrokerClient{
		createWithGatherFunc: func(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			return nil, &RemoteEnvRequirementsResponse{Needs: []string{"SOME_VAR"}}, nil
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-gather-needs-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-gather-needs-project"),
		RuntimeBrokerID: broker.ID,
	}

	createRes, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	envReqs := createRes.EnvRequirements()
	require.NoError(t, err)
	require.NotNil(t, envReqs)
	require.Len(t, envReqs.Needs, 1)

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "the dispatcher must not revoke on a non-error needs-still-missing response")
}

func TestHTTPAgentDispatcher_DispatchAgentProvision_RevokesCredentialOnFailure(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-provision-fail-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("provision rejected")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-provision-fail-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-provision-fail-project"),
		RuntimeBrokerID: broker.ID,
	}

	err := dispatcher.DispatchAgentProvision(ctx, agent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provision rejected")

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	require.NotNil(t, cred.RevokedAt)
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
}

func TestHTTPAgentDispatcher_DispatchFinalizeEnv_RevokesCredentialOnStillMissing(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-finalize-fail-host")

	mockClient := &mockRuntimeBrokerClient{
		createWithGatherFunc: func(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			return nil, &RemoteEnvRequirementsResponse{Needs: []string{"STILL_MISSING"}}, nil
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-finalize-fail-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-finalize-fail-project"),
		RuntimeBrokerID: broker.ID,
	}

	_, err := dispatcher.DispatchFinalizeEnv(ctx, agent, nil)
	require.Error(t, err)
	var stillMissing *ErrEnvStillMissing
	require.ErrorAs(t, err, &stillMissing, "the revoke must not change the error type callers switch on")
	assert.Equal(t, []string{"STILL_MISSING"}, stillMissing.Requirements.Needs)

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	require.NotNil(t, cred.RevokedAt)
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
}

// TestHTTPAgentDispatcher_DispatchAgentStart_RevokesCredentialOnFailure
// covers every confirmed non-running phase the start-failure revoke must
// still arm for: an empty phase (no prior phase recorded at all, or never
// set) and explicit allow-listed phases such as "stopped" and "suspended",
// so the positive case is not pinned to the empty-phase value alone. The
// dispatch failure is a *brokerStatusError, the same type the broker
// transport returns for a non-2xx response, so the failure itself is also
// a confirmed-safe one under isConfirmedStartNotActedOnError.
func TestHTTPAgentDispatcher_DispatchAgentStart_RevokesCredentialOnFailure(t *testing.T) {
	for _, phase := range []string{"", string(state.PhaseStopped), string(state.PhaseSuspended)} {
		name := phase
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			memStore := createTestStore(t)
			broker := newTestBrokerForRevoke(t, memStore, "revoke-start-fail-host-"+name)

			mockClient := &mockRuntimeBrokerClient{returnErr: &brokerStatusError{StatusCode: 400, Body: "start rejected"}}
			dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: memStore}
			dispatcher.SetTokenGenerator(gen)

			agent := &store.Agent{
				ID:              tid("revoke-start-fail-agent-" + name),
				Name:            "test-agent",
				Slug:            "test-agent",
				ProjectID:       tid("revoke-start-fail-project-" + name),
				RuntimeBrokerID: broker.ID,
				Phase:           phase,
			}

			err := dispatcher.DispatchAgentStart(ctx, agent, "", false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "start rejected")

			require.Len(t, gen.jtis, 1)
			cred := getTestAgentCredential(t, memStore, gen.lastJTI())
			require.NotNil(t, cred.RevokedAt)
			require.NotNil(t, cred.RevokeReason)
			assert.Equal(t, agentCredentialRevokeReasonStartFailed, *cred.RevokeReason)
		})
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_SuccessDoesNotRevokeCredential(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-ok-host")

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-ok-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-ok-project"),
		RuntimeBrokerID: broker.ID,
	}

	require.NoError(t, dispatcher.DispatchAgentStart(ctx, agent, "", false))

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt)
}

// TestHTTPAgentDispatcher_DispatchAgentStart_UnconfirmedLaunchErrorSkipsRevoke
// covers the unconfirmed-launch case: a resume dispatched at an agent whose
// last launch ended with a value the Hub declared without broker
// confirmation (LaunchErrorLaunchTimeout / LaunchErrorBrokerLost) must not
// revoke-by-agent on a subsequent start failure, because the broker never
// confirmed that agent's prior container — and its still-valid credential —
// is actually gone.
func TestHTTPAgentDispatcher_DispatchAgentStart_UnconfirmedLaunchErrorSkipsRevoke(t *testing.T) {
	for _, launchError := range []string{store.LaunchErrorLaunchTimeout, store.LaunchErrorBrokerLost} {
		t.Run(launchError, func(t *testing.T) {
			ctx := context.Background()
			memStore := createTestStore(t)
			broker := newTestBrokerForRevoke(t, memStore, "revoke-start-unconfirmed-host-"+launchError)

			mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("start rejected")}
			dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: memStore}
			dispatcher.SetTokenGenerator(gen)

			agent := &store.Agent{
				ID:              tid("revoke-start-unconfirmed-agent-" + launchError),
				Name:            "test-agent",
				Slug:            "test-agent",
				ProjectID:       tid("revoke-start-unconfirmed-project-" + launchError),
				RuntimeBrokerID: broker.ID,
				LaunchError:     launchError,
			}

			err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
			require.Error(t, err)

			require.Len(t, gen.jtis, 1)
			cred := getTestAgentCredential(t, memStore, gen.lastJTI())
			assert.Nil(t, cred.RevokedAt, "must not revoke when the prior launch end was Hub-declared, not broker-confirmed")
		})
	}
}

// TestHTTPAgentDispatcher_DispatchAgentStart_ConfirmedLaunchErrorStillRevokes
// is the complement: a broker-confirmed prior failure (e.g. the agent
// process itself crashed) must still revoke on a subsequent start failure.
// The dispatch failure itself is a *brokerStatusError (a confirmed-safe
// failure under isConfirmedStartNotActedOnError), so this test isolates the
// LaunchError condition from the dispatch-error condition.
func TestHTTPAgentDispatcher_DispatchAgentStart_ConfirmedLaunchErrorStillRevokes(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-confirmed-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: &brokerStatusError{StatusCode: 400, Body: "start rejected"}}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-confirmed-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-confirmed-project"),
		RuntimeBrokerID: broker.ID,
		LaunchError:     store.LaunchErrorAgentError,
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
	require.Error(t, err)

	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	require.NotNil(t, cred.RevokedAt, "a broker-confirmed prior failure must still revoke on a start failure")
	assert.Equal(t, agentCredentialRevokeReasonStartFailed, *cred.RevokeReason)
}

// TestHTTPAgentDispatcher_DispatchAgentStart_AmbiguousTransportErrorSkipsRevoke
// covers a start request that fails with a transport-level timeout rather
// than a response from the broker: the Hub cannot tell whether the broker
// received and began acting on the request before the timeout, so the
// broker may already have started a container that uses the credential
// minted for this call. This must not revoke by agent, the same as an
// unconfirmed LaunchError.
func TestHTTPAgentDispatcher_DispatchAgentStart_AmbiguousTransportErrorSkipsRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-timeout-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: fmt.Errorf("failed to send request: %w", context.DeadlineExceeded)}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-timeout-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-timeout-project"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
	require.Error(t, err)

	require.Len(t, gen.jtis, 1, "this call still mints its own credential even though the revoke must not arm")
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "a start that fails with an ambiguous transport timeout must not revoke — the broker may already have started the container")
}

// TestHTTPAgentDispatcher_DispatchAgentStart_ControlChannelTimeoutSkipsRevoke
// covers a start sent over the control channel (the default path for a
// locally connected broker) rather than direct HTTP. BrokerConnection.
// TunnelRequest returns a plain "request timeout after ..." error when the
// Hub gives up waiting for the broker's response, which
// ControlChannelBrokerClient.doRequest wraps as "control channel request
// failed: ...". This is exactly as ambiguous as an HTTP round-trip timeout
// — the broker may already have started the container — and must not
// revoke by agent either.
func TestHTTPAgentDispatcher_DispatchAgentStart_ControlChannelTimeoutSkipsRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-cctimeout-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: fmt.Errorf("control channel request failed: %w", fmt.Errorf("request timeout after %v", 120*time.Second))}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-cctimeout-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-cctimeout-project"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
	require.Error(t, err)

	require.Len(t, gen.jtis, 1, "this call still mints its own credential even though the revoke must not arm")
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "a control-channel request timeout must not revoke — the broker may already have started the container")
}

// TestHTTPAgentDispatcher_DispatchAgentStart_HTTPDecodeFailureSkipsRevoke
// covers an HTTP 2xx response whose body fails to decode
// (brokerHTTPTransport.decodeResponseWithSnippet's "failed to decode
// response: ..." error). The broker answered successfully — it may well
// have started the container — so a decode failure on the Hub's side must
// not revoke by agent either.
func TestHTTPAgentDispatcher_DispatchAgentStart_HTTPDecodeFailureSkipsRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-decodefail-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: fmt.Errorf("failed to decode response: %w (body=%q)", errors.New("invalid character 'x'"), "not-json")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-decodefail-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-decodefail-project"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
	require.Error(t, err)

	require.Len(t, gen.jtis, 1, "this call still mints its own credential even though the revoke must not arm")
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "an undecodable 2xx response must not revoke — the broker already answered successfully")
}

// TestHTTPAgentDispatcher_DispatchAgentStart_RunningPhaseSkipsRevoke covers
// the user-facing lifecycle Restart action (stop-then-start, tolerating a
// failed stop) and the lifecycle Start action dispatched again against an
// agent whose phase is already running (resume-in-place). In both cases
// agent.Phase as loaded by the caller is "running" by the time
// DispatchAgentStart is entered, and the container that phase describes may
// genuinely still be up. A start failure here must not revoke by agent.
func TestHTTPAgentDispatcher_DispatchAgentStart_RunningPhaseSkipsRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-running-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("start rejected")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-running-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-running-project"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start rejected")

	require.Len(t, gen.jtis, 1, "this call still mints its own credential even though the revoke must not arm")
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "a start dispatched against a phase=running agent must not revoke on failure — the container may still be up")
}

// TestHTTPAgentDispatcher_DispatchAgentStart_TransitionalPhaseSkipsRevoke
// covers the two transitional phases that sit between a live container and
// a confirmed stop: "starting" (wake_dm.go leaves a resumed agent here after
// a readiness-wait timeout, even though the container may just be slow to
// report ready) and "stopping" (reincarnate_worker.go writes this before its
// own stop dispatch has confirmed the prior container actually exited). A
// start dispatched while the agent is in either phase must not revoke by
// agent on failure, for the same reason "running" does not.
func TestHTTPAgentDispatcher_DispatchAgentStart_TransitionalPhaseSkipsRevoke(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseStarting, state.PhaseStopping} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			memStore := createTestStore(t)
			broker := newTestBrokerForRevoke(t, memStore, "revoke-start-transitional-host-"+string(phase))

			mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("start rejected")}
			dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: memStore}
			dispatcher.SetTokenGenerator(gen)

			agent := &store.Agent{
				ID:              tid("revoke-start-transitional-agent-" + string(phase)),
				Name:            "test-agent",
				Slug:            "test-agent",
				ProjectID:       tid("revoke-start-transitional-project-" + string(phase)),
				RuntimeBrokerID: broker.ID,
				Phase:           string(phase),
			}

			err := dispatcher.DispatchAgentStart(ctx, agent, "", false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "start rejected")

			require.Len(t, gen.jtis, 1, "this call still mints its own credential even though the revoke must not arm")
			cred := getTestAgentCredential(t, memStore, gen.lastJTI())
			assert.Nil(t, cred.RevokedAt, "a start dispatched against a transitional phase must not revoke on failure — the container may still be up")
		})
	}
}

// TestHTTPAgentDispatcher_DispatchAgentStart_NoTokenGeneratedSkipsRevoke
// covers the case where buildStartEnv's GenerateAgentToken call fails (or no
// generator is configured), this call issued no credential, so a later
// start failure must not revoke by agent — a credential seeded before this
// dispatch (standing in for one still active from an earlier successful
// start) must survive.
func TestHTTPAgentDispatcher_DispatchAgentStart_NoTokenGeneratedSkipsRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-notoken-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("start rejected")}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore, failWith: errors.New("mint failed")}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-notoken-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-notoken-project"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
	}
	insertTestAgentCredential(t, memStore, agent.ID, agent.ProjectID, "revoke-start-notoken-preexisting-jti")

	err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
	require.Error(t, err)
	assert.Empty(t, gen.jtis, "the generator failed, so no credential should have been minted")

	cred := getTestAgentCredential(t, memStore, "revoke-start-notoken-preexisting-jti")
	assert.Nil(t, cred.RevokedAt, "a start that minted no credential must not revoke the agent's pre-existing one on failure")
}

// TestHTTPAgentDispatcher_DispatchAgentStart_CrossNodeDeferDisarmsOriginatorRevoke
// covers the cross-node hand-off: when client.StartAgent reports that the
// broker lives on another node (ErrLifecycleDeferred), this node hands off via
// deferredStart, which never carries the credential this node's
// buildStartEnv minted — the owning node mints and revokes its own on its
// own DispatchAgentStart. This node's own revoke must be disarmed before
// the hand-off, regardless of how the subsequent cross-node wait concludes
// (here, it fails immediately because no event bus/command bus is wired up
// in this test, standing in for every other way that wait can end
// ambiguously — a rolling timeout or a closed event channel — without
// telling this node whether the owning node's own start, and its own
// credential, actually succeeded).
func TestHTTPAgentDispatcher_DispatchAgentStart_CrossNodeDeferDisarmsOriginatorRevoke(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "revoke-start-crossnode-host")

	mockClient := &mockRuntimeBrokerClient{returnErr: ErrLifecycleDeferred}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)

	agent := &store.Agent{
		ID:              tid("revoke-start-crossnode-agent"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("revoke-start-crossnode-project"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
	}
	// Stands in for the credential the owning node mints for its own start
	// attempt. It must never be touched by this node.
	insertTestAgentCredential(t, memStore, agent.ID, agent.ProjectID, "revoke-start-crossnode-owner-jti")

	err := dispatcher.DispatchAgentStart(ctx, agent, "", true)
	require.Error(t, err, "the cross-node wait failure must still surface as an error")

	require.Len(t, gen.jtis, 1, "this node's buildStartEnv must still mint its own credential before handing off")
	originatorCred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, originatorCred.RevokedAt, "the originator must not revoke the credential it minted once it hands off to the owning node")

	ownerCred := getTestAgentCredential(t, memStore, "revoke-start-crossnode-owner-jti")
	assert.Nil(t, ownerCred.RevokedAt, "the originator must never revoke a credential it did not mint, including the owning node's")
}
