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
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// The exec routes (exec-schedules design "sciontool exec control routes",
// D9 = b'; .design/conduit/exec-control-routes.md). They are registered
// only when Options.Exec is set, which sciontool does only for an agent
// launched with hub.exec_schedules (ExecEnabled).
const (
	RouteExecStart  = agentcontrol.RouteExecStart
	RouteExecStatus = agentcontrol.RouteExecStatus
)

// ExperimentExecSchedules is the launch-time flag of the exec routes.
const ExperimentExecSchedules = "hub.exec_schedules"

// Key store and concurrency bounds (design D5, D9-S2).
const (
	// execMaxLive is the most runs live in this container at once.
	execMaxLive = 2
	// execTombMax, execTombTTL and execTombFloor bound the finished-run
	// tombstones: at most execTombMax, each kept execTombTTL, LRU, but an
	// entry younger than execTombFloor is never evicted.
	execTombMax   = 256
	execTombTTL   = 60 * time.Minute
	execTombFloor = 10 * time.Minute
	// execMaxBody bounds an exec request body: the argv bounds plus JSON
	// escaping (up to 6 bytes per byte) and the other fields.
	execMaxBody = 6*agentcontrol.MaxExecArgvBytes + 4096
)

// ExecOptions configure the exec routes.
type ExecOptions struct {
	// UID, GID and Username are the identity runs execute as: the conduit
	// PTY user (conduit.PTYUser), with its supplementary groups. A UID or
	// GID that is not positive means sciontool's own identity, which is
	// refused when RequirePrivilegeDrop is set.
	UID, GID             int
	Username             string
	RequirePrivilegeDrop bool
	// Env returns the environment of a run: the harness's tmux session
	// environment (supervisor.HarnessShellEnv). Called once per run.
	// Required.
	Env func() ([]string, error)
	// LaunchID is the agent launch this sciontool serves; every run_id is
	// bound to it.
	LaunchID string
	// Workdir is the preferred working directory; "" means /workspace.
	// When it is not a directory the user's home is used.
	Workdir string
	// Now replaces the clock in tests; nil uses time.Now.
	Now func() time.Time
}

// execBinding is what a run_id is bound to (D9-S2). A start whose run_id
// is known with another binding is refused with mismatch.
type execBinding struct {
	scheduleID string
	digest     string
	timeoutS   int
	launchID   string
}

// execFinal is a finished run's outcome: never output or error text.
type execFinal struct {
	outcome     string
	exitCode    int
	durationMS  int64
	outputBytes int64
}

// execRun is a live run: reserved in the live map before its spawn, until
// its main process has exited (and, on timeout, the group kill sequence
// has finished).
type execRun struct {
	runID   string
	binding execBinding
	// decided is closed once the spawn decision is made: the process
	// started, failed to start, or the start was refused. Duplicates and
	// status reads wait for it.
	decided chan struct{}
	// refused (set before decided closes) means the start was refused
	// before spawning (expired_before_start); the run never existed.
	refused bool
}

// execRoute serves exec-start and exec-status.
type execRoute struct {
	opts ExecOptions
	now  func() time.Time

	// Platform executor and its timings (tests shorten them).
	start     func(spec execSpec) (execProcess, error)
	killGrace time.Duration
	waitDelay time.Duration

	mu         sync.Mutex
	live       map[string]*execRun
	bySchedule map[string]string // schedule id -> run id of its live run
	tombs      *execTombs

	// beforeSpawn, when set (tests), runs after the run is reserved and
	// before the spawn-time checks.
	beforeSpawn func(runID string)
	// finished, when set (tests), runs after a run's tombstone is stored.
	finished func(runID string)
}

// execSpec is one run as the platform executor starts it.
type execSpec struct {
	path      string
	argv      []string
	env       []string
	dir       string
	uid, gid  int
	drop      bool
	timeout   time.Duration
	killGrace time.Duration
	waitDelay time.Duration
}

// execProcess is a started run. wait blocks until the run is over: the
// main process exited normally, or the timeout kill sequence finished,
// and the main process has been reaped.
type execProcess interface {
	wait() execFinal
}

func newExecRoute(o ExecOptions) *execRoute {
	if o.Env == nil {
		panic("control: ExecOptions.Env is required")
	}
	e := &execRoute{
		opts:       o,
		now:        o.Now,
		start:      startExec,
		killGrace:  agentcontrol.ExecKillGrace,
		waitDelay:  agentcontrol.ExecWaitDelay,
		live:       map[string]*execRun{},
		bySchedule: map[string]string{},
	}
	if e.now == nil {
		e.now = time.Now
	}
	e.tombs = newExecTombs(execTombMax, execTombTTL, execTombFloor)
	return e
}

// execAnswer is one answer and the fields its log line carries.
type execAnswer struct {
	status int
	resp   agentcontrol.ExecResponse
}

func (e *execRoute) serveStart(w http.ResponseWriter, r *http.Request) {
	receipt := e.now()
	body, field := readExecBody(r)
	var req *agentcontrol.ExecStartRequest
	if field == "" {
		var err error
		req, err = agentcontrol.DecodeExecStartRequest(body)
		if err == nil {
			err = req.Validate()
		}
		field = execErrField(err)
	}
	if field != "" {
		log.Info("Control: exec-start refused (field %s)", field)
		writeExec(w, execInvalid(field))
		return
	}
	ans := e.startRun(r.Context(), req, receipt)
	e.logStart(req, ans)
	writeExec(w, ans)
}

// startRun admits and spawns one run, or answers a replay or refusal.
func (e *execRoute) startRun(ctx context.Context, req *agentcontrol.ExecStartRequest, receipt time.Time) execAnswer {
	b := execBinding{
		scheduleID: req.ScheduleID,
		digest:     agentcontrol.ExecArgvDigest(req.Argv),
		timeoutS:   req.TimeoutS,
		launchID:   e.opts.LaunchID,
	}
	deadline := time.UnixMilli(req.NotAfterUnixMS).Add(agentcontrol.ExecClockTolerance)

	e.mu.Lock()
	if run, ok := e.live[req.RunID]; ok {
		e.mu.Unlock()
		if run.binding != b {
			return execRefusal(http.StatusConflict, agentcontrol.ExecOutcomeMismatch)
		}
		return e.replayLive(ctx, run)
	}
	if t, ok := e.tombs.get(e.now(), req.RunID); ok {
		e.mu.Unlock()
		if t.binding != b {
			return execRefusal(http.StatusConflict, agentcontrol.ExecOutcomeMismatch)
		}
		return finishedAnswer(http.StatusOK, t.final, true)
	}
	// A new run_id. Nothing below spawns once the deadline has passed.
	if receipt.After(deadline) {
		e.mu.Unlock()
		return execRefusal(http.StatusRequestTimeout, agentcontrol.ExecOutcomeExpiredBeforeStart)
	}
	if b.scheduleID != "" {
		if _, ok := e.bySchedule[b.scheduleID]; ok {
			e.mu.Unlock()
			return execRefusal(http.StatusConflict, agentcontrol.ExecOutcomeOverlap)
		}
	}
	// Capacity is reserved before the spawn: a live slot and the
	// tombstone slot the run will need when it finishes.
	if len(e.live) >= execMaxLive || !e.tombs.canReserve(e.now(), len(e.live)+1) {
		e.mu.Unlock()
		return execRefusal(http.StatusConflict, agentcontrol.ExecOutcomeBusy)
	}
	run := &execRun{runID: req.RunID, binding: b, decided: make(chan struct{})}
	e.live[req.RunID] = run
	if b.scheduleID != "" {
		e.bySchedule[b.scheduleID] = req.RunID
	}
	e.mu.Unlock()

	if e.beforeSpawn != nil {
		e.beforeSpawn(req.RunID)
	}
	spec, prepErr := e.prepare(req)

	// The spawn-time checks (D9-S1): the deadline again, and spawn within
	// ExecSpawnWithin of receipt.
	if now := e.now(); now.After(deadline) || now.Sub(receipt) > agentcontrol.ExecSpawnWithin {
		e.mu.Lock()
		e.release(run)
		run.refused = true
		e.mu.Unlock()
		close(run.decided)
		return execRefusal(http.StatusRequestTimeout, agentcontrol.ExecOutcomeExpiredBeforeStart)
	}

	var proc execProcess
	if prepErr == nil {
		proc, prepErr = e.start(spec)
	}
	if prepErr != nil {
		// The raw error can name a path or argv: it is never returned or
		// logged, only its class.
		final := execFinal{outcome: agentcontrol.ExecOutcomeStartFailed}
		e.finish(run, final)
		close(run.decided)
		return finishedAnswer(http.StatusAccepted, final, false)
	}
	close(run.decided)
	go func() {
		final := proc.wait()
		e.finish(run, final)
		e.logFinish(run, final)
	}()
	return execAnswer{status: http.StatusAccepted, resp: agentcontrol.ExecResponse{State: agentcontrol.ExecStateRunning}}
}

// replayLive answers a start for a live run_id with the same binding,
// once its spawn decision is made.
func (e *execRoute) replayLive(ctx context.Context, run *execRun) execAnswer {
	select {
	case <-run.decided:
	case <-ctx.Done():
		// The caller gave up; nothing was started for this request.
		return execAnswer{status: http.StatusOK, resp: agentcontrol.ExecResponse{State: agentcontrol.ExecStateRunning, Replayed: true}}
	}
	if run.refused {
		return execRefusal(http.StatusRequestTimeout, agentcontrol.ExecOutcomeExpiredBeforeStart)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.live[run.runID]; ok {
		return execAnswer{status: http.StatusOK, resp: agentcontrol.ExecResponse{State: agentcontrol.ExecStateRunning, Replayed: true}}
	}
	if t, ok := e.tombs.get(e.now(), run.runID); ok {
		return finishedAnswer(http.StatusOK, t.final, true)
	}
	// Finished and already gone from the store: never restart it.
	return execRefusal(http.StatusConflict, agentcontrol.ExecOutcomeMismatch)
}

// release removes run from the live maps. e.mu must be held.
func (e *execRoute) release(run *execRun) {
	if e.live[run.runID] == run {
		delete(e.live, run.runID)
	}
	if s := run.binding.scheduleID; s != "" && e.bySchedule[s] == run.runID {
		delete(e.bySchedule, s)
	}
}

// finish moves run from the live map to a tombstone, atomically, so a
// replay sees one or the other and never neither.
func (e *execRoute) finish(run *execRun, final execFinal) {
	e.mu.Lock()
	e.release(run)
	e.tombs.put(e.now(), &execTomb{runID: run.runID, binding: run.binding, final: final})
	e.mu.Unlock()
	if e.finished != nil {
		e.finished(run.runID)
	}
}

// prepare builds the run's spec: the environment, the working directory,
// the executable resolved on the run's own PATH, and the identity. An
// error means start_failed; its text is never shown.
func (e *execRoute) prepare(req *agentcontrol.ExecStartRequest) (execSpec, error) {
	o := e.opts
	drop := o.UID > 0 && o.GID > 0
	if o.RequirePrivilegeDrop && !drop {
		return execSpec{}, errExecNoPrivilegeDrop
	}
	env, err := o.Env()
	if err != nil {
		return execSpec{}, err
	}
	dir := execWorkdir(o.Workdir, o.Username, env)
	env = setEnv(env, "PWD", dir)
	path, err := lookPathEnv(req.Argv[0], envValue(env, "PATH"), dir)
	if err != nil {
		return execSpec{}, err
	}
	return execSpec{
		path:      path,
		argv:      append([]string(nil), req.Argv...),
		env:       env,
		dir:       dir,
		uid:       o.UID,
		gid:       o.GID,
		drop:      drop,
		timeout:   time.Duration(req.TimeoutS) * time.Second,
		killGrace: e.killGrace,
		waitDelay: e.waitDelay,
	}, nil
}

func (e *execRoute) serveStatus(w http.ResponseWriter, r *http.Request) {
	body, field := readExecBody(r)
	var req *agentcontrol.ExecStatusRequest
	if field == "" {
		var err error
		req, err = agentcontrol.DecodeExecStatusRequest(body)
		if err == nil {
			err = req.Validate()
		}
		field = execErrField(err)
	}
	if field != "" {
		log.Info("Control: exec-status refused (field %s)", field)
		writeExec(w, execInvalid(field))
		return
	}
	writeExec(w, e.status(r.Context(), req.RunID))
}

// status answers exec-status for runID. A run whose spawn decision is
// pending is waited for, so a status read never reports running for a
// start that is then refused.
func (e *execRoute) status(ctx context.Context, runID string) execAnswer {
	e.mu.Lock()
	run, live := e.live[runID]
	e.mu.Unlock()
	if live {
		select {
		case <-run.decided:
		case <-ctx.Done():
			return execAnswer{status: http.StatusOK, resp: agentcontrol.ExecResponse{State: agentcontrol.ExecStateRunning}}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.live[runID]; ok {
		return execAnswer{status: http.StatusOK, resp: agentcontrol.ExecResponse{State: agentcontrol.ExecStateRunning}}
	}
	if t, ok := e.tombs.get(e.now(), runID); ok {
		return finishedAnswer(http.StatusOK, t.final, false)
	}
	return execAnswer{status: http.StatusNotFound, resp: agentcontrol.ExecResponse{State: agentcontrol.ExecStateUnknown}}
}

// readExecBody reads a JSON request body, or names the failing field.
func readExecBody(r *http.Request) ([]byte, string) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, agentcontrol.FieldBody
	}
	limit := int64(min(execMaxBody, core.MaxRPCBody))
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, agentcontrol.FieldBody
	}
	return body, ""
}

func execErrField(err error) string {
	if err == nil {
		return ""
	}
	var fe *agentcontrol.ExecFieldError
	if errors.As(err, &fe) {
		return fe.Field
	}
	return agentcontrol.FieldBody
}

func execInvalid(field string) execAnswer {
	return execAnswer{status: http.StatusBadRequest, resp: agentcontrol.ExecResponse{Outcome: agentcontrol.ExecOutcomeInvalid, Field: field}}
}

func execRefusal(status int, outcome string) execAnswer {
	return execAnswer{status: status, resp: agentcontrol.ExecResponse{Outcome: outcome}}
}

func finishedAnswer(status int, f execFinal, replayed bool) execAnswer {
	resp := agentcontrol.ExecResponse{State: agentcontrol.ExecStateFinished, Outcome: f.outcome, Replayed: replayed}
	if f.outcome != agentcontrol.ExecOutcomeStartFailed {
		code, dur, n := f.exitCode, f.durationMS, f.outputBytes
		resp.ExitCode, resp.DurationMS, resp.OutputBytes = &code, &dur, &n
	}
	return execAnswer{status: status, resp: resp}
}

func writeExec(w http.ResponseWriter, a execAnswer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.status)
	_ = json.NewEncoder(w).Encode(a.resp)
}

// logStart and logFinish write the content-free info lines (SEC-P3): ids,
// status, outcome and numbers only; never argv, output or error text.
func (e *execRoute) logStart(req *agentcontrol.ExecStartRequest, a execAnswer) {
	log.Info("%s", execLogLine("exec-start", req.RunID, req.ScheduleID, a))
}

func (e *execRoute) logFinish(run *execRun, f execFinal) {
	log.Info("%s", execLogLine("exec-finish", run.runID, run.binding.scheduleID, finishedAnswer(0, f, false)))
}

func execLogLine(what, runID, scheduleID string, a execAnswer) string {
	line := fmt.Sprintf("Control: %s run_id=%s schedule_id=%s status=%d state=%s outcome=%s replayed=%t",
		what, runID, scheduleID, a.status, a.resp.State, a.resp.Outcome, a.resp.Replayed)
	if a.resp.ExitCode != nil {
		line += fmt.Sprintf(" exit_code=%d duration_ms=%d output_bytes=%d", *a.resp.ExitCode, *a.resp.DurationMS, *a.resp.OutputBytes)
	}
	return line
}

// execWorkdir is preferred if it is a directory, else the user's home.
func execWorkdir(preferred, username string, env []string) string {
	if preferred == "" {
		preferred = "/workspace"
	}
	if isDir(preferred) {
		return preferred
	}
	home := envValue(env, "HOME")
	if username != "" {
		home = "/home/" + username
	}
	if home != "" && isDir(home) {
		return home
	}
	return "/"
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// errExecNoPrivilegeDrop: the run would execute as sciontool's own
// identity while a privilege drop is required (the PTY path's rule).
var errExecNoPrivilegeDrop = errors.New("control: privilege drop required but no agent user is set")

// errNotFound: argv[0] is not an executable on the run's PATH.
var errNotFound = errors.New("control: executable not found")

// lookPathEnv resolves file the way a shell with PATH=pathEnv would: a
// name with a slash is used as it is (relative to dir), any other is
// searched in pathEnv's absolute entries. exec.Command would search
// sciontool's own PATH instead of the run's.
func lookPathEnv(file, pathEnv, dir string) (string, error) {
	if strings.Contains(file, "/") {
		if !filepath.IsAbs(file) {
			file = filepath.Join(dir, file)
		}
		if executable(file) {
			return file, nil
		}
		return "", errNotFound
	}
	for _, d := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(d) {
			continue
		}
		if p := filepath.Join(d, file); executable(p) {
			return p, nil
		}
	}
	return "", errNotFound
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // the last entry wins, as for exec
		}
	}
	return v
}

func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// countingDiscard counts and discards a run's output (D2 = b, SEC-P2):
// nothing is buffered, whatever the output size.
type countingDiscard struct{ n atomic.Int64 }

func (c *countingDiscard) Write(b []byte) (int, error) {
	c.n.Add(int64(len(b)))
	return len(b), nil
}

// ExecEnabled reports whether sciontool should register the exec routes:
// the platform runs them (Linux) and the agent was launched with
// hub.exec_schedules in SCION_HUB_EXPERIMENTS (sent by the hub only while
// hub.conduit is served). Turning the experiment on later does not reach a
// running agent: it registers the routes only after a restart.
func ExecEnabled(getenv func(string) string) bool {
	if !execSupported {
		return false
	}
	for _, name := range strings.Split(getenv(EnvHubExperiments), ",") {
		if strings.TrimSpace(name) == ExperimentExecSchedules {
			return true
		}
	}
	return false
}

// execTomb is a finished run: its binding and outcome only.
type execTomb struct {
	runID      string
	binding    execBinding
	final      execFinal
	finishedAt time.Time
}

// execTombs holds finished-run tombstones (D9-S2): at most max, each kept
// ttl, LRU, but an entry younger than floor is never evicted. Callers
// reserve a slot (canReserve) before a run spawns, so put never needs to
// evict an entry under the floor.
type execTombs struct {
	max   int
	ttl   time.Duration
	floor time.Duration
	order *list.List // front = most recently used
	byKey map[string]*list.Element
}

func newExecTombs(max int, ttl, floor time.Duration) *execTombs {
	return &execTombs{max: max, ttl: ttl, floor: floor, order: list.New(), byKey: map[string]*list.Element{}}
}

// purge drops tombstones older than ttl.
func (s *execTombs) purge(now time.Time) {
	for el := s.order.Front(); el != nil; {
		next := el.Next()
		if t := el.Value.(*execTomb); now.Sub(t.finishedAt) > s.ttl {
			s.order.Remove(el)
			delete(s.byKey, t.runID)
		}
		el = next
	}
}

// canReserve reports whether n more tombstones fit without evicting one
// younger than floor: tombstones under the floor plus n stay within max.
func (s *execTombs) canReserve(now time.Time, n int) bool {
	s.purge(now)
	young := 0
	for _, el := range s.byKey {
		if now.Sub(el.Value.(*execTomb).finishedAt) < s.floor {
			young++
		}
	}
	return young+n <= s.max
}

func (s *execTombs) get(now time.Time, runID string) (*execTomb, bool) {
	el, ok := s.byKey[runID]
	if !ok {
		return nil, false
	}
	t := el.Value.(*execTomb)
	if now.Sub(t.finishedAt) > s.ttl {
		s.order.Remove(el)
		delete(s.byKey, runID)
		return nil, false
	}
	s.order.MoveToFront(el)
	return t, true
}

// put stores t, finished at now, evicting least recently used entries at
// or above the floor while the store is over max.
func (s *execTombs) put(now time.Time, t *execTomb) {
	t.finishedAt = now
	if el, ok := s.byKey[t.runID]; ok {
		s.order.Remove(el)
	}
	s.byKey[t.runID] = s.order.PushFront(t)
	s.purge(now)
	for s.order.Len() > s.max {
		victim := (*list.Element)(nil)
		for el := s.order.Back(); el != nil; el = el.Prev() {
			if now.Sub(el.Value.(*execTomb).finishedAt) >= s.floor {
				victim = el
				break
			}
		}
		if victim == nil {
			// Unreachable while every spawn reserves its slot; keep the
			// bound rather than grow.
			victim = s.order.Back()
		}
		s.order.Remove(victim)
		delete(s.byKey, victim.Value.(*execTomb).runID)
	}
}

func (s *execTombs) len() int { return s.order.Len() }
