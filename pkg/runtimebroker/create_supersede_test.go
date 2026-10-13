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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for ptone/scion#2632 items 2, 3 and 5: a create registers under the
// agent's name and waits for an earlier start of it before
// buildStartContext (sync and async); the wait ends in distinct retryable
// answers; and a stop or delete cancels an in-flight start only for the
// agent ID the hub names.

// buildStartContextProbe installs testHookCreateBeforeBuildStartContext for
// creates of name and counts the calls. The hook is removed at test end.
func buildStartContextProbe(t *testing.T, name string, onCall func()) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	testHookCreateBeforeBuildStartContext = func(req CreateAgentRequest) {
		if req.Name != name {
			return
		}
		n.Add(1)
		if onCall != nil {
			onCall()
		}
	}
	t.Cleanup(func() { testHookCreateBeforeBuildStartContext = nil })
	return &n
}

// holdRecord registers a launch record for key standing in for an earlier
// start still running (or cleaning up) on this broker. It reports whether
// the record was cancelled, and returns a release func (also run at test
// end; only the first call releases).
func holdRecord(t *testing.T, srv *Server, key launchKey, agentID string) (*launchRecord, *atomic.Bool, func()) {
	t.Helper()
	var cancelled atomic.Bool
	rec := newLaunchRecord("L-held-"+key.Slug, agentID, store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() { cancelled.Store(true) })
	srv.launchRegistry.Begin(key, rec)
	var once sync.Once
	release := func() { once.Do(func() { srv.launchRegistry.Finish(key, rec) }) }
	t.Cleanup(release)
	return rec, &cancelled, release
}

func registryRecord(srv *Server, key launchKey) *launchRecord {
	srv.launchRegistry.mu.Lock()
	defer srv.launchRegistry.mu.Unlock()
	return srv.launchRegistry.records[key]
}

// Item 2, sync: while an earlier start of the name is still finishing, a
// synchronous create does not reach buildStartContext at all; it is refused
// once the wait ends.
func TestCreateAgent_SyncSupersedeWaitBeforeBuildStartContext(t *testing.T) {
	srv, _, projectPath, _ := newSyncStartTestServer(t)
	srv.createSupersedeWait = 50 * time.Millisecond
	calls := buildStartContextProbe(t, "same-name", nil)
	_, cancelled, _ := holdRecord(t, srv, launchKey{Slug: "same-name"}, "agent-old-id")

	w := createSync(srv, "agent-new-id", projectPath)
	if n := calls.Load(); n != 0 {
		t.Fatalf("buildStartContext ran %d time(s) while an earlier start of the name was still in flight", n)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if !cancelled.Load() {
		t.Error("the earlier start of the name was not cancelled")
	}
}

// Item 2, sync: once the earlier start finishes, buildStartContext runs,
// and only then.
func TestCreateAgent_BuildStartContextRunsAfterSupersededStartFinishes(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	key := launchKey{Slug: "same-name"}
	old, _, releaseOld := holdRecord(t, srv, key, "agent-old-id")
	var oldDoneAtBuild atomic.Bool
	calls := buildStartContextProbe(t, "same-name", func() {
		select {
		case <-old.done:
			oldDoneAtBuild.Store(true)
		default:
		}
	})
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		return &api.AgentInfo{ID: "c-new", Name: opts.Name, Phase: "running"}, nil
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- createSync(srv, "agent-new-id", projectPath) }()
	if !waitUntil(t, syncStartTestTimeout, func() bool {
		rec := registryRecord(srv, key)
		return rec != nil && rec != old
	}) {
		t.Fatal("the new create never registered under the name")
	}
	time.Sleep(50 * time.Millisecond) // a create that does not wait would reach buildStartContext now
	if n := calls.Load(); n != 0 {
		t.Fatalf("buildStartContext ran %d time(s) before the earlier start finished", n)
	}
	releaseOld()

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the create did not return after the earlier start finished")
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if calls.Load() != 1 || !oldDoneAtBuild.Load() {
		t.Errorf("buildStartContext calls = %d, earlier start finished at that point = %v; want 1, true", calls.Load(), oldDoneAtBuild.Load())
	}
}

// Item 2, async: the same wait precedes buildStartContext on an async
// create, and its refusal comes before the 201 (nothing is launched).
func TestCreateAgent_AsyncSupersedeWaitBeforeBuildStartContext(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	srv.createSupersedeWait = 50 * time.Millisecond
	calls := buildStartContextProbe(t, "agent-sup-async", nil)
	_, cancelled, _ := holdRecord(t, srv, launchKey{Slug: "agent-sup-async"}, "agent-old-id")

	w := postCreate(t, srv, map[string]any{
		"id": "agent-new-id", "name": "agent-sup-async", "asyncLaunch": true, "launchId": "L-sup-async",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if n := calls.Load(); n != 0 {
		t.Fatalf("buildStartContext ran %d time(s) while an earlier start of the name was still in flight", n)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != api.BrokerErrCodePriorStartInProgress {
		t.Errorf("error code = %q, want %q", code, api.BrokerErrCodePriorStartInProgress)
	}
	if !cancelled.Load() {
		t.Error("the earlier start of the name was not cancelled")
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Errorf("Start called %d time(s) for a refused create", n)
	}
	if n := len(rtb.getLaunchReports()); n != 0 {
		t.Errorf("%d launch report(s) sent for a refused create", n)
	}
}

// Item 2, async: the launch takes the create's registration over, so the
// registry holds the launch's own record (its stop/delete cancel and its
// Finish work as before), not the registration's.
func TestAsyncCreate_HandsRegistrationToLaunch(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	defer close(mgr.startBlock)
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-handoff"}

	w := postCreate(t, srv, map[string]any{
		"id": "agent-handoff-id", "name": "agent-handoff", "asyncLaunch": true, "launchId": "L-handoff",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, syncStartTestTimeout, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("the launch never reached Start")
	}
	rec := registryRecord(srv, key)
	if rec == nil || rec.ID != "L-handoff" {
		t.Fatalf("registry record for the name = %+v, want the launch's record L-handoff", rec)
	}
	if !srv.launchRegistry.CancelLocalFor(key, "agent-handoff-id", "") {
		t.Fatal("a delete of the agent did not reach the launch")
	}
}

// Item 2, async: a stop or delete (or a newer start) that takes the name
// while buildStartContext runs makes the async create refuse with 409
// start_cancelled instead of accepting a launch.
func TestAsyncCreate_RegistrationCancelledBeforeHandoff_409(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-cancel-build"}
	buildStartContextProbe(t, "agent-cancel-build", func() {
		srv.launchRegistry.CancelLocalFor(key, "agent-cancel-build-id", "")
	})

	w := postCreate(t, srv, map[string]any{
		"id": "agent-cancel-build-id", "name": "agent-cancel-build", "asyncLaunch": true, "launchId": "L-cancel-build",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != api.BrokerErrCodeStartCancelled {
		t.Errorf("error code = %q, want %q", code, api.BrokerErrCodeStartCancelled)
	}
	time.Sleep(50 * time.Millisecond) // give a wrongly started launch a chance to claim
	if n := len(rtb.getLaunchReports()); n != 0 {
		t.Errorf("%d launch report(s) sent for a refused create", n)
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Errorf("registry still holds %+v for the refused create", rec)
	}
}

// Item 2: a create refused after it registered (here by Preflight, on the
// async path) releases its registration, so a later create of the name does
// not wait for it.
func TestCreateAgent_RefusalAfterRegistrationReleasesIt(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setPreflightErr(config.ErrTemplateNotFound)
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-release"}

	w := postCreate(t, srv, map[string]any{
		"id": "agent-release-id", "name": "agent-release", "asyncLaunch": true, "launchId": "L-release",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "missing"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Fatalf("registry still holds %+v after the create was refused", rec)
	}
}

// Item 3: a stop or delete of the agent while the create waits for an
// earlier start ends the wait with 409 start_cancelled.
func TestCreateAgent_CancelDuringSupersedeWait_409(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	srv.createSupersedeWait = syncStartTestTimeout
	key := launchKey{Slug: "same-name"}
	old, _, _ := holdRecord(t, srv, key, "agent-old-id")
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		t.Error("Start ran for a create cancelled during its supersede wait")
		return nil, ctx.Err()
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- createSync(srv, "agent-new-id", projectPath) }()
	if !waitUntil(t, syncStartTestTimeout, func() bool {
		rec := registryRecord(srv, key)
		return rec != nil && rec != old
	}) {
		t.Fatal("the new create never registered under the name")
	}
	srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/api/v1/agents/same-name?agentId=agent-new-id", nil))

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the delete did not end the create's supersede wait")
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != api.BrokerErrCodeStartCancelled {
		t.Errorf("error code = %q, want %q", code, api.BrokerErrCodeStartCancelled)
	}
}

func TestLaunchRegistry_Handoff(t *testing.T) {
	key := launchKey{ProjectID: "p1", Slug: "a"}
	notCancelled := func() bool { return false }

	t.Run("current record is replaced without cancelling", func(t *testing.T) {
		r := newLaunchRegistry()
		var fromCancelled atomic.Bool
		from := newLaunchRecord("from", "id", store.LaunchKindCreate, "", time.Time{}, func() { fromCancelled.Store(true) })
		to := newLaunchRecord("to", "id", store.LaunchKindCreate, "", time.Time{}, func() {})
		r.Begin(key, from)
		if !r.Handoff(key, from, to, notCancelled) {
			t.Fatal("Handoff refused the current record")
		}
		if r.records[key] != to {
			t.Fatalf("registry holds %+v, want the handed-over record", r.records[key])
		}
		if fromCancelled.Load() {
			t.Error("Handoff cancelled the record it replaced")
		}
	})
	t.Run("superseded record is refused", func(t *testing.T) {
		r := newLaunchRegistry()
		from := newLaunchRecord("from", "id", store.LaunchKindCreate, "", time.Time{}, func() {})
		newer := newLaunchRecord("newer", "id2", store.LaunchKindCreate, "", time.Time{}, func() {})
		to := newLaunchRecord("to", "id", store.LaunchKindCreate, "", time.Time{}, func() {})
		r.Begin(key, from)
		r.Begin(key, newer)
		if r.Handoff(key, from, to, notCancelled) {
			t.Fatal("Handoff accepted a record a newer start had replaced")
		}
		if r.records[key] != newer {
			t.Fatalf("registry holds %+v, want the newer start's record", r.records[key])
		}
	})
	t.Run("cancelled record is refused", func(t *testing.T) {
		r := newLaunchRegistry()
		from := newLaunchRecord("from", "id", store.LaunchKindCreate, "", time.Time{}, func() {})
		to := newLaunchRecord("to", "id", store.LaunchKindCreate, "", time.Time{}, func() {})
		r.Begin(key, from)
		if r.Handoff(key, from, to, func() bool { return true }) {
			t.Fatal("Handoff accepted a cancelled record")
		}
		if r.records[key] != from {
			t.Fatalf("registry holds %+v, want it unchanged", r.records[key])
		}
	})
}

// Item 5: CancelLocalFor matches on the hub's agent ID when both sides have
// one, and by name otherwise.
func TestLaunchRegistry_CancelLocalFor_AgentID(t *testing.T) {
	key := launchKey{ProjectID: "p1", Slug: "a"}
	for _, tc := range []struct {
		name       string
		recAgentID string
		reqAgentID string
		want       bool
	}{
		{"same agent", "agent-new", "agent-new", true},
		{"different agent", "agent-new", "agent-old", false},
		{"request without agent ID (older hub)", "agent-new", "", true},
		{"launch without agent ID", "", "agent-old", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLaunchRegistry()
			var cancelled atomic.Bool
			r.Begin(key, newLaunchRecord("L", tc.recAgentID, store.LaunchKindCreate, "", time.Time{}, func() { cancelled.Store(true) }))
			if got := r.CancelLocalFor(key, tc.reqAgentID, ""); got != tc.want || cancelled.Load() != tc.want {
				t.Fatalf("CancelLocalFor = %v, cancelled = %v; want %v", got, cancelled.Load(), tc.want)
			}
		})
	}
}

// Item 5: a late stop or delete for an old agent leaves alone the in-flight
// start of a newer agent of the same name; one naming the newer agent, or
// naming no agent (an older hub), still cancels it.
func TestStopAndDelete_AgentIDScopesLaunchCancel(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
	}{
		{"delete", http.MethodDelete, "/api/v1/agents/same-name?projectId=p1"},
		{"stop", http.MethodPost, "/api/v1/agents/same-name/stop?projectId=p1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, _ := newSyncStartTestServer(t)
			key := launchKey{ProjectID: "p1", Slug: "same-name"}
			_, cancelled, _ := holdRecord(t, srv, key, "agent-new")
			send := func(query string) {
				srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path+query, nil))
			}

			send("&" + api.AgentIDQueryParam + "=agent-old")
			if cancelled.Load() {
				t.Fatalf("a %s for the old agent cancelled the newer agent's start", tc.name)
			}
			send("&" + api.AgentIDQueryParam + "=agent-new")
			if !cancelled.Load() {
				t.Fatalf("a %s for the agent did not cancel its start", tc.name)
			}

			cancelled.Store(false)
			send("")
			if !cancelled.Load() {
				t.Fatalf("a %s without an agent ID (older hub) did not cancel the start by name", tc.name)
			}
		})
	}
}

// Item 2 regression (r1 finding 1): moving the registration ahead of
// buildStartContext must not move the synchronous start off the dispatch
// context. Manager.Start still sees the harness-config policy hook, the
// skill resolver and the hub agent defaults createAgent attaches after the
// registration.
func TestCreateAgent_FullSyncStartRunsUnderDispatchContext(t *testing.T) {
	srv, mgr := hookTestServer(t, false, "")
	code, body := postJSON(t, srv, "/api/v1/agents", `{
		"name": "dispatch-ctx", "id": "id-dispatch-ctx", "slug": "dispatch-ctx",
		"config": {"template": "none", "hubAgentDefaults": {"autoExposePorts": true}},
		"preResolvedSkills": {"resolved": [{"uri": "skill://scion/global/s@1.0.0", "name": "s",
			"resolvedVersion": "1.0.0", "contentHash": "sha256:abc", "files": []}]}
	}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", code, http.StatusCreated, body)
	}
	ctx := mgr.lastStartCtx
	if ctx == nil {
		t.Fatal("Start was never called")
	}
	if config.HarnessConfigPolicyFromContext(ctx) == nil {
		t.Error("the sync Start context has no harness-config policy hook")
	}
	if agent.SkillResolverFromContext(ctx) == nil {
		t.Error("the sync Start context has no skill resolver")
	}
	if hd := api.HubAgentDefaultsFromContext(ctx); hd == nil || hd.AutoExposePorts == nil || !*hd.AutoExposePorts {
		t.Errorf("the sync Start context hub defaults = %+v, want autoExposePorts=true", hd)
	}
}

// Item 3 (r1 finding 3): a stop, delete or newer start that cancels a sync
// create's registration while buildStartContext runs ends the create with
// 409 start_cancelled, before the download or Start.
func TestCreateAgent_SyncRegistrationCancelledDuringBuild_409(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	key := launchKey{Slug: "same-name"}
	buildStartContextProbe(t, "same-name", func() {
		srv.launchRegistry.CancelLocalFor(key, "agent-new-id", "")
	})
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		t.Error("Start ran for a create cancelled during buildStartContext")
		return nil, ctx.Err()
	}

	w := createSync(srv, "agent-new-id", projectPath)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != api.BrokerErrCodeStartCancelled {
		t.Errorf("error code = %q, want %q", code, api.BrokerErrCodeStartCancelled)
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Errorf("registry still holds %+v for the refused create", rec)
	}
}

// Item 2 (r1 finding 2): an async create of a name whose earlier async
// launch is still in its failure cleanup (a real runLaunch blocked in
// CleanupLaunch) waits for that cleanup before buildStartContext, then is
// accepted, hands its registration to its launch and reaches Start.
func TestAsyncCreate_WaitsForPredecessorCleanupThenAccepts(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(errors.New("boom"))
	cleanupBlock := make(chan struct{})
	mgr.setCleanupBlock(cleanupBlock)
	srv, rtb := newAsyncTestServer(t, mgr)
	srv.createSupersedeWait = syncStartTestTimeout
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.LaunchID == "L-old" && req.State == hubclient.AgentLaunchReportStateFailed {
			// 403 is one of the design's cleanup reasons, so the old
			// launch's failure drives it into cleanupAbortedLaunch, which
			// blocks on cleanupBlock.
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, nil
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}
	key := launchKey{Slug: "agent-shared"}
	body := func(id, launchID string) map[string]any {
		return map[string]any{
			"id": id, "name": "agent-shared", "asyncLaunch": true, "launchId": launchID,
			"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
		}
	}

	if w := postCreate(t, srv, body("agent-old-id", "L-old")); w.Code != http.StatusCreated {
		t.Fatalf("old create status = %d: %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, syncStartTestTimeout, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("the old launch never reached CleanupLaunch")
	}
	// The old launch is blocked in CleanupLaunch; its registry record (and
	// done channel) is still held. The new launch's Start succeeds and
	// stays running so its record can be inspected.
	mgr.setStartErr(nil)
	mgr.mu.Lock()
	mgr.startBlock = make(chan struct{})
	mgr.mu.Unlock()
	defer func() {
		mgr.mu.Lock()
		close(mgr.startBlock)
		mgr.mu.Unlock()
	}()
	calls := buildStartContextProbe(t, "agent-shared", nil)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postCreate(t, srv, body("agent-new-id", "L-new")) }()
	if !waitUntil(t, syncStartTestTimeout, func() bool {
		rec := registryRecord(srv, key)
		return rec != nil && strings.HasPrefix(rec.ID, "sync-")
	}) {
		t.Fatal("the new create never registered under the name")
	}
	time.Sleep(100 * time.Millisecond) // a create that does not wait would reach buildStartContext and Start now
	if n := calls.Load(); n != 0 {
		t.Fatalf("buildStartContext ran %d time(s) while the old launch was still cleaning up", n)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("Start calls = %d while the old launch was still cleaning up, want 1 (the old launch's)", n)
	}

	close(cleanupBlock)
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the new create did not return after the old launch's cleanup finished")
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("new create status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if resp := decodeCreateResponse(t, w); !resp.LaunchPending || resp.LaunchID != "L-new" {
		t.Fatalf("new create response = %+v, want an accepted launch L-new", resp)
	}
	if calls.Load() != 1 {
		t.Errorf("buildStartContext calls = %d, want 1", calls.Load())
	}
	if !waitUntil(t, syncStartTestTimeout, func() bool { return mgr.StartCallCount() >= 2 }) {
		t.Fatal("the new launch never reached Start")
	}
	if rec := registryRecord(srv, key); rec == nil || rec.ID != "L-new" {
		t.Fatalf("registry record for the name = %+v, want the new launch's record L-new", rec)
	}
}

// Item 5 (r1 finding 5): the cancel after delete resolution, keyed by the
// resolved entry's project (the delete names no projectId, so the early
// cancel cannot match), also honours agentId.
func TestDeleteAgent_ResolvedCancelHonoursAgentID(t *testing.T) {
	for _, tc := range []struct {
		name, agentID string
		wantCancel    bool
	}{
		{"old agent", "agent-old", false},
		{"the launch's agent", "agent-new", true},
		{"no agent ID (older hub)", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			mgr.agents = []api.AgentInfo{labelled("dev", "cid-legacy", scopeProjB, scionB)}

			cancels := 0
			srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"},
				newLaunchRecord("sync-1", "agent-new", "create", "", time.Time{}, func() { cancels++ }))

			query := ""
			if tc.agentID != "" {
				query = api.AgentIDQueryParam + "=" + tc.agentID
			}
			if code := doDelete(t, srv, "dev", query).Code; code != http.StatusNoContent {
				t.Fatalf("delete: expected 204, got %d", code)
			}
			if got := cancels > 0; got != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v", got, tc.wantCancel)
			}
		})
	}
}

// Item 5 (r1 finding 5): a run-scoped stop also honours agentId: a stop of
// the right run for an old agent leaves alone the newer agent's launch.
func TestStopAgent_RunScopedCancelHonoursAgentID(t *testing.T) {
	for _, tc := range []struct {
		name, agentID string
		wantCancel    bool
	}{
		{"old agent", "agent-old", false},
		{"the launch's agent", "agent-new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, _, _ := newSyncStartTestServer(t)
			setAgents(mgr, trackedRunEntry("c-a", "run-a"))
			cancels := 0
			rec := newLaunchRecord("async-1", "agent-new", "create", "", time.Time{}, func() { cancels++ })
			rec.RunID = "run-b"
			srv.launchRegistry.Begin(launchKey{Slug: "same-name"}, rec)

			actionWithRun(srv, "stop", "runId=run-b&"+api.AgentIDQueryParam+"="+tc.agentID, "")
			if got := cancels > 0; got != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v", got, tc.wantCancel)
			}
			if mgr.StopCalls() != 0 {
				t.Errorf("stop calls = %d, want 0: run-a's container must not be stopped", mgr.StopCalls())
			}
		})
	}
}

// Item 2 x flat ownership (GCP#3463): an async create whose registration is
// cancelled while buildStartContext runs (a stop, delete or newer start of
// the name) is refused at handoff with 409 start_cancelled, before the
// launch is accepted. beginAsyncLaunch then reports the create as not
// accepted, so createAgent's deferred release frees the ownership mirror
// buildStartContext installed, and the same slug can be created again.
func TestFlatAsyncCreate_HandoffRefusalReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	const name = "async-handoff-409"
	buildStartContextProbe(t, name, func() {
		f.srv.launchRegistry.CancelLocalFor(launchKey{ProjectID: flatTestProjectID, Slug: name}, "", "")
	})

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, name, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != api.BrokerErrCodeStartCancelled {
		t.Fatalf("error code = %q, want %q", code, api.BrokerErrCodeStartCancelled)
	}
	if n := ownedStartCount(f.srv); n != 0 {
		t.Fatalf("%d ownership mirror(s) left after the handoff refusal, want 0", n)
	}
	if n := f.mgr.StartCalls(); n != 0 {
		t.Fatalf("Start calls = %d after a refused async create, want 0", n)
	}
	testHookCreateBeforeBuildStartContext = nil
	assertMirrorReleasedAndSlugReusable(t, f, name)
}
