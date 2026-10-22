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
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
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

// conduitViaActions returns the actions a user stream of kind requires when
// its origin is via, for the open and every re-check (design v2.21.29 §3.5,
// Origin claim via):
//
//	tunnel + tcp → ActionPortAccess, ActionTunnel
//	tunnel + ssh → ActionAttach, ActionTunnel
//	tunnel + pty → ActionAttach, ActionTunnel
//	proxy  + tcp → ActionPortAccess
//	pty    + pty → ActionAttach
//
// The pairs are grant.ViaAllows, the one table shared with targets; the
// actions are conduitStreamActions for the kind, with ActionTunnel for the
// tunnel origin. It returns nil (no renewal) for any other pair.
func conduitViaActions(via, kind string) []Action {
	if !grant.ViaAllows(via, kind) {
		return nil
	}
	return conduitStreamActions(kind, via == grant.ViaTunnel)
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
// sets itself. tcp allows exactly host and port from the caller; pty allows
// exactly cols, rows and session; ssh, logs and events allow none until
// their targets define params. For a broker target the hub binds the grant
// to the authorized agent with grant.ParamAgentID; a caller may repeat that
// value but not change it.
// Hub-set params are added here.
func conduitStreamParams(req conduitGrantRequest) (map[string]string, error) {
	var allowed []string
	switch req.Stream.Kind {
	case grant.StreamKindTCP:
		allowed = []string{grant.ParamHost, grant.ParamPort}
	case grant.StreamKindPTY:
		allowed = []string{grant.ParamCols, grant.ParamRows, grant.ParamSession}
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

// conduitPTYSession is the only tmux session a pty grant may name.
const conduitPTYSession = "scion"

// conduitPTYMaxDim is the largest cols or rows a pty grant may carry.
const conduitPTYMaxDim = 4096

// conduitPTYTarget validates pty stream params: exactly {cols, rows,
// session}, cols and rows canonical base-10 integers in 1..4096, session
// "scion" (contracts §2).
func conduitPTYTarget(params map[string]string) error {
	if len(params) != 3 || params[grant.ParamSession] != conduitPTYSession {
		return fmt.Errorf("%w: pty target must be exactly {cols, rows, session:%s}", errConduitInvalid, conduitPTYSession)
	}
	for _, k := range []string{grant.ParamCols, grant.ParamRows} {
		raw, ok := params[k]
		n, err := strconv.Atoi(raw)
		if !ok || err != nil || n < 1 || n > conduitPTYMaxDim || strconv.Itoa(n) != raw {
			return fmt.Errorf("%w: invalid pty %s %q", errConduitInvalid, k, raw)
		}
	}
	return nil
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
	// Via is the origin that admitted the stream (grant.ViaTunnel,
	// grant.ViaProxy or grant.ViaPTY). The caller sets it from the route
	// it serves, never from a request or relay field. The open is
	// authorized for the action set of (Via, Stream.Kind). Empty means no
	// origin (agent callers and library uses such as broker targets): the
	// kind's action alone, and the grant is never stamped.
	Via string
	// TargetRenews reports that the target session row the grant binds to
	// advertises conduit.FeatureStreamRenewal (conduitTargetRenews).
	TargetRenews bool
	// CanRenew reports that this node can deliver renewals for the stream
	// on the resolved route.
	CanRenew bool
	// Route is the metric and log route label of the open
	// (conduitRouteRelayTunnel for a relay-held tunnel; contracts
	// 1bbe35f0 §2, Renewal path unavailable, Message and observability).
	// Empty means conduitRouteOf(Via). It is hub-internal: only the hub's
	// own caller sets it, never from a request or relay field; it is never
	// in the grant, an instruction or any wire field, and never an input to
	// authorization or the re-check (Via is).
	Route string
}

// route is req's metric and log route label: Route, or the label of Via.
func (req conduitGrantRequest) route() string {
	if req.Route != "" {
		return req.Route
	}
	return conduitRouteOf(req.Via)
}

// mintConduitGrant authorizes req and, only if every check passes, mints a
// single-use grant for it. Errors wrap errConduitDisabled,
// errConduitInvalid, errConduitForbidden,
// errConduitStreamRenewalUnavailable (503 stream_renewal_unavailable),
// errConduitAgentUpgradeRequired (409 agent_upgrade_required) or
// errConduitGrantInternal (500 internal_error, nothing minted).
func (s *Server) mintConduitGrant(ctx context.Context, req conduitGrantRequest) ([]byte, *grant.Claims, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return nil, nil, errConduitDisabled
	}
	if req.Identity == nil || req.Agent == nil {
		return nil, nil, fmt.Errorf("%w: missing identity or agent", errConduitInvalid)
	}
	agent := req.Agent
	if conduitStreamAction(req.Stream.Kind) == "" {
		return nil, nil, fmt.Errorf("%w: unknown stream kind %q", errConduitInvalid, req.Stream.Kind)
	}
	var actions []Action
	if req.Via == "" {
		// No origin: agent callers and library uses (broker targets,
		// logs, events). Authorized as before; never stamped (below).
		actions = conduitStreamActions(req.Stream.Kind, false)
	} else {
		actions = conduitViaActions(req.Via, req.Stream.Kind)
	}
	if actions == nil {
		return nil, nil, fmt.Errorf("%w: origin %q does not carry kind %q", errConduitInvalid, req.Via, req.Stream.Kind)
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
	authorize := s.authorizeConduitAction
	if hook := s.conduitAuthorizeActionTestHook; hook != nil {
		authorize = hook
	}
	for _, a := range actions {
		if err := authorize(ctx, req.Identity, agent, a); err != nil {
			return nil, nil, err
		}
	}
	stamp := conduitStampsDeadline(req.Identity, req.TargetRenews)
	if stamp && req.Via == "" {
		return nil, nil, fmt.Errorf("%w: a stream to a renewal target needs an origin", errConduitInvalid)
	}
	// Renewal path (contracts v2.21.36, Renewal path unavailable): after
	// authorization, before the placement guard and the mint. The route
	// decides this at resolution; this is the backstop for a re-resolution
	// onto another row after that check. With enforce it also refuses a
	// renewal-unsupported principal.
	// Target without stream renewal (contracts, Target lacks stream
	// renewal): the backstop of the open paths' check on the resolved row,
	// at the same point, so it precedes the placement guard. Only user
	// stream origins (tunnel, proxy, pty) are concerned.
	if req.Via != "" && s.conduitTargetUpgradeRequired(ctx, req.Identity, req.TargetRenews, req.route(), agent.ID) {
		return nil, nil, errConduitAgentUpgradeRequired
	}
	if s.conduitRenewalPathUnavailable(ctx, req.Identity, req.TargetRenews, req.CanRenew, req.route(), agent.ID) {
		return nil, nil, errConduitStreamRenewalUnavailable
	}
	// Design v2.21.36 §3.5, Principals without a renewal path: with off, a
	// renewal-unsupported principal's grant is minted unstamped, as for a
	// target without stream_renewal.v2, and the leaf-holding hub node
	// re-checks the stream in-process. The mint is logged and counted.
	unstampedKind := ""
	if stamp {
		if kind, unsupported := conduitRenewalUnsupportedPrincipal(req.Identity); unsupported {
			stamp, unstampedKind = false, kind
		}
	}
	// Flat placement guard (requirePinnedPlacement), after the permission
	// checks so a caller without permission learns nothing about the
	// agent's placement. Every grant reaching an agent is refused for a
	// pinned agent whose saved placement is inconsistent, the agent-port
	// proxy, the TCP tunnel and the agent PTY path alike, for user and
	// agent callers: fail closed on conduit access, as resolvePTYPath does
	// for attach. The refusal is a *PlacementConflictError (409), never an
	// authorization result. callAgentControl mints no grant and is not
	// covered: it only queues a credential refresh on the agent's own
	// session (Hub-internal credential upkeep).
	// A managed runtime reaches no Runtime Broker and is never pinned, so
	// it is not checked (as in resolvePTYPath).
	if !isManagedAgentRuntime(agent.Runtime) {
		if _, err := checkAgentPinnedPlacement(ctx, s.store, agent); err != nil {
			if _, refused := placementRefusalOf(err); refused {
				return nil, nil, err
			}
			// The check itself could not read the agent's Runtime Broker.
			// For a pinned agent that is a temporary failure, answered as
			// routing unavailable (503 registry_unavailable), not as a
			// grant-key fault. An unpinned agent keeps today's path.
			if agent.IsPinned() {
				return nil, nil, fmt.Errorf("%w: agent placement check: %w", router.ErrRegistryUnavailable, err)
			}
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
	if req.Stream.Kind == grant.StreamKindPTY {
		if err := conduitPTYTarget(params); err != nil {
			return nil, nil, err
		}
	}
	var scope *grant.Scope
	if stamp {
		// The scope claim is the design §3.5 credential context that the
		// renewal re-check rebuilds the identity from
		// (conduitScopedIdentity). A scoped stream's grant is never minted
		// without it.
		sc, err := s.conduitScopeOf(req.Identity)
		if err != nil {
			slog.ErrorContext(ctx, "Conduit open grant not minted: stream scope cannot be determined",
				"agent_id", agent.ID, "kind", req.Stream.Kind, "via", req.Via)
			return nil, nil, fmt.Errorf("%w: %w", errConduitGrantInternal, err)
		}
		scope = sc
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
	if stamp {
		// Design §3.5 Open grant: a user stream to a stream_renewal.v2
		// target carries its origin, scope and authorization deadline.
		iv := s.conduitUserStreamAuthzInterval().Truncate(time.Second)
		claims.Via, claims.Scope = req.Via, scope
		claims.AuthzUntil, claims.AuthzInterval = now.Add(iv), iv
	}
	jti, err := grant.NewJTI()
	if err != nil {
		return nil, nil, err
	}
	claims.JTI = jti
	tok, err := grant.Mint(signer, claims)
	if err != nil {
		if errors.Is(err, grant.ErrTooLarge) {
			slog.ErrorContext(ctx, "Conduit open grant exceeds the grant size limit; not minted",
				"agent_id", agent.ID, "kind", req.Stream.Kind, "via", req.Via)
			return nil, nil, fmt.Errorf("%w: %w", errConduitGrantInternal, err)
		}
		return nil, nil, err
	}
	claims.KeyID = signer.KeyID
	claims.Audience = grant.Audience
	if unstampedKind != "" {
		slog.InfoContext(ctx, "Conduit open grant minted without a deadline: the principal has no renewal path",
			"agent_id", agent.ID, "kind", req.Stream.Kind, "via", req.Via, "principal_kind", unstampedKind)
		s.recordConduitOpenUnstamped(req.route(), unstampedKind)
	}
	return tok, &claims, nil
}

// A renewal the hub declines for any reason other than an authorization
// deny wraps errConduitRenewalUnavailable (conduit_renewal.go): the
// presented grant does not verify in re-check mode, does not match the
// stream named by the request, or names an origin and kind outside the
// origin table, or the agent's saved placement refuses it
// (conduitStreamPlacement). Nothing is minted and nothing closes; the
// stream keeps its current deadline. An authorization deny wraps
// errConduitForbidden.

// conduitRenewalRequest is what a relay presents to renew one user stream's
// authorization deadline (design §3.5, Re-check). Only LatestGrant carries
// authority. The other fields name the stream and must agree with it.
type conduitRenewalRequest struct {
	// LatestGrant is the stream's most recent hub-signed grant: its open
	// grant or any renewal for the same open_jti.
	LatestGrant []byte
	// TargetSessionID and TargetStreamID name the stream on the target's
	// own session, as the target knows it.
	TargetSessionID string
	TargetStreamID  uint32
	// OpenJTI is the jti of the stream's open grant.
	OpenJTI string
	// PrincipalID and Kind, when set, are the user and stream kind the
	// holder of the stream's leaf has for it; they must equal the grant's.
	PrincipalID string
	Kind        string
	// RelayHeld marks a request a separate relay presented for a stream on
	// one of its user sessions (recheckRelayStream). PrincipalID is then
	// that session's admitted principal, and a grant naming another
	// principal is a session binding failure: a deny
	// (errConduitRenewalSessionBinding). On a hub-held stream the hub built
	// the request from its own state, so a mismatch is an internal
	// inconsistency: unavailable, logged at error level (design v2.21.40
	// §3.9, Session binding).
	RelayHeld bool
}

// conduitRenewal is a minted renewal grant and the deadline it carries.
type conduitRenewal struct {
	Token      []byte
	AuthzUntil time.Time
	Claims     *grant.RenewalClaims
}

// renewConduitStream re-checks a user stream from its latest grant alone
// and, if the principal still holds the stream's authorization, mints a
// renewal grant moving its deadline to now plus the user stream
// authorization interval. The hub keeps no per-stream state: the target's
// identity comes from the registry, and the principal, project, kind,
// params and origin from the grant's signed claims.
//
// Errors wrap errConduitDisabled, errConduitRenewalUnavailable (bad or
// mismatched grant, no target session, origin and kind outside the table,
// or a pinned agent whose saved placement is inconsistent; see
// conduitStreamPlacement), errConduitForbidden (authorization denied,
// including an inactive user), errConduitAuthzUnavailable (a lookup
// failed: no renewal, and the stream is not closed early), or
// errConduitAgentGone.
func (s *Server) renewConduitStream(ctx context.Context, req conduitRenewalRequest) (*conduitRenewal, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return nil, errConduitDisabled
	}
	if len(req.LatestGrant) == 0 || req.TargetSessionID == "" || req.TargetStreamID == 0 || req.OpenJTI == "" {
		return nil, fmt.Errorf("%w: incomplete request", errConduitRenewalUnavailable)
	}
	target, err := s.conduitRenewalTarget(ctx, req.TargetSessionID)
	if err != nil {
		return nil, err
	}
	keys := s.conduitGrantKeySet()
	recheck, gen, err := keys.recheckKeysGen(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: grant keys: %v", errConduitAuthzUnavailable, err)
	}
	now := keys.now()
	expect := grant.RecheckExpectation{Issuer: conduitGrantIssuer, Target: target, OpenJTI: req.OpenJTI, StreamID: req.TargetStreamID}
	c, err := grant.VerifyRecheck(req.LatestGrant, recheck, expect, now)
	if errors.Is(err, grant.ErrUnknownKey) {
		// Design v2.21.36 §3.5: an unknown kid gets one ring reload before
		// it is a deny, so a node whose cached ring lags a rotation does not
		// refuse a valid grant. A failed reload is unavailable, never a deny.
		// Renewals that met the same kid share one reload (reloadRecheckKeys).
		if recheck, err = keys.reloadRecheckKeys(ctx, gen); err != nil {
			return nil, fmt.Errorf("%w: grant keys reload: %v", errConduitAuthzUnavailable, err)
		}
		now = keys.now()
		c, err = grant.VerifyRecheck(req.LatestGrant, recheck, expect, now)
	}
	if err != nil {
		// %w for both, so ClassifyConduitRenewal sees the verification result.
		return nil, fmt.Errorf("%w: %w", errConduitRenewalUnavailable, err)
	}
	userID, ok := strings.CutPrefix(c.Subject, grant.SubjectUserPrefix)
	if !ok || userID == "" {
		return nil, fmt.Errorf("%w: not a user stream", errConduitRenewalUnavailable)
	}
	if req.PrincipalID != "" && req.PrincipalID != userID {
		if req.RelayHeld {
			return nil, fmt.Errorf("%w: the grant names another principal than the stream's session", errConduitRenewalSessionBinding)
		}
		slog.Error("Conduit renewal: the grant names another principal than the stream the hub holds; no renewal minted",
			"target_session_id", req.TargetSessionID, "stream_id", req.TargetStreamID)
		return nil, fmt.Errorf("%w: principal does not match the grant", errConduitRenewalUnavailable)
	}
	if req.Kind != "" && req.Kind != c.Stream.Kind {
		return nil, fmt.Errorf("%w: kind does not match the grant", errConduitRenewalUnavailable)
	}
	if conduitViaActions(c.Via, c.Stream.Kind) == nil {
		return nil, fmt.Errorf("%w: origin %q does not carry kind %q", errConduitRenewalUnavailable, c.Via, c.Stream.Kind)
	}
	var port int
	if c.Stream.Kind == grant.StreamKindTCP {
		if port, err = conduitTCPTarget(c.Stream.Params, false); err != nil {
			return nil, fmt.Errorf("%w: %v", errConduitRenewalUnavailable, err)
		}
	}

	if verdict, _, err := s.evaluateConduitStream(ctx, conduitStreamEval{
		AgentID:     target.ID,
		ProjectID:   c.ProjectID,
		Identity:    func() (Identity, error) { return s.conduitRenewalIdentity(ctx, c, userID) },
		Kind:        c.Stream.Kind,
		ViaTunnel:   c.Via == grant.ViaTunnel,
		Port:        port,
		ConfirmDeny: func(agent *store.Agent) bool { return s.conduitConfirmAgentPresent(ctx, agent.ID) },
		LogAttrs:    []any{"target_session_id", req.TargetSessionID, "stream_id", req.TargetStreamID},
	}); verdict != conduitAuthzAllowed {
		return nil, err
	}

	signer, err := keys.signer(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: grant keys: %v", errConduitAuthzUnavailable, err)
	}
	mintAt := now.Truncate(time.Second)
	rc := grant.RenewalClaims{
		Issuer:     conduitGrantIssuer,
		Subject:    c.Subject,
		ProjectID:  c.ProjectID,
		Target:     target,
		Stream:     c.Stream,
		Via:        c.Via,
		Scope:      c.Scope,
		StreamID:   req.TargetStreamID,
		OpenJTI:    req.OpenJTI,
		AuthzUntil: mintAt.Add(s.conduitUserStreamAuthzInterval()),
		NotBefore:  mintAt,
		Expiry:     mintAt.Add(conduitGrantTTL),
	}
	jti, err := grant.NewJTI()
	if err != nil {
		return nil, fmt.Errorf("%w: renewal jti: %w", errConduitAuthzUnavailable, err)
	}
	rc.JTI = jti
	tok, err := grant.MintRenewal(signer, rc)
	if err != nil {
		if errors.Is(err, grant.ErrTooLarge) {
			// The stream gets no renewal and ends at its deadline.
			slog.Error("Conduit renewal grant exceeds the grant size limit; no renewal minted",
				"target_session_id", req.TargetSessionID, "stream_id", req.TargetStreamID, "error", err)
		}
		return nil, fmt.Errorf("%w: mint renewal: %w", errConduitAuthzUnavailable, err)
	}
	rc.KeyID, rc.Audience = signer.KeyID, grant.Audience
	return &conduitRenewal{Token: tok, AuthzUntil: rc.AuthzUntil, Claims: &rc}, nil
}

// conduitRenewalIdentity returns the identity a renewal re-check evaluates
// for the user named by a verified grant (design v2.21.31 §3.5, Scope claim
// scope): the user, read from the store, and for a grant with a scope the
// scoped identity rebuilt from it (conduitScopedIdentity), so the re-check
// runs the same scoped-token check as a request with that token. A user
// row that is gone wraps errConduitPrincipalInactive; a failed lookup wraps
// errConduitAuthzUnavailable.
func (s *Server) conduitRenewalIdentity(ctx context.Context, c *grant.RecheckClaims, userID string) (Identity, error) {
	// The development user (design v2.21.40 §3.5, Dev user) is rebuilt as
	// such only while development authentication is enabled on this hub,
	// read now and never from the grant. With it off, the user id takes the
	// normal path, and a missing row is an inactive principal. Either way
	// the account-status rule (checkUserAccount) applies to the user row.
	devUser := userID == DevUserID && s.authConfig.DevAuthEnabled
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: userID}
	if devUser {
		principal = devUserPrincipal()
	}
	user, err := checkUserAccount(ctx, s.store, principal)
	switch refusal := classifyUserAccount(user, err); refusal {
	case accountAllowed:
	case accountLookupFault:
		return nil, fmt.Errorf("%w: user lookup: %v", errConduitAuthzUnavailable, err)
	default:
		return nil, fmt.Errorf("%w: %s", errConduitPrincipalInactive, accountRefusalReason(refusal))
	}
	var ident UserIdentity
	if devUser {
		ident = NewDevUser(s.authConfig.DevUserCfg)
	} else {
		ident = NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "")
	}
	if c.Scope == nil {
		return ident, nil
	}
	return conduitScopedIdentity(ident, c.Scope), nil
}

// errConduitScope marks an identity whose scope claim cannot be decided:
// one carrying part of a scoped token's credential context, or whose
// context does not form a valid scope, or an identity kind with no scope
// rule. The open mint refuses it rather than minting a grant without scope.
var errConduitScope = errors.New("conduit: stream scope cannot be determined")

// conduitScopedCredential is the credential context of a scoped token as
// the scoped-token check consumes it: ScopedUserIdentity's Boundary,
// Ceiling and ScopedScopes. An identity exposing any of them is treated as
// scoped.
type conduitScopedCredential interface {
	Boundary() TokenBoundary
	Ceiling() permissions.FrozenPermissionCeiling
	ScopedScopes() []string
}

// conduitScopeFor returns the scope claim of identity (design v2.21.31
// §3.5). An identity with a scoped token's credential context gets the
// token's scopes, boundary and ceiling, both lists sorted and deduplicated
// and the ceiling copied as stored. Only a plain user identity
// (AuthenticatedUser, DevUser) gets nil, meaning no scope. Anything else,
// including an identity exposing only part of the context, a scoped
// identity that is not a user, or a context that is not a valid scope,
// fails with errConduitScope.
func conduitScopeFor(identity Identity) (*grant.Scope, error) {
	if isNilIdentity(identity) {
		return nil, fmt.Errorf("%w: no identity", errConduitScope)
	}
	_, hasBoundary := identity.(interface{ Boundary() TokenBoundary })
	_, hasCeiling := identity.(interface {
		Ceiling() permissions.FrozenPermissionCeiling
	})
	_, hasScopes := identity.(interface{ ScopedScopes() []string })
	if hasBoundary || hasCeiling || hasScopes {
		cred, complete := identity.(conduitScopedCredential)
		if !complete {
			return nil, fmt.Errorf("%w: incomplete scoped credential context", errConduitScope)
		}
		if _, user := identity.(UserIdentity); !user {
			return nil, fmt.Errorf("%w: scoped identity is not a user", errConduitScope)
		}
		b, c := cred.Boundary(), cred.Ceiling()
		sc := grant.NewScope(cred.ScopedScopes(), grant.ScopeBoundary{Kind: string(b.Kind), ProjectID: b.ProjectID}, int(c.Version), c.PermissionIDs)
		if err := sc.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", errConduitScope, err)
		}
		return sc, nil
	}
	switch identity.(type) {
	// New plain-user identity types must be added here deliberately; any
	// type not listed fails closed.
	case *AuthenticatedUser, *DevUser:
		return nil, nil
	}
	return nil, fmt.Errorf("%w: no scope rule for %T", errConduitScope, identity)
}

// conduitRenewalUnsupportedPrincipal decides, by principal class, whether
// identity is a renewal-unsupported principal (design v2.21.36 §3.5,
// Principals without a renewal path): a user principal the authz layer may
// admit for a stream action, but that the stateless renewal re-check
// cannot rebuild from a grant's signed claims, which name a local user id
// and the scope. The re-check rebuilds a local user (GetUser plus
// NewAuthenticatedUser) and, from scope, a scoped identity over it with
// the scoped-identity constructor (conduitScopedIdentity). So:
//   - a federated user (PrincipalKindFederatedUser): its id is
//     issuer:subject, with no local user row;
//   - a visitor (PrincipalKindVisitor): it would be rebuilt as an
//     interactive local user, not a visitor;
//   - a scoped identity over either of them.
//
// A local user (PrincipalKindUser, PrincipalKindDev) and a scoped identity
// over one are supported: every production scoped identity wraps
// NewAuthenticatedUser built from the local user row (ValidateToken,
// scheduled authority, relay admission), which the scoped constructor
// rebuilds faithfully. Any other class is not a member; conduitScopeFor's
// fail-closed path decides it. kind is the principal kind for logs and
// metrics ("scoped_" prefixed for a scoped identity).
func conduitRenewalUnsupportedPrincipal(identity Identity) (kind string, unsupported bool) {
	if isNilIdentity(identity) {
		return "", false
	}
	inner, prefix := identity, ""
	if s, ok := identity.(*ScopedUserIdentity); ok {
		if s == nil || isNilIdentity(s.UserIdentity) {
			return "", false
		}
		inner, prefix = s.UserIdentity, "scoped_"
	}
	switch k := principalContextForIdentity(inner).Kind; k {
	case PrincipalKindFederatedUser, PrincipalKindVisitor:
		return prefix + string(k), true
	}
	return "", false
}

// conduitScopedIdentity rebuilds the scoped identity of user from a
// verified scope claim, with the constructor for scoped identities outside
// a request: the boundary kind and project, the scopes and the ceiling are
// taken as they are, so a hub boundary stays a hub boundary.
func conduitScopedIdentity(user UserIdentity, sc *grant.Scope) Identity {
	return NewScopedUserIdentityWithBoundary(user,
		TokenBoundary{Kind: BoundaryKind(sc.Boundary.Kind), ProjectID: sc.Boundary.ProjectID},
		slices.Clone(sc.Scopes), "",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersion(sc.Ceiling.Version), PermissionIDs: slices.Clone(sc.Ceiling.PermissionIDs)})
}

// conduitUserStreamAuthzInterval is the user stream authorization interval,
// with the 8h default.
func (s *Server) conduitUserStreamAuthzInterval() time.Duration {
	if d := s.config.ConduitUserStreamAuthzMax; d > 0 {
		return d
	}
	return defaultConduitUserStreamAuthzMax
}

// conduitRenewalTarget returns the target identity of the live session
// sessionID as the registry records it. Only agent sessions are renewal
// targets: user streams to brokers are not renewed until brokers move to
// conduit, and user sessions are never targets.
func (s *Server) conduitRenewalTarget(ctx context.Context, sessionID string) (grant.Target, error) {
	// The registry of whichever conduit runtime runs on this node: the
	// in-process one, or the shared registry of external relay mode.
	_, st, _ := s.conduitRegistry()
	if st == nil {
		return grant.Target{}, fmt.Errorf("%w: no conduit registry on this node", errConduitAuthzUnavailable)
	}
	ps, found, err := st.ListPrincipalSessionsBySession(ctx, sessionID)
	if err != nil {
		return grant.Target{}, fmt.Errorf("%w: registry: %v", errConduitAuthzUnavailable, err)
	}
	if found {
		for _, v := range ps.Sessions {
			r := v.Session
			if r.SessionID != sessionID {
				continue
			}
			if r.PrincipalKind != registry.PrincipalAgent {
				return grant.Target{}, fmt.Errorf("%w: session is not an agent target", errConduitRenewalUnavailable)
			}
			return grant.Target{
				Kind: grant.TargetKindAgent, ID: r.PrincipalID, EndpointIncarnation: r.EndpointIncarnation,
				SessionID: r.SessionID, ConnectionEpoch: r.ConnectionEpoch,
			}, nil
		}
	}
	return grant.Target{}, fmt.Errorf("%w: target session not found", errConduitRenewalUnavailable)
}
