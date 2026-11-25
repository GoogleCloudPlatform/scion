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
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orphanSweepFakeStorage is an in-memory storage.Storage fake that supports
// delimiter listing (returning child prefixes like GCS/LocalStorage),
// prefix deletion, and injecting raw/malformed prefixes for T10 tests.
type orphanSweepFakeStorage struct {
	mu                sync.Mutex
	objects           map[string][]byte
	extraPrefixes     []string
	ignoreMaxResults  bool
	deletePrefixCalls []string
	listCalls         []storage.ListOptions
}

func newOrphanSweepFakeStorage() *orphanSweepFakeStorage {
	return &orphanSweepFakeStorage{
		objects: make(map[string][]byte),
	}
}

func (f *orphanSweepFakeStorage) Bucket() string             { return "orphan-sweep-bucket" }
func (f *orphanSweepFakeStorage) Provider() storage.Provider { return storage.ProviderGCS }
func (f *orphanSweepFakeStorage) Close() error               { return nil }

func (f *orphanSweepFakeStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	return &storage.SignedURL{
		URL:     "https://storage.example.com/" + objectPath,
		Method:  opts.Method,
		Expires: time.Now().Add(opts.Expires),
	}, nil
}

func (f *orphanSweepFakeStorage) Upload(_ context.Context, objectPath string, r io.Reader, _ storage.UploadOptions) (*storage.Object, error) {
	var data []byte
	if r != nil {
		var err error
		data, err = io.ReadAll(r)
		if err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.objects[objectPath] = data
	f.mu.Unlock()
	return &storage.Object{Name: objectPath, Size: int64(len(data))}, nil
}

func (f *orphanSweepFakeStorage) Download(_ context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	f.mu.Lock()
	data, ok := f.objects[objectPath]
	f.mu.Unlock()
	if !ok {
		return nil, nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), &storage.Object{Name: objectPath, Size: int64(len(data))}, nil
}

func (f *orphanSweepFakeStorage) Delete(_ context.Context, objectPath string) error {
	f.mu.Lock()
	delete(f.objects, objectPath)
	f.mu.Unlock()
	return nil
}

func (f *orphanSweepFakeStorage) DeletePrefix(_ context.Context, prefix string) error {
	if prefix == "" {
		return storage.ErrInvalidPath
	}
	f.mu.Lock()
	f.deletePrefixCalls = append(f.deletePrefixCalls, prefix)
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			delete(f.objects, k)
		}
	}
	f.mu.Unlock()
	return nil
}

func (f *orphanSweepFakeStorage) List(_ context.Context, opts storage.ListOptions) (*storage.ListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, opts)

	res := &storage.ListResult{}
	seenPrefix := make(map[string]struct{})
	var prefixes []string
	var objs []storage.Object

	for k, v := range f.objects {
		if opts.Prefix != "" && !strings.HasPrefix(k, opts.Prefix) {
			continue
		}
		if opts.Delimiter != "" {
			rest := strings.TrimPrefix(k, opts.Prefix)
			if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
				p := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				if _, ok := seenPrefix[p]; !ok {
					seenPrefix[p] = struct{}{}
					prefixes = append(prefixes, p)
				}
				continue
			}
		}
		objs = append(objs, storage.Object{Name: k, Size: int64(len(v))})
	}
	for _, ep := range f.extraPrefixes {
		if _, ok := seenPrefix[ep]; !ok {
			seenPrefix[ep] = struct{}{}
			prefixes = append(prefixes, ep)
		}
	}

	sort.Strings(prefixes)
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })

	if !f.ignoreMaxResults && opts.MaxResults > 0 && len(prefixes) > opts.MaxResults {
		res.NextOffset = prefixes[opts.MaxResults]
		prefixes = prefixes[:opts.MaxResults]
	}
	res.Prefixes = prefixes
	res.Objects = objs
	return res, nil
}

func (f *orphanSweepFakeStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	f.mu.Lock()
	_, ok := f.objects[objectPath]
	f.mu.Unlock()
	return ok, nil
}

func (f *orphanSweepFakeStorage) GetObject(_ context.Context, objectPath string) (*storage.Object, error) {
	f.mu.Lock()
	data, ok := f.objects[objectPath]
	f.mu.Unlock()
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.Object{Name: objectPath, Size: int64(len(data))}, nil
}

func (f *orphanSweepFakeStorage) Copy(_ context.Context, _, _ string) (*storage.Object, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *orphanSweepFakeStorage) seedBundle(t *testing.T, hubID, rid string) {
	t.Helper()
	partKey, err := storage.MoveBundlePartPath(hubID, rid, storage.MoveBundleRootWorkspace, 0)
	require.NoError(t, err)
	manifestKey, err := storage.MoveBundleManifestPath(hubID, rid)
	require.NoError(t, err)
	f.mu.Lock()
	f.objects[partKey] = []byte("part-bytes")
	f.objects[manifestKey] = []byte(`{"version":"bundle/1"}`)
	f.mu.Unlock()
}

func (f *orphanSweepFakeStorage) hasAnyUnder(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (f *orphanSweepFakeStorage) deleteCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletePrefixCalls...)
}

func newOrphanSweepTestServer(t *testing.T) (*Server, store.Store, *orphanSweepFakeStorage) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	cfg := DefaultServerConfig()
	cfg.HubID = "hub-orphan-test"
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)
	stor := newOrphanSweepFakeStorage()
	srv.SetStorage(stor)
	return srv, s, stor
}

func createReincarnationAt(t *testing.T, s store.Store, id, state string, updatedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	rec := &store.AgentReincarnation{
		ID:             id,
		AgentID:        "agent-" + id,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStatePending,
		TransferMode:   store.TransferModeBundle,
		RequestedAt:    updatedAt,
		UpdatedAt:      updatedAt,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec))
	ok, err := s.TryAdvanceAgentReincarnation(ctx, &store.AgentReincarnation{
		ID:        id,
		State:     state,
		UpdatedAt: updatedAt,
	}, store.AgentReincarnationStatePending, time.Time{})
	require.NoError(t, err)
	require.True(t, ok, "failed to set state=%s updatedAt=%s for %s", state, updatedAt, id)
}

// TestMoveBundleOrphanSweep_T10_EligibilityMatrix verifies design §3.7 / T10:
//   - missing record -> deleted
//   - terminal (completed / failed) older than grace -> deleted
//   - non-terminal (pending / stopping / provisioning / starting) -> skipped
//   - young terminal (completed / failed within grace) -> skipped
//   - sibling-prefix safety: deleting <rid>/ never touches <rid>x/
func TestMoveBundleOrphanSweep_T10_EligibilityMatrix(t *testing.T) {
	srv, s, stor := newOrphanSweepTestServer(t)
	ctx := context.Background()
	hubID := srv.HubID()
	now := time.Now().UTC().Truncate(time.Second)

	oldTime := now.Add(-defaultMoveBundleOrphanGrace - time.Minute)
	youngTime := now.Add(-defaultMoveBundleOrphanGrace + 5*time.Minute)

	// Missing record (no DB row at all) -> MUST delete.
	stor.seedBundle(t, hubID, "rid-missing")

	// Old terminal records -> MUST delete.
	createReincarnationAt(t, s, "rid-old-completed", store.AgentReincarnationStateCompleted, oldTime)
	stor.seedBundle(t, hubID, "rid-old-completed")

	createReincarnationAt(t, s, "rid-old-failed", store.AgentReincarnationStateFailed, oldTime)
	stor.seedBundle(t, hubID, "rid-old-failed")

	// Young terminal records (within grace) -> MUST skip.
	createReincarnationAt(t, s, "rid-young-completed", store.AgentReincarnationStateCompleted, youngTime)
	stor.seedBundle(t, hubID, "rid-young-completed")

	createReincarnationAt(t, s, "rid-young-failed", store.AgentReincarnationStateFailed, youngTime)
	stor.seedBundle(t, hubID, "rid-young-failed")

	// Non-terminal records (even if older than grace!) -> MUST skip.
	for _, st := range store.AgentReincarnationNonTerminalStates {
		rid := "rid-active-" + st
		createReincarnationAt(t, s, rid, st, oldTime)
		stor.seedBundle(t, hubID, rid)
	}

	// Sibling-prefix safety: "rid-sib" is missing (deleted), while "rid-sibx"
	// is non-terminal (must remain completely untouched).
	stor.seedBundle(t, hubID, "rid-sib")
	createReincarnationAt(t, s, "rid-sibx", store.AgentReincarnationStateStopping, oldTime)
	stor.seedBundle(t, hubID, "rid-sibx")

	deleted, err := srv.sweepOrphanMoveBundlesAt(ctx, now, defaultMoveBundleOrphanGrace, moveBundleOrphanSweepLimit)
	require.NoError(t, err)
	assert.Equal(t, 4, deleted, "expected 4 deleted prefixes: missing, old-completed, old-failed, rid-sib")

	// Verify deleted prefixes are gone.
	for _, rid := range []string{"rid-missing", "rid-old-completed", "rid-old-failed", "rid-sib"} {
		base, err := storage.MoveBundlePath(hubID, rid)
		require.NoError(t, err)
		assert.False(t, stor.hasAnyUnder(storage.DirPrefix(base)), "expected %s to be deleted", rid)
	}

	// Verify kept prefixes are untouched (including sibling rid-sibx).
	kept := []string{
		"rid-young-completed",
		"rid-young-failed",
		"rid-sibx",
	}
	for _, st := range store.AgentReincarnationNonTerminalStates {
		kept = append(kept, "rid-active-"+st)
	}
	for _, rid := range kept {
		base, err := storage.MoveBundlePath(hubID, rid)
		require.NoError(t, err)
		assert.True(t, stor.hasAnyUnder(storage.DirPrefix(base)), "expected %s to remain untouched", rid)
	}

	// Verify every DeletePrefix call used DirPrefix (trailing "/").
	for _, p := range stor.deleteCalls() {
		assert.True(t, strings.HasSuffix(p, "/"), "DeletePrefix must use DirPrefix with trailing slash, got %q", p)
	}
}

// TestMoveBundleOrphanSweep_T10_Bound100PerPass verifies that a single sweep
// pass processes at most 100 prefixes even when storage returns more than 100.
func TestMoveBundleOrphanSweep_T10_Bound100PerPass(t *testing.T) {
	srv, _, stor := newOrphanSweepTestServer(t)
	// Simulate a storage backend that ignores MaxResults on delimiter listing
	// so we prove the sweep loop itself enforces the 100-per-pass bound.
	stor.ignoreMaxResults = true

	ctx := context.Background()
	hubID := srv.HubID()
	now := time.Now().UTC()

	const total = 125
	for i := range total {
		rid := fmt.Sprintf("rid-%04d", i)
		stor.seedBundle(t, hubID, rid)
	}

	deleted1, err := srv.sweepOrphanMoveBundlesAt(ctx, now, defaultMoveBundleOrphanGrace, moveBundleOrphanSweepLimit)
	require.NoError(t, err)
	assert.Equal(t, 100, deleted1, "first pass must be bounded to 100 prefixes")
	assert.Len(t, stor.deleteCalls(), 100)

	// Second pass cleans up the remaining 25.
	deleted2, err := srv.sweepOrphanMoveBundlesAt(ctx, now, defaultMoveBundleOrphanGrace, moveBundleOrphanSweepLimit)
	require.NoError(t, err)
	assert.Equal(t, 25, deleted2, "second pass should clean up the remaining 25 prefixes")
	assert.False(t, stor.hasAnyUnder(storage.DirPrefix(storage.MoveBundlesRoot(hubID))))
}

// TestMoveBundleOrphanSweep_T10_MalformedRIDSkipped verifies that malformed
// <rid> entries returned by storage listing are skipped and never widen a
// prefix delete.
func TestMoveBundleOrphanSweep_T10_MalformedRIDSkipped(t *testing.T) {
	srv, _, stor := newOrphanSweepTestServer(t)
	ctx := context.Background()
	hubID := srv.HubID()
	rootPrefix := storage.DirPrefix(storage.MoveBundlesRoot(hubID))

	// Inject malformed prefixes directly into the fake's listing output.
	stor.extraPrefixes = []string{
		rootPrefix,                        // empty rid (root itself)
		rootPrefix + "./",                 // dot
		rootPrefix + "../",                // dotdot
		rootPrefix + "../other/",          // traversal
		rootPrefix + ".hidden/",           // leading dot
		rootPrefix + "a/b/",               // nested slash
		rootPrefix + "a..b/",              // embedded dotdot
		rootPrefix + "rid with space/",    // whitespace
		rootPrefix + `bad\slash/`,         // backslash
		"hubs/other-hub/move-bundles/x/",  // outside this hub's root
		rootPrefix + "missing-slash-tail", // missing trailing slash
	}

	// Also seed one valid orphan and an unrelated object outside move-bundles/.
	stor.seedBundle(t, hubID, "rid-valid-orphan")
	_, err := stor.Upload(ctx, "hubs/"+hubID+"/templates/global/keep/file.txt", strings.NewReader("keep"), storage.UploadOptions{})
	require.NoError(t, err)

	deleted, err := srv.sweepOrphanMoveBundlesAt(ctx, time.Now().UTC(), defaultMoveBundleOrphanGrace, moveBundleOrphanSweepLimit)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "only the single valid orphan should be deleted")

	wantDel, err := storage.MoveBundlePath(hubID, "rid-valid-orphan")
	require.NoError(t, err)
	assert.Equal(t, []string{storage.DirPrefix(wantDel)}, stor.deleteCalls())
	assert.True(t, stor.hasAnyUnder("hubs/"+hubID+"/templates/global/keep/"), "unrelated objects must never be touched")
}

// TestMoveBundleOrphanSweep_T10_GraceExceedsSignedURLTTL verifies that the
// default grace period is strictly greater than SignedURLExpiry (15m) and that
// any grace <= SignedURLExpiry is rejected without deleting anything.
func TestMoveBundleOrphanSweep_T10_GraceExceedsSignedURLTTL(t *testing.T) {
	assert.Greater(t, defaultMoveBundleOrphanGrace, SignedURLExpiry,
		"defaultMoveBundleOrphanGrace must be strictly greater than SignedURLExpiry")

	srv, _, stor := newOrphanSweepTestServer(t)
	ctx := context.Background()
	stor.seedBundle(t, srv.HubID(), "rid-orphan")

	for _, badGrace := range []time.Duration{
		-time.Minute,
		0,
		SignedURLExpiry - time.Second,
		SignedURLExpiry,
	} {
		n, err := srv.sweepOrphanMoveBundlesAt(ctx, time.Now().UTC(), badGrace, moveBundleOrphanSweepLimit)
		require.Error(t, err, "grace %s must be rejected", badGrace)
		assert.Equal(t, 0, n)
	}
	assert.Empty(t, stor.deleteCalls(), "no prefix may be deleted when grace validation fails")
	assert.True(t, stor.hasAnyUnder(storage.DirPrefix(storage.MoveBundlesRoot(srv.HubID()))))
}

// TestMoveBundleOrphanSweep_T10_RunsWithExperimentOffAndWiredToHandler verifies
// that:
//   - the orphan sweep still runs when hub.agent_move_bundle is disabled
//     (cleanup of leftovers from before the experiment was turned off);
//   - reincarnationSweepHandler invokes the orphan sweep alongside the stale
//     reincarnation sweep;
//   - nil storage is a clean no-op.
func TestMoveBundleOrphanSweep_T10_RunsWithExperimentOffAndWiredToHandler(t *testing.T) {
	srv, _, stor := newOrphanSweepTestServer(t)
	ctx := context.Background()

	// Ensure hub.agent_move_bundle is explicitly OFF.
	require.False(t, srv.experimentEnabled(experiments.AgentMoveBundle),
		"test precondition: hub.agent_move_bundle should be off by default")

	stor.seedBundle(t, srv.HubID(), "rid-leftover-after-disable")

	// Invoke the periodic sweep handler wired into the scheduler.
	srv.reincarnationSweepHandler()(ctx)

	base, err := storage.MoveBundlePath(srv.HubID(), "rid-leftover-after-disable")
	require.NoError(t, err)
	assert.False(t, stor.hasAnyUnder(storage.DirPrefix(base)),
		"orphan sweep must clean leftovers even when hub.agent_move_bundle is off")

	// Nil storage is a clean no-op.
	srv.SetStorage(nil)
	n, err := srv.sweepOrphanMoveBundles(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
