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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Outcomes of an agent AuthRefresh (the outcome attribute of
// scion.hub.conduit.agent_refresh).
const (
	// conduitRefreshOK: the refreshed credential passed every check.
	conduitRefreshOK = "ok"
	// conduitRefreshRefused: a check refused it; the session is closed
	// with 4401.
	conduitRefreshRefused = "refused"
	// conduitRefreshUnevaluated: a store read failed, so a check could
	// not be evaluated; the session keeps its admitted identity.
	conduitRefreshUnevaluated = "unevaluated"
)

// conduitRefreshRoute is the path recorded for an AuthRefresh in the
// run-scope log.
const conduitRefreshRoute = "/api/v1/conduit"

// conduitAgentRefresh is the relay's RefreshFunc. It validates the
// credential an agent presents in an AuthRefresh on its conduit session
// (design §3.3, §7 Phase 4) with the checks admission applies to the
// token that opened the session:
//   - the token is a valid agent token, and passes the credential, hold
//     and token-run checks of every agent request
//     (verifyAgentTokenCredential);
//   - it names the session's agent and project;
//   - it carries agent:port:forward, and the agent row still exists, is
//     not deleted, and is in the token's project
//     (conduitAgentAdmissionCheck);
//   - its run matches the session's endpoint incarnation (§3.11), refused
//     only when run-scope enforcement is on, as at admission.
//
// A refusal closes the session with 4401. When a check cannot be
// evaluated because a store read failed, the session stays open on its
// admitted identity and the agent's next refresh is checked again: a
// check that cannot be evaluated neither renews nor closes (§3.5). It
// never touches a stream's authorization: an agent's AuthRefresh is about
// the agent's own identity only.
//
// Sessions of other principals (brokers, users) do not refresh in band;
// their AuthRefresh is refused with 4401, as before agent refresh existed.
func (s *Server) conduitAgentRefresh(ctx context.Context, p relay.Principal, sess relay.RefreshSession, ar *conduitv1.AuthRefresh) error {
	log := slog.Default().With("subsystem", "hub.conduit",
		"principal_kind", p.Kind, "principal_id", p.ID, "session_id", sess.SessionID)
	refuse := func(reason string) error {
		log.Warn("Conduit agent credential refresh refused", "reason", reason)
		s.recordConduitAgentRefresh(conduitRefreshRefused)
		return conduit.Reject(conduit.CloseUnauthenticated, relay.ReasonUnauthenticated)
	}
	unevaluated := func(err error) error {
		log.Warn("Conduit agent credential refresh could not be checked; keeping the session",
			"outcome", conduitRefreshUnevaluated, "error", err)
		s.recordConduitAgentRefresh(conduitRefreshUnevaluated)
		return nil
	}

	if p.Kind != registry.PrincipalAgent {
		return refuse("not_supported")
	}
	svc := s.authConfig.AgentTokenSvc
	if svc == nil {
		return refuse("no_token_service")
	}
	claims, err := svc.ValidateAgentToken(string(ar.GetCredential()))
	if err != nil {
		return refuse("invalid_token")
	}
	if claims.Subject != p.ID || claims.ProjectID != p.ProjectID {
		return refuse("principal_mismatch")
	}

	req := runScopeRequest{
		method:    "AuthRefresh",
		path:      conduitRefreshRoute,
		classPath: conduitRefreshRoute,
		route:     conduitRefreshRoute,
		hello:     sess.EndpointIncarnation,
	}
	authLog := s.authConfig.Logger
	if authLog == nil {
		authLog = slog.Default()
	}
	switch _, refusal := verifyAgentTokenCredential(ctx, s.authConfig, authLog, claims, req, runScopeSourceConduit); refusal {
	case agentTokenAccepted:
	case agentTokenUnevaluated:
		return unevaluated(errors.New("credential status, hold or token run unavailable"))
	default:
		return refuse("credential")
	}

	if _, err := s.conduitAgentAdmissionCheck(ctx, &agentIdentityWrapper{claims}); err != nil {
		if conduitAgentCheckRefused(err) {
			return refuse("agent")
		}
		return unevaluated(err)
	}

	// The token's run against the session's endpoint incarnation, as
	// admission compares it with the Hello's (relay.TokenRunBinding).
	if rs := s.authConfig.AgentRunScope; rs != nil {
		if b := rs.conduitBinding(context.WithoutCancel(ctx), claims, req); b != nil && b.RunID != sess.EndpointIncarnation {
			if b.OnMismatch != nil {
				b.OnMismatch(sess.EndpointIncarnation)
			}
			if b.Enforce {
				return refuse("run_mismatch")
			}
		}
	}

	var exp time.Time
	if claims.Expiry != nil {
		exp = claims.Expiry.Time().UTC()
	}
	log.Info("Conduit agent credential refreshed", "agent_id", p.ID, "expires_at", exp)
	s.recordConduitAgentRefresh(conduitRefreshOK)
	return nil
}

// OTelConduitAgentRefreshMetrics counts agent AuthRefresh outcomes as
// scion.hub.conduit.agent_refresh (attribute outcome: ok, refused or
// unevaluated).
type OTelConduitAgentRefreshMetrics struct {
	total metric.Int64Counter
}

// NewOTelConduitAgentRefreshMetrics creates the counter.
func NewOTelConduitAgentRefreshMetrics(mp metric.MeterProvider) (*OTelConduitAgentRefreshMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit agent refresh metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.conduit.agent_refresh",
		metric.WithUnit("{refresh}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitAgentRefreshMetrics{total: c}, nil
}

// RecordConduitAgentRefresh counts one AuthRefresh with outcome.
func (m *OTelConduitAgentRefreshMetrics) RecordConduitAgentRefresh(outcome string) {
	m.total.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// SetConduitAgentRefreshMetrics wires the scion.hub.conduit.agent_refresh
// counter. Until it is set, outcomes are not counted.
func (s *Server) SetConduitAgentRefreshMetrics(m *OTelConduitAgentRefreshMetrics) {
	s.conduitRefreshMetrics.Store(m)
}

func (s *Server) recordConduitAgentRefresh(outcome string) {
	if m := s.conduitRefreshMetrics.Load(); m != nil {
		m.RecordConduitAgentRefresh(outcome)
	}
}
