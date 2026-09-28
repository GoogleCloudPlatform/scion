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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1975: clone concurrency: make same-name clones atomic.
//
// handleTemplateClone and handleHarnessConfigClone copy files to a storage
// path computed deterministically from (scope, scopeID, slug), then create
// the record. When two requests race to clone into the same destination
// name, both compute the same path; the losing request's failure-path
// cleanup can then run after the winner has already written its files
// there, removing them out from under the just-created record. This suite
// pins the fix: each clone request now writes to a storage path unique to
// itself, so a losing request's cleanup can only ever remove its own
// subtree, and exactly one request ends up with a usable record.
//
// Making the race reproducible: without synchronization, whether two
// requests actually interleave the way the fix cares about is up to the Go
// scheduler, and measurement showed the unsynchronized version of this suite
// caught a reverted fix in only a small fraction of runs. Every test below
// instead installs a copyBarrier on the mock storage's Copy method so that
// every racing request is guaranteed to pass the pre-check
// (GetTemplateBySlug/GetHarnessConfigBySlug) before any of them reaches
// CreateTemplate/CreateHarnessConfig, which is the interleaving the fix
// exists for. Each test also asserts, via assertNoOrphanedCloneStorage, that
// no storage survives under the destination prefix except the winner's own
// subtree.
// ============================================================================

const cloneRaceGoroutines = 20

// raceRequestIDKey carries a per-goroutine request identity through the
// request context so copyBarrier (below) can tell racing requests apart even
// when they compute the identical storage path — which is exactly what
// happens with the per-request storage suffix reverted (ptone/scion#1975).
// Identifying requests by the directory portion of the path they copy into
// would break in that scenario: with the suffix reverted, every racing
// request computes the same deterministic (scope, scopeID, slug) path with
// no per-request suffix, so all of them would look like the same "request"
// to the barrier and only one would ever arrive, hanging the other n-1
// forever.
type raceRequestIDKey struct{}

func withRaceRequestID(ctx context.Context, id int) context.Context {
	return context.WithValue(ctx, raceRequestIDKey{}, id)
}

func raceRequestIDFromContext(ctx context.Context) (int, bool) {
	id, ok := ctx.Value(raceRequestIDKey{}).(int)
	return id, ok
}

// raceRequest performs an HTTP request without using *testing.T, so it is
// safe to call from concurrent goroutines (t.Fatalf/require are not
// goroutine-safe when called outside the test's own goroutine). id
// identifies this call to a copyBarrier installed on the server's storage,
// if any; callers that don't need barrier synchronization can pass any
// value, since cloneMockStorage.Copy only consults it when a raceBarrier is
// set.
func raceRequest(srv *Server, token, method, path string, body interface{}, id int) int {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req = req.WithContext(withRaceRequestID(req.Context(), id))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// copyBarrier makes concurrent clone requests interleave deterministically.
// arrive blocks the calling goroutine until n distinct requests have all
// made their first Copy call, then releases every one of them at once. That
// guarantees every racing request has already passed its pre-check
// (GetTemplateBySlug/GetHarnessConfigBySlug, which runs before the storage
// path is even computed) before any of them reaches
// CreateTemplate/CreateHarnessConfig — the interleaving the per-request
// storage suffix fix (ptone/scion#1975) exists for, rather than one the
// goroutine scheduler only produces some of the time.
//
// Each request is identified by the raceRequestID carried on its context
// (see raceRequest), not by the path it copies into: with the fix reverted,
// every racing request computes the identical storage path, so a
// path-derived identity would collapse all of them into a single arrival
// and the barrier would never release. A request that copies more than one
// file only ever contributes to the arrival count once: its first Copy call
// arrives and blocks, and by the time it is released its own ID is already
// marked seen, so a second Copy call for the same request returns
// immediately from the already-closed release channel instead of
// re-arriving at the barrier.
//
// If a future change let some racing request exit before its first Copy
// call (a new early validation, a transient store error on the fast-path
// read), fewer than n requests would ever arrive and a waiter would block
// forever. arrive guards against that by giving up after
// copyBarrierArriveTimeout and recording the timeout so the test goroutine
// can fail with a clear message instead of hanging until go test's global
// timeout.
type copyBarrier struct {
	mu          sync.Mutex
	n           int
	seen        map[int]bool
	arrived     int
	release     chan struct{}
	releaseOnce sync.Once
	timedOut    bool
}

// copyBarrierArriveTimeout bounds how long a single arrive call waits to be
// released. It only matters when the barrier is stuck (see copyBarrier's doc
// comment above); it just needs to be comfortably longer than any real test
// run.
const copyBarrierArriveTimeout = 10 * time.Second

func newCopyBarrier(n int) *copyBarrier {
	return &copyBarrier{
		n:       n,
		seen:    make(map[int]bool),
		release: make(chan struct{}),
	}
}

func (b *copyBarrier) arrive(id int) {
	b.mu.Lock()
	if b.seen[id] {
		b.mu.Unlock()
		b.wait()
		return
	}
	b.seen[id] = true
	b.arrived++
	last := b.arrived == b.n
	b.mu.Unlock()

	if last {
		b.releaseOnce.Do(func() { close(b.release) })
		return
	}
	b.wait()
}

// wait blocks until the barrier releases or copyBarrierArriveTimeout
// elapses, whichever comes first. On timeout it records the failure so
// `failed` can report it, then closes release through releaseOnce so it
// happens exactly once even if arrive already closed it or an earlier
// timeout already did: later Copy calls from the same request then find
// release already closed instead of each waiting out their own
// copyBarrierArriveTimeout. Either way wait returns so the calling request
// proceeds rather than hanging.
func (b *copyBarrier) wait() {
	select {
	case <-b.release:
	case <-time.After(copyBarrierArriveTimeout):
		b.mu.Lock()
		b.timedOut = true
		b.mu.Unlock()
		b.releaseOnce.Do(func() { close(b.release) })
	}
}

// failed reports whether any arrive call gave up waiting instead of being
// released normally, i.e. fewer than n racing requests ever reached Copy.
// Tests that use a copyBarrier should call this after every request they
// fired has returned and t.Fatalf if it reports true, since that means the
// assertions that follow would be checking a race that never happened as
// intended.
func (b *copyBarrier) failed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timedOut
}

// assertNoOrphanedCloneStorage asserts that, under basePath (the
// deterministic <resource-kind>/<scope-layout>/<slug> prefix shared by every
// racing request, before the per-request clone-ID suffix the fix adds), the
// mock storage holds only the winner's own <cloneID>/... subtree. Every
// losing request's subtree must have been removed by its own failure-path
// DeletePrefix; any object under basePath outside the winner's subtree means
// a loser's cleanup ran on a prefix it did not own, or a winner's files were
// never cleaned up as expected.
func assertNoOrphanedCloneStorage(t *testing.T, stor *cloneMockStorage, basePath, winnerID string) {
	t.Helper()
	stor.mu.Lock()
	defer stor.mu.Unlock()

	wantPrefix := basePath + "/" + winnerID + "/"
	for p := range stor.objects {
		if !strings.HasPrefix(p, basePath+"/") {
			continue
		}
		assert.True(t, strings.HasPrefix(p, wantPrefix),
			"found storage left over outside the winning clone's own subtree: %s (winner's subtree is %s)", p, wantPrefix)
	}
}

// assertExactlyOneWinner runs n concurrent requests via fire and asserts
// exactly one 201 and the rest 409. fire receives the goroutine's index,
// which it should pass through to raceRequest as the barrier request ID.
func assertExactlyOneWinner(t *testing.T, n int, fire func(idx int) int) {
	t.Helper()
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			codes[idx] = fire(idx)
		}(i)
	}
	wg.Wait()

	var winners, conflicts int
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			winners++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status code %d from a concurrent clone", c)
		}
	}
	assert.Equal(t, 1, winners, "exactly one concurrent clone to the same name must win")
	assert.Equal(t, n-1, conflicts, "every other concurrent clone must be rejected as a conflict")
}

// requireCopyBarrierNotTimedOut fails the test immediately if the given
// copyBarrier had to give up waiting for a racing request (see
// copyBarrier.failed). Call it once every request fired against the barrier
// has returned, before trusting any assertion that depends on all of them
// having interleaved at the barrier as intended.
func requireCopyBarrierNotTimedOut(t *testing.T, b *copyBarrier) {
	t.Helper()
	require.False(t, b.failed(), "copyBarrier gave up waiting for a racing request to reach Copy; see copyBarrier's doc comment")
}

func TestTemplateClone_ConcurrentSameName_GlobalScope_ExactlyOneWinner(t *testing.T) {
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(cloneRaceGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "race-source-global")
	files := []store.TemplateFile{
		{Path: "scion-agent.yaml", Size: int64(len("scion-agent-config: race\n"))},
		{Path: "README.md", Size: int64(len("hello\n"))},
	}
	stor.seedObject(sourcePath+"/scion-agent.yaml", []byte("scion-agent-config: race\n"))
	stor.seedObject(sourcePath+"/README.md", []byte("hello\n"))
	source := &store.Template{
		ID: api.NewUUID(), Slug: "race-source-global", Name: "Source", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateTemplate(ctx, source))

	assertExactlyOneWinner(t, cloneRaceGoroutines, func(idx int) int {
		return raceRequest(srv, testDevToken, http.MethodPost, "/api/v1/templates/"+source.ID+"/clone", map[string]interface{}{
			"name":  "race-clone-global",
			"scope": "global",
		}, idx)
	})
	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	winner, err := s.GetTemplateBySlug(ctx, "race-clone-global", store.TemplateScopeGlobal, "")
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files, "the winning clone must have its file manifest populated")

	for _, f := range winner.Files {
		getRec := doRequest(t, srv, http.MethodGet, "/api/v1/templates/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "race-clone-global")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}

func TestTemplateClone_ConcurrentSameName_ProjectScope_ExactlyOneWinner(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(cloneRaceGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "race-source-project")
	files := []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len("scion-agent-config: race\n"))}}
	stor.seedObject(sourcePath+"/scion-agent.yaml", []byte("scion-agent-config: race\n"))
	source := &store.Template{
		ID: api.NewUUID(), Slug: "race-source-project", Name: "Source", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateTemplate(ctx, source))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)

	assertExactlyOneWinner(t, cloneRaceGoroutines, func(idx int) int {
		return raceRequest(srv, token, http.MethodPost, "/api/v1/templates/"+source.ID+"/clone", map[string]interface{}{
			"name":    "race-clone-project",
			"scope":   "project",
			"scopeId": project.ID,
		}, idx)
	})

	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	winner, err := s.GetTemplateBySlug(ctx, "race-clone-project", store.TemplateScopeProject, project.ID)
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files)

	for _, f := range winner.Files {
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeProject, project.ID, "race-clone-project")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}

// TestTemplateClone_ConcurrentSameName_UserScope_SameOwner_ExactlyOneWinner
// confirms the fix does not regress the already-closed cross-owner case: a
// single owner racing clones into their own user scope must still resolve
// to exactly one winner with intact files. It reads the winner's files back
// through the files API, like the global- and project-scope variants above:
// without that readback (and without the copyBarrier forcing the
// interleaving), this test could pass whether or not the fix is present,
// since a record can exist with a populated file manifest while the
// underlying storage has actually been deleted out from under it.
func TestTemplateClone_ConcurrentSameName_UserScope_SameOwner_ExactlyOneWinner(t *testing.T) {
	srv, s, alice, _, _ := setupTemplateAuthzTest(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(cloneRaceGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "race-source-user")
	files := []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len("scion-agent-config: race\n"))}}
	stor.seedObject(sourcePath+"/scion-agent.yaml", []byte("scion-agent-config: race\n"))
	source := &store.Template{
		ID: api.NewUUID(), Slug: "race-source-user", Name: "Source", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateTemplate(ctx, source))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)

	assertExactlyOneWinner(t, cloneRaceGoroutines, func(idx int) int {
		return raceRequest(srv, token, http.MethodPost, "/api/v1/templates/"+source.ID+"/clone", map[string]interface{}{
			"name":  "race-clone-user",
			"scope": "user",
		}, idx)
	})

	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	winner, err := s.GetTemplateBySlug(ctx, "race-clone-user", store.TemplateScopeUser, alice.ID)
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files)

	for _, f := range winner.Files {
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeUser, alice.ID, "race-clone-user")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}

func TestHarnessConfigClone_ConcurrentSameName_GlobalScope_ExactlyOneWinner(t *testing.T) {
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(cloneRaceGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "race-source-global")
	files := []store.TemplateFile{{Path: "config.yaml", Size: int64(len("harness: claude\n"))}}
	stor.seedObject(sourcePath+"/config.yaml", []byte("harness: claude\n"))
	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "race-source-global", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	assertExactlyOneWinner(t, cloneRaceGoroutines, func(idx int) int {
		return raceRequest(srv, testDevToken, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone", map[string]interface{}{
			"name":  "race-clone-global",
			"scope": "global",
		}, idx)
	})

	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	winner, err := s.GetHarnessConfigBySlug(ctx, "race-clone-global", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files)

	for _, f := range winner.Files {
		getRec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "race-clone-global")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}

func TestHarnessConfigClone_ConcurrentSameName_ProjectScope_ExactlyOneWinner(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(cloneRaceGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "race-source-project")
	files := []store.TemplateFile{{Path: "config.yaml", Size: int64(len("harness: claude\n"))}}
	stor.seedObject(sourcePath+"/config.yaml", []byte("harness: claude\n"))
	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "race-source-project", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)

	assertExactlyOneWinner(t, cloneRaceGoroutines, func(idx int) int {
		return raceRequest(srv, token, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone", map[string]interface{}{
			"name":    "race-clone-project",
			"scope":   "project",
			"scopeId": project.ID,
		}, idx)
	})

	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	winner, err := s.GetHarnessConfigBySlug(ctx, "race-clone-project", store.HarnessConfigScopeProject, project.ID)
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files)

	for _, f := range winner.Files {
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/harness-configs/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeProject, project.ID, "race-clone-project")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}

// ============================================================================
// ptone/scion#1975: a concurrent mix of legacy ("grove") and canonical
// ("project") scope spellings racing the same destination name.
//
// isValidTemplateScope / isValidHarnessConfigScope reject "grove" with 400
// before authorization, the collision lookup, or any storage write runs
// (#1968); template_clone_scope_test.go already pins that
// rejection for a solitary legacy request. This suite additionally mixes
// legacy requests into the same concurrent race as canonical ones, to pin
// that a legacy request never sneaks in as the winner, never leaves storage
// behind, and never disturbs the canonical requests' single-winner
// guarantee — since a legacy request never reaches GetTemplateBySlug or
// Copy, the copyBarrier below is still sized to the canonical request count
// only.
// ============================================================================

const (
	legacyScopeMixGoroutines    = 10
	canonicalScopeMixGoroutines = 10
)

// fireLegacyCanonicalMix runs legacyScopeMixGoroutines requests using the
// legacy scope name and canonicalScopeMixGoroutines requests using the
// canonical scope name concurrently, all against the same destination name,
// and returns their status codes.
func fireLegacyCanonicalMix(srv *Server, token, url, canonicalScope, scopeID, name string) (legacyCodes, canonicalCodes []int) {
	legacyCodes = make([]int, legacyScopeMixGoroutines)
	canonicalCodes = make([]int, canonicalScopeMixGoroutines)
	var wg sync.WaitGroup
	for i := 0; i < legacyScopeMixGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Legacy requests are rejected before ever reaching Copy, so
			// this ID is never consulted by the copyBarrier; it only needs
			// to be a valid raceRequest argument. It is offset from the
			// canonical requests' IDs below so that every concurrently
			// fired request in this test has a distinct identity, even
			// though legacy and canonical requests are fired in separate
			// loops that each start counting from 0.
			legacyCodes[idx] = raceRequest(srv, token, http.MethodPost, url, map[string]interface{}{
				"name":    name,
				"scope":   "grove",
				"scopeId": scopeID,
			}, 1000+idx)
		}(i)
	}
	for i := 0; i < canonicalScopeMixGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			canonicalCodes[idx] = raceRequest(srv, token, http.MethodPost, url, map[string]interface{}{
				"name":    name,
				"scope":   canonicalScope,
				"scopeId": scopeID,
			}, idx)
		}(i)
	}
	wg.Wait()
	return legacyCodes, canonicalCodes
}

func TestTemplateClone_ConcurrentLegacyAndCanonicalScope_OneCanonicalWinnerNoOrphans(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(canonicalScopeMixGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "race-source-legacy-mix")
	files := []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len("scion-agent-config: race\n"))}}
	stor.seedObject(sourcePath+"/scion-agent.yaml", []byte("scion-agent-config: race\n"))
	source := &store.Template{
		ID: api.NewUUID(), Slug: "race-source-legacy-mix", Name: "Source", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateTemplate(ctx, source))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)

	legacyCodes, canonicalCodes := fireLegacyCanonicalMix(srv, token,
		"/api/v1/templates/"+source.ID+"/clone", "project", project.ID, "race-clone-legacy-mix")
	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	for _, c := range legacyCodes {
		assert.Equal(t, http.StatusBadRequest, c,
			"every legacy-scope request must be rejected with 400 regardless of the concurrent canonical requests")
	}

	var winners, conflicts int
	for _, c := range canonicalCodes {
		switch c {
		case http.StatusCreated:
			winners++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status code %d from a concurrent canonical clone", c)
		}
	}
	assert.Equal(t, 1, winners, "exactly one canonical clone to the same name must win")
	assert.Equal(t, canonicalScopeMixGoroutines-1, conflicts, "every other canonical clone must be rejected as a conflict")

	winner, err := s.GetTemplateBySlug(ctx, "race-clone-legacy-mix", store.TemplateScopeProject, project.ID)
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files)

	for _, f := range winner.Files {
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeProject, project.ID, "race-clone-legacy-mix")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}

func TestHarnessConfigClone_ConcurrentLegacyAndCanonicalScope_OneCanonicalWinnerNoOrphans(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	stor := newCloneMockStorage("test-bucket")
	stor.raceBarrier = newCopyBarrier(canonicalScopeMixGoroutines)
	srv.SetStorage(stor)
	ctx := context.Background()

	sourcePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "race-source-legacy-mix")
	files := []store.TemplateFile{{Path: "config.yaml", Size: int64(len("harness: claude\n"))}}
	stor.seedObject(sourcePath+"/config.yaml", []byte("harness: claude\n"))
	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "race-source-legacy-mix", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket", Files: files,
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	token, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)

	legacyCodes, canonicalCodes := fireLegacyCanonicalMix(srv, token,
		"/api/v1/harness-configs/"+source.ID+"/clone", "project", project.ID, "race-clone-legacy-mix")
	requireCopyBarrierNotTimedOut(t, stor.raceBarrier)

	for _, c := range legacyCodes {
		assert.Equal(t, http.StatusBadRequest, c,
			"every legacy-scope request must be rejected with 400 regardless of the concurrent canonical requests")
	}

	var winners, conflicts int
	for _, c := range canonicalCodes {
		switch c {
		case http.StatusCreated:
			winners++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status code %d from a concurrent canonical clone", c)
		}
	}
	assert.Equal(t, 1, winners, "exactly one canonical clone to the same name must win")
	assert.Equal(t, canonicalScopeMixGoroutines-1, conflicts, "every other canonical clone must be rejected as a conflict")

	winner, err := s.GetHarnessConfigBySlug(ctx, "race-clone-legacy-mix", store.HarnessConfigScopeProject, project.ID)
	require.NoError(t, err)
	require.NotEmpty(t, winner.Files)

	for _, f := range winner.Files {
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/harness-configs/"+winner.ID+"/files/"+f.Path, nil)
		require.Equal(t, http.StatusOK, getRec.Code,
			"every file on the winning record must be readable through the files API: %s", getRec.Body.String())
	}

	basePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeProject, project.ID, "race-clone-legacy-mix")
	assertNoOrphanedCloneStorage(t, stor, basePath, winner.ID)
}
