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

package relay

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
)

// Endpoint incarnation policy (design v2.4 §3.4). This file is the single
// place that decides which endpoint_incarnation a session is admitted with
// and which incarnation routing asks for, so admission and routing cannot
// drift. The hub's admission path (1d-ii) uses the same functions.

// CloseSupersededIncarnation (4409 superseded_incarnation) refuses a
// session whose Hello presents an agent launch id other than the agent
// row's current launch_id: the dialing container belongs to a superseded
// launch (a zombie). It is defined here, not in pkg/conduit, while the
// protocol core is in upstream review; the 1a dialer treats it like any
// unknown GoAway code, as a failure redialled with backoff.
const CloseSupersededIncarnation uint32 = 4409

// ReasonSupersededIncarnation is the close reason sent with 4409.
const ReasonSupersededIncarnation = "superseded_incarnation"

// Incarnation sources recorded in capabilities.incarnation_source of agent
// sessions (contracts §5). Broker and user sessions record "".
const (
	// IncarnationSourceLaunchID: the Hello presented the agent's launch id
	// and it matched agents.run_id. Fences zombie containers.
	IncarnationSourceLaunchID = "launch_id"
	// IncarnationSourceGeneration: the Hello presented no launch id (old
	// broker or sciontool); the hub assigned "gen-<agent.Generation>".
	// Fences `scion reincarnate` but not a zombie container of the same
	// generation (design v2.4 interim; T6 partition sub-case unmet).
	IncarnationSourceGeneration = "generation"
)

// generationPrefix marks hub-assigned interim incarnations. A launch id is
// never accepted if it carries this prefix, so the two namespaces cannot
// collide.
const generationPrefix = "gen-"

// AgentIncarnationFacts are the authoritative agent-row values the policy
// needs. LaunchID is the agent's current run id (store.Agent.RunID, which
// the broker also injects as SCION_LAUNCH_ID); Generation is
// store.Agent.Generation.
type AgentIncarnationFacts struct {
	LaunchID   string
	Generation int64
}

// Incarnation is a resolved endpoint incarnation and where it came from.
type Incarnation struct {
	Value  string
	Source string
}

// ReasonLegacyHelloSuperseded is the close reason sent with 4409 when
// CheckFallbackAgainstLaunchID refuses a Hello without a launch id.
const ReasonLegacyHelloSuperseded = "legacy_hello_superseded"

// ErrSupersededIncarnation is the 4409 refusal of a launch-id mismatch (a
// *conduit.CloseError).
var ErrSupersededIncarnation error = &conduit.CloseError{Code: CloseSupersededIncarnation, Reason: ReasonSupersededIncarnation}

// ErrLegacyHelloSuperseded is the 4409 refusal of a Hello without a launch
// id while a session admitted with the agent's current launch id exists.
var ErrLegacyHelloSuperseded error = &conduit.CloseError{Code: CloseSupersededIncarnation, Reason: ReasonLegacyHelloSuperseded}

func launchIncarnation(f AgentIncarnationFacts) (Incarnation, bool) {
	if f.LaunchID == "" || strings.HasPrefix(f.LaunchID, generationPrefix) {
		return Incarnation{}, false
	}
	return Incarnation{Value: f.LaunchID, Source: IncarnationSourceLaunchID}, true
}

func generationIncarnation(f AgentIncarnationFacts) Incarnation {
	return Incarnation{Value: generationPrefix + strconv.FormatInt(f.Generation, 10), Source: IncarnationSourceGeneration}
}

// AdmitAgentIncarnation decides the incarnation an agent session is
// admitted with, given the Hello's capabilities.endpoint_incarnation.
//
//   - presented non-empty: it is the container's launch id and must equal
//     agents.run_id exactly; otherwise ErrSupersededIncarnation (4409).
//     The agent row is never updated from the Hello. A row without a
//     launch id cannot vouch for any presented value, so that is refused
//     too.
//   - presented empty: interim fallback "gen-<Generation>", source
//     "generation". Admission must then also run
//     CheckFallbackAgainstLaunchID (it needs the registry), which refuses
//     the fallback while the current launch is connected.
func AdmitAgentIncarnation(f AgentIncarnationFacts, presented string) (Incarnation, error) {
	if presented == "" {
		return generationIncarnation(f), nil
	}
	want, ok := launchIncarnation(f)
	if !ok || presented != want.Value {
		return Incarnation{}, ErrSupersededIncarnation
	}
	return want, nil
}

// RouteAgentIncarnations lists the incarnations a router may ask for, in
// preference order: the current launch id (if the row has one), then the
// interim generation value. A session admitted with a superseded launch id
// can match neither (admission refuses it anyway); a session admitted via
// the fallback matches the second, which is the documented interim gap.
func RouteAgentIncarnations(f AgentIncarnationFacts) []Incarnation {
	gen := generationIncarnation(f)
	if li, ok := launchIncarnation(f); ok {
		return []Incarnation{li, gen}
	}
	return []Incarnation{gen}
}

// AdmitBrokerIncarnation decides a broker session's incarnation. With an
// authoritative value the Hello must match it (an empty Hello value takes
// it); without one the Hello's process start id is required and used.
func AdmitBrokerIncarnation(authoritative, presented string) (Incarnation, error) {
	switch {
	case authoritative != "" && (presented == "" || presented == authoritative):
		return Incarnation{Value: authoritative}, nil
	case authoritative != "":
		return Incarnation{}, ErrSupersededIncarnation
	case presented == "":
		return Incarnation{}, reject(conduit.CloseProtocolError, ReasonBadHello, "broker Hello must carry capabilities.endpoint_incarnation")
	default:
		return Incarnation{Value: presented}, nil
	}
}

// RouteBrokerIncarnations is the routing counterpart of
// AdmitBrokerIncarnation: brokers route on the incarnation the caller
// knows (the authoritative value, or the one recorded by the broker
// registry).
func RouteBrokerIncarnations(known string) []Incarnation {
	if known == "" {
		return nil
	}
	return []Incarnation{{Value: known}}
}

// admitIncarnation applies the policy for p's kind. Users carry no
// incarnation.
func admitIncarnation(p Principal, presented string) (Incarnation, error) {
	switch p.Kind {
	case registry.PrincipalAgent:
		return AdmitAgentIncarnation(p.Agent, presented)
	case registry.PrincipalBroker:
		return AdmitBrokerIncarnation(p.Incarnation, presented)
	case registry.PrincipalUser:
		if presented != "" {
			return Incarnation{}, reject(conduit.CloseProtocolError, ReasonBadHello, "user sessions carry no endpoint incarnation")
		}
		return Incarnation{}, nil
	default:
		return Incarnation{}, reject(conduit.CloseForbidden, ReasonForbidden, fmt.Sprintf("principal kind %q may not open a session", p.Kind))
	}
}

// IsSupersededIncarnation reports whether err is a 4409 refusal (either
// reason; the check is code-based).
func IsSupersededIncarnation(err error) bool {
	var ce *conduit.CloseError
	return errors.As(err, &ce) && ce.Code == CloseSupersededIncarnation
}

// CheckFallbackAgainstLaunchID enforces the mixed-version fence on an
// agent Hello that presented no launch id (inc.Source ==
// IncarnationSourceGeneration). While the agent holds ANY non-draining
// session row admitted with its CURRENT launch_id (source launch_id),
// whatever that row's relay liveness or epoch currency, a launch-id-less
// Hello can only come from a container that predates launch ids for this
// agent (a superseded launch, i.e. a zombie): it is refused with
// ErrLegacyHelloSuperseded (4409 legacy_hello_superseded). Liveness and
// epoch currency are deliberately ignored: a launch session that is still
// connected but briefly ineligible (its relay missed heartbeats, or a
// racing insert made it epoch-obsolete) gets no signal to reconnect, so a
// fallback admitted in that gap would keep routing indefinitely. Dead rows
// are bounded by the reapers (sessions on a stale relay go with the relay,
// stale sessions after registry.DefaultSessionReapAfter).
//
// Admission calls it twice: before InsertSessionWithNextEpoch with
// ownSessionID "" (a refusal writes no row and burns no epoch), and again
// after inserting a fallback row with that row's id, closing the
// check-then-insert race with a concurrent launch-id Hello (see
// admitter.recheckFallback). A store read error is returned as is
// (callers fail closed with 4504).
//
// Remaining interim window (until every target presents a launch id, 1e):
// a launch-id-less Hello that arrives while the agent holds no
// non-draining launch_id row (the current launch has not connected yet, is
// between sessions, or is draining) is admitted as gen-N and owns routing
// until the current launch connects with its launch id; that Hello passes
// AdmitAgentIncarnation, bumps the epoch and fences the fallback session.
func CheckFallbackAgainstLaunchID(ctx context.Context, store registry.Store, agentID string, f AgentIncarnationFacts, inc Incarnation, ownSessionID string) error {
	if inc.Source != IncarnationSourceGeneration {
		return nil
	}
	cur, ok := launchIncarnation(f)
	if !ok {
		return nil // pure legacy: no launch id to protect
	}
	ps, err := store.ListPrincipalSessions(ctx, registry.PrincipalAgent, agentID)
	if err != nil {
		return err
	}
	for _, v := range ps.Sessions {
		if v.Session.SessionID != ownSessionID && isLaunchRow(v.Session, cur.Value) {
			return ErrLegacyHelloSuperseded
		}
	}
	return nil
}

// isLaunchRow reports whether s is a non-draining row admitted with the
// launch id launchID.
func isLaunchRow(s registry.SessionRecord, launchID string) bool {
	return !s.Draining && s.Capabilities.IncarnationSource == IncarnationSourceLaunchID && s.EndpointIncarnation == launchID
}
