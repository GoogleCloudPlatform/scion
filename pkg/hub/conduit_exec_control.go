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
	"net/http"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Typed client of the exec control routes (exec-schedules design
// "sciontool exec control routes", D9 = b';
// .design/conduit/exec-control-routes.md). Only the scheduler's exec event
// handler and its run tracker and sweeper may call these (ptone/scion#7883):
// never a user-facing endpoint, and there is no API through which a caller
// reaches exec-start. TestExecControlCallers guards that.
//
// Both calls go through callAgentControlPinned: the agent's own conduit
// session of the run's launch only, only when the session advertises the
// route, with agentcontrol.HubExecCallTimeout. The hub never resends
// exec-start: one attempt per run; on an ambiguous error the caller only
// polls exec-status.

// ErrAgentExecLaunchChanged: the agent's current launch, or the only
// session the router can resolve, is not of the launch the run was admitted
// under (D9-S3). Nothing was sent; the run is abandoned, never queried on a
// replacement incarnation.
var ErrAgentExecLaunchChanged = errors.New("agent exec: the agent's launch changed")

// AgentExecLaunchChangedError wraps ErrAgentExecLaunchChanged.
type AgentExecLaunchChangedError struct {
	AgentID string
}

func (e *AgentExecLaunchChangedError) Error() string {
	return fmt.Sprintf("%v: agent %s (nothing sent)", ErrAgentExecLaunchChanged, e.AgentID)
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
//   - *AgentExecLaunchChangedError: the agent's launch, or the resolvable
//     session's, is not launchID; nothing was sent;
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

// callAgentExec calls an exec route on the agent's launch launchID only
// (D9-S3). It reads the agent row once, refuses a row whose launch is not
// launchID (nothing sent), and routes with that validated snapshot's
// incarnation facts (callAgentControlPinned), so the call can reach only a
// session of launch launchID, locally or through another relay, never a
// replacement incarnation.
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
	return s.callAgentControlPinned(ctx, agent, route, body, agentcontrol.HubExecCallTimeout)
}

// callAgentControlPinned is callAgentControlJSON for an agent snapshot the
// caller has already validated: the router is asked for the snapshot's
// incarnation (agentIncarnationFacts of agent, not a fresh read of the
// row), and the resolved session must have been admitted with the
// snapshot's launch id. A session admitted through the interim generation
// fallback, or of any other launch, is refused before anything is sent
// (*AgentExecLaunchChangedError). A session's incarnation is fixed at
// admission and a relayed call is delivered to the resolved session id, so
// this holds locally and across nodes. The route must be advertised, as
// for callAgentControlJSON; errors are the same otherwise.
func (s *Server) callAgentControlPinned(ctx context.Context, agent *store.Agent, route string, body []byte, timeout time.Duration) (int, []byte, error) {
	if !s.conduitRoutable() {
		return 0, nil, router.ErrNoSession
	}
	rtr := s.conduitRouter()
	if rtr == nil {
		return 0, nil, router.ErrNoSession
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := router.Request{
		Op:    router.OpStatefulRPC,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID},
		Agent: agentIncarnationFacts(agent),
	}
	rpc := &conduitv1.RpcRequest{
		Method:  http.MethodPost,
		Path:    agentControlPrefix + route,
		Body:    body,
		Headers: map[string]string{"Content-Type": "application/json"},
	}
	var status int
	var respBody []byte
	err := rtr.Do(ctx, req, func(ctx context.Context, res router.Resolved) error {
		if res.Record.EndpointIncarnation != agent.RunID || res.Want.Incarnation != agent.RunID {
			return &AgentExecLaunchChangedError{AgentID: agent.ID}
		}
		if !slices.Contains(res.Session.Info().Capabilities.GetRpc(), route) {
			return fmt.Errorf("%w: agent %s, route %s", ErrAgentControlNotAdvertised, agent.ID, route)
		}
		resp, err := res.Session.Call(ctx, rpc)
		if err != nil {
			return err
		}
		status, respBody = int(resp.GetStatus()), resp.GetBody()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return status, respBody, nil
}
