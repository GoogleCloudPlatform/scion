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

package runtimebroker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The first read creates the marker with a UUID; later reads, from the same
// broker or another one seeing the same directory, return that UUID.
func TestReadOrCreateExportID_CreatesOnceThenReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share", "projects")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	id, err := readOrCreateExportID(dir)
	require.NoError(t, err)
	_, err = uuid.Parse(id)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, api.ExportIDMarkerName))

	again, err := readOrCreateExportID(dir)
	require.NoError(t, err)
	require.Equal(t, id, again)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary files are left behind")
}

// Brokers racing to create the marker all end up with the same UUID.
func TestReadOrCreateExportID_ConcurrentCreatorsAgree(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = readOrCreateExportID(dir)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i])
	}
}

// A marker that does not hold a UUID is an error and is left untouched.
func TestReadOrCreateExportID_InvalidMarkerIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, api.ExportIDMarkerName)
	require.NoError(t, os.WriteFile(marker, []byte("not-a-uuid\n"), 0o644))

	_, err := readOrCreateExportID(dir)
	require.Error(t, err)
	got, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "not-a-uuid\n", string(got))
}

// The sub-path root is never created: a missing directory is an error and
// nothing is written (a mount that went away must not get a marker on the
// broker's local disk under the bare mountpoint).
func TestReadOrCreateExportID_MissingDirIsNotCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share", "projects")
	_, err := readOrCreateExportID(dir)
	require.Error(t, err)
	require.NoDirExists(t, dir)
	require.NoDirExists(t, filepath.Dir(dir))
}

// A last good ID is reported while a newer read is in flight (a concurrent
// descriptor build, or a hung mount), until it is older than the TTL; a
// failed read clears it.
func TestExportIDProbe_CachesLastGoodWhileInflight(t *testing.T) {
	const good = "22222222-2222-2222-2222-222222222222"
	now := time.Unix(1_000_000, 0)
	var release chan struct{}
	var fail atomic.Bool
	p := &exportIDProbe{
		now: func() time.Time { return now },
		read: func(string) (string, error) {
			if release != nil {
				<-release
			}
			if fail.Load() {
				return "", os.ErrPermission
			}
			return good, nil
		},
	}
	waitIdle := func() {
		require.Eventually(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return !p.inflight
		}, time.Second, time.Millisecond)
	}

	require.Equal(t, good, p.get("d", time.Second))

	release = make(chan struct{})
	require.Equal(t, good, p.get("d", 20*time.Millisecond), "a slow read reports the cached ID")
	require.Equal(t, good, p.get("d", time.Second), "a call during the in-flight read reports the cached ID")
	now = now.Add(exportIDCacheTTL)
	require.Equal(t, "", p.get("d", time.Second), "a cached ID older than the TTL is not reported")
	close(release)
	waitIdle()

	// Each good read refreshes the cache's age: a read just before the TTL
	// keeps the ID reportable for a further TTL.
	release = nil
	require.Equal(t, good, p.get("d", time.Second))
	waitIdle()
	now = now.Add(exportIDCacheTTL - time.Second)
	require.Equal(t, good, p.get("d", time.Second))
	waitIdle()
	now = now.Add(2 * time.Second)
	release = make(chan struct{})
	require.Equal(t, good, p.get("d", 20*time.Millisecond), "the refreshed ID is still reported past the first read's TTL")
	close(release)
	waitIdle()

	release = nil
	require.Equal(t, good, p.get("d", time.Second))
	fail.Store(true)
	require.Equal(t, "", p.get("d", time.Second))
	waitIdle()
	release = make(chan struct{})
	require.Equal(t, "", p.get("d", 20*time.Millisecond), "a failed read clears the cache")
	close(release)
	waitIdle()
}

// With nothing cached, a read that outlives the timeout reports "", and
// while it is still stuck later calls report "" at once without starting
// another read.
func TestExportIDProbe_TimeoutAndSingleInflight(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	p := &exportIDProbe{read: func(string) (string, error) {
		calls.Add(1)
		<-release
		return "11111111-1111-1111-1111-111111111111", nil
	}}

	require.Equal(t, "", p.get("d", 20*time.Millisecond))
	require.Equal(t, "", p.get("d", time.Second), "a stuck read is not joined or repeated")
	require.Equal(t, int32(1), calls.Load())

	close(release)
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !p.inflight
	}, time.Second, time.Millisecond)
	require.Equal(t, "11111111-1111-1111-1111-111111111111", p.get("d", time.Second))
	require.Equal(t, int32(2), calls.Load())
}

// healthyShareServer returns a server whose NFS config points at a temp
// mount root, with Shares[0] marked healthy or not by its reconciler.
func healthyShareServer(t *testing.T, healthy bool) (*Server, string) {
	t.Helper()
	cfg := twoShareNFSConfig()
	cfg.MountRoot = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cfg.MountRoot, "primary", "projects"), 0o755))
	s := &Server{config: ServerConfig{WorkspaceStorageBackend: "nfs", NFSConfig: cfg}}
	s.nfsMountReconciler = NewNFSMountReconciler(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.nfsMountReconciler.setStatus("primary", filepath.Join(cfg.MountRoot, "primary"), healthy, "test")
	return s, filepath.Join(cfg.MountRoot, "primary", "projects", api.ExportIDMarkerName)
}

// With Shares[0] healthy, the descriptor carries the export identity read
// from <mountRoot>/<Shares[0].ID>/<subPathRoot>/.scion-export-id.
func TestWorkspaceStorageDescriptor_ReportsExportIDFromShareZero(t *testing.T) {
	s, marker := healthyShareServer(t, true)
	desc := s.workspaceStorageDescriptor()
	require.NotNil(t, desc.NFS)
	require.True(t, desc.NFS.Healthy)
	require.NotEmpty(t, desc.NFS.ExportID)

	b, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, desc.NFS.ExportID+"\n", string(b))
	require.Equal(t, desc.NFS.ExportID, s.workspaceStorageDescriptor().NFS.ExportID)
}

// An unhealthy share reports no export identity and the broker does not
// touch the export.
func TestWorkspaceStorageDescriptor_UnhealthyShareNoExportID(t *testing.T) {
	s, marker := healthyShareServer(t, false)
	desc := s.workspaceStorageDescriptor()
	require.NotNil(t, desc.NFS)
	require.Empty(t, desc.NFS.ExportID)
	require.NoFileExists(t, marker)
}

// The heartbeat reports the broker's default profile when a provider is
// set (an empty name is reported as such), and omits it otherwise.
func TestHeartbeat_ReportsDefaultProfile(t *testing.T) {
	hb := NewHeartbeatService(&mockRuntimeBrokerService{}, "test-host", time.Hour, &mockManager{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Nil(t, hb.buildHeartbeat(context.Background()).DefaultProfile)

	name := "k8s"
	hb.defaultProfile = func() *string { return &name }
	got := hb.buildHeartbeat(context.Background()).DefaultProfile
	require.NotNil(t, got)
	require.Equal(t, "k8s", *got)

	empty := ""
	hb.defaultProfile = func() *string { return &empty }
	got = hb.buildHeartbeat(context.Background()).DefaultProfile
	require.NotNil(t, got)
	require.Equal(t, "", *got)

	// Unknown (the broker's settings failed to load): omitted, so the hub
	// keeps its stored value.
	hb.defaultProfile = func() *string { return nil }
	require.Nil(t, hb.buildHeartbeat(context.Background()).DefaultProfile)
}

// HubConnection.Start wires the configured default profile into the
// heartbeat.
func TestHubConnectionStart_HeartbeatReportsDefaultProfile(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)
	cfg := srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false
	gke := "gke"
	cfg.DefaultProfile = &gke
	srv.config = cfg

	srv.hubMu.RLock()
	conn, ok := srv.hubConnections["local"]
	srv.hubMu.RUnlock()
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, conn.Start(ctx, srv))

	conn.mu.RLock()
	hb := conn.Heartbeat
	conn.mu.RUnlock()
	require.NotNil(t, hb)
	got := hb.buildHeartbeat(ctx).DefaultProfile
	require.NotNil(t, got)
	require.Equal(t, "gke", *got)
}

// A start's workspace placement reaches the hub in the agent response.
func TestAgentInfoToResponse_CarriesWorkspacePlacement(t *testing.T) {
	resp := AgentInfoToResponse(api.AgentInfo{Name: "a", WorkspacePlacement: api.WorkspacePlacementExport})
	require.Equal(t, api.WorkspacePlacementExport, resp.WorkspacePlacement)
}
