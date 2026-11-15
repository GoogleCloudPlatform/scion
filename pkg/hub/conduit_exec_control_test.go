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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const execTestRunID = "00000000-0000-4000-8000-0000000000a1"

// execControlOptions registers the real exec routes, running commands on
// this host as the test user.
func execControlOptions(t *testing.T) control.Options {
	dir := t.TempDir()
	return control.Options{Exec: &control.ExecOptions{
		Env:     func() ([]string, error) { return []string{"PATH=/usr/bin:/bin", "HOME=" + dir}, nil },
		Workdir: dir,
	}}
}

// execCapture records the exec requests that reached the agent's control
// handler and optionally answers in its place.
type execCapture struct {
	mu     sync.Mutex
	bodies map[string][][]byte // path -> bodies
	answer func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse
}

func (c *execCapture) wrap(next conduit.RPCHandler) conduit.RPCHandler {
	return conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		c.mu.Lock()
		if c.bodies == nil {
			c.bodies = map[string][][]byte{}
		}
		c.bodies[req.GetPath()] = append(c.bodies[req.GetPath()], req.GetBody())
		answer := c.answer
		c.mu.Unlock()
		if answer != nil {
			if resp := answer(req); resp != nil {
				return resp
			}
		}
		return next.HandleRPC(ctx, req)
	})
}

func (c *execCapture) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies[path])
}

func execStartReq(argv ...string) agentcontrol.ExecStartRequest {
	return agentcontrol.ExecStartRequest{RunID: execTestRunID, ScheduleID: "00000000-0000-4000-8000-0000000000b1", Argv: argv, TimeoutS: 10}
}

func waitExecFinished(t *testing.T, f *ptyConduitFixture, runID string) agentcontrol.ExecResult {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := f.srv.AgentExecRunStatus(context.Background(), f.launched.ID, f.launched.RunID, runID)
		require.NoError(t, err)
		if res.Finished() {
			return res
		}
		require.Equal(t, agentcontrol.ExecStateRunning, res.State)
		if time.Now().After(deadline) {
			t.Fatal("run not finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAgentExecClient_RoundTrip: exec-start and exec-status reach the real
// routes on the agent's session, locally and across nodes; a second start
// of the same run_id is a replay; status reports the final outcome.
func TestAgentExecClient_RoundTrip(t *testing.T) {
	for _, crossNode := range []bool{false, true} {
		name := map[bool]string{false: "local", true: "cross-node"}[crossNode]
		t.Run(name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			enableExecSchedules(t, f.srv)
			hubURL := f.public.URL
			if crossNode {
				hubURL = f.startPeerRelay(t, "hub-b")
			}
			c := &execCapture{}
			f.startControlAgentWithOptions(t, hubURL, true, execControlOptions(t), c.wrap)
			ctx := context.Background()
			sent := time.Now()
			res, err := f.srv.StartAgentExecRun(ctx, f.launched.ID, f.launched.RunID, execStartReq("sh", "-c", "exit 3"))
			require.NoError(t, err)
			assert.Equal(t, http.StatusAccepted, res.Status)
			assert.Equal(t, agentcontrol.ExecStateRunning, res.State)

			// The hub set v and not_after (send time + 60s).
			require.Equal(t, 1, c.count(execRoutePath(agentcontrol.RouteExecStart)))
			var got agentcontrol.ExecStartRequest
			require.NoError(t, json.Unmarshal(c.bodies[execRoutePath(agentcontrol.RouteExecStart)][0], &got))
			assert.Equal(t, 1, got.V)
			notAfter := time.UnixMilli(got.NotAfterUnixMS)
			assert.WithinDuration(t, sent.Add(agentcontrol.ExecNotAfter), notAfter, 5*time.Second)

			fin := waitExecFinished(t, f, execTestRunID)
			assert.Equal(t, agentcontrol.ExecOutcomeNonzero, fin.Outcome)
			assert.Equal(t, 3, fin.ExitCode)

			res, err = f.srv.StartAgentExecRun(ctx, f.launched.ID, f.launched.RunID, execStartReq("sh", "-c", "exit 3"))
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, res.Status)
			assert.True(t, res.Replayed)
			assert.True(t, res.Finished())

			// A different command under the same run_id is a mismatch.
			res, err = f.srv.StartAgentExecRun(ctx, f.launched.ID, f.launched.RunID, execStartReq("true"))
			require.NoError(t, err)
			assert.Equal(t, http.StatusConflict, res.Status)
			assert.Equal(t, agentcontrol.ExecOutcomeMismatch, res.Outcome)

			unknown, err := f.srv.AgentExecRunStatus(ctx, f.launched.ID, f.launched.RunID, "00000000-0000-4000-8000-0000000000ff")
			require.NoError(t, err)
			assert.Equal(t, http.StatusNotFound, unknown.Status)
			assert.Equal(t, agentcontrol.ExecStateUnknown, unknown.State)
		})
	}
}

// TestAgentExecClient_NotReached: no session, or a session that does not
// advertise the exec routes (an agent launched without the flag), is an
// error and nothing is sent.
func TestAgentExecClient_NotReached(t *testing.T) {
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	ctx := context.Background()
	_, err := f.srv.StartAgentExecRun(ctx, f.launched.ID, f.launched.RunID, execStartReq("true"))
	require.ErrorIs(t, err, router.ErrNoSession)
	_, err = f.srv.AgentExecRunStatus(ctx, f.launched.ID, f.launched.RunID, execTestRunID)
	require.ErrorIs(t, err, router.ErrNoSession)

	// The real control handler without exec options: exec routes are
	// neither advertised nor served.
	ca := f.startControlAgent(t, f.public.URL, true, nil)
	_, err = f.srv.StartAgentExecRun(ctx, f.launched.ID, f.launched.RunID, execStartReq("true"))
	require.ErrorIs(t, err, ErrAgentControlNotAdvertised)
	_, err = f.srv.AgentExecRunStatus(ctx, f.launched.ID, f.launched.RunID, execTestRunID)
	require.ErrorIs(t, err, ErrAgentControlNotAdvertised)
	assert.Zero(t, ca.rpcs.Load())
}

// TestAgentExecClient_SchemaMismatchIsError: an answer outside the route's
// schema is an error wrapping ErrExecSchema, never a success.
func TestAgentExecClient_SchemaMismatchIsError(t *testing.T) {
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	var mu sync.Mutex
	var status int32
	var body string
	c := &execCapture{answer: func(*conduitv1.RpcRequest) *conduitv1.RpcResponse {
		mu.Lock()
		defer mu.Unlock()
		return &conduitv1.RpcResponse{Status: status, Body: []byte(body)}
	}}
	f.startControlAgentWithOptions(t, f.public.URL, true, execControlOptions(t), c.wrap)
	set := func(s int32, b string) { mu.Lock(); status, body = s, b; mu.Unlock() }
	for _, a := range []struct {
		status int32
		body   string
	}{
		{202, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0}`},
		{200, `{"state":"running"}`},
		{200, `{"state":"finished","outcome":"ok","replayed":true}`},
		{202, `{"state":"running","stdout":"leak"}`},
		{500, `{}`},
		{404, `404 page not found`},
		{202, ``},
	} {
		set(a.status, a.body)
		_, err := f.srv.StartAgentExecRun(context.Background(), f.launched.ID, f.launched.RunID, execStartReq("true"))
		assert.ErrorIs(t, err, agentcontrol.ErrExecSchema, "start %d %s", a.status, a.body)
	}
	for _, a := range []struct {
		status int32
		body   string
	}{
		{200, `{"state":"finished","outcome":"ok"}`},
		{200, `{"state":"bogus"}`},
		{400, `{"outcome":"invalid","field":"run_id"}`},
		{404, `404 page not found`},
	} {
		set(a.status, a.body)
		_, err := f.srv.AgentExecRunStatus(context.Background(), f.launched.ID, f.launched.RunID, execTestRunID)
		assert.ErrorIs(t, err, agentcontrol.ErrExecSchema, "status %d %s", a.status, a.body)
	}
}

// TestAgentExecClient_InvalidRequestNotSent: a request outside the bounds
// is refused by the hub before anything is sent.
func TestAgentExecClient_InvalidRequestNotSent(t *testing.T) {
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	c := &execCapture{}
	ca := f.startControlAgentWithOptions(t, f.public.URL, true, execControlOptions(t), c.wrap)
	bad := execStartReq("a\x00b")
	_, err := f.srv.StartAgentExecRun(context.Background(), f.launched.ID, f.launched.RunID, bad)
	var fe *agentcontrol.ExecFieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, agentcontrol.FieldArgv, fe.Field)
	bad = execStartReq("true")
	bad.TimeoutS = agentcontrol.MaxExecTimeoutSec + 1
	_, err = f.srv.StartAgentExecRun(context.Background(), f.launched.ID, f.launched.RunID, bad)
	require.ErrorAs(t, err, &fe)
	_, err = f.srv.AgentExecRunStatus(context.Background(), f.launched.ID, f.launched.RunID, "not-a-uuid")
	require.ErrorAs(t, err, &fe)
	assert.Zero(t, ca.rpcs.Load())
}

// TestAgentExecClient_LaunchPinned (D9-S3): a call for a run admitted under
// another launch (or with no launch) sends nothing.
func TestAgentExecClient_LaunchPinned(t *testing.T) {
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	ctx := context.Background()
	ca := f.startControlAgentWithOptions(t, f.public.URL, true, execControlOptions(t), (&execCapture{}).wrap)
	_, err := f.srv.StartAgentExecRun(ctx, f.launched.ID, "other-launch", execStartReq("true"))
	var le *AgentExecLaunchChangedError
	require.ErrorAs(t, err, &le)
	_, err = f.srv.AgentExecRunStatus(ctx, f.launched.ID, "other-launch", execTestRunID)
	require.ErrorIs(t, err, ErrAgentExecLaunchChanged)
	_, err = f.srv.AgentExecRunStatus(ctx, f.launched.ID, "", execTestRunID)
	require.ErrorIs(t, err, ErrAgentExecLaunchChanged)
	assert.Zero(t, ca.rpcs.Load(), "nothing sent for another launch")
}

// execSnapshotKey marks the calls whose first agent read returns a stale
// snapshot.
type execSnapshotKey struct{}

// staleSnapshotStore answers the first GetAgent of a marked call with the
// row as it was under oldLaunch; every later read sees the current row.
type staleSnapshotStore struct {
	store.Store
	oldLaunch string
	mu        sync.Mutex
	reads     int
}

func (s *staleSnapshotStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err != nil || ctx.Value(execSnapshotKey{}) == nil {
		return a, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.reads == 1 {
		stale := *a
		stale.RunID = s.oldLaunch
		return &stale, nil
	}
	return a, nil
}

// TestAgentExecClient_StaleSnapshotNeverReachesReplacement is the review
// round 1 regression (B1 / B-SEC-1): the run was admitted under launch
// old-launch; by the time the call routes, the row and the connected
// session belong to a replacement launch. Neither exec-start nor
// exec-status may reach the replacement session, locally or across nodes:
// nothing is sent and the command never runs.
func TestAgentExecClient_StaleSnapshotNeverReachesReplacement(t *testing.T) {
	for _, crossNode := range []bool{false, true} {
		name := map[bool]string{false: "local", true: "cross-node"}[crossNode]
		t.Run(name, func(t *testing.T) {
			var wrapped *staleSnapshotStore
			f := newPTYConduitFixtureServer(t, func(s *Server) {
				wrapped = &staleSnapshotStore{Store: s.store, oldLaunch: "old-launch"}
				s.store = wrapped
			})
			enableExecSchedules(t, f.srv)
			hubURL := f.public.URL
			if crossNode {
				hubURL = f.startPeerRelay(t, "hub-b")
			}
			c := &execCapture{}
			opts := execControlOptions(t)
			ca := f.startControlAgentWithOptions(t, hubURL, true, opts, c.wrap)
			marker := filepath.Join(opts.Exec.Workdir, "wrong-incarnation-started")

			ctx := context.WithValue(context.Background(), execSnapshotKey{}, true)
			_, err := f.srv.StartAgentExecRun(ctx, f.launched.ID, "old-launch", execStartReq("sh", "-c", "touch "+marker))
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrAgentExecLaunchChanged) || errors.Is(err, router.ErrNoSession), "err = %v", err)

			wrapped.mu.Lock()
			wrapped.reads = 0
			wrapped.mu.Unlock()
			_, err = f.srv.AgentExecRunStatus(ctx, f.launched.ID, "old-launch", execTestRunID)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrAgentExecLaunchChanged) || errors.Is(err, router.ErrNoSession), "err = %v", err)

			time.Sleep(300 * time.Millisecond)
			_, statErr := os.Stat(marker)
			assert.True(t, os.IsNotExist(statErr), "the command ran on the replacement incarnation")
			assert.Zero(t, ca.rpcs.Load(), "an exec route reached the replacement incarnation")
		})
	}
}

// TestAgentExecClient_GenerationFallbackSessionRefused: a session admitted
// through the interim generation fallback (no presented launch id) matches
// the router's second incarnation choice, but is not provably of the run's
// launch: it is refused before anything is sent.
func TestAgentExecClient_GenerationFallbackSessionRefused(t *testing.T) {
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	ca := f.startControlAgentAs(t, f.public.URL, "", true, execControlOptions(t), (&execCapture{}).wrap)
	ctx := context.Background()
	_, err := f.srv.StartAgentExecRun(ctx, f.launched.ID, f.launched.RunID, execStartReq("true"))
	require.ErrorIs(t, err, ErrAgentExecLaunchChanged)
	_, err = f.srv.AgentExecRunStatus(ctx, f.launched.ID, f.launched.RunID, execTestRunID)
	require.ErrorIs(t, err, ErrAgentExecLaunchChanged)
	assert.Zero(t, ca.rpcs.Load(), "nothing sent to a generation-fallback session")
}

// TestAgentExecClient_CallTimeout: the 30s per-route timeout bounds a call
// to an agent that never answers, through the real client path; one RPC,
// no retry.
func TestAgentExecClient_CallTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 30s exec call timeout")
	}
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := &execCapture{answer: func(*conduitv1.RpcRequest) *conduitv1.RpcResponse {
		<-release
		return &conduitv1.RpcResponse{Status: 202, Body: []byte(`{"state":"running"}`)}
	}}
	ca := f.startControlAgentWithOptions(t, f.public.URL, true, execControlOptions(t), c.wrap)
	start := time.Now()
	_, err := f.srv.StartAgentExecRun(context.Background(), f.launched.ID, f.launched.RunID, execStartReq("true"))
	took := time.Since(start)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, took, agentcontrol.HubExecCallTimeout-time.Second)
	assert.Less(t, took, agentcontrol.HubExecCallTimeout+10*time.Second)
	assert.Equal(t, int32(1), ca.rpcs.Load(), "one RPC, never resent")
}

// TestDispatchExperiments_ExecSchedules: hub.exec_schedules reaches the
// broker and the agent only while conduit is served (design D8).
func TestDispatchExperiments_ExecSchedules(t *testing.T) {
	f := newPTYConduitFixture(t)
	has := func() bool { return slices.Contains(f.srv.dispatchExperiments(), experiments.ExecSchedules) }
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: true})
	assert.True(t, has(), "both on")
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
	assert.False(t, has(), "exec_schedules off")
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: false, experiments.ExecSchedules: true})
	assert.False(t, has(), "conduit off")
}

// TestApplyAgentExperiments_ExecSchedules: the launch flag is sent to the
// agent in SCION_HUB_EXPERIMENTS.
func TestApplyAgentExperiments_ExecSchedules(t *testing.T) {
	env := map[string]string{}
	cls := map[string]api.EnvKind{}
	applyAgentExperiments(env, &cls, []string{conduitExperiment, experiments.ExecSchedules, experiments.K8sNFSHome})
	assert.Equal(t, "hub.conduit,hub.exec_schedules", env[envHubExperiments])
	if runtime.GOOS == "linux" {
		assert.True(t, control.ExecEnabled(func(k string) string { return env[k] }), "sciontool must read the value the hub sends")
	}

	env = map[string]string{envHubExperiments: "hub.conduit,hub.exec_schedules"}
	applyAgentExperiments(env, &cls, []string{conduitExperiment})
	assert.Equal(t, "hub.conduit", env[envHubExperiments], "a forged flag does not survive")
}

// TestExecControlRoutes_NotReachableFromSessions (AC 18): a user session's
// RPC to an exec route is refused by the hub (the user-session RPC surface
// serves only /v1/tunnels) and an agent session gets 501; neither reaches
// the target agent's control handler.
func TestExecControlRoutes_NotReachableFromSessions(t *testing.T) {
	tf := newTunnelFixture(t)
	c := &execCapture{}
	ca := tf.startControlAgentWithOptions(t, tf.public.URL, true, execControlOptions(t), c.wrap)
	body, _ := json.Marshal(agentcontrol.ExecStartRequest{V: 1, RunID: execTestRunID, Argv: []string{"true"}, TimeoutS: 5,
		NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	owner := tf.ownerClient(t)
	for _, route := range []string{agentcontrol.RouteExecStart, agentcontrol.RouteExecStatus} {
		for _, p := range []string{"/v1/control/" + route, "/" + route, "/v1/tunnels/../control/" + route} {
			resp, err := owner.ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodPost, Path: p, Body: body,
				Headers: map[string]string{"Content-Type": "application/json"}})
			require.NoError(t, err)
			assert.Equal(t, int32(http.StatusNotFound), resp.GetStatus(), "user session %s", p)
		}
	}
	// A tunnel request naming an exec route as its kind is invalid.
	tb, _ := json.Marshal(map[string]any{"agent": tf.launched.ID, "kind": "exec", "params": map[string]string{"argv": "true"}})
	resp, err := owner.ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodPost, Path: conduitTunnelsPath, Body: tb})
	require.NoError(t, err)
	assert.Equal(t, int32(http.StatusBadRequest), resp.GetStatus())

	ls, _, err := tf.dial(t, tf.agentToken(t, tf.launched), tf.launched.RunID)
	require.NoError(t, err)
	resp, err = ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodPost, Path: "/v1/control/" + agentcontrol.RouteExecStart, Body: body,
		Headers: map[string]string{"Content-Type": "application/json"}})
	require.NoError(t, err)
	assert.Equal(t, int32(http.StatusNotImplemented), resp.GetStatus(), "agent session")

	assert.Zero(t, ca.rpcs.Load(), "nothing reached the agent's control handler")
}

// TestExecControlRoutes_NotReachableThroughDeliver (AC 18): the broker's
// /deliver endpoint is fixed to deliver-message; an exec-start body is
// refused and never forwarded, and only deliver-message ever reaches the
// agent through it.
func TestExecControlRoutes_NotReachableThroughDeliver(t *testing.T) {
	f := newDeliverFixture(t)
	c := &execCapture{}
	opts := execControlOptions(t)
	opts.Deliver = &control.DeliverOptions{Runner: &okTmux{}}
	ca := f.startControlAgentWithOptions(t, f.public.URL, true, opts, c.wrap)
	execBody, _ := json.Marshal(agentcontrol.ExecStartRequest{V: 1, RunID: execTestRunID, Argv: []string{"true"}, TimeoutS: 5,
		NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli()})
	code, resp := f.postDeliver(t, f.broker, f.launched.ID, execBody)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, agentcontrol.OutcomeInvalid, resp.Outcome)
	// A deliver body carrying a route field is refused too (strict schema).
	withRoute := `{"v":1,"delivery_id":"d1","message_ids":["m1"],"op":"submit_only","deadline_ms":1000,"submit_keys":["Enter"],"route":"exec-start"}`
	code, _ = f.postDeliver(t, f.broker, f.launched.ID, []byte(withRoute))
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Zero(t, ca.rpcs.Load(), "nothing forwarded to the agent")
	assert.Zero(t, c.count(execRoutePath(agentcontrol.RouteExecStart)))
}

// TestExecControlCallers guards the amendment's first bound: in pkg/hub,
// only conduit_exec_control.go names the exec routes, and the typed
// client is called from no other non-test file than the scheduler's exec
// fire, run tracker and sweeper (scheduled_exec_fire.go, ptone/scion#7883).
// No user-facing endpoint may ever be added there.
func TestExecControlCallers(t *testing.T) {
	allowedCallers := map[string]bool{
		"conduit_exec_control.go": true,
		"scheduled_exec_fire.go":  true,
	}
	fset := token.NewFileSet()
	// pkgmove:scan-covers guardScanSubdirs
	// The client and every possible caller live in pkg/hub and its
	// guard-scanned subdirectories.
	files, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				switch x.Sel.Name {
				case "RouteExecStart", "RouteExecStatus", "StartAgentExecRun", "AgentExecRunStatus", "callAgentExec":
					if !allowedCallers[name] {
						t.Errorf("%s: %s refers to %s; only the scheduler's exec path may call the exec routes",
							fset.Position(x.Pos()), name, x.Sel.Name)
					}
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && (strings.Contains(x.Value, "exec-start") || strings.Contains(x.Value, "exec-status")) && !allowedCallers[name] {
					t.Errorf("%s: %s names an exec route literally", fset.Position(x.Pos()), name)
				}
			}
			return true
		})
	}
}

func execRoutePath(route string) string { return agentControlPrefix + route }

// enableExecSchedules turns on the hub gate of exec-start (hub.conduit and
// hub.exec_schedules, AC18).
func enableExecSchedules(t *testing.T, srv *Server) {
	t.Helper()
	setHubExperiments(t, srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: true})
}
