// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

const fixedCreateSecretMessage = "agent secrets could not be resolved; the agent was not created"

// Create, create-with-gather and finalize-env fail on a secret resolution
// error with the fixed "not created" message, before any broker request and
// before any credential is issued.
func TestCreate_SecretResolutionError_FailsBeforeBroker(t *testing.T) {
	for _, tc := range runDispatches[:3] {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunDispatchFixture(t)
			f.d.SetSecretBackend(&failingResolveBackend{})

			err := tc.dispatch(context.Background(), f)
			var resErr *secretResolutionError
			require.ErrorAs(t, err, &resErr)
			assert.Equal(t, "created", resErr.Verb)
			assert.Equal(t, fixedCreateSecretMessage, err.Error())
			assertNoSecretResolutionDetail(t, err.Error(), "the error text")
			assert.True(t, isConfirmedStartNotActedOnError(err))

			_, _, called := tc.sent(f)
			assert.False(t, called, "nothing is sent to the broker")
			got, gerr := f.store.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, gerr)
			assert.Equal(t, "run-before", got.RunID, "no run is begun, so no credential is issued")
		})
	}
}

// Absence is not an error: with an empty secret backend every create path
// still reaches the broker.
func TestCreate_NoSecretsStillCreates(t *testing.T) {
	for _, tc := range runDispatches[:3] {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunDispatchFixture(t)
			f.d.SetSecretBackend(&mockSecretBackend{})
			require.NoError(t, tc.dispatch(context.Background(), f))
			_, _, called := tc.sent(f)
			assert.True(t, called)
		})
	}
}

// Provision and reprovision keep going on a secret resolution error: the
// broker's provision does not read the secrets, and a later start resolves
// them again and fails there. Each succeeds and sends no secrets.
func TestProvision_SecretResolutionError_StillProvisions(t *testing.T) {
	cases := []struct {
		name     string
		dispatch func(ctx context.Context, f *runDispatchFixture) error
	}{
		{"provision", func(ctx context.Context, f *runDispatchFixture) error {
			return f.d.DispatchAgentProvision(ctx, f.agent)
		}},
		{"reprovision", func(ctx context.Context, f *runDispatchFixture) error {
			return f.d.DispatchAgentReprovision(ctx, f.agent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunDispatchFixture(t)
			f.d.SetSecretBackend(&failingResolveBackend{})
			require.NoError(t, tc.dispatch(context.Background(), f))
			require.True(t, f.client.createCalled, "the provision request is sent")
			require.NotNil(t, f.client.lastCreateReq)
			assert.True(t, f.client.lastCreateReq.ProvisionOnly)
			assert.Empty(t, f.client.lastCreateReq.ResolvedSecrets, "no secrets are sent")
		})
	}
}

func TestSecretResolutionVerb(t *testing.T) {
	for op, want := range map[string]string{
		"start": "started", "restart": "restarted", "create": "created", "finalize_env": "created", "": "started",
	} {
		assert.Equal(t, want, secretResolutionVerb(op), "op %q", op)
	}
	for _, op := range []string{"create", "finalize_env"} {
		err := dispatchFailureError(&store.BrokerDispatch{Op: op, Result: `{"secretResolution":true}`})
		var got *secretResolutionError
		require.ErrorAs(t, err, &got, op)
		assert.Equal(t, "created", got.Verb, op)
	}
}

// A full create that fails on a secret resolution error answers 503
// unavailable with the fixed message and is rolled back: the agent row is
// removed, as for any create dispatch failure. Both create sites.
func TestCreateHandler_SecretResolutionError_503RollsBack(t *testing.T) {
	for _, gatherEnv := range []bool{true, false} {
		t.Run(fmt.Sprintf("gatherEnv=%v", gatherEnv), func(t *testing.T) {
			disp := &failingCreateDispatcher{createErr: fakeSecretResolutionErr("created")}
			srv, s, project := setupCreateAgentServer(t, disp)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      fmt.Sprintf("secrets-gather-%v", gatherEnv),
				ProjectID: project.ID,
				Task:      "do something",
				GatherEnv: gatherEnv,
			})
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, ErrCodeUnavailable, body.Error.Code)
			assert.Equal(t, fixedCreateSecretMessage, body.Error.Message)
			assertNoSecretResolutionDetail(t, rec.Body.String(), "the API body")

			require.NotNil(t, disp.capturedAgent, "the create reached dispatch")
			_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
			assert.ErrorIs(t, err, store.ErrNotFound, "the agent row is rolled back")
		})
	}
}

// An env-gather submit whose owning node fails secret resolution answers
// 503 with the fixed message; the record stays provisioning so the submit
// can be retried, and the claim is released.
func TestFinalizeEnvHandler_SecretResolutionError_503KeepsRecord(t *testing.T) {
	srv, agent, ownerDisp := crossNodeHandlerServerWithOwner(t, nil, state.PhaseProvisioning)
	ownerDisp.finalizeErr = fakeSecretResolutionErr("created")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent.ProjectID+"/agents/"+agent.Slug+"/env",
		map[string]interface{}{"env": map[string]string{"A_KEY": "v"}})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeUnavailable, code)
	assert.Contains(t, rec.Body.String(), fixedCreateSecretMessage)
	assertNoSecretResolutionDetail(t, rec.Body.String(), "the API body")

	got := getAgent(t, srv.store, agent.ID)
	assert.Equal(t, string(state.PhaseProvisioning), got.Phase, "the record is kept")
	assert.Empty(t, got.StartClaimID)
}

// The workspace bootstrap create-and-start, through the real dispatcher
// with a failing secret backend: 503 with the fixed message, no broker
// create, and the record is kept.
func TestWorkspaceFinalize_SecretResolutionError_503(t *testing.T) {
	srv, f, c := newLandingCompletionServer(t)
	d := NewHTTPAgentDispatcherWithClient(f.store, c, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return AsyncLaunchSettings{} })
	d.SetSecretBackend(&failingResolveBackend{})
	srv.SetDispatcher(d)
	agent := f.agent(t, "fin-secrets", string(state.PhaseProvisioning), false)

	rec := doBootstrapRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to/finalize", agent.ID),
		SyncToFinalizeRequest{Manifest: &transfer.Manifest{Version: "1.0"}})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeUnavailable)
	assert.Contains(t, rec.Body.String(), fixedCreateSecretMessage)
	assertNoSecretResolutionDetail(t, rec.Body.String(), "the API body")
	assert.Nil(t, c.lastCreateReq, "nothing is sent to the broker")

	got, err := f.store.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err, "the record is kept")
	assert.Empty(t, got.StartClaimID)
}
