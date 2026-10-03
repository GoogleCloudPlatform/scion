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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// Additional broker-level coverage for the record-less-actor delete/stop
// handling: a file-scan hit
// bypassing the record-less-actor probe on delete, a stop lookup error that
// still fell through to the idempotent 202, a false 409 for a same-project
// stop-then-delete race with no restart involved, the probe's own error
// path, the broker-level idempotence refinement against an atespace holding
// only recorded actors, and the delete-path project-blind guard.

const gapProjBID = "bbbbbbbbbbbb"

// gapAtespaceB is computed, not hardcoded, so it never drifts from
// substrateAtespaceName's own hash algorithm (pkg/runtime/substrate_runtime.go).
var gapAtespaceB = runtime.SubstrateAtespaceNameForTest(gapProjBID)

// simulateBrokerRestart wipes the substrate runtime's in-memory agent
// records AND control tokens mid-test, leaving the fake ateapi's actors in
// place — what a broker process restart looks like to the runtime.
// newTestSubstrateBrokerServer already wipes this state
// once at setup for test isolation; this is for a test that needs the wipe
// to happen AFTER it has already driven an agent through Run.
func simulateBrokerRestart(t *testing.T) {
	t.Helper()
	t.Cleanup(runtime.WipeSubstrateAgentStateForTest())
}

// onlyScopedListFails fails only an atespace-scoped ListActors call — the
// shape RecordlessActors' probe makes — leaving an unscoped call (List's own
// cluster-wide listing) unaffected.
func onlyScopedListFails(err error) func(*ateapipb.ListActorsRequest) error {
	return func(in *ateapipb.ListActorsRequest) error {
		if in.GetAtespace() != "" {
			return err
		}
		return nil
	}
}

// onlyUnscopedListFails fails only an unscoped ListActors call — the shape
// SubstrateRuntime.List makes internally (and therefore LookupContainerID,
// through it) — leaving an atespace-scoped probe call unaffected.
func onlyUnscopedListFails(err error) func(*ateapipb.ListActorsRequest) error {
	return func(in *ateapipb.ListActorsRequest) error {
		if in.GetAtespace() == "" {
			return err
		}
		return nil
	}
}

// TestSubstrateBroker_RealRestart_PreRestartAgentIs409_NewAgentDeletes is an
// end-to-end restart: an agent started through this broker, then a restart
// (records and tokens wiped, actor still in ateapi). Delete and stop of that
// agent in its own project must be 409 identity-unknown and touch nothing; a
// NEW agent started after the restart must still delete normally, and a
// second new agent must still stop normally (stop is delete, so a
// successful stop here means the underlying actor is gone).
func TestSubstrateBroker_RealRestart_PreRestartAgentIs409_NewAgentDeletes(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
	fc.mu.Lock()
	_, created := fc.actors[gapAtespaceB+"/projb--dev"]
	fc.mu.Unlock()
	if !created {
		t.Fatal("setup: Run did not create projb--dev in the fake ateapi")
	}
	simulateBrokerRestart(t)

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code != http.StatusConflict || decodeBrokerAPIError(t, w) != ErrCodeAgentIdentityUnknown {
		t.Fatalf("delete of pre-restart agent: status=%d body=%s, want 409 %s", w.Code, w.Body.String(), ErrCodeAgentIdentityUnknown)
	}
	w = httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code != http.StatusConflict || decodeBrokerAPIError(t, w) != ErrCodeAgentIdentityUnknown {
		t.Fatalf("stop of pre-restart agent: status=%d body=%s, want 409 %s", w.Code, w.Body.String(), ErrCodeAgentIdentityUnknown)
	}
	fc.mu.Lock()
	if n := len(fc.deleteActorCalls); n != 0 {
		t.Errorf("DeleteActor called %d time(s) on a pre-restart agent, want 0", n)
	}
	fc.mu.Unlock()

	runSubstrateAgentForProject(t, srv.manager, "fresh", "projb", gapProjBID, testProjectScionDir(t, "projb"))
	w = httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/fresh", nil), "fresh", gapProjBID)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete of post-restart agent: status=%d body=%s, want 204", w.Code, w.Body.String())
	}

	runSubstrateAgentForProject(t, srv.manager, "fresh-stop", "projb", gapProjBID, testProjectScionDir(t, "projb"))
	w = httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/fresh-stop/stop", nil), "fresh-stop", gapProjBID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("stop of post-restart agent: status=%d body=%s, want 202", w.Code, w.Body.String())
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, ok := fc.actors[gapAtespaceB+"/projb--fresh"]; ok {
		t.Error("post-restart agent still present after a successful delete")
	}
	if _, ok := fc.actors[gapAtespaceB+"/projb--fresh-stop"]; ok {
		t.Error("post-restart agent still present after a successful stop")
	}
	if _, ok := fc.actors[gapAtespaceB+"/projb--dev"]; !ok {
		t.Error("pre-restart actor was removed by acting on a different agent")
	}
}

// TestSubstrateBroker_AbsentSlug_OwnAtespaceAllRecorded_StaysIdempotent is
// the idempotence refinement at the broker level: an absent slug in a
// project whose OWN atespace holds only recorded actors must stay a 404
// (delete) and 202 (stop). The existing absent-slug tests use an empty
// atespace, so they cannot tell "count only record-less actors" apart from
// "count all actors".
func TestSubstrateBroker_AbsentSlug_OwnAtespaceAllRecorded_StaysIdempotent(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/gone", nil), "gone", gapProjBID)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete absent slug: status=%d body=%s, want 404", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/gone/stop", nil), "gone", gapProjBID)
	if w.Code != http.StatusAccepted {
		t.Errorf("stop absent slug: status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf("DeleteActor called %v, want none", fc.deleteActorCalls)
	}
}

// TestSubstrateBroker_ProbeOnlyError_ExplicitFailure covers the probe itself
// failing (only the atespace-scoped ListActors call errors; the resolve-time
// unscoped List succeeds) — this must be an explicit 5xx for delete and
// stop, never 404/202. The original probe-error test fails every
// ListActors, so it returns at listErr (delete) or a clean not-found
// (stop) before the probe itself ever runs.
func TestSubstrateBroker_ProbeOnlyError_ExplicitFailure(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	fc.mu.Lock()
	fc.listActorsErrFor = onlyScopedListFails(errors.New("simulated probe failure"))
	fc.mu.Unlock()

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code < 500 {
		t.Errorf("delete with failing probe: status=%d body=%s, want 5xx", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code < 500 {
		t.Errorf("stop with failing probe: status=%d body=%s, want 5xx", w.Code, w.Body.String())
	}
}

// TestSubstrateBroker_StopLookupErrorNoRecordless_ExplicitErrorNot202 covers
// a persistent slug lookup failure (every unscoped List call fails) while
// the probe would find zero record-less actors. The outcome is unknown, so
// it must be an explicit error, not the idempotent 202. The actor here is
// RECORDED — exactly the case the record-less-actor probe alone cannot
// catch.
func TestSubstrateBroker_StopLookupErrorNoRecordless_ExplicitErrorNot202(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
	fc.mu.Lock()
	fc.listActorsErrFor = onlyUnscopedListFails(errors.New("simulated transient list failure"))
	fc.mu.Unlock()

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code == http.StatusAccepted || w.Code < 400 {
		t.Errorf("stop with failing lookup: status=%d body=%s, want an explicit error, not 202", w.Code, w.Body.String())
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, ok := fc.actors[gapAtespaceB+"/projb--dev"]; !ok {
		t.Error("actor removed although the lookup failed")
	}
}

// TestSubstrateBroker_StopLookupError_BodyNeverNamesRuntimeScope proves that
// a raw List failure — which, against a real backend, can carry an actor
// name, an atespace/runtime-scope identifier or an internal address — never
// reaches the HTTP response body. The body must stay the fixed "Failed to
// stop agent" message; the raw error is logged server-side only.
func TestSubstrateBroker_StopLookupError_BodyNeverNamesRuntimeScope(t *testing.T) {
	leakyDetail := "rpc error: atespace=" + gapAtespaceB + " backend=10.1.2.3:9443 unreachable"
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
	fc.mu.Lock()
	fc.listActorsErrFor = onlyUnscopedListFails(errors.New(leakyDetail))
	fc.mu.Unlock()

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)

	body := w.Body.String()
	if strings.Contains(body, gapAtespaceB) || strings.Contains(body, "10.1.2.3") || strings.Contains(body, leakyDetail) {
		t.Errorf("response body leaks runtime-scope detail: %s", body)
	}
}

// TestSubstrateBroker_DeleteFileScanHitWithRecordlessActor_NotSuccess covers
// a delete with a persistent ~/.scion: the agent's files survive the
// restart in the hub-managed project dir, so findAgentProjectDir hits and a
// file-only delete target is available without the probe ever running. The
// actor is still in ateapi (record-less), so reporting success would orphan
// it. Must be 409 identity-unknown, with no files deleted and no
// DeleteActor call.
func TestSubstrateBroker_DeleteFileScanHitWithRecordlessActor_NotSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	srv, fc := newTestSubstrateBrokerServer(t)
	scionB, infoB := makeHubProject(t, home, "projb", gapProjBID, "dev")
	fc.putActor(gapAtespaceB, "projb--dev", "uid-projb-dev-prerestart")

	rec := doDelete(t, srv, "dev", "projectId="+gapProjBID+"&deleteFiles=true")
	if rec.Code/100 == 2 || rec.Code == http.StatusNotFound {
		t.Errorf("delete with file-scan hit and a record-less actor: status=%d body=%s, want 409 (never success while the actor exists)", rec.Code, rec.Body.String())
	} else if code := decodeBrokerAPIError(t, rec); code != ErrCodeAgentIdentityUnknown {
		t.Errorf("error code = %q, want %q", code, ErrCodeAgentIdentityUnknown)
	}
	assertUntouched(t, scionB, "dev", infoB)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, ok := fc.actors[gapAtespaceB+"/projb--dev"]; !ok {
		t.Error("record-less actor removed")
	}
}

// TestSubstrateBroker_ProjectBlindDelete_NotProbed: a project-blind
// (projectID == "") delete is out of scope for the probe: an absent slug
// stays 404 even when the atespace an empty projectID maps to ("scion-x")
// holds a record-less actor. Pins the delete-path projectID != "" guard.
func TestSubstrateBroker_ProjectBlindDelete_NotProbed(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor("scion-x", "ghost", "uid-ghost")

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/nobody", nil), "nobody", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("project-blind delete of absent slug: status=%d body=%s, want 404", w.Code, w.Body.String())
	}
}

// TestNonProberRuntime_RealAgentManager_NotFoundUnchanged: a non-substrate
// runtime behind a REAL *agent.AgentManager (the existing non-prober tests
// use filteringMockManager, which the probe skips at the *agent.AgentManager
// type assertion and so never reaches the RecordlessActorProber assertion at
// all) — not-found delete stays 404 and not-found stop stays 202.
func TestNonProberRuntime_RealAgentManager_NotFoundUnchanged(t *testing.T) {
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete: status=%d body=%s, want 404", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code != http.StatusAccepted {
		t.Errorf("stop: status=%d body=%s, want 202", w.Code, w.Body.String())
	}
}

// TestSubstrateBroker_StopThenDeleteWhileActorStillListedDeleting_StaysIdempotent
// covers normal operation, no restart at all. Stop (== Delete)
// drops this process's own record for the actor it just deleted, but Delete
// is fire-and-forget (substrate-runtime.md §9) — the actor can stay listed,
// in ACTOR_STATE_DELETING, for a while afterward. A later absent-slug
// delete of the same project must not mistake that lingering, record-less,
// DELETING entry for a post-restart identity-unknown case: it must stay the
// ordinary idempotent 404, not a false 409. Regression test for
// RecordlessActors excluding ACTOR_STATE_DELETING.
func TestSubstrateBroker_StopThenDeleteWhileActorStillListedDeleting_StaysIdempotent(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
	fc.mu.Lock()
	uid := fc.actors[gapAtespaceB+"/projb--dev"].GetMetadata().GetUid()
	fc.mu.Unlock()

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	// The fake's DeleteActor removed the actor outright; a real cluster
	// instead leaves it listed in DELETING for a while (the documented
	// fire-and-forget window). Simulate that: same UID, no record (this
	// process's own Delete already dropped it), state DELETING.
	fc.putActor(gapAtespaceB, "projb--dev", uid)
	fc.mu.Lock()
	fc.actors[gapAtespaceB+"/projb--dev"].Status.State = ateapipb.ActorState_ACTOR_STATE_DELETING
	fc.mu.Unlock()

	w = httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete of a slug whose only atespace entry is a DELETING, record-less actor: status=%d body=%s, want 404 (idempotent -- no restart happened)", w.Code, w.Body.String())
	}

	// Re-delete of the now-absent slug must stay 404 too (idempotent).
	w = httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code != http.StatusNotFound {
		t.Errorf("re-delete: status=%d body=%s, want 404", w.Code, w.Body.String())
	}
}

// unscopedListFailsOnlyOnCallN fails only the Nth unscoped ListActors call
// (1-indexed, counting only calls whose request carries no Atespace — the
// shape LookupContainerID's own List makes) and succeeds on every other
// call, including any later retry. This is what distinguishes a single
// transient failure from a persistent one: onlyUnscopedListFails above
// fails every such call, so it cannot tell a fix that re-derives the answer
// from a second, independent call (masking the first call's transient
// failure) apart from a fix that correctly decides from the first call's
// own error.
func unscopedListFailsOnlyOnCallN(n int, err error) func(*ateapipb.ListActorsRequest) error {
	calls := 0
	return func(in *ateapipb.ListActorsRequest) error {
		if in.GetAtespace() != "" {
			return nil
		}
		calls++
		if calls == n {
			return err
		}
		return nil
	}
}

// TestSubstrateBroker_StopTransientLookupFailure_ExplicitErrorNot202 pins
// the invariant against a SINGLE transient list failure at any position in
// the stop path's lookup sequence, not just a persistent one (unlike
// TestSubstrateBroker_StopLookupErrorNoRecordless_ExplicitErrorNot202
// above, which fails every unscoped call). Whichever call fails, the
// response must never be 202 unless Stop actually ran and removed the
// still-recorded actor — a fix that re-derives the outcome from a second,
// independent list call could see that second call succeed (because the
// original failure was transient) and report a false 202 while Stop was
// never called.
//
// Two call sequences are exercised, each trimmed to the calls that actually
// exist for it rather than padded with vacuous extra Ns. wantErr records,
// per call number, whether that call's own failure MUST surface as an
// explicit error — this is deliberately stronger than "202 implies a real
// action happened": for the absent-slug sequence, a false "not found" and a
// genuine one are both a 202 with nothing to act on, so a test that only
// checked side effects could not tell a masked failure apart from the
// correct idempotent case. The exact failure this exists to catch — the
// fallback list's own error swallowed as "not found" instead of propagated —
// would pass every side-effect check below and still be a false success.
//   - a recorded agent's stop makes 2 unscoped List calls: the prober-path
//     lookup's own primary call (must not be swallowed), then — once that
//     lookup resolves the target container ID — the agent manager's own
//     Stop-time List. That second call's own failure is tolerable: it
//     resolves a slug to a container ID for runtimes that only support
//     lookup-by-name, but Stop is already being called with a container ID
//     here, so its fallback path (pass the ID through unresolved) still
//     stops the right target. Calls 3+ don't exist for this sequence;
//   - an absent slug's stop reaches the prober-path lookup's
//     backward-compatibility FALLBACK list call too (call 2) before falling
//     through to the (scoped, so this unscoped-only failure injection does
//     not reach it) record-less-actor probe. Neither call may be swallowed as
//     "not found".
func TestSubstrateBroker_StopTransientLookupFailure_ExplicitErrorNot202(t *testing.T) {
	cases := []struct {
		name    string
		slug    string
		wantErr map[int]bool // call number -> must be an explicit error
	}{
		{"recorded agent", "dev", map[int]bool{1: true, 2: false}},
		{"absent slug (reaches the fallback list call)", "gone", map[int]bool{1: true, 2: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for n, wantErr := range tc.wantErr {
				t.Run(fmt.Sprintf("call_%d", n), func(t *testing.T) {
					srv, fc := newTestSubstrateBrokerServer(t)
					runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
					fc.mu.Lock()
					fc.listActorsErrFor = unscopedListFailsOnlyOnCallN(n, errors.New("simulated transient list failure"))
					fc.mu.Unlock()

					w := httptest.NewRecorder()
					srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+tc.slug+"/stop", nil), tc.slug, gapProjBID)

					// "dev" is always recorded, whichever slug this stop
					// targets: it doubles as a canary that an absent-slug
					// stop never touches an unrelated, still-existing agent.
					fc.mu.Lock()
					_, stillExists := fc.actors[gapAtespaceB+"/projb--dev"]
					deletes := len(fc.deleteActorCalls)
					fc.mu.Unlock()

					if wantErr {
						if w.Code < 500 {
							t.Errorf("call %d: status=%d, want an explicit 5xx (this call's own failure must not be swallowed as a not-found)", n, w.Code)
						}
						if !stillExists || deletes != 0 {
							t.Errorf("call %d: status=%d but the actor was touched (stillExists=%v deleteActorCalls=%d)", n, w.Code, stillExists, deletes)
						}
						return
					}
					if w.Code != http.StatusAccepted {
						t.Fatalf("call %d: status=%d, want 202 (this call's failure must not block the genuine outcome)", n, w.Code)
					}
					if tc.slug == "dev" {
						// A genuine 202 is only safe if Stop (== Delete)
						// actually ran and removed the actor.
						// Anything else is a false success: 202 while the
						// recorded actor is untouched.
						if stillExists || deletes == 0 {
							t.Errorf("call %d: status=202 but the actor was not actually stopped (stillExists=%v deleteActorCalls=%d) — false success", n, stillExists, deletes)
						}
					} else if !stillExists || deletes != 0 {
						t.Errorf("call %d: status=202 for an absent slug but the canary actor was touched (stillExists=%v deleteActorCalls=%d)", n, stillExists, deletes)
					}
				})
			}
		})
	}
}

// TestNonProberRuntime_StopLookupError_PropagatesAsFailure pins that on a
// broker with no RecordlessActorProber runtime registered, a default-runtime
// listing failure during the stop-target lookup is a real 5xx, never the
// idempotent 202. Every broker, prober or not, uses the
// same lookup (lookupAgentTarget); see
// TestNonProberRuntime_StopPrimaryListError_PropagatesAsUpstream in
// substrate_restart_lookup_edge_test.go for the same invariant pinned from
// the other direction.
func TestNonProberRuntime_StopLookupError_PropagatesAsFailure(t *testing.T) {
	listCalls := 0
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			listCalls++
			return nil, errors.New("simulated docker list failure")
		},
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code < 500 {
		t.Errorf("stop with failing lookup on a non-prober broker: status=%d body=%s, want 5xx", w.Code, w.Body.String())
	}
	if listCalls == 0 {
		t.Error("setup: the failing List was never called, so the lookup-error path was not exercised")
	}
}

// TestRecordlessActorProbe_DedupesAcrossManagers pins that the same
// prober's actors are not double-counted when the same underlying runtime
// is reachable through more than one manager — e.g. a per-profile substrate
// manager cached by resolveManagerForOpts alongside the default manager,
// both pointed at the same ateapi endpoint. Today's deployed topology never
// registers substrate twice, but nothing in recordlessActorProbe's loop
// otherwise prevents it from happening if that ever changes.
func TestRecordlessActorProbe_DedupesAcrossManagers(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor(gapAtespaceB, "ghost", "uid-ghost")

	dupMgr := agent.NewManager(srv.runtime)
	t.Cleanup(dupMgr.Close)
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["substrate-dup"] = auxiliaryRuntime{Runtime: srv.runtime, Manager: dupMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	atespace, names, err := recordlessActorProbe(context.Background(), srv.allManagers(), gapProjBID)
	if err != nil {
		t.Fatalf("recordlessActorProbe() error = %v", err)
	}
	if atespace != gapAtespaceB {
		t.Errorf("atespace = %q, want %q", atespace, gapAtespaceB)
	}
	if len(names) != 1 || names[0] != "ghost" {
		t.Errorf("names = %v, want exactly one entry [ghost]: the same actor reached through two managers over the same runtime must not be double-counted", names)
	}
}

// TestSubstrateBroker_DeleteIdentityUnknown_LogsNamesNotInBody pins that the
// record-less actor names reach only the broker's own WARN log, never the
// HTTP response body — the body carries just a generic count
// (AgentIdentityUnknown), which alone can't tell an operator which
// actor(s) matched. See deploy/substrate/README.md step 0.
func TestSubstrateBroker_DeleteIdentityUnknown_LogsNamesNotInBody(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor(gapAtespaceB, "projb--ghost", "uid-ghost")

	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "projb--ghost") {
		t.Errorf("HTTP body names the record-less actor, want only a generic count: %s", w.Body.String())
	}
	if !strings.Contains(logBuf.String(), "projb--ghost") {
		t.Errorf("broker log does not carry the record-less actor name: %s", logBuf.String())
	}
	assertRuntimeScopeLogged(t, logBuf.String(), gapAtespaceB)
}

// TestSubstrateBroker_StopIdentityUnknown_LogsNamesNotInBody is the stop-path
// twin of the delete test above.
func TestSubstrateBroker_StopIdentityUnknown_LogsNamesNotInBody(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor(gapAtespaceB, "projb--ghost", "uid-ghost")

	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "projb--ghost") {
		t.Errorf("HTTP body names the record-less actor, want only a generic count: %s", w.Body.String())
	}
	if !strings.Contains(logBuf.String(), "projb--ghost") {
		t.Errorf("broker log does not carry the record-less actor name: %s", logBuf.String())
	}
	assertRuntimeScopeLogged(t, logBuf.String(), gapAtespaceB)
}

// assertRuntimeScopeLogged checks that the prober-supplied scope is logged
// under the broker's generic runtime_scope key.
func assertRuntimeScopeLogged(t *testing.T, log, scope string) {
	t.Helper()
	if want := `"runtime_scope":"` + scope + `"`; !strings.Contains(log, want) {
		t.Errorf("broker log does not carry %s: %s", want, log)
	}
}
