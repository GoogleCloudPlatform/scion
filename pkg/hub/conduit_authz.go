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
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Conduit stream authorization (design v2.1 §3.5 "Authorization mapping").
// The kinds stay distinct; port visibility never implies shell:
//
//	pty, ssh     → ActionAttach (agent.attach)
//	tcp          → ActionPortAccess (agent.port_access) AND an authorized
//	               agent-local target {127.0.0.1, port}: an exposed port or
//	               an operator allow-listed one, never 9810/18380
//	logs, events → ActionRead (agent.read)
//
// ActionTunnel additionally gates requests that arrive through the
// `scion tunnel` / `scion ssh` path.

// conduitStreamAction returns the action a stream kind requires, or "" for
// an unknown kind (denied).
func conduitStreamAction(kind string) Action {
	switch kind {
	case grant.StreamKindPTY, grant.StreamKindSSH:
		return ActionAttach
	case grant.StreamKindTCP:
		return ActionPortAccess
	case grant.StreamKindLogs, grant.StreamKindEvents:
		return ActionRead
	default:
		return ""
	}
}

// conduitAuthzFor returns the authz action and registered permission that
// decide a Conduit action. ActionTunnel has no permission of its own and is
// decided as agent.port_access.
func conduitAuthzFor(action Action) (Action, string) {
	switch action {
	case ActionAttach:
		return ActionAttach, "agent.attach"
	case ActionPortAccess, ActionTunnel:
		return ActionPortAccess, "agent.port_access"
	case ActionRead:
		return ActionRead, "agent.read"
	default:
		return "", ""
	}
}

// authorizeConduitAction decides action for identity on agent. It returns a
// wrapped errConduitForbidden on denial; a decision that could not be
// evaluated also wraps errConduitAuthzUnavailable (admission still refuses
// it as forbidden; a stream re-check defers instead of closing).
func (s *Server) authorizeConduitAction(ctx context.Context, identity Identity, agent *store.Agent, action Action) error {
	if identity == nil || agent == nil {
		return fmt.Errorf("%w: missing identity or agent", errConduitForbidden)
	}
	authzAction, permission := conduitAuthzFor(action)
	if permission == "" {
		return fmt.Errorf("%w: no permission for action %q", errConduitForbidden, action)
	}
	if authzAction == ActionAttach {
		// The same decision every attach-gated route makes (PTY, keys).
		if denial := s.authorizeAgentTargetAction(ctx, identity, agent, ActionAttach); denial != nil {
			if denial.indeterminate {
				return fmt.Errorf("%w: %w: %s", errConduitForbidden, errConduitAuthzUnavailable, denial.reason)
			}
			return fmt.Errorf("%w: %s", errConduitForbidden, denial.reason)
		}
		return nil
	}
	switch ident := identity.(type) {
	case AgentIdentity:
		// As for the port and log routes: an agent never reaches another
		// project's agents, and port access is only to its own agent.
		if ident.ProjectID() == "" || ident.ProjectID() != agent.ProjectID {
			return fmt.Errorf("%w: agent project mismatch", errConduitForbidden)
		}
		if authzAction == ActionPortAccess && ident.ID() != agent.ID {
			return fmt.Errorf("%w: agents can only reach their own ports", errConduitForbidden)
		}
		switch authzAction {
		case ActionRead:
			if !ident.HasScope(ScopeProjectRead) {
				return fmt.Errorf("%w: missing scope %s", errConduitForbidden, ScopeProjectRead)
			}
			// The agent logs and events routes deny agent tokens too: no
			// agent token scope maps to agent.read in the permission
			// registry, so the kernel never grants it to an agent. Deny
			// here rather than relying on that, and revisit if a scope
			// ever grants agent.read.
			return fmt.Errorf("%w: agents cannot read agent streams", errConduitForbidden)
		case ActionPortAccess:
			return nil
		default:
			return fmt.Errorf("%w: no agent rule for action %q", errConduitForbidden, authzAction)
		}
	case UserIdentity:
	default:
		return fmt.Errorf("%w: identity may not open streams", errConduitForbidden)
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   agentResource(agent),
		Action:     authzAction,
		Permission: permission,
	})
	if !decision.Allowed {
		if decision.IsIndeterminate() {
			return fmt.Errorf("%w: %w: %s", errConduitForbidden, errConduitAuthzUnavailable, decision.Reason)
		}
		return fmt.Errorf("%w: %s", errConduitForbidden, decision.Reason)
	}
	return nil
}

// conduitStreamParams builds the params signed into a grant: the caller's
// params, restricted to the stream kind's allow-list, plus the params the hub
// sets itself. tcp allows exactly host and port from the caller; pty, ssh,
// logs and events allow none until their targets define params. For a broker
// target the hub binds the grant to the authorized agent with
// grant.ParamAgentID; a caller may repeat that value but not change it.
// Hub-set params are added here.
func conduitStreamParams(req conduitGrantRequest) (map[string]string, error) {
	var allowed []string
	if req.Stream.Kind == grant.StreamKindTCP {
		allowed = []string{grant.ParamHost, grant.ParamPort}
	}
	broker := req.Target.Kind == grant.TargetKindBroker
	params := make(map[string]string, len(req.Stream.Params)+1)
	for k, v := range req.Stream.Params {
		switch {
		case k == grant.ParamAgentID:
			if !broker || v != req.Agent.ID {
				return nil, fmt.Errorf("%w: param %q does not match the authorized target", errConduitInvalid, k)
			}
		case slices.Contains(allowed, k):
			params[k] = v
		default:
			return nil, fmt.Errorf("%w: param %q is not allowed for %s streams", errConduitInvalid, k, req.Stream.Kind)
		}
	}
	if broker {
		params[grant.ParamAgentID] = req.Agent.ID
	}
	if len(params) == 0 {
		return nil, nil
	}
	return params, nil
}

// conduitTCPTarget validates tcp stream params and returns the port. The
// params must be exactly {host: "127.0.0.1", port: <canonical decimal>},
// plus agent_id for a broker target.
func conduitTCPTarget(params map[string]string, broker bool) (int, error) {
	want := 2
	if broker {
		want = 3
		if params[grant.ParamAgentID] == "" {
			return 0, fmt.Errorf("%w: broker tcp target must name the agent", errConduitInvalid)
		}
	}
	if len(params) != want || params[grant.ParamHost] != "127.0.0.1" {
		return 0, fmt.Errorf("%w: tcp target must be exactly {host:127.0.0.1, port}", errConduitInvalid)
	}
	raw := params[grant.ParamPort]
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != raw {
		return 0, fmt.Errorf("%w: invalid tcp port %q", errConduitInvalid, raw)
	}
	return port, nil
}

// authorizeConduitTCPTarget checks that port is an authorized agent-local
// target: never a reserved port, and either exposed by the agent or on the
// operator allow-list.
func (s *Server) authorizeConduitTCPTarget(agent *store.Agent, port int) error {
	if reason := deniedExposedPorts[port]; reason != "" {
		return fmt.Errorf("%w: port %d is reserved for %s", errConduitForbidden, port, reason)
	}
	if findExposedPort(agent.ExposedPorts, port) != nil || slices.Contains(s.config.ConduitTCPAllowedPorts, port) {
		return nil
	}
	return fmt.Errorf("%w: port %d is not an authorized target", errConduitForbidden, port)
}

// conduitGrantRequest is the input to mintConduitGrant.
type conduitGrantRequest struct {
	// Identity is the authenticated principal asking for the stream.
	Identity Identity
	// Agent is the agent the stream reaches.
	Agent *store.Agent
	// Stream is the kind and params of the StreamOpen to be sent.
	Stream grant.StreamHeader
	// Target is the live session the stream will be opened on, from the
	// registry (kind, id, endpoint_incarnation, session_id,
	// connection_epoch).
	Target grant.Target
	// ViaTunnel marks a request from the `scion tunnel` / `scion ssh`
	// path, which also requires ActionTunnel.
	ViaTunnel bool
}

// mintConduitGrant authorizes req and, only if every check passes, mints a
// single-use grant for it. Errors wrap errConduitDisabled,
// errConduitInvalid or errConduitForbidden.
func (s *Server) mintConduitGrant(ctx context.Context, req conduitGrantRequest) ([]byte, *grant.Claims, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return nil, nil, errConduitDisabled
	}
	if req.Identity == nil || req.Agent == nil {
		return nil, nil, fmt.Errorf("%w: missing identity or agent", errConduitInvalid)
	}
	agent := req.Agent
	action := conduitStreamAction(req.Stream.Kind)
	if action == "" {
		return nil, nil, fmt.Errorf("%w: unknown stream kind %q", errConduitInvalid, req.Stream.Kind)
	}
	switch req.Target.Kind {
	case grant.TargetKindAgent:
		if req.Target.ID != agent.ID {
			return nil, nil, fmt.Errorf("%w: target is not this agent", errConduitInvalid)
		}
	case grant.TargetKindBroker:
		if agent.RuntimeBrokerID == "" || req.Target.ID != agent.RuntimeBrokerID {
			return nil, nil, fmt.Errorf("%w: target is not this agent's broker", errConduitInvalid)
		}
	default:
		return nil, nil, fmt.Errorf("%w: invalid target kind %q", errConduitInvalid, req.Target.Kind)
	}

	var subject string
	switch ident := req.Identity.(type) {
	case AgentIdentity:
		subject = "agent:" + ident.ID()
	case UserIdentity:
		subject = "user:" + ident.ID()
	default:
		return nil, nil, fmt.Errorf("%w: identity may not open streams", errConduitForbidden)
	}

	// Permission first, so a caller without it learns nothing about the
	// target (for example which ports are exposed).
	if err := s.authorizeConduitAction(ctx, req.Identity, agent, action); err != nil {
		return nil, nil, err
	}
	if req.ViaTunnel {
		if err := s.authorizeConduitAction(ctx, req.Identity, agent, ActionTunnel); err != nil {
			return nil, nil, err
		}
	}

	params, err := conduitStreamParams(req)
	if err != nil {
		return nil, nil, err
	}
	if req.Stream.Kind == grant.StreamKindTCP {
		port, err := conduitTCPTarget(params, req.Target.Kind == grant.TargetKindBroker)
		if err != nil {
			return nil, nil, err
		}
		if err := s.authorizeConduitTCPTarget(agent, port); err != nil {
			return nil, nil, err
		}
	}

	keys := s.conduitGrantKeySet()
	signer, err := keys.signer(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := keys.now().Truncate(time.Second)
	claims := grant.Claims{
		Issuer:    conduitGrantIssuer,
		Subject:   subject,
		ProjectID: agent.ProjectID,
		Target:    req.Target,
		Stream:    grant.StreamHeader{Kind: req.Stream.Kind, Params: params},
		NotBefore: now,
		Expiry:    now.Add(conduitGrantTTL),
	}
	jti, err := grant.NewJTI()
	if err != nil {
		return nil, nil, err
	}
	claims.JTI = jti
	tok, err := grant.Mint(signer, claims)
	if err != nil {
		return nil, nil, err
	}
	claims.KeyID = signer.KeyID
	claims.Audience = grant.Audience
	return tok, &claims, nil
}
