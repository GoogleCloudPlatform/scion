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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// exec and reset-auth derive both the container id to act on and the
// manager/runtime to dispatch through from the same strict, sorted lookup
// (lookupAgentTarget, via LookupContainerID), exactly like stop/restart —
// never a second, independent call that scans auxiliary runtimes in
// unsorted (Go map) order and never reports a listing failure, which could
// resolve to a different backend than the one the sorted lookup matched.
// These tests pin that single-lookup invariant.

// execRecorder records the container ids a MockRuntime's Exec/ExecWithStdin
// receive.
type execRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *execRecorder) exec(_ context.Context, id string, _ []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return "", nil
}

func (r *execRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// newPlainDockerBrokerExec returns a broker whose default runtime is a plain
// (capability-free) "docker" MockRuntime with no agents, recording any Exec
// call it receives (which would mean exec/reset-auth dispatched to the
// wrong, default runtime).
func newPlainDockerBrokerExec(t *testing.T, defaultExec *execRecorder) *Server {
	t.Helper()
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		// ExecWithStdin falls back to ExecFunc when unset (see MockRuntime),
		// so this single func covers both exec's Exec call and reset-auth's
		// ExecWithStdin/Exec calls.
		ExecFunc: defaultExec.exec,
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	return New(DefaultServerConfig(), mgr, rt)
}

// matchingAuxRuntimeExec reports one project-labelled "dev" entry with the
// given container ID on every List call, and records every Exec/ExecWithStdin
// call it receives.
func matchingAuxRuntimeExec(name, containerID string, rec *execRecorder) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name:        "dev",
				ContainerID: containerID,
				Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: stopLookupProjectID},
			}}, nil
		},
		ExecFunc: rec.exec,
	}
}

func doExecLookupTest(srv *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/exec", strings.NewReader(`{"command":["echo","hi"]}`))
	srv.execCommand(w, req, "dev", stopLookupProjectID)
	return w
}

func doResetAuthLookupTest(srv *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/reset-auth", strings.NewReader(`{"token":"t0k3n"}`))
	srv.resetAuth(w, req, "dev", stopLookupProjectID)
	return w
}

// 1. exec dispatches to the manager whose List call produced the matching
// target, not a default or randomly-resolved one. Registered in reverse
// name order and looped, so insertion order and Go's randomized map
// iteration can't stand in for the deterministic, sorted-first pick the
// strict lookup makes.
func TestExecCommand_TwoAuxMatches_DispatchesToMatchingManager(t *testing.T) {
	for i := 0; i < 30; i++ {
		defaultExec := &execRecorder{}
		recA, recB := &execRecorder{}, &execRecorder{}
		srv := newPlainDockerBrokerExec(t, defaultExec)
		// Registered in reverse order, so insertion order can't stand in for
		// sorting.
		addAuxRuntime(t, srv, "aux-b", matchingAuxRuntimeExec("aux-b", "c-b", recB))
		addAuxRuntime(t, srv, "aux-a", matchingAuxRuntimeExec("aux-a", "c-a", recA))

		w := doExecLookupTest(srv)
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: status=%d body=%s, want 200", i, w.Code, w.Body.String())
		}
		a, b, def := recA.calls(), recB.calls(), defaultExec.calls()
		if len(a) != 1 || a[0] != "c-a" || len(b) != 0 || len(def) != 0 {
			t.Fatalf("iteration %d: Exec calls aux-a=%v aux-b=%v default=%v, want only aux-a [c-a]", i, a, b, def)
		}
	}
}

// 2. reset-auth dispatches its token write and PID-1 signal through the
// manager whose List call produced the matching target, exactly like exec.
func TestResetAuth_TwoAuxMatches_DispatchesToMatchingManager(t *testing.T) {
	for i := 0; i < 30; i++ {
		defaultExec := &execRecorder{}
		recA, recB := &execRecorder{}, &execRecorder{}
		srv := newPlainDockerBrokerExec(t, defaultExec)
		addAuxRuntime(t, srv, "aux-b", matchingAuxRuntimeExec("aux-b", "c-b", recB))
		addAuxRuntime(t, srv, "aux-a", matchingAuxRuntimeExec("aux-a", "c-a", recA))

		w := doResetAuthLookupTest(srv)
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: status=%d body=%s, want 200", i, w.Code, w.Body.String())
		}
		a, b, def := recA.calls(), recB.calls(), defaultExec.calls()
		// The token write (ExecWithStdin) and the USR2 signal (Exec) both go
		// through aux-a: two calls, both against c-a.
		if len(a) != 2 || a[0] != "c-a" || a[1] != "c-a" || len(b) != 0 || len(def) != 0 {
			t.Fatalf("iteration %d: calls aux-a=%v aux-b=%v default=%v, want only aux-a [c-a c-a]", i, a, b, def)
		}
	}
}

// 3. A transient List failure on the one auxiliary runtime that actually
// holds the agent must not cause exec to act on the default runtime.
//
// A two-call approach — resolving the dispatch runtime with a separate,
// independent lookup call (resolveRuntimeForAgent) and the target id with a
// second, independent call (LookupContainerID) — cannot make this
// guarantee: the first call would see the aux runtime's List fail — and,
// since resolveAgentRuntimeTarget never treats a List error as anything but
// "no match", would silently fall through to its unconditional final
// fallback, the default runtime. The second call, made moments later,
// would find the agent on the aux runtime (the "transient" failure having
// passed) and return its container id. Combined, exec would send the aux
// runtime's container id to the *default* runtime's Exec — a call that
// could not possibly succeed, and on a real runtime would act on whatever
// the default happened to resolve the id to.
//
// A single lookup call, used for both the target and the manager, cannot
// produce that split: with only one List attempt on the aux runtime (the
// one made inside lookupAgentTarget), the transient failure is the one that
// counts. When an auxiliary list call fails, the lookup is classified
// ErrAgentListUnavailable and the handler returns 503 without dispatching —
// neither runtime's Exec is ever called, and the raw listing error never
// reaches the response body.
//
// projectID is empty here so neither lookup's backward-compatibility
// fallback stage (which retries every runtime a second time) masks the aux
// runtime's List failure behind a second, successful attempt.
func TestExecCommand_AuxListErrorThenMatch_DoesNotDispatchToDefaultRuntime(t *testing.T) {
	defaultExec := &execRecorder{}
	auxExec := &execRecorder{}
	srv := newPlainDockerBrokerExec(t, defaultExec)

	var calls int
	var mu sync.Mutex
	aux := &runtime.MockRuntime{
		NameFunc: func() string { return "aux-a" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				// The first (and only) call observes a
				// transient failure.
				return nil, errors.New("simulated transient list failure")
			}
			// A later call (only reachable by a second, independent lookup)
			// finds the agent, genuinely present on this runtime the whole
			// time.
			return []api.AgentInfo{{
				Name:        "dev",
				ContainerID: "c-a",
				Labels:      map[string]string{"scion.name": "dev"},
			}}, nil
		},
		ExecFunc: auxExec.exec,
	}
	addAuxRuntime(t, srv, "aux-a", aux)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/exec", strings.NewReader(`{"command":["echo","hi"]}`))
	srv.execCommand(w, req, "dev", "")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want exactly 503 (ErrAgentListUnavailable on the aux runtime's List error)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "simulated transient list failure") {
		t.Fatalf("body=%s must not contain the raw aux List error text", w.Body.String())
	}
	if got := defaultExec.calls(); len(got) != 0 {
		t.Fatalf("default runtime Exec calls = %v, want none — must never act on the wrong runtime", got)
	}
	if got := auxExec.calls(); len(got) != 0 {
		t.Fatalf("aux-a Exec calls = %v, want none — the single lookup must see the transient failure, not a later success", got)
	}
}
