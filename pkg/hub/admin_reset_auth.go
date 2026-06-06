package hub

import (
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleAdminResetAuthAll handles POST /api/v1/admin/agents/reset-auth-all.
// It lists all running agents and dispatches an auth reset for each one,
// returning a summary of successes and failures.
func (s *Server) handleAdminResetAuthAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	if s.dispatcher == nil {
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
	}

	var succeeded []agentResult
	var failed []agentResult

	for _, agent := range agents.Items {
		a := agent
		if err := s.dispatcher.DispatchAgentResetAuth(ctx, &a); err != nil {
			slog.Error("Bulk reset-auth failed for agent", "agent_id", a.ID, "error", err)
			failed = append(failed, agentResult{ID: a.ID, Name: a.Name, Error: err.Error()})
		} else {
			succeeded = append(succeeded, agentResult{ID: a.ID, Name: a.Name})
		}
	}

	slog.Info("Bulk reset-auth completed", "succeeded", len(succeeded), "failed", len(failed), "user", user.Email())

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"succeeded": succeeded,
		"failed":    failed,
		"total":     len(agents.Items),
	})
}
