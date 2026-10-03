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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// Edge coverage for the stop lookup on a broker with a RecordlessActorProber
// runtime registered: every "could not determine" branch
// of lookupAgentTarget, the deterministic,
// keep-scanning auxiliary-runtime iteration, the prober check on auxiliary
// runtimes, actor-identity dedupe across managers, and project-blind stop
// parity for other runtimes.

// proberMockRuntime is a MockRuntime that also implements
// RecordlessActorProber, so the broker takes its prober-scoped paths while
// the test controls List output directly.
type proberMockRuntime struct {
	*runtime.MockRuntime
	recordless []string
}

func (p *proberMockRuntime) RecordlessActors(context.Context, string) (string, []runtime.RecordlessActor, error) {
	actors := make([]runtime.RecordlessActor, len(p.recordless))
	for i, name := range p.recordless {
		actors[i] = runtime.RecordlessActor{Name: name, UID: "uid-" + name}
	}
	return "scion-" + gapProjBID, actors, nil
}

func isProjectScopedFilter(filter map[string]string) bool {
	return filter[projectkeys.LabelProjectID] != ""
}

func newProberBroker(t *testing.T, list func(context.Context, map[string]string) ([]api.AgentInfo, error), stops *stopRecorder) *Server {
	t.Helper()
	rt := &proberMockRuntime{MockRuntime: &runtime.MockRuntime{
		NameFunc: func() string { return "substrate" },
		ListFunc: list,
		StopFunc: stops.stop,
	}}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	return New(DefaultServerConfig(), mgr, rt)
}

// Any List failure while the lookup runs must come back wrapping
// ErrAgentListUnavailable, never as "not found" — for a
// present slug (primary call) and an absent one (primary and
// unlabelled-fallback calls).
func TestLookupAgentTarget_AnyListFailureIsCouldNotDetermine(t *testing.T) {
	for _, slug := range []string{"dev", "gone"} {
		for n := 1; n <= 3; n++ {
			t.Run(fmt.Sprintf("%s/call_%d", slug, n), func(t *testing.T) {
				srv, fc := newTestSubstrateBrokerServer(t)
				runSubstrateAgentForProject(t, srv.manager, "dev", "projb", gapProjBID, testProjectScionDir(t, "projb"))
				calls, injected := 0, false
				fc.mu.Lock()
				fc.listActorsErrFor = func(*ateapipb.ListActorsRequest) error {
					calls++
					if calls == n {
						injected = true
						return errors.New("simulated transient list failure")
					}
					return nil
				}
				fc.mu.Unlock()

				target, _, _, err := srv.lookupAgentTarget(context.Background(), slug, gapProjBID)

				fc.mu.Lock()
				defer fc.mu.Unlock()
				if !injected {
					if err != nil && !errors.Is(err, ErrAgentNotFound) {
						t.Errorf("no failure injected, err = %v", err)
					}
					return
				}
				if !errors.Is(err, ErrAgentListUnavailable) || target != "" {
					t.Errorf("list call %d failed during the lookup: got (%q, %v), want (\"\", ErrAgentListUnavailable)", n, target, err)
				}
			})
		}
	}
}

// Ambiguity on the prober path is "could not determine": explicit 5xx,
// Stop never called.
func TestProberBroker_StopAmbiguousLookup_ExplicitErrorNot202(t *testing.T) {
	stops := &stopRecorder{}
	labels := map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: gapProjBID}
	srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{Name: "dev", ContainerID: "c1", Labels: labels},
			{Name: "dev", ContainerID: "c2", Labels: labels},
		}, nil
	}, stops)

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code < 500 {
		t.Errorf("ambiguous stop lookup: status=%d body=%s, want 5xx", w.Code, w.Body.String())
	}
	if got := stops.calls(); len(got) != 0 {
		t.Errorf("Stop called %v on an ambiguous lookup", got)
	}
}

// A matched entry with no container ID folds into "not found" on the
// prober path, exactly as on every other broker: nothing is
// addressable to stop, so this is the idempotent 202, not a 5xx. With no
// record-less actors registered, the record-less-actor probe below finds
// nothing and the stop falls through cleanly.
func TestProberBroker_StopEntryWithoutContainerID_TreatedAsNotFound(t *testing.T) {
	stops := &stopRecorder{}
	srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{{Name: "dev", Labels: map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: gapProjBID}}}, nil
	}, stops)

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code != http.StatusAccepted {
		t.Errorf("stop of an entry with no container ID: status=%d body=%s, want 202 (not found)", w.Code, w.Body.String())
	}
	if got := stops.calls(); len(got) != 0 {
		t.Errorf("Stop called %v for an entry with no container ID", got)
	}
}

// An auxiliary runtime's List error on the prober path, with NO other
// auxiliary runtime holding a match, is never silently skipped: whichever
// lookup stage it hits (project-scoped or unlabelled fallback), the stop is
// an explicit 5xx, not the idempotent 202.
func TestProberBroker_StopAuxRuntimeListError_ExplicitErrorNot202(t *testing.T) {
	cases := []struct {
		name  string
		fails func(map[string]string) bool
	}{
		{"project-scoped stage", isProjectScopedFilter},
		{"unlabelled fallback stage", func(f map[string]string) bool { return !isProjectScopedFilter(f) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stops := &stopRecorder{}
			srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, stops)
			addAuxRuntime(t, srv, "docker", &runtime.MockRuntime{
				NameFunc: func() string { return "docker" },
				ListFunc: func(_ context.Context, f map[string]string) ([]api.AgentInfo, error) {
					if f["scion.name"] != "" && tc.fails(f) {
						return nil, errors.New("simulated docker list failure")
					}
					return nil, nil
				},
			})

			w := httptest.NewRecorder()
			srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
			if w.Code < 500 {
				t.Errorf("stop with a failing auxiliary List: status=%d body=%s, want 5xx", w.Code, w.Body.String())
			}
		})
	}
}

// A match from one auxiliary runtime is authoritative even when another
// auxiliary runtime's List call fails: the stop succeeds, deterministically,
// regardless of which of the two sorts before the other. Before the
// deterministic, keep-scanning rewrite, this outcome depended on Go's
// randomized map iteration order over auxiliaryRuntimes — this test names
// the two runtimes on both sides of the alphabetical order, and repeats
// each shape several times, to show the result no longer depends on it.
func TestProberBroker_StopAuxMatchIsAuthoritativeOverAnotherAuxsListError(t *testing.T) {
	const iterations = 20
	cases := []struct {
		name        string
		erroringAux string
		matchingAux string
	}{
		{"erroring runtime sorts first", "aux-a-erroring", "aux-b-matching"},
		{"matching runtime sorts first", "aux-a-matching", "aux-b-erroring"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				defaultStops := &stopRecorder{} // Stop must NOT be dispatched here.
				auxStops := &stopRecorder{}     // the match lives on this aux runtime; Stop must land here.
				srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, defaultStops)
				addAuxRuntime(t, srv, tc.erroringAux, &runtime.MockRuntime{
					NameFunc: func() string { return "erroring" },
					ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
						return nil, errors.New("simulated list failure")
					},
				})
				addAuxRuntime(t, srv, tc.matchingAux, &runtime.MockRuntime{
					NameFunc: func() string { return "matching" },
					ListFunc: func(_ context.Context, f map[string]string) ([]api.AgentInfo, error) {
						if !isProjectScopedFilter(f) {
							return nil, nil
						}
						return []api.AgentInfo{{
							Name:        "dev",
							ContainerID: "aux-container",
							Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: gapProjBID},
						}}, nil
					},
					StopFunc: auxStops.stop,
				})

				w := httptest.NewRecorder()
				srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
				if w.Code != http.StatusAccepted {
					t.Fatalf("iteration %d: status=%d body=%s, want 202 (a match elsewhere is authoritative over another auxiliary's list error)", i, w.Code, w.Body.String())
				}
				if got := defaultStops.calls(); len(got) != 0 {
					t.Fatalf("iteration %d: default runtime's Stop called %v, want none: the match is on the auxiliary runtime", i, got)
				}
				if got := auxStops.calls(); len(got) != 1 || got[0] != "aux-container" {
					t.Fatalf("iteration %d: auxiliary runtime's Stop calls = %v, want [aux-container]", i, got)
				}
			}
		})
	}
}

// A prober registered only as an auxiliary runtime still switches stop to
// the error-preserving lookup and still gets probed.
func TestProberAsAuxiliaryRuntime_StopFailsClosed(t *testing.T) {
	newBroker := func(t *testing.T, defaultList func(context.Context, map[string]string) ([]api.AgentInfo, error), recordless []string) *Server {
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }, ListFunc: defaultList}
		mgr := agent.NewManager(rt)
		t.Cleanup(mgr.Close)
		srv := New(DefaultServerConfig(), mgr, rt)
		addAuxRuntime(t, srv, "substrate", &proberMockRuntime{
			MockRuntime: &runtime.MockRuntime{
				NameFunc: func() string { return "substrate" },
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			},
			recordless: recordless,
		})
		return srv
	}

	t.Run("lookup error is explicit 5xx", func(t *testing.T) {
		srv := newBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, errors.New("simulated docker list failure")
		}, nil)
		w := httptest.NewRecorder()
		srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
		if w.Code < 500 {
			t.Errorf("status=%d body=%s, want 5xx", w.Code, w.Body.String())
		}
	})

	t.Run("record-less actor on the auxiliary prober is 409", func(t *testing.T) {
		srv := newBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, []string{"projb--ghost"})
		w := httptest.NewRecorder()
		srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
		if w.Code != http.StatusConflict || decodeBrokerAPIError(t, w) != ErrCodeAgentIdentityUnknown {
			t.Errorf("status=%d body=%s, want 409 %s", w.Code, w.Body.String(), ErrCodeAgentIdentityUnknown)
		}
	})
}

// On a broker with no registered RecordlessActorProber, stopAgent takes the
// non-prober branch and gets upstream's exact projectScopedTarget handling:
// a primary-List failure is a real error, surfaced as a
// 5xx, never silently folded into the idempotent "not found" 202. This pins
// that the non-prober branch is unaffected by the prober-only strictness
// added above — it is upstream's behavior, verbatim.
func TestNonProberRuntime_StopPrimaryListError_PropagatesAsUpstream(t *testing.T) {
	stops := &stopRecorder{}
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, errors.New("simulated transient list failure")
		},
		StopFunc: stops.stop,
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code < 500 {
		t.Errorf("non-prober stop with a failing primary List: status=%d body=%s, want 5xx", w.Code, w.Body.String())
	}
	if got := stops.calls(); len(got) != 0 {
		t.Errorf("Stop called %v on a primary List failure", got)
	}
}

// Project-blind stop on a broker with no prober keeps the generic
// behaviour: an id the runtime does not list is passed straight through to
// Runtime.Stop (a container ID or a legacy name), never silently dropped.
func TestNonProberRuntime_ProjectBlindStop_PassesIDThrough(t *testing.T) {
	stops := &stopRecorder{}
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		StopFunc: stops.stop,
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/abc123/stop", nil), "abc123", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if got := stops.calls(); len(got) != 1 || got[0] != "abc123" {
		t.Errorf("Runtime.Stop calls = %v, want [abc123]", got)
	}
}

// The error-preserving lookup is project-scoped only: a project-blind stop
// on a prober broker keeps the generic pass-through, exactly like any other
// runtime.
func TestProberBroker_ProjectBlindStop_KeepsGenericPassThrough(t *testing.T) {
	stops := &stopRecorder{}
	srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, stops)

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/abc123/stop", nil), "abc123", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if got := stops.calls(); len(got) != 1 || got[0] != "abc123" {
		t.Errorf("Runtime.Stop calls = %v, want [abc123]", got)
	}
}

// TestRecordlessActorProbe_DistinctUIDsSameNameAcrossEndpoints_NotCollapsed
// pins the other half of the dedupe fix: two DIFFERENT actors that happen to
// share an atespace name (substrateAtespaceName is derived only from
// projectID, so it is identical for two ateapi endpoints) and the same actor
// name, but carry different UIDs because they are actually different
// backing actors on two different endpoints, must both be reported — a key
// that falls back to atespace/name once UIDs differ would wrongly collapse
// them to one, under-reporting the count and hiding one actor's name from
// the operator entirely.
func TestRecordlessActorProbe_DistinctUIDsSameNameAcrossEndpoints_NotCollapsed(t *testing.T) {
	srv, fc := newTestSubstrateBrokerServer(t)
	fc.putActor(gapAtespaceB, "ghost", "uid-endpoint-1")

	// A second, independent SubstrateRuntime instance over its own fake
	// ateapi client stands in for a second substrate manager pointed at a
	// different ateapi endpoint: same atespace name (same projectID), same
	// actor name, but a different backing actor (different UID).
	fc2 := newFakeSubstrateControlClient(&substrateEgressRecorder{})
	fc2.putActor(gapAtespaceB, "ghost", "uid-endpoint-2")
	rt2 := runtime.NewSubstrateRuntimeForTest(fc2, substrate.NewRouterClient(""), nil, config.V1SubstrateConfig{})
	addAuxRuntime(t, srv, "substrate-second-endpoint", rt2)

	atespace, names, err := recordlessActorProbe(context.Background(), srv.allManagers(), gapProjBID)
	if err != nil {
		t.Fatalf("recordlessActorProbe() error = %v", err)
	}
	if atespace != gapAtespaceB {
		t.Errorf("atespace = %q, want %q", atespace, gapAtespaceB)
	}
	if len(names) != 2 {
		t.Errorf("names = %v, want exactly two entries: distinct actors (different UIDs) that merely share an atespace/name must not be collapsed", names)
	}
}
