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
			if err := s.dispatcher.DispatchAgentResetAuth(ctx, &a); err != nil {
				slog.Error("Bulk reset-auth failed for agent", "agent_id", a.ID, "error", err)
				res.Error = err.Error()
			}
			results <- res
		}()
	}

	var succeeded []agentResult
	var failed []agentResult
	for range agents.Items {
		res := <-results
		if res.Error != "" {
			failed = append(failed, res)
		} else {
			succeeded = append(succeeded, res)
		}
	}

	slog.Info("Bulk reset-auth completed", "succeeded", len(succeeded), "failed", len(failed), "user", user.Email())

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"succeeded": succeeded,
		"failed":    failed,
		"total":     len(agents.Items),
	})
}
