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

package tmuxdeliver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// Default readiness timings. They apply when the init-run file has no line 2
// (an image older than the ready spec); otherwise line 2's settle and
// deadline are used.
const (
	DefaultReadyPollInterval  = 500 * time.Millisecond
	DefaultReadyDeadline      = state.ReadyDeadline
	DefaultReadySettle        = state.ReadySettleHook
	DefaultReadySettleSession = state.ReadySettleSession
	// ReadyProbeTimeout bounds one probe run.
	ReadyProbeTimeout = 5 * time.Second
)

// ModeNoInitRun labels a probe of an image that writes no init-run file (an
// older sciontool). It is handled like settle mode.
const ModeNoInitRun = "no-init-run"

// Timings are the readiness wait timings. A zero field uses its default.
type Timings struct {
	// PollInterval is the delay between probes.
	PollInterval time.Duration
	// Deadline bounds the whole wait, measured from its start.
	Deadline time.Duration
	// Settle is slept once readiness first appears during the wait, giving
	// the harness's input a moment to mount after its hook fired.
	Settle time.Duration
	// SettleFallback is the minimum tmux session age that counts as ready in
	// settle mode, or when the image writes no init-run file.
	SettleFallback time.Duration
}

// WithDefaults returns t with every zero field set to its default.
func (t Timings) WithDefaults() Timings {
	if t.PollInterval <= 0 {
		t.PollInterval = DefaultReadyPollInterval
	}
	if t.Deadline <= 0 {
		t.Deadline = DefaultReadyDeadline
	}
	if t.Settle <= 0 {
		t.Settle = DefaultReadySettle
	}
	if t.SettleFallback <= 0 {
		t.SettleFallback = DefaultReadySettleSession
	}
	return t
}

// ProbeArgv is the single command run per probe. It reports whether tmux
// session "scion" exists and when it was created, the container's current
// time (so every age uses one clock), the first line of each readiness file
// and the second line of the init-run file (the ready spec), size-capped,
// and the ready marker's mtime (stat -c %Y, which busybox and coreutils both
// support). Either file is read only when it is a regular file and not a
// symlink (regularFileGuard). It always exits 0; missing pieces show up as
// empty values. The script is a constant: nothing in it comes from a
// request.
func ProbeArgv() []string {
	script := `if tmux has-session -t scion 2>/dev/null; then ` +
		`echo "session=$(tmux display-message -p -t scion: '#{session_created}' 2>/dev/null)"; ` +
		`else echo "session="; fi; ` +
		`echo "now=$(date +%s)"; ` +
		`echo "initrun=$(` + regularFileGuard(state.HarnessInitRunFile) + ` && head -c 256 ` + state.HarnessInitRunFile + ` 2>/dev/null | head -n 1)"; ` +
		`echo "initspec=$(` + regularFileGuard(state.HarnessInitRunFile) + ` && head -c 256 ` + state.HarnessInitRunFile + ` 2>/dev/null | sed -n 2p)"; ` +
		`echo "ready=$(` + regularFileGuard(state.HarnessReadyFile) + ` && head -c 256 ` + state.HarnessReadyFile + ` 2>/dev/null | head -n 1)"; ` +
		`echo "readymtime=$(` + regularFileGuard(state.HarnessReadyFile) + ` && stat -c %Y ` + state.HarnessReadyFile + ` 2>/dev/null)"`
	return []string{"sh", "-c", script}
}

// regularFileGuard is a shell test that succeeds only when path is a regular
// file and not a symlink; anything else (missing, a symlink, a FIFO, a
// directory) is reported as an empty value, the same as a missing file.
func regularFileGuard(path string) string {
	return `[ -f ` + path + ` ] && [ ! -L ` + path + ` ]`
}

// Probe is the parsed result of one probe.
type Probe struct {
	// Ready reports whether the harness can take a pasted message.
	Ready bool
	// Mode is state.ReadyModeHook, state.ReadyModeSettle or ModeNoInitRun.
	Mode string
	// Nonce is the init-run nonce, empty without a valid init-run file.
	Nonce string
	// Reason says why the probe is not ready; empty when Ready.
	Reason string
	// MarkerFresh reports, in hook mode, that the ready marker was written
	// less than the settle period ago, so delivery should settle first even
	// on the first probe.
	MarkerFresh bool
	// Settle is the post-marker settle this probe used: line 2's for hook
	// mode when the init-run file has one, otherwise the caller's.
	Settle time.Duration
	// Deadline is line 2's hook-mode deadline; zero when the probe has no
	// line 2 or is not in hook mode, which keeps the caller's deadline.
	Deadline time.Duration
	// HasSpec reports that the init-run file has a line 2 (the image's own
	// ready spec); SpecSettle and SpecTimeout are its settle (the post-marker
	// settle in hook mode, the session age in settle mode) and its hook
	// deadline (zero in settle mode).
	HasSpec     bool
	SpecSettle  time.Duration
	SpecTimeout time.Duration
}

// ParseProbe interprets ProbeArgv's output.
//
// Hook mode is ready when the tmux session exists and the ready marker holds
// the init-run nonce; a marker from an earlier run (another nonce) does not
// count, and there is no session-age fallback. Settle mode, or no valid
// init-run file (an older image), is ready when the session exists and is at
// least settleFallback old. In hook mode, MarkerFresh is set when the
// marker's mtime is less than settle before the container's current time.
//
// When the init-run file has a line 2 (the ready spec init wrote), its values
// replace the caller's: the settle is the session age in settle mode and the
// post-marker settle in hook mode, and the hook deadline is returned in
// Deadline. Without a line 2 the caller's timings apply.
func ParseProbe(out string, settleFallback, settle time.Duration) Probe {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, val, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			if _, seen := fields[key]; !seen {
				fields[key] = val
			}
		}
	}

	res := Probe{Mode: ModeNoInitRun}
	if run, ok := state.ParseInitRunSpec(fields["initrun"] + "\n" + fields["initspec"]); ok {
		res.Mode, res.Nonce = run.Mode, run.Nonce
		if run.HasSpec {
			res.HasSpec, res.SpecSettle = true, run.Spec.Settle
			if run.Mode == state.ReadyModeHook {
				res.SpecTimeout = run.Spec.Timeout
			}
			if run.Mode == state.ReadyModeHook {
				settle = run.Spec.Settle
				res.Deadline = run.Spec.Timeout
			} else {
				settleFallback = run.Spec.Settle
			}
		}
	}
	res.Settle = settle

	created, err := strconv.ParseInt(strings.TrimSpace(fields["session"]), 10, 64)
	if err != nil || created <= 0 {
		res.Reason = "tmux session not found"
		return res
	}

	if res.Mode == state.ReadyModeHook {
		marker, ok := state.ParseReadyMarker(fields["ready"])
		switch {
		case !ok:
			res.Reason = "ready marker missing"
		case marker != res.Nonce:
			res.Reason = "ready marker from another run"
		default:
			res.Ready = true
			now, nerr := strconv.ParseInt(strings.TrimSpace(fields["now"]), 10, 64)
			mtime, merr := strconv.ParseInt(strings.TrimSpace(fields["readymtime"]), 10, 64)
			if nerr == nil && merr == nil && time.Duration(now-mtime)*time.Second < settle {
				res.MarkerFresh = true
			}
		}
		return res
	}

	now, err := strconv.ParseInt(strings.TrimSpace(fields["now"]), 10, 64)
	if err != nil {
		res.Reason = "container time unavailable"
		return res
	}
	if age := time.Duration(now-created) * time.Second; age < settleFallback {
		res.Reason = fmt.Sprintf("session younger than %s", settleFallback)
		return res
	}
	res.Ready = true
	return res
}

// WaitOutcome says how a readiness wait ended.
type WaitOutcome int

const (
	// WaitReady: the first probe was ready and nothing needed settling, or
	// the run had already been seen ready.
	WaitReady WaitOutcome = iota
	// WaitSettled: readiness appeared during the wait (or the marker was
	// fresh) and the settle period was slept.
	WaitSettled
	// WaitTimedOut: the deadline passed first. FirstTimeout tells whether it
	// is the first timeout recorded for this run.
	WaitTimedOut
	// WaitRunTimedOut: an earlier wait for this hook-mode run already
	// reached the deadline, so this one did not wait.
	WaitRunTimedOut
	// WaitCancelled: ctx ended.
	WaitCancelled
	// WaitTargetGone: a probe failed and the Gone hook reported the target
	// gone.
	WaitTargetGone
	// WaitOutOfBounds: the image's own ready spec (init-run line 2) has a
	// settle or timeout above WaitConfig.BoundSettle or BoundTimeout. The
	// wait stopped at once, without waiting or settling; Reason names the
	// field (FieldReadySettle or FieldReadyTimeout).
	WaitOutOfBounds
)

// WaitResult is what Wait returns. The caller logs it; Wait logs nothing.
type WaitResult struct {
	Outcome WaitOutcome
	Mode    string
	Reason  string
	Waited  time.Duration
	// FirstTimeout is set with WaitTimedOut when this is the first timeout
	// recorded for the run, or when the run has no nonce: the caller warns
	// only then.
	FirstTimeout bool
	// ProbeErr is the last probe error, if any.
	ProbeErr error
}

// WaitConfig configures one Wait.
type WaitConfig struct {
	// Runner runs ProbeArgv. Wait probes only through it.
	Runner Runner
	// Timings are the wait timings; zero fields use their defaults.
	Timings Timings
	// Memo remembers outcomes per init run; nil disables the memo.
	Memo *MemoStore
	// Key identifies the target in Memo, for example the container ID.
	Key string
	// MaxDeadline and MaxSettle, when not zero, cap the deadline and the
	// settle sleep, including the values the init-run file's line 2 sets
	// (up to 10 min and 60 s). The deliver-message route sets them to its
	// bounds (MaxReadyTimeout, MaxReadySettle) so the wait always ends
	// before its hard stop. Zero means no cap: the exec path is unchanged.
	MaxDeadline time.Duration
	MaxSettle   time.Duration
	// BoundSettle and BoundTimeout, when not zero, refuse an image ready
	// spec above them: Wait returns WaitOutOfBounds after the first probe
	// that shows it, instead of capping the spec. The deliver-message route
	// sets them to its bounds (MaxReadySettle, MaxReadyTimeout), so it never
	// shortens a harness's readiness policy; the caller then uses exec.
	BoundSettle  time.Duration
	BoundTimeout time.Duration
	// Gone, when set, is asked after a failed probe whether the target is
	// gone; true ends the wait at once.
	Gone func(ctx context.Context, probeErr error) bool
	// Now and Sleep replace the clock in tests; nil uses the real clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool
}

// Wait waits until the harness is ready to read a pasted message, polling
// with one probe run per poll (see ParseProbe for what "ready" means per
// mode). It waits once per delivery, never per retry, and takes no lock.
//
// If readiness first appears during the wait, or the ready marker was written
// less than the settle period ago (even on the first probe), it sleeps the
// settle period before returning WaitSettled. At the deadline it returns
// WaitTimedOut, and the caller pastes anyway: that is not a delivery failure.
// The settle and deadline come from the init-run file's line 2 when the image
// writes one, checked on every probe; the deadline is still measured from the
// start of the wait.
//
// Outcomes are remembered per init-run nonce in Memo: once a hook-mode wait
// for nonce N reaches the deadline, later waits return WaitRunTimedOut as soon
// as a probe shows the same nonce still not ready. Once N has been seen ready,
// a later wait does not settle unless the marker is fresh.
//
// It returns at once when ctx is cancelled.
func Wait(ctx context.Context, cfg WaitConfig) WaitResult {
	timings := cfg.Timings.WithDefaults()
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	start := now()
	capDeadline := func(d time.Duration) time.Duration {
		if cfg.MaxDeadline > 0 && d > cfg.MaxDeadline {
			return cfg.MaxDeadline
		}
		return d
	}
	capSettle := func(d time.Duration) time.Duration {
		if cfg.MaxSettle > 0 && d > cfg.MaxSettle {
			return cfg.MaxSettle
		}
		return d
	}
	deadline := start.Add(capDeadline(timings.Deadline))

	probe := func() (Probe, error) {
		probeTimeout := deadline.Sub(now())
		if probeTimeout > ReadyProbeTimeout {
			probeTimeout = ReadyProbeTimeout
		}
		if probeTimeout <= 0 {
			probeTimeout = time.Millisecond
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		out, err := cfg.Runner.Run(pctx, ProbeArgv(), nil)
		if err != nil {
			return Probe{Mode: ModeNoInitRun, Reason: "probe failed: " + err.Error()}, err
		}
		return ParseProbe(out, timings.SettleFallback, timings.Settle), nil
	}

	lastNonce := ""
	var memo memoEntry
	for first := true; ; first = false {
		res, perr := probe()
		if res.HasSpec {
			if cfg.BoundSettle > 0 && res.SpecSettle > cfg.BoundSettle {
				return WaitResult{Outcome: WaitOutOfBounds, Mode: res.Mode, Reason: FieldReadySettle, Waited: now().Sub(start)}
			}
			if cfg.BoundTimeout > 0 && res.SpecTimeout > cfg.BoundTimeout {
				return WaitResult{Outcome: WaitOutOfBounds, Mode: res.Mode, Reason: FieldReadyTimeout, Waited: now().Sub(start)}
			}
		}
		if res.Deadline > 0 {
			deadline = start.Add(capDeadline(res.Deadline))
		}
		if res.Nonce != "" {
			if res.Nonce != lastNonce {
				memo = cfg.Memo.get(cfg.Key, res.Nonce)
			}
			lastNonce = res.Nonce
		}
		result := WaitResult{Mode: res.Mode, Reason: res.Reason, ProbeErr: perr}
		if res.Ready {
			if res.Nonce != "" {
				cfg.Memo.markReady(cfg.Key, res.Nonce)
			}
			if !res.MarkerFresh && (first || memo.ready) {
				result.Outcome, result.Waited = WaitReady, now().Sub(start)
				return result
			}
			if !sleep(ctx, capSettle(res.Settle)) {
				result.Outcome, result.Waited = WaitCancelled, now().Sub(start)
				return result
			}
			result.Outcome, result.Waited = WaitSettled, now().Sub(start)
			return result
		}
		if ctx.Err() != nil {
			result.Outcome, result.Waited = WaitCancelled, now().Sub(start)
			return result
		}
		// memo is loaded when this wait first sees the nonce and is never
		// updated by this wait, so timedOut means an earlier wait for this
		// run reached the deadline.
		if res.Mode == state.ReadyModeHook && memo.timedOut {
			result.Outcome, result.Waited = WaitRunTimedOut, now().Sub(start)
			return result
		}
		if perr != nil && cfg.Gone != nil && cfg.Gone(ctx, perr) {
			result.Outcome, result.Waited = WaitTargetGone, now().Sub(start)
			return result
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			result.FirstTimeout = true
			if res.Mode == state.ReadyModeHook && lastNonce != "" {
				result.FirstTimeout = cfg.Memo.markTimedOut(cfg.Key, lastNonce)
			}
			result.Outcome, result.Waited = WaitTimedOut, now().Sub(start)
			return result
		}
		if !sleep(ctx, min(timings.PollInterval, remaining)) {
			result.Outcome, result.Waited = WaitCancelled, now().Sub(start)
			return result
		}
	}
}

// MaxMemos bounds MemoStore.
const MaxMemos = 1024

// memoEntry is what is known about one init run (one nonce).
type memoEntry struct {
	key string
	// ready: the run has been seen ready.
	ready bool
	// timedOut: a hook-mode wait for the run reached the deadline.
	timedOut bool
}

// MemoStore remembers readiness outcomes per init-run nonce. A nonce is fresh
// for every `sciontool init` run, so a recreated container or a new run in
// the same container never inherits an outcome, even where the key repeats.
// Recording a new nonce for a key drops that key's older entries; the store
// is bounded by MaxMemos. The zero value is ready to use, and a nil
// *MemoStore remembers nothing.
type MemoStore struct {
	mu sync.Mutex
	m  map[string]memoEntry // keyed by nonce
}

func (s *MemoStore) get(key, nonce string) memoEntry {
	if s == nil {
		return memoEntry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[nonce]; ok && e.key == key {
		return e
	}
	return memoEntry{}
}

// update applies fn to the entry for nonce, creating it (and evicting the
// key's older nonces, and if full one other entry) when new.
func (s *MemoStore) update(key, nonce string, fn func(*memoEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]memoEntry)
	}
	e, ok := s.m[nonce]
	if !ok || e.key != key {
		for n, other := range s.m {
			if other.key == key {
				delete(s.m, n)
			}
		}
		if len(s.m) >= MaxMemos {
			for n := range s.m {
				delete(s.m, n)
				break
			}
		}
		e = memoEntry{key: key}
	}
	fn(&e)
	s.m[nonce] = e
}

func (s *MemoStore) markReady(key, nonce string) {
	if s == nil {
		return
	}
	s.update(key, nonce, func(e *memoEntry) { e.ready = true })
}

// markTimedOut records a deadline for nonce and reports whether this is the
// first one recorded for it. A nil store reports true.
func (s *MemoStore) markTimedOut(key, nonce string) (first bool) {
	if s == nil {
		return true
	}
	s.update(key, nonce, func(e *memoEntry) {
		first = !e.timedOut
		e.timedOut = true
	})
	return first
}

// Clear drops every entry.
func (s *MemoStore) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.m = nil
	s.mu.Unlock()
}

// Len returns the number of entries.
func (s *MemoStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// sleepCtx sleeps for d, returning false early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
