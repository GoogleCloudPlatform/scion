package hub

import (
	"context"
	"log/slog"
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
	ConnectedBrokers int `json:"connectedBrokers,omitempty"`
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
	if brokerResult, err := s.store.ListRuntimeBrokers(r.Context(), store.RuntimeBrokerFilter{Status: store.BrokerStatusOnline}, store.ListOptions{Limit: 1}); err == nil {
		stats.ConnectedBrokers = brokerResult.TotalCount
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
	RuntimeBrokerID string            `json:"runtimeBrokerId,omitempty"` // Optional: uses grove's default if not specified
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
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
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

	// Enrich agents with grove and broker names
	s.enrichAgents(ctx, result.Items)

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

	// Resolve the runtime broker
	runtimeBrokerID, err := s.resolveRuntimeBroker(ctx, w, req.RuntimeBrokerID, grove)
	if err != nil {
		// Error response already written by resolveRuntimeBroker
		return
	}

	// Resolve template if specified - the client may pass either a template ID or name
	var resolvedTemplate *store.Template
	if req.Template != "" {
		resolvedTemplate, err = s.resolveTemplate(ctx, req.Template, req.GroveID)
		if err != nil && err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
		// If template was requested but not found, return an error
		if resolvedTemplate == nil {
			NotFound(w, "Template")
			return
		}
	}

	// Create agent
	agent := &store.Agent{
		ID:            api.NewUUID(),
		AgentID:       api.Slugify(req.Name),
		Name:          req.Name,
		Template:      req.Template,
		GroveID:       req.GroveID,
		RuntimeBrokerID: runtimeBrokerID,
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
			Harness: s.getHarnessFromTemplate(resolvedTemplate, req.Template),
			Task:    req.Task,
		}
	} else {
		agent.Detached = true
		// Store task even when no config override is provided
		agent.AppliedConfig = &store.AgentAppliedConfig{
			Harness: s.getHarnessFromTemplate(resolvedTemplate, req.Template),
			Task:    req.Task,
		}
	}

	// Populate template ID and hash if template was resolved
	if resolvedTemplate != nil && agent.AppliedConfig != nil {
		agent.AppliedConfig.TemplateID = resolvedTemplate.ID
		agent.AppliedConfig.TemplateHash = resolvedTemplate.ContentHash
	}

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If a dispatcher is available (co-located runtime broker) and a task was provided,
	// dispatch the agent to start it immediately.
	// Without a task, this is a "create only" operation (e.g., scion create).
	var warnings []string
	if req.Task != "" {
		if dispatcher := s.GetDispatcher(); dispatcher != nil {
			if err := dispatcher.DispatchAgentCreate(ctx, agent); err != nil {
				// Log the error but don't fail the request - agent is created in Hub
				warnings = append(warnings, "Failed to dispatch to runtime broker: "+err.Error())
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

	// Enrich agent with grove and broker names for display
	s.enrichAgent(ctx, agent, grove, nil)

	writeJSON(w, http.StatusCreated, CreateAgentResponse{
		Agent:    agent,
		Warnings: warnings,
	})
}

// enrichAgents populates Grove and RuntimeBrokerName fields for a slice of agents.
// This provides human-readable names from the related IDs for display purposes.
func (s *Server) enrichAgents(ctx context.Context, agents []store.Agent) {
	if len(agents) == 0 {
		return
	}

	// Collect unique grove and broker IDs
	groveIDs := make(map[string]struct{})
	brokerIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.GroveID != "" {
			groveIDs[a.GroveID] = struct{}{}
		}
		if a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
	}

	// Fetch groves
	groveNames := make(map[string]string)
	for id := range groveIDs {
		if grove, err := s.store.GetGrove(ctx, id); err == nil {
			groveNames[id] = grove.Name
		}
	}

	// Fetch brokers
	brokerInfo := make(map[string]*store.RuntimeBroker)
	for id := range brokerIDs {
		if broker, err := s.store.GetRuntimeBroker(ctx, id); err == nil {
			brokerInfo[id] = broker
		}
	}

	// Enrich agents
	for i := range agents {
		if name, ok := groveNames[agents[i].GroveID]; ok {
			agents[i].Grove = name
		}
		if broker, ok := brokerInfo[agents[i].RuntimeBrokerID]; ok {
			agents[i].RuntimeBrokerName = broker.Name
			// Also populate Runtime if not already set (from broker's active profile)
			if agents[i].Runtime == "" && len(broker.Profiles) > 0 {
				for _, p := range broker.Profiles {
					if p.Available {
						agents[i].Runtime = p.Type
						break
					}
				}
			}
		}
	}
}

// enrichAgent populates Grove and RuntimeBrokerName fields for a single agent.
// grove and broker parameters are optional pre-fetched values to avoid redundant lookups.
func (s *Server) enrichAgent(ctx context.Context, agent *store.Agent, grove *store.Grove, broker *store.RuntimeBroker) {
	if agent == nil {
		return
	}

	// Populate grove name
	if grove != nil {
		agent.Grove = grove.Name
	} else if agent.GroveID != "" {
		if g, err := s.store.GetGrove(ctx, agent.GroveID); err == nil {
			agent.Grove = g.Name
		}
	}

	// Populate broker info
	if broker != nil {
		agent.RuntimeBrokerName = broker.Name
		if agent.Runtime == "" && len(broker.Profiles) > 0 {
			for _, p := range broker.Profiles {
				if p.Available {
					agent.Runtime = p.Type
					break
				}
			}
		}
	} else if agent.RuntimeBrokerID != "" {
		b, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
		if err != nil {
			slog.Debug("failed to get runtime broker for enrichment", "brokerID", agent.RuntimeBrokerID, "error", err)
		} else {
			agent.RuntimeBrokerName = b.Name
			slog.Debug("enriched agent with broker name", "agentID", agent.AgentID, "brokerName", b.Name)
			if agent.Runtime == "" && len(b.Profiles) > 0 {
				for _, p := range b.Profiles {
					if p.Available {
						agent.Runtime = p.Type
						break
					}
				}
			}
		}
	}
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}

	// Handle PTY WebSocket connections
	if action == "pty" && isWebSocketUpgrade(r) {
		s.handleAgentPTY(w, r)
		return
	}

	// Handle workspace routes (supports GET for status and POST for sync operations)
	if action == "workspace" || strings.HasPrefix(action, "workspace/") {
		// Require user authentication for workspace operations
		if GetUserIdentityFromContext(r.Context()) == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user authentication", nil)
			return
		}
		// Extract workspace sub-action (sync-from, sync-to, sync-to/finalize)
		workspaceAction := strings.TrimPrefix(action, "workspace")
		workspaceAction = strings.TrimPrefix(workspaceAction, "/")
		s.handleWorkspaceRoutes(w, r, id, workspaceAction)
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
	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enrich agent with grove and broker names
	s.enrichAgent(ctx, agent, nil, nil)

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

// checkBrokerAvailability verifies the agent's runtime broker is reachable.
// Returns true if the broker is available (or no broker is assigned).
// Returns false and writes a 503 error response if the broker is offline.
func (s *Server) checkBrokerAvailability(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	if agent.RuntimeBrokerID == "" {
		return true
	}

	// Check real-time WebSocket connectivity first (no DB query needed)
	if s.controlChannel != nil && s.controlChannel.IsConnected(agent.RuntimeBrokerID) {
		return true
	}

	// Fall back to DB status check (covers co-located mode where there's no WebSocket)
	broker, err := s.store.GetRuntimeBroker(r.Context(), agent.RuntimeBrokerID)
	if err != nil {
		slog.Warn("Failed to check broker status", "brokerID", agent.RuntimeBrokerID, "error", err)
		// If we can't verify, let it through rather than blocking
		return true
	}

	if broker.Status == store.BrokerStatusOnline {
		return true
	}

	RuntimeBrokerUnavailable(w, agent.RuntimeBrokerID, nil)
	return false
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"

	// Get the agent to dispatch deletion to runtime broker
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify broker is reachable before deleting to avoid orphaned containers
	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// If a dispatcher is available, dispatch the deletion to the runtime broker
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeBrokerID != "" {
		if err := dispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch); err != nil {
			// Log but continue - the agent record should still be deleted from hub
			// The runtime broker deletion is best-effort
			// (agent may already be stopped/deleted on the broker)
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

	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// If a dispatcher is available, dispatch the message to the runtime broker
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeBrokerID != "" {
		if err := dispatcher.DispatchAgentMessage(ctx, agent, req.Message, req.Interrupt); err != nil {
			RuntimeError(w, "Failed to send message to runtime broker: "+err.Error())
			return
		}
	} else {
		// No dispatcher available
		RuntimeError(w, "No runtime broker dispatcher available for this agent")
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

	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	var newStatus string
	var dispatchErr error

	// If a dispatcher is available, dispatch the operation to the runtime broker
	dispatcher := s.GetDispatcher()

	switch action {
	case "start":
		newStatus = store.AgentStatusRunning
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent)
		}
	case "stop":
		newStatus = store.AgentStatusStopped
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
		}
	case "restart":
		newStatus = store.AgentStatusRunning
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			dispatchErr = dispatcher.DispatchAgentRestart(ctx, agent)
		}
	}

	// If dispatch failed, return error
	if dispatchErr != nil {
		RuntimeError(w, "Failed to dispatch to runtime broker: "+dispatchErr.Error())
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
	ID       string              `json:"id,omitempty"` // Client-provided grove ID
	Name     string              `json:"name"`
	GitRemote string              `json:"gitRemote"`
	Path     string              `json:"path,omitempty"`
	BrokerID string              `json:"brokerId,omitempty"` // Link to existing broker (two-phase flow)
	Broker   *RegisterBrokerInfo `json:"broker,omitempty"`   // DEPRECATED: Use BrokerID with two-phase registration
	Profiles []string            `json:"profiles,omitempty"`
	Labels   map[string]string   `json:"labels,omitempty"`
}

type RegisterBrokerInfo struct {
	ID           string                  `json:"id,omitempty"`
	Name         string                  `json:"name"`
	Version      string                  `json:"version,omitempty"`
	Capabilities *store.BrokerCapabilities `json:"capabilities,omitempty"`
	Profiles     []store.BrokerProfile     `json:"profiles,omitempty"`
}

type RegisterGroveResponse struct {
	Grove     *store.Grove       `json:"grove"`
	Broker *store.RuntimeBroker `json:"broker,omitempty"`
	Created   bool               `json:"created"`
	BrokerToken string             `json:"brokerToken,omitempty"` // DEPRECATED: use two-phase registration
	SecretKey string             `json:"secretKey,omitempty"` // DEPRECATED: secrets only from /brokers/join
}

// AddProviderRequest is the request for adding a broker as a grove provider.
type AddProviderRequest struct {
	BrokerID  string `json:"brokerId"`
	LocalPath string `json:"localPath,omitempty"`
}

// AddProviderResponse is the response after adding a provider.
type AddProviderResponse struct {
	Provider *store.GroveProvider `json:"provider"`
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
		BrokerID:          query.Get("brokerId"),
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

		// Auto-link brokers that have auto_provide enabled
		autoProvideTrue := true
		autoProviders, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{
			AutoProvide: &autoProvideTrue,
		}, store.ListOptions{})
		if err == nil {
			for _, autoBroker := range autoProviders.Items {
				provider := &store.GroveProvider{
					GroveID:    grove.ID,
					BrokerID:   autoBroker.ID,
					BrokerName: autoBroker.Name,
					Status:     autoBroker.Status,
					LinkedBy:   "auto-provide",
				}
				if addErr := s.store.AddGroveProvider(ctx, provider); addErr != nil {
					util.Debugf("Warning: failed to auto-link broker %s to grove %s: %v", autoBroker.Name, grove.ID, addErr)
				}
			}
		} else {
			util.Debugf("Warning: failed to query auto-provide brokers: %v", err)
		}
	}

	// Handle broker linking - two paths:
	// 1. New flow (preferred): BrokerID provided - link to existing broker (no secret generation)
	// 2. Deprecated flow: Broker object provided - create/update broker AND generate secret
	var broker *store.RuntimeBroker
	var brokerToken string
	var secretKey string

	if req.BrokerID != "" {
		// NEW FLOW: Link to existing broker registered via two-phase /brokers + /brokers/join
		existingBroker, err := s.store.GetRuntimeBroker(ctx, req.BrokerID)
		if err != nil {
			if err == store.ErrNotFound {
				ValidationError(w, "brokerId not found: broker must be registered via POST /brokers and /brokers/join first", map[string]interface{}{
					"field":  "brokerId",
					"brokerId": req.BrokerID,
				})
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		broker = existingBroker

		// Add as grove provider
		provider := &store.GroveProvider{
			GroveID:    grove.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  req.Path,
			Status:     broker.Status,
		}

		if err := s.store.AddGroveProvider(ctx, provider); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Set as default runtime broker if grove doesn't have one
		if grove.DefaultRuntimeBrokerID == "" {
			grove.DefaultRuntimeBrokerID = broker.ID
			if err := s.store.UpdateGrove(ctx, grove); err != nil {
				util.Debugf("Warning: failed to set default runtime broker: %v", err)
			}
		}

		// No secret returned - broker already has credentials from /brokers/join
	} else if req.Broker != nil {
		// DEPRECATED FLOW: Embedded broker registration (creates broker and generates secret)
		util.Debugf("Warning: embedded Broker field in grove registration is deprecated. Use two-phase registration: POST /brokers + POST /brokers/join, then pass brokerId")

		brokerID := req.Broker.ID

		// Try to find existing broker by ID first, then by name
		var existingBroker *store.RuntimeBroker
		var err error

		if brokerID != "" {
			existingBroker, err = s.store.GetRuntimeBroker(ctx, brokerID)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		// If not found by ID, try to find by name (prevents duplicate brokers with same hostname)
		if existingBroker == nil && req.Broker.Name != "" {
			existingBroker, err = s.store.GetRuntimeBrokerByName(ctx, req.Broker.Name)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		if existingBroker != nil {
			// Update existing broker
			broker = existingBroker
			broker.Name = req.Broker.Name
			broker.Slug = api.Slugify(req.Broker.Name)
			broker.Version = req.Broker.Version
			broker.Status = store.BrokerStatusOnline
			broker.ConnectionState = "connected"
			broker.Capabilities = req.Broker.Capabilities
			broker.Profiles = req.Broker.Profiles

			if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			// Create new broker
			if brokerID == "" {
				brokerID = api.NewUUID()
			}

			broker = &store.RuntimeBroker{
				ID:              brokerID,
				Name:            req.Broker.Name,
				Slug:            api.Slugify(req.Broker.Name),
				Version:         req.Broker.Version,
				Status:          store.BrokerStatusOnline,
				ConnectionState: "connected",
				Capabilities:    req.Broker.Capabilities,
				Profiles:        req.Broker.Profiles,
			}

			if err := s.store.CreateRuntimeBroker(ctx, broker); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		// Add as grove provider
		provider := &store.GroveProvider{
			GroveID:    grove.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  req.Path, // Filesystem path to the grove on this broker
			Status:     store.BrokerStatusOnline,
		}

		if err := s.store.AddGroveProvider(ctx, provider); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Set as default runtime broker if grove doesn't have one
		// (first broker to register becomes the default)
		if grove.DefaultRuntimeBrokerID == "" {
			grove.DefaultRuntimeBrokerID = broker.ID
			if err := s.store.UpdateGrove(ctx, grove); err != nil {
				// Log but don't fail - the broker is registered, default can be set later
				util.Debugf("Warning: failed to set default runtime broker: %v", err)
			}
		}

		// Generate HMAC credentials for the broker if broker auth service is available
		// (deprecated flow only - new flow gets secrets from /brokers/join)
		if s.brokerAuthService != nil {
			var err error
			secretKey, err = s.brokerAuthService.GenerateAndStoreSecret(ctx, broker.ID)
			if err != nil {
				// Log but don't fail - broker is registered, can complete join later
				util.Debugf("Warning: failed to generate broker secret: %v", err)
				// Fall back to simple token for backward compatibility
				brokerToken = "broker_" + api.NewShortID() + "_" + api.NewShortID()
			}
		} else {
			// No broker auth service - use simple token
			brokerToken = "broker_" + api.NewShortID() + "_" + api.NewShortID()
		}
	}

	writeJSON(w, http.StatusOK, RegisterGroveResponse{
		Grove:     grove,
		Broker:    broker,
		Created:   created,
		BrokerToken: brokerToken,
		SecretKey: secretKey,
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

	// Check for nested /providers path
	if strings.HasPrefix(subPath, "providers") {
		providerPath := strings.TrimPrefix(subPath, "providers")
		providerPath = strings.TrimPrefix(providerPath, "/")
		s.handleGroveProviders(w, r, groveID, providerPath)
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
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
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

	// Enrich agents with grove and broker names
	s.enrichAgents(ctx, result.Items)

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

	// Get grove to access its configuration (including default runtime broker)
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve the runtime broker
	runtimeBrokerID, err := s.resolveRuntimeBroker(ctx, w, req.RuntimeBrokerID, grove)
	if err != nil {
		// Error response already written by resolveRuntimeBroker
		return
	}

	// Resolve template if specified - the client may pass either a template ID or name
	var resolvedTemplate *store.Template
	if req.Template != "" {
		resolvedTemplate, err = s.resolveTemplate(ctx, req.Template, groveID)
		if err != nil && err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
		// If template was requested but not found, return an error
		if resolvedTemplate == nil {
			NotFound(w, "Template")
			return
		}
	}

	// Create agent
	agent := &store.Agent{
		ID:            api.NewUUID(),
		AgentID:       api.Slugify(req.Name),
		Name:          req.Name,
		Template:      req.Template,
		GroveID:       groveID,
		RuntimeBrokerID: runtimeBrokerID,
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
			Harness: s.getHarnessFromTemplate(resolvedTemplate, req.Template),
			Task:    req.Task,
		}
	} else {
		agent.Detached = true
		// Store task even when no config override is provided
		agent.AppliedConfig = &store.AgentAppliedConfig{
			Harness: s.getHarnessFromTemplate(resolvedTemplate, req.Template),
			Task:    req.Task,
		}
	}

	// Populate template ID and hash if template was resolved
	if resolvedTemplate != nil && agent.AppliedConfig != nil {
		agent.AppliedConfig.TemplateID = resolvedTemplate.ID
		agent.AppliedConfig.TemplateHash = resolvedTemplate.ContentHash
	}

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If a dispatcher is available (co-located runtime broker) and a task was provided,
	// dispatch the agent to start it immediately.
	// Without a task, this is a "create only" operation (e.g., scion create).
	var warnings []string
	if req.Task != "" {
		if dispatcher := s.GetDispatcher(); dispatcher != nil {
			if err := dispatcher.DispatchAgentCreate(ctx, agent); err != nil {
				// Log the error but don't fail the request - agent is created in Hub
				warnings = append(warnings, "Failed to dispatch to runtime broker: "+err.Error())
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

	// Enrich agent with grove and broker names for display
	s.enrichAgent(ctx, agent, grove, nil)

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

	// Enrich agent with grove and broker names
	s.enrichAgent(ctx, agent, nil, nil)

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

	// Verify broker is reachable before deleting to avoid orphaned containers
	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// If a dispatcher is available, dispatch the deletion to the runtime broker
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeBrokerID != "" {
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
// RuntimeBroker Endpoints
// ============================================================================

type ListRuntimeBrokersResponse struct {
	Brokers []store.RuntimeBroker `json:"brokers"`
	NextCursor string              `json:"nextCursor,omitempty"`
	TotalCount int                 `json:"totalCount"`
}

// RuntimeBrokerWithProvider extends RuntimeBroker with grove-specific provider data.
// This is returned when listing brokers filtered by groveId, providing the local path
// for the grove on each broker.
type RuntimeBrokerWithProvider struct {
	store.RuntimeBroker
	LocalPath string `json:"localPath,omitempty"` // Filesystem path to the grove on this broker
}

// ListRuntimeBrokersWithProviderResponse is returned when filtering by groveId.
type ListRuntimeBrokersWithProviderResponse struct {
	Brokers []RuntimeBrokerWithProvider `json:"brokers"`
	NextCursor string                    `json:"nextCursor,omitempty"`
	TotalCount int                       `json:"totalCount"`
}

func (s *Server) handleRuntimeBrokers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRuntimeBrokers(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listRuntimeBrokers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	groveID := query.Get("groveId")
	filter := store.RuntimeBrokerFilter{
		Status:  query.Get("status"),
		GroveID: groveID,
		Name:    query.Get("name"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListRuntimeBrokers(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If filtering by groveId, include grove-specific provider data (like localPath)
	if groveID != "" {
		// Get provider data for this grove to include localPath
		providers, err := s.store.GetGroveProviders(ctx, groveID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Build a map of brokerId -> localPath for quick lookup
		brokerLocalPaths := make(map[string]string)
		for _, p := range providers {
			brokerLocalPaths[p.BrokerID] = p.LocalPath
		}

		// Build extended broker list with provider data
		extendedBrokers := make([]RuntimeBrokerWithProvider, 0, len(result.Items))
		for _, broker := range result.Items {
			extendedBrokers = append(extendedBrokers, RuntimeBrokerWithProvider{
				RuntimeBroker: broker,
				LocalPath:   brokerLocalPaths[broker.ID],
			})
		}

		writeJSON(w, http.StatusOK, ListRuntimeBrokersWithProviderResponse{
			Brokers:    extendedBrokers,
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		})
		return
	}

	writeJSON(w, http.StatusOK, ListRuntimeBrokersResponse{
		Brokers:    result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) handleRuntimeBrokerRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract broker ID and remaining path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/runtime-brokers/")
	if path == "" {
		NotFound(w, "RuntimeBroker")
		return
	}

	// Parse the broker ID and subpath
	parts := strings.SplitN(path, "/", 2)
	brokerID := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Check for nested /env path
	if strings.HasPrefix(subPath, "env") {
		envPath := strings.TrimPrefix(subPath, "env")
		envPath = strings.TrimPrefix(envPath, "/")
		if envPath == "" {
			s.handleBrokerEnvVars(w, r, brokerID)
		} else {
			s.handleBrokerEnvVarByKey(w, r, brokerID, envPath)
		}
		return
	}

	// Check for nested /secrets path
	if strings.HasPrefix(subPath, "secrets") {
		secretPath := strings.TrimPrefix(subPath, "secrets")
		secretPath = strings.TrimPrefix(secretPath, "/")
		if secretPath == "" {
			s.handleBrokerSecrets(w, r, brokerID)
		} else {
			s.handleBrokerSecretByKey(w, r, brokerID, secretPath)
		}
		return
	}

	// Delegate to the original handler for other operations
	s.handleRuntimeBrokerByIDInternal(w, r, brokerID, subPath)
}

func (s *Server) handleRuntimeBrokerByIDInternal(w http.ResponseWriter, r *http.Request, id, subPath string) {
	if id == "" {
		NotFound(w, "RuntimeBroker")
		return
	}

	// Handle heartbeat action
	if subPath == "heartbeat" && r.Method == http.MethodPost {
		s.handleBrokerHeartbeat(w, r, id)
		return
	}

	// Handle groves action
	if subPath == "groves" && r.Method == http.MethodGet {
		s.getBrokerGroves(w, r, id)
		return
	}

	// Only handle if no subpath (direct resource)
	if subPath != "" {
		NotFound(w, "RuntimeBroker resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRuntimeBroker(w, r, id)
	case http.MethodPatch:
		s.updateRuntimeBroker(w, r, id)
	case http.MethodDelete:
		s.deleteRuntimeBroker(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleRuntimeBrokerByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/runtime-brokers")

	if id == "" {
		NotFound(w, "RuntimeBroker")
		return
	}

	if action == "heartbeat" && r.Method == http.MethodPost {
		s.handleBrokerHeartbeat(w, r, id)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRuntimeBroker(w, r, id)
	case http.MethodPatch:
		s.updateRuntimeBroker(w, r, id)
	case http.MethodDelete:
		s.deleteRuntimeBroker(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	broker, err := s.store.GetRuntimeBroker(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, broker)
}

func (s *Server) updateRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, id)
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
		broker.Name = updates.Name
	}
	if updates.Labels != nil {
		broker.Labels = updates.Labels
	}

	if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, broker)
}

func (s *Server) deleteRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Get the user who is performing this action for audit logging
	var actorID string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		actorID = user.ID()
	}

	// Get broker info before deletion for audit logging
	broker, _ := s.store.GetRuntimeBroker(ctx, id)
	brokerName := ""
	if broker != nil {
		brokerName = broker.Name
	}

	if err := s.store.DeleteRuntimeBroker(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Log the deregistration event
	LogDeregisterEvent(ctx, s.auditLogger, id, brokerName, actorID, getClientIP(r))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBrokerHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	var heartbeat struct {
		Status string `json:"status"`
	}

	if err := readJSON(r, &heartbeat); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if err := s.store.UpdateRuntimeBrokerHeartbeat(r.Context(), id, heartbeat.Status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// BrokerGroveInfo describes a grove from a broker's perspective.
type BrokerGroveInfo struct {
	GroveID    string `json:"groveId"`
	GroveName  string `json:"groveName"`
	GitRemote  string `json:"gitRemote,omitempty"`
	AgentCount int    `json:"agentCount"`
	LocalPath  string `json:"localPath,omitempty"`
}

// ListBrokerGrovesResponse is the response for listing groves a broker provides.
type ListBrokerGrovesResponse struct {
	Groves []BrokerGroveInfo `json:"groves"`
}

func (s *Server) getBrokerGroves(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Get all groves this broker provides for
	providers, err := s.store.GetBrokerGroves(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Build response with grove details
	groves := make([]BrokerGroveInfo, 0, len(providers))
	for _, p := range providers {
		info := BrokerGroveInfo{
			GroveID:   p.GroveID,
			LocalPath: p.LocalPath,
		}

		// Fetch grove details for name and git remote
		if grove, err := s.store.GetGrove(ctx, p.GroveID); err == nil {
			info.GroveName = grove.Name
			info.GitRemote = grove.GitRemote
		}

		// Count agents for this grove on this broker
		agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{
			GroveID:         p.GroveID,
			RuntimeBrokerID: brokerID,
		}, store.ListOptions{Limit: 0})
		if err == nil {
			info.AgentCount = agentResult.TotalCount
		}

		groves = append(groves, info)
	}

	writeJSON(w, http.StatusOK, ListBrokerGrovesResponse{Groves: groves})
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
// Grove Providers Endpoints
// ============================================================================

// handleGroveProviders handles provider operations for a grove.
// Path: /api/v1/groves/{groveId}/providers[/{brokerId}]
func (s *Server) handleGroveProviders(w http.ResponseWriter, r *http.Request, groveID, subPath string) {
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

	// No subpath - collection endpoint
	if subPath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listGroveProviders(w, r, groveID)
		case http.MethodPost:
			s.addGroveProvider(w, r, groveID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// subPath is the brokerId - resource endpoint
	brokerID := subPath
	switch r.Method {
	case http.MethodDelete:
		s.removeGroveProvider(w, r, groveID, brokerID)
	default:
		MethodNotAllowed(w)
	}
}

// listGroveProviders returns all providers for a grove.
func (s *Server) listGroveProviders(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	providers, err := s.store.GetGroveProviders(ctx, groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": providers,
	})
}

// addGroveProvider adds a broker as a provider to a grove.
func (s *Server) addGroveProvider(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	var req AddProviderRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.BrokerID == "" {
		ValidationError(w, "brokerId is required", nil)
		return
	}

	// Verify broker exists
	broker, err := s.store.GetRuntimeBroker(ctx, req.BrokerID)
	if err != nil {
		if err == store.ErrNotFound {
			ValidationError(w, "brokerId not found", map[string]interface{}{
				"field":  "brokerId",
				"brokerId": req.BrokerID,
			})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Get the user who is performing this action
	var linkedBy string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		linkedBy = user.ID()
	}

	// Create provider record
	provider := &store.GroveProvider{
		GroveID:    groveID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  req.LocalPath,
		Status:     broker.Status,
		LinkedBy:   linkedBy,
	}

	if err := s.store.AddGroveProvider(ctx, provider); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Get the grove to check if we should set default runtime broker
	grove, err := s.store.GetGrove(ctx, groveID)
	if err == nil && grove.DefaultRuntimeBrokerID == "" {
		grove.DefaultRuntimeBrokerID = broker.ID
		_ = s.store.UpdateGrove(ctx, grove)
	}

	// Log the link event
	LogLinkEvent(ctx, s.auditLogger, broker.ID, broker.Name, groveID, linkedBy, getClientIP(r))

	writeJSON(w, http.StatusCreated, AddProviderResponse{
		Provider: provider,
	})
}

// removeGroveProvider removes a broker from a grove's providers.
func (s *Server) removeGroveProvider(w http.ResponseWriter, r *http.Request, groveID, brokerID string) {
	ctx := r.Context()

	// Get the user who is performing this action for audit logging
	var actorID string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		actorID = user.ID()
	}

	if err := s.store.RemoveGroveProvider(ctx, groveID, brokerID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Log the unlink event
	LogUnlinkEvent(ctx, s.auditLogger, brokerID, groveID, actorID, getClientIP(r))

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// RuntimeBroker-scoped Env and Secrets Endpoints
// ============================================================================

func (s *Server) handleBrokerEnvVars(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
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
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleBrokerEnvVarByKey(w http.ResponseWriter, r *http.Request, brokerID, key string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID)
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
			Scope:       store.ScopeRuntimeBroker,
			ScopeID:     brokerID,
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
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleBrokerSecrets(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		secrets, err := s.store.ListSecrets(ctx, store.SecretFilter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, ListSecretsResponse{
			Secrets: secrets,
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleBrokerSecretByKey(w http.ResponseWriter, r *http.Request, brokerID, key string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		secret, err := s.store.GetSecret(ctx, key, store.ScopeRuntimeBroker, brokerID)
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
			Scope:          store.ScopeRuntimeBroker,
			ScopeID:        brokerID,
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
		if err := s.store.DeleteSecret(ctx, key, store.ScopeRuntimeBroker, brokerID); err != nil {
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

// resolveTemplate looks up a template by ID or name/slug.
// It tries: 1) by ID, 2) by slug in grove scope, 3) by slug in global scope.
// Returns nil if not found, or an error for actual failures.
func (s *Server) resolveTemplate(ctx context.Context, templateRef, groveID string) (*store.Template, error) {
	// Try looking up by ID first (the CLI typically resolves names to IDs)
	template, err := s.store.GetTemplate(ctx, templateRef)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	if template != nil {
		return template, nil
	}

	// Try by slug/name within grove scope
	template, err = s.store.GetTemplateBySlug(ctx, templateRef, "grove", groveID)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	if template != nil {
		return template, nil
	}

	// Try global scope
	template, err = s.store.GetTemplateBySlug(ctx, templateRef, "global", "")
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	return template, nil
}

// getHarnessFromTemplate returns the harness type from a resolved template,
// or the fallback value if no template was resolved.
func (s *Server) getHarnessFromTemplate(template *store.Template, fallback string) string {
	if template != nil && template.Harness != "" {
		return template.Harness
	}
	return fallback
}

// resolveRuntimeBroker determines which runtime broker should run the agent.
// Priority order:
//  1. Explicitly specified broker (requestedBrokerID) - verified to be a provider
//  2. Grove's default runtime broker - verified to be available (online)
//  3. Single provider (any status) - used automatically
//  4. Multiple providers with online brokers - returns error requiring explicit selection
//  5. No providers - returns error
// Returns the runtime broker ID or an error (after writing the HTTP error response).
func (s *Server) resolveRuntimeBroker(ctx context.Context, w http.ResponseWriter, requestedBrokerID string, grove *store.Grove) (string, error) {
	// Get ALL providers for this grove (regardless of status)
	allProviders, err := s.store.GetGroveProviders(ctx, grove.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	// Get available (online) brokers for fallback logic
	availableBrokers, err := s.getAvailableBrokersForGrove(ctx, grove.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	// Convert to summary for error responses
	brokerSummaries := make([]RuntimeBrokerSummary, len(availableBrokers))
	for i, h := range availableBrokers {
		brokerSummaries[i] = RuntimeBrokerSummary{
			ID:     h.ID,
			Name:   h.Name,
			Status: h.Status,
		}
	}

	// Case 1: Explicit runtime broker specified
	if requestedBrokerID != "" {
		// Check if the requested broker is a provider to this grove (by ID, Name, or Slug)
		for _, p := range allProviders {
			if p.BrokerID == requestedBrokerID || p.BrokerName == requestedBrokerID {
				return p.BrokerID, nil
			}
			// Fetch broker to check slug
			broker, err := s.store.GetRuntimeBroker(ctx, p.BrokerID)
			if err == nil && broker.Slug == requestedBrokerID {
				return broker.ID, nil
			}
		}
		// Requested broker is not a provider
		RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
		return "", store.ErrNotFound
	}

	// Case 2: Use grove's default runtime broker (must be online)
	if grove.DefaultRuntimeBrokerID != "" {
		// Check if the default broker is still available
		for _, h := range availableBrokers {
			if h.ID == grove.DefaultRuntimeBrokerID {
				return grove.DefaultRuntimeBrokerID, nil
			}
		}
		// Default broker is not available
		if len(availableBrokers) > 0 {
			NoRuntimeBroker(w, "Default runtime broker is unavailable; specify an alternative", brokerSummaries)
		} else {
			NoRuntimeBroker(w, "Default runtime broker is unavailable and no alternatives found", brokerSummaries)
		}
		return "", store.ErrNotFound
	}

	// Case 3: No default and no explicit broker - check for single provider
	// If there's exactly one provider, use it regardless of online status
	// (the dispatch will fail gracefully if the broker is truly unavailable)
	if len(allProviders) == 1 {
		return allProviders[0].BrokerID, nil
	}

	// Case 4: Multiple providers - require explicit selection from online brokers
	switch len(availableBrokers) {
	case 0:
		NoRuntimeBroker(w, "No runtime brokers available for this grove; register a runtime broker first", brokerSummaries)
		return "", store.ErrNotFound
	default:
		// Multiple brokers available - require explicit selection
		NoRuntimeBroker(w, "Multiple runtime brokers available for this grove; specify runtimeBrokerId to select one", brokerSummaries)
		return "", store.ErrNotFound
	}
}

// getAvailableBrokersForGrove returns online runtime brokers that are providers to the grove.
func (s *Server) getAvailableBrokersForGrove(ctx context.Context, groveID string) ([]store.RuntimeBroker, error) {
	// Get providers for this grove
	providers, err := s.store.GetGroveProviders(ctx, groveID)
	if err != nil {
		return nil, err
	}

	// Filter to online brokers and fetch their full details
	var availableBrokers []store.RuntimeBroker
	for _, provider := range providers {
		if provider.Status == store.BrokerStatusOnline {
			broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
			if err != nil {
				continue // Skip brokers we can't fetch
			}
			if broker.Status == store.BrokerStatusOnline {
				availableBrokers = append(availableBrokers, *broker)
			}
		}
	}

	return availableBrokers, nil
}
