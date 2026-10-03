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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Coverage for the prober-path stop lookup's project isolation, the
// delete-path probe over auxiliary runtimes, the 409 message contents, the
// sorted auxiliary iteration, and the empty-UID dedupe fallback.

const isoProjAID = "aaaaaaaaaaaa"

// projADev is project A's "dev" agent, as a runtime whose List ignores its
// label filter might return it for a project B request.
func projADev(containerID string) api.AgentInfo {
	return api.AgentInfo{
		Name:        "dev",
		ContainerID: containerID,
		Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: isoProjAID},
	}
}

// The error-preserving stop lookup must never select another project's
// same-slug agent, whichever stage the entry comes from: the default
// runtime's project-scoped List, or an auxiliary runtime's. The runtimes
// here ignore their label filter, so only the broker's own project check
// stands between a project B stop and project A's agent.
func TestProberBroker_StopNeverTargetsAnotherProjectsSameSlugAgent(t *testing.T) {
	t.Run("default runtime lists the other project's agent", func(t *testing.T) {
		stops := &stopRecorder{}
		srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{projADev("c-proja")}, nil
		}, stops)

		w := httptest.NewRecorder()
		srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
		if w.Code != http.StatusAccepted {
			t.Errorf("status=%d body=%s, want 202 (not found in project B)", w.Code, w.Body.String())
		}
		if got := stops.calls(); len(got) != 0 {
			t.Errorf("Stop called %v: a project B stop reached project A's agent", got)
		}
	})

	t.Run("auxiliary runtime lists the other project's agent", func(t *testing.T) {
		defaultStops, auxStops := &stopRecorder{}, &stopRecorder{}
		srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, defaultStops)
		addAuxRuntime(t, srv, "docker", &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{projADev("c-proja-aux")}, nil
			},
			StopFunc: auxStops.stop,
		})

		w := httptest.NewRecorder()
		srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
		if w.Code != http.StatusAccepted {
			t.Errorf("status=%d body=%s, want 202 (not found in project B)", w.Code, w.Body.String())
		}
		if got := append(defaultStops.calls(), auxStops.calls()...); len(got) != 0 {
			t.Errorf("Stop called %v: a project B stop reached project A's agent", got)
		}
	})
}

// A prober registered only as an auxiliary runtime must still be probed on
// delete: an absent slug in a project whose atespace holds a record-less
// actor is 409, never the idempotent 404.
func TestProberAsAuxiliaryRuntime_DeleteFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)
	addAuxRuntime(t, srv, "substrate", &proberMockRuntime{
		MockRuntime: &runtime.MockRuntime{
			NameFunc: func() string { return "substrate" },
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		},
		recordless: []string{"projb--ghost"},
	})

	w := httptest.NewRecorder()
	srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
	if w.Code != http.StatusConflict || decodeBrokerAPIError(t, w) != ErrCodeAgentIdentityUnknown {
		t.Errorf("status=%d body=%s, want 409 %s", w.Code, w.Body.String(), ErrCodeAgentIdentityUnknown)
	}
}

// The 409 body is what a hub/CLI caller sees: it must stay generic across
// every runtime, so it carries the record-less count but never the
// runtime's own scope (e.g. an atespace) or a runtime-specific remedy
// pointer — those go to the broker's own log only. True for both delete and
// stop.
func TestSubstrateBroker_IdentityUnknownBody_GenericCountOnly(t *testing.T) {
	cases := []struct {
		name string
		call func(*Server, *httptest.ResponseRecorder)
	}{
		{"delete", func(srv *Server, w *httptest.ResponseRecorder) {
			srv.deleteAgent(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/dev", nil), "dev", gapProjBID)
		}},
		{"stop", func(srv *Server, w *httptest.ResponseRecorder) {
			srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fc := newTestSubstrateBrokerServer(t)
			fc.putActor(gapAtespaceB, "projb--ghost1", "uid-ghost1")
			fc.putActor(gapAtespaceB, "projb--ghost2", "uid-ghost2")

			w := httptest.NewRecorder()
			tc.call(srv, w)
			if w.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", w.Code, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body is not an ErrorResponse: %v (%s)", err, w.Body.String())
			}
			if !strings.Contains(resp.Error.Message, "2 actor(s)") {
				t.Errorf("message %q does not contain %q", resp.Error.Message, "2 actor(s)")
			}
			for _, notWant := range []string{gapAtespaceB, "atespace", "deploy/substrate/README.md"} {
				if strings.Contains(resp.Error.Message, notWant) {
					t.Errorf("message %q must stay generic, but contains %q", resp.Error.Message, notWant)
				}
			}
		})
	}
}

// When two auxiliary runtimes both hold a project-matched entry for the
// slug, the lookup resolves the sorted-first runtime's
// entry every time, never whichever one Go's map iteration visits first.
func TestLookupAgentTarget_TwoAuxMatches_SortedFirstDeterministic(t *testing.T) {
	matching := func(containerID string) *runtime.MockRuntime {
		return &runtime.MockRuntime{
			NameFunc: func() string { return containerID },
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{
					Name:        "dev",
					ContainerID: containerID,
					Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: gapProjBID},
				}}, nil
			},
		}
	}
	for i := 0; i < 20; i++ {
		srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, &stopRecorder{})
		// Registered in reverse order, so insertion order can't stand in
		// for sorting.
		addAuxRuntime(t, srv, "aux-b", matching("c-b"))
		addAuxRuntime(t, srv, "aux-a", matching("c-a"))

		target, mgr, rt, err := srv.lookupAgentTarget(context.Background(), "dev", gapProjBID)
		if err != nil || target != "c-a" {
			t.Fatalf("iteration %d: lookupAgentTarget = (%q, %v), want (\"c-a\", nil)", i, target, err)
		}
		if want := srv.auxiliaryRuntimes["aux-a"].Manager; mgr != want {
			t.Fatalf("iteration %d: lookupAgentTarget manager = %v, want aux-a's manager %v", i, mgr, want)
		}
		if want := srv.auxiliaryRuntimes["aux-a"].Runtime; rt != want {
			t.Fatalf("iteration %d: lookupAgentTarget runtime = %v, want aux-a's runtime %v", i, rt, want)
		}
	}
}

// emptyUIDProber reports record-less actors with no UID, which only the
// defensive fallback in recordlessActorProbe handles.
type emptyUIDProber struct {
	*runtime.MockRuntime
	names []string
}

func (p *emptyUIDProber) RecordlessActors(context.Context, string) (string, []runtime.RecordlessActor, error) {
	actors := make([]runtime.RecordlessActor, len(p.names))
	for i, n := range p.names {
		actors[i] = runtime.RecordlessActor{Name: n}
	}
	return gapAtespaceB, actors, nil
}

// An entry without a UID is still counted, keyed by atespace/name: two
// distinct names both count, and it is never silently dropped.
func TestRecordlessActorProbe_EmptyUIDFallsBackToAtespaceNameKey(t *testing.T) {
	rt := &emptyUIDProber{
		MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "substrate" }},
		names:       []string{"projb--ghost1", "projb--ghost2", "projb--ghost1"},
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)

	atespace, names, err := recordlessActorProbe(context.Background(), []agent.Manager{mgr}, gapProjBID)
	if err != nil {
		t.Fatalf("recordlessActorProbe() error = %v", err)
	}
	if atespace != gapAtespaceB {
		t.Errorf("atespace = %q, want %q", atespace, gapAtespaceB)
	}
	if len(names) != 2 || names[0] != "projb--ghost1" || names[1] != "projb--ghost2" {
		t.Errorf("names = %v, want [projb--ghost1 projb--ghost2]", names)
	}
}
