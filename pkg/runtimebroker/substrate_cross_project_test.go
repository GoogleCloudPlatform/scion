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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// putActor directly registers an actor as if some earlier CreateActor call
// had created it, without going through Run — used to simulate an actor
// this SubstrateRuntime instance never itself started (e.g. a broker
// restart), so no in-memory agent record exists for it.
func (f *fakeSubstrateControlClient) putActor(atespace, name, uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors[atespace+"/"+name] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: uid},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
}

// newTestSubstrateBrokerServer builds a *Server whose default runtime is a
// real *SubstrateRuntime over a fake ateapi client, for the cross-project
// regression tests below. Returns the server and the fake client so a test
// can both inject actors directly and inspect which calls it received.
func newTestSubstrateBrokerServer(t *testing.T) (*Server, *fakeSubstrateControlClient) {
	t.Helper()
	// substrateAgentRecords/substrateControlTokens are process-wide
	// (pkg/runtime/substrate_runtime.go), not scoped to the *SubstrateRuntime
	// instance this test builds below. Without resetting them, a record left
	// behind by an earlier test in this package — under a UID this fake's
	// deterministic CreateActor ("uid-"+name) can easily reproduce for a
	// same-named actor in a later test — would leak in, silently turning a
	// meant-to-be-record-less actor into a recorded one. Wipe at the start of
	// every test and restore whatever was there before once it ends.
	t.Cleanup(runtime.WipeSubstrateAgentStateForTest())
	fc := newFakeSubstrateControlClient(&substrateEgressRecorder{})
	actorServer := newFakeSubstrateActorServer()
	t.Cleanup(actorServer.Close)

	rt := runtime.NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(actorServer.URL), nil, config.V1SubstrateConfig{})
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = ""
	return New(cfg, mgr, rt), fc
}

// testProjectScionDir creates a real, on-disk project directory named
// projectName (so config.GetProjectName's parent-basename convention
// recovers projectName from it) with a .scion subdirectory, and returns
// that .scion path — the exact shape config.GetResolvedProjectDir resolves
// a real project's ProjectPath to (pkg/agent/run.go's Start stores this
// same resolved path as the "scion.project_path" annotation). A fictional,
// non-existent path doesn't round-trip through
// config.GetResolvedProjectDir/GetProjectName the way a real project
// directory does, so tests that exercise AgentManager.Delete's
// deletionProjectName resolution need a real directory, not a string
// literal.
func testProjectScionDir(t *testing.T, projectName string) string {
	t.Helper()
	root := t.TempDir()
	projectDir := filepath.Join(root, projectName)
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", scionDir, err)
	}
	return scionDir
}

// runSubstrateAgentForProject drives an agent through rt.Run with the same
// labels and annotations pkg/agent/run.go's AgentManager.Start actually
// produces for a hub-dispatched (or CLI) start — see Start's Labels builder
// (scion.name, scion.agent, plus projectkeys.ProjectNameLabels/
// ProjectIDLabels merged in) and its Annotations
// (projectkeys.ProjectPathLabels(projectDir), set unconditionally
// once the project directory resolves, lines ~1207-1227) — plus the
// RunConfig.Project/ProjectID fields Start sets directly alongside the
// labels. actorName follows containerName(projectName, agentSlug) =
// "<project>--<agent>", also matching Start's convention.
func runSubstrateAgentForProject(t *testing.T, mgr agent.Manager, agentSlug, projectName, projectID, projectPath string) {
	t.Helper()
	am, ok := mgr.(*agent.AgentManager)
	if !ok {
		t.Fatalf("manager is a %T, want *agent.AgentManager", mgr)
	}
	actorName := projectName + "--" + agentSlug
	labels := map[string]string{
		"scion.name":  agentSlug,
		"scion.agent": "true",
	}
	for k, v := range projectkeys.ProjectNameLabels(projectName) {
		labels[k] = v
	}
	for k, v := range projectkeys.ProjectIDLabels(projectID) {
		labels[k] = v
	}
	cfg := runtime.RunConfig{
		Name:         actorName,
		Project:      projectName,
		ProjectID:    projectID,
		Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
		UnixUsername: "scion",
		NoAuth:       true,
		Labels:       labels,
		Annotations:  projectkeys.ProjectPathLabels(projectPath),
	}
	if _, err := am.Runtime.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run(%q) error = %v", actorName, err)
	}
}

// TestSubstrateBroker_RecordlessActorNotResolvableAcrossProjects is the
// broker-level counterpart to the runtime-level tests in
// pkg/runtime/substrate_runtime_test.go: it drives the real HTTP-facing
// handlers (stopAgent, deleteAgent) and the real LookupContainerID, against
// a real *Server backed by a real *SubstrateRuntime, to confirm a
// record-less actor in one project is never resolved — let alone acted
// on — by a same-slug request scoped to a DIFFERENT project.
//
// The scenario: after a broker restart, this runtime instance has no
// in-memory record for anything. A single actor exists,
// "scion-aaaaaaaaaaaa/projA--dev" (project A, agent slug "dev"). A request
// for agent "dev" scoped to project B must not find it, and must certainly
// never call DeleteActor on it.
func TestSubstrateBroker_RecordlessActorNotResolvableAcrossProjects(t *testing.T) {
	const (
		actorA  = "projA--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")

	t.Run("LookupContainerID scoped to project B returns nothing", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		fc.putActor(atespaceA, actorA, "uid-a")

		// A "not found" error alongside an empty containerID is the
		// expected shape here (LookupContainerID returns an error when
		// nothing matches) — what matters is that it never resolves to
		// project A's actor, not that it's silently error-free.
		got, _ := srv.LookupContainerID(context.Background(), "dev", projBID)
		if got != "" {
			t.Errorf(`LookupContainerID("dev", projB) = %q, want "" — must never resolve to project A's actor`, got)
		}
	})

	t.Run("stopAgent scoped to project B makes no Stop/Delete call", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		fc.putActor(atespaceA, actorA, "uid-a")

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil)
		srv.stopAgent(w, req, "dev", projBID)

		fc.mu.Lock()
		_, stillThere := fc.actors[atespaceA+"/"+actorA]
		fc.mu.Unlock()
		if !stillThere {
			t.Error("projA's actor was removed by a project-B-scoped stop — it must be untouched")
		}
	})

	t.Run("deleteAgent scoped to project B makes no DeleteActor call", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		fc.putActor(atespaceA, actorA, "uid-a")

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
		srv.deleteAgent(w, req, "dev", projBID)

		fc.mu.Lock()
		_, stillThere := fc.actors[atespaceA+"/"+actorA]
		fc.mu.Unlock()
		if !stillThere {
			t.Error("projA's actor was removed by a project-B-scoped delete — it must be untouched")
		}
	})
}

// TestSubstrateBroker_RecordlessActorNoOpEvenInItsOwnProject documents the
// accepted trade-off: a record-less actor is not resolvable by slug even
// through a request scoped to its OWN project, since nothing here can
// verify a record-less actor's project identity strongly enough to trust a
// slug match at all — see pkg/runtime/substrate_runtime.go's List doc
// comment. A same-project stop/delete therefore never calls Stop/DeleteActor
// on it and never touches it either way; it surfaces as an explicit 409
// (RecordlessActorProber finds the project's own atespace holds a
// record-less actor) rather than a no-op success, but
// this test only asserts the actor is left untouched — the status code is
// covered by the dedicated identity-unknown tests below.
func TestSubstrateBroker_RecordlessActorNoOpEvenInItsOwnProject(t *testing.T) {
	const (
		actorA  = "projA--dev"
		projAID = "aaaaaaaaaaaa"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")

	t.Run("LookupContainerID scoped to project A also returns nothing", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		fc.putActor(atespaceA, actorA, "uid-a")

		got, _ := srv.LookupContainerID(context.Background(), "dev", projAID)
		if got != "" {
			t.Errorf(`LookupContainerID("dev", projA) = %q, want "" (documented no-op: a record-less actor is never resolvable by slug)`, got)
		}
	})

	t.Run("stopAgent scoped to project A is a no-op", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		fc.putActor(atespaceA, actorA, "uid-a")

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil)
		srv.stopAgent(w, req, "dev", projAID)

		fc.mu.Lock()
		_, stillThere := fc.actors[atespaceA+"/"+actorA]
		fc.mu.Unlock()
		if !stillThere {
			t.Error("projA's own actor was removed by a same-project stop — it must be untouched (documented no-op)")
		}
	})

	t.Run("deleteAgent scoped to project A is a no-op", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		fc.putActor(atespaceA, actorA, "uid-a")

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
		srv.deleteAgent(w, req, "dev", projAID)

		fc.mu.Lock()
		_, stillThere := fc.actors[atespaceA+"/"+actorA]
		fc.mu.Unlock()
		if !stillThere {
			t.Error("projA's own actor was removed by a same-project delete — it must be untouched (documented no-op)")
		}
	})
}

// TestSubstrateBroker_SameSlugDifferentProjects_DeleteScopedToOwnProject is
// the record-HAVING counterpart to
// TestSubstrateBroker_RecordlessActorNotResolvableAcrossProjects: two
// agents, both started for real through rt.Run with the realistic
// Project/ProjectID/ProjectPath labels and annotations a hub-dispatched
// start produces (runSubstrateAgentForProject), sharing the agent slug
// "dev" in two different projects.
//
// This is the scenario a wrong-actor delete was found in: an unscoped
// listing can't tell the two same-slug actors apart. resolveDeleteTarget
// (handlers.go) closes this by including the requested project's ID in the
// very first Runtime.List call, so SubstrateRuntime.List returns only the
// requested project's actor in the first place — no same-slug tally, no
// ambiguity, no guess. The resolved entry's own ContainerID (a
// project-qualified "<atespace>/<actor>" pair, globally unique) is what
// Manager.DeleteTarget acts on, so the runtime-level delete never needs to
// list by bare slug again either.
//
// This proves both liveness and safety: a delete scoped to one project
// calls DeleteActor exactly twice (Stop, then Delete — see below), both
// calls naming only that project's own actor, and leaves the other
// project's same-slug actor completely untouched — checked with the
// request directed at each project in turn.
func TestSubstrateBroker_SameSlugDifferentProjects_DeleteScopedToOwnProject(t *testing.T) {
	const (
		actorA  = "proja--dev"
		projAID = "aaaaaaaaaaaa"
		actorB  = "projb--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	tests := []struct {
		name              string
		targetProjectID   string
		targetAtespace    string
		targetActor       string
		untouchedAtespace string
		untouchedActor    string
	}{
		{"delete project A", projAID, atespaceA, actorA, atespaceB, actorB},
		{"delete project B", projBID, atespaceB, actorB, atespaceA, actorA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, fc := newTestSubstrateBrokerServer(t)
			runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))
			runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
			srv.deleteAgent(w, req, "dev", tt.targetProjectID)

			if w.Code != http.StatusNoContent {
				t.Fatalf("deleteAgent status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
			}

			fc.mu.Lock()
			defer fc.mu.Unlock()
			// DeleteTarget (pkg/agent/manager.go's deleteResolved) calls
			// Runtime.Stop then Runtime.Delete unconditionally on the
			// resolved container ID; SubstrateRuntime.Stop is Delete
			// (substrate-runtime.md §9), so a single logical delete
			// deterministically issues exactly two DeleteActor RPCs, both
			// for the same actor. The second one hits an already-deleted
			// actor and gets NotFound back from the fake — tolerated by
			// SubstrateRuntime.Delete (pkg/runtime/substrate_runtime.go)
			// the same way a real cluster's repeat delete would be.
			// Asserting the exact sequence (not just "at least one call")
			// catches a regression that adds a third call or silently drops
			// the second.
			if len(fc.deleteActorCalls) != 2 {
				t.Fatalf("deleteAgent(%q) called DeleteActor %d times, want exactly 2 (Stop, then Delete): %v", tt.targetProjectID, len(fc.deleteActorCalls), fc.deleteActorCalls)
			}
			for i, call := range fc.deleteActorCalls {
				gotActor := call.GetActor()
				if gotActor.GetAtespace() != tt.targetAtespace || gotActor.GetName() != tt.targetActor {
					t.Errorf("DeleteActor call %d = %s/%s, want %s/%s", i, gotActor.GetAtespace(), gotActor.GetName(), tt.targetAtespace, tt.targetActor)
				}
			}
			if _, ok := fc.actors[tt.targetAtespace+"/"+tt.targetActor]; ok {
				t.Error("the requested project's actor is still present after delete")
			}
			if _, ok := fc.actors[tt.untouchedAtespace+"/"+tt.untouchedActor]; !ok {
				t.Error("the other project's same-slug actor was removed — it must never be touched by a delete scoped to a different project")
			}
		})
	}
}

// TestSubstrateBroker_SameSlugDifferentProjects_LookupContainerIDResolvesOwnProject
// is LookupContainerID's counterpart to the delete test above:
// scopedNameFilter (server.go) includes the project ID in the same
// Runtime.List call as the name filter, so SubstrateRuntime.List's
// same-slug ambiguity guard (which only applies to a name filter with no
// project scope) never engages, and each project-scoped lookup resolves to
// that project's own actor.
func TestSubstrateBroker_SameSlugDifferentProjects_LookupContainerIDResolvesOwnProject(t *testing.T) {
	const (
		projAID = "aaaaaaaaaaaa"
		projBID = "bbbbbbbbbbbb"
	)
	wantAContain := runtime.SubstrateAtespaceNameForTest(projAID) + "/proja--dev"
	wantBContain := runtime.SubstrateAtespaceNameForTest(projBID) + "/projb--dev"

	srv, _ := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))

	if got, err := srv.LookupContainerID(context.Background(), "dev", projAID); got != wantAContain || err != nil {
		t.Errorf(`LookupContainerID("dev", projA) = (%q, %v), want (%q, nil)`, got, err, wantAContain)
	}
	if got, err := srv.LookupContainerID(context.Background(), "dev", projBID); got != wantBContain || err != nil {
		t.Errorf(`LookupContainerID("dev", projB) = (%q, %v), want (%q, nil)`, got, err, wantBContain)
	}
}

// TestSubstrateBroker_UniqueSlugDeleteStillSucceeds is the ordinary-case
// control: exactly one record-having agent for a given slug (the common
// case) resolves and deletes normally through resolveDeleteTarget/
// DeleteTarget.
func TestSubstrateBroker_UniqueSlugDeleteStillSucceeds(t *testing.T) {
	const (
		actorA  = "proja--dev"
		projAID = "aaaaaaaaaaaa"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")

	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
	srv.deleteAgent(w, req, "dev", projAID)

	if w.Code != http.StatusNoContent {
		t.Fatalf("deleteAgent status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) == 0 {
		t.Fatal(`deleteAgent("dev", projA) called DeleteActor 0 times, want at least 1 — the single-project happy path must still delete`)
	}
	if _, ok := fc.actors[atespaceA+"/"+actorA]; ok {
		t.Error("projA's actor is still present after delete")
	}
}

// TestSubstrateBroker_DeleteAbsentSlugInProject_NotFound covers a
// project-B-scoped delete when project B has no record-having "dev" of its
// own — only project A does. resolveDeleteTarget's project-scoped List
// call (including project B's ID) returns nothing for project B, and the
// legacy no-project-identity and file-only fallbacks find nothing either
// (project A's entry carries its own project identity, so it is never a
// candidate for a project B request). deleteAgent reports
// errDeleteTargetNotFound as 404 without ever calling DeleteActor, and
// project A's actor is left untouched — a project without the slug touches
// nothing.
func TestSubstrateBroker_DeleteAbsentSlugInProject_NotFound(t *testing.T) {
	const (
		actorA  = "proja--dev"
		projAID = "aaaaaaaaaaaa"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")

	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code != http.StatusNotFound {
		t.Errorf(`deleteAgent("dev", projB) status = %d, want %d (no matching entry in project B)`, w.Code, http.StatusNotFound)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf(`deleteAgent("dev", projB) called DeleteActor %v, want zero — project B has no "dev" of its own, project A's must never be the fallback target`, fc.deleteActorCalls)
	}
	if _, ok := fc.actors[atespaceA+"/"+actorA]; !ok {
		t.Error("projA's actor was removed by a project-B-scoped delete that had no matching entry in project B — it must never be the wrong-actor target")
	}
}

// decodeBrokerAPIError decodes w's body as the standard ErrorResponse shape
// and returns the error code, failing the test if the body isn't that shape.
func decodeBrokerAPIError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body is not a valid ErrorResponse: %v (body=%s)", err, w.Body.String())
	}
	return resp.Error.Code
}

// TestSubstrateBroker_DeleteAbsentSlugInProject_RecordlessOwnAtespace_IdentityUnknown
// covers a project whose OWN atespace holds a record-less actor — the exact
// post-restart scenario this guards against: a delete for a slug that
// does not resolve (a record-less actor is never resolvable by slug — see
// SubstrateRuntime.List's doc comment) must not be reported as the ordinary
// idempotent not-found (which the hub treats as a completed delete),
// because the actor might be the very thing the caller meant. It must
// return 409 agent_identity_unknown instead, and DeleteActor must
// never be called.
func TestSubstrateBroker_DeleteAbsentSlugInProject_RecordlessOwnAtespace_IdentityUnknown(t *testing.T) {
	const (
		actorB  = "projb--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	srv, fc := newTestSubstrateBrokerServer(t)
	// projB's actor is injected directly (never through this runtime's own
	// Run), so it genuinely has no in-memory record — simulating a broker
	// restart. This server has no auxiliary runtimes registered at all
	// (newTestSubstrateBrokerServer), so this exercises the default-runtime
	// listErr path directly, not the aux-runtime listErr masking.
	fc.putActor(atespaceB, actorB, "uid-projb-recordless")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code != http.StatusConflict {
		t.Errorf(`deleteAgent("dev", projB) status = %d, want %d (a record-less actor exists in projB's own atespace)`, w.Code, http.StatusConflict)
	}
	if code := decodeBrokerAPIError(t, w); code != ErrCodeAgentIdentityUnknown {
		t.Errorf("deleteAgent(...) error code = %q, want %q", code, ErrCodeAgentIdentityUnknown)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf(`deleteAgent("dev", projB) called DeleteActor %v, want zero`, fc.deleteActorCalls)
	}
	if _, ok := fc.actors[atespaceB+"/"+actorB]; !ok {
		t.Error("projB's record-less actor was removed — it must be untouched")
	}
}

// TestSubstrateBroker_StopAbsentSlugInProject_RecordlessOwnAtespace_IdentityUnknown
// is stopAgent's counterpart to the delete test above: a stop that would
// otherwise report the generic idempotent 202 must instead return 409
// agent_identity_unknown when the target project's own atespace
// holds a record-less actor, and must never call Stop/DeleteActor.
func TestSubstrateBroker_StopAbsentSlugInProject_RecordlessOwnAtespace_IdentityUnknown(t *testing.T) {
	const (
		actorB  = "projb--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor(atespaceB, actorB, "uid-projb-recordless")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil)
	srv.stopAgent(w, req, "dev", projBID)

	if w.Code != http.StatusConflict {
		t.Errorf(`stopAgent("dev", projB) status = %d, want %d, not 202 — a record-less actor exists in projB's own atespace`, w.Code, http.StatusConflict)
	}
	if code := decodeBrokerAPIError(t, w); code != ErrCodeAgentIdentityUnknown {
		t.Errorf("stopAgent(...) error code = %q, want %q", code, ErrCodeAgentIdentityUnknown)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf(`stopAgent("dev", projB) called DeleteActor %v, want zero`, fc.deleteActorCalls)
	}
	if _, ok := fc.actors[atespaceB+"/"+actorB]; !ok {
		t.Error("projB's record-less actor was removed by stop — it must be untouched")
	}
}

// TestSubstrateBroker_DeleteAllListCallsFail_ExplicitFailureNot404 covers
// every ListActors call failing (both the resolve-time unscoped List and
// the atespace-scoped record-less-actor probe): this must surface as an
// explicit 5xx failure, never as the ordinary not-found/idempotent-success
// path, since a failed lookup proves nothing about whether the actor
// exists. This test's own fc.listActorsErr fails unconditionally, so it
// actually exercises the EARLIER resolve-time list-error path, returning
// before the probe itself is ever reached — despite this test's former
// name ("...ProbeError..."), which claimed otherwise. See
// TestSubstrateBroker_ProbeOnlyError_ExplicitFailure
// (substrate_restart_gaps_test.go) for the probe-specific case: only the
// atespace-scoped call fails, with the unscoped one succeeding.
func TestSubstrateBroker_DeleteAllListCallsFail_ExplicitFailureNot404(t *testing.T) {
	const projBID = "bbbbbbbbbbbb"

	srv, fc := newTestSubstrateBrokerServer(t)
	fc.mu.Lock()
	fc.listActorsErr = errors.New("simulated ateapi outage")
	fc.mu.Unlock()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code == http.StatusNotFound {
		t.Errorf(`deleteAgent("dev", projB) status = %d, want an explicit failure, never 404, when the probe itself errors`, w.Code)
	}
	if w.Code < 500 {
		t.Errorf(`deleteAgent("dev", projB) status = %d, want a 5xx (the probe could not determine safety)`, w.Code)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf(`deleteAgent("dev", projB) called DeleteActor %v, want zero`, fc.deleteActorCalls)
	}
}

// TestSubstrateBroker_MixedAtespace_RecordedDeletesRecordlessTriggersIdentityUnknown
// covers a single atespace holding both a record-having actor (this
// project's real, ordinary agent) and a record-less one (as if this project
// also had a pre-restart actor). Deleting the record-having agent by its own
// slug must succeed and touch only that actor; deleting an unrelated,
// genuinely-absent slug in the same project must return 409, since the
// record-less actor could be the very thing meant.
func TestSubstrateBroker_MixedAtespace_RecordedDeletesRecordlessTriggersIdentityUnknown(t *testing.T) {
	const (
		recordedName = "projb--dev"
		ghostName    = "projb--ghost"
		projBID      = "bbbbbbbbbbbb"
	)
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	t.Run("deleting the recorded agent succeeds and touches only it", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))
		fc.putActor(atespaceB, ghostName, "uid-ghost-recordless")

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
		srv.deleteAgent(w, req, "dev", projBID)

		if w.Code != http.StatusNoContent {
			t.Fatalf("deleteAgent(dev) status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
		}
		fc.mu.Lock()
		defer fc.mu.Unlock()
		if _, ok := fc.actors[atespaceB+"/"+recordedName]; ok {
			t.Error("the recorded agent is still present after delete")
		}
		if _, ok := fc.actors[atespaceB+"/"+ghostName]; !ok {
			t.Error("the record-less actor was touched by a delete of a different, recorded agent")
		}
	})

	t.Run("deleting an unrelated absent slug returns 409", func(t *testing.T) {
		srv, fc := newTestSubstrateBrokerServer(t)
		runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))
		fc.putActor(atespaceB, ghostName, "uid-ghost-recordless")

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/nonexistent-slug", nil)
		srv.deleteAgent(w, req, "nonexistent-slug", projBID)

		if w.Code != http.StatusConflict {
			t.Errorf(`deleteAgent(nonexistent-slug) status = %d, want %d`, w.Code, http.StatusConflict)
		}
		if code := decodeBrokerAPIError(t, w); code != ErrCodeAgentIdentityUnknown {
			t.Errorf("deleteAgent(...) error code = %q, want %q", code, ErrCodeAgentIdentityUnknown)
		}
		fc.mu.Lock()
		defer fc.mu.Unlock()
		if len(fc.deleteActorCalls) != 0 {
			t.Errorf(`deleteAgent(nonexistent-slug) called DeleteActor %v, want zero`, fc.deleteActorCalls)
		}
		if _, ok := fc.actors[atespaceB+"/"+recordedName]; !ok {
			t.Error("the unrelated recorded agent was removed by a 409'd delete of an unknown slug")
		}
	})
}

// TestSubstrateBroker_LogsForRecordlessAgent_ExplicitNotFound covers the
// logs endpoint for a slug that only resolves to a record-less actor: since
// a record-less actor is never resolvable by slug (SubstrateRuntime.List's
// doc comment), getLogs must report an explicit 404, never a 200 with an
// empty body — the hub-visible shape of "empty success" this design's
// invariant forbids.
func TestSubstrateBroker_LogsForRecordlessAgent_ExplicitNotFound(t *testing.T) {
	const (
		actorB  = "projb--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor(atespaceB, actorB, "uid-projb-recordless")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dev/logs", nil)
	srv.getLogs(w, req, "dev", projBID)

	if w.Code != http.StatusNotFound {
		t.Errorf(`getLogs("dev", projB) status = %d, want %d (never a 200 with an empty body)`, w.Code, http.StatusNotFound)
	}
}

// TestSubstrateBroker_ScopedDeleteMatchesOwnProject is the mirror of the
// not-found cases above: project B genuinely has its own record-having
// "dev" — resolveDeleteTarget finds it scoped to project B's own ID, and
// the delete proceeds and succeeds normally.
func TestSubstrateBroker_ScopedDeleteMatchesOwnProject(t *testing.T) {
	const (
		actorB  = "projb--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code != http.StatusNoContent {
		t.Fatalf("deleteAgent status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) == 0 {
		t.Fatal(`deleteAgent("dev", projB) called DeleteActor 0 times, want at least 1 — project B's own agent must still delete normally`)
	}
	if _, ok := fc.actors[atespaceB+"/"+actorB]; ok {
		t.Error("projB's actor is still present after delete")
	}
}

// TestSubstrateBroker_MatchedDelete_NeverFallsBackToHubManagedProjectScan
// covers deleteAgent's older, file-based fallback — findAgentProjectDir
// (via findAgentInHubManagedProjects), reached when a matched entry has no
// trusted project path and the request needs one (deleteFiles or
// softDelete). That fallback is itself project-scoped: it only accepts a
// hub-managed project directory whose own recorded project ID equals the
// requested one, so a same-named agent in a different project is never
// returned as the file target.
//
// For a MATCHED substrate entry started through a normal Run, this
// fallback is unreachable in the first place: the matched entry's own
// ProjectPath is non-empty and trusted, so resolveDeleteTarget never needs
// to resolve one from disk. This test proves that by planting a decoy
// "other project" whose own agents/dev directory a project-blind scan
// would return if it were ever invoked, and confirming a deleteFiles=true
// delete of the real, matched dev agent in project B never touches it.
func TestSubstrateBroker_MatchedDelete_NeverFallsBackToHubManagedProjectScan(t *testing.T) {
	// Point config.GetGlobalDir() (via os.UserHomeDir()) at a temp HOME so
	// the decoy hub-managed project below is the only thing
	// findAgentInHubManagedProjects could possibly find.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	decoyAgentDir := filepath.Join(tmpHome, ".scion", "projects", "decoy-project", ".scion", "agents", "dev")
	if err := os.MkdirAll(decoyAgentDir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(decoyAgentDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0644); err != nil {
		t.Fatal(err)
	}

	// Confirm the decoy is actually findable by the project-blind scan —
	// otherwise this test would pass for the wrong reason (the scan simply
	// finding nothing, not the matched-entry guard preventing the call).
	if got, err := findAgentInHubManagedProjects("dev", ""); got == "" || err != nil {
		t.Fatalf("test setup: findAgentInHubManagedProjects(\"dev\", \"\") = (%q, %v), want the decoy project's .scion dir to be found", got, err)
	}

	const projBID = "bbbbbbbbbbbb"
	srv, _ := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev?deleteFiles=true", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code != http.StatusNoContent {
		t.Fatalf("deleteAgent status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("decoy project's agent dir was touched (sentinel gone or dir removed: %v) — a matched substrate delete must never fall back to the project-blind hub-managed-project scan", err)
	}
}

// TestSubstrateBroker_NoMatchDelete_NeverMarksWrongProjectAgentInfo covers
// the NOT-FOUND case: resolveDeleteTarget returns errDeleteTargetNotFound
// before deleteAgent ever reaches the soft-delete agent-info.json marking
// step, so a project-B-scoped delete for a "dev" that doesn't exist in
// project B can never mark some OTHER project's agent-info.json as deleted
// — a real cross-project data-integrity bug even though no file content is
// deleted (only a status field would have been overwritten).
func TestSubstrateBroker_NoMatchDelete_NeverMarksWrongProjectAgentInfo(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	decoyAgentHome := filepath.Join(tmpHome, ".scion", "projects", "decoy-project", ".scion", "agents", "dev", "home")
	if err := os.MkdirAll(decoyAgentHome, 0755); err != nil {
		t.Fatal(err)
	}
	agentInfoPath := filepath.Join(decoyAgentHome, "agent-info.json")
	const originalContent = `{"phase":"running"}`
	if err := os.WriteFile(agentInfoPath, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	if got, err := findAgentInHubManagedProjects("dev", ""); got == "" || err != nil {
		t.Fatalf("test setup: findAgentInHubManagedProjects(\"dev\", \"\") = (%q, %v), want the decoy project's .scion dir to be found", got, err)
	}

	const (
		actorA  = "proja--dev"
		projAID = "aaaaaaaaaaaa"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev?deleteFiles=true&softDelete=true", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code != http.StatusNotFound {
		t.Errorf(`deleteAgent("dev", projB, deleteFiles=true, softDelete=true) status = %d, want %d`, w.Code, http.StatusNotFound)
	}
	got, err := os.ReadFile(agentInfoPath)
	if err != nil {
		t.Fatalf("decoy agent-info.json vanished: %v", err)
	}
	if string(got) != originalContent {
		t.Errorf("decoy project's agent-info.json = %s, want unchanged %s — a no-match substrate delete must never mark a project-blind guess's agent-info.json as deleted", got, originalContent)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, ok := fc.actors[atespaceA+"/"+actorA]; !ok {
		t.Error("projA's actor was removed — it must be untouched")
	}
}

// captureAgentLifecycleLogs redirects the default slog logger into a
// buffer for the duration of the test. s.agentLifecycleLog
// (pkg/runtimebroker/server.go) is built from slog.Default() once, at
// New() time (logging.Subsystem just wraps whatever the current default
// is), so this must run before the *Server under test is constructed to
// see its output.
func captureAgentLifecycleLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestSubstrateBroker_NoMatchDelete_LogsAtInfo confirms the not-found case
// is still visible in broker logs: an info-level line naming the agent
// slug and project ID, no secrets, so an operator can tell a substrate
// delete resolved to "not found in this project" rather than the request
// simply vanishing.
func TestSubstrateBroker_NoMatchDelete_LogsAtInfo(t *testing.T) {
	buf := captureAgentLifecycleLogs(t)

	const (
		projAID = "aaaaaaaaaaaa"
		projBID = "bbbbbbbbbbbb"
	)
	srv, _ := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil)
	srv.deleteAgent(w, req, "dev", projBID)

	if w.Code != http.StatusNotFound {
		t.Fatalf("deleteAgent status = %d, want %d", w.Code, http.StatusNotFound)
	}

	found := false
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["level"] == "INFO" && rec["agent_id"] == "dev" && rec["project_id"] == projBID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no info-level log line with agent_id=%q project_id=%q found; log output:\n%s", "dev", projBID, buf.String())
	}
}

// TestSubstrateBroker_SameSlugDifferentProjects_StopScopedToOwnProject is
// Stop's counterpart to
// TestSubstrateBroker_SameSlugDifferentProjects_DeleteScopedToOwnProject:
// stopAgent (handlers.go) resolves the target via projectScopedTarget, which
// calls LookupContainerID — using the same project-ID-scoped filter
// (scopedNameFilter) as the delete path — to get the project-matched
// entry's own container ID before calling Manager.Stop with that ID
// directly. Manager.Stop's own internal List call is unscoped by slug, but
// by the time it runs, agentID is already a container ID (a
// globally-unique "<atespace>/<actor>" pair for substrate), not the bare
// slug "dev" — slugifying it does not match any real actor's slug, so
// Stop's internal lookup finds nothing and falls through to
// Runtime.Stop(ctx, agentID) with that exact container ID unchanged. Since
// SubstrateRuntime.Stop is Delete (substrate-runtime.md §9,
// §4's Stop row), this issues exactly one DeleteActor call — not two, since
// stopAgent's own call chain invokes Runtime-level Stop only once, unlike
// deleteAgent's Stop-then-Delete pair.
// TestSubstrateBroker_SameSlugDifferentProjects_StopScopedToOwnProject covers
// both target directions (A and B) and, within each, both ListActors orders
// (via forceListOrder) so the assertion doesn't depend on Go's randomized map
// iteration order to catch a project-blind, pick-first stop regression.
func TestSubstrateBroker_SameSlugDifferentProjects_StopScopedToOwnProject(t *testing.T) {
	const (
		actorA  = "proja--dev"
		projAID = "aaaaaaaaaaaa"
		actorB  = "projb--dev"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")
	var atespaceB = runtime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")

	tests := []struct {
		name              string
		targetProjectID   string
		targetAtespace    string
		targetActor       string
		untouchedAtespace string
		untouchedActor    string
		listAFirst        bool
	}{
		{"stop project A, list A first", projAID, atespaceA, actorA, atespaceB, actorB, true},
		{"stop project A, list B first", projAID, atespaceA, actorA, atespaceB, actorB, false},
		{"stop project B, list A first", projBID, atespaceB, actorB, atespaceA, actorA, true},
		{"stop project B, list B first", projBID, atespaceB, actorB, atespaceA, actorA, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, fc := newTestSubstrateBrokerServer(t)
			runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))
			runSubstrateAgentForProject(t, srv.manager, "dev", "projb", projBID, testProjectScionDir(t, "projb"))

			fc.mu.Lock()
			actorAObj := fc.actors[atespaceA+"/"+actorA]
			actorBObj := fc.actors[atespaceB+"/"+actorB]
			if tt.listAFirst {
				fc.forceListOrder = []*ateapipb.Actor{actorAObj, actorBObj}
			} else {
				fc.forceListOrder = []*ateapipb.Actor{actorBObj, actorAObj}
			}
			fc.mu.Unlock()

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil)
			srv.stopAgent(w, req, "dev", tt.targetProjectID)

			if w.Code != http.StatusAccepted {
				t.Fatalf("stopAgent status = %d, want %d; body=%s", w.Code, http.StatusAccepted, w.Body.String())
			}

			fc.mu.Lock()
			defer fc.mu.Unlock()
			if len(fc.deleteActorCalls) != 1 {
				t.Fatalf("stopAgent(\"dev\", %q) called DeleteActor %d times, want exactly 1: %v", tt.targetProjectID, len(fc.deleteActorCalls), fc.deleteActorCalls)
			}
			gotActor := fc.deleteActorCalls[0].GetActor()
			if gotActor.GetAtespace() != tt.targetAtespace || gotActor.GetName() != tt.targetActor {
				t.Errorf("DeleteActor called for %s/%s, want %s/%s", gotActor.GetAtespace(), gotActor.GetName(), tt.targetAtespace, tt.targetActor)
			}
			if _, ok := fc.actors[tt.targetAtespace+"/"+tt.targetActor]; ok {
				t.Error("the requested project's actor is still present after a scoped stop")
			}
			if _, ok := fc.actors[tt.untouchedAtespace+"/"+tt.untouchedActor]; !ok {
				t.Error("the other project's same-slug actor was removed — it must never be touched by a stop scoped to a different project")
			}
		})
	}
}

// TestSubstrateBroker_StopAbsentSlugInProject_TouchesNothing is the stop
// counterpart to TestSubstrateBroker_DeleteAbsentSlugInProject_NotFound: a
// stop scoped to a project that has no "dev" of its own must not touch a
// different project's same-slug agent. projectScopedTarget returns "" when
// LookupContainerID can't resolve within the requested project, and
// stopAgent treats that as an idempotent no-op without ever calling
// Manager.Stop.
func TestSubstrateBroker_StopAbsentSlugInProject_TouchesNothing(t *testing.T) {
	const (
		actorA  = "proja--dev"
		projAID = "aaaaaaaaaaaa"
		projBID = "bbbbbbbbbbbb"
	)
	var atespaceA = runtime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")
	srv, fc := newTestSubstrateBrokerServer(t)
	runSubstrateAgentForProject(t, srv.manager, "dev", "proja", projAID, testProjectScionDir(t, "proja"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil)
	srv.stopAgent(w, req, "dev", projBID)

	// stopAgent reports 202 "accepted" for both an actual stop and this
	// not-found-in-project no-op (see handlers.go) — the hub's async stop
	// flow does not distinguish the two at this layer, only whether the
	// broker took a wrong-actor action, which is what this test guards.
	if w.Code != http.StatusAccepted {
		t.Errorf("stopAgent(\"dev\", projB) status = %d, want %d", w.Code, http.StatusAccepted)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf(`stopAgent("dev", projB) called DeleteActor %v, want zero — project B has no "dev" of its own`, fc.deleteActorCalls)
	}
	if _, ok := fc.actors[atespaceA+"/"+actorA]; !ok {
		t.Error("projA's actor was removed by a project-B-scoped stop that had no matching entry in project B")
	}
}
