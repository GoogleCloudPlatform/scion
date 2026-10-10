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
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Hub-to-agent control RPCs (design §3.7, Phase 4). sciontool serves
// /v1/control/* on the agent's own conduit session and lists the routes
// it implements in Hello.capabilities.rpc. The hub calls a route only
// when the agent's session advertises it.

// Agent control routes. The names are the Hello.capabilities.rpc
// entries; the paths are what sciontool's control handler serves
// (pkg/sciontool/control).
const (
	agentControlPrefix           = "/v1/control/"
	agentControlRouteRotateToken = "rotate-token"
)

// agentControlCallTimeout caps one control request as a whole: the
// resolution, any re-resolutions the router makes, and the RPC. Control
// routes answer at once (rotate-token queues a refresh and returns 202),
// so the cap only bounds a session that stopped answering. The caller's
// ctx may be shorter.
const agentControlCallTimeout = 30 * time.Second

// ErrAgentControlNotAdvertised: the agent's conduit session does not list
// the control route in Hello.capabilities.rpc, so it was not called.
var ErrAgentControlNotAdvertised = errors.New("agent control: route not advertised by the agent session")

// AgentControlStatusError is a control RPC that the agent answered with a
// status other than the route's success status.
type AgentControlStatusError struct {
	AgentID string
	Route   string
	Status  int
}

func (e *AgentControlStatusError) Error() string {
	return fmt.Sprintf("agent control: %s on agent %s answered %d", e.Route, e.AgentID, e.Status)
}

// RequestAgentTokenRotation asks the agent to refresh its hub token now,
// by calling POST /v1/control/rotate-token on the agent's conduit session
// (local, or on another hub node through its relay). It returns nil when
// the agent answered 202, meaning the request is queued for the agent's
// refresh loop, not that a refresh has run. rotate-token coalesces, so a
// repeated request is harmless, but nothing here retries it.
//
// Errors:
//   - an error matching router.ErrNoSession (errors.Is) when the agent
//     has no eligible conduit session; the router may wrap it after
//     re-resolving a stale route. With hub.conduit off, or no relay on
//     this node, it is router.ErrNoSession itself and nothing is read or
//     resolved. There is no fallback through the broker.
//   - ErrAgentControlNotAdvertised (wrapped) when the session does not
//     advertise rotate-token; no RPC is sent.
//   - *AgentControlStatusError for any other status the agent answered.
//   - the store error when the agent row cannot be read, and the
//     router's or session's error (including ctx cancellation, which is
//     propagated to the agent as RpcCancel) otherwise.
func (s *Server) RequestAgentTokenRotation(ctx context.Context, agentID string) error {
	return s.callAgentControl(ctx, agentID, agentControlRouteRotateToken, http.StatusAccepted)
}

// callAgentControl calls POST /v1/control/<route> on the agent's conduit
// session and maps the answer: want is nil, anything else an
// AgentControlStatusError. The route is called only if the resolved
// session advertises it. The router is asked for no capability: the
// registry would filter a session that lacks it, which would report a
// connected agent as having no session.
func (s *Server) callAgentControl(ctx context.Context, agentID, route string, want int) error {
	if !s.conduitServing() {
		return router.ErrNoSession
	}
	rt := s.conduit.Load()
	if rt == nil || rt.router == nil {
		return router.ErrNoSession
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, agentControlCallTimeout)
	defer cancel()
	req := router.Request{
		Op:    router.OpStatefulRPC,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID},
		Agent: agentIncarnationFacts(agent),
	}
	return rt.router.Do(ctx, req, func(ctx context.Context, res router.Resolved) error {
		if !slices.Contains(res.Session.Info().Capabilities.GetRpc(), route) {
			return fmt.Errorf("%w: agent %s, route %s", ErrAgentControlNotAdvertised, agent.ID, route)
		}
		resp, err := res.Session.Call(ctx, &conduitv1.RpcRequest{
			Method: http.MethodPost,
			Path:   agentControlPrefix + route,
		})
		if err != nil {
			return err
		}
		if int(resp.GetStatus()) != want {
			return &AgentControlStatusError{AgentID: agent.ID, Route: route, Status: int(resp.GetStatus())}
		}
		return nil
	})
}
