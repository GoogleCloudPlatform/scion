// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mintSite names an agent-token mint site in the agent_token_issue_denied
// audit record. Each caller passes its own site constant.
type mintSite string

// Agent-token mint sites.
const (
	mintSiteCreate    mintSite = "create"
	mintSiteStart     mintSite = "start"
	mintSiteRestart   mintSite = "restart"
	mintSiteResetAuth mintSite = "reset_auth"
	mintSiteRefresh   mintSite = "refresh"
)

// mutationTypeAgentTokenIssueDenied is the audit mutation type written when a
// mint or refresh issues no token.
const mutationTypeAgentTokenIssueDenied = "agent_token_issue_denied"

// Actor recorded on an agent_token_issue_denied record written with no
// request principal in the context.
const (
	mintAuditSystemActorKind = "system"
	mintAuditSystemActorID   = "hub"
)

// Error classes recorded for a mint error that carries no DenyCause.
const (
	mintErrorClassLookup       = "lookup_error"
	mintErrorClassTokenService = "token_service_error"
)

// agentTokenIssueError is the error a mint site returns when
// GenerateAgentTokenForAgent issues no token. It unwraps to the cause.
type agentTokenIssueError struct {
	Site mintSite
	// Cause is set for a structural chain outcome (403).
	Cause DenyCause
	// Lookup is set for a store or lookup fault (503).
	Lookup bool
	Err    error
}

func (e *agentTokenIssueError) Error() string {
	return fmt.Sprintf("agent token not issued (%s): %v", e.Site, e.Err)
}

func (e *agentTokenIssueError) Unwrap() error { return e.Err }

// errorClass is the value recorded in the audit record: the DenyCause, or
// the lookup or token-service error class.
func (e *agentTokenIssueError) errorClass() string {
	switch {
	case e.Cause != "":
		return string(e.Cause)
	case e.Lookup:
		return mintErrorClassLookup
	default:
		return mintErrorClassTokenService
	}
}

// errMintLookup marks a ceiling lookup fault inside GenerateAgentTokenForAgent.
var errMintLookup = errors.New("agent token: ceiling lookup failed")

// GenerateAgentTokenForAgent issues an agent JWT for the stored agent record.
// The scopes are the mint candidates (role scopes plus config-derived
// scopes, after the dev-auth override) filtered by the agent's chain effect
// ceiling; the ancestry is the stored agent.Ancestry. It issues no token on
// any error: a structural chain outcome returns an error wrapping the
// provenance sentinel, a lookup fault returns an error wrapping
// errMintLookup.
func (s *Server) GenerateAgentTokenForAgent(ctx context.Context, agent *store.Agent) (string, error) {
	s.mu.RLock()
	tokenService := s.agentTokenService
	s.mu.RUnlock()
	if tokenService == nil {
		return "", fmt.Errorf("agent token service not initialized")
	}
	if agent == nil {
		return "", fmt.Errorf("%w: no agent", ErrProvenanceChain)
	}
	if s.authzService == nil {
		return "", fmt.Errorf("%w: authorization service not initialized", errMintLookup)
	}

	candidates := s.authzService.mintCandidateScopes(agent)
	scopes, err := s.authzService.ceilingFilteredAgentScopes(ctx, agent, candidates)
	if err != nil {
		if isStructuralProvenanceError(err) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", errMintLookup, err)
	}
	return tokenService.GenerateAgentToken(agent.ID, agent.ProjectID, scopes, agent.Ancestry)
}

// mintAgentTokenAt calls gen.GenerateAgentTokenForAgent for the named site.
// On error it writes the agent_token_issue_denied audit record synchronously
// and returns an *agentTokenIssueError.
func mintAgentTokenAt(ctx context.Context, gen AgentTokenGenerator, st store.Store, agent *store.Agent, site mintSite) (string, error) {
	token, err := gen.GenerateAgentTokenForAgent(ctx, agent)
	if err == nil {
		return token, nil
	}
	issueErr := &agentTokenIssueError{Site: site, Err: err}
	if cause, structural := ceilingDenyCauseForError(err); structural {
		issueErr.Cause = cause
	} else if errors.Is(err, errMintLookup) {
		issueErr.Lookup = true
	}
	recordAgentTokenIssueDenied(ctx, st, agent, issueErr)
	return "", issueErr
}

// recordAgentTokenIssueDenied writes the agent_token_issue_denied audit
// record naming the agent, site and error class. It never records scopes.
// A write failure is logged; the mint site has already failed.
func recordAgentTokenIssueDenied(ctx context.Context, st store.Store, agent *store.Agent, e *agentTokenIssueError) {
	agentID := ""
	if agent != nil {
		agentID = agent.ID
	}
	slog.WarnContext(ctx, "agent token not issued",
		"agent_id", agentID, "site", e.Site, "deny_cause", e.errorClass(), "error", e.Err)
	if st == nil {
		return
	}
	summary, _ := json.Marshal(map[string]string{"site": string(e.Site), "deny_cause": e.errorClass()})
	record := &store.MutationAuditRecord{
		MutationType: mutationTypeAgentTokenIssueDenied,
		TargetType:   "agent",
		TargetID:     agentID,
		AfterSummary: string(summary),
		Timestamp:    time.Now(),
	}
	auditActorFromContext(ctx).ApplyActor(record)
	if record.ActorPrincipalKind == "" {
		// A hub-initiated mint (reconcile restart, scheduled dispatch) runs
		// with no request principal.
		record.ActorPrincipalKind = mintAuditSystemActorKind
		record.ActorPrincipalID = mintAuditSystemActorID
	}
	if err := st.CreateMutationAudit(ctx, record); err != nil {
		slog.ErrorContext(ctx, "agent token not issued: audit write failed",
			"agent_id", agentID, "site", e.Site, "error", err)
	}
}

// writeAgentTokenIssueError writes the response for a mint error and
// reports whether it did: a structural chain outcome is 403 with
// details.denied_by="delegation_ceiling", a lookup fault is 503. Any other
// error, including a token-service error, is left to the site's existing
// response.
func writeAgentTokenIssueError(w http.ResponseWriter, err error) bool {
	var e *agentTokenIssueError
	if !errors.As(err, &e) {
		return false
	}
	switch {
	case e.Cause != "":
		writeForbiddenDenial(w, agentTokenDenialMessage(e.Cause), DeniedByDelegationCeiling)
		return true
	case e.Lookup:
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"Unable to evaluate the agent's delegation ceiling; retry later", nil)
		return true
	default:
		return false
	}
}

// agentTokenDenialMessage is the neutral response message for a mint that
// the agent's delegation chain does not allow.
func agentTokenDenialMessage(cause DenyCause) string {
	switch cause {
	case DenyCauseCeilingSourceNotAllowed:
		return "The agent's delegation record names a source that is not accepted on this server"
	default:
		return "The agent's delegation record is missing or inconsistent; recreate the agent"
	}
}
