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

// ptone/scion#4722: an agent whose saved profile now selects another
// runtime than the one its container runs in. Start (and a stop naming no
// run) must reach the container left in the previous runtime, as restart
// does (TestRestartAgent_OwnRuntimeNoMatch_StopsInPreviousRuntime).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// previousRuntimeFixture is newTestServerForSavedProfileRemap with the
// agent's container left in the broker's default runtime ("docker", mgr)
// while its saved profile selects the "other" runtime, which lists nothing.
// runs counts Run calls in the "other" runtime; stopsAtRun is the default
// runtime's stop count when Run was first called.
type previousRuntimeFixture struct {
	srv        *Server
	mgr        *mockManager
	runs       atomic.Int32
	stopsAtRun atomic.Int32
	logs       *bytes.Buffer
}

func newPreviousRuntimeFixture(t *testing.T) *previousRuntimeFixture {
	t.Helper()
	srv, mgr, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1", "other")
	mgr.agents[0].ContainerID = "old-container"
	if own := agentOwnRuntimeFrom(srv.ensureAgentOwnRuntime(context.Background(), "test-agent-1", "", "")); own == nil || own.rt.Name() != "other" {
		t.Fatalf("own runtime = %+v, want the saved profile's runtime", own)
	}
	f := &previousRuntimeFixture{srv: srv, mgr: mgr, logs: &bytes.Buffer{}}
	f.stopsAtRun.Store(-1)
	remapRuntime.RunFunc = func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
		if f.runs.Add(1) == 1 {
			f.stopsAtRun.Store(int32(mgr.StopCalls()))
		}
		return "new-id", nil
	}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(f.logs, nil))
	return f
}

// addRuntime registers another auxiliary runtime named name, backed by m.
func (f *previousRuntimeFixture) addRuntime(name string, m *mockManager) {
	f.srv.auxiliaryRuntimesMu.Lock()
	defer f.srv.auxiliaryRuntimesMu.Unlock()
	f.srv.auxiliaryRuntimes[name] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return name }},
		Manager: m,
	}
}

func (f *previousRuntimeFixture) do(method, path string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == http.MethodPost && strings.HasSuffix(path, "/start") {
		body = strings.NewReader(`{}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

// AC1: the container in the previous runtime gets one stop before the new
// runtime's Run, and the start is accepted.
func TestStartAgent_OwnRuntimeNoMatch_StopsInPreviousRuntimeBeforeRun(t *testing.T) {
	f := newPreviousRuntimeFixture(t)

	w := f.do(http.MethodPost, "/api/v1/agents/test-agent-1/start")
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body %s; want 202", w.Code, w.Body.String())
	}
	if f.mgr.StopCalls() != 1 || f.mgr.LastStopAgentID() != "old-container" {
		t.Fatalf("stop on the previous runtime: calls=%d target=%q; want 1 old-container", f.mgr.StopCalls(), f.mgr.LastStopAgentID())
	}
	if n := f.runs.Load(); n != 1 {
		t.Fatalf("starts in the new runtime = %d, want 1", n)
	}
	if got := f.stopsAtRun.Load(); got != 1 {
		t.Fatalf("stops on the previous runtime when Run was called = %d, want 1 (stop before Run)", got)
	}
	if !strings.Contains(f.logs.String(), "Start: stopped container left in previous runtime") {
		t.Errorf("no Info log for the stop; logs:\n%s", f.logs.String())
	}
}

// AC2: a failed stop refuses the start with 409 naming the agent and the
// previous runtime; nothing is run in the new runtime, and the runtime's
// raw error stays out of the response.
func TestStartAgent_PreviousRuntimeStopFails_Refused(t *testing.T) {
	f := newPreviousRuntimeFixture(t)
	f.mgr.stopErr = errors.New("connection refused: secret-host:6443")

	w := f.do(http.MethodPost, "/api/v1/agents/test-agent-1/start")
	if w.Code != http.StatusConflict {
		t.Fatalf("start status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "test-agent-1") || !strings.Contains(body, "'docker'") {
		t.Errorf("409 body %s does not name the agent and the previous runtime", body)
	}
	if strings.Contains(body, "secret-host") {
		t.Errorf("409 body %s carries the runtime's raw error", body)
	}
	if f.mgr.StopCalls() != 1 {
		t.Errorf("stop calls = %d, want 1", f.mgr.StopCalls())
	}
	if n := f.runs.Load(); n != 0 {
		t.Fatalf("starts in the new runtime = %d, want 0", n)
	}
}

// A stop answering that the container is already gone is not a refusal:
// nothing is left running in the previous runtime.
func TestStartAgent_PreviousRuntimeContainerAlreadyGone_Proceeds(t *testing.T) {
	f := newPreviousRuntimeFixture(t)
	f.mgr.stopErr = errors.New("container old-container not found")

	w := f.do(http.MethodPost, "/api/v1/agents/test-agent-1/start")
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body %s; want 202", w.Code, w.Body.String())
	}
	if n := f.runs.Load(); n != 1 {
		t.Fatalf("starts in the new runtime = %d, want 1", n)
	}
}

// AC3: containers of the agent in two other runtimes are ambiguous: 409,
// with no stop and no Run.
func TestStartAgent_PreviousRuntimeAmbiguous_Refused(t *testing.T) {
	f := newPreviousRuntimeFixture(t)
	second := &mockManager{agents: []api.AgentInfo{{
		ID: "test-agent-1", Name: "test-agent-1", ContainerID: "second-container",
		ProjectPath: f.mgr.agents[0].ProjectPath, Phase: "running",
	}}}
	f.addRuntime("aux-second", second)

	w := f.do(http.MethodPost, "/api/v1/agents/test-agent-1/start")
	if w.Code != http.StatusConflict {
		t.Fatalf("start status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "test-agent-1") {
		t.Errorf("409 body %s does not name the agent", w.Body.String())
	}
	if f.mgr.StopCalls() != 0 || second.StopCalls() != 0 {
		t.Fatalf("stop calls = %d, %d; want none", f.mgr.StopCalls(), second.StopCalls())
	}
	if n := f.runs.Load(); n != 0 {
		t.Fatalf("starts in the new runtime = %d, want 0", n)
	}
}

// AC4: a runtime that cannot be listed (Forbidden) is logged at Warn and
// skipped; the other runtimes are still searched, and the start proceeds.
func TestStartAgent_PreviousRuntimeSearch_ForbiddenRuntimeSkipped(t *testing.T) {
	for _, tc := range []struct {
		name           string
		defaultHolds   bool
		wantDefStops   int
		wantStopTarget string
	}{
		{name: "container elsewhere is still stopped", defaultHolds: true, wantDefStops: 1, wantStopTarget: "old-container"},
		{name: "no container anywhere", defaultHolds: false, wantDefStops: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreviousRuntimeFixture(t)
			if !tc.defaultHolds {
				f.mgr.agents[0].ContainerID = ""
			}
			f.addRuntime("aux-forbidden", &mockManager{listErr: errors.New(`pods is forbidden: User "broker" cannot list resource "pods"`)})

			w := f.do(http.MethodPost, "/api/v1/agents/test-agent-1/start")
			if w.Code != http.StatusAccepted {
				t.Fatalf("start status = %d, body %s; want 202", w.Code, w.Body.String())
			}
			if n := f.runs.Load(); n != 1 {
				t.Fatalf("starts in the new runtime = %d, want 1", n)
			}
			if f.mgr.StopCalls() != tc.wantDefStops || f.mgr.LastStopAgentID() != tc.wantStopTarget {
				t.Fatalf("stop on the default runtime: calls=%d target=%q; want %d %q",
					f.mgr.StopCalls(), f.mgr.LastStopAgentID(), tc.wantDefStops, tc.wantStopTarget)
			}
			logs := f.logs.String()
			if !strings.Contains(logs, `"level":"WARN","msg":"Start: runtime list failed while searching the other runtimes; skipped"`) {
				t.Errorf("no Warn log for the skipped runtime; logs:\n%s", logs)
			}
		})
	}
}

// AC6: the search keeps delete's project scoping, so a same-slug container
// of another project in another runtime is never matched (and so never
// stopped), while one of the agent's own project is.
func TestPreviousRuntimeMatch_SameSlugOtherProjectNotMatched(t *testing.T) {
	f := newPreviousRuntimeFixture(t)
	own := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), "test-agent-1", "", ""))
	f.mgr.agents = []api.AgentInfo{{
		ID: "test-agent-1", Name: "test-agent-1", ContainerID: "foreign-container", ProjectID: "project-b",
		Labels: map[string]string{"scion.agent": "true", "scion.name": "test-agent-1", "scion.project_id": "project-b"},
	}}

	prior, priorErr := agentMatch{}, error(&agentNotFoundError{slug: "test-agent-1"})
	m, err := f.srv.previousRuntimeMatch(context.Background(), own, "test-agent-1", "project-a", prior, priorErr, "test")
	if !errors.Is(err, ErrAgentNotFound) || m.matched {
		t.Fatalf("match = %+v, err = %v; want the prior not-found result", m, err)
	}

	f.mgr.agents[0].ProjectID = "project-a"
	f.mgr.agents[0].Labels["scion.project_id"] = "project-a"
	m, err = f.srv.previousRuntimeMatch(context.Background(), own, "test-agent-1", "project-a", prior, priorErr, "test")
	if err != nil || m.containerID != "foreign-container" {
		t.Fatalf("match = %+v, err = %v; want the own project's container", m, err)
	}
}
