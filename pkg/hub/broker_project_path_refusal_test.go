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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// projectPathLocalPath is the provider's stored LocalPath in these tests.
	projectPathLocalPath = "/srv/provider-local-path"
	// projectPathResolvedTarget stands for a path the broker resolved (a
	// symlink or marker target). It appears only in the broker's own
	// message, and must never reach a client.
	projectPathResolvedTarget = "/resolved/symlink-target"
)

// brokerProjectPathRefusal is the broker's 409 project_path_not_initialized
// as it reaches the hub, its message naming host paths.
func brokerProjectPathRefusal() error {
	return brokerHarnessConfigErr(http.StatusConflict, projectPathNotInitializedErrorCode,
		"project path "+projectPathLocalPath+" has no .scion directory or marker (resolved "+projectPathResolvedTarget+")")
}

// requireProjectPathRefusal checks w is 409 project_path_not_initialized and
// that the message names no broker-resolved path. It returns the message.
func requireProjectPathRefusal(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, `"`+projectPathNotInitializedErrorCode+`"`)
	assert.NotContains(t, body, projectPathResolvedTarget, "a broker-resolved path must never reach the client")
	return body
}

// TestRelayBrokerRefusal_ProjectPathNotInitialized: the generic relay (used
// where the hub does not hold the agent) answers 409 with the code and the
// hub's own path-free text, never the broker's message.
func TestRelayBrokerRefusal_ProjectPathNotInitialized(t *testing.T) {
	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, brokerProjectPathRefusal(), "")
	body := requireProjectPathRefusal(t, w)
	assert.Contains(t, body, projectPathRefusalGeneric)
	assert.NotContains(t, body, projectPathLocalPath, "the generic relay names no path")
}

func TestProjectPathRefusalText(t *testing.T) {
	withPath := projectPathRefusalText("edge-1", projectPathLocalPath)
	assert.Contains(t, withPath, "Runtime Broker edge-1")
	assert.Contains(t, withPath, projectPathLocalPath+"/.scion")
	without := projectPathRefusalText("edge-1", "")
	assert.Contains(t, without, "Runtime Broker edge-1")
	assert.NotContains(t, without, "/")
}

// TestRelayProjectPathRefusal_ProviderReaders: the agent-aware relay names
// the provider's stored LocalPath to a caller who may read the project's
// provider records, and leaves it out for anyone else. Neither sees a path
// the broker resolved.
func TestRelayProjectPathRefusal_ProviderReaders(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	brokerID := tid("pp-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "pp-broker", Slug: "pp-broker", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: brokerID, BrokerName: "pp-broker", LocalPath: projectPathLocalPath,
	}))
	agent := &store.Agent{ID: tid("pp-agent"), ProjectID: project.ID, RuntimeBrokerID: brokerID}

	userCtx := func(u *store.User) context.Context {
		return contextWithIdentity(ctx, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web"))
	}
	require.True(t, srv.callerMayReadProviders(userCtx(alice), project.ID), "fixture: alice reads the project")
	require.False(t, srv.callerMayReadProviders(userCtx(bob), project.ID), "fixture: bob does not")

	t.Run("provider reader sees the stored LocalPath", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.True(t, srv.relayProjectPathRefusal(userCtx(alice), w, agent, brokerProjectPathRefusal()))
		body := requireProjectPathRefusal(t, w)
		assert.Contains(t, body, projectPathLocalPath+"/.scion")
		assert.Contains(t, body, "pp-broker")
	})
	t.Run("other caller gets the code without the path", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.True(t, srv.relayProjectPathRefusal(userCtx(bob), w, agent, brokerProjectPathRefusal()))
		body := requireProjectPathRefusal(t, w)
		assert.NotContains(t, body, projectPathLocalPath)
	})
	t.Run("no identity gets the code without the path", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.True(t, srv.relayProjectPathRefusal(ctx, w, agent, brokerProjectPathRefusal()))
		body := requireProjectPathRefusal(t, w)
		assert.NotContains(t, body, projectPathLocalPath)
	})
	t.Run("other broker errors are not taken", func(t *testing.T) {
		w := httptest.NewRecorder()
		other := brokerHarnessConfigErr(http.StatusConflict, "conflict", "name in use")
		assert.False(t, srv.relayProjectPathRefusal(userCtx(alice), w, agent, other))
		assert.False(t, strings.Contains(w.Body.String(), projectPathNotInitializedErrorCode))
	})
}
