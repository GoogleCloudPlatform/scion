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

package control

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

// The key store, admission and refusal tests drive the route with a fake
// process (fakeExec) so the interleavings are deterministic. The real
// executor (process group, timeout kill, output drain) is tested in
// exec_linux_test.go.

// execClock is a settable clock.
type execClock struct {
	mu sync.Mutex
	t  time.Time
}

func newExecClock() *execClock { return &execClock{t: time.UnixMilli(1_800_000_000_000)} }

func (c *execClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *execClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeExec counts spawns and lets a test end each run.
type fakeExec struct {
	mu      sync.Mutex
	spawned map[string]int // argv[0] (a per-run tag) -> spawns
	procs   map[string]*fakeProc
	total   atomic.Int64
	fail    bool
}

type fakeProc struct {
	done  chan execFinal
	specs execSpec
}

func (p *fakeProc) wait() execFinal { return <-p.done }

func newFakeExec() *fakeExec {
	return &fakeExec{spawned: map[string]int{}, procs: map[string]*fakeProc{}}
}

func (f *fakeExec) start(spec execSpec) (execProcess, error) {
	if f.fail {
		return nil, fmt.Errorf("start failed for %s", strings.Join(spec.argv, " "))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tag := spec.argv[len(spec.argv)-1]
	f.spawned[tag]++
	f.total.Add(1)
	p := &fakeProc{done: make(chan execFinal, 1), specs: spec}
	f.procs[tag] = p
	return p, nil
}

func (f *fakeExec) spawns(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawned[tag]
}

// end finishes the run tagged tag with exit code code.
func (f *fakeExec) end(t *testing.T, tag string, code int) {
	t.Helper()
	f.mu.Lock()
	p := f.procs[tag]
	f.mu.Unlock()
	if p == nil {
		t.Fatalf("no run %q was spawned", tag)
	}
	outcome := agentcontrol.ExecOutcomeOK
	if code != 0 {
		outcome = agentcontrol.ExecOutcomeNonzero
	}
	p.done <- execFinal{outcome: outcome, exitCode: code, durationMS: 5, outputBytes: 7}
}

// execFixture is a Handler with the exec routes on a fake executor.
type execFixture struct {
	h     *Handler
	route *execRoute
	clk   *execClock
	fx    *fakeExec
	// finishedCh receives each run_id whose tombstone was stored.
	finishedCh chan string
}

func newExecFixture(t *testing.T, mutate ...func(*ExecOptions)) *execFixture {
	t.Helper()
	clk := newExecClock()
	o := ExecOptions{
		UID: 1000, GID: 1000, Username: "scion",
		Env:      func() ([]string, error) { return []string{"PATH=/usr/bin:/bin", "HOME=/tmp"}, nil },
		LaunchID: "launch-1",
		Workdir:  t.TempDir(),
		Now:      clk.Now,
	}
	for _, m := range mutate {
		m(&o)
	}
	h := New(Options{KickTokenRefresh: func() {}, Exec: &o})
	f := &execFixture{h: h, clk: clk, fx: newFakeExec(), finishedCh: make(chan string, 1024)}
	f.route = execRouteOf(t, h)
	f.route.start = f.fx.start
	f.route.finished = func(id string) { f.finishedCh <- id }
	return f
}

// execRouteOf returns h's exec route.
func execRouteOf(t *testing.T, h *Handler) *execRoute {
	t.Helper()
	if h.exec == nil {
		t.Fatal("handler has no exec route")
	}
	return h.exec
}

func uuidN(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// startReq is a valid exec-start request for run n whose argv ends with
// tag (the fake executor's key), not_after one minute after the clock.
func (f *execFixture) startReq(n int, tag string) agentcontrol.ExecStartRequest {
	return agentcontrol.ExecStartRequest{
		V:              1,
		RunID:          uuidN(n),
		Argv:           []string{"true", tag},
		TimeoutS:       60,
		NotAfterUnixMS: f.clk.Now().Add(agentcontrol.ExecNotAfter).UnixMilli(),
	}
}

func postExec(t *testing.T, h http.Handler, route string, body any) (int, agentcontrol.ExecResponse, string) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case []byte:
		raw = b
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, Prefix+route, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp agentcontrol.ExecResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Errorf("answer %d is not JSON: %q", rec.Code, rec.Body.String())
	}
	return rec.Code, resp, rec.Body.String()
}

func (f *execFixture) start(t *testing.T, req agentcontrol.ExecStartRequest) (int, agentcontrol.ExecResponse) {
	t.Helper()
	code, resp, raw := postExec(t, f.h, RouteExecStart, req)
	// Every answer must parse under the hub's schema check.
	if _, err := agentcontrol.ParseExecStartAnswer(code, []byte(raw)); err != nil {
		t.Errorf("exec-start answer %d %s fails the hub schema: %v", code, raw, err)
	}
	return code, resp
}

func (f *execFixture) status(t *testing.T, n int) (int, agentcontrol.ExecResponse) {
	t.Helper()
	code, resp, raw := postExec(t, f.h, RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: uuidN(n)})
	if _, err := agentcontrol.ParseExecStatusAnswer(code, []byte(raw)); err != nil {
		t.Errorf("exec-status answer %d %s fails the hub schema: %v", code, raw, err)
	}
	return code, resp
}

func (f *execFixture) waitFinished(t *testing.T, n int) {
	t.Helper()
	want := uuidN(n)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case id := <-f.finishedCh:
			if id == want {
				return
			}
		case <-deadline:
			t.Fatalf("run %s never finished", want)
		}
	}
}

func TestExecRoutesRegisteredOnlyWithOptions(t *testing.T) {
	routes := New(Options{KickTokenRefresh: func() {}}).Routes()
	if slices.Contains(routes, RouteExecStart) || slices.Contains(routes, RouteExecStatus) {
		t.Fatalf("exec routes advertised without Exec options: %v", routes)
	}
	f := newExecFixture(t)
	routes = f.h.Routes()
	if !slices.Contains(routes, RouteExecStart) || !slices.Contains(routes, RouteExecStatus) {
		t.Fatalf("exec routes not advertised with Exec options: %v", routes)
	}
	for _, r := range []string{RouteExecStart, RouteExecStatus} {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+r, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", r, rec.Code)
		}
	}
}

func TestExecEnabled(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvHubExperiments {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]bool{
		"":                                     false,
		"hub.conduit":                          false,
		"hub.conduit,hub.conduit_deliver":      false,
		"hub.conduit, hub.exec_schedules":      execSupported,
		"hub.exec_schedules_x":                 false,
		"hub.conduit,hub.exec_schedules,other": execSupported,
	} {
		if got := ExecEnabled(env(v)); got != want {
			t.Errorf("ExecEnabled(%q) = %t, want %t", v, got, want)
		}
	}
}

func TestExecStartSchema400(t *testing.T) {
	f := newExecFixture(t)
	good := f.startReq(1, "a")
	mut := func(m func(*agentcontrol.ExecStartRequest)) agentcontrol.ExecStartRequest {
		r := good
		r.Argv = append([]string(nil), good.Argv...)
		m(&r)
		return r
	}
	long := strings.Repeat("x", agentcontrol.MaxExecArgBytes+1)
	cases := map[string]struct {
		body  any
		field string
	}{
		"unknown field": {`{"v":1,"run_id":"` + good.RunID + `","argv":["true"],"timeout_s":1,"not_after_unix_ms":1,"cwd":"/"}`, "body"},
		"trailing data": {`{"v":1,"run_id":"` + good.RunID + `","argv":["true"],"timeout_s":1,"not_after_unix_ms":1} {}`, "body"},
		"not json":      {`argv=true`, "body"},
		"version":       {mut(func(r *agentcontrol.ExecStartRequest) { r.V = 2 }), "v"},
		"run id":        {mut(func(r *agentcontrol.ExecStartRequest) { r.RunID = "not-a-uuid" }), "run_id"},
		"run id upper":  {mut(func(r *agentcontrol.ExecStartRequest) { r.RunID = "AAAAAAAA-0000-4000-8000-00000000000A" }), "run_id"},
		"schedule id":   {mut(func(r *agentcontrol.ExecStartRequest) { r.ScheduleID = "x" }), "schedule_id"},
		"empty argv":    {mut(func(r *agentcontrol.ExecStartRequest) { r.Argv = nil }), "argv"},
		"empty argv0":   {mut(func(r *agentcontrol.ExecStartRequest) { r.Argv = []string{"", "x"} }), "argv"},
		"nul":           {mut(func(r *agentcontrol.ExecStartRequest) { r.Argv = []string{"true", "a\x00b"} }), "argv"},
		"argc":          {mut(func(r *agentcontrol.ExecStartRequest) { r.Argv = make([]string, 65); r.Argv[0] = "true" }), "argv"},
		"element size":  {mut(func(r *agentcontrol.ExecStartRequest) { r.Argv = []string{"true", long} }), "argv"},
		"total size": {mut(func(r *agentcontrol.ExecStartRequest) {
			r.Argv = []string{"true", long[:4000], long[:4000], long[:4000], long[:4000], long[:400]}
		}), "argv"},
		"timeout zero":    {mut(func(r *agentcontrol.ExecStartRequest) { r.TimeoutS = 0 }), "timeout_s"},
		"timeout too big": {mut(func(r *agentcontrol.ExecStartRequest) { r.TimeoutS = 3601 }), "timeout_s"},
		"no not_after":    {mut(func(r *agentcontrol.ExecStartRequest) { r.NotAfterUnixMS = 0 }), "not_after_unix_ms"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			code, resp := f.startRaw(t, c.body)
			if code != http.StatusBadRequest || resp.Outcome != agentcontrol.ExecOutcomeInvalid || resp.Field != c.field {
				t.Fatalf("got %d %+v, want 400 invalid field %s", code, resp, c.field)
			}
		})
	}
	// The limits themselves are accepted.
	edge := mut(func(r *agentcontrol.ExecStartRequest) {
		r.Argv = make([]string, 64)
		for i := range r.Argv {
			r.Argv[i] = "y"
		}
		r.Argv[0] = "true"
		r.TimeoutS = 3600
		r.RunID = uuidN(2)
	})
	if code, resp := f.start(t, edge); code != http.StatusAccepted {
		t.Fatalf("edge request = %d %+v, want 202", code, resp)
	}
	if f.fx.total.Load() != 1 {
		t.Fatalf("spawns = %d, want only the valid request's", f.fx.total.Load())
	}

	// Wrong content type.
	req := httptest.NewRequest(http.MethodPost, Prefix+RouteExecStart, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("text/plain = %d, want 400", rec.Code)
	}
	// Oversize body.
	big := `{"v":1,"pad":"` + strings.Repeat("a", execMaxBody) + `"}`
	if code, _ := f.startRaw(t, big); code != http.StatusBadRequest {
		t.Fatalf("oversize body = %d, want 400", code)
	}
}

func (f *execFixture) startRaw(t *testing.T, body any) (int, agentcontrol.ExecResponse) {
	t.Helper()
	code, resp, _ := postExec(t, f.h, RouteExecStart, body)
	return code, resp
}

func TestExecStatusSchema400(t *testing.T) {
	f := newExecFixture(t)
	for name, body := range map[string]string{
		"unknown field": `{"v":1,"run_id":"` + uuidN(1) + `","x":1}`,
		"version":       `{"v":0,"run_id":"` + uuidN(1) + `"}`,
		"run id":        `{"v":1,"run_id":"r1"}`,
	} {
		code, resp, _ := postExec(t, f.h, RouteExecStatus, body)
		if code != http.StatusBadRequest || resp.Outcome != agentcontrol.ExecOutcomeInvalid {
			t.Errorf("%s: got %d %+v, want 400 invalid", name, code, resp)
		}
	}
}

func TestExecStartStatusLifecycle(t *testing.T) {
	f := newExecFixture(t)
	if code, resp := f.status(t, 1); code != http.StatusNotFound || resp.State != agentcontrol.ExecStateUnknown {
		t.Fatalf("status before start = %d %+v", code, resp)
	}
	code, resp := f.start(t, f.startReq(1, "a"))
	if code != http.StatusAccepted || resp.State != agentcontrol.ExecStateRunning || resp.Replayed {
		t.Fatalf("start = %d %+v, want 202 running", code, resp)
	}
	if code, resp := f.status(t, 1); code != http.StatusOK || resp.State != agentcontrol.ExecStateRunning {
		t.Fatalf("status while running = %d %+v", code, resp)
	}
	f.fx.end(t, "a", 3)
	f.waitFinished(t, 1)
	code, resp = f.status(t, 1)
	if code != http.StatusOK || resp.State != agentcontrol.ExecStateFinished || resp.Outcome != agentcontrol.ExecOutcomeNonzero ||
		*resp.ExitCode != 3 || *resp.DurationMS != 5 || *resp.OutputBytes != 7 {
		t.Fatalf("status after finish = %d %+v", code, resp)
	}
	code, resp = f.start(t, f.startReq(1, "a"))
	if code != http.StatusOK || !resp.Replayed || resp.State != agentcontrol.ExecStateFinished || *resp.ExitCode != 3 {
		t.Fatalf("replay after finish = %d %+v", code, resp)
	}
	if n := f.fx.spawns("a"); n != 1 {
		t.Fatalf("spawns = %d, want 1", n)
	}
}

// TestExecConcurrentDuplicateStarts: many identical starts at once, held
// between reservation and spawn, spawn exactly once; the others are
// replays that answer only after the spawn decision.
func TestExecConcurrentDuplicateStarts(t *testing.T) {
	f := newExecFixture(t)
	release := make(chan struct{})
	reserved := make(chan struct{}, 1)
	f.route.beforeSpawn = func(string) {
		reserved <- struct{}{}
		<-release
	}
	req := f.startReq(1, "dup")
	const n = 32
	codes := make(chan int, n)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, _ := f.start(t, req)
		codes <- c
	}()
	<-reserved
	var replays atomic.Int64
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, resp := f.start(t, req)
			if c == http.StatusOK && resp.Replayed && resp.State == agentcontrol.ExecStateRunning {
				replays.Add(1)
			}
			codes <- c
		}()
	}
	// Let the duplicates queue on the pending decision, then release.
	time.Sleep(50 * time.Millisecond)
	if got := f.fx.spawns("dup"); got != 0 {
		t.Fatalf("spawned %d before the decision", got)
	}
	close(release)
	wg.Wait()
	close(codes)
	accepted := 0
	for c := range codes {
		if c == http.StatusAccepted {
			accepted++
		}
	}
	if accepted != 1 || replays.Load() != n-1 {
		t.Fatalf("accepted=%d replays=%d, want 1 and %d", accepted, replays.Load(), n-1)
	}
	if got := f.fx.spawns("dup"); got != 1 {
		t.Fatalf("spawns = %d, want 1", got)
	}
}

// TestExecDuplicateOfRefusedStart: a duplicate that waited on a start then
// refused at spawn time is refused too, and nothing spawns.
func TestExecDuplicateOfRefusedStart(t *testing.T) {
	f := newExecFixture(t)
	release := make(chan struct{})
	reserved := make(chan struct{}, 1)
	f.route.beforeSpawn = func(string) {
		reserved <- struct{}{}
		<-release
		f.clk.Add(agentcontrol.ExecSpawnWithin + time.Second) // queue delay
	}
	req := f.startReq(1, "late")
	first := make(chan int, 1)
	go func() { c, _ := f.start(t, req); first <- c }()
	<-reserved
	dup := make(chan agentcontrol.ExecResponse, 1)
	dupCode := make(chan int, 1)
	go func() { c, r := f.start(t, req); dupCode <- c; dup <- r }()
	time.Sleep(30 * time.Millisecond)
	close(release)
	if c := <-first; c != http.StatusRequestTimeout {
		t.Fatalf("first = %d, want 408", c)
	}
	if c, r := <-dupCode, <-dup; c != http.StatusRequestTimeout || r.Outcome != agentcontrol.ExecOutcomeExpiredBeforeStart {
		t.Fatalf("duplicate = %d %+v, want 408 expired_before_start", c, r)
	}
	if f.fx.total.Load() != 0 {
		t.Fatal("a refused start spawned")
	}
	if code, _ := f.status(t, 1); code != http.StatusNotFound {
		t.Fatalf("status of a refused start = %d, want 404", code)
	}
}

func TestExecMismatch(t *testing.T) {
	f := newExecFixture(t)
	base := f.startReq(1, "m")
	base.ScheduleID = uuidN(900)
	if code, _ := f.start(t, base); code != http.StatusAccepted {
		t.Fatalf("start = %d", code)
	}
	variants := map[string]func(*agentcontrol.ExecStartRequest){
		"argv":        func(r *agentcontrol.ExecStartRequest) { r.Argv = []string{"false", "m"} },
		"timeout":     func(r *agentcontrol.ExecStartRequest) { r.TimeoutS = 61 },
		"schedule":    func(r *agentcontrol.ExecStartRequest) { r.ScheduleID = uuidN(901) },
		"no schedule": func(r *agentcontrol.ExecStartRequest) { r.ScheduleID = "" },
	}
	check := func(phase string) {
		for name, m := range variants {
			r := base
			m(&r)
			code, resp := f.start(t, r)
			if code != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeMismatch {
				t.Errorf("%s %s: got %d %+v, want 409 mismatch", phase, name, code, resp)
			}
		}
	}
	check("live")
	f.fx.end(t, "m", 0)
	f.waitFinished(t, 1)
	check("tombstone")
	if f.fx.total.Load() != 1 {
		t.Fatalf("spawns = %d, want 1", f.fx.total.Load())
	}
	// A different launch id is a mismatch too.
	f.route.opts.LaunchID = "launch-2"
	if code, resp := f.start(t, base); code != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeMismatch {
		t.Fatalf("other launch: got %d %+v, want 409 mismatch", code, resp)
	}
}

func TestExecOverlapAndBusy(t *testing.T) {
	f := newExecFixture(t)
	sched := uuidN(500)
	r1 := f.startReq(1, "s1")
	r1.ScheduleID = sched
	if code, _ := f.start(t, r1); code != http.StatusAccepted {
		t.Fatalf("first = %d", code)
	}
	r2 := f.startReq(2, "s2")
	r2.ScheduleID = sched
	if code, resp := f.start(t, r2); code != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeOverlap {
		t.Fatalf("same schedule = %d %+v, want 409 overlap", code, resp)
	}
	// A second schedule and a one-shot fill the container's two slots.
	r3 := f.startReq(3, "s3")
	if code, _ := f.start(t, r3); code != http.StatusAccepted {
		t.Fatalf("one-shot = %d", code)
	}
	r4 := f.startReq(4, "s4")
	r4.ScheduleID = uuidN(501)
	if code, resp := f.start(t, r4); code != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeBusy {
		t.Fatalf("third live run = %d %+v, want 409 busy", code, resp)
	}
	// Refusals reserve nothing and are not remembered: the same run_id
	// can be admitted once there is room.
	f.fx.end(t, "s1", 0)
	f.waitFinished(t, 1)
	if code, _ := f.start(t, r2); code != http.StatusAccepted {
		t.Fatalf("schedule after its run finished = %d, want 202", code)
	}
	if f.fx.spawns("s2") != 1 || f.fx.spawns("s4") != 0 {
		t.Fatal("unexpected spawns")
	}
}

// TestExecExpiredBeforeStart covers the D9-S1 guard: refused at receipt
// and at spawn, with the agent clock ahead or behind, never spawning.
func TestExecExpiredBeforeStart(t *testing.T) {
	type tc struct {
		notAfter    time.Duration // relative to the agent clock at receipt
		queueDelay  time.Duration // clock advance between reservation and spawn
		wantStarted bool
	}
	cases := map[string]tc{
		"in time":                                     {notAfter: 60 * time.Second, wantStarted: true},
		"past at receipt beyond tolerance":            {notAfter: -11 * time.Second},
		"past at receipt within tolerance":            {notAfter: -9 * time.Second, wantStarted: true},
		"agent clock 11s ahead (delayed delivery)":    {notAfter: -11 * time.Second},
		"agent clock behind: far future still spawns": {notAfter: 10 * time.Minute, wantStarted: true},
		"queue delay past the deadline":               {notAfter: 2 * time.Second, queueDelay: 13 * time.Second},
		"queue delay past receipt+10s":                {notAfter: 10 * time.Minute, queueDelay: 11 * time.Second},
		"queue delay within both bounds":              {notAfter: 60 * time.Second, queueDelay: 9 * time.Second, wantStarted: true},
	}
	i := 0
	for name, c := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			f := newExecFixture(t)
			if c.queueDelay > 0 {
				f.route.beforeSpawn = func(string) { f.clk.Add(c.queueDelay) }
			}
			req := f.startReq(i, "x")
			req.NotAfterUnixMS = f.clk.Now().Add(c.notAfter).UnixMilli()
			code, resp := f.start(t, req)
			if c.wantStarted {
				if code != http.StatusAccepted {
					t.Fatalf("got %d %+v, want 202", code, resp)
				}
				return
			}
			if code != http.StatusRequestTimeout || resp.Outcome != agentcontrol.ExecOutcomeExpiredBeforeStart {
				t.Fatalf("got %d %+v, want 408 expired_before_start", code, resp)
			}
			if f.fx.total.Load() != 0 {
				t.Fatal("an expired start spawned")
			}
			// Nothing was reserved.
			if len(f.route.live) != 0 {
				t.Fatal("an expired start kept a live slot")
			}
		})
	}
}

// TestExecTombstoneCapacityBusy: when every tombstone is under the
// 10-minute floor, a new start is refused busy instead of evicting one;
// once some age past the floor, starts are admitted and evict those.
func TestExecTombstoneCapacityBusy(t *testing.T) {
	f := newExecFixture(t)
	n := 0
	for ; n < execTombMax; n++ {
		if code, resp := f.start(t, f.startReq(n, fmt.Sprint("t", n))); code != http.StatusAccepted {
			t.Fatalf("run %d = %d %+v", n, code, resp)
		}
		f.fx.end(t, fmt.Sprint("t", n), 0)
		f.waitFinished(t, n)
	}
	if code, resp := f.start(t, f.startReq(n, "over")); code != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeBusy {
		t.Fatalf("start with %d young tombstones = %d %+v, want busy", execTombMax, code, resp)
	}
	for i := 0; i < execTombMax; i++ {
		if code, _ := f.status(t, i); code != http.StatusOK {
			t.Fatalf("tombstone %d lost under the floor", i)
		}
	}
	f.clk.Add(execTombFloor)
	if code, _ := f.start(t, f.startReq(n, "over")); code != http.StatusAccepted {
		t.Fatalf("start after the floor = %d", code)
	}
	f.fx.end(t, "over", 0)
	f.waitFinished(t, n)
	if f.route.tombs.len() != execTombMax {
		t.Fatalf("tombstones = %d, want the bound %d", f.route.tombs.len(), execTombMax)
	}
	if code, _ := f.status(t, n); code != http.StatusOK {
		t.Fatal("the new tombstone is missing")
	}
}

// TestExecReplayUnderTombstoneChurn: a 60-minute run stays live and
// replayable while 300+ short runs start and finish around it, and after
// it finishes its tombstone answers replays; nothing runs twice, and an
// evicted run_id is never started again (its start window has closed).
func TestExecReplayUnderTombstoneChurn(t *testing.T) {
	f := newExecFixture(t)
	long := f.startReq(0, "long")
	long.TimeoutS = 3600
	if code, _ := f.start(t, long); code != http.StatusAccepted {
		t.Fatal("long run not admitted")
	}
	type sent struct {
		req agentcontrol.ExecStartRequest
		tag string
	}
	var short []sent
	for i := 1; i <= 320; i++ {
		f.clk.Add(10 * time.Second) // 320 runs over ~53 minutes
		tag := fmt.Sprint("short", i)
		req := f.startReq(i, tag)
		code, resp := f.start(t, req)
		if code != http.StatusAccepted {
			t.Fatalf("short run %d = %d %+v", i, code, resp)
		}
		short = append(short, sent{req, tag})
		f.fx.end(t, tag, 0)
		f.waitFinished(t, i)
		if i%40 == 0 {
			if code, resp := f.start(t, long); code != http.StatusOK || !resp.Replayed || resp.State != agentcontrol.ExecStateRunning {
				t.Fatalf("replay of the live long run at %d = %d %+v", i, code, resp)
			}
		}
	}
	f.clk.Add(5 * time.Minute)
	f.fx.end(t, "long", 0)
	f.waitFinished(t, 0)
	// The long run's tombstone is the youngest: it answers replays.
	for i := 0; i < 3; i++ {
		if code, resp := f.start(t, long); code != http.StatusOK || !resp.Replayed || resp.State != agentcontrol.ExecStateFinished {
			t.Fatalf("replay after finish = %d %+v", code, resp)
		}
	}
	// Every short run replays from its tombstone or, once evicted, is
	// refused (its not_after is long past): never a second spawn.
	for _, s := range short {
		code, resp := f.start(t, s.req)
		switch {
		case code == http.StatusOK && resp.Replayed:
		case code == http.StatusRequestTimeout:
		default:
			t.Fatalf("replay of %s = %d %+v", s.tag, code, resp)
		}
		if n := f.fx.spawns(s.tag); n != 1 {
			t.Fatalf("%s spawned %d times", s.tag, n)
		}
	}
	if n := f.fx.spawns("long"); n != 1 {
		t.Fatalf("long run spawned %d times", n)
	}
	if f.route.tombs.len() > execTombMax {
		t.Fatalf("tombstones grew past the bound: %d", f.route.tombs.len())
	}
}

// TestExecInterleavingStress drives starts, duplicate starts, status reads,
// finishes and clock movement concurrently (run with -race). Invariants: no
// run_id spawns twice; a start answer is always schema-valid; an admitted
// run_id is never reported unknown while its tombstone is within the floor;
// live runs never exceed the cap; tombstones never exceed their bound.
func TestExecInterleavingStress(t *testing.T) {
	f := newExecFixture(t)
	const runs = 400
	const workers = 16
	rng := rand.New(rand.NewSource(1))
	var rngMu sync.Mutex
	rnd := func(n int) int { rngMu.Lock(); defer rngMu.Unlock(); return rng.Intn(n) }

	admitted := make([]atomic.Bool, runs)
	var wg sync.WaitGroup
	// Finisher: ends spawned runs at random.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.fx.mu.Lock()
			var pending []*fakeProc
			for _, p := range f.fx.procs {
				pending = append(pending, p)
			}
			f.fx.mu.Unlock()
			for _, p := range pending {
				select {
				case p.done <- execFinal{outcome: agentcontrol.ExecOutcomeOK, durationMS: 1}:
				default:
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < runs; k++ {
				i := rnd(runs)
				req := agentcontrol.ExecStartRequest{
					V: 1, RunID: uuidN(i), Argv: []string{"true", fmt.Sprint("r", i)}, TimeoutS: 60,
					NotAfterUnixMS: f.clk.Now().Add(time.Hour).UnixMilli(),
				}
				if i%3 == 0 {
					req.ScheduleID = uuidN(10000 + i%7)
				}
				switch rnd(3) {
				case 0, 1:
					code, resp := f.start(t, req)
					if code == http.StatusAccepted || (code == http.StatusOK && resp.Replayed) {
						admitted[i].Store(true)
					}
					if code == http.StatusConflict && resp.Outcome == agentcontrol.ExecOutcomeMismatch {
						t.Errorf("identical request for %d answered mismatch", i)
					}
				case 2:
					code, _ := f.status(t, i)
					if code == http.StatusNotFound && admitted[i].Load() {
						t.Errorf("admitted run %d reported unknown", i)
					}
				}
				f.route.mu.Lock()
				if len(f.route.live) > execMaxLive || f.route.tombs.len() > execTombMax {
					t.Errorf("bounds broken: live=%d tombs=%d", len(f.route.live), f.route.tombs.len())
				}
				f.route.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	f.fx.mu.Lock()
	defer f.fx.mu.Unlock()
	for tag, n := range f.fx.spawned {
		if n != 1 {
			t.Errorf("%s spawned %d times", tag, n)
		}
	}
}

func TestExecStartFailed(t *testing.T) {
	t.Run("executor error", func(t *testing.T) {
		f := newExecFixture(t)
		f.fx.fail = true
		code, resp := f.start(t, f.startReq(1, "SENTINEL-start-err"))
		if code != http.StatusAccepted || resp.State != agentcontrol.ExecStateFinished || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
			t.Fatalf("got %d %+v, want 202 finished start_failed", code, resp)
		}
		if code, resp := f.status(t, 1); code != http.StatusOK || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
			t.Fatalf("status = %d %+v", code, resp)
		}
		if code, resp := f.start(t, f.startReq(1, "SENTINEL-start-err")); code != http.StatusOK || !resp.Replayed || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
			t.Fatalf("replay = %d %+v", code, resp)
		}
	})
	t.Run("not on the run's PATH", func(t *testing.T) {
		f := newExecFixture(t, func(o *ExecOptions) {
			o.Env = func() ([]string, error) { return []string{"PATH=" + t.TempDir()}, nil }
		})
		req := f.startReq(1, "x")
		req.Argv[0] = "sh" // on sciontool's PATH, not the run's
		code, resp := f.start(t, req)
		if code != http.StatusAccepted || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
			t.Fatalf("got %d %+v, want start_failed", code, resp)
		}
		if f.fx.total.Load() != 0 {
			t.Fatal("spawned")
		}
	})
	t.Run("env error", func(t *testing.T) {
		f := newExecFixture(t, func(o *ExecOptions) {
			o.Env = func() ([]string, error) { return nil, fmt.Errorf("boom") }
		})
		code, resp := f.start(t, f.startReq(1, "x"))
		if code != http.StatusAccepted || resp.Outcome != agentcontrol.ExecOutcomeStartFailed || f.fx.total.Load() != 0 {
			t.Fatalf("got %d %+v spawns=%d", code, resp, f.fx.total.Load())
		}
	})
	// Privilege drop fails closed: no agent user while a drop is required
	// means start_failed, never a run as sciontool's own identity.
	for name, ids := range map[string][2]int{"no uid": {0, 1000}, "no gid": {1000, 0}, "neither": {0, 0}} {
		t.Run("privilege drop required: "+name, func(t *testing.T) {
			f := newExecFixture(t, func(o *ExecOptions) {
				o.UID, o.GID, o.RequirePrivilegeDrop = ids[0], ids[1], true
			})
			code, resp := f.start(t, f.startReq(1, "x"))
			if code != http.StatusAccepted || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
				t.Fatalf("got %d %+v, want start_failed", code, resp)
			}
			if f.fx.total.Load() != 0 {
				t.Fatal("ran without the privilege drop")
			}
		})
	}
}

// TestExecSpecIdentityEnvCwd: the spec the executor gets carries the PTY
// user, the configured env with PWD, and the working directory rule.
func TestExecSpecIdentityEnvCwd(t *testing.T) {
	work := t.TempDir()
	f := newExecFixture(t, func(o *ExecOptions) {
		o.Workdir = work
		o.Env = func() ([]string, error) { return []string{"PATH=/usr/bin:/bin", "HARNESS_ONLY=1", "PWD=/old"}, nil }
	})
	if code, _ := f.start(t, f.startReq(1, "id")); code != http.StatusAccepted {
		t.Fatal("not started")
	}
	spec := f.fx.procs["id"].specs
	if spec.uid != 1000 || spec.gid != 1000 || !spec.drop {
		t.Fatalf("identity = %d:%d drop=%t", spec.uid, spec.gid, spec.drop)
	}
	if spec.dir != work || envValue(spec.env, "PWD") != work || envValue(spec.env, "HARNESS_ONLY") != "1" {
		t.Fatalf("dir=%q env=%v", spec.dir, spec.env)
	}
	if !strings.HasSuffix(spec.path, "/true") || spec.timeout != time.Minute ||
		spec.killGrace != agentcontrol.ExecKillGrace || spec.waitDelay != agentcontrol.ExecWaitDelay {
		t.Fatalf("spec = %+v", spec)
	}

	// No preferred directory: the user's home, else /.
	if got := execWorkdir(filepath.Join(work, "missing"), "", []string{"HOME=" + work}); got != work {
		t.Fatalf("fallback to HOME = %q", got)
	}
	if got := execWorkdir(filepath.Join(work, "missing"), "no-such-user-xyz", nil); got != "/" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestLookPathEnv(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := lookPathEnv("tool", "relative:"+dir, "/"); err != nil || p != exe {
		t.Fatalf("tool = %q %v", p, err)
	}
	if _, err := lookPathEnv("plain", dir, "/"); err == nil {
		t.Fatal("a non-executable resolved")
	}
	if _, err := lookPathEnv("tool", "relative", dir); err == nil {
		t.Fatal("a relative PATH entry was searched")
	}
	if p, err := lookPathEnv("./tool", "", dir); err != nil || p != exe {
		t.Fatalf("./tool = %q %v", p, err)
	}
}

func TestExecTombsUnit(t *testing.T) {
	now := time.Unix(0, 0)
	s := newExecTombs(3, time.Hour, 10*time.Minute)
	put := func(id string) { s.put(now, &execTomb{runID: id}) }
	put("a")
	put("b")
	put("c")
	if s.canReserve(now, 1) {
		t.Fatal("reserved past the bound with every entry under the floor")
	}
	now = now.Add(10 * time.Minute)
	if !s.canReserve(now, 1) {
		t.Fatal("entries at the floor should be evictable")
	}
	s.get(now, "a") // a becomes most recently used
	put("d")
	if _, ok := s.get(now, "b"); ok {
		t.Fatal("LRU victim should be b")
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, ok := s.get(now, id); !ok {
			t.Fatalf("%s evicted", id)
		}
	}
	now = now.Add(time.Hour + time.Second)
	if _, ok := s.get(now, "d"); ok {
		t.Fatal("entry kept past the TTL")
	}
	s.purge(now)
	if s.len() != 0 {
		t.Fatalf("len after TTL = %d", s.len())
	}
}
