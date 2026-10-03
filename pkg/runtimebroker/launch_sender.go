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
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Per-attempt timeouts and retry backoff ranges (design §3.8.5, §3.10),
// named so their values can be asserted directly rather than only through
// timing-sensitive tests. Each report kind gets its own attempt-timeout
// constant even though all three are 5s today, so a change to one cannot
// silently change the others.
const (
	claimAttemptTimeout      = 5 * time.Second
	checkpointAttemptTimeout = 5 * time.Second
	keepaliveAttemptTimeout  = 5 * time.Second
	terminalAttemptTimeout   = 5 * time.Second

	reportMinBackoff = 1 * time.Second
	reportMaxBackoff = 10 * time.Second

	keepaliveMinBackoff = 2 * time.Second
	keepaliveMaxBackoff = 5 * time.Second
)

// errLaunchReportUnreachable is sendOnce's "retry" bucket: a transport
// failure, a 5xx, a non-structured 404 (design §5 N-9, handled inside
// hubclient.ReportAgentLaunch), or no hub connection at all.
var errLaunchReportUnreachable = errors.New("launch report: no reachable hub connection")

// keepaliveOutcome is what the keepalive loop records when a report answer
// classifies to anything other than gateContinue (design §3.8.2's
// gate-answer table applies to keepalives too, not only claim/checkpoint).
type keepaliveOutcome struct {
	action gateAction
	result *hubclient.AgentLaunchReportResult
}

// launchSender is one launch's report sender (design §3.8.5: "One per
// launch"). It owns the keepalive goroutine and every report this launch
// sends. Nothing on launchSender ever touches an *http.Request: it is built
// from (projectID, slug)-resolved data before the launch goroutine starts,
// never from the request that accepted the launch (design §7 P1b-1 B-6).
type launchSender struct {
	server     *Server
	agentID    string
	instanceID string
	rec        *launchRecord

	keepaliveInterval time.Duration

	mu           sync.Mutex
	seq          int64
	completed    bool
	abortOutcome *keepaliveOutcome
	// checkpointUnreachable records that a checkpoint ran out of ctx'
	// without any definitive Hub answer (design §3.8.2: then abort with
	// failed{hub_unreachable}).
	checkpointUnreachable bool

	terminalOnce    sync.Once
	terminalStarted int32 // atomic
	stopKeepalive   chan struct{}
	// abortCh closes exactly once, when a keepalive answer classifies to an
	// abort action. runLaunch selects on it to react immediately instead of
	// waiting for Manager.Start to return on its own.
	abortCh     chan struct{}
	abortOnce   sync.Once
	keepaliveWG sync.WaitGroup
}

// newLaunchSender builds a sender for rec. keepaliveInterval <= 0 defaults to
// 15s (design §3.7's broker-side default, used when the Hub's create request
// omitted LaunchKeepaliveSeconds).
func newLaunchSender(server *Server, rec *launchRecord, agentID, instanceID string, keepaliveInterval time.Duration) *launchSender {
	if keepaliveInterval <= 0 {
		keepaliveInterval = 15 * time.Second
	}
	return &launchSender{
		server:            server,
		agentID:           agentID,
		instanceID:        instanceID,
		rec:               rec,
		keepaliveInterval: keepaliveInterval,
		stopKeepalive:     make(chan struct{}),
		abortCh:           make(chan struct{}),
	}
}

func (s *launchSender) nextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

func (s *launchSender) currentSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// isTerminalStarted reports whether a terminal send has begun -- from then
// on, "only the terminal's answer decides cleanup" (design §3.8.5), and the
// keepalive loop exits.
func (s *launchSender) isTerminalStarted() bool {
	return atomic.LoadInt32(&s.terminalStarted) == 1
}

func (s *launchSender) markTerminalStarted() {
	s.terminalOnce.Do(func() {
		atomic.StoreInt32(&s.terminalStarted, 1)
		close(s.stopKeepalive)
	})
}

// recordKeepaliveCompleted marks that a keepalive answer was "completed"
// (design: skip remaining checkpoints, Run continues; a later failure sends
// nothing and never cleans up). It does not stop the keepalive loop.
func (s *launchSender) recordKeepaliveCompleted() {
	s.mu.Lock()
	s.completed = true
	s.mu.Unlock()
}

// IsCompleted reports whether any report (claim or keepalive) has been
// answered "completed" so far.
func (s *launchSender) IsCompleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completed
}

// recordKeepaliveAbort records the first keepalive answer that classifies to
// an abort action and closes abortCh, waking anything selecting on
// KeepaliveAborted. Only the first call has any effect.
func (s *launchSender) recordKeepaliveAbort(action gateAction, result *hubclient.AgentLaunchReportResult) {
	s.abortOnce.Do(func() {
		s.mu.Lock()
		s.abortOutcome = &keepaliveOutcome{action: action, result: result}
		s.mu.Unlock()
		close(s.abortCh)
	})
}

// KeepaliveAborted returns a channel that closes once a keepalive answer has
// classified to an abort action (gateAbortCleanup or gateAbortNoCleanup).
// runLaunch selects on this alongside Manager.Start's completion so a stale
// 409, 403 or unknown-launch answer during a long Start acts immediately
// (design §3.8.2's gate-answer table) instead of waiting for Start to
// return.
func (s *launchSender) KeepaliveAborted() <-chan struct{} {
	return s.abortCh
}

// IsAborted is a non-blocking check of KeepaliveAborted, for use between
// synchronous steps that are not otherwise selecting on it.
func (s *launchSender) IsAborted() bool {
	select {
	case <-s.abortCh:
		return true
	default:
		return false
	}
}

// LastAbortOutcome returns the outcome recorded by recordKeepaliveAbort, or
// nil if KeepaliveAborted has not closed yet. Safe to call after
// KeepaliveAborted closes.
func (s *launchSender) LastAbortOutcome() *keepaliveOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortOutcome
}

// targets resolves which hub connection(s) a report should go to (design
// §3.8.5 routing). rec.HubName (resolved at admission from the header or the
// authenticating connection, or the sole connection) pins to one connection
// for the whole launch. Otherwise, once the fan-out has pinned OwnerHub, every
// later report goes only there. With neither set, every connection with a
// HubClient is a target (routing rule 4).
func (s *launchSender) targets() []*HubConnection {
	s.server.hubMu.RLock()
	defer s.server.hubMu.RUnlock()

	if name := s.rec.HubName; name != "" {
		if conn, ok := s.server.hubConnections[name]; ok && conn.HubClient != nil {
			return []*HubConnection{conn}
		}
		return nil
	}
	if name := s.rec.OwnerHub(); name != "" {
		if conn, ok := s.server.hubConnections[name]; ok && conn.HubClient != nil {
			return []*HubConnection{conn}
		}
		return nil
	}
	var all []*HubConnection
	for _, conn := range s.server.hubConnections {
		if conn.HubClient != nil {
			all = append(all, conn)
		}
	}
	return all
}

// sendTo posts report to one connection, bounding the attempt at timeout.
func (s *launchSender) sendTo(ctx context.Context, conn *HubConnection, report *hubclient.AgentLaunchReport, timeout time.Duration) (*hubclient.AgentLaunchReportResult, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.HubClient.RuntimeBrokers().ReportAgentLaunch(attemptCtx, conn.BrokerID, s.agentID, report)
}

// fanOutAttempt is one connection's answer, used only inside sendOnce's
// fan-out branch.
type fanOutAttempt struct {
	conn   *HubConnection
	result *hubclient.AgentLaunchReportResult
	err    error
}

// sendOnce makes one attempt. With more than one target it fans out
// concurrently (design §3.8.5 routing rule 4): the first 2xx or 409
// stale_launch pins OwnerHub and is returned immediately; a 403 is
// definitive for that connection (this broker is not the agent's broker
// there) but does not pin, so other connections are still consulted;
// agent_launch_unknown from every connection means unknown (returned as a
// definitive result, not an error); any unreachable answer means retry
// (returned as errLaunchReportUnreachable), even mixed with definitive
// unknown or 403 answers from other connections, since the unreachable one
// might still turn out to be the owner.
func (s *launchSender) sendOnce(ctx context.Context, report *hubclient.AgentLaunchReport, timeout time.Duration) (*hubclient.AgentLaunchReportResult, error) {
	targets := s.targets()
	if len(targets) == 0 {
		return nil, errLaunchReportUnreachable
	}
	if len(targets) == 1 {
		return s.sendTo(ctx, targets[0], report, timeout)
	}

	resultsCh := make(chan fanOutAttempt, len(targets))
	for _, conn := range targets {
		conn := conn
		go func() {
			result, err := s.sendTo(ctx, conn, report, timeout)
			resultsCh <- fanOutAttempt{conn, result, err}
		}()
	}

	var sawUnreachable, sawUnknown bool
	var lastDefiniteNonPinning *hubclient.AgentLaunchReportResult
	for i := 0; i < len(targets); i++ {
		at := <-resultsCh
		if at.err != nil {
			sawUnreachable = true
			continue
		}
		if at.result.HTTPStatus == http.StatusForbidden || at.result.HTTPStatus == http.StatusBadRequest || at.result.HTTPStatus == http.StatusUnauthorized {
			// 403 is definitive for that connection (this broker is not the
			// agent's broker there) but never pins; 400/401 get the same
			// treatment so they can never be preferred over a connection
			// that genuinely owns the launch (design §3.8.2's gate-answer
			// table does not list 400/401 as a reason to prefer one
			// connection over another).
			lastDefiniteNonPinning = at.result
			continue
		}
		if at.result.HTTPStatus == http.StatusNotFound && at.result.Code == hubclient.AgentLaunchReportCodeUnknownLaunch {
			sawUnknown = true
			continue
		}
		// A 2xx or a 409 stale_launch: pin and return immediately. Any
		// slower attempts still in flight deliver into the buffered
		// channel harmlessly and are never read.
		s.rec.SetOwnerHub(at.conn.Name)
		return at.result, nil
	}
	if sawUnreachable {
		return nil, errLaunchReportUnreachable
	}
	if sawUnknown {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, nil
	}
	if lastDefiniteNonPinning != nil {
		return lastDefiniteNonPinning, nil
	}
	return nil, errLaunchReportUnreachable
}

// jitteredBackoff returns a random duration in [min, max).
func jitteredBackoff(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)))
}

// errAbortedByKeepalive is sendReportBlocking's signal that a concurrent
// keepalive answer classified to an abort action while this call was still
// retrying (design §3.8.2's gate-answer table), so the caller should stop
// retrying and act on that outcome (launchSender.LastAbortOutcome) instead
// of whatever this call was waiting for.
var errAbortedByKeepalive = errors.New("launch report: a keepalive answer ended the launch")

// sendReportBlocking retries sendOnce with a jittered backoff in
// [minBackoff, maxBackoff) until it gets a definitive answer or ctx is done.
// abortable also lets a keepalive answer that concurrently classified to an
// abort action end the wait early; SendClaim and Checkpoint set it. Design §3.8.5:
// "the keepalive stops when the terminal's first attempt starts; from then
// on only the terminal's answer decides cleanup" -- so SendTerminal must
// keep retrying on its own TTL-bounded ctx regardless of abortCh, never
// handing a terminal's outcome to a keepalive answer that arrived
// concurrently with it.
func (s *launchSender) sendReportBlocking(ctx context.Context, report *hubclient.AgentLaunchReport, minBackoff, maxBackoff, attemptTimeout time.Duration, abortable bool) (*hubclient.AgentLaunchReportResult, error) {
	for {
		if abortable {
			select {
			case <-s.abortCh:
				return nil, errAbortedByKeepalive
			default:
			}
		}
		result, err := s.sendOnce(ctx, report, attemptTimeout)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(jitteredBackoff(minBackoff, maxBackoff))
		if !abortable {
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-s.abortCh:
			timer.Stop()
			return nil, errAbortedByKeepalive
		}
	}
}

// SendClaim sends the synchronous claim report before anything touches the
// runtime (design §3.8.2 step 5.1). It blocks, retrying with a 1-10s
// backoff, until ctx (the launch's ctx') is done.
func (s *launchSender) SendClaim(ctx context.Context) (*hubclient.AgentLaunchReportResult, error) {
	report := &hubclient.AgentLaunchReport{
		LaunchID:   s.rec.ID,
		InstanceID: s.instanceID,
		Seq:        s.nextSeq(),
		State:      hubclient.AgentLaunchReportStateClaim,
		At:         time.Now(),
	}
	return s.sendReportBlocking(ctx, report, reportMinBackoff, reportMaxBackoff, claimAttemptTimeout, true)
}

// errLaunchEndedAtCheckpoint is what Checkpoint returns to the runtime when
// the launch must create nothing further: a checkpoint answer (or an
// earlier keepalive answer) ended it. The runtime returns it, wrapped, from
// Run; runLaunch then acts on the recorded outcome (LastAbortOutcome), not
// on this error.
var errLaunchEndedAtCheckpoint = errors.New("launch checkpoint: the launch has ended")

// Checkpoint is the runtime's pre-create gate (api.StartOptions.Checkpoint,
// design t1-async-create-v11.md §3.8.3), called immediately before each
// resource-creating call. It sends a synchronous checkpoint report and
// blocks, retrying with a 1-10s backoff, until a definitive answer or until
// ctx (the launch's ctx') is done. Answers follow the §3.8.2 table:
//   - applied/duplicate: return nil, the create proceeds;
//   - completed: return nil and skip every later checkpoint (Run continues);
//   - any abort answer: record it exactly as a keepalive abort would (so
//     runLaunch's KeepaliveAborted select wakes and applies the cleanup
//     rule) and return errLaunchEndedAtCheckpoint, so nothing is created.
//
// A checkpoint after an earlier abort returns errLaunchEndedAtCheckpoint
// without sending.
func (s *launchSender) Checkpoint(ctx context.Context, step string) error {
	if s.IsCompleted() {
		return nil
	}
	if s.IsAborted() {
		return errLaunchEndedAtCheckpoint
	}
	report := &hubclient.AgentLaunchReport{
		LaunchID:   s.rec.ID,
		InstanceID: s.instanceID,
		Seq:        s.nextSeq(),
		State:      hubclient.AgentLaunchReportStateCheckpoint,
		Step:       step,
		At:         time.Now(),
	}
	result, err := s.sendReportBlocking(ctx, report, reportMinBackoff, reportMaxBackoff, checkpointAttemptTimeout, true)
	if err != nil {
		if errors.Is(err, errAbortedByKeepalive) {
			return errLaunchEndedAtCheckpoint
		}
		if errors.Is(err, context.DeadlineExceeded) {
			s.mu.Lock()
			s.checkpointUnreachable = true
			s.mu.Unlock()
		}
		return fmt.Errorf("launch checkpoint %s: %w", step, err)
	}
	switch action := classifyGateAnswer(result); action {
	case gateContinue:
		return nil
	case gateCompleted:
		s.recordKeepaliveCompleted()
		return nil
	case gateStopNoCleanup:
		s.server.agentLifecycleLog.Error("runLaunch: checkpoint got a protocol/auth answer; stopping with no cleanup",
			"agent_id", s.agentID, "launch_id", s.rec.ID, "step", step, "http_status", result.HTTPStatus)
		s.recordKeepaliveAbort(action, result)
		return errLaunchEndedAtCheckpoint
	default:
		s.recordKeepaliveAbort(action, result)
		return errLaunchEndedAtCheckpoint
	}
}

// CheckpointUnreachable reports whether a checkpoint ran out of ctx'
// without a definitive Hub answer.
func (s *launchSender) CheckpointUnreachable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointUnreachable
}

// StartKeepalive starts the per-launch keepalive goroutine (design §3.8.5).
// Started before the claim even returns: the design's B-3 test case expects
// the keepalive to keep its cadence "while a claim or checkpoint is blocked
// on backoff", which requires it running from the start, not only once the
// claim succeeds. Never blocked by Manager.Start, a blocked claim/
// checkpoint, or a pending progress post; each attempt times out at 5s; a
// failed attempt is retried after a 2-5s backoff; the loop exits once a
// terminal send starts, a keepalive answer aborts, or ctx is done.
func (s *launchSender) StartKeepalive(ctx context.Context) {
	s.keepaliveWG.Add(1)
	go func() {
		defer s.keepaliveWG.Done()
		timer := time.NewTimer(s.keepaliveInterval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopKeepalive:
				return
			case <-s.abortCh:
				return
			case <-timer.C:
			}
			if s.isTerminalStarted() {
				return
			}
			s.sendKeepaliveOnce(ctx)
			if s.isTerminalStarted() {
				return
			}
			select {
			case <-s.abortCh:
				return
			default:
			}
			timer.Reset(s.keepaliveInterval)
		}
	}()
}

// StopKeepalive stops the keepalive loop without claiming a terminal has
// started (unlike markTerminalStarted). Safe to call multiple times and
// before StartKeepalive.
func (s *launchSender) StopKeepalive() {
	s.terminalOnce.Do(func() {
		close(s.stopKeepalive)
	})
}

// WaitKeepaliveStopped blocks until the keepalive goroutine has exited.
// Callers stop the loop first (StopKeepalive, markTerminalStarted via
// SendTerminal, an abort, or ctx being done) and then call this so runLaunch
// never returns while the keepalive goroutine is still running.
func (s *launchSender) WaitKeepaliveStopped() {
	s.keepaliveWG.Wait()
}

// sendKeepaliveOnce sends one keepalive, retrying on an unreachable answer
// after a 2-5s jittered backoff until it lands, ctx is done, or a terminal
// send starts. A definitive non-continue answer is classified exactly like a
// claim/checkpoint answer (design §3.8.2's table applies to keepalives too):
// "completed" is recorded for later terminal handling; an abort action is
// recorded and wakes KeepaliveAborted.
func (s *launchSender) sendKeepaliveOnce(ctx context.Context) {
	report := &hubclient.AgentLaunchReport{
		LaunchID:   s.rec.ID,
		InstanceID: s.instanceID,
		Seq:        s.currentSeq(),
		State:      hubclient.AgentLaunchReportStateProgress,
		At:         time.Now(),
	}
	for {
		if s.isTerminalStarted() {
			return
		}
		result, err := s.sendOnce(ctx, report, keepaliveAttemptTimeout)
		if err == nil {
			if s.isTerminalStarted() {
				// The terminal's first attempt started while this
				// in-flight keepalive attempt was still waiting on its
				// answer. Design §3.8.5: "the keepalive stops when the
				// terminal's first attempt starts; from then on only the
				// terminal's answer decides cleanup." Drop this answer
				// rather than act on it -- acting on it here could abort
				// the terminal's own retries (sendReportBlocking's
				// abortable path) or record an outcome the terminal's own
				// answer should decide instead.
				return
			}
			switch action := classifyGateAnswer(result); action {
			case gateContinue:
				// nothing to do
			case gateCompleted:
				s.recordKeepaliveCompleted()
			case gateStopNoCleanup:
				// A 400/401 on a keepalive is not evidence the launch is
				// over (design §3.8.2's table has no entry for it), so
				// unlike every other non-continue answer this does not
				// abort the launch; it is logged and the keepalive carries
				// on.
				s.server.agentLifecycleLog.Error("runLaunch: keepalive got a protocol/auth answer; continuing",
					"agent_id", s.agentID, "launch_id", s.rec.ID, "http_status", result.HTTPStatus)
			default:
				s.recordKeepaliveAbort(action, result)
			}
			return
		}
		timer := time.NewTimer(jitteredBackoff(keepaliveMinBackoff, keepaliveMaxBackoff))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stopKeepalive:
			timer.Stop()
			return
		}
	}
}

// SendTerminal sends the launch's terminal report (succeeded or failed),
// retrying with a 1-10s backoff, each attempt bounded at 5s, until ctx (the
// terminal's own TTL-bounded context, design §3.8.5: deadline + 10 min, NOT
// ctx') is done or a definitive answer arrives. It stops the keepalive loop
// first: "only the terminal's answer decides cleanup" from this point on
// (design §3.8.5). step records which generic step (claim/launching) the
// launch was in when it failed (design §3.9's generic claim -> launching ->
// terminal sequence); ignored for a succeeded terminal.
func (s *launchSender) SendTerminal(ctx context.Context, succeeded bool, step, errorCode, message string, agentInfo *hubclient.AgentLaunchReportInfo) (*hubclient.AgentLaunchReportResult, error) {
	s.markTerminalStarted()
	state := hubclient.AgentLaunchReportStateFailed
	if succeeded {
		state = hubclient.AgentLaunchReportStateSucceeded
		step = ""
	}
	report := &hubclient.AgentLaunchReport{
		LaunchID:   s.rec.ID,
		InstanceID: s.instanceID,
		Seq:        s.currentSeq(),
		State:      state,
		Step:       step,
		ErrorCode:  errorCode,
		Message:    message,
		Agent:      agentInfo,
		At:         time.Now(),
	}
	return s.sendReportBlocking(ctx, report, reportMinBackoff, reportMaxBackoff, terminalAttemptTimeout, false)
}
