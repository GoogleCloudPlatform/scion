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

// These tests pin cases the tests in exec_lookup_test.go do not reach: a
// match on an auxiliary runtime that is not the sorted-first one, a match on
// the default runtime while an auxiliary runtime also reports the slug,
// reset-auth's own List-error path failing closed, and a matched manager
// with no paired runtime. Without them, a manager-to-runtime resolution
// step that picked "the sorted-first aux runtime" positionally (ignoring
// which manager actually matched) could pass unnoticed, as could a
// reset-auth fail-open on a List error or a silent fallback to the default
// runtime when no runtime was paired with the match.
//
// lookupAgentTarget returns the matched agent.Manager and scionrt.Runtime
// together, paired at the stage (default or a specific auxiliary runtime)
// that produced the match — there is no separate runtimeForManager step
// that could get that pairing wrong. execCommand/resetAuth use the returned
// runtime directly and fail closed (503) if it is ever nil.

// emptyAuxRuntimeExec is an aux runtime that lists nothing (so it never
// matches) and records any Exec/ExecWithStdin it wrongly receives.
func emptyAuxRuntimeExec(name string, rec *execRecorder) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		ExecFunc: rec.exec,
	}
}

// transientListFailAuxRuntime fails its first List call and reports the
// agent (container c-a, no project label) on every later call.
func transientListFailAuxRuntime(name string, rec *execRecorder) *runtime.MockRuntime {
	var mu sync.Mutex
	var calls int
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return nil, errors.New("simulated transient list failure")
			}
			return []api.AgentInfo{{
				Name:        "dev",
				ContainerID: "c-a",
				Labels:      map[string]string{"scion.name": "dev"},
			}}, nil
		},
		ExecFunc: rec.exec,
	}
}

// Mirror of TestExecCommand_AuxListErrorThenMatch_DoesNotDispatchToDefaultRuntime
// for reset-auth: when an auxiliary list call fails, the lookup is
// classified ErrAgentListUnavailable and the handler returns 503 without
// dispatching — it must not write a token to, or signal, any runtime, and
// the raw listing error never reaches the response body.
func TestResetAuth_AuxListErrorThenMatch_DoesNotDispatchToAnyRuntime(t *testing.T) {
	defaultExec, auxExec := &execRecorder{}, &execRecorder{}
	srv := newPlainDockerBrokerExec(t, defaultExec)
	addAuxRuntime(t, srv, "aux-a", transientListFailAuxRuntime("aux-a", auxExec))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/reset-auth", strings.NewReader(`{"token":"t0k3n"}`))
	srv.resetAuth(w, req, "dev", "")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want exactly 503 (ErrAgentListUnavailable on the aux runtime's List error)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "simulated transient list failure") {
		t.Fatalf("body=%s must not contain the raw aux List error text", w.Body.String())
	}
	if got := defaultExec.calls(); len(got) != 0 {
		t.Fatalf("default runtime exec calls = %v, want none", got)
	}
	if got := auxExec.calls(); len(got) != 0 {
		t.Fatalf("aux-a exec calls = %v, want none", got)
	}
}

// The single matching aux runtime is NOT the sorted-first one: aux-a lists
// nothing, aux-b holds the agent. Dispatch must follow the manager/runtime
// pair that produced the match, not a positional (sorted-first) choice.
// Pinned two ways: the HTTP-level Exec calls, and a direct check that
// lookupAgentTarget itself returns aux-b's manager AND runtime together —
// the pair execCommand/resetAuth now use directly, with no intermediate
// manager-to-runtime resolution step left to pick the wrong one.
func TestExecCommand_MatchOnLaterSortedAux_DispatchesToMatchingManager(t *testing.T) {
	defaultExec, recA, recB := &execRecorder{}, &execRecorder{}, &execRecorder{}
	srv := newPlainDockerBrokerExec(t, defaultExec)
	addAuxRuntime(t, srv, "aux-a", emptyAuxRuntimeExec("aux-a", recA))
	addAuxRuntime(t, srv, "aux-b", matchingAuxRuntimeExec("aux-b", "c-b", recB))

	w := doExecLookupTest(srv)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	a, b, def := recA.calls(), recB.calls(), defaultExec.calls()
	if len(b) != 1 || b[0] != "c-b" || len(a) != 0 || len(def) != 0 {
		t.Fatalf("Exec calls aux-a=%v aux-b=%v default=%v, want only aux-b [c-b]", a, b, def)
	}

	target, mgr, rt, err := srv.lookupAgentTarget(context.Background(), "dev", stopLookupProjectID)
	if err != nil || target != "c-b" {
		t.Fatalf("lookupAgentTarget = (%q, %v), want (\"c-b\", nil)", target, err)
	}
	if want := srv.auxiliaryRuntimes["aux-b"].Manager; mgr != want {
		t.Fatalf("lookupAgentTarget manager = %v, want aux-b's manager %v", mgr, want)
	}
	if want := srv.auxiliaryRuntimes["aux-b"].Runtime; rt != want {
		t.Fatalf("lookupAgentTarget runtime = %v, want aux-b's runtime %v", rt, want)
	}
}

func TestResetAuth_MatchOnLaterSortedAux_DispatchesToMatchingManager(t *testing.T) {
	defaultExec, recA, recB := &execRecorder{}, &execRecorder{}, &execRecorder{}
	srv := newPlainDockerBrokerExec(t, defaultExec)
	addAuxRuntime(t, srv, "aux-a", emptyAuxRuntimeExec("aux-a", recA))
	addAuxRuntime(t, srv, "aux-b", matchingAuxRuntimeExec("aux-b", "c-b", recB))

	w := doResetAuthLookupTest(srv)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	a, b, def := recA.calls(), recB.calls(), defaultExec.calls()
	if len(b) != 2 || b[0] != "c-b" || b[1] != "c-b" || len(a) != 0 || len(def) != 0 {
		t.Fatalf("calls aux-a=%v aux-b=%v default=%v, want only aux-b [c-b c-b]", a, b, def)
	}

	target, mgr, rt, err := srv.lookupAgentTarget(context.Background(), "dev", stopLookupProjectID)
	if err != nil || target != "c-b" {
		t.Fatalf("lookupAgentTarget = (%q, %v), want (\"c-b\", nil)", target, err)
	}
	if want := srv.auxiliaryRuntimes["aux-b"].Manager; mgr != want {
		t.Fatalf("lookupAgentTarget manager = %v, want aux-b's manager %v", mgr, want)
	}
	if want := srv.auxiliaryRuntimes["aux-b"].Runtime; rt != want {
		t.Fatalf("lookupAgentTarget runtime = %v, want aux-b's runtime %v", rt, want)
	}
}

// When the default runtime holds the agent, it is authoritative (the lookup
// consults it first) even though an aux runtime also reports a same-slug
// entry; exec must dispatch to the default runtime only. Also pinned
// directly: lookupAgentTarget must pair the default manager with the
// default runtime, not with whichever runtime an aux match would have used.
func TestExecCommand_DefaultRuntimeMatch_DispatchesToDefaultRuntime(t *testing.T) {
	defaultExec, auxExec := &execRecorder{}, &execRecorder{}
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name:        "dev",
				ContainerID: "c-default",
				Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: stopLookupProjectID},
			}}, nil
		},
		ExecFunc: defaultExec.exec,
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)
	addAuxRuntime(t, srv, "aux-a", matchingAuxRuntimeExec("aux-a", "c-a", auxExec))

	w := doExecLookupTest(srv)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	def, aux := defaultExec.calls(), auxExec.calls()
	if len(def) != 1 || def[0] != "c-default" || len(aux) != 0 {
		t.Fatalf("Exec calls default=%v aux-a=%v, want only default [c-default]", def, aux)
	}

	target, gotMgr, gotRt, err := srv.lookupAgentTarget(context.Background(), "dev", stopLookupProjectID)
	if err != nil || target != "c-default" {
		t.Fatalf("lookupAgentTarget = (%q, %v), want (\"c-default\", nil)", target, err)
	}
	if gotMgr != mgr {
		t.Fatalf("lookupAgentTarget manager = %v, want the default manager %v", gotMgr, mgr)
	}
	if gotRt != rt {
		t.Fatalf("lookupAgentTarget runtime = %v, want the default runtime %v", gotRt, rt)
	}
}

// addAuxRuntimeWithNilRuntime registers an auxiliary manager without its
// paired runtime — a state that should be unreachable in production (every
// real auxiliary registration builds the manager and stores the runtime it
// wraps together, in discoverAuxiliaryRuntimes and its counterpart
// registration site in handlers.go), but which pins the defensive guard in
// execCommand/resetAuth against ever silently falling back to a different
// runtime than the one that matched.
func addAuxRuntimeWithNilRuntime(t *testing.T, srv *Server, name string, mgr agent.Manager) {
	t.Helper()
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[name] = auxiliaryRuntime{Runtime: nil, Manager: mgr}
	srv.auxiliaryRuntimesMu.Unlock()
}

// A match on an auxiliary manager with no paired runtime must fail closed
// (503) rather than fall back to the default runtime or any other runtime —
// there is nothing else it is safe to dispatch Exec to.
func TestExecCommand_NilPairedRuntime_FailsClosedWithoutAnyExec(t *testing.T) {
	defaultExec, auxExec := &execRecorder{}, &execRecorder{}
	srv := newPlainDockerBrokerExec(t, defaultExec)
	mgr := agent.NewManager(matchingAuxRuntimeExec("aux-a", "c-a", auxExec))
	t.Cleanup(mgr.Close)
	addAuxRuntimeWithNilRuntime(t, srv, "aux-a", mgr)

	w := doExecLookupTest(srv)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503 (fail closed on a nil paired runtime)", w.Code, w.Body.String())
	}
	if got := defaultExec.calls(); len(got) != 0 {
		t.Fatalf("default runtime exec calls = %v, want none", got)
	}
	if got := auxExec.calls(); len(got) != 0 {
		t.Fatalf("aux-a exec calls = %v, want none — the matched manager's own runtime was nil", got)
	}
}

// Same guard, exercised through reset-auth: neither the token write nor the
// PID-1 signal may reach any runtime when the matched manager has no paired
// runtime.
func TestResetAuth_NilPairedRuntime_FailsClosedWithoutAnyExec(t *testing.T) {
	defaultExec, auxExec := &execRecorder{}, &execRecorder{}
	srv := newPlainDockerBrokerExec(t, defaultExec)
	mgr := agent.NewManager(matchingAuxRuntimeExec("aux-a", "c-a", auxExec))
	t.Cleanup(mgr.Close)
	addAuxRuntimeWithNilRuntime(t, srv, "aux-a", mgr)

	w := doResetAuthLookupTest(srv)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503 (fail closed on a nil paired runtime)", w.Code, w.Body.String())
	}
	if got := defaultExec.calls(); len(got) != 0 {
		t.Fatalf("default runtime exec calls = %v, want none", got)
	}
	if got := auxExec.calls(); len(got) != 0 {
		t.Fatalf("aux-a exec calls = %v, want none — the matched manager's own runtime was nil", got)
	}
}
