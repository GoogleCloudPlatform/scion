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
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// errLaunchReportUnreachable is sendOnce's "retry" bucket: a transport
// failure, a 5xx, a non-structured 404 (design §5 N-9, handled inside
// hubclient.ReportAgentLaunch), or no hub connection at all.
var errLaunchReportUnreachable = errors.New("launch report: no reachable hub connection")

// launchSender is one launch's report sender (design §3.8.5: "One per
// launch"). It owns the keepalive goroutine and every report this launch
// sends. Nothing on launchSender ever touches an *http.Request: it is built
// from (projectID, slug)-resolved data before the launch goroutine starts,
// never from the request that accepted the launch (design §7 P1b-1 B-6,
// asserted with -race).
type launchSender struct {
	server     *Server
	agentID    string
	instanceID string
	rec        *launchRecord

	keepaliveInterval time.Duration

	mu  sync.Mutex
	seq int64

	terminalOnce    sync.Once
	terminalStarted int32 // atomic
	stopKeepalive   chan struct{}
	keepaliveWG     sync.WaitGroup
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
// on, "only the terminal's answer decides cleanup" (design §3.8.5, r8-8),
// and the keepalive loop exits.
func (s *launchSender) isTerminalStarted() bool {
	return atomic.LoadInt32(&s.terminalStarted) == 1
}

func (s *launchSender) markTerminalStarted() {
	s.terminalOnce.Do(func() {
		atomic.StoreInt32(&s.terminalStarted, 1)
		close(s.stopKeepalive)
	})
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

// sendOnce makes one attempt, fanning out across every target when more than
// one applies (design §3.8.5 routing rule 4): the first 2xx or 409 pins
// OwnerHub and is returned; agent_launch_unknown from every connection means
// unknown (returned as a definitive result, not an error); a mix of
// unreachable and 404 means retry (returned as errLaunchReportUnreachable).
func (s *launchSender) sendOnce(ctx context.Context, report *hubclient.AgentLaunchReport, timeout time.Duration) (*hubclient.AgentLaunchReportResult, error) {
	targets := s.targets()
	if len(targets) == 0 {
		return nil, errLaunchReportUnreachable
	}
	if len(targets) == 1 {
		return s.sendTo(ctx, targets[0], report, timeout)
	}

	var sawUnknown, sawUnreachable bool
	for _, conn := range targets {
		result, err := s.sendTo(ctx, conn, report, timeout)
		if err != nil {
			sawUnreachable = true
			continue
		}
		if result.HTTPStatus == http.StatusNotFound && result.Code == hubclient.AgentLaunchReportCodeUnknownLaunch {
			sawUnknown = true
			continue
		}
		s.rec.SetOwnerHub(conn.Name)
		return result, nil
	}
	if sawUnreachable {
		return nil, errLaunchReportUnreachable
	}
	if sawUnknown {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, nil
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

// sendReportBlocking retries sendOnce with a jittered backoff in
// [minBackoff, maxBackoff) until it gets a definitive answer or ctx is done.
func (s *launchSender) sendReportBlocking(ctx context.Context, report *hubclient.AgentLaunchReport, minBackoff, maxBackoff, attemptTimeout time.Duration) (*hubclient.AgentLaunchReportResult, error) {
	for {
		result, err := s.sendOnce(ctx, report, attemptTimeout)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(jitteredBackoff(minBackoff, maxBackoff))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
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
	return s.sendReportBlocking(ctx, report, time.Second, 10*time.Second, 5*time.Second)
}

// StartKeepalive starts the per-launch keepalive goroutine (design §3.8.5):
// never blocked by Manager.Start, a blocked claim/checkpoint, or a pending
// progress post; each attempt times out at 5s; a failed attempt is retried
// after a 2-5s backoff; the loop exits once a terminal send starts or ctx is
// done.
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
			case <-timer.C:
			}
			if s.isTerminalStarted() {
				return
			}
			s.sendKeepaliveOnce(ctx)
			timer.Reset(s.keepaliveInterval)
		}
	}()
}

// WaitKeepaliveStopped blocks until the keepalive goroutine has exited.
func (s *launchSender) WaitKeepaliveStopped() {
	s.keepaliveWG.Wait()
}

// sendKeepaliveOnce sends one keepalive, retrying on an unreachable answer
// after a 2-5s jittered backoff until it lands, ctx is done, or a terminal
// send starts.
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
		_, err := s.sendOnce(ctx, report, 5*time.Second)
		if err == nil {
			return
		}
		timer := time.NewTimer(jitteredBackoff(2*time.Second, 5*time.Second))
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
// (design r8-8).
func (s *launchSender) SendTerminal(ctx context.Context, succeeded bool, phase, errorCode, message string, agentInfo *hubclient.AgentLaunchReportInfo) (*hubclient.AgentLaunchReportResult, error) {
	s.markTerminalStarted()
	state := hubclient.AgentLaunchReportStateFailed
	if succeeded {
		state = hubclient.AgentLaunchReportStateSucceeded
	}
	report := &hubclient.AgentLaunchReport{
		LaunchID:   s.rec.ID,
		InstanceID: s.instanceID,
		Seq:        s.currentSeq(),
		State:      state,
		Phase:      phase,
		ErrorCode:  errorCode,
		Message:    message,
		Agent:      agentInfo,
		At:         time.Now(),
	}
	return s.sendReportBlocking(ctx, report, time.Second, 10*time.Second, 5*time.Second)
}
