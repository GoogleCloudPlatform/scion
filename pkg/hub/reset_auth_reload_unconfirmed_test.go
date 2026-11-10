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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5460: a broker's 502 reload_unconfirmed answer to a
// reset-auth reaches the caller on every hub path.

// reloadUnconfirmedBody is a broker's reload_unconfirmed error body. It
// carries a raw marker the hub must never echo.
const reloadUnconfirmedBody = `{"error":{"code":"reload_unconfirmed","message":"raw-broker-text-5460"}}`

func reloadUnconfirmedErr() error {
	return &brokerStatusError{StatusCode: http.StatusBadGateway, Body: reloadUnconfirmedBody}
}

// reloadUnconfirmedClient answers every reset-auth push with the broker's
// reload_unconfirmed 502.
type reloadUnconfirmedClient struct {
	*mintBrokerClient
}

func (m *reloadUnconfirmedClient) ResetAuthAgent(_ context.Context, _, _, _, _, _, _ string) error {
	return reloadUnconfirmedErr()
}

func TestIsBrokerReloadUnconfirmed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"502 reload_unconfirmed", reloadUnconfirmedErr(), true},
		{"wrapped", fmt.Errorf("dispatch: %w", reloadUnconfirmedErr()), true},
		{"502 other code", &brokerStatusError{StatusCode: http.StatusBadGateway, Body: `{"error":{"code":"runtime_error"}}`}, false},
		{"500 reload_unconfirmed", &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: reloadUnconfirmedBody}, false},
		{"plain error", errors.New("reload_unconfirmed"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isBrokerReloadUnconfirmed(tc.err))
		})
	}
}

func TestWriteBrokerReloadUnconfirmed(t *testing.T) {
	rec := httptest.NewRecorder()
	assert.False(t, writeBrokerReloadUnconfirmed(rec, errors.New("other")))

	rec = httptest.NewRecorder()
	require.True(t, writeBrokerReloadUnconfirmed(rec, reloadUnconfirmedErr()))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, api.BrokerErrCodeReloadUnconfirmed, apiErr.Code)
	assert.Equal(t, api.ReloadUnconfirmedMessage, apiErr.Message)
	assert.NotContains(t, rec.Body.String(), "raw-broker-text-5460")
}

// useReloadUnconfirmedClient swaps f's dispatcher for one whose broker
// answers every reset-auth with reload_unconfirmed.
func useReloadUnconfirmedClient(f *mintFixture) {
	disp := NewHTTPAgentDispatcherWithClient(f.store, &reloadUnconfirmedClient{f.client}, false, nil)
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)
}

// The single-agent reset-auth answers 502 reload_unconfirmed with the shared
// message, not the generic 500 carrying the broker body.
func TestHandleAgentResetAuth_ReloadUnconfirmed(t *testing.T) {
	f := newMintFixture(t, "reset-reload")
	a := f.agent(t, "reset-reload-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provSession)
	useReloadUnconfirmedClient(f)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reset-auth", nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, api.BrokerErrCodeReloadUnconfirmed, apiErr.Code)
	assert.Equal(t, api.ReloadUnconfirmedMessage, apiErr.Message)
	assert.NotContains(t, rec.Body.String(), "raw-broker-text-5460")
}

// The bulk reset-auth reports the agent as failed with code
// reload_unconfirmed and the shared message.
func TestAdminResetAuthAll_ReloadUnconfirmed(t *testing.T) {
	f := newMintFixture(t, "rar-reload")
	setBackfillCompleted(t, f.store)
	a := healthyResetAuthAgent(t, f, "rar-reload-agent")
	useReloadUnconfirmedClient(f)

	body, raw := postResetAuthAll(t, f.srv)

	assert.Empty(t, body.Succeeded, raw)
	require.Len(t, body.Failed, 1, raw)
	assert.Equal(t, map[string]any{
		"id": a.ID, "name": a.Name,
		"code":  api.BrokerErrCodeReloadUnconfirmed,
		"error": api.ReloadUnconfirmedMessage,
	}, body.Failed[0])
	assert.Equal(t, map[string]int{api.BrokerErrCodeReloadUnconfirmed: 1}, body.FailedByCode)
	assert.NotContains(t, raw, "raw-broker-text-5460")
}

// A scope re-issue whose push gets reload_unconfirmed counts as dispatched:
// the credential was delivered. The response carries reload_unconfirmed, not
// a dispatch error, and the dispatch audit row records both.
func TestScopeReissue_ReloadUnconfirmedIsDispatched(t *testing.T) {
	f := newReissueFixture(t, "rs-reload", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	disp := NewHTTPAgentDispatcherWithClient(f.store, &reloadUnconfirmedClient{f.client}, false, nil)
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)

	resp := f.run(t, f.child, false)
	assert.True(t, resp.Dispatched)
	assert.Empty(t, resp.DispatchError)
	assert.Equal(t, api.ReloadUnconfirmedMessage, resp.ReloadUnconfirmed)

	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch)
	require.Len(t, recs, 1)
	var summary reissueDispatchSummary
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &summary))
	assert.True(t, summary.Dispatched)
	assert.Equal(t, api.BrokerErrCodeReloadUnconfirmed, summary.ErrorClass)
}

// In a bulk re-issue such an agent stays "changed", is not counted in
// push_failed, and carries reload_unconfirmed.
func TestScopeReissueBulk_ReloadUnconfirmedStaysChanged(t *testing.T) {
	f := newReissueFixture(t, "rsb-reload", store.ProjectRoleOwner)
	disp := NewHTTPAgentDispatcherWithClient(f.store, &reloadUnconfirmedClient{f.client}, false, nil)
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)

	resp := f.bulk(t, false)
	child := bulkAgent(t, resp, f.child.ID)
	assert.Equal(t, "changed", child.Outcome)
	assert.Equal(t, api.ReloadUnconfirmedMessage, child.ReloadUnconfirmed)
	assert.Empty(t, child.DispatchErr)
	assert.Empty(t, resp.PushFailed)
}
