package hub

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/util"
)

// ============================================================================
// Health Endpoints
// ============================================================================

type HealthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Uptime  string            `json:"uptime"`
	Checks  map[string]string `json:"checks,omitempty"`
	Stats   *HealthStats      `json:"stats,omitempty"`
}

type HealthStats struct {
	ConnectedHosts int `json:"connectedHosts,omitempty"`
	ActiveAgents   int `json:"activeAgents,omitempty"`
	Groves         int `json:"groves,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	checks := make(map[string]string)

	// Check database
	if err := s.store.Ping(r.Context()); err != nil {
		checks["database"] = "unhealthy"
	} else {
		checks["database"] = "healthy"
	}

	// Get stats
	stats := &HealthStats{}
	if agentResult, err := s.store.ListAgents(r.Context(), store.AgentFilter{Status: store.AgentStatusRunning}, store.ListOptions{Limit: 1}); err == nil {
		stats.ActiveAgents = agentResult.TotalCount
	}
	if groveResult, err := s.store.ListGroves(r.Context(), store.GroveFilter{}, store.ListOptions{Limit: 1}); err == nil {
		stats.Groves = groveResult.TotalCount
	}
	if hostResult, err := s.store.ListRuntimeHosts(r.Context(), store.RuntimeHostFilter{Status: store.HostStatusOnline}, store.ListOptions{Limit: 1}); err == nil {
		stats.ConnectedHosts = hostResult.TotalCount
	}

	status := "healthy"
	for _, v := range checks {
		if v != "healthy" {
			status = "degraded"
			break
		}
	}

	resp := HealthResponse{
		Status:  status,
		Version: "0.1.0", // TODO: Get from build info
		Uptime:  time.Since(s.startTime).Round(time.Second).String(),
		Checks:  checks,
		Stats:   stats,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Check if database is connected and migrated
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "database not available",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Return metrics snapshot if available
	if s.metrics != nil {
		snapshot := s.metrics.GetSnapshot()
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	// No metrics available
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "no_metrics",
		"reason": "metrics not configured",
	})
}

// ============================================================================
// Agent Endpoints
// ============================================================================

type ListAgentsResponse struct {
	Agents     []store.Agent `json:"agents"`
	NextCursor string        `json:"nextCursor,omitempty"`
	TotalCount int           `json:"totalCount"`
}

type CreateAgentRequest struct {
	Name          string            `json:"name"`
	GroveID       string            `json:"groveId"`
	RuntimeHostID string            `json:"runtimeHostId,omitempty"` // Optional: uses grove's default if not specified
	Template      string            `json:"template"`
	Task          string            `json:"task,omitempty"`
	Branch        string            `json:"branch,omitempty"`
	Workspace     string            `json:"workspace,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Config        *AgentConfigOverride `json:"config,omitempty"`
}

type AgentConfigOverride struct {
	Image    string            `json:"image,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Detached *bool             `json:"detached,omitempty"`
	Model    string            `json:"model,omitempty"`
}

type CreateAgentResponse struct {
	Agent    *store.Agent `json:"agent"`
	Warnings []string     `json:"warnings,omitempty"`
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.createAgent(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.AgentFilter{
		GroveID:       query.Get("groveId"),
		RuntimeHostID: query.Get("runtimeHostId"),
		Status:        query.Get("status"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:     result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.GroveID == "" {
		ValidationError(w, "groveId is required", nil)
		return
	}

	// Verify grove exists and get its configuration
	grove, err := s.store.GetGrove(ctx, req.GroveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve the runtime host
	runtimeHostID, err := s.resolveRuntimeHost(ctx, w, req.RuntimeHostID, grove)
	if err != nil {
		// Error response already written by resolveRuntimeHost
		return
	}

	// Create agent
	agent := &store.Agent{
		ID:            api.NewUUID(),
		AgentID:       api.Slugify(req.Name),
		Name:          req.Name,
		Template:      req.Template,
		GroveID:       req.GroveID,
		RuntimeHostID: runtimeHostID,
		Status:        store.AgentStatusPending,
		Labels:        req.Labels,
		Visibility:    store.VisibilityPrivate,
	}

	if req.Config != nil {
		agent.Image = req.Config.Image
		if req.Config.Detached != nil {
			agent.Detached = *req.Config.Detached
		} else {
			agent.Detached = true
		}
		agent.AppliedConfig = &store.AgentAppliedConfig{
			Image:   req.Config.Image,
			Env:     req.Config.Env,
			Model:   req.Config.Model,
			Harness: req.Template,
			Task:    req.Task,
		}
	} else {
		agent.Detached = true
		// Store task even when no config override is provided
		if req.Task != "" {
			agent.AppliedConfig = &store.AgentAppliedConfig{
				Harness: req.Template,
				Task:    req.Task,
			}
		}
	}

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If a dispatcher is available (co-located runtime host) and a task was provided,
	// dispatch the agent to start it immediately.
	// Without a task, this is a "create only" operation (e.g., scion create).
	var warnings []string
	if req.Task != "" {
		if dispatcher := s.GetDispatcher(); dispatcher != nil {
			if err := dispatcher.DispatchAgentCreate(ctx, agent); err != nil {
				// Log the error but don't fail the request - agent is created in Hub
				warnings = append(warnings, "Failed to dispatch to runtime host: "+err.Error())
				// The agent remains in pending status
			} else {
				// Update agent status to reflect it's being started
				agent.Status = store.AgentStatusProvisioning
				if err := s.store.UpdateAgent(ctx, agent); err != nil {
					warnings = append(warnings, "Failed to update agent status: "+err.Error())
				}
			}
		}
	}

	writeJSON(w, http.StatusCreated, CreateAgentResponse{
		Agent:    agent,
		Warnings: warnings,
	})
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}

	// Handle actions
	if action != "" {
		s.handleAgentAction(w, r, id, action)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAgent(w, r, id)
	case http.MethodPatch:
		s.updateAgent(w, r, id)
	case http.MethodDelete:
		s.deleteAgent(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, id string) {
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updates struct {
		Name         string            `json:"name,omitempty"`
		Labels       map[string]string `json:"labels,omitempty"`
		Annotations  map[string]string `json:"annotations,omitempty"`
		TaskSummary  string            `json:"taskSummary,omitempty"`
		StateVersion int64             `json:"stateVersion"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Check version for optimistic locking
	if updates.StateVersion != 0 && updates.StateVersion != agent.StateVersion {
		Conflict(w, "Version conflict - resource was modified")
		return
	}

	// Apply updates
	if updates.Name != "" {
		agent.Name = updates.Name
	}
	if updates.Labels != nil {
		agent.Labels = updates.Labels
	}
	if updates.Annotations != nil {
		agent.Annotations = updates.Annotations
	}
	if updates.TaskSummary != "" {
		agent.TaskSummary = updates.TaskSummary
	}

	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"

	// Get the agent to dispatch deletion to runtime host
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If a dispatcher is available, dispatch the deletion to the runtime host
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeHostID != "" {
		if err := dispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch); err != nil {
			// Log but continue - the agent record should still be deleted from hub
			// The runtime host deletion is best-effort
			// (agent may already be stopped/deleted on the host)
		}
	}

	if err := s.store.DeleteAgent(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// For actions other than "status", we require user authentication.
	// Agents should only be able to update their own status.
	if action != "status" {
		if GetUserIdentityFromContext(r.Context()) == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user authentication", nil)
			return
		}
	}

	switch action {
	case "status":
		s.updateAgentStatus(w, r, id)
	case "start", "stop", "restart":
		s.handleAgentLifecycle(w, r, id, action)
	case "message":
		s.handleAgentMessage(w, r, id)
	default:
		NotFound(w, "Action")
	}
}

// MessageRequest is the request body for sending a message to an agent.
type MessageRequest struct {
	Message   string `json:"message"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Message == "" {
		ValidationError(w, "message is required", nil)
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If a dispatcher is available, dispatch the message to the runtime host
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeHostID != "" {
		if err := dispatcher.DispatchAgentMessage(ctx, agent, req.Message, req.Interrupt); err != nil {
			RuntimeError(w, "Failed to send message to runtime host: "+err.Error())
			return
		}
	} else {
		// No dispatcher available
		RuntimeError(w, "No runtime host dispatcher available for this agent")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) updateAgentStatus(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)

	// If identity is an agent, verify it's the same agent and has the correct scope
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ID() != id {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only update their own status", nil)
			return
		}
		if !agentIdent.HasScope(ScopeAgentStatusUpdate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: agent:status:update", nil)
			return
		}
	} else if identity == nil {
		Unauthorized(w)
		return
	}

	var status store.AgentStatusUpdate
	if err := readJSON(r, &status); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if err := s.store.UpdateAgentStatus(ctx, id, status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAgentLifecycle(w http.ResponseWriter, r *http.Request, id, action string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var newStatus string
	var dispatchErr error

	// If a dispatcher is available, dispatch the operation to the runtime host
	dispatcher := s.GetDispatcher()

	switch action {
	case "start":
		newStatus = store.AgentStatusRunning
		if dispatcher != nil && agent.RuntimeHostID != "" {
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent)
		}
	case "stop":
		newStatus = store.AgentStatusStopped
		if dispatcher != nil && agent.RuntimeHostID != "" {
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
		}
	case "restart":
		newStatus = store.AgentStatusRunning
		if dispatcher != nil && agent.RuntimeHostID != "" {
			dispatchErr = dispatcher.DispatchAgentRestart(ctx, agent)
		}
	}

	// If dispatch failed, return error
	if dispatchErr != nil {
		RuntimeError(w, "Failed to dispatch to runtime host: "+dispatchErr.Error())
		return
	}

	if err := s.store.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{
		Status: newStatus,
	}); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agent.Status = newStatus
	writeJSON(w, http.StatusOK, agent)
}

// ============================================================================
// Grove Endpoints
// ============================================================================

type ListGrovesResponse struct {
	Groves     []store.Grove `json:"groves"`
	NextCursor string        `json:"nextCursor,omitempty"`
	TotalCount int           `json:"totalCount"`
}

type CreateGroveRequest struct {
	Name       string            `json:"name"`
	GitRemote  string            `json:"gitRemote,omitempty"`
	Visibility string            `json:"visibility,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type RegisterGroveRequest struct {
	ID        string            `json:"id,omitempty"` // Client-provided grove ID
	Name      string            `json:"name"`
	GitRemote string            `json:"gitRemote"`
	Path      string            `json:"path,omitempty"`
	Host      *RegisterHostInfo `json:"host,omitempty"`
	Profiles  []string          `json:"profiles,omitempty"`
	Mode      string            `json:"mode,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type RegisterHostInfo struct {
	ID           string                  `json:"id,omitempty"`
	Name         string                  `json:"name"`
	Version      string                  `json:"version,omitempty"`
	Capabilities *store.HostCapabilities `json:"capabilities,omitempty"`
	Profiles     []store.HostProfile     `json:"profiles,omitempty"`
}

type RegisterGroveResponse struct {
	Grove     *store.Grove       `json:"grove"`
	Host      *store.RuntimeHost `json:"host,omitempty"`
	Created   bool               `json:"created"`
	HostToken string             `json:"hostToken,omitempty"`
}

func (s *Server) handleGroves(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listGroves(w, r)
	case http.MethodPost:
		s.createGrove(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listGroves(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.GroveFilter{
		Visibility:      query.Get("visibility"),
		GitRemotePrefix: util.NormalizeGitRemote(query.Get("gitRemote")),
		HostID:          query.Get("hostId"),
		Name:            query.Get("name"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListGroves(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListGrovesResponse{
		Groves:     result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) createGrove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateGroveRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       req.Name,
		Slug:       api.Slugify(req.Name),
		GitRemote:  util.NormalizeGitRemote(req.GitRemote),
		Labels:     req.Labels,
		Visibility: req.Visibility,
	}

	if grove.Visibility == "" {
		grove.Visibility = store.VisibilityPrivate
	}

	if err := s.store.CreateGrove(ctx, grove); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, grove)
}

func (s *Server) handleGroveRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	var req RegisterGroveRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	normalizedRemote := util.NormalizeGitRemote(req.GitRemote)

	// Try to find existing grove
	var grove *store.Grove
	var created bool

	// First, try to look up by client-provided grove ID
	if req.ID != "" {
		existingGrove, err := s.store.GetGrove(ctx, req.ID)
		if err == nil {
			grove = existingGrove
		} else if err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// If not found by ID, try git remote lookup
	if grove == nil && normalizedRemote != "" {
		// For groves with git remote, look up by git remote (exact match)
		existingGrove, err := s.store.GetGroveByGitRemote(ctx, normalizedRemote)
		if err == nil {
			grove = existingGrove
		} else if err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// If still not found and no git remote, try by slug (for global groves)
	if grove == nil && normalizedRemote == "" {
		// For groves without git remote (like global groves), look up by slug (case-insensitive)
		slug := api.Slugify(req.Name)
		existingGrove, err := s.store.GetGroveBySlugCaseInsensitive(ctx, slug)
		if err == nil {
			grove = existingGrove
		} else if err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Create new grove if not found
	if grove == nil {
		// Use client-provided ID if available, otherwise generate
		groveID := req.ID
		if groveID == "" {
			groveID = api.NewUUID()
		}

		grove = &store.Grove{
			ID:         groveID,
			Name:       req.Name,
			Slug:       api.Slugify(req.Name),
			GitRemote:  normalizedRemote,
			Labels:     req.Labels,
			Visibility: store.VisibilityPrivate,
		}

		if err := s.store.CreateGrove(ctx, grove); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		created = true
	}

	// Handle host registration if provided
	var host *store.RuntimeHost
	var hostToken string

	if req.Host != nil {
		hostID := req.Host.ID

		// Try to find existing host by ID first, then by name
		var existingHost *store.RuntimeHost
		var err error

		if hostID != "" {
			existingHost, err = s.store.GetRuntimeHost(ctx, hostID)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		// If not found by ID, try to find by name (prevents duplicate hosts with same hostname)
		if existingHost == nil && req.Host.Name != "" {
			existingHost, err = s.store.GetRuntimeHostByName(ctx, req.Host.Name)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		if existingHost != nil {
			// Update existing host
			host = existingHost
			host.Name = req.Host.Name
			host.Slug = api.Slugify(req.Host.Name)
			host.Version = req.Host.Version
			host.Status = store.HostStatusOnline
			host.ConnectionState = "connected"
			host.Capabilities = req.Host.Capabilities
			host.Profiles = req.Host.Profiles

			if req.Mode != "" {
				host.Mode = req.Mode
			}

			if err := s.store.UpdateRuntimeHost(ctx, host); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			// Create new host
			if hostID == "" {
				hostID = api.NewUUID()
			}

			host = &store.RuntimeHost{
				ID:              hostID,
				Name:            req.Host.Name,
				Slug:            api.Slugify(req.Host.Name),
				Mode:            req.Mode,
				Version:         req.Host.Version,
				Status:          store.HostStatusOnline,
				ConnectionState: "connected",
				Capabilities:    req.Host.Capabilities,
				Profiles:        req.Host.Profiles,
			}

			if host.Mode == "" {
				host.Mode = store.HostModeConnected
			}

			if err := s.store.CreateRuntimeHost(ctx, host); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		// Add as grove contributor
		contrib := &store.GroveContributor{
			GroveID:   grove.ID,
			HostID:    host.ID,
			HostName:  host.Name,
			LocalPath: req.Path, // Filesystem path to the grove on this host
			Mode:      host.Mode,
			Status:    store.HostStatusOnline,
		}

		if err := s.store.AddGroveContributor(ctx, contrib); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Set as default runtime host if grove doesn't have one
		// (first host to register becomes the default)
		if grove.DefaultRuntimeHostID == "" {
			grove.DefaultRuntimeHostID = host.ID
			if err := s.store.UpdateGrove(ctx, grove); err != nil {
				// Log but don't fail - the host is registered, default can be set later
				// In production, log this error
			}
		}

		// Generate a simple token (in production, use proper token generation)
		hostToken = "host_" + api.NewShortID() + "_" + api.NewShortID()
	}

	writeJSON(w, http.StatusOK, RegisterGroveResponse{
		Grove:     grove,
		Host:      host,
		Created:   created,
		HostToken: hostToken,
	})
}

// handleGroveRoutes routes requests under /api/v1/groves/{groveId}/...
// It supports both the grove resource endpoints and nested agent endpoints.
func (s *Server) handleGroveRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract grove ID and remaining path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/groves/")
	if path == "" {
		NotFound(w, "Grove")
		return
	}

	// Parse the grove ID (supports both UUID and {uuid}__{slug} format)
	// The grove ID may contain "__" so we need to find the first "/"
	parts := strings.SplitN(path, "/", 2)
	groveIDRaw := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Skip the register endpoint - it's handled separately
	if groveIDRaw == "register" {
		NotFound(w, "Grove")
		return
	}

	// Parse grove ID to extract UUID (supports {uuid}__{slug} format)
	groveID := resolveGroveID(groveIDRaw)

	// Check for nested /agents path
	if strings.HasPrefix(subPath, "agents") {
		agentPath := strings.TrimPrefix(subPath, "agents")
		agentPath = strings.TrimPrefix(agentPath, "/")
		s.handleGroveAgents(w, r, groveID, agentPath)
		return
	}

	// Check for nested /env path
	if strings.HasPrefix(subPath, "env") {
		envPath := strings.TrimPrefix(subPath, "env")
		envPath = strings.TrimPrefix(envPath, "/")
		if envPath == "" {
			s.handleGroveEnvVars(w, r, groveID)
		} else {
			s.handleGroveEnvVarByKey(w, r, groveID, envPath)
		}
		return
	}

	// Check for nested /secrets path
	if strings.HasPrefix(subPath, "secrets") {
		secretPath := strings.TrimPrefix(subPath, "secrets")
		secretPath = strings.TrimPrefix(secretPath, "/")
		if secretPath == "" {
			s.handleGroveSecrets(w, r, groveID)
		} else {
			s.handleGroveSecretByKey(w, r, groveID, secretPath)
		}
		return
	}

	// Otherwise handle as grove resource
	s.handleGroveByIDInternal(w, r, groveID, subPath)
}

// handleGroveByIDInternal handles grove resource operations
func (s *Server) handleGroveByIDInternal(w http.ResponseWriter, r *http.Request, groveID, subPath string) {
	// Only handle if no subpath (direct grove resource)
	if subPath != "" {
		NotFound(w, "Grove resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getGrove(w, r, groveID)
	case http.MethodPatch:
		s.updateGrove(w, r, groveID)
	case http.MethodDelete:
		s.deleteGrove(w, r, groveID)
	default:
		MethodNotAllowed(w)
	}
}

// handleGroveAgents handles agent operations scoped to a grove
// Path: /api/v1/groves/{groveId}/agents[/{agentId}[/{action}]]
func (s *Server) handleGroveAgents(w http.ResponseWriter, r *http.Request, groveID, agentPath string) {
	ctx := r.Context()

	// Verify grove exists
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// No agent ID - list or create agents in this grove
	if agentPath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listGroveAgents(w, r, grove.ID)
		case http.MethodPost:
			s.createGroveAgent(w, r, grove.ID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// Parse agent ID and action
	parts := strings.SplitN(agentPath, "/", 2)
	agentIDRaw := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// Handle actions
	if action != "" {
		s.handleGroveAgentAction(w, r, grove.ID, agentIDRaw, action)
		return
	}

	// Handle agent by ID within grove
	switch r.Method {
	case http.MethodGet:
		s.getGroveAgent(w, r, grove.ID, agentIDRaw)
	case http.MethodPatch:
		s.updateGroveAgent(w, r, grove.ID, agentIDRaw)
	case http.MethodDelete:
		s.deleteGroveAgent(w, r, grove.ID, agentIDRaw)
	default:
		MethodNotAllowed(w)
	}
}

// listGroveAgents lists agents within a specific grove
func (s *Server) listGroveAgents(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.AgentFilter{
		GroveID:       groveID,
		RuntimeHostID: query.Get("runtimeHostId"),
		Status:        query.Get("status"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:     result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

// createGroveAgent creates an agent within a specific grove
func (s *Server) createGroveAgent(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	var req CreateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	// Get grove to access its configuration (including default runtime host)
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve the runtime host
	runtimeHostID, err := s.resolveRuntimeHost(ctx, w, req.RuntimeHostID, grove)
	if err != nil {
		// Error response already written by resolveRuntimeHost
		return
	}

	// Create agent
	agent := &store.Agent{
		ID:            api.NewUUID(),
		AgentID:       api.Slugify(req.Name),
		Name:          req.Name,
		Template:      req.Template,
		GroveID:       groveID,
		RuntimeHostID: runtimeHostID,
		Status:        store.AgentStatusPending,
		Labels:        req.Labels,
		Visibility:    store.VisibilityPrivate,
	}

	if req.Config != nil {
		agent.Image = req.Config.Image
		if req.Config.Detached != nil {
			agent.Detached = *req.Config.Detached
		} else {
			agent.Detached = true
		}
		agent.AppliedConfig = &store.AgentAppliedConfig{
			Image:   req.Config.Image,
			Env:     req.Config.Env,
			Model:   req.Config.Model,
			Harness: req.Template,
			Task:    req.Task,
		}
	} else {
		agent.Detached = true
		// Store task even when no config override is provided
		if req.Task != "" {
			agent.AppliedConfig = &store.AgentAppliedConfig{
				Harness: req.Template,
				Task:    req.Task,
			}
		}
	}

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If a dispatcher is available (co-located runtime host) and a task was provided,
	// dispatch the agent to start it immediately.
	// Without a task, this is a "create only" operation (e.g., scion create).
	var warnings []string
	if req.Task != "" {
		if dispatcher := s.GetDispatcher(); dispatcher != nil {
			if err := dispatcher.DispatchAgentCreate(ctx, agent); err != nil {
				// Log the error but don't fail the request - agent is created in Hub
				warnings = append(warnings, "Failed to dispatch to runtime host: "+err.Error())
				// The agent remains in pending status
			} else {
				// Update agent status to reflect it's being started
				agent.Status = store.AgentStatusProvisioning
				if err := s.store.UpdateAgent(ctx, agent); err != nil {
					warnings = append(warnings, "Failed to update agent status: "+err.Error())
				}
			}
		}
	}

	writeJSON(w, http.StatusCreated, CreateAgentResponse{
		Agent:    agent,
		Warnings: warnings,
	})
}

// getGroveAgent gets an agent by ID within a specific grove
func (s *Server) getGroveAgent(w http.ResponseWriter, r *http.Request, groveID, agentID string) {
	ctx := r.Context()

	// Try to get by slug first (more common case)
	agent, err := s.store.GetAgentBySlug(ctx, groveID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by UUID
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			// Verify it belongs to this grove
			if agent.GroveID != groveID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	writeJSON(w, http.StatusOK, agent)
}

// updateGroveAgent updates an agent within a specific grove
func (s *Server) updateGroveAgent(w http.ResponseWriter, r *http.Request, groveID, agentID string) {
	ctx := r.Context()

	// Try to get by slug first
	agent, err := s.store.GetAgentBySlug(ctx, groveID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by UUID
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.GroveID != groveID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	var updates struct {
		Name         string            `json:"name,omitempty"`
		Labels       map[string]string `json:"labels,omitempty"`
		Annotations  map[string]string `json:"annotations,omitempty"`
		TaskSummary  string            `json:"taskSummary,omitempty"`
		StateVersion int64             `json:"stateVersion"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Check version for optimistic locking
	if updates.StateVersion != 0 && updates.StateVersion != agent.StateVersion {
		Conflict(w, "Version conflict - resource was modified")
		return
	}

	// Apply updates
	if updates.Name != "" {
		agent.Name = updates.Name
	}
	if updates.Labels != nil {
		agent.Labels = updates.Labels
	}
	if updates.Annotations != nil {
		agent.Annotations = updates.Annotations
	}
	if updates.TaskSummary != "" {
		agent.TaskSummary = updates.TaskSummary
	}

	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, agent)
}

// deleteGroveAgent deletes an agent within a specific grove
func (s *Server) deleteGroveAgent(w http.ResponseWriter, r *http.Request, groveID, agentID string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"

	// Try to get by slug first to verify grove membership
	agent, err := s.store.GetAgentBySlug(ctx, groveID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by UUID
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.GroveID != groveID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// If a dispatcher is available, dispatch the deletion to the runtime host
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeHostID != "" {
		if err := dispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch); err != nil {
			// Log but continue - the agent record should still be deleted from hub
		}
	}

	if err := s.store.DeleteAgent(ctx, agent.ID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleGroveAgentAction handles actions on agents within a grove
func (s *Server) handleGroveAgentAction(w http.ResponseWriter, r *http.Request, groveID, agentID, action string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Resolve agent ID
	agent, err := s.store.GetAgentBySlug(ctx, groveID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.GroveID != groveID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	switch action {
	case "status":
		s.updateAgentStatus(w, r, agent.ID)
	case "start", "stop", "restart":
		s.handleAgentLifecycle(w, r, agent.ID, action)
	case "message":
		s.handleAgentMessage(w, r, agent.ID)
	default:
		NotFound(w, "Action")
	}
}

// resolveGroveID extracts the UUID from a grove ID that may be in {uuid}__{slug} format
func resolveGroveID(groveIDRaw string) string {
	id, _, ok := api.ParseGroveID(groveIDRaw)
	if ok {
		return id
	}
	// Not in hosted format - return as-is (may be just a UUID or slug)
	return groveIDRaw
}

// handleGroveByID is deprecated - use handleGroveRoutes instead
func (s *Server) handleGroveByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/groves")

	if id == "" || id == "register" {
		// Handled by handleGroveRegister
		NotFound(w, "Grove")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getGrove(w, r, id)
	case http.MethodPatch:
		s.updateGrove(w, r, id)
	case http.MethodDelete:
		s.deleteGrove(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getGrove(w http.ResponseWriter, r *http.Request, id string) {
	grove, err := s.store.GetGrove(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, grove)
}

func (s *Server) updateGrove(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	grove, err := s.store.GetGrove(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updates struct {
		Name       string            `json:"name,omitempty"`
		Labels     map[string]string `json:"labels,omitempty"`
		Visibility string            `json:"visibility,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if updates.Name != "" {
		grove.Name = updates.Name
	}
	if updates.Labels != nil {
		grove.Labels = updates.Labels
	}
	if updates.Visibility != "" {
		grove.Visibility = updates.Visibility
	}

	if err := s.store.UpdateGrove(ctx, grove); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, grove)
}

func (s *Server) deleteGrove(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteGrove(r.Context(), id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// RuntimeHost Endpoints
// ============================================================================

type ListRuntimeHostsResponse struct {
	Hosts      []store.RuntimeHost `json:"hosts"`
	NextCursor string              `json:"nextCursor,omitempty"`
	TotalCount int                 `json:"totalCount"`
}

// RuntimeHostWithContributor extends RuntimeHost with grove-specific contributor data.
// This is returned when listing hosts filtered by groveId, providing the local path
// for the grove on each host.
type RuntimeHostWithContributor struct {
	store.RuntimeHost
	LocalPath string `json:"localPath,omitempty"` // Filesystem path to the grove on this host
}

// ListRuntimeHostsWithContributorResponse is returned when filtering by groveId.
type ListRuntimeHostsWithContributorResponse struct {
	Hosts      []RuntimeHostWithContributor `json:"hosts"`
	NextCursor string                       `json:"nextCursor,omitempty"`
	TotalCount int                          `json:"totalCount"`
}

func (s *Server) handleRuntimeHosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRuntimeHosts(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listRuntimeHosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	groveID := query.Get("groveId")
	filter := store.RuntimeHostFilter{
		Status:  query.Get("status"),
		Mode:    query.Get("mode"),
		GroveID: groveID,
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListRuntimeHosts(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If filtering by groveId, include grove-specific contributor data (like localPath)
	if groveID != "" {
		// Get contributor data for this grove to include localPath
		contributors, err := s.store.GetGroveContributors(ctx, groveID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Build a map of hostId -> localPath for quick lookup
		hostLocalPaths := make(map[string]string)
		for _, c := range contributors {
			hostLocalPaths[c.HostID] = c.LocalPath
		}

		// Build extended host list with contributor data
		extendedHosts := make([]RuntimeHostWithContributor, 0, len(result.Items))
		for _, host := range result.Items {
			extendedHosts = append(extendedHosts, RuntimeHostWithContributor{
				RuntimeHost: host,
				LocalPath:   hostLocalPaths[host.ID],
			})
		}

		writeJSON(w, http.StatusOK, ListRuntimeHostsWithContributorResponse{
			Hosts:      extendedHosts,
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		})
		return
	}

	writeJSON(w, http.StatusOK, ListRuntimeHostsResponse{
		Hosts:      result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) handleRuntimeHostRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract host ID and remaining path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/runtime-hosts/")
	if path == "" {
		NotFound(w, "RuntimeHost")
		return
	}

	// Parse the host ID and subpath
	parts := strings.SplitN(path, "/", 2)
	hostID := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Check for nested /env path
	if strings.HasPrefix(subPath, "env") {
		envPath := strings.TrimPrefix(subPath, "env")
		envPath = strings.TrimPrefix(envPath, "/")
		if envPath == "" {
			s.handleHostEnvVars(w, r, hostID)
		} else {
			s.handleHostEnvVarByKey(w, r, hostID, envPath)
		}
		return
	}

	// Check for nested /secrets path
	if strings.HasPrefix(subPath, "secrets") {
		secretPath := strings.TrimPrefix(subPath, "secrets")
		secretPath = strings.TrimPrefix(secretPath, "/")
		if secretPath == "" {
			s.handleHostSecrets(w, r, hostID)
		} else {
			s.handleHostSecretByKey(w, r, hostID, secretPath)
		}
		return
	}

	// Delegate to the original handler for other operations
	s.handleRuntimeHostByIDInternal(w, r, hostID, subPath)
}

func (s *Server) handleRuntimeHostByIDInternal(w http.ResponseWriter, r *http.Request, id, subPath string) {
	if id == "" {
		NotFound(w, "RuntimeHost")
		return
	}

	// Handle heartbeat action
	if subPath == "heartbeat" && r.Method == http.MethodPost {
		s.handleHostHeartbeat(w, r, id)
		return
	}

	// Handle groves action
	if subPath == "groves" && r.Method == http.MethodGet {
		// TODO: Implement getHostGroves endpoint
		NotFound(w, "RuntimeHost resource")
		return
	}

	// Only handle if no subpath (direct resource)
	if subPath != "" {
		NotFound(w, "RuntimeHost resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRuntimeHost(w, r, id)
	case http.MethodPatch:
		s.updateRuntimeHost(w, r, id)
	case http.MethodDelete:
		s.deleteRuntimeHost(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleRuntimeHostByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/runtime-hosts")

	if id == "" {
		NotFound(w, "RuntimeHost")
		return
	}

	if action == "heartbeat" && r.Method == http.MethodPost {
		s.handleHostHeartbeat(w, r, id)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRuntimeHost(w, r, id)
	case http.MethodPatch:
		s.updateRuntimeHost(w, r, id)
	case http.MethodDelete:
		s.deleteRuntimeHost(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getRuntimeHost(w http.ResponseWriter, r *http.Request, id string) {
	host, err := s.store.GetRuntimeHost(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, host)
}

func (s *Server) updateRuntimeHost(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	host, err := s.store.GetRuntimeHost(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updates struct {
		Name   string            `json:"name,omitempty"`
		Labels map[string]string `json:"labels,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if updates.Name != "" {
		host.Name = updates.Name
	}
	if updates.Labels != nil {
		host.Labels = updates.Labels
	}

	if err := s.store.UpdateRuntimeHost(ctx, host); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, host)
}

func (s *Server) deleteRuntimeHost(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteRuntimeHost(r.Context(), id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHostHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	var heartbeat struct {
		Status string `json:"status"`
	}

	if err := readJSON(r, &heartbeat); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if err := s.store.UpdateRuntimeHostHeartbeat(r.Context(), id, heartbeat.Status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ============================================================================
// Template Endpoints
// ============================================================================

type ListTemplatesResponse struct {
	Templates  []store.Template `json:"templates"`
	NextCursor string           `json:"nextCursor,omitempty"`
	TotalCount int              `json:"totalCount"`
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTemplates(w, r)
	case http.MethodPost:
		s.createTemplate(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.TemplateFilter{
		Scope:   query.Get("scope"),
		GroveID: query.Get("groveId"),
		Harness: query.Get("harness"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListTemplates(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListTemplatesResponse{
		Templates:  result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if template.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if template.Harness == "" {
		ValidationError(w, "harness is required", nil)
		return
	}

	template.ID = api.NewUUID()
	template.Slug = api.Slugify(template.Name)

	if template.Scope == "" {
		template.Scope = "global"
	}
	if template.Visibility == "" {
		template.Visibility = store.VisibilityPrivate
	}

	if err := s.store.CreateTemplate(ctx, &template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, template)
}

func (s *Server) handleTemplateByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/templates")

	if id == "" {
		NotFound(w, "Template")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getTemplate(w, r, id)
	case http.MethodPut:
		s.updateTemplate(w, r, id)
	case http.MethodDelete:
		s.deleteTemplate(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request, id string) {
	template, err := s.store.GetTemplate(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, template)
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Preserve ID and timestamps
	template.ID = existing.ID
	template.Created = existing.Created

	if template.Slug == "" {
		template.Slug = api.Slugify(template.Name)
	}

	if err := s.store.UpdateTemplate(ctx, &template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, template)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteTemplate(r.Context(), id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// User Endpoints
// ============================================================================

type ListUsersResponse struct {
	Users      []store.User `json:"users"`
	NextCursor string       `json:"nextCursor,omitempty"`
	TotalCount int          `json:"totalCount"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUsers(w, r)
	case http.MethodPost:
		s.createUser(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.UserFilter{
		Role:   query.Get("role"),
		Status: query.Get("status"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListUsers(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListUsersResponse{
		Users:      result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var user store.User
	if err := readJSON(r, &user); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if user.Email == "" {
		ValidationError(w, "email is required", nil)
		return
	}
	if user.DisplayName == "" {
		ValidationError(w, "displayName is required", nil)
		return
	}

	user.ID = api.NewUUID()
	if user.Role == "" {
		user.Role = store.UserRoleMember
	}
	if user.Status == "" {
		user.Status = "active"
	}

	if err := s.store.CreateUser(ctx, &user); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/users")

	if id == "" {
		NotFound(w, "User")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getUser(w, r, id)
	case http.MethodPatch:
		s.updateUser(w, r, id)
	case http.MethodDelete:
		s.deleteUser(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, id string) {
	user, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, user)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updates struct {
		DisplayName string                  `json:"displayName,omitempty"`
		Role        string                  `json:"role,omitempty"`
		Status      string                  `json:"status,omitempty"`
		Preferences *store.UserPreferences `json:"preferences,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if updates.DisplayName != "" {
		user.DisplayName = updates.DisplayName
	}
	if updates.Role != "" {
		user.Role = updates.Role
	}
	if updates.Status != "" {
		user.Status = updates.Status
	}
	if updates.Preferences != nil {
		user.Preferences = updates.Preferences
	}

	if err := s.store.UpdateUser(ctx, user); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, user)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// Environment Variables Endpoints
// ============================================================================

type ListEnvVarsResponse struct {
	EnvVars []store.EnvVar `json:"envVars"`
	Scope   string         `json:"scope"`
	ScopeID string         `json:"scopeId"`
}

type SetEnvVarRequest struct {
	Value       string `json:"value"`
	Scope       string `json:"scope,omitempty"`
	ScopeID     string `json:"scopeId,omitempty"`
	Description string `json:"description,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
}

type SetEnvVarResponse struct {
	EnvVar  *store.EnvVar `json:"envVar"`
	Created bool          `json:"created"`
}

func (s *Server) handleEnvVars(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEnvVars(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listEnvVars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := query.Get("scopeId")

	// For user scope, use authenticated user ID (placeholder for now)
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	filter := store.EnvVarFilter{
		Scope:   scope,
		ScopeID: scopeID,
		Key:     query.Get("key"),
	}

	envVars, err := s.store.ListEnvVars(ctx, filter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Mask sensitive values
	for i := range envVars {
		if envVars[i].Sensitive {
			envVars[i].Value = "********"
		}
	}

	writeJSON(w, http.StatusOK, ListEnvVarsResponse{
		EnvVars: envVars,
		Scope:   scope,
		ScopeID: scopeID,
	})
}

func (s *Server) handleEnvVarByKey(w http.ResponseWriter, r *http.Request) {
	key := extractID(r, "/api/v1/env")

	if key == "" {
		NotFound(w, "EnvVar")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getEnvVar(w, r, key)
	case http.MethodPut:
		s.setEnvVar(w, r, key)
	case http.MethodDelete:
		s.deleteEnvVar(w, r, key)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := query.Get("scopeId")
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	envVar, err := s.store.GetEnvVar(ctx, key, scope, scopeID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Mask sensitive values
	if envVar.Sensitive {
		envVar.Value = "********"
	}

	writeJSON(w, http.StatusOK, envVar)
}

func (s *Server) setEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()

	var req SetEnvVarRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Value == "" {
		ValidationError(w, "value is required", nil)
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := req.ScopeID
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	envVar := &store.EnvVar{
		ID:          api.NewUUID(),
		Key:         key,
		Value:       req.Value,
		Scope:       scope,
		ScopeID:     scopeID,
		Description: req.Description,
		Sensitive:   req.Sensitive,
	}

	created, err := s.store.UpsertEnvVar(ctx, envVar)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Mask sensitive values in response
	if envVar.Sensitive {
		envVar.Value = "********"
	}

	writeJSON(w, http.StatusOK, SetEnvVarResponse{
		EnvVar:  envVar,
		Created: created,
	})
}

func (s *Server) deleteEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := query.Get("scopeId")
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	if err := s.store.DeleteEnvVar(ctx, key, scope, scopeID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// Secrets Endpoints
// ============================================================================

type ListSecretsResponse struct {
	Secrets []store.Secret `json:"secrets"`
	Scope   string         `json:"scope"`
	ScopeID string         `json:"scopeId"`
}

type SetSecretRequest struct {
	Value       string `json:"value"`
	Scope       string `json:"scope,omitempty"`
	ScopeID     string `json:"scopeId,omitempty"`
	Description string `json:"description,omitempty"`
}

type SetSecretResponse struct {
	Secret  *store.Secret `json:"secret"`
	Created bool          `json:"created"`
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSecrets(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := query.Get("scopeId")
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	filter := store.SecretFilter{
		Scope:   scope,
		ScopeID: scopeID,
		Key:     query.Get("key"),
	}

	secrets, err := s.store.ListSecrets(ctx, filter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListSecretsResponse{
		Secrets: secrets,
		Scope:   scope,
		ScopeID: scopeID,
	})
}

func (s *Server) handleSecretByKey(w http.ResponseWriter, r *http.Request) {
	key := extractID(r, "/api/v1/secrets")

	if key == "" {
		NotFound(w, "Secret")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getSecret(w, r, key)
	case http.MethodPut:
		s.setSecret(w, r, key)
	case http.MethodDelete:
		s.deleteSecret(w, r, key)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := query.Get("scopeId")
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	secret, err := s.store.GetSecret(ctx, key, scope, scopeID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Clear the encrypted value - never expose it
	secret.EncryptedValue = ""

	writeJSON(w, http.StatusOK, secret)
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()

	var req SetSecretRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Value == "" {
		ValidationError(w, "value is required", nil)
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := req.ScopeID
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	// TODO: In production, encrypt the value before storing
	// For now, we store it as-is (should use proper encryption)
	encryptedValue := req.Value

	secret := &store.Secret{
		ID:             api.NewUUID(),
		Key:            key,
		EncryptedValue: encryptedValue,
		Scope:          scope,
		ScopeID:        scopeID,
		Description:    req.Description,
	}

	created, err := s.store.UpsertSecret(ctx, secret)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Clear the encrypted value from response
	secret.EncryptedValue = ""

	writeJSON(w, http.StatusOK, SetSecretResponse{
		Secret:  secret,
		Created: created,
	})
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}
	scopeID := query.Get("scopeId")
	if scope == store.ScopeUser && scopeID == "" {
		scopeID = "default" // TODO: Get from auth context
	}

	if err := s.store.DeleteSecret(ctx, key, scope, scopeID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// Grove-scoped Env and Secrets Endpoints
// ============================================================================

func (s *Server) handleGroveEnvVars(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	// Verify grove exists
	_, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{
			Scope:   store.ScopeGrove,
			ScopeID: groveID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Mask sensitive values
		for i := range envVars {
			if envVars[i].Sensitive {
				envVars[i].Value = "********"
			}
		}
		writeJSON(w, http.StatusOK, ListEnvVarsResponse{
			EnvVars: envVars,
			Scope:   store.ScopeGrove,
			ScopeID: groveID,
		})
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGroveEnvVarByKey(w http.ResponseWriter, r *http.Request, groveID, key string) {
	ctx := r.Context()

	// Verify grove exists
	_, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeGrove, groveID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, envVar)

	case http.MethodPut:
		var req SetEnvVarRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Value == "" {
			ValidationError(w, "value is required", nil)
			return
		}
		envVar := &store.EnvVar{
			ID:          api.NewUUID(),
			Key:         key,
			Value:       req.Value,
			Scope:       store.ScopeGrove,
			ScopeID:     groveID,
			Description: req.Description,
			Sensitive:   req.Sensitive,
		}
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeGrove, groveID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGroveSecrets(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	// Verify grove exists
	_, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		secrets, err := s.store.ListSecrets(ctx, store.SecretFilter{
			Scope:   store.ScopeGrove,
			ScopeID: groveID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, ListSecretsResponse{
			Secrets: secrets,
			Scope:   store.ScopeGrove,
			ScopeID: groveID,
		})
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGroveSecretByKey(w http.ResponseWriter, r *http.Request, groveID, key string) {
	ctx := r.Context()

	// Verify grove exists
	_, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		secret, err := s.store.GetSecret(ctx, key, store.ScopeGrove, groveID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		secret.EncryptedValue = ""
		writeJSON(w, http.StatusOK, secret)

	case http.MethodPut:
		var req SetSecretRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Value == "" {
			ValidationError(w, "value is required", nil)
			return
		}
		secret := &store.Secret{
			ID:             api.NewUUID(),
			Key:            key,
			EncryptedValue: req.Value, // TODO: Encrypt
			Scope:          store.ScopeGrove,
			ScopeID:        groveID,
			Description:    req.Description,
		}
		created, err := s.store.UpsertSecret(ctx, secret)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		secret.EncryptedValue = ""
		writeJSON(w, http.StatusOK, SetSecretResponse{Secret: secret, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteSecret(ctx, key, store.ScopeGrove, groveID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// ============================================================================
// RuntimeHost-scoped Env and Secrets Endpoints
// ============================================================================

func (s *Server) handleHostEnvVars(w http.ResponseWriter, r *http.Request, hostID string) {
	ctx := r.Context()

	// Verify host exists
	_, err := s.store.GetRuntimeHost(ctx, hostID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeHost")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{
			Scope:   store.ScopeRuntimeHost,
			ScopeID: hostID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		for i := range envVars {
			if envVars[i].Sensitive {
				envVars[i].Value = "********"
			}
		}
		writeJSON(w, http.StatusOK, ListEnvVarsResponse{
			EnvVars: envVars,
			Scope:   store.ScopeRuntimeHost,
			ScopeID: hostID,
		})
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleHostEnvVarByKey(w http.ResponseWriter, r *http.Request, hostID, key string) {
	ctx := r.Context()

	// Verify host exists
	_, err := s.store.GetRuntimeHost(ctx, hostID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeHost")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeRuntimeHost, hostID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, envVar)

	case http.MethodPut:
		var req SetEnvVarRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Value == "" {
			ValidationError(w, "value is required", nil)
			return
		}
		envVar := &store.EnvVar{
			ID:          api.NewUUID(),
			Key:         key,
			Value:       req.Value,
			Scope:       store.ScopeRuntimeHost,
			ScopeID:     hostID,
			Description: req.Description,
			Sensitive:   req.Sensitive,
		}
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeRuntimeHost, hostID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleHostSecrets(w http.ResponseWriter, r *http.Request, hostID string) {
	ctx := r.Context()

	// Verify host exists
	_, err := s.store.GetRuntimeHost(ctx, hostID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeHost")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		secrets, err := s.store.ListSecrets(ctx, store.SecretFilter{
			Scope:   store.ScopeRuntimeHost,
			ScopeID: hostID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, ListSecretsResponse{
			Secrets: secrets,
			Scope:   store.ScopeRuntimeHost,
			ScopeID: hostID,
		})
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleHostSecretByKey(w http.ResponseWriter, r *http.Request, hostID, key string) {
	ctx := r.Context()

	// Verify host exists
	_, err := s.store.GetRuntimeHost(ctx, hostID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeHost")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		secret, err := s.store.GetSecret(ctx, key, store.ScopeRuntimeHost, hostID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		secret.EncryptedValue = ""
		writeJSON(w, http.StatusOK, secret)

	case http.MethodPut:
		var req SetSecretRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Value == "" {
			ValidationError(w, "value is required", nil)
			return
		}
		secret := &store.Secret{
			ID:             api.NewUUID(),
			Key:            key,
			EncryptedValue: req.Value, // TODO: Encrypt
			Scope:          store.ScopeRuntimeHost,
			ScopeID:        hostID,
			Description:    req.Description,
		}
		created, err := s.store.UpsertSecret(ctx, secret)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		secret.EncryptedValue = ""
		writeJSON(w, http.StatusOK, SetSecretResponse{Secret: secret, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteSecret(ctx, key, store.ScopeRuntimeHost, hostID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// ============================================================================
// Helpers
// ============================================================================

// resolveRuntimeHost determines which runtime host should run the agent.
// It checks the explicitly specified host, the grove's default, or returns an error
// with available alternatives if none is available.
// Returns the runtime host ID or an error (after writing the HTTP error response).
func (s *Server) resolveRuntimeHost(ctx context.Context, w http.ResponseWriter, requestedHostID string, grove *store.Grove) (string, error) {
	// Get available hosts for this grove (online hosts that are contributors)
	availableHosts, err := s.getAvailableHostsForGrove(ctx, grove.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	// Convert to summary for error responses
	hostSummaries := make([]RuntimeHostSummary, len(availableHosts))
	for i, h := range availableHosts {
		hostSummaries[i] = RuntimeHostSummary{
			ID:     h.ID,
			Name:   h.Name,
			Status: h.Status,
		}
	}

	// Case 1: Explicit runtime host specified
	if requestedHostID != "" {
		// Check if the requested host is available
		for _, h := range availableHosts {
			if h.ID == requestedHostID {
				return requestedHostID, nil
			}
		}
		// Requested host not available
		RuntimeHostUnavailable(w, requestedHostID, hostSummaries)
		return "", store.ErrNotFound
	}

	// Case 2: Use grove's default runtime host
	if grove.DefaultRuntimeHostID != "" {
		// Check if the default host is still available
		for _, h := range availableHosts {
			if h.ID == grove.DefaultRuntimeHostID {
				return grove.DefaultRuntimeHostID, nil
			}
		}
		// Default host is not available
		if len(availableHosts) > 0 {
			NoRuntimeHost(w, "Default runtime host is unavailable; specify an alternative", hostSummaries)
		} else {
			NoRuntimeHost(w, "Default runtime host is unavailable and no alternatives found", hostSummaries)
		}
		return "", store.ErrNotFound
	}

	// Case 3: No default and no explicit host
	if len(availableHosts) == 0 {
		NoRuntimeHost(w, "No runtime hosts available for this grove; register a runtime host first", hostSummaries)
		return "", store.ErrNotFound
	}

	// No explicit host and no default - this shouldn't happen if registration sets defaults properly
	NoRuntimeHost(w, "No runtime host specified and grove has no default; specify runtimeHostId or set a default", hostSummaries)
	return "", store.ErrNotFound
}

// getAvailableHostsForGrove returns online runtime hosts that are contributors to the grove.
func (s *Server) getAvailableHostsForGrove(ctx context.Context, groveID string) ([]store.RuntimeHost, error) {
	// Get contributors for this grove
	contributors, err := s.store.GetGroveContributors(ctx, groveID)
	if err != nil {
		return nil, err
	}

	// Filter to online hosts and fetch their full details
	var availableHosts []store.RuntimeHost
	for _, contrib := range contributors {
		if contrib.Status == store.HostStatusOnline {
			host, err := s.store.GetRuntimeHost(ctx, contrib.HostID)
			if err != nil {
				continue // Skip hosts we can't fetch
			}
			if host.Status == store.HostStatusOnline {
				availableHosts = append(availableHosts, *host)
			}
		}
	}

	return availableHosts, nil
}
