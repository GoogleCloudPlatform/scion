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
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// controlPathTestHome points $HOME at a temporary home with a Scion global
// directory and returns the global directory and an unrelated directory.
func controlPathTestHome(t *testing.T) (global, outside string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	global = filepath.Join(home, ".scion")
	outside = filepath.Join(root, "data")
	for _, d := range []string{global, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return global, outside
}

func decodeErrorCode(t *testing.T, body io.Reader) string {
	t.Helper()
	var resp ErrorResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return resp.Error.Code
}

func TestCreateAgent_RefusesVolumesReachingControlPaths(t *testing.T) {
	controlPathTestHome(t)
	for _, vols := range []string{
		`"config": {"volumes": [{"source": "~/.scion", "target": "/x", "read_only": true}]}`,
		`"inlineConfig": {"volumes": [{"source": "GLOBAL/settings.yaml", "target": "/x"}]}`,
	} {
		// newTestServer may point $HOME elsewhere: take the global
		// directory from the home in effect once the server exists.
		srv := newTestServer(t)
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		global := filepath.ToSlash(filepath.Join(home, ".scion"))
		body := `{"name": "a1", ` + strings.ReplaceAll(vols, "GLOBAL", global) + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", vols, w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w.Body); code != brokercontrol.ErrorCode {
			t.Fatalf("code = %q, want %q", code, brokercontrol.ErrorCode)
		}
	}
}

func TestStartAgent_RefusesInlineVolumesReachingControlPaths(t *testing.T) {
	controlPathTestHome(t)
	mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "stopped"}}}
	srv := newTestServerWithManager(t, mgr)
	body := `{"inlineConfig": {"volumes": [{"source": "~", "target": "/x"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w.Body); code != brokercontrol.ErrorCode {
		t.Fatalf("code = %q, want %q", code, brokercontrol.ErrorCode)
	}
	if mgr.startCalls != 0 {
		t.Fatalf("Start was called %d times after a refused volume", mgr.startCalls)
	}
}

func TestSqliteDatabaseFile(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "hub.db")
	cases := []struct {
		db   config.DatabaseConfig
		want string
	}{
		{config.DatabaseConfig{Driver: "sqlite", URL: abs}, abs},
		{config.DatabaseConfig{Driver: "sqlite", URL: "file:" + abs + "?_busy_timeout=5000"}, abs},
		{config.DatabaseConfig{Driver: "sqlite", URL: ":memory:"}, ""},
		{config.DatabaseConfig{Driver: "sqlite", URL: "file::memory:?cache=shared"}, ""},
		{config.DatabaseConfig{Driver: "postgres", URL: "postgres://h/db"}, ""},
	}
	for _, tc := range cases {
		if got := sqliteDatabaseFile(tc.db); got != tc.want {
			t.Errorf("sqliteDatabaseFile(%+v) = %q, want %q", tc.db, got, tc.want)
		}
	}
}

func TestRegisterControlPaths_StateDirIsProtected(t *testing.T) {
	_, outside := controlPathTestHome(t)
	state := filepath.Join(outside, "state", "b1")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	registerControlPaths("test-broker", state, "")
	t.Cleanup(func() { brokercontrol.SetExtraPaths(controlPathsOwner("test-broker"), nil) })
	err := checkRequestVolumes([]api.VolumeMount{{Source: filepath.Join(outside, "state"), Target: "/x"}})
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
}

// sweepRuntime is a mock runtime with scripted agents and inspect results.
type sweepRuntime struct {
	*scionrt.MockRuntime
	agents  []api.AgentInfo
	inspect map[string][]sweepInspect
	calls   map[string]int
	stopped []string
	// listErrs and stopErrs are returned by successive List and Stop
	// calls (nil entries, or past the end, succeed).
	listErrs  []error
	listCalls int
	stopErrs  []error
	stopCalls int
}

type sweepInspect struct {
	binds []scionrt.BindMount
	err   error
	// reported, when set, is what the runtime reports instead of binds
	// (binds stay the broker's record).
	reported []scionrt.BindMount
	// unrecorded drops the broker's record.
	unrecorded bool
	// daemonLocal is the runtime's daemon locality check, if any.
	daemonLocal func() (bool, error)
}

func newSweepRuntime(agents []api.AgentInfo, inspect map[string][]sweepInspect) *sweepRuntime {
	r := &sweepRuntime{agents: agents, inspect: inspect, calls: map[string]int{}}
	r.MockRuntime = &scionrt.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			i := r.listCalls
			r.listCalls++
			if i < len(r.listErrs) && r.listErrs[i] != nil {
				return nil, r.listErrs[i]
			}
			return r.agents, nil
		},
		StopFunc: func(_ context.Context, ref scionrt.RunRef) error {
			i := r.stopCalls
			r.stopCalls++
			if i < len(r.stopErrs) && r.stopErrs[i] != nil {
				return r.stopErrs[i]
			}
			r.stopped = append(r.stopped, ref.ID)
			for i := range r.agents {
				if r.agents[i].ContainerID == ref.ID {
					r.agents[i].Phase = "stopped"
				}
			}
			return nil
		},
	}
	return r
}

func (r *sweepRuntime) InspectBindMounts(_ context.Context, id string) (scionrt.InspectedBinds, error) {
	res := r.inspect[id]
	i := min(r.calls[id], len(res)-1)
	r.calls[id]++
	if res[i].err != nil {
		return scionrt.InspectedBinds{}, res[i].err
	}
	reported := res[i].binds
	if res[i].reported != nil {
		reported = res[i].reported
	}
	ib := scionrt.InspectedBinds{Reported: reported, ReportedKnown: true, DaemonLocal: res[i].daemonLocal}
	if !res[i].unrecorded {
		data, _ := json.Marshal(res[i].binds)
		ib.Labels = map[string]string{scionrt.LabelBindSources: base64.StdEncoding.EncodeToString(data)}
	}
	return ib, nil
}

func newTestSweeper(rts ...scionrt.Runtime) (*controlPathSweeper, *[]string) {
	sw := newControlPathSweeper(func() []scionrt.Runtime { return rts }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var marked []string
	sw.markError = func(a agentRef) { marked = append(marked, a.name) }
	return sw, &marked
}

func TestControlPathSweep(t *testing.T) {
	global, outside := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	agents := []api.AgentInfo{
		{ContainerID: "clean", Name: "clean", Phase: "running", Labels: scion},
		{ContainerID: "bad", Name: "bad", Phase: "running", Labels: scion},
		{ContainerID: "flaky", Name: "flaky", Phase: "running", Labels: scion},
		{ContainerID: "broken", Name: "broken", Phase: "running", Labels: scion},
		{ContainerID: "stopped", Name: "stopped", Phase: "stopped", Labels: scion},
		{ContainerID: "other", Name: "other-workload", Phase: "running"},
	}
	ok := sweepInspect{binds: []scionrt.BindMount{{Source: outside, Destination: "/x"}}}
	bad := sweepInspect{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}}
	fail := sweepInspect{err: scionrt.ErrMountsUnverifiable}
	rt := newSweepRuntime(agents, map[string][]sweepInspect{
		"clean":   {ok},
		"bad":     {bad},
		"flaky":   {fail, ok},
		"broken":  {fail, fail},
		"stopped": {bad},
		"other":   {bad},
	})
	sw, marked := newTestSweeper(rt)
	now := time.Now()

	sw.tick(context.Background(), now)
	if strings.Join(rt.stopped, ",") != "bad" {
		t.Fatalf("after the first sweep, stopped = %v, want [bad] (a violation stops at once; an inspect failure waits a tick)", rt.stopped)
	}
	if rt.calls["stopped"] != 0 || rt.calls["other"] != 0 {
		t.Fatalf("swept a stopped agent or a non-Scion container: %v", rt.calls)
	}

	// The next tick is due because inspects failed: flaky now inspects
	// clean, broken fails a second time and is stopped.
	sw.tick(context.Background(), now.Add(controlPathSweepTick))
	if strings.Join(rt.stopped, ",") != "bad,broken" {
		t.Fatalf("after the retry tick, stopped = %v, want [bad broken]", rt.stopped)
	}
	if strings.Join(*marked, ",") != "bad,broken" {
		t.Fatalf("marked = %v", *marked)
	}
	if len(sw.failures) != 0 {
		t.Fatalf("failures not cleared: %v", sw.failures)
	}

	// Nothing failed and the control paths did not move: not due.
	before := rt.calls["clean"]
	sw.tick(context.Background(), now.Add(2*controlPathSweepTick))
	if rt.calls["clean"] != before {
		t.Fatal("swept before the interval with no change")
	}
	// The full interval elapsed: due.
	sw.tick(context.Background(), now.Add(controlPathSweepInterval+2*controlPathSweepTick))
	if rt.calls["clean"] != before+1 {
		t.Fatal("did not sweep after the interval")
	}
}

func TestControlPathSweep_ControlPathsMoved(t *testing.T) {
	_, outside := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	ws := filepath.Join(outside, "ws")
	rt := newSweepRuntime(
		[]api.AgentInfo{{ContainerID: "a", Name: "a", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"a": {{binds: []scionrt.BindMount{{Source: outside, Destination: "/x"}}}}},
	)
	sw, _ := newTestSweeper(rt)
	now := time.Now()
	sw.tick(context.Background(), now)
	if len(rt.stopped) != 0 {
		t.Fatalf("stopped = %v", rt.stopped)
	}
	// A control path is now registered inside the agent's bind: the next
	// tick sees the set change and stops the agent without waiting for the
	// interval.
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	brokercontrol.SetExtraPaths("sweep-test", []brokercontrol.Entry{{Path: ws, Kind: brokercontrol.Dir, Class: brokercontrol.ClassHostState}})
	t.Cleanup(func() { brokercontrol.SetExtraPaths("sweep-test", nil) })
	sw.tick(context.Background(), now.Add(controlPathSweepTick))
	if strings.Join(rt.stopped, ",") != "a" {
		t.Fatalf("stopped = %v, want [a]", rt.stopped)
	}
}

func TestControlPathSweep_RuntimeWithoutInspection(t *testing.T) {
	rt := &scionrt.MockRuntime{}
	sw, _ := newTestSweeper(rt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sw.run(ctx) // returns at once: nothing to sweep
}

func TestCheckLayoutRoots(t *testing.T) {
	global, outside := controlPathTestHome(t)
	home := filepath.Dir(global)
	if err := checkLayoutRoots(ServerConfig{WorktreeBase: filepath.Join(outside, "wt"), NFSConfig: &config.V1NFSConfig{MountRoot: filepath.Join(outside, "nfs")}}); err != nil {
		t.Fatalf("roots clear of control paths: %v", err)
	}
	for _, cfg := range []ServerConfig{
		{WorktreeBase: home},
		{NFSConfig: &config.V1NFSConfig{MountRoot: global}},
	} {
		if err := checkLayoutRoots(cfg); !errors.Is(err, brokercontrol.ErrControlPathMount) {
			t.Errorf("checkLayoutRoots(%+v) = %v, want ErrControlPathMount", cfg, err)
		}
	}
}

func TestControlPathSweep_MismatchAndMissingRecord(t *testing.T) {
	_, outside := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	ok := []scionrt.BindMount{{Source: outside, Destination: "/x"}}
	rt := newSweepRuntime([]api.AgentInfo{
		{ContainerID: "mismatch", Name: "mismatch", Phase: "running", Labels: scion},
		{ContainerID: "unrecorded", Name: "unrecorded", Phase: "running", Labels: scion},
		{ContainerID: "fine", Name: "fine", Phase: "running", Labels: scion},
	}, map[string][]sweepInspect{
		// The runtime reports another source than the broker recorded,
		// one that does not exist on this host.
		"mismatch":   {{binds: ok, reported: []scionrt.BindMount{{Source: "/vm/translated/home", Destination: "/x"}}}},
		"unrecorded": {{binds: ok, unrecorded: true}},
		"fine":       {{binds: ok}},
	})
	sw, _ := newTestSweeper(rt)
	now := time.Now()
	sw.tick(context.Background(), now)
	if strings.Join(rt.stopped, ",") != "mismatch" {
		t.Fatalf("after the first sweep, stopped = %v, want [mismatch] (a mismatch stops at once)", rt.stopped)
	}
	sw.tick(context.Background(), now.Add(controlPathSweepTick))
	if strings.Join(rt.stopped, ",") != "mismatch,unrecorded" {
		t.Fatalf("after the retry tick, stopped = %v, want [mismatch unrecorded] (a container without the record is unverifiable)", rt.stopped)
	}
}

func TestControlPathSweep_AuxiliaryRuntimes(t *testing.T) {
	global, outside := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	primary := newSweepRuntime([]api.AgentInfo{{ContainerID: "p1", Name: "p1", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"p1": {{binds: []scionrt.BindMount{{Source: outside, Destination: "/x"}}}}})
	aux := newSweepRuntime([]api.AgentInfo{{ContainerID: "a1", Name: "a1", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"a1": {{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}}}})
	noInspect := &scionrt.MockRuntime{}
	// The primary runtime listed twice is swept once.
	sw, _ := newTestSweeper(primary, aux, noInspect, primary)
	sw.tick(context.Background(), time.Now())
	if len(primary.stopped) != 0 || primary.calls["p1"] != 1 {
		t.Fatalf("primary: stopped=%v calls=%v", primary.stopped, primary.calls)
	}
	if strings.Join(aux.stopped, ",") != "a1" {
		t.Fatalf("auxiliary runtime agent was not stopped: %v", aux.stopped)
	}
}

func TestServerSweptRuntimes_IncludesAuxiliary(t *testing.T) {
	srv := newTestServer(t)
	aux := &scionrt.MockRuntime{}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["aux-test"] = auxiliaryRuntime{Runtime: aux}
	srv.auxiliaryRuntimesMu.Unlock()
	rts := srv.sweptRuntimes()
	found := false
	for _, rt := range rts {
		if rt == scionrt.Runtime(aux) {
			found = true
		}
	}
	if len(rts) < 2 || !found {
		t.Fatalf("sweptRuntimes = %v, want the primary and the auxiliary runtime", rts)
	}
}

func TestCheckRequestVolumes_WorkspaceReachingControlPath(t *testing.T) {
	global, outside := controlPathTestHome(t)
	if err := checkRequestVolumes([]api.VolumeMount{{Source: global, Target: "/workspace"}}); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
	if err := checkRequestVolumes([]api.VolumeMount{{Source: outside, Target: "/workspace"}}); err != nil {
		t.Fatalf("clean workspace: %v", err)
	}
}

func TestControlPathSweep_ListFailureKeepsRetryCount(t *testing.T) {
	_, outside := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	fail := sweepInspect{err: scionrt.ErrMountsUnverifiable}
	rt := newSweepRuntime([]api.AgentInfo{{ContainerID: "c", Name: "c", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"c": {fail, fail, {binds: []scionrt.BindMount{{Source: outside, Destination: "/x"}}}}})
	// Listing succeeds, then fails, then succeeds.
	rt.listErrs = []error{nil, errors.New("daemon busy"), nil}
	sw, _ := newTestSweeper(rt)
	now := time.Now()
	sw.tick(context.Background(), now)                             // inspect fails (1)
	sw.tick(context.Background(), now.Add(controlPathSweepTick))   // list fails: count kept
	sw.tick(context.Background(), now.Add(2*controlPathSweepTick)) // inspect fails (2): stop
	if strings.Join(rt.stopped, ",") != "c" {
		t.Fatalf("stopped = %v, want [c]: the retry count must survive a list failure", rt.stopped)
	}
}

func TestControlPathSweep_FailedStopIsRetriedNextTick(t *testing.T) {
	global, _ := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	rt := newSweepRuntime([]api.AgentInfo{{ContainerID: "bad", Name: "bad", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"bad": {{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}}}})
	rt.stopErrs = []error{errors.New("stop timed out")}
	sw, _ := newTestSweeper(rt)
	now := time.Now()
	sw.tick(context.Background(), now)
	if len(rt.stopped) != 0 || rt.stopCalls != 1 {
		t.Fatalf("first tick: stopped=%v stopCalls=%d", rt.stopped, rt.stopCalls)
	}
	// Well before the 60 s interval, the next tick retries the stop.
	sw.tick(context.Background(), now.Add(controlPathSweepTick))
	if strings.Join(rt.stopped, ",") != "bad" {
		t.Fatalf("stop was not retried on the next tick: stopped=%v stopCalls=%d", rt.stopped, rt.stopCalls)
	}
}

func TestControlPathSweep_ListFailureContinuesToOtherRuntimes(t *testing.T) {
	global, _ := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	broken := newSweepRuntime(nil, nil)
	broken.listErrs = []error{errors.New("daemon down")}
	other := newSweepRuntime([]api.AgentInfo{{ContainerID: "bad", Name: "bad", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"bad": {{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}}}})
	sw, _ := newTestSweeper(broken, other)
	sw.tick(context.Background(), time.Now())
	if strings.Join(other.stopped, ",") != "bad" {
		t.Fatalf("a list failure on one runtime kept another from being swept: %v", other.stopped)
	}
}

func TestControlPathSweep_UnrecordedContainers(t *testing.T) {
	global, outside := controlPathTestHome(t)
	scion := map[string]string{"scion.name": "x"}
	local := func() (bool, error) { return true, nil }
	remote := func() (bool, error) { return false, nil }
	clean := []scionrt.BindMount{{Source: outside, Destination: "/x"}}
	rt := newSweepRuntime([]api.AgentInfo{
		{ContainerID: "kept", Name: "kept", Phase: "running", Labels: scion},
		{ContainerID: "remote", Name: "remote", Phase: "running", Labels: scion},
		{ContainerID: "absent", Name: "absent", Phase: "running", Labels: scion},
		{ContainerID: "bad", Name: "bad", Phase: "running", Labels: scion},
	}, map[string][]sweepInspect{
		"kept":   {{binds: clean, unrecorded: true, daemonLocal: local}},
		"remote": {{binds: clean, unrecorded: true, daemonLocal: remote}},
		"absent": {{binds: []scionrt.BindMount{{Source: filepath.Join(outside, "absent"), Destination: "/x"}}, unrecorded: true, daemonLocal: local}},
		"bad":    {{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}, unrecorded: true, daemonLocal: local}},
	})
	sw, _ := newTestSweeper(rt)
	now := time.Now()
	sw.tick(context.Background(), now)
	if strings.Join(rt.stopped, ",") != "bad" {
		t.Fatalf("after the first sweep, stopped = %v, want [bad] (a control-path source stops at once)", rt.stopped)
	}
	sw.tick(context.Background(), now.Add(controlPathSweepTick))
	if strings.Join(rt.stopped, ",") != "bad,remote,absent" {
		t.Fatalf("after the retry tick, stopped = %v, want [bad remote absent]; kept must keep running", rt.stopped)
	}
}

func TestControlPathSweep_HungListIsBounded(t *testing.T) {
	controlPathTestHome(t)
	prev := controlPathSweepCallTimeout
	controlPathSweepCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controlPathSweepCallTimeout = prev })
	hung := newSweepRuntime(nil, nil)
	hung.ListFunc = func(ctx context.Context, _ map[string]string) ([]api.AgentInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sw, _ := newTestSweeper(hung)
	done := make(chan struct{})
	go func() { sw.tick(context.Background(), time.Now()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung List stalled the sweep")
	}
}

func TestControlPathSweep_HungStopIsBoundedAndRetried(t *testing.T) {
	global, _ := controlPathTestHome(t)
	prev := controlPathSweepCallTimeout
	controlPathSweepCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controlPathSweepCallTimeout = prev })
	scion := map[string]string{"scion.name": "x"}
	rt := newSweepRuntime([]api.AgentInfo{{ContainerID: "bad", Name: "bad", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"bad": {{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}}}})
	hangs := true
	rt.StopFunc = func(ctx context.Context, ref scionrt.RunRef) error {
		if hangs {
			<-ctx.Done()
			return ctx.Err()
		}
		rt.stopped = append(rt.stopped, ref.ID)
		return nil
	}
	sw, _ := newTestSweeper(rt)
	now := time.Now()
	done := make(chan struct{})
	go func() { sw.tick(context.Background(), now); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung Stop stalled the sweep")
	}
	if len(rt.stopped) != 0 {
		t.Fatalf("stopped = %v after a hung stop", rt.stopped)
	}
	// The stop is retried on the next tick.
	hangs = false
	sw.tick(context.Background(), now.Add(controlPathSweepTick))
	if strings.Join(rt.stopped, ",") != "bad" {
		t.Fatalf("the stop was not retried on the next tick: %v", rt.stopped)
	}
}

func TestControlPathSweep_HungListOnOneRuntimeOthersSwept(t *testing.T) {
	global, _ := controlPathTestHome(t)
	prev := controlPathSweepCallTimeout
	controlPathSweepCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { controlPathSweepCallTimeout = prev })
	scion := map[string]string{"scion.name": "x"}
	hung := newSweepRuntime(nil, nil)
	hung.ListFunc = func(ctx context.Context, _ map[string]string) ([]api.AgentInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	other := newSweepRuntime([]api.AgentInfo{{ContainerID: "bad", Name: "bad", Phase: "running", Labels: scion}},
		map[string][]sweepInspect{"bad": {{binds: []scionrt.BindMount{{Source: global, Destination: "/g"}}}}})
	sw, _ := newTestSweeper(hung, other)
	done := make(chan struct{})
	go func() { sw.tick(context.Background(), time.Now()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung List on one runtime stalled the sweep")
	}
	if strings.Join(other.stopped, ",") != "bad" {
		t.Fatalf("the other runtime was not swept: %v", other.stopped)
	}
}
