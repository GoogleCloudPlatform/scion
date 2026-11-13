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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireProjectDeleteResponse checks a successful project delete: either
// 204 with no body, or (the Hub cannot see the shared project storage) 200
// whose body is exactly the one fixed storageKept item. Tests whose subject
// is not storage visibility use it; the visibility tests below pin which.
func requireProjectDeleteResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	switch rec.Code {
	case http.StatusNoContent:
		assert.Empty(t, rec.Body.String())
	case http.StatusOK:
		requireSharedStorageKeptBody(t, rec)
	default:
		require.Failf(t, "unexpected project delete status", "status %d, body: %s", rec.Code, rec.Body.String())
	}
}

// requireSharedStorageKeptBody checks a 200 project delete body: exactly
// one storageKept item, with the fixed code and the fixed message.
func requireSharedStorageKeptBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got projectDeleteResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body: %s", rec.Body.String())
	require.Len(t, got.StorageKept, 1, "body: %s", rec.Body.String())
	assert.Equal(t, "shared-project-storage", got.StorageKept[0].Code)
	assert.Equal(t, "The Hub cannot see the shared project storage, so the deleted project's shared tree was kept. Hub health reports the shared project storage as not ready on nfs while the Hub has projects, and as degraded on the other shared backends.", got.StorageKept[0].Message)
}

// requireBrokerDirKeptBody checks a 200 project delete body when one or more
// Runtime Brokers declined directory cleanup with 409 Conflict
// (ptone/scion#4474).
func requireBrokerDirKeptBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got projectDeleteResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body: %s", rec.Body.String())
	require.Len(t, got.StorageKept, 1, "body: %s", rec.Body.String())
	assert.Equal(t, "runtime-broker-project-directory", got.StorageKept[0].Code)
	assert.Equal(t, projectDeleteBrokerDirKeptMessage, got.StorageKept[0].Message)
}

// When the Hub cannot see the shared project storage (the share is not
// mounted on the Hub), the delete still commits, but it does not answer an
// unqualified 204: it answers 200 with the fixed, path-free storageKept
// item.
func TestDeleteProject_SharedStorageNotVisible_ReportsKept(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "kept-unmounted")
	absent := filepath.Join(e.home, "absent")
	e.srv.config.ProjectStorageConfig.NFS.MountRoot = absent
	logs := captureProjectsLog(t, e.srv)

	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)

	requireSharedStorageKeptBody(t, rec)
	body := rec.Body.String()
	for _, leak := range []string{absent, e.home, "share1", "10.0.0.2", "/ws", "projects/", e.project.ID, e.project.Slug} {
		assert.NotContains(t, body, leak, "the response must not carry %q", leak)
	}
	e.srv.storageCleanupWG.Wait()
	_, err := e.s.GetProject(context.Background(), e.project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the delete itself commits")

	// The kept tree is one ERROR audit line on the storage-deletion channel,
	// naming the project ID and no path.
	var keptLines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `msg="project storage deletion"`) && strings.Contains(l, "outcome=kept") && strings.Contains(l, "Hub cannot see") {
			keptLines = append(keptLines, l)
		}
	}
	require.Len(t, keptLines, 1, "logs:\n%s", logs.String())
	assert.Contains(t, keptLines[0], "level=ERROR")
	assert.Contains(t, keptLines[0], "project_id="+e.project.ID)
	assert.Contains(t, keptLines[0], "deleter=hub")
	assert.NotContains(t, keptLines[0], "path=")
	assert.NotContains(t, keptLines[0], absent)
}

// With the shared project storage visible, the delete answers 204 with no
// body, as before.
func TestDeleteProject_SharedStorageVisible_NoContent(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "kept-mounted")
	e.seedTree(t, e.project.ID)

	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)

	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, rec.Body.String())
	e.srv.storageCleanupWG.Wait()
}

// A hung visibility probe (a stale or hung mount) must not block the delete
// handler: after the short deadline it counts as not visible, and the delete
// answers 200 with the storageKept item.
func TestDeleteProject_SharedStorageProbeHangs_ReportsKept(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "kept-hung")
	release := make(chan struct{})
	prevProbe, prevTimeout := projectStorageTreeProbe, projectDeleteStorageCheckTimeout
	projectStorageTreeProbe = func(projectfs.Layout) error { <-release; return nil }
	projectDeleteStorageCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		projectStorageTreeProbe, projectDeleteStorageCheckTimeout = prevProbe, prevTimeout
	})

	start := time.Now()
	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)
	elapsed := time.Since(start)

	requireSharedStorageKeptBody(t, rec)
	assert.Less(t, elapsed, 10*time.Second, "the delete must not wait for the hung probe")
	e.srv.storageCleanupWG.Wait()
}

// ptone/scion#4474: when a Runtime Broker declines directory cleanup with 409
// Conflict (another flat instance on the same host or a remaining agent still
// uses the shared directory), DELETE /api/v1/projects/{id} commits the delete
// and answers 200 OK with a path-free, ID-free storageKept item across both
// Hub dispatch paths (direct HTTP and HybridBrokerClient control-channel /
// Conduit tunnels).
func TestDeleteProject_BrokerDeclinesDirectoryCleanup409_ReportsKept(t *testing.T) {
	setupProjectWithBroker := func(t *testing.T, suffix, endpoint string) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
		t.Helper()
		srv, s := testServer(t)
		ctx := context.Background()
		project := &store.Project{
			ID:   tid("project-broker-kept-" + suffix),
			Slug: "broker-kept-" + suffix,
			Name: "Broker Kept " + suffix,
		}
		require.NoError(t, s.CreateProject(ctx, project))
		broker := &store.RuntimeBroker{
			ID:       tid("broker-kept-" + suffix),
			Slug:     "broker-kept-slug-" + suffix,
			Name:     "Broker Kept " + suffix,
			Status:   store.BrokerStatusOnline,
			Endpoint: endpoint,
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LinkedBy:   "test",
		}))
		return srv, s, project, broker
	}

	assertNoLeaks := func(t *testing.T, body string, leaks ...string) {
		t.Helper()
		for _, leak := range leaks {
			if leak != "" {
				assert.NotContains(t, body, leak, "response must not leak %q", leak)
			}
		}
	}

	t.Run("direct HTTP broker client", func(t *testing.T) {
		var httpCalls atomic.Int32
		brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpCalls.Add(1)
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, "/api/v1/projects/broker-kept-http", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"another instance at /var/lib/scion/projects/broker-kept-http still uses this directory"}}`))
		}))
		t.Cleanup(brokerSrv.Close)

		srv, s, project, broker := setupProjectWithBroker(t, "http", brokerSrv.URL)
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, NewHTTPRuntimeBrokerClient(), false, slog.Default()))

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
		requireBrokerDirKeptBody(t, rec)
		assert.EqualValues(t, 1, httpCalls.Load())
		assertNoLeaks(t, rec.Body.String(),
			"/var/lib/scion", brokerSrv.URL, broker.ID, broker.Slug, project.ID, project.Slug, "projects/")

		_, err := s.GetProject(context.Background(), project.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("HybridBrokerClient control-channel and Conduit tunnels", func(t *testing.T) {
		runOverHubTunnels(t, func(t *testing.T, tr hubTunnelTransport) {
			srv, s, project, broker := setupProjectWithBroker(t, "cc-"+tr.name, "http://unreachable-broker.invalid:9800")
			tunnel := &mockControlChannelTunnel{
				connected: true,
				status:    http.StatusConflict,
				body:      []byte(`{"error":{"code":"conflict","message":"kept at /srv/scion/projects/secret"}}`),
			}
			fallbackHTTP := &countingBrokerClient{}
			hybrid := &HybridBrokerClient{
				controlChannel: &ControlChannelBrokerClient{manager: tr.wrap(t, tunnel)},
				httpClient:     fallbackHTTP,
			}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, hybrid, false, slog.Default()))

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
			requireBrokerDirKeptBody(t, rec)
			assert.Zero(t, fallbackHTTP.count(), "cleanup must route over the control-channel tunnel, not HTTP")
			require.NotNil(t, tunnel.lastRequest)
			assert.Equal(t, http.MethodDelete, tunnel.lastRequest.Method)
			assert.Equal(t, "/api/v1/projects/"+project.Slug+"?project_id="+project.ID, tunnel.lastRequest.Path)
			assertNoLeaks(t, rec.Body.String(),
				"/srv/scion", broker.Endpoint, broker.ID, broker.Slug, project.ID, project.Slug, "projects/")

			_, err := s.GetProject(context.Background(), project.ID)
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	})

	t.Run("HybridBrokerClient routed Conduit RPC path", func(t *testing.T) {
		runOverHubTunnels(t, func(t *testing.T, tr hubTunnelTransport) {
			srv, s, project, broker := setupProjectWithBroker(t, "routed-"+tr.name, "http://unreachable-broker.invalid:9800")
			routedTunnel := &mockControlChannelTunnel{
				connected: true,
				status:    http.StatusConflict,
				body:      []byte(`{"error":{"code":"conflict","message":"kept by sibling flat instance"}}`),
			}
			disconnectedLocal := &mockControlChannelTunnel{connected: false}
			fallbackHTTP := &countingBrokerClient{}
			hybrid := &HybridBrokerClient{
				routed:         &ControlChannelBrokerClient{manager: tr.wrap(t, routedTunnel)},
				controlChannel: &ControlChannelBrokerClient{manager: tr.wrap(t, disconnectedLocal)},
				httpClient:     fallbackHTTP,
			}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, hybrid, false, slog.Default()))

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
			requireBrokerDirKeptBody(t, rec)
			assert.Zero(t, fallbackHTTP.count(), "cleanup must be handled by the routed Conduit client")
			require.NotNil(t, routedTunnel.lastRequest)
			assertNoLeaks(t, rec.Body.String(), broker.Endpoint, broker.ID, project.ID, project.Slug)
		})
	})

	t.Run("multiple brokers returning 409 emit a single storageKept item", func(t *testing.T) {
		srv, s, project, _ := setupProjectWithBroker(t, "multi", "http://broker1:9800")
		ctx := context.Background()
		broker2 := &store.RuntimeBroker{
			ID:       tid("broker-kept-multi-2"),
			Slug:     "broker-kept-slug-multi-2",
			Name:     "Broker Kept Multi 2",
			Status:   store.BrokerStatusOnline,
			Endpoint: "http://broker2:9800",
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker2))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker2.ID,
			BrokerName: broker2.Name,
			LinkedBy:   "test",
		}))
		mockClient := &mockRuntimeBrokerClient{cleanupErr: &brokerStatusError{
			StatusCode: http.StatusConflict,
			Body:       `{"error":{"code":"conflict","message":"kept"}}`,
		}}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default()))

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
		requireBrokerDirKeptBody(t, rec)
		assert.Equal(t, 2, mockClient.cleanupCalls)
	})
}
