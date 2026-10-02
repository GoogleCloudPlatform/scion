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
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// These tests pin two things the tests in exec_lookup_test.go and
// exec_lookup_gap_test.go do not reach.
//
// 1. The backward-compatibility (unlabelled) fallback stage of
// lookupAgentTarget pairs its own runtime with its match: the default
// runtime when the default manager matches, the matching aux runtime when an
// aux manager matches. The existing tests only reach stage 1 (project
// labelled entries), so neither the `matchRuntime = s.runtime` reset nor the
// `matchRuntime = auxRuntime` assignment in the fallback stage was pinned.
// Because stage 1's aux scan returns a nil runtime when nothing matches,
// dropping the reset makes a legacy unlabelled agent on the default runtime
// unreachable via exec/reset-auth (503); dropping the aux assignment sends
// the aux container id to the default runtime.
//
// 2. The aux-dispatch reset-auth tests record only container ids, and
// MockRuntime.ExecWithStdin falls back to ExecFunc, so they cannot tell the
// stdin token write from the `kill -USR2 1` signal. The tests below record
// which method was called, the command, and the stdin payload.

// execCall is one Exec or ExecWithStdin call observed by a callRecorder.
type execCall struct {
	Method string // "Exec" or "ExecWithStdin"
	ID     string
	Cmd0   string
	Stdin  string
}

type callRecorder struct {
	mu    sync.Mutex
	calls []execCall
}

func (r *callRecorder) exec(_ context.Context, id string, cmd []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, execCall{Method: "Exec", ID: id, Cmd0: firstArg(cmd)})
	return "", nil
}

func (r *callRecorder) execWithStdin(_ context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	var b []byte
	if stdin != nil {
		b, _ = io.ReadAll(stdin)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, execCall{Method: "ExecWithStdin", ID: id, Cmd0: firstArg(cmd), Stdin: string(b)})
	return "", nil
}

func (r *callRecorder) got() []execCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]execCall(nil), r.calls...)
}

func firstArg(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	return cmd[0]
}

// recordingRuntime lists the given entries (ignoring the filter, as a
// runtime that doesn't implement label filtering would) and records every
// Exec/ExecWithStdin call.
func recordingRuntime(name string, entries []api.AgentInfo, rec *callRecorder) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return append([]api.AgentInfo(nil), entries...), nil
		},
		ExecFunc:          rec.exec,
		ExecWithStdinFunc: rec.execWithStdin,
	}
}

// addAuxRuntime registers rt as an auxiliary runtime under name, with its
// own manager.
func addAuxRuntime(t *testing.T, srv *Server, name string, rt runtime.Runtime) {
	t.Helper()
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[name] = auxiliaryRuntime{Runtime: rt, Manager: mgr}
	srv.auxiliaryRuntimesMu.Unlock()
}

// unlabelledDev is a legacy "dev" entry with no project label, reachable
// only through lookupAgentTarget's backward-compatibility fallback stage.
func unlabelledDev(containerID string) []api.AgentInfo {
	return []api.AgentInfo{{
		Name:        "dev",
		ContainerID: containerID,
		Labels:      map[string]string{"scion.name": "dev"},
	}}
}

// resetAuthCalls is the exact call sequence reset-auth must make against the
// runtime that holds the target: the token over stdin, then the PID 1
// signal, both on the same container.
func resetAuthCalls(id string) []execCall {
	return []execCall{
		{Method: "ExecWithStdin", ID: id, Cmd0: "sh", Stdin: "t0k3n"},
		{Method: "Exec", ID: id, Cmd0: "kill"},
	}
}

func execCalls(id string) []execCall {
	return []execCall{{Method: "Exec", ID: id, Cmd0: "echo"}}
}

// fallbackOnDefaultBroker: the default runtime holds an unlabelled "dev";
// one aux runtime is registered and lists nothing.
func fallbackOnDefaultBroker(t *testing.T, defRec, auxRec *callRecorder) (*Server, agent.Manager, runtime.Runtime) {
	t.Helper()
	rt := recordingRuntime("docker", unlabelledDev("c-legacy"), defRec)
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)
	addAuxRuntime(t, srv, "aux-a", recordingRuntime("aux-a", nil, auxRec))
	return srv, mgr, rt
}

// fallbackOnLaterAuxBroker: the default runtime and the sorted-first aux
// runtime list nothing; the later-sorted aux-b holds an unlabelled "dev".
func fallbackOnLaterAuxBroker(t *testing.T, defRec, recA, recB *callRecorder) *Server {
	t.Helper()
	rt := recordingRuntime("docker", nil, defRec)
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)
	addAuxRuntime(t, srv, "aux-a", recordingRuntime("aux-a", nil, recA))
	addAuxRuntime(t, srv, "aux-b", recordingRuntime("aux-b", unlabelledDev("c-b"), recB))
	return srv
}

func TestLookupAgentTarget_UnlabelledFallbackOnDefault_PairsDefaultManagerAndRuntime(t *testing.T) {
	defRec, auxRec := &callRecorder{}, &callRecorder{}
	srv, mgr, rt := fallbackOnDefaultBroker(t, defRec, auxRec)

	target, gotMgr, gotRt, err := srv.lookupAgentTarget(context.Background(), "dev", stopLookupProjectID)

	if err != nil || target != "c-legacy" {
		t.Fatalf("lookupAgentTarget = (%q, %v), want (\"c-legacy\", nil)", target, err)
	}
	if gotMgr != mgr {
		t.Fatalf("manager = %v, want the default manager %v", gotMgr, mgr)
	}
	if gotRt != rt {
		t.Fatalf("runtime = %v, want the default runtime %v (the fallback stage must pair its own runtime, not carry stage 1's)", gotRt, rt)
	}
}

func TestExecCommand_UnlabelledFallbackOnDefault_DispatchesToDefaultRuntime(t *testing.T) {
	defRec, auxRec := &callRecorder{}, &callRecorder{}
	srv, _, _ := fallbackOnDefaultBroker(t, defRec, auxRec)

	w := doExecLookupTest(srv)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (legacy unlabelled agent on the default runtime)", w.Code, w.Body.String())
	}
	if got, want := defRec.got(), execCalls("c-legacy"); !reflect.DeepEqual(got, want) {
		t.Fatalf("default runtime calls = %+v, want %+v", got, want)
	}
	if got := auxRec.got(); len(got) != 0 {
		t.Fatalf("aux-a calls = %+v, want none", got)
	}
}

func TestResetAuth_UnlabelledFallbackOnDefault_WritesAndSignalsOnDefaultRuntime(t *testing.T) {
	defRec, auxRec := &callRecorder{}, &callRecorder{}
	srv, _, _ := fallbackOnDefaultBroker(t, defRec, auxRec)

	w := doResetAuthLookupTest(srv)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (legacy unlabelled agent on the default runtime)", w.Code, w.Body.String())
	}
	if got, want := defRec.got(), resetAuthCalls("c-legacy"); !reflect.DeepEqual(got, want) {
		t.Fatalf("default runtime calls = %+v, want %+v", got, want)
	}
	if got := auxRec.got(); len(got) != 0 {
		t.Fatalf("aux-a calls = %+v, want none", got)
	}
}

func TestLookupAgentTarget_UnlabelledFallbackOnLaterSortedAux_PairsThatAuxManagerAndRuntime(t *testing.T) {
	defRec, recA, recB := &callRecorder{}, &callRecorder{}, &callRecorder{}
	srv := fallbackOnLaterAuxBroker(t, defRec, recA, recB)

	target, mgr, rt, err := srv.lookupAgentTarget(context.Background(), "dev", stopLookupProjectID)

	if err != nil || target != "c-b" {
		t.Fatalf("lookupAgentTarget = (%q, %v), want (\"c-b\", nil)", target, err)
	}
	if want := srv.auxiliaryRuntimes["aux-b"].Manager; mgr != want {
		t.Fatalf("manager = %v, want aux-b's manager %v", mgr, want)
	}
	if want := srv.auxiliaryRuntimes["aux-b"].Runtime; rt != want {
		t.Fatalf("runtime = %v, want aux-b's runtime %v", rt, want)
	}
}

func TestExecCommand_UnlabelledFallbackOnLaterSortedAux_DispatchesToThatAux(t *testing.T) {
	defRec, recA, recB := &callRecorder{}, &callRecorder{}, &callRecorder{}
	srv := fallbackOnLaterAuxBroker(t, defRec, recA, recB)

	w := doExecLookupTest(srv)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got, want := recB.got(), execCalls("c-b"); !reflect.DeepEqual(got, want) {
		t.Fatalf("aux-b calls = %+v, want %+v", got, want)
	}
	if d, a := defRec.got(), recA.got(); len(d) != 0 || len(a) != 0 {
		t.Fatalf("calls default=%+v aux-a=%+v, want none", d, a)
	}
}

func TestResetAuth_UnlabelledFallbackOnLaterSortedAux_WritesAndSignalsOnThatAux(t *testing.T) {
	defRec, recA, recB := &callRecorder{}, &callRecorder{}, &callRecorder{}
	srv := fallbackOnLaterAuxBroker(t, defRec, recA, recB)

	w := doResetAuthLookupTest(srv)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got, want := recB.got(), resetAuthCalls("c-b"); !reflect.DeepEqual(got, want) {
		t.Fatalf("aux-b calls = %+v, want %+v", got, want)
	}
	if d, a := defRec.got(), recA.got(); len(d) != 0 || len(a) != 0 {
		t.Fatalf("calls default=%+v aux-a=%+v, want none", d, a)
	}
}

// Stage-1 (project-labelled) match on a later-sorted aux runtime: the token
// must reach that runtime over stdin (ExecWithStdin, token verbatim), and
// the PID 1 signal must follow via Exec on the same container. Strengthens
// TestResetAuth_TwoAuxMatches_DispatchesToMatchingManager and
// TestResetAuth_MatchOnLaterSortedAux_DispatchesToMatchingManager, whose
// id-only assertions pass even if the write and the signal are swapped, the
// token is dropped, or the signal is never sent, as long as two calls hit
// the right container.
func TestResetAuth_AuxMatch_TokenViaStdinThenSignalOnSameRuntime(t *testing.T) {
	defRec, recA, recB := &callRecorder{}, &callRecorder{}, &callRecorder{}
	rt := recordingRuntime("docker", nil, defRec)
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(DefaultServerConfig(), mgr, rt)
	addAuxRuntime(t, srv, "aux-a", recordingRuntime("aux-a", nil, recA))
	addAuxRuntime(t, srv, "aux-b", recordingRuntime("aux-b", []api.AgentInfo{{
		Name:        "dev",
		ContainerID: "c-b",
		Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: stopLookupProjectID},
	}}, recB))

	w := doResetAuthLookupTest(srv)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got, want := recB.got(), resetAuthCalls("c-b"); !reflect.DeepEqual(got, want) {
		t.Fatalf("aux-b calls = %+v, want %+v", got, want)
	}
	if d, a := defRec.got(), recA.got(); len(d) != 0 || len(a) != 0 {
		t.Fatalf("calls default=%+v aux-a=%+v, want none", d, a)
	}
}
