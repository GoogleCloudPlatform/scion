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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// Tests for the delete dispatch deadline (notAfter, ptone/scion#2906): a
// delete that reaches the broker after its deadline (plus the skew margin)
// is refused with 409 stale_dispatch and has no side effects.

// fencingManager records every side effect a delete can have: the runtime
// delete (via the embedded mock), the leftover-object cleanup and the NFS
// file removal. onList, when set, runs on every List (used to move the
// clock while the target is being resolved).
type fencingManager struct {
	cleanupRecordingManager
	nfsMu    sync.Mutex
	nfsCalls int
	onList   func()
}

func (m *fencingManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.onList != nil {
		m.onList()
	}
	return m.cleanupRecordingManager.List(ctx, filter)
}

func (m *fencingManager) RemoveNFSAgentFiles(_ context.Context, _, _, _ string) ([]string, error) {
	m.nfsMu.Lock()
	defer m.nfsMu.Unlock()
	m.nfsCalls++
	return nil, nil
}

func (m *fencingManager) nfsCallCount() int {
	m.nfsMu.Lock()
	defer m.nfsMu.Unlock()
	return m.nfsCalls
}

var _ nfsAgentFilesRemover = (*fencingManager)(nil)

// fakeDeleteClock is a settable clock for Server.deleteClock.
type fakeDeleteClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeDeleteClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeDeleteClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

var fenceT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type fenceFixture struct {
	srv     *Server
	mgr     *fencingManager
	clock   *fakeDeleteClock
	scion   string
	info    string
	cancels *int
}

// newFenceFixture builds a broker holding one running agent "dev" in
// project B (with files and an in-flight launch record for its run), whose
// delete clock reads fenceT0.
func newFenceFixture(t *testing.T, withAgent bool) *fenceFixture {
	t.Helper()
	mgr := &fencingManager{}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	if withAgent {
		mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-dev", scopeProjB, scionB), "run-a")}
	}
	clock := &fakeDeleteClock{now: fenceT0}
	srv.deleteClock = clock.Now
	cancels := 0
	rec := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() { cancels++ })
	rec.RunID = "run-a"
	srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, rec)
	return &fenceFixture{srv: srv, mgr: mgr, clock: clock, scion: scionB, info: infoB, cancels: &cancels}
}

func notAfterParam(t time.Time) string {
	return "&notAfter=" + url.QueryEscape(t.UTC().Format(time.RFC3339))
}

// assertNoDeleteSideEffects checks that nothing a delete can do was done.
func (f *fenceFixture) assertNoDeleteSideEffects(t *testing.T) {
	t.Helper()
	f.assertNoPostResolutionSideEffects(t)
	if *f.cancels != 0 {
		t.Errorf("in-flight launch cancelled %d times", *f.cancels)
	}
}

// assertNoPostResolutionSideEffects checks everything but the launch cancel.
func (f *fenceFixture) assertNoPostResolutionSideEffects(t *testing.T) {
	t.Helper()
	if n := f.mgr.DeleteCalls(); n != 0 {
		t.Errorf("DeleteTarget called %d times, want 0", n)
	}
	if calls := f.mgr.cleanupCalls(); len(calls) != 0 {
		t.Errorf("leftover cleanup ran: %v", calls)
	}
	if n := f.mgr.nfsCallCount(); n != 0 {
		t.Errorf("NFS file removal ran %d times", n)
	}
	assertUntouched(t, f.scion, "dev", f.info)
}

func assertStaleDispatch(t *testing.T, code int, body []byte) {
	t.Helper()
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode body: %v; body = %s", err, body)
	}
	if resp.Error.Code != ErrCodeStaleDispatch {
		t.Errorf("error code = %q, want %q", resp.Error.Code, ErrCodeStaleDispatch)
	}
}

const fenceQuery = "projectId=" + scopeProjB + "&runId=run-a" + allDeleteParams

func TestDeleteAgent_NotAfterPassed_409NoSideEffects(t *testing.T) {
	f := newFenceFixture(t, true)
	// Past the deadline by more than the skew margin.
	rec := doDelete(t, f.srv, "dev", fenceQuery+notAfterParam(fenceT0.Add(-deleteNotAfterSkew-time.Second)))
	assertStaleDispatch(t, rec.Code, rec.Body.Bytes())
	f.assertNoDeleteSideEffects(t)
}

// A not-found target (container and files gone) normally runs the
// leftover-object cleanup; a stale delete must not.
func TestDeleteAgent_NotAfterPassed_NotFound_NoLeftoverCleanup(t *testing.T) {
	f := newFenceFixture(t, false)
	rec := doDelete(t, f.srv, "ghost", "projectId="+scopeProjB+notAfterParam(fenceT0.Add(-time.Minute)))
	assertStaleDispatch(t, rec.Code, rec.Body.Bytes())
	f.assertNoDeleteSideEffects(t)
}

func TestDeleteAgent_NotAfterInFuture_Deletes(t *testing.T) {
	f := newFenceFixture(t, true)
	rec := doDelete(t, f.srv, "dev", fenceQuery+notAfterParam(fenceT0.Add(30*time.Second)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if f.mgr.LastDeleteContainerID() != "cid-dev" {
		t.Errorf("deleted container %q, want cid-dev", f.mgr.LastDeleteContainerID())
	}
}

// A deadline passed by less than the skew margin is still accepted.
func TestDeleteAgent_NotAfterWithinSkew_Deletes(t *testing.T) {
	f := newFenceFixture(t, true)
	rec := doDelete(t, f.srv, "dev", fenceQuery+notAfterParam(fenceT0.Add(-deleteNotAfterSkew)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if f.mgr.DeleteCalls() != 1 {
		t.Errorf("DeleteTarget calls = %d, want 1", f.mgr.DeleteCalls())
	}
}

// No notAfter: no check, whatever the clock says (an older hub).
func TestDeleteAgent_NoNotAfter_Unchanged(t *testing.T) {
	f := newFenceFixture(t, true)
	f.clock.Set(fenceT0.Add(1000 * time.Hour))
	rec := doDelete(t, f.srv, "dev", fenceQuery)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if f.mgr.DeleteCalls() != 1 {
		t.Errorf("DeleteTarget calls = %d, want 1", f.mgr.DeleteCalls())
	}
}

func TestDeleteAgent_NotAfterMalformed_400NoSideEffects(t *testing.T) {
	for _, v := range []string{"yesterday", "2026-10-05", "1759665600"} {
		t.Run(v, func(t *testing.T) {
			f := newFenceFixture(t, true)
			rec := doDelete(t, f.srv, "dev", fenceQuery+"&notAfter="+url.QueryEscape(v))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			f.assertNoDeleteSideEffects(t)
		})
	}
}

// The deadline passes while the target is being resolved (arrival was in
// time): the second check refuses before any side effect of the resolved
// delete. The in-flight launch cancel that runs before resolution (so a
// start still provisioning cannot outlive the delete) already ran, while
// the dispatch was within its deadline and the hub's claim, which blocks
// any new start, was live; it is the only thing that ran.
func TestDeleteAgent_NotAfterPassesDuringResolution_409NoSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withAgent bool
		slug      string
	}{
		{"found", true, "dev"},
		{"not found", false, "ghost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFixture(t, tc.withAgent)
			notAfter := fenceT0.Add(10 * time.Second)
			f.mgr.onList = func() { f.clock.Set(notAfter.Add(deleteNotAfterSkew + time.Second)) }
			rec := doDelete(t, f.srv, tc.slug, "projectId="+scopeProjB+"&runId=run-a"+allDeleteParams+notAfterParam(notAfter))
			assertStaleDispatch(t, rec.Code, rec.Body.Bytes())
			f.assertNoPostResolutionSideEffects(t)
		})
	}
}

// The same checks over the control channel, which tunnels the same query
// string to the same handler.
func TestControlChannel_DeleteNotAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		notAfter   time.Time
		wantStatus int
	}{
		{"passed", fenceT0.Add(-time.Minute), http.StatusConflict},
		{"future", fenceT0.Add(time.Minute), http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFixture(t, true)
			brokerConn, hubConn, cleanup := newWSPair(t)
			t.Cleanup(cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			client := &ControlChannelClient{
				config:      ControlChannelConfig{},
				conn:        brokerConn,
				handlers:    f.srv.Handler(),
				log:         slog.Default(),
				streams:     make(map[string]*StreamHandler),
				dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
				cancels:     make(map[string]context.CancelFunc),
				ctx:         ctx,
				cancel:      cancel,
			}
			req := wsprotocol.RequestEnvelope{
				Type:      "request",
				RequestID: "del-1",
				Method:    http.MethodDelete,
				Path:      "/api/v1/agents/dev",
				Query:     fenceQuery + notAfterParam(tc.notAfter),
			}
			client.wg.Add(1)
			go client.dispatchRequest(brokerConn, req)
			client.wg.Wait()

			var resp wsprotocol.ResponseEnvelope
			if err := hubConn.ReadJSON(&resp); err != nil {
				t.Fatalf("read response: %v", err)
			}
			if tc.wantStatus == http.StatusConflict {
				assertStaleDispatch(t, resp.StatusCode, resp.Body)
				f.assertNoDeleteSideEffects(t)
				return
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantStatus, resp.Body)
			}
			if f.mgr.DeleteCalls() != 1 {
				t.Errorf("DeleteTarget calls = %d, want 1", f.mgr.DeleteCalls())
			}
		})
	}
}
