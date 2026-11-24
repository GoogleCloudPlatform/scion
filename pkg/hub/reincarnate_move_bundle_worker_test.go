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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httptestMoveBundleStorage is a fake GCS-provider storage backed by an
// httptest.Server for T11 worker integration tests.
type httptestMoveBundleStorage struct {
	bucket string
	ts     *httptest.Server

	mu                sync.Mutex
	objects           map[string][]byte
	deletePrefixCalls []string
	deletePrefixHook  func(prefix string)
}

func newHTTPTestMoveBundleStorage(t *testing.T) *httptestMoveBundleStorage {
	t.Helper()
	st := &httptestMoveBundleStorage{
		bucket:  "mb-worker-bucket",
		objects: make(map[string][]byte),
	}
	st.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/obj/"))
		if err != nil || key == "" {
			http.Error(w, "bad key", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read failed", http.StatusInternalServerError)
				return
			}
			st.mu.Lock()
			st.objects[key] = body
			st.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			st.mu.Lock()
			data, ok := st.objects[key]
			st.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(st.ts.Close)
	return st
}

func (s *httptestMoveBundleStorage) Bucket() string             { return s.bucket }
func (s *httptestMoveBundleStorage) Provider() storage.Provider { return storage.ProviderGCS }
func (s *httptestMoveBundleStorage) Close() error               { return nil }

func (s *httptestMoveBundleStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	u := s.ts.URL + "/obj/" + url.PathEscape(objectPath) + "?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Signature=test-secret-sig"
	headers := map[string]string{}
	if opts.Method == http.MethodPut && opts.ContentType != "" {
		headers["Content-Type"] = opts.ContentType
	}
	return &storage.SignedURL{
		URL:     u,
		Method:  opts.Method,
		Expires: time.Now().Add(opts.Expires),
		Headers: headers,
	}, nil
}

func (s *httptestMoveBundleStorage) Upload(_ context.Context, objectPath string, reader io.Reader, _ storage.UploadOptions) (*storage.Object, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.objects[objectPath] = data
	s.mu.Unlock()
	return &storage.Object{Name: objectPath, Size: int64(len(data))}, nil
}

func (s *httptestMoveBundleStorage) Download(_ context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	s.mu.Lock()
	data, ok := s.objects[objectPath]
	s.mu.Unlock()
	if !ok {
		return nil, nil, storage.ErrNotFound
	}
	cp := append([]byte(nil), data...)
	return io.NopCloser(bytes.NewReader(cp)), &storage.Object{Name: objectPath, Size: int64(len(cp))}, nil
}

func (s *httptestMoveBundleStorage) Delete(_ context.Context, objectPath string) error {
	s.mu.Lock()
	delete(s.objects, objectPath)
	s.mu.Unlock()
	return nil
}

func (s *httptestMoveBundleStorage) DeletePrefix(_ context.Context, prefix string) error {
	s.mu.Lock()
	hook := s.deletePrefixHook
	s.deletePrefixCalls = append(s.deletePrefixCalls, prefix)
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			delete(s.objects, k)
		}
	}
	s.mu.Unlock()
	if hook != nil {
		hook(prefix)
	}
	return nil
}

func (s *httptestMoveBundleStorage) List(_ context.Context, opts storage.ListOptions) (*storage.ListResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var objs []storage.Object
	for k, v := range s.objects {
		if strings.HasPrefix(k, opts.Prefix) {
			objs = append(objs, storage.Object{Name: k, Size: int64(len(v))})
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	return &storage.ListResult{Objects: objs}, nil
}

func (s *httptestMoveBundleStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[objectPath]
	return ok, nil
}

func (s *httptestMoveBundleStorage) GetObject(_ context.Context, objectPath string) (*storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[objectPath]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.Object{Name: objectPath, Size: int64(len(data))}, nil
}

func (s *httptestMoveBundleStorage) Copy(_ context.Context, srcPath, dstPath string) (*storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[srcPath]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := append([]byte(nil), data...)
	s.objects[dstPath] = cp
	return &storage.Object{Name: dstPath, Size: int64(len(cp))}, nil
}

func (s *httptestMoveBundleStorage) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *httptestMoveBundleStorage) deletePrefixes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletePrefixCalls...)
}

type bundleMoveWorkerFixture struct {
	*moveFixture
	stor *httptestMoveBundleStorage
}

func setMoveBundleExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	cur := srv.experimentRegistry()
	var active []experiments.Experiment
	for _, e := range cur.All() {
		if e.Name == experiments.AgentMoveBundle {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, cur.Retired())
	require.NoError(t, err)
	srv.setExperimentRegistry(reg)
	require.Equal(t, on, srv.experimentEnabled(experiments.AgentMoveBundle))
}

func setupBundleMoveWorkerFixture(t *testing.T) *bundleMoveWorkerFixture {
	t.Helper()
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.Capabilities = &store.BrokerCapabilities{
			Reprovision:     true,
			AgentMove:       true,
			AgentMoveBundle: true,
		}
		// Break same-export so the eligibility ladder selects bundle mode.
		dst.WorkspaceStorage = &api.BrokerWorkspaceStorage{
			Backend: api.WorkspaceStorageBackendLocal,
		}
		dst.Profiles = []store.BrokerProfile{{Name: "docker", Type: "docker", Available: true}}
		dst.DefaultProfile = "docker"
	})
	ctx := context.Background()

	// Update source broker to Docker + AgentMoveBundle (local workspace).
	src, err := f.s.GetRuntimeBroker(ctx, f.src.ID)
	require.NoError(t, err)
	src.Capabilities = &store.BrokerCapabilities{
		Reprovision:     true,
		AgentMove:       true,
		AgentMoveBundle: true,
	}
	src.WorkspaceStorage = &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendLocal,
	}
	src.Profiles = []store.BrokerProfile{{Name: "docker", Type: "docker", Available: true}}
	src.DefaultProfile = "docker"
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, src))
	f.src = src

	// Place agent on source with local workspace and docker runtime/profile.
	require.NoError(t, f.s.SetAgentWorkspacePlacement(ctx, f.agent.ID, api.WorkspacePlacementLocal))
	_, err = f.s.SetAgentRunID(ctx, f.agent.ID, "run-src", nil)
	require.NoError(t, err)
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.Runtime = "docker"
	if a.AppliedConfig != nil {
		a.AppliedConfig.Profile = "docker"
	}
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	f.agent = a

	reserveBrokerSlot(t, f.s, f.src, f.agent.ID)
	require.NoError(t, f.s.UpdateAgentExposedPorts(ctx, f.agent.ID, []store.ExposedPort{{Port: 8080, Label: "web", Mode: "http"}}))

	// Configure GCS-like httptest storage, enable hub.agent_move_bundle, and
	// shorten poll/touch intervals for fast deterministic tests.
	stor := newHTTPTestMoveBundleStorage(t)
	f.srv.SetStorage(stor)
	setMoveBundleExperiment(t, f.srv, true)
	f.srv.moveBundlePollInterval = 10 * time.Millisecond
	f.srv.moveBundleTouchInterval = 30 * time.Millisecond

	// Configure dispatcher to track target files and runs.
	f.disp.mu.Lock()
	f.disp.runStore = f.s
	f.disp.startPlacement = api.WorkspacePlacementLocal
	f.disp.recreateFiles = map[string]bool{}
	f.disp.mu.Unlock()

	bf := &bundleMoveWorkerFixture{moveFixture: f, stor: stor}
	bf.installDefaultBundleHooks(t)
	return bf
}

func bundlePartHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// installDefaultBundleHooks configures precheck, export (uploading real parts
// through signed PUT URLs to the httptest storage), and import (verifying
// manifest + hash and reading parts via signed GET URLs).
func (bf *bundleMoveWorkerFixture) installDefaultBundleHooks(t *testing.T) {
	t.Helper()
	bf.disp.mu.Lock()
	defer bf.disp.mu.Unlock()

	var exportManifestMu sync.Mutex
	exportManifests := map[string]*bundle.Manifest{}

	bf.disp.moveBundlePrecheckFn = func(_ context.Context, _ *store.Agent, _ api.MoveBundleLimits, _ []string) (*api.MoveBundlePrecheckResponse, error) {
		return &api.MoveBundlePrecheckResponse{
			Durable:   true,
			Workspace: api.MoveBundleRootEstimate{Bytes: 4096, Entries: 12},
			Home:      api.MoveBundleRootEstimate{Bytes: 2048, Entries: 6},
		}, nil
	}

	bf.disp.moveBundleExportFn = func(ctx context.Context, _ *store.Agent, rid string, limits api.MoveBundleLimits, _ []string) (*api.MoveBundleJobStatus, error) {
		wsData := []byte("workspace-tar-gz-part-0-bytes")
		homeData := []byte("home-tar-gz-part-0-bytes")

		// Upload both parts via signed PUT URLs against httptestMoveBundleStorage.
		for _, pair := range []struct {
			root string
			data []byte
		}{
			{storage.MoveBundleRootWorkspace, wsData},
			{storage.MoveBundleRootHome, homeData},
		} {
			partKey, err := storage.MoveBundlePartPath(bf.srv.HubID(), rid, pair.root, 0)
			if err != nil {
				return nil, err
			}
			signed, err := bf.stor.GenerateSignedURL(ctx, partKey, storage.SignedURLOptions{
				Method:      http.MethodPut,
				Expires:     SignedURLExpiry,
				ContentType: "application/octet-stream",
			})
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, signed.URL, bytes.NewReader(pair.data))
			if err != nil {
				return nil, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("signed PUT returned %d", resp.StatusCode)
			}
		}

		m := &bundle.Manifest{
			Version:         bundle.ManifestVersion,
			ReincarnationID: rid,
			Roots: map[string]bundle.RootManifest{
				bundle.RootWorkspace: {
					Parts:    []bundle.Part{{Index: 0, Size: int64(len(wsData)), Hash: bundlePartHash(wsData)}},
					PartSize: limits.PartSize,
					Entries:  12,
					Bytes:    4096,
				},
				bundle.RootHome: {
					Parts:    []bundle.Part{{Index: 0, Size: int64(len(homeData)), Hash: bundlePartHash(homeData)}},
					PartSize: limits.PartSize,
					Entries:  6,
					Bytes:    2048,
				},
			},
		}
		exportManifestMu.Lock()
		exportManifests[rid] = m
		exportManifestMu.Unlock()

		return &api.MoveBundleJobStatus{
			ReincarnationID: rid,
			Kind:            api.MoveBundleJobKindExport,
			State:           api.MoveBundleJobStateRunning,
			Progress:        api.MoveBundleProgress{BytesDone: 1024, BytesTotal: 6144, PartsDone: 1, PartsTotal: 2},
		}, nil
	}

	bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
		exportManifestMu.Lock()
		m := exportManifests[rid]
		exportManifestMu.Unlock()
		return &api.MoveBundleJobStatus{
			ReincarnationID: rid,
			Kind:            api.MoveBundleJobKindExport,
			State:           api.MoveBundleJobStateSucceeded,
			Progress:        api.MoveBundleProgress{BytesDone: 6144, BytesTotal: 6144, PartsDone: 2, PartsTotal: 2},
			Manifest:        m,
		}, nil
	}

	bf.disp.moveBundleImportFn = func(_ context.Context, _ *store.Agent, rid string, manifest bundle.Manifest, manifestHash string, _ api.MoveBundleLimits) (*api.MoveBundleJobStatus, error) {
		_, computedHash, err := bundle.CanonicalManifest(manifest)
		if err != nil {
			return nil, err
		}
		if computedHash != manifestHash {
			return nil, fmt.Errorf("manifestHash mismatch: got %s, want %s", manifestHash, computedHash)
		}
		return &api.MoveBundleJobStatus{
			ReincarnationID: rid,
			Kind:            api.MoveBundleJobKindImport,
			State:           api.MoveBundleJobStateRunning,
			Progress:        api.MoveBundleProgress{BytesDone: 2048, BytesTotal: 6144, PartsDone: 1, PartsTotal: 2},
		}, nil
	}

	bf.disp.moveBundleImportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
		return &api.MoveBundleJobStatus{
			ReincarnationID: rid,
			Kind:            api.MoveBundleJobKindImport,
			State:           api.MoveBundleJobStateSucceeded,
			Progress:        api.MoveBundleProgress{BytesDone: 6144, BytesTotal: 6144, PartsDone: 2, PartsTotal: 2},
		}, nil
	}
}

func waitForBundleMoveSettled(t *testing.T, s store.Store, agentID string) (*store.AgentReincarnation, *store.Agent) {
	t.Helper()
	r := waitForReincarnationSettled(t, s, agentID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, err := s.GetAgentReincarnation(context.Background(), r.ID)
		if err == nil && cur.SourceCleanup != "" {
			a, aerr := s.GetAgent(context.Background(), agentID)
			require.NoError(t, aerr)
			return cur, a
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for source cleanup outcome on %s (state=%q err=%q)", r.ID, r.State, r.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForBundleStagingDeleted(t *testing.T, st *httptestMoveBundleStorage, wantPrefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, p := range st.deletePrefixes() {
			if p == wantPrefix {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for DeletePrefix(%q); got %v", wantPrefix, st.deletePrefixes())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startBundleMove(t *testing.T, bf *bundleMoveWorkerFixture) (*store.AgentReincarnation, *store.Agent) {
	t.Helper()
	rec := bf.reincarnate(t, ReincarnateAgentRequest{Handoff: "bundle handoff", TargetBroker: bf.dst.ID})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, MoveTransferModeBundle, resp.TransferMode)
	return waitForBundleMoveSettled(t, bf.s, bf.agent.ID)
}

func expectedBundlePrefix(t *testing.T, hubID, rid string) string {
	t.Helper()
	base, err := storage.MoveBundlePath(hubID, rid)
	require.NoError(t, err)
	return storage.DirPrefix(base)
}

// T11.1: Happy path → completed, source cleanup done, staging prefix deleted,
// manifest persisted & verified, agent message cleared, no recreate sentence.
func TestMoveBundleWorker_HappyPath(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)

	var seenManifestHash string
	var seenManifestInStorage bool
	origImport := bf.disp.moveBundleImportFn
	bf.disp.mu.Lock()
	bf.disp.moveBundleImportFn = func(ctx context.Context, a *store.Agent, rid string, manifest bundle.Manifest, manifestHash string, limits api.MoveBundleLimits) (*api.MoveBundleJobStatus, error) {
		seenManifestHash = manifestHash
		manifestKey, err := storage.MoveBundleManifestPath(bf.srv.HubID(), rid)
		if err == nil {
			if ok, _ := bf.stor.Exists(ctx, manifestKey); ok {
				seenManifestInStorage = true
			}
		}
		return origImport(ctx, a, rid, manifest, manifestHash, limits)
	}
	bf.disp.mu.Unlock()

	r, a := startBundleMove(t, bf)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.Equal(t, store.TransferModeBundle, r.TransferMode)
	assert.Equal(t, store.SourceCleanupDone, r.SourceCleanup)
	assert.Equal(t, bf.dst.ID, a.RuntimeBrokerID)
	assert.Equal(t, 2, a.Generation)
	assert.Empty(t, a.Message, "progress message must be cleared on completion")
	assert.True(t, seenManifestInStorage, "Hub must persist manifest.json before import")
	assert.True(t, strings.HasPrefix(seenManifestHash, "sha256:"), "manifestHash must be sha256:<hex>")

	waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
	assert.Equal(t, 0, bf.stor.objectCount(), "all staging objects must be deleted on completion")

	provisions, localDeletes, starts := bf.disp.moveSnapshot()
	assert.Equal(t, []string{bf.dst.ID + "|fresh-agent-dir"}, provisions)
	assert.Equal(t, []string{bf.dst.ID}, starts)
	assert.Equal(t, []string{bf.src.ID + "|run-src"}, localDeletes)
	assert.Empty(t, bf.disp.recreatedDeletes())

	bf.disp.mu.Lock()
	task := bf.disp.lastStartTask
	bf.disp.mu.Unlock()
	assert.NotContains(t, task, "recreated workspace", "recreatedWorkspaceFor stays recreate-only")
	assert.Contains(t, task, "bundle handoff")
	assert.EqualValues(t, 0, brokerReservationCount(t, bf.s, bf.src.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, bf.s, bf.dst.ID))
}

// T11.2: Precheck over-limit in worker recheck → failed from pending with
// "bundle exceeds limit:" prefix, agent never stopped, untouched on source.
func TestMoveBundleWorker_PrecheckOverLimitFailsBeforeStop(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)

	var precheckCalls atomic.Int32
	bf.disp.mu.Lock()
	bf.disp.moveBundlePrecheckFn = func(_ context.Context, _ *store.Agent, _ api.MoveBundleLimits, _ []string) (*api.MoveBundlePrecheckResponse, error) {
		if precheckCalls.Add(1) == 1 {
			// Plan-time check succeeds.
			return &api.MoveBundlePrecheckResponse{
				Durable:   true,
				Workspace: api.MoveBundleRootEstimate{Bytes: 1024, Entries: 4},
			}, nil
		}
		// Worker recheck sees workspace grew over limit before stop.
		return &api.MoveBundlePrecheckResponse{
			Durable:   true,
			Over:      "workspace.bytes",
			Workspace: api.MoveBundleRootEstimate{Bytes: 5 << 30, Entries: 100, Over: "bytes"},
		}, nil
	}
	bf.disp.mu.Unlock()

	r, a := startBundleMove(t, bf)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.True(t, strings.HasPrefix(r.Error, "bundle exceeds limit:"), "got %q", r.Error)
	assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
	assert.Equal(t, 1, a.Generation)

	bf.disp.mu.Lock()
	assert.Equal(t, 0, bf.disp.stopCalls, "agent must never be stopped when precheck fails")
	bf.disp.mu.Unlock()
	provisions, localDeletes, starts := bf.disp.moveSnapshot()
	assert.Empty(t, provisions)
	assert.Empty(t, localDeletes)
	assert.Empty(t, starts)
}

// T11.3: Export failure, timeout, and 404 → failed from stopping, agent stays
// on source with design §3.6 message, URLs redacted, staging prefix deleted,
// no target calls.
func TestMoveBundleWorker_ExportFailures(t *testing.T) {
	const leakedURL = "https://storage.googleapis.com/mb-bucket/part-00000.tgz?X-Goog-Signature=supersecret"

	cases := []struct {
		name       string
		setup      func(bf *bundleMoveWorkerFixture)
		wantPrefix string
	}{
		{
			name: "job failed with URL in broker error",
			setup: func(bf *bundleMoveWorkerFixture) {
				bf.disp.mu.Lock()
				bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
					return &api.MoveBundleJobStatus{
						ReincarnationID: rid,
						Kind:            api.MoveBundleJobKindExport,
						State:           api.MoveBundleJobStateFailed,
						Error:           "PUT " + leakedURL + " failed: 503",
					}, nil
				}
				bf.disp.mu.Unlock()
			},
			wantPrefix: "bundle export failed:",
		},
		{
			name: "export timeout",
			setup: func(bf *bundleMoveWorkerFixture) {
				bf.srv.config.MoveBundle.ExportTimeout = time.Second
				// Shorten poll interval so we hit deadline quickly.
				bf.srv.moveBundlePollInterval = 15 * time.Millisecond
				bf.disp.mu.Lock()
				bf.disp.moveBundleExportStatusFn = func(ctx context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
					// Simulate immediate deadline exceeded on status poll.
					return nil, context.DeadlineExceeded
				}
				bf.disp.mu.Unlock()
			},
			wantPrefix: "bundle export failed:",
		},
		{
			name: "export 404 broker restarted",
			setup: func(bf *bundleMoveWorkerFixture) {
				bf.disp.mu.Lock()
				bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, _ string) (*api.MoveBundleJobStatus, error) {
					return nil, &brokerStatusError{StatusCode: http.StatusNotFound, Body: `{"error":{"code":"not_found","message":"job not found"}}`}
				}
				bf.disp.mu.Unlock()
			},
			wantPrefix: "bundle export failed:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bf := setupBundleMoveWorkerFixture(t)
			tc.setup(bf)

			r, a := startBundleMove(t, bf)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)
			assert.True(t, strings.HasPrefix(r.Error, tc.wantPrefix), "got %q", r.Error)
			assert.NotContains(t, r.Error, "X-Goog-Signature", "signed URL must be redacted from record error")
			assert.NotContains(t, r.Error, "supersecret")

			assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
			assert.Equal(t, "error", a.Phase)
			assert.Contains(t, a.Message, "move failed: ")
			assert.Contains(t, a.Message, "agent state is intact on "+bf.src.Name)
			assert.Contains(t, a.Message, "`scion start` to resume")
			assert.NotContains(t, a.Message, "X-Goog-Signature", "signed URL must be redacted from agent message")

			waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
			assert.Equal(t, 0, bf.stor.objectCount(), "staging objects must be deleted on export failure")

			provisions, localDeletes, starts := bf.disp.moveSnapshot()
			assert.Empty(t, provisions, "no target provision call on export failure")
			assert.Empty(t, localDeletes)
			assert.Empty(t, starts)
			assert.Empty(t, bf.disp.recreatedDeletes())
		})
	}
}

// T11.4: Manifest verify mismatch (storage object size differs from Part.Size)
// → failed from stopping, agent stays on source, staging deleted, no target call.
func TestMoveBundleWorker_ManifestVerifySizeMismatch(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)

	origStatus := bf.disp.moveBundleExportStatusFn
	bf.disp.mu.Lock()
	bf.disp.moveBundleExportStatusFn = func(ctx context.Context, a *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
		st, err := origStatus(ctx, a, rid)
		if err != nil {
			return nil, err
		}
		// Corrupt the workspace part size in the manifest so it disagrees with storage.
		rm := st.Manifest.Roots[bundle.RootWorkspace]
		rm.Parts[0].Size += 123
		st.Manifest.Roots[bundle.RootWorkspace] = rm
		return st, nil
	}
	bf.disp.mu.Unlock()

	r, a := startBundleMove(t, bf)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.True(t, strings.HasPrefix(r.Error, "bundle export failed:"), "got %q", r.Error)
	assert.Contains(t, r.Error, "size mismatch")
	assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
	assert.Contains(t, a.Message, "agent state is intact on "+bf.src.Name)

	waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
	assert.Equal(t, 0, bf.stor.objectCount())
	provisions, _, starts := bf.disp.moveSnapshot()
	assert.Empty(t, provisions)
	assert.Empty(t, starts)
}

// T11.5: Import failure, timeout, and 404 → rollback with marker-guarded
// DispatchAgentDeleteRecreated, RuntimeBrokerID restored, staging prefix deleted.
func TestMoveBundleWorker_ImportFailuresRollBack(t *testing.T) {
	const leakedURL = "https://storage.googleapis.com/mb-bucket/home/part-00000.tgz?X-Goog-Signature=importsecret"

	cases := []struct {
		name  string
		setup func(bf *bundleMoveWorkerFixture)
	}{
		{
			name: "import job failed with URL in error",
			setup: func(bf *bundleMoveWorkerFixture) {
				bf.disp.mu.Lock()
				bf.disp.moveBundleImportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
					return &api.MoveBundleJobStatus{
						ReincarnationID: rid,
						Kind:            api.MoveBundleJobKindImport,
						State:           api.MoveBundleJobStateFailed,
						Error:           "GET " + leakedURL + " hash mismatch",
					}, nil
				}
				bf.disp.mu.Unlock()
			},
		},
		{
			name: "import timeout",
			setup: func(bf *bundleMoveWorkerFixture) {
				bf.disp.mu.Lock()
				bf.disp.moveBundleImportStatusFn = func(_ context.Context, _ *store.Agent, _ string) (*api.MoveBundleJobStatus, error) {
					return nil, context.DeadlineExceeded
				}
				bf.disp.mu.Unlock()
			},
		},
		{
			name: "import 404 broker restarted",
			setup: func(bf *bundleMoveWorkerFixture) {
				bf.disp.mu.Lock()
				bf.disp.moveBundleImportStatusFn = func(_ context.Context, _ *store.Agent, _ string) (*api.MoveBundleJobStatus, error) {
					return nil, &brokerStatusError{StatusCode: http.StatusNotFound, Body: `{"error":{"code":"not_found","message":"import job lost"}}`}
				}
				bf.disp.mu.Unlock()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bf := setupBundleMoveWorkerFixture(t)
			tc.setup(bf)

			r, a := startBundleMove(t, bf)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)
			assert.True(t, strings.HasPrefix(r.Error, "bundle import failed:"), "got %q", r.Error)
			assert.NotContains(t, r.Error, "X-Goog-Signature")
			assert.NotContains(t, r.Error, "importsecret")

			assert.Equal(t, bf.src.ID, a.RuntimeBrokerID, "agent must be restored to source broker")
			assert.Equal(t, "docker", a.Runtime, "source runtime restored")
			assert.Equal(t, "run-src", a.RunID, "source run restored")
			assert.Equal(t, 1, a.Generation)
			assert.EqualValues(t, 1, brokerReservationCount(t, bf.s, bf.src.ID))
			assert.EqualValues(t, 0, brokerReservationCount(t, bf.s, bf.dst.ID))

			assert.Equal(t, []string{bf.dst.ID + "|" + bf.project.ID + "|"}, bf.disp.recreatedDeletes(),
				"marker-guarded DispatchAgentDeleteRecreated must be sent to target")
			bf.disp.mu.Lock()
			assert.False(t, bf.disp.recreateFiles[bf.dst.ID], "target files removed by rollback")
			bf.disp.mu.Unlock()

			waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
			assert.Equal(t, 0, bf.stor.objectCount())
		})
	}
}

// T11.6: Definitive start failure → rollback with DispatchAgentDeleteRecreated,
// RuntimeBrokerID restored, staging prefix deleted.
func TestMoveBundleWorker_DefinitiveStartFailureRollsBack(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)
	bf.disp.mu.Lock()
	bf.disp.startErr = &brokerStatusError{StatusCode: http.StatusBadRequest, Body: `{"error":{"code":"bad_request","message":"invalid container spec"}}`}
	bf.disp.mu.Unlock()

	r, a := startBundleMove(t, bf)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
	assert.Equal(t, "run-src", a.RunID)
	assert.Equal(t, 1, a.Generation)
	assert.Len(t, bf.disp.recreatedDeletes(), 1, "definitive start failure rolls back target with DeleteRecreated")
	waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
	assert.Equal(t, 0, bf.stor.objectCount())
}

// T11.7: Ambiguous start failure → stays on target, source untouched, staging
// blobs kept until record is terminal and then deleted.
func TestMoveBundleWorker_AmbiguousStartStaysOnTargetAndCleansUpAfterTerminal(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)
	bf.disp.mu.Lock()
	bf.disp.startErr = errors.New("connection reset by peer")
	bf.disp.mu.Unlock()

	var stateAtDelete atomic.Value
	bf.stor.mu.Lock()
	bf.stor.deletePrefixHook = func(prefix string) {
		recs, err := bf.s.ListAgentReincarnations(context.Background(), bf.agent.ID)
		if err == nil && len(recs) > 0 {
			stateAtDelete.Store(recs[0].State)
		}
	}
	bf.stor.mu.Unlock()

	r, a := startBundleMove(t, bf)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Equal(t, sourceCleanupSkippedAmbiguousStart, r.SourceCleanup)
	assert.Equal(t, bf.dst.ID, a.RuntimeBrokerID, "ambiguous start stays on target")
	assert.Empty(t, bf.disp.recreatedDeletes(), "ambiguous start must not delete target")
	_, localDeletes, _ := bf.disp.moveSnapshot()
	assert.Empty(t, localDeletes, "ambiguous start must leave source untouched")

	waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
	assert.Equal(t, store.AgentReincarnationStateFailed, stateAtDelete.Load(),
		"staging DeletePrefix must run only after record is terminal")
	assert.Equal(t, 0, bf.stor.objectCount())
}

// T11.8a: Stale sweep vs touch — while export is polling and touching the
// record, a sweep whose cutoff predates the touch does not sweep the record,
// and the move completes normally.
func TestMoveBundleWorker_StaleSweepRespectsTouch(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)
	bf.srv.moveBundlePollInterval = 15 * time.Millisecond
	bf.srv.moveBundleTouchInterval = 25 * time.Millisecond

	origStatus := bf.disp.moveBundleExportStatusFn
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var polls atomic.Int32

	bf.disp.mu.Lock()
	bf.disp.moveBundleExportStatusFn = func(ctx context.Context, a *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
		n := polls.Add(1)
		if n == 1 {
			startOnce.Do(func() { close(started) })
		}
		select {
		case <-release:
			return origStatus(ctx, a, rid)
		default:
			return &api.MoveBundleJobStatus{
				ReincarnationID: rid,
				Kind:            api.MoveBundleJobKindExport,
				State:           api.MoveBundleJobStateRunning,
				Progress:        api.MoveBundleProgress{BytesDone: int64(n) * 512, BytesTotal: 6144},
			}, nil
		}
	}
	bf.disp.mu.Unlock()

	rec := bf.reincarnate(t, ReincarnateAgentRequest{Handoff: "slow export", TargetBroker: bf.dst.ID})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	<-started
	recs, err := bf.s.ListAgentReincarnations(context.Background(), bf.agent.ID)
	require.NoError(t, err)
	require.NotEmpty(t, recs)
	rid := recs[0].ID
	initialUpdatedAt := recs[0].UpdatedAt

	// Wait until touchReincarnation bumps UpdatedAt strictly past initialUpdatedAt.
	deadline := time.Now().Add(3 * time.Second)
	var touchedAt time.Time
	for {
		cur, err := bf.s.GetAgentReincarnation(context.Background(), rid)
		require.NoError(t, err)
		if cur.UpdatedAt.After(initialUpdatedAt) {
			touchedAt = cur.UpdatedAt
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for touchReincarnation to bump UpdatedAt (initial=%v)", initialUpdatedAt)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Sweep with cutoff slightly after initialUpdatedAt (so an untouched record
	// would be swept), but before touchedAt: the touched record must survive!
	cutoff := initialUpdatedAt.Add(time.Millisecond)
	require.True(t, touchedAt.After(cutoff), "touchedAt=%v must be after cutoff=%v", touchedAt, cutoff)
	swept, err := bf.srv.sweepStaleReincarnationsOlderThan(context.Background(), cutoff)
	require.NoError(t, err)
	assert.Equal(t, 0, swept, "actively touched bundle move must not be swept")

	close(release)
	r, a := waitForBundleMoveSettled(t, bf.s, bf.agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.Equal(t, bf.dst.ID, a.RuntimeBrokerID)
	assert.Equal(t, 2, a.Generation)
}

// T11.8b: Lost touch CAS — when another writer (the stale sweep) marks the
// record failed while the worker is polling export, touchReincarnation sees
// the lost CAS and the worker stops polling and returns without side effects.
func TestMoveBundleWorker_LostTouchCASStopsWorkerWithoutSideEffects(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)
	bf.srv.moveBundlePollInterval = 15 * time.Millisecond
	bf.srv.moveBundleTouchInterval = 25 * time.Millisecond

	started := make(chan struct{})
	var startOnce sync.Once
	var pollCount atomic.Int32

	bf.disp.mu.Lock()
	bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
		pollCount.Add(1)
		startOnce.Do(func() { close(started) })
		return &api.MoveBundleJobStatus{
			ReincarnationID: rid,
			Kind:            api.MoveBundleJobKindExport,
			State:           api.MoveBundleJobStateRunning,
		}, nil
	}
	bf.disp.mu.Unlock()

	rec := bf.reincarnate(t, ReincarnateAgentRequest{Handoff: "lost cas", TargetBroker: bf.dst.ID})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	<-started

	// Force the stale sweep to fail the record out from under the worker.
	swept, err := bf.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, swept)

	// Give the worker a couple of touch cycles to observe the lost CAS and exit.
	time.Sleep(120 * time.Millisecond)
	countAfterExit := pollCount.Load()
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, countAfterExit, pollCount.Load(), "worker must stop polling after lost touch CAS")

	recs, err := bf.s.ListAgentReincarnations(context.Background(), bf.agent.ID)
	require.NoError(t, err)
	require.NotEmpty(t, recs)
	assert.Equal(t, store.AgentReincarnationStateFailed, recs[0].State)
	assert.Contains(t, recs[0].Error, "hub restarted during reincarnation", "worker must not overwrite sweep's error")

	provisions, _, starts := bf.disp.moveSnapshot()
	assert.Empty(t, provisions, "no target provision after lost CAS")
	assert.Empty(t, starts, "no target start after lost CAS")
}

// T11.9: Mode changes between plan and worker recheck (e.g. experiment
// disabled after plan, or brokers become export-eligible) → refused before stop.
func TestMoveBundleWorker_ModeChangesBetweenPlanAndRecheck(t *testing.T) {
	t.Run("experiment disabled before worker recheck", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		bf.disp.mu.Lock()
		var calls atomic.Int32
		bf.disp.moveBundlePrecheckFn = func(_ context.Context, _ *store.Agent, _ api.MoveBundleLimits, _ []string) (*api.MoveBundlePrecheckResponse, error) {
			if calls.Add(1) == 1 {
				// Flip experiment off right after plan precheck succeeds, before worker runs.
				setMoveBundleExperiment(t, bf.srv, false)
			}
			return &api.MoveBundlePrecheckResponse{Durable: true}, nil
		}
		bf.disp.mu.Unlock()

		r, a := startBundleMove(t, bf)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.Contains(t, r.Error, "move no longer eligible")
		assert.Contains(t, r.Error, experiments.AgentMoveBundle)
		assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
		bf.disp.mu.Lock()
		assert.Equal(t, 0, bf.disp.stopCalls, "must fail before stop")
		bf.disp.mu.Unlock()
	})

	t.Run("mode switches from bundle to export before worker recheck", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		ctx := context.Background()
		// Start with k8s profiles and export placement, but src/dst on
		// DIFFERENT NFS exports so plan selects bundle.
		sharedNFS := moveFixtureStorage()
		otherNFS := &api.BrokerWorkspaceStorage{
			Backend: api.WorkspaceStorageBackendLocal,
		}
		k8sProfiles := []store.BrokerProfile{{Name: "docker", Type: "kubernetes", Available: true}}
		for _, pair := range []struct {
			id string
			ws *api.BrokerWorkspaceStorage
		}{
			{bf.src.ID, sharedNFS},
			{bf.dst.ID, otherNFS},
		} {
			b, err := bf.s.GetRuntimeBroker(ctx, pair.id)
			require.NoError(t, err)
			b.WorkspaceStorage = pair.ws
			b.Profiles = k8sProfiles
			require.NoError(t, bf.s.UpdateRuntimeBroker(ctx, b))
		}
		require.NoError(t, bf.s.SetAgentWorkspacePlacement(ctx, bf.agent.ID, api.WorkspacePlacementExport))
		a0, err := bf.s.GetAgent(ctx, bf.agent.ID)
		require.NoError(t, err)
		a0.Runtime = "kubernetes"
		require.NoError(t, bf.s.UpdateAgent(ctx, a0))

		bf.disp.mu.Lock()
		var calls atomic.Int32
		bf.disp.moveBundlePrecheckFn = func(pctx context.Context, _ *store.Agent, _ api.MoveBundleLimits, _ []string) (*api.MoveBundlePrecheckResponse, error) {
			if calls.Add(1) == 1 {
				// Update dst broker's NFS export to match src before the worker
				// recheck runs (without touching the agent row's state_version).
				b, err := bf.s.GetRuntimeBroker(pctx, bf.dst.ID)
				if err == nil {
					b.WorkspaceStorage = sharedNFS
					_ = bf.s.UpdateRuntimeBroker(pctx, b)
				}
			}
			return &api.MoveBundlePrecheckResponse{Durable: true}, nil
		}
		bf.disp.mu.Unlock()

		logs := &syncBuffer{}
		bf.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))

		r, a := startBundleMove(t, bf)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.Contains(t, r.Error, "transfer mode changed from bundle to export")
		assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
		bf.disp.mu.Lock()
		assert.Equal(t, 0, bf.disp.stopCalls, "must fail before stop")
		bf.disp.mu.Unlock()
	})

	t.Run("forced bundle move over limit preserves bundle reason", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		ctx := context.Background()

		// Make recreate eligible as fallback when bundle becomes unavailable.
		dst, err := bf.s.GetRuntimeBroker(ctx, bf.dst.ID)
		require.NoError(t, err)
		dst.Capabilities.AgentMoveRecreate = true
		require.NoError(t, bf.s.UpdateRuntimeBroker(ctx, dst))
		proj, err := bf.s.GetProject(ctx, bf.project.ID)
		require.NoError(t, err)
		proj.GitRemote = "https://example.com/forced-bundle-repo.git"
		require.NoError(t, bf.s.UpdateProject(ctx, proj))

		var precheckCalls atomic.Int32
		bf.disp.mu.Lock()
		bf.disp.moveBundlePrecheckFn = func(_ context.Context, _ *store.Agent, _ api.MoveBundleLimits, _ []string) (*api.MoveBundlePrecheckResponse, error) {
			if precheckCalls.Add(1) == 1 {
				return &api.MoveBundlePrecheckResponse{Durable: true}, nil
			}
			return &api.MoveBundlePrecheckResponse{
				Durable:   true,
				Over:      "workspace.bytes",
				Workspace: api.MoveBundleRootEstimate{Bytes: 5 << 30, Entries: 100, Over: "bytes"},
			}, nil
		}
		bf.disp.mu.Unlock()

		rec := bf.reincarnate(t, ReincarnateAgentRequest{Handoff: "forced bundle", TargetBroker: bf.dst.ID, ForceMove: true})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		var resp ReincarnateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Equal(t, MoveTransferModeBundle, resp.TransferMode)

		r, a := waitForBundleMoveSettled(t, bf.s, bf.agent.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.Contains(t, r.Error, "transfer mode changed from bundle to recreate")
		assert.Contains(t, r.Error, "bundle exceeds limit:")
		assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
		bf.disp.mu.Lock()
		assert.Equal(t, 0, bf.disp.stopCalls, "must fail before stop")
		bf.disp.mu.Unlock()
	})
}

// R1: When the stale sweep fails the record while status polls report changing
// progress, CAS-gated progress writes must detect the lost CAS and exit
// without resetting agent.ReincarnationState back to stopping/provisioning or
// overwriting the sweep's failure message.
func TestMoveBundleWorker_LostCASWithChangingProgressPreservesFailedAgent(t *testing.T) {
	t.Run("export phase (stopping)", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		bf.srv.moveBundlePollInterval = 10 * time.Millisecond
		bf.srv.moveBundleProgressInterval = 15 * time.Millisecond
		bf.srv.moveBundleTouchInterval = 500 * time.Millisecond

		started := make(chan struct{})
		var startOnce sync.Once
		var polls atomic.Int32

		bf.disp.mu.Lock()
		bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
			n := polls.Add(1)
			startOnce.Do(func() { close(started) })
			return &api.MoveBundleJobStatus{
				ReincarnationID: rid,
				Kind:            api.MoveBundleJobKindExport,
				State:           api.MoveBundleJobStateRunning,
				Progress:        api.MoveBundleProgress{BytesDone: int64(n) * (1 << 20), BytesTotal: 1 << 30},
			}, nil
		}
		bf.disp.mu.Unlock()

		rec := bf.reincarnate(t, ReincarnateAgentRequest{Handoff: "r1 export", TargetBroker: bf.dst.ID})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		<-started
		swept, err := bf.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, swept)

		// Wait for worker to observe lost CAS on next progress write and exit.
		time.Sleep(120 * time.Millisecond)
		afterExit := polls.Load()
		time.Sleep(60 * time.Millisecond)
		assert.Equal(t, afterExit, polls.Load(), "worker must stop polling after lost CAS")

		a, err := bf.s.GetAgent(context.Background(), bf.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ReincarnationStateFailed, a.ReincarnationState, "progress write must not reset agent back to stopping")
		assert.Equal(t, "error", a.Phase)
		assert.Contains(t, a.Message, "hub restarted during reincarnation", "sweep message must remain intact")
		assert.NotContains(t, a.Message, "migrating:")
	})

	t.Run("import phase (provisioning)", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		bf.srv.moveBundlePollInterval = 10 * time.Millisecond
		bf.srv.moveBundleProgressInterval = 15 * time.Millisecond
		bf.srv.moveBundleTouchInterval = 500 * time.Millisecond

		started := make(chan struct{})
		var startOnce sync.Once
		var polls atomic.Int32

		bf.disp.mu.Lock()
		bf.disp.moveBundleImportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
			n := polls.Add(1)
			startOnce.Do(func() { close(started) })
			return &api.MoveBundleJobStatus{
				ReincarnationID: rid,
				Kind:            api.MoveBundleJobKindImport,
				State:           api.MoveBundleJobStateRunning,
				Progress:        api.MoveBundleProgress{BytesDone: int64(n) * (1 << 20), BytesTotal: 1 << 30},
			}, nil
		}
		bf.disp.mu.Unlock()

		rec := bf.reincarnate(t, ReincarnateAgentRequest{Handoff: "r1 import", TargetBroker: bf.dst.ID})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		<-started
		swept, err := bf.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, swept)

		time.Sleep(120 * time.Millisecond)
		afterExit := polls.Load()
		time.Sleep(60 * time.Millisecond)
		assert.Equal(t, afterExit, polls.Load(), "worker must stop polling after lost CAS")

		a, err := bf.s.GetAgent(context.Background(), bf.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ReincarnationStateFailed, a.ReincarnationState, "progress write must not reset agent back to provisioning")
		assert.Equal(t, "error", a.Phase)
		assert.Contains(t, a.Message, "hub restarted during reincarnation", "sweep message must remain intact")
		assert.NotContains(t, a.Message, "migrating:")

		_, _, starts := bf.disp.moveSnapshot()
		assert.Empty(t, starts, "worker must not start target after lost CAS during import")
	})
}

// R2: Real hub-side poll timeout when broker keeps returning running forever
// while touchReincarnation keeps the record alive.
func TestMoveBundleWorker_HubPollTimeout(t *testing.T) {
	t.Run("export poll timeout", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		bf.srv.config.MoveBundle.ExportTimeout = time.Second
		bf.srv.moveBundlePollInterval = 20 * time.Millisecond
		bf.srv.moveBundleTouchInterval = 50 * time.Millisecond

		bf.disp.mu.Lock()
		bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
			return &api.MoveBundleJobStatus{
				ReincarnationID: rid,
				Kind:            api.MoveBundleJobKindExport,
				State:           api.MoveBundleJobStateRunning,
			}, nil
		}
		bf.disp.mu.Unlock()

		r, a := startBundleMove(t, bf)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.True(t, strings.HasPrefix(r.Error, "bundle export failed:"), "got %q", r.Error)
		assert.Contains(t, r.Error, "timed out after")
		assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
		assert.Contains(t, a.Message, "agent state is intact on "+bf.src.Name)

		waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
		assert.Equal(t, 0, bf.stor.objectCount())
		provisions, _, starts := bf.disp.moveSnapshot()
		assert.Empty(t, provisions)
		assert.Empty(t, starts)
	})

	t.Run("import poll timeout", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		bf.srv.config.MoveBundle.ImportTimeout = time.Second
		bf.srv.moveBundlePollInterval = 20 * time.Millisecond
		bf.srv.moveBundleTouchInterval = 50 * time.Millisecond

		bf.disp.mu.Lock()
		bf.disp.moveBundleImportStatusFn = func(_ context.Context, _ *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
			return &api.MoveBundleJobStatus{
				ReincarnationID: rid,
				Kind:            api.MoveBundleJobKindImport,
				State:           api.MoveBundleJobStateRunning,
			}, nil
		}
		bf.disp.mu.Unlock()

		r, a := startBundleMove(t, bf)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.True(t, strings.HasPrefix(r.Error, "bundle import failed:"), "got %q", r.Error)
		assert.Contains(t, r.Error, "timed out after")
		assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
		assert.Len(t, bf.disp.recreatedDeletes(), 1, "import poll timeout must roll back with DeleteRecreated")

		waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
		assert.Equal(t, 0, bf.stor.objectCount())
	})
}

// O2: Transient transport errors on status polls are tolerated up to
// moveBundleMaxConsecutivePollErrors; 404 stays immediately definitive.
func TestMoveBundleWorker_TransientPollErrors(t *testing.T) {
	t.Run("recovers after transient errors below limit", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		origStatus := bf.disp.moveBundleExportStatusFn
		var calls atomic.Int32
		bf.disp.mu.Lock()
		bf.disp.moveBundleExportStatusFn = func(ctx context.Context, a *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
			if calls.Add(1) < moveBundleMaxConsecutivePollErrors {
				return nil, errors.New("temporary control-channel reconnect")
			}
			return origStatus(ctx, a, rid)
		}
		bf.disp.mu.Unlock()

		r, a := startBundleMove(t, bf)
		require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
		assert.Equal(t, bf.dst.ID, a.RuntimeBrokerID)
		assert.Equal(t, int32(moveBundleMaxConsecutivePollErrors), calls.Load())
	})

	t.Run("fails after consecutive error limit reached", func(t *testing.T) {
		bf := setupBundleMoveWorkerFixture(t)
		var calls atomic.Int32
		bf.disp.mu.Lock()
		bf.disp.moveBundleExportStatusFn = func(_ context.Context, _ *store.Agent, _ string) (*api.MoveBundleJobStatus, error) {
			calls.Add(1)
			return nil, errors.New("persistent network blip")
		}
		bf.disp.mu.Unlock()

		r, a := startBundleMove(t, bf)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.True(t, strings.HasPrefix(r.Error, "bundle export failed:"), "got %q", r.Error)
		assert.Contains(t, r.Error, "persistent network blip")
		assert.Equal(t, int32(moveBundleMaxConsecutivePollErrors), calls.Load())
		assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
		waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
	})
}

// O4: Step-5 manifest verification requires both workspace and home roots.
func TestMoveBundleWorker_ManifestMissingRequiredRoot(t *testing.T) {
	bf := setupBundleMoveWorkerFixture(t)
	origStatus := bf.disp.moveBundleExportStatusFn
	bf.disp.mu.Lock()
	bf.disp.moveBundleExportStatusFn = func(ctx context.Context, a *store.Agent, rid string) (*api.MoveBundleJobStatus, error) {
		st, err := origStatus(ctx, a, rid)
		if err != nil {
			return nil, err
		}
		delete(st.Manifest.Roots, bundle.RootHome)
		return st, nil
	}
	bf.disp.mu.Unlock()

	r, a := startBundleMove(t, bf)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.True(t, strings.HasPrefix(r.Error, "bundle export failed:"), "got %q", r.Error)
	assert.Contains(t, r.Error, `manifest missing required root "home"`)
	assert.Equal(t, bf.src.ID, a.RuntimeBrokerID)
	waitForBundleStagingDeleted(t, bf.stor, expectedBundlePrefix(t, bf.srv.HubID(), r.ID))
	provisions, _, starts := bf.disp.moveSnapshot()
	assert.Empty(t, provisions)
	assert.Empty(t, starts)
}
