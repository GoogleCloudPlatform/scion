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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

// Typed client of the exec control routes (exec-schedules design
// "sciontool exec control routes", D9 = b';
// .design/conduit/exec-control-routes.md). Only the scheduler's exec event
// handler and its run tracker and sweeper may call these (ptone/scion#7883):
// never a user-facing endpoint, and there is no API through which a caller
// reaches exec-start. TestExecControlCallers guards that.
//
// Both calls go through callAgentControlJSON: the agent's own conduit
// session for its current incarnation, only when the session advertises
// the route, with agentcontrol.HubExecCallTimeout. The hub never resends
// exec-start: one attempt per run; on an ambiguous error the caller only
// polls exec-status.

// ErrAgentExecLaunchChanged: the agent's current launch is not the one the
// run was admitted under (D9-S3). Before the call nothing was sent; after
// it, the answer came from (or raced) another incarnation and is
// discarded. Either way the run is abandoned, never re-queried on the
// replacement incarnation.
var ErrAgentExecLaunchChanged = errors.New("agent exec: the agent's launch changed")

// AgentExecLaunchChangedError wraps ErrAgentExecLaunchChanged. Sent reports
// whether the request was sent before the change was seen.
type AgentExecLaunchChangedError struct {
	AgentID string
	Sent    bool
}

func (e *AgentExecLaunchChangedError) Error() string {
	return fmt.Sprintf("%v: agent %s (sent=%t)", ErrAgentExecLaunchChanged, e.AgentID, e.Sent)
}

func (e *AgentExecLaunchChangedError) Unwrap() error { return ErrAgentExecLaunchChanged }

// StartAgentExecRun sends exec-start for one admitted run to the agent's
// launch launchID. It sets req.V and req.NotAfterUnixMS (send time plus
// agentcontrol.ExecNotAfter, on the hub clock) and validates req before
// sending; an invalid req is an *agentcontrol.ExecFieldError and nothing is
// sent.
//
// A nil error carries the agent's documented answer (ExecResult.Status
// 202, 200 replayed, 409 overlap/busy/mismatch, 408 expired_before_start,
// 400). Errors:
//   - router.ErrNoSession (possibly wrapped) or ErrAgentControlNotAdvertised
//     (wrapped): nothing was sent (the run is skipped, "agent executor
//     unavailable");
//   - *AgentExecLaunchChangedError: the launch differs (Sent says whether
//     anything was sent);
//   - agentcontrol.ErrExecSchema (wrapped): the agent answered something
//     outside the schema; never success;
//   - otherwise the store, router or session error. A timeout or drop after
//     send is ambiguous: the run may have started.
func (s *Server) StartAgentExecRun(ctx context.Context, agentID, launchID string, req agentcontrol.ExecStartRequest) (agentcontrol.ExecResult, error) {
	req.V = agentcontrol.ExecVersion
	req.NotAfterUnixMS = time.Now().Add(agentcontrol.ExecNotAfter).UnixMilli()
	if err := req.Validate(); err != nil {
		return agentcontrol.ExecResult{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return agentcontrol.ExecResult{}, err
	}
	status, resp, err := s.callAgentExec(ctx, agentID, launchID, agentcontrol.RouteExecStart, body)
	if err != nil {
		return agentcontrol.ExecResult{}, err
	}
	return agentcontrol.ParseExecStartAnswer(status, resp)
}

// AgentExecRunStatus reads exec-status for runID on the agent's launch
// launchID. It is a read and safe to repeat. A nil error carries 200
// running or finished, or 404 unknown (ExecResult.State). Errors are as for
// StartAgentExecRun; a 400 is a schema error (the hub sent a bad request).
func (s *Server) AgentExecRunStatus(ctx context.Context, agentID, launchID, runID string) (agentcontrol.ExecResult, error) {
	req := agentcontrol.ExecStatusRequest{V: agentcontrol.ExecVersion, RunID: runID}
	if err := req.Validate(); err != nil {
		return agentcontrol.ExecResult{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return agentcontrol.ExecResult{}, err
	}
	status, resp, err := s.callAgentExec(ctx, agentID, launchID, agentcontrol.RouteExecStatus, body)
	if err != nil {
		return agentcontrol.ExecResult{}, err
	}
	return agentcontrol.ParseExecStatusAnswer(status, resp)
}

// callAgentExec calls an exec route on the agent's launch launchID. The
// router binds the call to the incarnation it reads from the agent row
// (agentIncarnationFacts), so the launch is checked before the call (a
// mismatch sends nothing) and again after it (a change in between discards
// the answer).
func (s *Server) callAgentExec(ctx context.Context, agentID, launchID, route string, body []byte) (int, []byte, error) {
	if launchID == "" {
		return 0, nil, &AgentExecLaunchChangedError{AgentID: agentID}
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return 0, nil, err
	}
	if agent.RunID != launchID {
		return 0, nil, &AgentExecLaunchChangedError{AgentID: agentID}
	}
	status, resp, err := s.callAgentControlJSON(ctx, agentID, route, body, agentcontrol.HubExecCallTimeout)
	if err != nil {
		return 0, nil, err
	}
	agent, err = s.store.GetAgent(ctx, agentID)
	if err != nil {
		return 0, nil, err
	}
	if agent.RunID != launchID {
		return 0, nil, &AgentExecLaunchChangedError{AgentID: agentID, Sent: true}
	}
	return status, resp, nil
}
