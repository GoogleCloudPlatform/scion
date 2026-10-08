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

package hubsync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const localOnlyHintText = "\n\nTo use local-only mode, use: scion --no-hub <command>"

// newHintTestHubCtx returns a HubContext whose real hubclient talks to an
// httptest hub driven by handler, with HOME and agent context isolated.
func newHintTestHubCtx(t *testing.T, handler http.HandlerFunc) (*HubContext, *httptest.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_AGENT_ID", "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	return &HubContext{
		Client:      client,
		Endpoint:    srv.URL,
		ProjectID:   "project-hint",
		ProjectPath: projectDir,
		BrokerID:    "broker-hint",
		Settings:    &config.Settings{},
	}, srv
}

// noContentHub answers every request with 204, so any hubclient call that
// needs a body fails with an error wrapping apiclient.ErrNoContent.
func noContentHub(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// TestWrapHubError_EmptyResponseOnSyncPaths drives each sync and register
// call against a hub that answers 204 and wraps the error exactly as its
// caller in EnsureHubReady / resolveHubGlobalProjectID does. The rendered
// message must carry the empty-response note and no local-only hint
// (ptone/scion#3785).
func TestWrapHubError_EmptyResponseOnSyncPaths(t *testing.T) {
	const note = "\n\nThe hub returned an empty response where a result was expected."

	t.Run("register project", func(t *testing.T) {
		hubCtx, _ := newHintTestHubCtx(t, noContentHub)
		err := registerProject(context.Background(), hubCtx, "proj", true)
		require.Error(t, err)
		got := wrapHubError(fmt.Errorf("failed to register project: %w", err))
		assert.Equal(t, "failed to register project: server returned no content (status: 204)"+note, got.Error())
		assert.NotContains(t, got.Error(), "local-only")
	})

	t.Run("compare agents", func(t *testing.T) {
		hubCtx, _ := newHintTestHubCtx(t, noContentHub)
		_, err := CompareAgents(context.Background(), hubCtx)
		require.Error(t, err)
		got := wrapHubError(fmt.Errorf("failed to compare agents: %w", err))
		assert.Equal(t, "failed to compare agents: failed to list Hub agents: server returned no content (status: 204)"+note, got.Error())
		assert.NotContains(t, got.Error(), "local-only")
	})

	t.Run("sync agents", func(t *testing.T) {
		hubCtx, _ := newHintTestHubCtx(t, noContentHub)
		err := ExecuteSync(context.Background(), hubCtx, &SyncResult{ToRegister: []string{"a1"}}, true)
		require.Error(t, err)
		got := wrapHubError(fmt.Errorf("failed to sync agents: %w", err))
		assert.Equal(t, "failed to sync agents: failed to register agent 'a1': server returned no content (status: 204)"+note, got.Error())
		assert.NotContains(t, got.Error(), "local-only")
	})

	t.Run("global project lookup", func(t *testing.T) {
		hubCtx, srv := newHintTestHubCtx(t, noContentHub)
		_, err := resolveHubGlobalProjectID(context.Background(), hubCtx.Client, srv.URL)
		require.Error(t, err)
		assert.Equal(t, "failed to look up the Global project on hub "+srv.URL+": server returned no content (status: 204)"+note, err.Error())
		assert.NotContains(t, err.Error(), "local-only")
	})

	t.Run("hub-managed agent also gets the note", func(t *testing.T) {
		hubCtx, _ := newHintTestHubCtx(t, noContentHub)
		t.Setenv("SCION_AGENT_ID", "agent-uuid-123")
		err := registerProject(context.Background(), hubCtx, "proj", true)
		require.Error(t, err)
		got := wrapHubError(fmt.Errorf("failed to register project: %w", err))
		assert.Equal(t, "failed to register project: server returned no content (status: 204)"+note, got.Error())
	})
}

// TestWrapHubError_OtherErrorsUnchanged pins the rendering of the other
// error classes on the same paths so the empty-response branch does not
// swallow them: a connection failure and an API error keep the local-only
// hint, and a 401 keeps the login guidance.
func TestWrapHubError_OtherErrorsUnchanged(t *testing.T) {
	t.Run("connection failure keeps hint", func(t *testing.T) {
		hubCtx, srv := newHintTestHubCtx(t, noContentHub)
		srv.Close()
		err := registerProject(context.Background(), hubCtx, "proj", true)
		require.Error(t, err)
		wrapped := fmt.Errorf("failed to register project: %w", err)
		got := wrapHubError(wrapped)
		assert.Equal(t, wrapped.Error()+localOnlyHintText, got.Error())
		assert.NotContains(t, got.Error(), "empty response")
	})

	t.Run("401 keeps login guidance", func(t *testing.T) {
		hubCtx, _ := newHintTestHubCtx(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad token"}}`))
		})
		_, err := CompareAgents(context.Background(), hubCtx)
		require.Error(t, err)
		got := wrapHubError(fmt.Errorf("failed to compare agents: %w", err))
		assert.Equal(t, "authentication failed, login to hub with 'scion hub auth login'", got.Error())
	})

	t.Run("API error keeps hint", func(t *testing.T) {
		hubCtx, _ := newHintTestHubCtx(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"already exists"}}`))
		})
		err := ExecuteSync(context.Background(), hubCtx, &SyncResult{ToRegister: []string{"a1"}}, true)
		require.Error(t, err)
		wrapped := fmt.Errorf("failed to sync agents: %w", err)
		got := wrapHubError(wrapped)
		assert.Equal(t, wrapped.Error()+localOnlyHintText, got.Error())
		assert.NotContains(t, got.Error(), "empty response")
	})
}
