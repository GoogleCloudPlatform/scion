//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var bundleCaps = &store.BrokerCapabilities{
	AgentMove:       true,
	AgentMoveBundle: true,
}

func TestAgentMoveBundleCapabilityIngest(t *testing.T) {
	for _, name := range []string{"agentMoveBundle", "agent_move_bundle"} {
		assert.True(t, capabilitiesFromStrings([]string{name}).AgentMoveBundle, "name=%q", name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"agentMove", "agentMoveRecreate"}).AgentMoveBundle)
}

func TestDispatchAgentProvisionForBundleMove_CapabilityAndWorkspaceSource(t *testing.T) {
	t.Run("capable broker receives WorkspaceSource=move-bundle", func(t *testing.T) {
		d, client, agent := newMoveDispatchFixture(t, bundleCaps)
		require.NoError(t, d.DispatchAgentProvisionForBundleMove(context.Background(), agent, "rid-bundle-1"))
		require.NotNil(t, client.lastCreateReq)
		assert.True(t, client.lastCreateReq.ProvisionOnly)
		assert.False(t, client.lastCreateReq.Reprovision)
		assert.Equal(t, "fresh-agent-dir", client.lastCreateReq.ExpectExistingNFSWorkspace)
		assert.Equal(t, "rid-bundle-1", client.lastCreateReq.MoveReincarnationID)
		assert.Equal(t, api.WorkspaceSourceMoveBundle, client.lastCreateReq.WorkspaceSource)
	})

	t.Run("missing reincarnation ID refused", func(t *testing.T) {
		d, client, agent := newMoveDispatchFixture(t, bundleCaps)
		require.Error(t, d.DispatchAgentProvisionForBundleMove(context.Background(), agent, ""))
		assert.Nil(t, client.lastCreateReq)
	})

	for name, caps := range map[string]*store.BrokerCapabilities{
		"agentMove only":       {AgentMove: true},
		"agentMoveBundle only": {AgentMoveBundle: true},
		"no capabilities":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			d, client, agent := newMoveDispatchFixture(t, caps)
			err := d.DispatchAgentProvisionForBundleMove(context.Background(), agent, "rid-bundle-1")
			require.Error(t, err)
			assert.True(t, errors.Is(err, errBrokerLacksAgentMoveBundle) || errors.Is(err, errBrokerLacksAgentMove), "err=%v", err)
			assert.Nil(t, client.lastCreateReq)
		})
	}
}

func TestDispatchMoveBundle_HTTPRoundTripsAndCapabilityGuard(t *testing.T) {
	t.Run("refuses broker without AgentMoveBundle before sending", func(t *testing.T) {
		d, _, agent := newMoveDispatchFixture(t, &store.BrokerCapabilities{AgentMove: true})
		lim := api.DefaultMoveBundleLimits()
		_, err := d.DispatchMoveBundlePrecheck(context.Background(), agent, lim, []string{"~/.config/hub/tok"})
		assert.ErrorIs(t, err, errBrokerLacksAgentMoveBundle)
		_, err = d.DispatchMoveBundleExport(context.Background(), agent, "rid-1", lim, []string{"~/.config/hub/tok"})
		assert.ErrorIs(t, err, errBrokerLacksAgentMoveBundle)
		_, err = d.DispatchMoveBundleExportStatus(context.Background(), agent, "rid-1")
		assert.ErrorIs(t, err, errBrokerLacksAgentMoveBundle)
		_, err = d.DispatchMoveBundleImport(context.Background(), agent, "rid-1", bundle.Manifest{}, "sha256:abc", lim)
		assert.ErrorIs(t, err, errBrokerLacksAgentMoveBundle)
		_, err = d.DispatchMoveBundleImportStatus(context.Background(), agent, "rid-1")
		assert.ErrorIs(t, err, errBrokerLacksAgentMoveBundle)
	})

	t.Run("returns resolveDispatchProjectInfo error instead of falling back to global project (R3)", func(t *testing.T) {
		d, _, agent := newMoveDispatchFixture(t, bundleCaps)
		agent.ProjectID = "nonexistent-project-id"
		_, err := d.DispatchMoveBundlePrecheck(context.Background(), agent, api.DefaultMoveBundleLimits(), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolve project dispatch info")
	})

	t.Run("HTTP client round-trips all five endpoints", func(t *testing.T) {
		var seenPaths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenPaths = append(seenPaths, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/agents/dev/move-bundle/precheck":
				_ = json.NewEncoder(w).Encode(api.MoveBundlePrecheckResponse{
					Durable:   true,
					Workspace: api.MoveBundleRootEstimate{Bytes: 42, Entries: 2},
				})
			case "/api/v1/agents/dev/move-bundle/export":
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(api.MoveBundleJobStatus{
					ReincarnationID: "rid-1",
					Kind:            api.MoveBundleJobKindExport,
					State:           api.MoveBundleJobStateRunning,
				})
			case "/api/v1/agents/dev/move-bundle/export/rid-1":
				_ = json.NewEncoder(w).Encode(api.MoveBundleJobStatus{
					ReincarnationID: "rid-1",
					Kind:            api.MoveBundleJobKindExport,
					State:           api.MoveBundleJobStateSucceeded,
				})
			case "/api/v1/agents/dev/move-bundle/import":
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(api.MoveBundleJobStatus{
					ReincarnationID: "rid-1",
					Kind:            api.MoveBundleJobKindImport,
					State:           api.MoveBundleJobStateRunning,
				})
			case "/api/v1/agents/dev/move-bundle/import/rid-1":
				_ = json.NewEncoder(w).Encode(api.MoveBundleJobStatus{
					ReincarnationID: "rid-1",
					Kind:            api.MoveBundleJobKindImport,
					State:           api.MoveBundleJobStateSucceeded,
					SkipCounts:      &api.MoveBundleImportSkipCounts{HomeProvisionWins: 3},
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		c := NewHTTPRuntimeBrokerClient()
		ctx := context.Background()

		pre, err := c.PrecheckMoveBundle(ctx, "broker-1", srv.URL, "dev", "proj-1", &api.MoveBundlePrecheckRequest{ProjectID: "proj-1"})
		require.NoError(t, err)
		assert.True(t, pre.Durable)
		assert.Equal(t, int64(42), pre.Workspace.Bytes)

		expStart, err := c.StartMoveBundleExport(ctx, "broker-1", srv.URL, "dev", "proj-1", &api.MoveBundleExportRequest{ReincarnationID: "rid-1"})
		require.NoError(t, err)
		assert.Equal(t, api.MoveBundleJobStateRunning, expStart.State)

		expSt, err := c.GetMoveBundleExportStatus(ctx, "broker-1", srv.URL, "dev", "proj-1", "rid-1")
		require.NoError(t, err)
		assert.Equal(t, api.MoveBundleJobStateSucceeded, expSt.State)

		impStart, err := c.StartMoveBundleImport(ctx, "broker-1", srv.URL, "dev", "proj-1", &api.MoveBundleImportRequest{ReincarnationID: "rid-1"})
		require.NoError(t, err)
		assert.Equal(t, api.MoveBundleJobStateRunning, impStart.State)

		impSt, err := c.GetMoveBundleImportStatus(ctx, "broker-1", srv.URL, "dev", "proj-1", "rid-1")
		require.NoError(t, err)
		assert.Equal(t, api.MoveBundleJobStateSucceeded, impSt.State)
		require.NotNil(t, impSt.SkipCounts)
		assert.Equal(t, int64(3), impSt.SkipCounts.HomeProvisionWins)

		assert.Equal(t, []string{
			"POST /api/v1/agents/dev/move-bundle/precheck",
			"POST /api/v1/agents/dev/move-bundle/export",
			"GET /api/v1/agents/dev/move-bundle/export/rid-1",
			"POST /api/v1/agents/dev/move-bundle/import",
			"GET /api/v1/agents/dev/move-bundle/import/rid-1",
		}, seenPaths)
	})
}
