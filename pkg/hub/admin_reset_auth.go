package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleAdminResetAuthAll handles POST /api/v1/admin/agents/reset-auth-all.
// It lists all running agents and dispatches an auth reset for each one,
// returning a summary of successes and failures.
// Authorization: enforced by routeGuard via hub.auth_reset.execute permission.
//
// With {"reissue_scopes": true} it runs the bulk scope re-issue instead
// (handleAdminScopeReissueAll): hub super-admin only, dry run unless
// "dry_run": false.
func (s *Server) handleAdminResetAuthAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	var bulk ScopeReissueBulkRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&bulk); err != nil && !errors.Is(err, io.EOF) {
			ValidationError(w, "invalid reset-auth-all request body", nil)
			return
		}
	}
	if bulk.ReissueScopes {
		s.handleAdminScopeReissueAll(w, r, bulk)
		return
	}
	if bulk.DryRun != nil {
		ValidationError(w, "dry_run applies only with reissue_scopes", nil)
		return
	}

	user := GetUserIdentityFromContext(r.Context())

	ctx := r.Context()

	disp := s.GetDispatcher()
	if disp == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"agent dispatcher not configured", nil)
		return
	}

	agents, err := s.store.ListAgents(ctx, store.AgentFilter{Phase: "running"}, store.ListOptions{Limit: 1000})
	if err != nil {
		slog.Error("Failed to list running agents for bulk reset-auth", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to list running agents: "+err.Error(), nil)
		return
	}

	type agentResult struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Error string `json:"error,omitempty"`
		// Code is set to runtime_unavailable when the agent's broker does
		// not have the agent's runtime available, and to internal_error
		// when the agent token could not be issued; both can be retried.
		// A structural delegation denial sets one of the delegation_* codes
		// (resetAuthDelegationFailure), and an agent not in good standing
		// agent_not_in_standing; retrying does not help those.
		Code string `json:"code,omitempty"`
		// Cause is the deny cause of a structural delegation denial.
		Cause DenyCause `json:"cause,omitempty"`
		// AncestorID and AncestorDepth name the creator (depth 1) or
		// further ancestor whose delegation is missing, for code
		// delegation_creator_missing.
		AncestorID    string `json:"ancestor_id,omitempty"`
		AncestorDepth int    `json:"ancestor_depth,omitempty"`
	}

	// Dispatch concurrently with a bounded worker pool to avoid timeouts
	// when many agents are running across slow or unreachable brokers.
	results := make(chan agentResult, len(agents.Items))
	sem := make(chan struct{}, 20)

	for _, agent := range agents.Items {
		a := agent
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			res := agentResult{ID: a.ID, Name: a.Name}
			if err := disp.DispatchAgentResetAuth(ctx, &a); err != nil {
				res.Error = err.Error()
				if errors.Is(err, errAgentTokenRecord) {
					res.Error = agentTokenRecordFailedMessage
					res.Code = ErrCodeInternalError
				} else if errors.Is(err, errAgentNotInStanding) {
					res.Error = resetAuthNotInStandingReason
					res.Code = resetAuthCodeNotInStanding
				} else if f, ok := resetAuthDelegationFailure(err); ok {
					res.Error = f.Reason
					res.Code = f.Code
					res.Cause = f.Cause
					res.AncestorID = f.AncestorID
					res.AncestorDepth = f.AncestorDepth
				} else if isBrokerRuntimeUnavailable(err) {
					res.Code = brokerCodeRuntimeUnavailable
				}
				// The raw error stays in the log; the response carries
				// the plain reason for the classified failures.
				slog.Error("Bulk reset-auth failed for agent", "agent_id", a.ID, "code", res.Code, "error", err)
			}
			results <- res
		}()
	}

	var succeeded []agentResult
	var failed []agentResult
	failedByCode := map[string]int{}
	for range agents.Items {
		res := <-results
		if res.Error != "" {
			failed = append(failed, res)
			code := res.Code
			if code == "" {
				code = resetAuthCodeUnclassified
			}
			failedByCode[code]++
		} else {
			succeeded = append(succeeded, res)
		}
	}

	slog.Info("Bulk reset-auth completed", "succeeded", len(succeeded), "failed", len(failed),
		"failed_by_code", failedByCode, "user", user.Email())

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"succeeded":      succeeded,
		"failed":         failed,
		"total":          len(agents.Items),
		"failed_by_code": failedByCode,
	})
}

// Per-agent failure codes of the bulk reset-auth for a structural delegation
// denial: the agent's chain grants no token, on this or any other mint path,
// until the agent is recreated (or, for its own missing edge, reincarnated).
const (
	// resetAuthCodeCreatorMissing: the agent's creator, or a further
	// ancestor, has no active delegation in the agent's project.
	resetAuthCodeCreatorMissing = "delegation_creator_missing"
	// resetAuthCodeOwnMissing: the agent has no active delegation of its
	// own in its project.
	resetAuthCodeOwnMissing = "delegation_own_missing"
	// resetAuthCodeDelegationCeiling: any other structural chain outcome
	// (ambiguous or invalid chain, a source that is not accepted).
	resetAuthCodeDelegationCeiling = "delegation_ceiling"
)

// resetAuthCodeNotInStanding is the per-agent failure code of the bulk
// reset-auth for an agent that is not in good standing (errAgentNotInStanding),
// with the fixed reason resetAuthNotInStandingReason; the specific standing
// reason stays in the server log.
const (
	resetAuthCodeNotInStanding   = "agent_not_in_standing"
	resetAuthNotInStandingReason = "this agent is not in good standing (for example it is held, or its owner no longer has access); resolve its standing, then reset auth again"
)

// resetAuthCodeUnclassified is the failed_by_code key for a failure that
// carries no code.
const resetAuthCodeUnclassified = "unclassified"

// resetAuthDelegation is the reported form of a structural delegation denial
// in the bulk reset-auth response: a stable code, the deny cause, the
// creator or ancestor it names, and a plain reason. The raw error stays in
// the server log.
type resetAuthDelegation struct {
	Code          string
	Cause         DenyCause
	AncestorID    string
	AncestorDepth int
	Reason        string
}

// resetAuthDelegationFailure classifies err, a DispatchAgentResetAuth error,
// as a structural delegation denial. ok is false for anything else (a lookup
// fault, an agent not in good standing, a broker error).
func resetAuthDelegationFailure(err error) (resetAuthDelegation, bool) {
	var issueErr *agentTokenIssueError
	if !errors.As(err, &issueErr) || issueErr.Cause == "" {
		return resetAuthDelegation{}, false
	}
	out := resetAuthDelegation{Cause: issueErr.Cause}
	var ancestor *ancestorEdgeMissingError
	switch {
	case errors.As(issueErr.Err, &ancestor):
		out.Code = resetAuthCodeCreatorMissing
		out.AncestorID = ancestor.AgentID
		out.AncestorDepth = ancestor.Depth
		if ancestor.Depth == 1 {
			out.Reason = fmt.Sprintf("creator agent %s no longer has an active delegation in this project; recreate this agent",
				ancestor.AgentID)
		} else {
			out.Reason = fmt.Sprintf("ancestor agent %s (%d levels up the creator chain) no longer has an active delegation in this project; recreate this agent",
				ancestor.AgentID, ancestor.Depth)
		}
	case issueErr.OwnEdgeMissing:
		out.Code = resetAuthCodeOwnMissing
		out.Reason = "this agent has no active delegation of its own in this project; reincarnate or recreate this agent"
	default:
		out.Code = resetAuthCodeDelegationCeiling
		out.Reason = fmt.Sprintf("this agent's delegation chain does not authorize a new credential (%s); see the hub log for details",
			issueErr.Cause)
	}
	return out, true
}
