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
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ptone/scion-agent/pkg/agent/state"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/gcp"
	"github.com/ptone/scion-agent/pkg/messages"
	"github.com/ptone/scion-agent/pkg/secret"
	"github.com/ptone/scion-agent/pkg/storage"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/transfer"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/ptone/scion-agent/pkg/version"
)

// ============================================================================
// Health Endpoints
// ============================================================================

type HealthResponse struct {
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	ScionVersion string            `json:"scionVersion"`
	Uptime       string            `json:"uptime"`
	Checks       map[string]string `json:"checks,omitempty"`
	Stats        *HealthStats      `json:"stats,omitempty"`
}

type HealthStats struct {
	ConnectedBrokers int `json:"connectedBrokers,omitempty"`
	ActiveAgents     int `json:"activeAgents,omitempty"`
	Groves           int `json:"groves,omitempty"`
}

// GetHealthInfo returns the current health status of the Hub server.
// This can be called directly by co-located components (e.g., the WebServer)
// to build composite health responses without making an HTTP round-trip.
func (s *Server) GetHealthInfo(ctx context.Context) *HealthResponse {
	checks := make(map[string]string)

	// Check database
	if err := s.store.Ping(ctx); err != nil {
		checks["database"] = "unhealthy"
	} else {
		checks["database"] = "healthy"
	}

	// Get stats
	stats := &HealthStats{}
	if agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{Phase: string(state.PhaseRunning)}, store.ListOptions{Limit: 1}); err == nil {
		stats.ActiveAgents = agentResult.TotalCount
	}
	if groveResult, err := s.store.ListGroves(ctx, store.GroveFilter{}, store.ListOptions{Limit: 1}); err == nil {
		stats.Groves = groveResult.TotalCount
	}
	if brokerResult, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Status: store.BrokerStatusOnline}, store.ListOptions{Limit: 1}); err == nil {
		stats.ConnectedBrokers = brokerResult.TotalCount
	}

	status := "healthy"
	for _, v := range checks {
		if v != "healthy" {
			status = "degraded"
			break
		}
	}

	return &HealthResponse{
		Status:       status,
		Version:      "0.1.0", // TODO: Get from build info
		ScionVersion: version.Short(),
		Uptime:       time.Since(s.startTime).Round(time.Second).String(),
		Checks:       checks,
		Stats:        stats,
	}
}

// HealthStatus returns the status string from the health response.
// This enables interface-based status checking from the web handler.
func (h *HealthResponse) HealthStatus() string {
	return h.Status
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	resp := s.GetHealthInfo(r.Context())
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
	Agents       []AgentWithCapabilities `json:"agents"`
	NextCursor   string                  `json:"nextCursor,omitempty"`
	TotalCount   int                     `json:"totalCount"`
	ServerTime   time.Time               `json:"serverTime"`
	Capabilities *Capabilities           `json:"_capabilities,omitempty"`
}

type CreateAgentRequest struct {
	Name            string            `json:"name"`
	GroveID         string            `json:"groveId"`
	RuntimeBrokerID string            `json:"runtimeBrokerId,omitempty"` // Optional: uses grove's default if not specified
	Template        string            `json:"template"`
	HarnessConfig   string            `json:"harnessConfig,omitempty"` // Explicit harness config name (used during sync when template may not be on Hub)
	HarnessAuth     string            `json:"harnessAuth,omitempty"`   // Late-binding override for auth_selected_type
	Profile         string            `json:"profile,omitempty"`       // Settings profile for the runtime broker to use
	Task            string            `json:"task,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	Workspace       string            `json:"workspace,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Config          *api.ScionConfig  `json:"config,omitempty"`
	Attach          bool              `json:"attach,omitempty"`        // If true, signals interactive attach mode to the broker/harness
	ProvisionOnly   bool              `json:"provisionOnly,omitempty"` // If true, provision only (write task to prompt.md) without starting
	// WorkspaceFiles is populated for non-git workspace bootstrap.
	// When present, the Hub generates signed upload URLs instead of dispatching immediately.
	WorkspaceFiles []transfer.FileInfo `json:"workspaceFiles,omitempty"`
	// GatherEnv enables the env-gather flow where the broker evaluates env
	// completeness and may return a 202 requiring the CLI to supply missing values.
	GatherEnv bool `json:"gatherEnv,omitempty"`
	// Notify subscribes the creating agent/user to status notifications for the new agent.
	Notify bool `json:"notify,omitempty"`
	// CleanupMode controls stale-existing-agent cleanup behavior during create:
	// "strict" (default) fails create if broker cleanup fails; "force" continues.
	CleanupMode string `json:"cleanupMode,omitempty"`
}

type CreateAgentResponse struct {
	Agent    *store.Agent `json:"agent"`
	Warnings []string     `json:"warnings,omitempty"`
	// UploadURLs is populated during workspace bootstrap (non-git groves).
	// The CLI uploads files to these URLs, then calls finalize to trigger dispatch.
	UploadURLs []transfer.UploadURLInfo `json:"uploadUrls,omitempty"`
	// Expires indicates when the upload URLs expire.
	Expires *time.Time `json:"expires,omitempty"`
	// EnvGather is populated when the broker returns 202, indicating env
	// vars need to be gathered from the CLI before the agent can start.
	EnvGather *EnvGatherResponse `json:"envGather,omitempty"`
}

// EnvGatherResponse contains env requirements relayed from the broker.
type EnvGatherResponse struct {
	AgentID     string                   `json:"agentId"`
	Required    []string                 `json:"required"`
	HubHas      []EnvSource              `json:"hubHas"`
	BrokerHas   []string                 `json:"brokerHas"`
	Needs       []string                 `json:"needs"`
	SecretInfo  map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
	HubWarnings []string                 `json:"hubWarnings,omitempty"`
}

// EnvSource tracks which scope provided an env var key.
type EnvSource struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

// SubmitEnvRequest is the request body for submitting gathered env vars.
type SubmitEnvRequest struct {
	Env map[string]string `json:"env"`
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
		GroveID:         query.Get("groveId"),
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
		Phase:           query.Get("phase"),
		IncludeDeleted:  query.Get("includeDeleted") == "true",
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

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	agents := make([]AgentWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = agentResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "agent")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			agents = append(agents, AgentWithCapabilities{Agent: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			agents = append(agents, AgentWithCapabilities{Agent: result.Items[i]})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "agent")
	}

	totalCount := result.TotalCount
	if identity != nil {
		totalCount = len(agents)
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:       agents,
		NextCursor:   result.NextCursor,
		TotalCount:   totalCount,
		ServerTime:   time.Now().UTC(),
		Capabilities: scopeCap,
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
	if req.CleanupMode != "" && req.CleanupMode != "strict" && req.CleanupMode != "force" {
		ValidationError(w, "cleanupMode must be 'strict' or 'force'", nil)
		return
	}

	// Check if the caller is an agent (sub-agent creation)
	var createdBy string
	var creatorName string
	var notifySubscriberType, notifySubscriberID string // For --notify subscription
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		// Agent callers must have the grove:agent:create scope
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: grove:agent:create", nil)
			return
		}
		// Enforce grove isolation: agents can only create sub-agents in their own grove
		if req.GroveID != agentIdent.GroveID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only create sub-agents within their own grove", nil)
			return
		}
		createdBy = agentIdent.ID()
		// Resolve human-readable creator name from the calling agent
		if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
			creatorName = creatorAgent.Name
			notifySubscriberType = store.SubscriberTypeAgent
			notifySubscriberID = creatorAgent.Slug
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
		creatorName = userIdent.Email()
		notifySubscriberType = store.SubscriberTypeUser
		notifySubscriberID = userIdent.ID()
		// Enforce policy-based authorization: user must have permission to create agents in this grove
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "agent",
			ParentType: "grove",
			ParentID:   req.GroveID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to create agents in this grove", nil)
			return
		}
	}

	s.createAgentInGrove(w, r, req, req.GroveID, createdBy, creatorName, notifySubscriberType, notifySubscriberID)
}

func (s *Server) createAgentInGrove(
	w http.ResponseWriter,
	r *http.Request,
	req CreateAgentRequest,
	groveID string,
	createdBy string,
	creatorName string,
	notifySubscriberType string,
	notifySubscriberID string,
) {
	ctx := r.Context()

	// Verify grove exists and get its configuration
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

	// Enforce broker-level dispatch authorization: only the broker owner can create agents on it
	if runtimeBrokerID != "" {
		if !s.checkBrokerDispatchAccess(ctx, w, runtimeBrokerID) {
			return
		}
	}

	// Check if the agent already exists (e.g. created via "scion create" for later start).
	// If it exists in "created" status, start it instead of creating a duplicate.
	// If it doesn't exist, fall through to create it.
	slug, err := api.ValidateAgentName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error(), nil)
		return
	}
	existingAgent, err := s.store.GetAgentBySlug(ctx, groveID, slug)
	if err != nil && err != store.ErrNotFound {
		writeErrorFromErr(w, err, "")
		return
	}

	switch s.handleExistingAgent(ctx, w, existingAgent, grove, runtimeBrokerID, req, notifySubscriberType, notifySubscriberID, createdBy) {
	case existingAgentStarted, existingAgentErrored:
		return // Response already written.
	case existingAgentDeleted:
		// Fall through to create a new agent below.
	case existingAgentNone:
		// No existing agent (or unhandled status) — fall through to create.
	}

	// Resolve template if specified - the client may pass either a template ID or name
	var resolvedTemplate *store.Template
	if req.Template != "" {
		resolvedTemplate, err = s.resolveTemplate(ctx, req.Template, groveID)
		if err != nil && err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
		// If template was requested but not found, check if the broker has local access
		if resolvedTemplate == nil {
			brokerHasLocal := false
			if runtimeBrokerID != "" {
				provider, err := s.store.GetGroveProvider(ctx, groveID, runtimeBrokerID)
				if err == nil && provider.LocalPath != "" {
					brokerHasLocal = true
				}
			}
			if !brokerHasLocal {
				NotFound(w, "Template")
				return
			}
			// Template will be resolved locally by the broker
		}
	}

	// Resolve harness config: prefer template metadata harness field, then explicit request field.
	// Do NOT use req.Template as fallback since it may contain a UUID.
	harnessConfig := s.getHarnessConfigFromTemplate(resolvedTemplate, req.HarnessConfig)

	agent := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            slug,
		Name:            req.Name,
		Template:        req.Template,
		GroveID:         groveID,
		RuntimeBrokerID: runtimeBrokerID,
		Phase:           string(state.PhaseCreated),
		Labels:          req.Labels,
		Visibility:      store.VisibilityPrivate,
		CreatedBy:       createdBy,
		OwnerID:         createdBy,
	}

	// Store human-friendly slug instead of UUID for display
	if resolvedTemplate != nil && resolvedTemplate.Slug != "" {
		agent.Template = resolvedTemplate.Slug
	}

	agent.AppliedConfig = s.buildAppliedConfig(req, harnessConfig, creatorName)
	if req.Config != nil {
		agent.Image = req.Config.Image
		if req.Config.Detached != nil {
			agent.Detached = *req.Config.Detached
		} else {
			agent.Detached = true
		}
	} else {
		agent.Detached = true
	}

	s.populateAgentConfig(agent, grove, resolvedTemplate)

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Create notification subscription if requested
	if req.Notify {
		s.createNotifySubscription(ctx, agent.ID, groveID, notifySubscriberType, notifySubscriberID, createdBy)
	}

	// Workspace bootstrap mode: if WorkspaceFiles are provided with a task,
	// generate signed upload URLs instead of dispatching immediately.
	// The CLI will upload files, then call finalize to trigger dispatch.
	//
	// Exception: if the target broker has a LocalPath for this grove, the broker
	// can access the workspace directly from the filesystem — skip the upload
	// and fall through to the normal dispatch path.
	if len(req.WorkspaceFiles) > 0 && req.Task != "" {
		// Check if the target broker has local filesystem access to this grove
		hasLocalPath := false
		if runtimeBrokerID != "" {
			provider, err := s.store.GetGroveProvider(ctx, groveID, runtimeBrokerID)
			if err == nil && provider.LocalPath != "" {
				hasLocalPath = true
				s.agentLifecycleLog.Debug("Workspace bootstrap: broker has local path, skipping upload",
					"broker", runtimeBrokerID, "localPath", provider.LocalPath)
			}
		}

		if !hasLocalPath && !s.isEmbeddedBroker(runtimeBrokerID) {
			stor := s.GetStorage()
			if stor == nil {
				RuntimeError(w, "Storage not configured for workspace bootstrap")
				return
			}

			storagePath := storage.WorkspaceStoragePath(agent.GroveID, agent.ID)
			uploadURLs, existingFiles, err := generateWorkspaceUploadURLs(ctx, stor, storagePath, req.WorkspaceFiles)
			if err != nil {
				RuntimeError(w, "Failed to generate upload URLs: "+err.Error())
				return
			}

			// Set agent to provisioning phase (not dispatched yet)
			agent.Phase = string(state.PhaseProvisioning)
			if err := s.store.UpdateAgent(ctx, agent); err != nil {
				s.agentLifecycleLog.Warn("Failed to update agent status to provisioning", "error", err)
			}

			expires := time.Now().Add(SignedURLExpiry)
			s.enrichAgent(ctx, agent, grove, nil)

			var warnings []string
			if len(existingFiles) > 0 {
				s.agentLifecycleLog.Debug("Workspace bootstrap: files already in storage", "count", len(existingFiles))
			}

			writeJSON(w, http.StatusCreated, CreateAgentResponse{
				Agent:      agent,
				Warnings:   warnings,
				UploadURLs: uploadURLs,
				Expires:    &expires,
			})
			return
		}
	}

	// Hub-native grove remote broker support: if the grove has no git remote
	// (hub-native) and the workspace is set, upload it to GCS so a remote broker
	// can download it. This mirrors the workspace bootstrap pattern above.
	if grove.GitRemote == "" && agent.AppliedConfig != nil && agent.AppliedConfig.Workspace != "" {
		hasLocalPath := false
		if runtimeBrokerID != "" {
			provider, err := s.store.GetGroveProvider(ctx, grove.ID, runtimeBrokerID)
			if err == nil && provider.LocalPath != "" {
				hasLocalPath = true
			}
		}

		if !hasLocalPath && !s.isEmbeddedBroker(runtimeBrokerID) {
			stor := s.GetStorage()
			if stor != nil {
				storagePath := storage.GroveWorkspaceStoragePath(grove.ID)
				if err := gcp.SyncToGCS(ctx, agent.AppliedConfig.Workspace, stor.Bucket(), storagePath+"/files"); err != nil {
					s.agentLifecycleLog.Warn("Failed to upload hub-native grove workspace to GCS",
						"grove", grove.ID, "error", err)
				} else {
					// Swap workspace to storage path for remote broker
					agent.AppliedConfig.Workspace = ""
					agent.AppliedConfig.WorkspaceStoragePath = storagePath
					if err := s.store.UpdateAgent(ctx, agent); err != nil {
						s.agentLifecycleLog.Warn("Failed to update agent with workspace storage path", "error", err)
					}
				}
			}
		}
	}

	// Dispatch to runtime broker if available.
	// Unless provision-only is requested, do a full create+start via DispatchAgentCreate.
	// Otherwise provision only — set up dirs, worktree, templates without launching the container.
	var warnings []string
	if dispatcher := s.GetDispatcher(); dispatcher != nil {
		if !req.ProvisionOnly {
			// Use env-gather dispatch if requested
			if req.GatherEnv {
				s.agentLifecycleLog.Debug("Hub: env-gather requested, using DispatchAgentCreateWithGather",
					"agent", agent.Name, "broker", agent.RuntimeBrokerID)
				envReqs, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
				if err != nil {
					// Dispatch failed — clean up provisioned files on the broker
					// and delete the agent record so orphaned local files don't
					// trigger spurious sync-registration attempts.
					_ = dispatcher.DispatchAgentDelete(ctx, agent, true, true, false, time.Time{})
					_ = s.store.DeleteAgent(ctx, agent.ID)
					RuntimeError(w, "Failed to dispatch to runtime broker: "+err.Error())
					return
				} else if envReqs != nil {
					// Broker returned 202: needs env gather
					agent.Phase = string(state.PhaseProvisioning)
					if err := s.store.UpdateAgent(ctx, agent); err != nil {
						s.agentLifecycleLog.Warn("Failed to update agent phase for env-gather", "error", err)
					}

					s.enrichAgent(ctx, agent, grove, nil)
					hubEnvGather := s.buildEnvGatherResponse(ctx, agent, envReqs)

					writeJSON(w, http.StatusAccepted, CreateAgentResponse{
						Agent:     agent,
						Warnings:  warnings,
						EnvGather: hubEnvGather,
					})
					return
				} else {
					if agent.Phase == string(state.PhaseCreated) {
						agent.Phase = string(state.PhaseProvisioning)
					}
					if err := s.store.UpdateAgent(ctx, agent); err != nil {
						warnings = append(warnings, "Failed to update agent phase: "+err.Error())
					}
				}
			} else {
				envReqs, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
				if err != nil {
					// Dispatch failed — clean up provisioned files on the broker
					// and delete the agent record so orphaned local files don't
					// trigger spurious sync-registration attempts.
					_ = dispatcher.DispatchAgentDelete(ctx, agent, true, true, false, time.Time{})
					_ = s.store.DeleteAgent(ctx, agent.ID)
					RuntimeError(w, "Failed to dispatch to runtime broker: "+err.Error())
					return
				} else if envReqs != nil && len(envReqs.Needs) > 0 {
					// Broker reported missing required env vars — fail the dispatch.
					// Clean up the provisioning agent and its files so orphaned
					// local state doesn't trigger spurious sync-registration.
					_ = dispatcher.DispatchAgentDelete(ctx, agent, true, true, false, time.Time{})
					_ = s.store.DeleteAgent(ctx, agent.ID)
					MissingEnvVars(w, envReqs.Needs, s.buildEnvGatherResponse(ctx, agent, envReqs))
					return
				} else {
					if agent.Phase == string(state.PhaseCreated) {
						agent.Phase = string(state.PhaseProvisioning)
					}
					if err := s.store.UpdateAgent(ctx, agent); err != nil {
						warnings = append(warnings, "Failed to update agent phase: "+err.Error())
					}
				}
			}
		} else {
			// Provision-only: set up agent filesystem without starting
			if err := dispatcher.DispatchAgentProvision(ctx, agent); err != nil {
				warnings = append(warnings, "Failed to provision on runtime broker: "+err.Error())
			} else {
				agent.Phase = string(state.PhaseCreated)
				if err := s.store.UpdateAgent(ctx, agent); err != nil {
					warnings = append(warnings, "Failed to update agent phase: "+err.Error())
				}
			}
		}
	}

	s.events.PublishAgentCreated(ctx, agent)

	// Enrich agent with grove and broker names for display
	s.enrichAgent(ctx, agent, grove, nil)

	writeJSON(w, http.StatusCreated, CreateAgentResponse{
		Agent:    agent,
		Warnings: warnings,
	})
}

// buildEnvGatherResponse converts a broker's env requirements into the Hub-level
// response format, enriching it with scope information from the dispatcher.
func (s *Server) buildEnvGatherResponse(ctx context.Context, agent *store.Agent, brokerReqs *RemoteEnvRequirementsResponse) *EnvGatherResponse {
	resp := &EnvGatherResponse{
		AgentID:   agent.ID,
		Required:  brokerReqs.Required,
		BrokerHas: brokerReqs.BrokerHas,
		Needs:     brokerReqs.Needs,
	}

	// Build hubHas with scope info
	// Try to determine the scope for each key the Hub provided
	for _, key := range brokerReqs.HubHas {
		source := EnvSource{Key: key, Scope: "hub"}

		// Check if we can determine a more specific scope
		if agent.OwnerID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID, Key: key})
			if err == nil && len(vars) > 0 {
				source.Scope = "user"
			}
		}
		if source.Scope == "hub" && agent.GroveID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "grove", ScopeID: agent.GroveID, Key: key})
			if err == nil && len(vars) > 0 {
				source.Scope = "grove"
			}
		}
		if source.Scope == "hub" {
			// Check if it came from config
			if agent.AppliedConfig != nil {
				if _, ok := agent.AppliedConfig.Env[key]; ok {
					source.Scope = "config"
				}
			}
		}
		if source.Scope == "hub" && s.secretBackend != nil {
			if agent.OwnerID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{
					Scope: "user", ScopeID: agent.OwnerID, Name: key,
				})
				if err == nil && len(metas) > 0 {
					source.Scope = "secret"
				}
			}
			if source.Scope == "hub" && agent.GroveID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{
					Scope: "grove", ScopeID: agent.GroveID, Name: key,
				})
				if err == nil && len(metas) > 0 {
					source.Scope = "secret"
				}
			}
		}
		resp.HubHas = append(resp.HubHas, source)
	}

	// Relay SecretInfo from broker
	if len(brokerReqs.SecretInfo) > 0 {
		resp.SecretInfo = make(map[string]SecretKeyInfo, len(brokerReqs.SecretInfo))
		for k, v := range brokerReqs.SecretInfo {
			resp.SecretInfo[k] = SecretKeyInfo{
				Description: v.Description,
				Source:      v.Source,
				Type:        v.Type,
			}
		}
	}

	// Cross-check: for each key the broker says it "needs", check whether the
	// Hub actually has it in storage (env_vars table or secret backend).  If
	// found, this indicates a resolution mismatch — the dispatch should have
	// included it but didn't.
	for _, key := range brokerReqs.Needs {
		// Check env_vars table
		if agent.OwnerID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID, Key: key})
			if err == nil && len(vars) > 0 {
				resp.HubWarnings = append(resp.HubWarnings,
					fmt.Sprintf("%s is stored in Hub env storage (user scope) but was not included in the dispatch — this may indicate a resolution issue", key))
				continue
			}
		}
		if agent.GroveID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "grove", ScopeID: agent.GroveID, Key: key})
			if err == nil && len(vars) > 0 {
				resp.HubWarnings = append(resp.HubWarnings,
					fmt.Sprintf("%s is stored in Hub env storage (grove scope) but was not included in the dispatch — this may indicate a resolution issue", key))
				continue
			}
		}
		// Check secret backend
		if s.secretBackend != nil {
			if agent.OwnerID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{Scope: "user", ScopeID: agent.OwnerID, Name: key})
				if err == nil && len(metas) > 0 {
					resp.HubWarnings = append(resp.HubWarnings,
						fmt.Sprintf("%s is stored in Hub secrets (user scope) but was not included in the dispatch — this may indicate a resolution issue", key))
					continue
				}
			}
			if agent.GroveID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{Scope: "grove", ScopeID: agent.GroveID, Name: key})
				if err == nil && len(metas) > 0 {
					resp.HubWarnings = append(resp.HubWarnings,
						fmt.Sprintf("%s is stored in Hub secrets (grove scope) but was not included in the dispatch — this may indicate a resolution issue", key))
					continue
				}
			}
		}
	}

	return resp
}

// submitAgentEnv handles POST /api/v1/groves/{groveId}/agents/{agentId}/env
// CLI submits gathered env vars after receiving a 202 env-gather response.
func (s *Server) submitAgentEnv(w http.ResponseWriter, r *http.Request, groveID, agentID string) {
	ctx := r.Context()

	var req SubmitEnvRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Env) == 0 {
		ValidationError(w, "env map is required and must not be empty", nil)
		return
	}

	// Resolve agent
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

	// Verify agent is in a state that expects env submission
	if agent.Phase != string(state.PhaseProvisioning) && agent.Phase != string(state.PhaseCreated) {
		writeError(w, http.StatusConflict, "invalid_state",
			fmt.Sprintf("agent is in '%s' phase; env submission only valid during provisioning", agent.Phase), nil)
		return
	}

	// Dispatch finalize-env to the broker
	dispatcher := s.GetDispatcher()
	if dispatcher == nil || agent.RuntimeBrokerID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"cannot finalize env: no runtime broker available", nil)
		return
	}

	if err := dispatcher.DispatchFinalizeEnv(ctx, agent, req.Env); err != nil {
		RuntimeError(w, "Failed to finalize env on runtime broker: "+err.Error())
		return
	}

	// Update agent phase from broker response
	if agent.Phase == string(state.PhaseProvisioning) || agent.Phase == string(state.PhaseCreated) {
		agent.Phase = string(state.PhaseRunning)
	}
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		s.agentLifecycleLog.Warn("Failed to update agent phase after env submit", "error", err)
	}

	// Enrich and return
	grove, _ := s.store.GetGrove(ctx, groveID)
	s.enrichAgent(ctx, agent, grove, nil)

	writeJSON(w, http.StatusOK, CreateAgentResponse{
		Agent: agent,
	})
}

// enrichAgents populates Grove and RuntimeBrokerName fields for a slice of agents.
// This provides human-readable names from the related IDs for display purposes.
func (s *Server) enrichAgents(ctx context.Context, agents []store.Agent) {
	if len(agents) == 0 {
		return
	}

	// Collect unique grove, broker, and template IDs
	groveIDs := make(map[string]struct{})
	brokerIDs := make(map[string]struct{})
	templateIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.GroveID != "" {
			groveIDs[a.GroveID] = struct{}{}
		}
		if a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
		if a.AppliedConfig != nil && a.AppliedConfig.TemplateID != "" {
			templateIDs[a.AppliedConfig.TemplateID] = struct{}{}
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

	// Fetch templates for slug enrichment
	templateSlugs := make(map[string]string)
	for id := range templateIDs {
		if tmpl, err := s.store.GetTemplate(ctx, id); err == nil && tmpl.Slug != "" {
			templateSlugs[id] = tmpl.Slug
		}
	}

	// Enrich agents
	for i := range agents {
		// Populate harness config from applied config
		if agents[i].HarnessConfig == "" && agents[i].AppliedConfig != nil && agents[i].AppliedConfig.HarnessConfig != "" {
			agents[i].HarnessConfig = agents[i].AppliedConfig.HarnessConfig
		}
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
		// Enrich template slug from TemplateID if Template is a UUID or empty
		if agents[i].AppliedConfig != nil && agents[i].AppliedConfig.TemplateID != "" {
			if slug, ok := templateSlugs[agents[i].AppliedConfig.TemplateID]; ok {
				agents[i].Template = slug
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

	// Populate harness config and auth from applied config
	if agent.AppliedConfig != nil {
		if agent.HarnessConfig == "" && agent.AppliedConfig.HarnessConfig != "" {
			agent.HarnessConfig = agent.AppliedConfig.HarnessConfig
		}
		if agent.HarnessAuth == "" && agent.AppliedConfig.HarnessAuth != "" {
			agent.HarnessAuth = agent.AppliedConfig.HarnessAuth
		}
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
			s.agentLifecycleLog.Debug("failed to get runtime broker for enrichment", "brokerID", agent.RuntimeBrokerID, "error", err)
		} else {
			agent.RuntimeBrokerName = b.Name
			s.agentLifecycleLog.Debug("enriched agent with broker name", "slug", agent.Slug, "brokerName", b.Name)
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

	// Enrich template slug from TemplateID
	if agent.AppliedConfig != nil && agent.AppliedConfig.TemplateID != "" {
		if tmpl, err := s.store.GetTemplate(ctx, agent.AppliedConfig.TemplateID); err == nil && tmpl.Slug != "" {
			agent.Template = tmpl.Slug
		}
	}
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}

	// Handle stop-all (POST /api/v1/agents/stop-all)
	if id == "stop-all" {
		s.handleStopAllAgents(w, r, "")
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

	// Handle groups query
	if action == "groups" {
		s.handleAgentGroups(w, r, id)
		return
	}

	// Handle cloud-logs (GET endpoints, handled before the POST-only action gate)
	if action == "cloud-logs" {
		s.handleAgentCloudLogs(w, r, id)
		return
	}
	if action == "cloud-logs/stream" {
		s.handleAgentCloudLogsStream(w, r, id)
		return
	}

	// Handle message-logs (GET endpoints for message audit log)
	if action == "message-logs" {
		s.handleAgentMessageLogs(w, r, id)
		return
	}
	if action == "message-logs/stream" {
		s.handleAgentMessageLogsStream(w, r, id)
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

	// If the caller is an agent, enforce grove isolation
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.GroveID != agentIdent.GroveID() {
			NotFound(w, "Agent")
			return
		}
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	// Enrich agent with grove and broker names
	s.enrichAgent(ctx, agent, nil, nil)
	resolvedHarness, harnessCaps := s.resolveAgentHarnessCapabilities(ctx, agent)

	// Compute capabilities for this agent
	resp := AgentWithCapabilities{
		Agent:               *agent,
		ResolvedHarness:     resolvedHarness,
		HarnessCapabilities: &harnessCaps,
		CloudLogging:        s.logQueryService != nil,
	}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, agentResource(agent))
	}

	writeJSON(w, http.StatusOK, resp)
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
		Config       *api.ScionConfig  `json:"config,omitempty"`
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

	// Apply config updates (only allowed for agents in 'created' phase)
	if updates.Config != nil {
		if agent.Phase != "created" {
			Conflict(w, "Config can only be updated for agents in 'created' phase")
			return
		}
		resolvedHarness, harnessCaps := s.resolveAgentHarnessCapabilities(ctx, agent)
		if issues := validateConfigAgainstHarnessCapabilities(updates.Config, harnessCaps); len(issues) > 0 {
			ValidationError(w, "Config contains unsupported fields for harness "+resolvedHarness, map[string]interface{}{
				"harness": resolvedHarness,
				"fields":  issues,
			})
			return
		}
		if agent.AppliedConfig == nil {
			agent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		cfg := updates.Config
		if cfg.Image != "" {
			agent.AppliedConfig.Image = cfg.Image
		}
		if cfg.Model != "" {
			agent.AppliedConfig.Model = cfg.Model
		}
		if cfg.Task != "" {
			agent.AppliedConfig.Task = cfg.Task
		}
		if cfg.AuthSelectedType != "" {
			agent.AppliedConfig.HarnessAuth = cfg.AuthSelectedType
		}
		if cfg.Env != nil {
			agent.AppliedConfig.Env = cfg.Env
		}
		agent.AppliedConfig.InlineConfig = cfg
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
		s.agentLifecycleLog.Warn("Failed to check broker status", "brokerID", agent.RuntimeBrokerID, "error", err)
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
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.performAgentDelete(w, r, agent)
}

// performAgentDelete handles both soft and hard deletion of an agent.
// Soft-delete: marks agent as deleted with a timestamp and retains the record.
// Hard-delete: permanently removes the agent record from the store.
func (s *Server) performAgentDelete(w http.ResponseWriter, r *http.Request, agent *store.Agent) {
	ctx := r.Context()

	// Enforce policy-based authorization: only the agent's creator (owner) or admins can delete
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "agent",
			ID:      agent.ID,
			OwnerID: agent.OwnerID,
		}, ActionDelete)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Only the agent's creator can delete it", nil)
			return
		}
	}

	query := r.URL.Query()

	// Default deleteFiles and removeBranch to true for full cleanup.
	// Callers can explicitly set them to "false" to preserve files/branches.
	deleteFiles := query.Get("deleteFiles") != "false"
	removeBranch := query.Get("removeBranch") != "false"
	force := query.Get("force") == "true"

	// Idempotency: already-deleted agent returns 204
	if !agent.DeletedAt.IsZero() {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Determine soft vs hard delete
	retention := s.config.SoftDeleteRetention
	softDelete := retention > 0 && !force

	// If SoftDeleteRetainFiles is configured, override deleteFiles for soft-deletes
	if softDelete && s.config.SoftDeleteRetainFiles {
		deleteFiles = false
	}

	// Verify broker is reachable before deleting to avoid orphaned containers.
	// Force mode bypasses this check so stuck agents can always be cleaned up.
	if !force && !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	now := time.Now()

	// If a dispatcher is available, dispatch the deletion to the runtime broker
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeBrokerID != "" {
		if err := dispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch, softDelete, now); err != nil {
			if force {
				// Force mode: log warning and continue with hub record deletion
				s.agentLifecycleLog.Warn("Failed to dispatch agent delete to broker (force=true, continuing)",
					"agentID", agent.ID, "error", err)
			} else {
				// Normal mode: fail the operation to avoid orphaning the agent on the broker
				s.agentLifecycleLog.Error("Failed to dispatch agent delete to broker", "agentID", agent.ID, "error", err)
				writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
					"Failed to delete agent on runtime broker: "+err.Error(), nil)
				return
			}
		}
	}

	if softDelete {
		// Soft delete: mark agent as deleted with timestamp
		agent.Phase = string(state.PhaseStopped)
		agent.DeletedAt = now
		agent.Updated = now
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		s.events.PublishAgentDeleted(ctx, agent.ID, agent.GroveID)
	} else {
		// Hard delete: permanently remove the agent record
		if err := s.store.DeleteAgent(ctx, agent.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		s.events.PublishAgentDeleted(ctx, agent.ID, agent.GroveID)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// For actions other than "status" and "token/refresh" (self-access),
	// we require user or agent authentication with appropriate scopes.
	// Token refresh is handled separately with self-access enforcement.
	if action != "status" && action != "token/refresh" {
		userIdent := GetUserIdentityFromContext(r.Context())
		agentIdent := GetAgentIdentityFromContext(r.Context())
		if userIdent == nil && agentIdent == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user or agent authentication", nil)
			return
		}
		// If the caller is an agent, verify scope and grove isolation for lifecycle actions
		if agentIdent != nil && userIdent == nil {
			if !agentIdent.HasScope(ScopeAgentLifecycle) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: grove:agent:lifecycle", nil)
				return
			}
			// Look up target agent for grove isolation check
			targetAgent, err := s.store.GetAgent(r.Context(), id)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if targetAgent.GroveID != agentIdent.GroveID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage agents within their own grove", nil)
				return
			}
		}
		// For user callers, enforce policy-based authorization on interactive actions
		if userIdent != nil {
			targetAgent, err := s.store.GetAgent(r.Context(), id)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			decision := s.authzService.CheckAccess(r.Context(), userIdent, Resource{
				Type:    "agent",
				ID:      targetAgent.ID,
				OwnerID: targetAgent.OwnerID,
			}, ActionAttach)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"Only the agent's creator can interact with it", nil)
				return
			}
		}
	}

	switch action {
	case "status":
		s.updateAgentStatus(w, r, id)
	case "start", "stop", "restart":
		s.handleAgentLifecycle(w, r, id, action)
	case "message":
		s.handleAgentMessage(w, r, id)
	case "restore":
		s.restoreAgent(w, r, id)
	case "token/refresh":
		s.handleAgentTokenRefresh(w, r, id)
	default:
		NotFound(w, "Action")
	}
}

// handleAgentTokenRefresh handles POST /api/v1/agents/{id}/token/refresh.
// An agent can refresh its own token before it expires to get a new token
// with a fresh expiry. This is a self-access operation: the agent must present
// a valid token whose subject matches the target agent ID.
func (s *Server) handleAgentTokenRefresh(w http.ResponseWriter, r *http.Request, id string) {
	agentIdent := GetAgentIdentityFromContext(r.Context())
	if agentIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"agent authentication required for token refresh", nil)
		return
	}

	// Enforce self-access: agents can only refresh their own token
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"agents can only refresh their own token", nil)
		return
	}

	// Require the token refresh scope
	if !agentIdent.HasScope(ScopeAgentTokenRefresh) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"missing required scope: agent:token:refresh", nil)
		return
	}

	if s.agentTokenService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"agent token service not available", nil)
		return
	}

	// Extract the current token from the request to refresh it
	token := extractAgentToken(r)
	if token == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"no agent token found in request", nil)
		return
	}

	newToken, expiresAt, err := s.agentTokenService.RefreshAgentToken(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"failed to refresh token: "+err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      newToken,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

// restoreAgent restores a soft-deleted agent.
func (s *Server) restoreAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if agent.DeletedAt.IsZero() {
		BadRequest(w, "Agent is not in deleted state")
		return
	}

	agent.DeletedAt = time.Time{}
	agent.Updated = time.Now()

	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.events.PublishAgentCreated(ctx, agent)

	writeJSON(w, http.StatusOK, agent.ToAPI())
}

// MessageRequest is the request body for sending a message to an agent.
type MessageRequest struct {
	// Plain text message (legacy field, used for backwards compatibility).
	Message string `json:"message,omitempty"`

	// Structured message (new field, used by default).
	StructuredMessage *messages.StructuredMessage `json:"structured_message,omitempty"`

	// Interrupt the harness before sending.
	Interrupt bool `json:"interrupt,omitempty"`

	// Notify subscribes the sender to status notifications for this agent
	// (COMPLETED, WAITING_FOR_INPUT, LIMITS_EXCEEDED, STALLED, ERROR).
	Notify bool `json:"notify,omitempty"`
}

func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Determine the message content and structured message to forward
	var plainMessage string
	var structuredMsg *messages.StructuredMessage

	if req.StructuredMessage != nil {
		structuredMsg = req.StructuredMessage
		plainMessage = req.StructuredMessage.Msg
	} else if req.Message != "" {
		plainMessage = req.Message
		// Build a structured message from the plain text so that downstream
		// logging and the broker receive a fully-populated payload.
		sender := "user:unknown"
		if user := GetUserIdentityFromContext(ctx); user != nil {
			if name := user.DisplayName(); name != "" {
				sender = "user:" + name
			} else if email := user.Email(); email != "" {
				sender = "user:" + email
			}
		}
		structuredMsg = messages.NewInstruction(sender, "agent:"+id, plainMessage)
	} else {
		ValidationError(w, "message or structured_message is required", nil)
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

	// Log the message dispatch to dedicated message log
	logAttrs := []any{
		"agent_id", agent.ID,
		"agent_name", agent.Name,
	}
	if structuredMsg != nil {
		logAttrs = append(logAttrs, structuredMsg.LogAttrs()...)
	}
	s.logMessage("message dispatched", logAttrs...)

	// If a dispatcher is available, dispatch the message to the runtime broker
	if dispatcher := s.GetDispatcher(); dispatcher != nil && agent.RuntimeBrokerID != "" {
		if err := dispatcher.DispatchAgentMessage(ctx, agent, plainMessage, req.Interrupt, structuredMsg); err != nil {
			RuntimeError(w, "Failed to send message to runtime broker: "+err.Error())
			return
		}
	} else {
		// No dispatcher available
		RuntimeError(w, "No runtime broker dispatcher available for this agent")
		return
	}

	// Create notification subscription if requested
	if req.Notify {
		var notifySubscriberType, notifySubscriberID, createdBy string
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			createdBy = agentIdent.ID()
			if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
				notifySubscriberType = store.SubscriberTypeAgent
				notifySubscriberID = creatorAgent.Slug
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
			notifySubscriberType = store.SubscriberTypeUser
			notifySubscriberID = userIdent.ID()
		}
		s.createNotifySubscription(ctx, agent.ID, agent.GroveID, notifySubscriberType, notifySubscriberID, createdBy)
	}

	w.WriteHeader(http.StatusOK)
}

// BroadcastMessageRequest is the request body for broadcasting a message via the broker.
type BroadcastMessageRequest struct {
	StructuredMessage *messages.StructuredMessage `json:"structured_message"`
	Interrupt         bool                        `json:"interrupt,omitempty"`
}

// handleGroveBroadcast handles POST /api/v1/groves/{groveId}/broadcast.
// It publishes a broadcast message to the grove's message broker topic,
// which fans out to all running agents in the grove.
func (s *Server) handleGroveBroadcast(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Require user or agent authentication
	ctx := r.Context()
	userIdent := GetUserIdentityFromContext(ctx)
	agentIdent := GetAgentIdentityFromContext(ctx)
	if userIdent == nil && agentIdent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Broadcast requires user or agent authentication", nil)
		return
	}

	// Agent callers must have message scope and be in the same grove
	if agentIdent != nil && userIdent == nil {
		if !agentIdent.HasScope(ScopeAgentLifecycle) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: grove:agent:lifecycle", nil)
			return
		}
		if agentIdent.GroveID() != groveID {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only broadcast within their own grove", nil)
			return
		}
	}

	var req BroadcastMessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.StructuredMessage == nil {
		ValidationError(w, "structured_message is required", nil)
		return
	}

	proxy := s.GetMessageBrokerProxy()
	if proxy == nil {
		// Fallback: no broker configured, do direct fan-out
		s.broadcastDirect(w, r, groveID, req.StructuredMessage, req.Interrupt)
		return
	}

	// Log the broadcast
	logAttrs := []any{"grove_id", groveID}
	logAttrs = append(logAttrs, req.StructuredMessage.LogAttrs()...)
	s.logMessage("broadcast message published", logAttrs...)

	if err := proxy.PublishBroadcast(ctx, groveID, req.StructuredMessage); err != nil {
		RuntimeError(w, "Failed to publish broadcast message: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
}

// broadcastDirect fans out a broadcast message directly to all running agents
// in the grove without using the message broker. This is the fallback when
// no broker is configured.
func (s *Server) broadcastDirect(w http.ResponseWriter, r *http.Request, groveID string, msg *messages.StructuredMessage, interrupt bool) {
	ctx := r.Context()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		RuntimeError(w, "No runtime broker dispatcher available")
		return
	}

	result, err := s.store.ListAgents(ctx, store.AgentFilter{
		GroveID: groveID,
		Phase:   "running",
	}, store.ListOptions{})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	for _, agent := range result.Items {
		// Skip the sender if it's an agent
		if msg.Sender == "agent:"+agent.Slug {
			continue
		}
		agentMsg := *msg
		agentMsg.Recipient = "agent:" + agent.Slug
		if err := dispatcher.DispatchAgentMessage(ctx, &agent, agentMsg.Msg, interrupt, &agentMsg); err != nil {
			s.messageLog.Error("Failed to deliver broadcast message to agent",
				"agentSlug", agent.Slug, "error", err)
		}
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

	// Publish status event (best-effort: fetch agent for GroveID)
	if agent, err := s.store.GetAgent(ctx, id); err == nil {
		s.events.PublishAgentStatus(ctx, agent)
	} else {
		s.agentLifecycleLog.Warn("Failed to fetch agent for status event", "agentID", id, "error", err)
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

	var newPhase string
	var dispatchErr error

	// If a dispatcher is available, dispatch the operation to the runtime broker
	dispatcher := s.GetDispatcher()

	switch action {
	case "start":
		newPhase = string(state.PhaseRunning)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent, "")
			// DispatchAgentStart applies the broker response in-place;
			// use the broker-reported phase if it was set.
			if dispatchErr == nil && agent.Phase != "" {
				newPhase = agent.Phase
			}
		}
	case "stop":
		newPhase = string(state.PhaseStopped)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Before stopping, sync workspace back for hub-native groves on remote brokers.
			// This is best-effort: failures are logged but don't block the stop.
			s.syncWorkspaceOnStop(ctx, agent)
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
		}
	case "restart":
		newPhase = string(state.PhaseRunning)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Restart is implemented as stop + start so that env vars
			// (API keys, secrets) are re-resolved from Hub storage.
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
			if dispatchErr == nil {
				dispatchErr = dispatcher.DispatchAgentStart(ctx, agent, "")
				// DispatchAgentStart applies the broker response in-place;
				// use the broker-reported phase if it was set.
				if dispatchErr == nil && agent.Phase != "" {
					newPhase = agent.Phase
				}
			}
		}
	}

	// If dispatch failed, return error
	if dispatchErr != nil {
		RuntimeError(w, "Failed to dispatch to runtime broker: "+dispatchErr.Error())
		return
	}

	statusUpdate := store.AgentStatusUpdate{
		Phase: newPhase,
	}
	// When stopping, also update container status so the hub immediately
	// reflects the stopped state without waiting for the next heartbeat.
	if action == "stop" {
		statusUpdate.ContainerStatus = "stopped"
		statusUpdate.Activity = ""
	}
	// When starting, propagate container status from broker response
	if action == "start" && agent.ContainerStatus != "" {
		statusUpdate.ContainerStatus = agent.ContainerStatus
	}
	if err := s.store.UpdateAgentStatus(ctx, id, statusUpdate); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agent.Phase = newPhase
	s.events.PublishAgentStatus(ctx, agent)

	writeJSON(w, http.StatusOK, agent)
}

// ============================================================================
// Stop All Agents
// ============================================================================

// stopAllResult represents the outcome of stopping a single agent.
type stopAllResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// StopAllAgentsResponse is the response from the stop-all endpoint.
type StopAllAgentsResponse struct {
	Stopped int              `json:"stopped"`
	Failed  int              `json:"failed"`
	Total   int              `json:"total"`
	Results []stopAllResult  `json:"results"`
}

// handleStopAllAgents stops all running agents, optionally scoped to a grove.
// Requires admin role. Fans out stop operations concurrently to each broker.
func (s *Server) handleStopAllAgents(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Require admin role
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil || userIdent.Role() != "admin" {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Only admins can stop all agents", nil)
		return
	}

	// List running agents (optionally scoped to grove)
	filter := store.AgentFilter{
		GroveID: groveID,
		Phase:   string(state.PhaseRunning),
	}

	result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit: 1000, // reasonable upper bound
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agents := result.Items
	if len(agents) == 0 {
		writeJSON(w, http.StatusOK, StopAllAgentsResponse{
			Results: []stopAllResult{},
		})
		return
	}

	dispatcher := s.GetDispatcher()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]stopAllResult, 0, len(agents))
	)

	for i := range agents {
		agent := &agents[i]
		wg.Add(1)
		go func(agent *store.Agent) {
			defer wg.Done()

			res := stopAllResult{
				ID:   agent.ID,
				Name: agent.Name,
			}

			// Dispatch stop to broker
			var dispatchErr error
			if dispatcher != nil && agent.RuntimeBrokerID != "" {
				opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				s.syncWorkspaceOnStop(opCtx, agent)
				dispatchErr = dispatcher.DispatchAgentStop(opCtx, agent)
			}

			if dispatchErr != nil {
				res.Status = "error"
				res.Error = dispatchErr.Error()
				s.agentLifecycleLog.Warn("stop-all: failed to stop agent",
					"agentID", agent.ID, "error", dispatchErr)
			} else {
				// Update agent status in store
				statusUpdate := store.AgentStatusUpdate{
					Phase:           string(state.PhaseStopped),
					ContainerStatus: "stopped",
					Activity:        "",
				}
				if updateErr := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); updateErr != nil {
					res.Status = "error"
					res.Error = updateErr.Error()
				} else {
					res.Status = "stopped"
					agent.Phase = string(state.PhaseStopped)
					s.events.PublishAgentStatus(ctx, agent)
				}
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(agent)
	}

	wg.Wait()

	stopped := 0
	failed := 0
	for _, r := range results {
		if r.Status == "stopped" {
			stopped++
		} else {
			failed++
		}
	}

	writeJSON(w, http.StatusOK, StopAllAgentsResponse{
		Stopped: stopped,
		Failed:  failed,
		Total:   len(results),
		Results: results,
	})
}

// ============================================================================
// Grove Endpoints
// ============================================================================

type ListGrovesResponse struct {
	Groves       []GroveWithCapabilities `json:"groves"`
	NextCursor   string                  `json:"nextCursor,omitempty"`
	TotalCount   int                     `json:"totalCount"`
	Capabilities *Capabilities           `json:"_capabilities,omitempty"`
}

type CreateGroveRequest struct {
	ID         string            `json:"id,omitempty"`
	Slug       string            `json:"slug,omitempty"`
	Name       string            `json:"name"`
	GitRemote  string            `json:"gitRemote,omitempty"`
	Visibility string            `json:"visibility,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type RegisterGroveRequest struct {
	ID        string              `json:"id,omitempty"` // Client-provided grove ID
	Name      string              `json:"name"`
	GitRemote string              `json:"gitRemote"`
	Path      string              `json:"path,omitempty"`
	BrokerID  string              `json:"brokerId,omitempty"` // Link to existing broker (two-phase flow)
	Broker    *RegisterBrokerInfo `json:"broker,omitempty"`   // DEPRECATED: Use BrokerID with two-phase registration
	Profiles  []string            `json:"profiles,omitempty"`
	Labels    map[string]string   `json:"labels,omitempty"`
}

type RegisterBrokerInfo struct {
	ID           string                    `json:"id,omitempty"`
	Name         string                    `json:"name"`
	Version      string                    `json:"version,omitempty"`
	Capabilities *store.BrokerCapabilities `json:"capabilities,omitempty"`
	Profiles     []store.BrokerProfile     `json:"profiles,omitempty"`
}

type RegisterGroveResponse struct {
	Grove       *store.Grove         `json:"grove"`
	Broker      *store.RuntimeBroker `json:"broker,omitempty"`
	Created     bool                 `json:"created"`
	BrokerToken string               `json:"brokerToken,omitempty"` // DEPRECATED: use two-phase registration
	SecretKey   string               `json:"secretKey,omitempty"`   // DEPRECATED: secrets only from /brokers/join
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
		BrokerID:        query.Get("brokerId"),
		Name:            query.Get("name"),
		Slug:            query.Get("slug"),
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

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	groves := make([]GroveWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = groveResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "grove")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			groves = append(groves, GroveWithCapabilities{Grove: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			groves = append(groves, GroveWithCapabilities{Grove: result.Items[i]})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "grove")
	}

	totalCount := result.TotalCount
	if identity != nil {
		totalCount = len(groves)
	}

	writeJSON(w, http.StatusOK, ListGrovesResponse{
		Groves:       groves,
		NextCursor:   result.NextCursor,
		TotalCount:   totalCount,
		Capabilities: scopeCap,
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

	// Idempotency: if client provided an ID, check for existing grove
	if req.ID != "" {
		existing, err := s.store.GetGrove(ctx, req.ID)
		if err == nil {
			// Grove already exists — return it as-is (idempotent)
			writeJSON(w, http.StatusOK, existing)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeErrorFromErr(w, err, "")
			return
		}
		// Not found — proceed to create with client-provided ID
	}

	groveID := req.ID
	if groveID == "" {
		groveID = api.NewUUID()
	}

	slug := req.Slug
	if slug == "" {
		slug = api.Slugify(req.Name)
	}

	grove := &store.Grove{
		ID:         groveID,
		Name:       req.Name,
		Slug:       slug,
		GitRemote:  util.NormalizeGitRemote(req.GitRemote),
		Labels:     req.Labels,
		Visibility: req.Visibility,
	}

	if grove.Visibility == "" {
		grove.Visibility = store.VisibilityPrivate
	}

	// Set ownership from authenticated user
	if user := GetUserIdentityFromContext(ctx); user != nil {
		grove.CreatedBy = user.ID()
		grove.OwnerID = user.ID()
	}

	if err := s.store.CreateGrove(ctx, grove); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Create the associated grove_agents group (best-effort)
	s.createGroveGroup(ctx, grove)

	// Create grove members group and policy (best-effort)
	s.createGroveMembersGroupAndPolicy(ctx, grove)

	// Initialize filesystem workspace for hub-native groves (no git remote).
	if grove.GitRemote == "" {
		if err := s.initHubNativeGrove(grove); err != nil {
			slog.Warn("failed to initialize hub-native grove workspace",
				"grove", grove.ID, "slug", grove.Slug, "error", err)
		}
	}

	// Auto-link brokers that have auto_provide enabled (mirrors registerGrove behavior).
	s.autoLinkProviders(ctx, grove)

	s.events.PublishGroveCreated(ctx, grove)

	writeJSON(w, http.StatusCreated, grove)
}

// createGroveGroup creates the implicit grove_agents group for a grove.
// This is a best-effort operation; failures are logged but don't fail the caller.
// If the group already exists (e.g., grove was deleted and recreated with the same
// slug), the existing group is reused and its grove ID association is updated.
func (s *Server) createGroveGroup(ctx context.Context, grove *store.Grove) {
	agentsSlug := "grove:" + grove.Slug + ":agents"
	groveGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      grove.Name + " Agents",
		Slug:      agentsSlug,
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   grove.ID,
		OwnerID:   grove.OwnerID,
		CreatedBy: grove.CreatedBy,
	}
	if err := s.store.CreateGroup(ctx, groveGroup); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to create grove group", "grove", grove.ID, "error", err)
			return
		}
		// Group already exists — look it up and update its grove ID association
		existing, lookupErr := s.store.GetGroupBySlug(ctx, agentsSlug)
		if lookupErr != nil {
			slog.Warn("failed to look up existing grove agents group",
				"grove", grove.ID, "slug", agentsSlug, "error", lookupErr)
			return
		}
		existing.GroveID = grove.ID
		if updateErr := s.store.UpdateGroup(ctx, existing); updateErr != nil {
			slog.Warn("failed to update existing grove agents group",
				"grove", grove.ID, "slug", agentsSlug, "error", updateErr)
		}
	}
}

// createGroveMembersGroupAndPolicy creates an explicit members group for a grove
// and a policy allowing members to create agents. Best-effort; failures are logged.
// If the group already exists (e.g., grove was deleted and recreated with the same
// slug), the existing group is reused and the creator is still added as a member.
func (s *Server) createGroveMembersGroupAndPolicy(ctx context.Context, grove *store.Grove) {
	membersSlug := "grove:" + grove.Slug + ":members"

	// Create grove members group, or look up the existing one
	membersGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      grove.Name + " Members",
		Slug:      membersSlug,
		GroupType: store.GroupTypeExplicit,
		GroveID:   grove.ID,
		OwnerID:   grove.OwnerID,
		CreatedBy: grove.CreatedBy,
	}
	if err := s.store.CreateGroup(ctx, membersGroup); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to create grove members group", "grove", grove.ID, "error", err)
			return
		}
		// Group already exists — look it up so we can still add the user
		existing, lookupErr := s.store.GetGroupBySlug(ctx, membersSlug)
		if lookupErr != nil {
			slog.Warn("failed to look up existing grove members group",
				"grove", grove.ID, "slug", membersSlug, "error", lookupErr)
			return
		}
		membersGroup = existing
		// Update the grove ID association in case it changed (recreated grove)
		membersGroup.GroveID = grove.ID
		if updateErr := s.store.UpdateGroup(ctx, membersGroup); updateErr != nil {
			slog.Warn("failed to update existing grove members group grove ID",
				"grove", grove.ID, "slug", membersSlug, "error", updateErr)
		}
	}

	// Add the creating user as a member (idempotent — ErrAlreadyExists is fine)
	if grove.CreatedBy != "" {
		if err := s.store.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    membersGroup.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   grove.CreatedBy,
			Role:       store.GroupMemberRoleMember,
		}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to add creator to grove members group",
				"grove", grove.ID, "user", grove.CreatedBy, "error", err)
		}
	}

	// Create grove-level policy for member agent creation
	policyName := "grove:" + grove.Slug + ":member-create-agents"
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "Allow grove members to create agents",
		ScopeType:    "grove",
		ScopeID:      grove.ID,
		ResourceType: "agent",
		Actions:      []string{"create"},
		Effect:       "allow",
	}
	if err := s.store.CreatePolicy(ctx, policy); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to create grove member policy",
				"grove", grove.ID, "policy", policyName, "error", err)
			return
		}
		// Policy already exists — look it up and update its scope ID in case the
		// grove was recreated. Also ensure the binding to the current members group.
		existing, lookupErr := s.store.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
		if lookupErr != nil || len(existing.Items) == 0 {
			slog.Warn("failed to look up existing grove member policy",
				"grove", grove.ID, "policy", policyName, "error", lookupErr)
			return
		}
		policy = &existing.Items[0]
		if policy.ScopeID != grove.ID {
			policy.ScopeID = grove.ID
			if updateErr := s.store.UpdatePolicy(ctx, policy); updateErr != nil {
				slog.Warn("failed to update existing grove member policy scope",
					"grove", grove.ID, "policy", policyName, "error", updateErr)
			}
		}
	}

	// Bind policy to the members group
	if err := s.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "group",
		PrincipalID:   membersGroup.ID,
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to bind grove member policy",
			"grove", grove.ID, "policy", policyName, "error", err)
	}
}

// hubNativeGrovePath returns the filesystem path for a hub-native grove workspace.
func hubNativeGrovePath(slug string) (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", fmt.Errorf("failed to get global dir: %w", err)
	}
	return filepath.Join(globalDir, "groves", slug), nil
}

// initHubNativeGrove initializes the filesystem workspace for a hub-native grove.
// It creates the workspace directory and seeds the .scion project structure with
// hub connection settings.
func (s *Server) initHubNativeGrove(grove *store.Grove) error {
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(workspacePath, 0755); err != nil {
		return fmt.Errorf("failed to create grove workspace directory: %w", err)
	}

	scionDir := filepath.Join(workspacePath, ".scion")
	if err := config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		return fmt.Errorf("failed to initialize .scion project: %w", err)
	}

	// Write hub connection settings into the seeded settings file.
	settingsUpdates := map[string]string{
		"hub.enabled":  "true",
		"hub.endpoint": s.config.HubEndpoint,
		"hub.groveId":  grove.ID,
		"grove_id":     grove.ID,
	}
	for key, value := range settingsUpdates {
		if err := config.UpdateSetting(scionDir, key, value, false); err != nil {
			slog.Warn("failed to update hub-native grove setting",
				"grove", grove.ID, "key", key, "error", err)
		}
	}

	return nil
}

// syncWorkspaceOnStop triggers a best-effort workspace sync-back for hub-native groves
// on remote brokers before the agent is stopped. It uploads the workspace from the
// broker to GCS via the control channel, then downloads from GCS to the Hub filesystem.
func (s *Server) syncWorkspaceOnStop(ctx context.Context, agent *store.Agent) {
	if agent.GroveID == "" || agent.RuntimeBrokerID == "" {
		return
	}

	grove, err := s.store.GetGrove(ctx, agent.GroveID)
	if err != nil || grove.GitRemote != "" {
		return // Not hub-native or grove not found
	}

	// Check if broker is co-located (embedded or has local path)
	if s.isEmbeddedBroker(agent.RuntimeBrokerID) {
		return // Embedded broker, no sync needed
	}
	provider, err := s.store.GetGroveProvider(ctx, grove.ID, agent.RuntimeBrokerID)
	if err == nil && provider.LocalPath != "" {
		return // Colocated broker, no sync needed
	}

	stor := s.GetStorage()
	cc := s.GetControlChannelManager()
	if stor == nil || cc == nil {
		return
	}

	storagePath := storage.GroveWorkspaceStoragePath(grove.ID)

	// Tunnel upload request to the broker
	uploadReq := RuntimeBrokerWorkspaceUploadRequest{
		Slug:        agent.Slug,
		StoragePath: storagePath,
	}
	var uploadResp RuntimeBrokerWorkspaceUploadResponse
	if err := tunnelWorkspaceRequest(ctx, cc, agent.RuntimeBrokerID, "POST", "/api/v1/workspace/upload", uploadReq, &uploadResp); err != nil {
		s.agentLifecycleLog.Warn("syncWorkspaceOnStop: failed to upload workspace from broker",
			"agent", agent.Name, "grove", grove.ID, "error", err)
		return
	}

	// Download from GCS to Hub filesystem
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	if err != nil {
		s.agentLifecycleLog.Warn("syncWorkspaceOnStop: failed to get grove path", "error", err)
		return
	}

	if err := gcp.SyncFromGCS(ctx, stor.Bucket(), storagePath+"/files", workspacePath); err != nil {
		s.agentLifecycleLog.Warn("syncWorkspaceOnStop: GCS download failed",
			"grove", grove.ID, "error", err)
	} else {
		s.agentLifecycleLog.Info("syncWorkspaceOnStop: workspace synced back to Hub",
			"grove", grove.ID, "path", workspacePath)
	}
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

		// Set ownership from authenticated user
		if user := GetUserIdentityFromContext(ctx); user != nil {
			grove.CreatedBy = user.ID()
			grove.OwnerID = user.ID()
		}

		if err := s.store.CreateGrove(ctx, grove); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		created = true

		// Create the associated grove_agents group (best-effort)
		s.createGroveGroup(ctx, grove)

		// Create grove members group and policy (best-effort)
		s.createGroveMembersGroupAndPolicy(ctx, grove)

		// Auto-link brokers that have auto_provide enabled
		s.autoLinkProviders(ctx, grove)
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
					"field":    "brokerId",
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
		Grove:       grove,
		Broker:      broker,
		Created:     created,
		BrokerToken: brokerToken,
		SecretKey:   secretKey,
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

	// Check for nested /broadcast path (message broker broadcast)
	if subPath == "broadcast" {
		s.handleGroveBroadcast(w, r, groveID)
		return
	}

	// Check for nested /scheduled-events path
	if strings.HasPrefix(subPath, "scheduled-events") {
		eventPath := strings.TrimPrefix(subPath, "scheduled-events")
		eventPath = strings.TrimPrefix(eventPath, "/")
		s.handleScheduledEvents(w, r, groveID, eventPath)
		return
	}

	// Check for nested /workspace/archive path (download workspace as zip)
	if subPath == "workspace/archive" {
		s.handleGroveWorkspaceArchive(w, r, groveID)
		return
	}

	// Check for nested /workspace/files path
	if strings.HasPrefix(subPath, "workspace/files") {
		filePath := strings.TrimPrefix(subPath, "workspace/files")
		filePath = strings.TrimPrefix(filePath, "/")
		s.handleGroveWorkspace(w, r, groveID, filePath)
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

	// Handle stop-all (POST /api/v1/groves/{groveId}/agents/stop-all)
	if agentPath == "stop-all" {
		s.handleStopAllAgents(w, r, grove.ID)
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
		GroveID:         groveID,
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
		Phase:           query.Get("phase"),
		IncludeDeleted:  query.Get("includeDeleted") == "true",
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

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	agents := make([]AgentWithCapabilities, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = agentResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "agent")
		for i := range result.Items {
			agents[i] = AgentWithCapabilities{Agent: result.Items[i], Cap: caps[i]}
		}
	} else {
		for i := range result.Items {
			agents[i] = AgentWithCapabilities{Agent: result.Items[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "grove", groveID, "agent")
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:       agents,
		NextCursor:   result.NextCursor,
		TotalCount:   result.TotalCount,
		ServerTime:   time.Now().UTC(),
		Capabilities: scopeCap,
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
	if req.CleanupMode != "" && req.CleanupMode != "strict" && req.CleanupMode != "force" {
		ValidationError(w, "cleanupMode must be 'strict' or 'force'", nil)
		return
	}

	// Resolve caller identity for creator tracking
	var createdBy string
	var creatorName string
	var notifySubscriberType, notifySubscriberID string
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		createdBy = agentIdent.ID()
		if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
			creatorName = creatorAgent.Name
			notifySubscriberType = store.SubscriberTypeAgent
			notifySubscriberID = creatorAgent.Slug
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
		creatorName = userIdent.Email()
		notifySubscriberType = store.SubscriberTypeUser
		notifySubscriberID = userIdent.ID()
	}
	s.createAgentInGrove(w, r, req, groveID, createdBy, creatorName, notifySubscriberType, notifySubscriberID)
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

	s.performAgentDelete(w, r, agent)
}

// handleGroveAgentAction handles actions on agents within a grove
func (s *Server) handleGroveAgentAction(w http.ResponseWriter, r *http.Request, groveID, agentID, action string) {
	// Cloud-logs actions are GET endpoints; handle before the POST-only gate.
	if action == "cloud-logs" || action == "cloud-logs/stream" {
		resolvedAgent, err := s.resolveGroveAgent(r.Context(), groveID, agentID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if action == "cloud-logs" {
			s.handleAgentCloudLogs(w, r, resolvedAgent.ID)
		} else {
			s.handleAgentCloudLogsStream(w, r, resolvedAgent.ID)
		}
		return
	}

	// Message-logs actions are GET endpoints; handle before the POST-only gate.
	if action == "message-logs" || action == "message-logs/stream" {
		resolvedAgent, err := s.resolveGroveAgent(r.Context(), groveID, agentID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if action == "message-logs" {
			s.handleAgentMessageLogs(w, r, resolvedAgent.ID)
		} else {
			s.handleAgentMessageLogsStream(w, r, resolvedAgent.ID)
		}
		return
	}

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

	// For interactive actions, enforce policy-based authorization (owner or admin only)
	switch action {
	case "start", "stop", "restart", "message":
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "agent",
				ID:      agent.ID,
				OwnerID: agent.OwnerID,
			}, ActionAttach)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"Only the agent's creator can interact with it", nil)
				return
			}
		}
	}

	switch action {
	case "status":
		s.updateAgentStatus(w, r, agent.ID)
	case "start", "stop", "restart":
		s.handleAgentLifecycle(w, r, agent.ID, action)
	case "message":
		s.handleAgentMessage(w, r, agent.ID)
	case "env":
		s.submitAgentEnv(w, r, groveID, agentID)
	case "restore":
		s.restoreAgent(w, r, agent.ID)
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
	ctx := r.Context()
	grove, err := s.store.GetGrove(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := GroveWithCapabilities{Grove: *grove}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, groveResource(grove))
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateGrove(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	grove, err := s.store.GetGrove(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updates struct {
		Name                   string            `json:"name,omitempty"`
		Labels                 map[string]string `json:"labels,omitempty"`
		Visibility             string            `json:"visibility,omitempty"`
		DefaultRuntimeBrokerID string            `json:"defaultRuntimeBrokerId,omitempty"`
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
	if updates.DefaultRuntimeBrokerID != "" {
		grove.DefaultRuntimeBrokerID = updates.DefaultRuntimeBrokerID
	}

	if err := s.store.UpdateGrove(ctx, grove); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.events.PublishGroveUpdated(ctx, grove)

	writeJSON(w, http.StatusOK, grove)
}

func (s *Server) deleteGrove(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Fetch the grove record before deletion so we can clean up the filesystem.
	grove, err := s.store.GetGrove(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Dispatch agent deletions to runtime brokers so containers are stopped
	// and agent files are cleaned up. The DB cascade will remove agent records,
	// but we need the broker to tear down the actual resources first.
	deleteAgents := r.URL.Query().Get("deleteAgents") == "true"
	if deleteAgents {
		s.deleteGroveAgents(ctx, grove)
	}

	// Clean up all groups associated with the grove (agents group, members group, etc.)
	if groveGroups, err := s.store.ListGroups(ctx, store.GroupFilter{GroveID: id}, store.ListOptions{Limit: 100}); err == nil {
		for _, g := range groveGroups.Items {
			if delErr := s.store.DeleteGroup(ctx, g.ID); delErr != nil {
				slog.Warn("failed to delete grove group", "grove", id, "group", g.ID, "slug", g.Slug, "error", delErr)
			}
		}
	}

	// Clean up grove-scoped policies (best-effort)
	if grovePolicies, err := s.store.ListPolicies(ctx, store.PolicyFilter{ScopeType: "grove", ScopeID: id}, store.ListOptions{Limit: 100}); err == nil {
		for _, p := range grovePolicies.Items {
			if delErr := s.store.DeletePolicy(ctx, p.ID); delErr != nil {
				slog.Warn("failed to delete grove policy", "grove", id, "policy", p.ID, "name", p.Name, "error", delErr)
			}
		}
	}

	// For hub-native groves, notify provider brokers to clean up their
	// local grove directories. This must run before DeleteGrove because
	// the cascade deletes the grove_providers we need to enumerate.
	if grove.GitRemote == "" {
		s.cleanupBrokerGroveDirectories(ctx, grove)
	}

	if err := s.store.DeleteGrove(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// For hub-native groves (no git remote), remove the filesystem directory.
	if grove.GitRemote == "" && grove.Slug != "" {
		if grovePath, err := hubNativeGrovePath(grove.Slug); err == nil {
			if err := util.RemoveAllSafe(grovePath); err != nil {
				slog.Warn("failed to remove hub-native grove directory",
					"grove", id, "slug", grove.Slug, "path", grovePath, "error", err)
			}
		}
	}

	s.events.PublishGroveDeleted(ctx, id)

	w.WriteHeader(http.StatusNoContent)
}

// deleteGroveAgents dispatches deletion of all agents in a grove to their
// runtime brokers. This is best-effort: failures are logged but do not block
// grove deletion. The database cascade will remove agent records regardless.
func (s *Server) deleteGroveAgents(ctx context.Context, grove *store.Grove) {
	dispatcher := s.GetDispatcher()

	result, err := s.store.ListAgents(ctx, store.AgentFilter{GroveID: grove.ID}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.agentLifecycleLog.Warn("failed to list agents for grove deletion", "grove", grove.ID, "error", err)
		return
	}

	now := time.Now()
	for _, agent := range result.Items {
		if !agent.DeletedAt.IsZero() {
			continue
		}
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			if err := dispatcher.DispatchAgentDelete(ctx, &agent, true, true, false, now); err != nil {
				s.agentLifecycleLog.Warn("failed to dispatch agent delete during grove deletion",
					"agent", agent.ID, "broker", agent.RuntimeBrokerID, "error", err)
			}
		}
		s.events.PublishAgentDeleted(ctx, agent.ID, agent.GroveID)
	}
}

// cleanupBrokerGroveDirectories notifies provider brokers to remove their local
// copies of a hub-native grove directory. This is best-effort: failures are
// logged but do not block grove deletion. The embedded broker is skipped
// because the hub already cleans up its own filesystem copy.
func (s *Server) cleanupBrokerGroveDirectories(ctx context.Context, grove *store.Grove) {
	if grove.Slug == "" {
		return
	}

	providers, err := s.store.GetGroveProviders(ctx, grove.ID)
	if err != nil {
		slog.Warn("failed to get grove providers for cleanup", "grove", grove.ID, "error", err)
		return
	}

	if len(providers) == 0 {
		return
	}

	// Get the RuntimeBrokerClient from the dispatcher.
	var client RuntimeBrokerClient
	if disp := s.GetDispatcher(); disp != nil {
		if httpDisp, ok := disp.(*HTTPAgentDispatcher); ok {
			client = httpDisp.GetClient()
		}
	}
	if client == nil {
		slog.Warn("no RuntimeBrokerClient available for grove cleanup dispatch", "grove", grove.ID)
		return
	}

	for _, provider := range providers {
		// Skip the embedded broker — the hub already cleans up its own copy.
		if s.isEmbeddedBroker(provider.BrokerID) {
			continue
		}

		broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
		if err != nil {
			slog.Warn("failed to get broker for grove cleanup",
				"grove", grove.ID, "broker", provider.BrokerID, "error", err)
			continue
		}

		endpoint := broker.Endpoint
		if endpoint == "" {
			continue
		}

		if err := client.CleanupGrove(ctx, provider.BrokerID, endpoint, grove.Slug); err != nil {
			slog.Warn("failed to cleanup grove on broker",
				"grove", grove.ID, "slug", grove.Slug,
				"broker", provider.BrokerID, "endpoint", endpoint, "error", err)
		}
	}
}

// ============================================================================
// RuntimeBroker Endpoints
// ============================================================================

type ListRuntimeBrokersResponse struct {
	Brokers    []store.RuntimeBroker `json:"brokers"`
	NextCursor string                `json:"nextCursor,omitempty"`
	TotalCount int                   `json:"totalCount"`
}

// RuntimeBrokerWithProvider extends RuntimeBroker with grove-specific provider data.
// This is returned when listing brokers filtered by groveId, providing the local path
// for the grove on each broker.
type RuntimeBrokerWithProvider struct {
	store.RuntimeBroker
	LocalPath string        `json:"localPath,omitempty"` // Filesystem path to the grove on this broker
	Cap       *Capabilities `json:"_capabilities,omitempty"`
}

// ListRuntimeBrokersWithProviderResponse is returned when filtering by groveId.
type ListRuntimeBrokersWithProviderResponse struct {
	Brokers    []RuntimeBrokerWithProvider `json:"brokers"`
	NextCursor string                      `json:"nextCursor,omitempty"`
	TotalCount int                         `json:"totalCount"`
}

// ListRuntimeBrokersWithCapsResponse is the standard broker list response with capabilities.
type ListRuntimeBrokersWithCapsResponse struct {
	Brokers    []RuntimeBrokerWithCapabilities `json:"brokers"`
	NextCursor string                          `json:"nextCursor,omitempty"`
	TotalCount int                             `json:"totalCount"`
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

	// Batch-resolve CreatedByName for all brokers
	s.enrichBrokerCreatorNames(ctx, result.Items)

	// Compute capabilities for the requesting user
	ident := GetIdentityFromContext(ctx)
	var caps []*Capabilities
	if ident != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = brokerResource(&result.Items[i])
		}
		caps = s.authzService.ComputeCapabilitiesBatch(ctx, ident, resources, "broker")
		// Auto-provide brokers grant dispatch to all authenticated users.
		for i, broker := range result.Items {
			if broker.AutoProvide && i < len(caps) && !capabilityAllows(caps[i], ActionDispatch) {
				caps[i].Actions = append(caps[i].Actions, string(ActionDispatch))
			}
		}
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
		for i, broker := range result.Items {
			if caps != nil && !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			eb := RuntimeBrokerWithProvider{
				RuntimeBroker: broker,
				LocalPath:     brokerLocalPaths[broker.ID],
			}
			if caps != nil && i < len(caps) {
				eb.Cap = caps[i]
			}
			extendedBrokers = append(extendedBrokers, eb)
		}

		totalCount := result.TotalCount
		if ident != nil {
			totalCount = len(extendedBrokers)
		}

		writeJSON(w, http.StatusOK, ListRuntimeBrokersWithProviderResponse{
			Brokers:    extendedBrokers,
			NextCursor: result.NextCursor,
			TotalCount: totalCount,
		})
		return
	}

	brokersWithCaps := make([]RuntimeBrokerWithCapabilities, 0, len(result.Items))
	for i, broker := range result.Items {
		if caps != nil && !capabilityAllows(caps[i], ActionRead) {
			continue
		}
		resp := RuntimeBrokerWithCapabilities{RuntimeBroker: broker}
		if caps != nil && i < len(caps) {
			resp.Cap = caps[i]
		}
		brokersWithCaps = append(brokersWithCaps, resp)
	}

	totalCount := result.TotalCount
	if ident != nil {
		totalCount = len(brokersWithCaps)
	}

	writeJSON(w, http.StatusOK, ListRuntimeBrokersWithCapsResponse{
		Brokers:    brokersWithCaps,
		NextCursor: result.NextCursor,
		TotalCount: totalCount,
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
	ctx := r.Context()
	broker, err := s.store.GetRuntimeBroker(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enrich CreatedByName
	if broker.CreatedBy != "" {
		if user, err := s.store.GetUser(ctx, broker.CreatedBy); err == nil {
			if user.DisplayName != "" {
				broker.CreatedByName = user.DisplayName
			} else {
				broker.CreatedByName = user.Email
			}
		}
	}

	// Compute capabilities for the requesting user
	resp := RuntimeBrokerWithCapabilities{RuntimeBroker: *broker}
	if ident := GetIdentityFromContext(ctx); ident != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, ident, brokerResource(broker))
		// Auto-provide brokers grant dispatch to all authenticated users.
		if broker.AutoProvide && !capabilityAllows(resp.Cap, ActionDispatch) {
			resp.Cap.Actions = append(resp.Cap.Actions, string(ActionDispatch))
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enforce authorization: only the broker owner or admins can update
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, brokerResource(broker), ActionUpdate)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
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

	// Get broker info before deletion for authz and audit logging
	broker, err := s.store.GetRuntimeBroker(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enforce authorization: only the broker owner or admins can delete
	var actorID string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		actorID = user.ID()
		decision := s.authzService.CheckAccess(ctx, user, brokerResource(broker), ActionDelete)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	}

	brokerName := broker.Name

	// Explicitly remove all grove provider records for this broker.
	// While the DB schema has ON DELETE CASCADE, we do this at the
	// application level to ensure cleanup regardless of DB behavior
	// and to clear default_runtime_broker_id on affected groves.
	clientIP := getClientIP(r)
	if groves, err := s.store.GetBrokerGroves(ctx, id); err == nil {
		for _, gp := range groves {
			_ = s.store.RemoveGroveProvider(ctx, gp.GroveID, id)
			LogUnlinkEvent(ctx, s.auditLogger, id, gp.GroveID, actorID, clientIP)

			// Clear default_runtime_broker_id if it points to this broker
			if grove, err := s.store.GetGrove(ctx, gp.GroveID); err == nil {
				if grove.DefaultRuntimeBrokerID == id {
					grove.DefaultRuntimeBrokerID = ""
					_ = s.store.UpdateGrove(ctx, grove)
				}
			}
		}
	}

	if err := s.store.DeleteRuntimeBroker(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Log the deregistration event
	LogDeregisterEvent(ctx, s.auditLogger, id, brokerName, actorID, clientIP)

	w.WriteHeader(http.StatusNoContent)
}

// checkBrokerDispatchAccess verifies that the current user has dispatch permission
// on the given broker. Returns true if access is granted. If denied, it writes a
// 403 response and returns false. If the broker cannot be found, it writes an error
// and returns false.
func (s *Server) checkBrokerDispatchAccess(ctx context.Context, w http.ResponseWriter, brokerID string) bool {
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		// No user identity (e.g. broker-to-broker) — allow
		return true
	}
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	// Auto-provide brokers are shared infrastructure (e.g. a combo hub-broker
	// server's default broker) and are dispatchable by any authenticated user.
	if broker.AutoProvide {
		return true
	}
	decision := s.authzService.CheckAccess(ctx, userIdent, brokerResource(broker), ActionDispatch)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to create agents on this broker", nil)
		return false
	}
	return true
}

// enrichBrokerCreatorNames batch-resolves CreatedBy UUIDs to display names for a slice of brokers.
func (s *Server) enrichBrokerCreatorNames(ctx context.Context, brokers []store.RuntimeBroker) {
	// Collect unique creator IDs
	creatorIDs := make(map[string]struct{})
	for _, b := range brokers {
		if b.CreatedBy != "" {
			creatorIDs[b.CreatedBy] = struct{}{}
		}
	}
	if len(creatorIDs) == 0 {
		return
	}

	// Resolve each unique creator ID to a display name
	nameMap := make(map[string]string, len(creatorIDs))
	for id := range creatorIDs {
		if user, err := s.store.GetUser(ctx, id); err == nil {
			if user.DisplayName != "" {
				nameMap[id] = user.DisplayName
			} else {
				nameMap[id] = user.Email
			}
		}
	}

	// Apply resolved names
	for i := range brokers {
		if name, ok := nameMap[brokers[i].CreatedBy]; ok {
			brokers[i].CreatedByName = name
		}
	}
}

// brokerHeartbeatRequest is the request body for broker heartbeats.
type brokerHeartbeatRequest struct {
	Status string                 `json:"status"`
	Groves []brokerGroveHeartbeat `json:"groves,omitempty"`
}

// brokerGroveHeartbeat is per-grove status in a heartbeat.
type brokerGroveHeartbeat struct {
	GroveID    string                 `json:"groveId"`
	AgentCount int                    `json:"agentCount"`
	Agents     []brokerAgentHeartbeat `json:"agents,omitempty"`
}

// brokerAgentHeartbeat is per-agent status in a heartbeat.
type brokerAgentHeartbeat struct {
	Slug            string `json:"slug"`   // Agent's URL-safe identifier (name)
	Status          string `json:"status"` // Session status (IDLE, THINKING, etc.)
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	HarnessAuth     string `json:"harnessAuth,omitempty"` // Resolved auth method from container labels
	Profile         string `json:"profile,omitempty"`      // Settings profile used
}

func (s *Server) handleBrokerHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var heartbeat brokerHeartbeatRequest
	if err := readJSON(r, &heartbeat); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Update the broker's heartbeat status
	if err := s.store.UpdateRuntimeBrokerHeartbeat(ctx, id, heartbeat.Status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Process agent status updates from each grove
	for _, grove := range heartbeat.Groves {
		for _, agentHB := range grove.Agents {
			// Look up the agent by name (slug) within the grove
			agent, err := s.store.GetAgentBySlug(ctx, grove.GroveID, agentHB.Slug)
			if err != nil {
				// Agent not found in this grove - skip silently
				// This can happen if the agent exists locally but isn't registered on the Hub
				continue
			}

			// Security check: ensure the agent belongs to this broker
			if agent.RuntimeBrokerID != id {
				slog.Warn("Broker attempted to update agent owned by different broker",
					"brokerID", id,
					"agentBrokerID", agent.RuntimeBrokerID,
					"agentID", agent.ID)
				continue
			}

			// Build status update with agent status and container status.
			// When the broker sends structured Phase/Activity fields, use
			// them directly. Fall back to container-status derivation for
			// backward compatibility with older brokers.
			statusUpdate := store.AgentStatusUpdate{
				ContainerStatus: agentHB.ContainerStatus,
				Heartbeat:       true, // Ensures LastSeen is updated
			}

			if agentHB.Phase != "" {
				// Structured path: broker sent Phase/Activity directly
				statusUpdate.Phase = agentHB.Phase
				// Only propagate Activity when it differs from the stored
				// value. Heartbeats always report the current activity, but
				// repeating the same value would refresh last_activity_event
				// on every heartbeat and prevent stalled detection from
				// ever triggering.
				if agentHB.Activity != agent.Activity {
					if agent.Activity == string(state.ActivityStalled) {
						// The agent is currently marked stalled. Only clear the
						// stall if the broker reports a genuinely different
						// activity than what caused the stall. If the broker is
						// still reporting the same pre-stall activity, the agent
						// hasn't recovered — keep it stalled.
						if agentHB.Activity != agent.StalledFromActivity {
							statusUpdate.Activity = agentHB.Activity
						}
					} else {
						statusUpdate.Activity = agentHB.Activity
					}
				}
			} else {
				// Legacy path: no structured fields, derive from ContainerStatus
				// Derive phase from container status to ensure agents
				// registered via sync (not started via hub) get proper state.
				// Terminal container states (exited/stopped) override agent phase.
				if agentHB.ContainerStatus != "" {
					containerStatusLower := strings.ToLower(agentHB.ContainerStatus)
					switch {
					case strings.HasPrefix(containerStatusLower, "up") || containerStatusLower == "running":
						statusUpdate.Phase = string(state.PhaseRunning)
					case strings.HasPrefix(containerStatusLower, "exited") || containerStatusLower == "stopped":
						statusUpdate.Phase = string(state.PhaseStopped)
						statusUpdate.Activity = ""
					case containerStatusLower == "created":
						// Don't downgrade a running agent to provisioning — the
						// container may briefly report "created" while the runtime
						// is transitioning to started.
						if agent.Phase != string(state.PhaseRunning) {
							statusUpdate.Phase = string(state.PhaseProvisioning)
						}
					}
				}
			}

			// Backfill HarnessAuth and Profile from heartbeat if the agent record is missing them.
			// This covers agents created before tracking was added, or
			// agents where values were auto-detected rather than explicitly set.
			needsUpdate := false
			if agentHB.HarnessAuth != "" && (agent.AppliedConfig == nil || agent.AppliedConfig.HarnessAuth == "") {
				if agent.AppliedConfig == nil {
					agent.AppliedConfig = &store.AgentAppliedConfig{}
				}
				agent.AppliedConfig.HarnessAuth = agentHB.HarnessAuth
				needsUpdate = true
			}
			if agentHB.Profile != "" && (agent.AppliedConfig == nil || agent.AppliedConfig.Profile == "") {
				if agent.AppliedConfig == nil {
					agent.AppliedConfig = &store.AgentAppliedConfig{}
				}
				agent.AppliedConfig.Profile = agentHB.Profile
				needsUpdate = true
			}
			if needsUpdate {
				if err := s.store.UpdateAgent(ctx, agent); err != nil {
					slog.Warn("Failed to backfill agent config from heartbeat",
						"agentID", agent.ID, "harnessAuth", agentHB.HarnessAuth, "profile", agentHB.Profile, "error", err)
				}
			}

			// Update the agent's status
			if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
				// Log error but continue processing other agents
				slog.Error("Failed to update agent status from heartbeat",
					"agentID", agent.ID,
					"agentSlug", agentHB.Slug,
					"groveID", grove.GroveID,
					"error", err)
			} else {
				// Publish SSE event so the frontend receives activity updates
				if updated, err := s.store.GetAgent(ctx, agent.ID); err == nil {
					s.events.PublishAgentStatus(ctx, updated)
				}
			}
		}
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
	Templates    []TemplateWithCapabilities `json:"templates"`
	NextCursor   string                     `json:"nextCursor,omitempty"`
	TotalCount   int                        `json:"totalCount"`
	Capabilities *Capabilities              `json:"_capabilities,omitempty"`
}

// ============================================================================
// HarnessConfig Endpoints
// ============================================================================

// ListHarnessConfigsResponse is the response for listing harness configs.
type ListHarnessConfigsResponse struct {
	HarnessConfigs []store.HarnessConfig `json:"harnessConfigs"`
	NextCursor     string                `json:"nextCursor,omitempty"`
	TotalCount     int                   `json:"totalCount"`
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

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	templates := make([]TemplateWithCapabilities, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = templateResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "template")
		for i := range result.Items {
			templates[i] = TemplateWithCapabilities{Template: result.Items[i], Cap: caps[i]}
		}
	} else {
		for i := range result.Items {
			templates[i] = TemplateWithCapabilities{Template: result.Items[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "template")
	}

	writeJSON(w, http.StatusOK, ListTemplatesResponse{
		Templates:    templates,
		NextCursor:   result.NextCursor,
		TotalCount:   result.TotalCount,
		Capabilities: scopeCap,
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
	ctx := r.Context()
	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := TemplateWithCapabilities{Template: *template}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, templateResource(template))
	}

	writeJSON(w, http.StatusOK, resp)
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
	Users        []UserWithCapabilities `json:"users"`
	NextCursor   string                 `json:"nextCursor,omitempty"`
	TotalCount   int                    `json:"totalCount"`
	Capabilities *Capabilities          `json:"_capabilities,omitempty"`
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

	// Compute per-item capabilities (users have no scope-level create action)
	identity := GetIdentityFromContext(ctx)
	users := make([]UserWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = userResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "user")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			users = append(users, UserWithCapabilities{User: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			users = append(users, UserWithCapabilities{User: result.Items[i]})
		}
	}

	totalCount := result.TotalCount
	if identity != nil {
		totalCount = len(users)
	}

	writeJSON(w, http.StatusOK, ListUsersResponse{
		Users:      users,
		NextCursor: result.NextCursor,
		TotalCount: totalCount,
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	// User creation is managed by the hub's internal sign-in flows (OAuth).
	// Direct API creation is not permitted.
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"user creation is managed through sign-in flows and cannot be performed via the API", nil)
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
	ctx := r.Context()
	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := UserWithCapabilities{User: *user}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, userResource(user))
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updates struct {
		DisplayName string                 `json:"displayName,omitempty"`
		Role        string                 `json:"role,omitempty"`
		Status      string                 `json:"status,omitempty"`
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
	Value         string `json:"value"`
	Scope         string `json:"scope,omitempty"`
	ScopeID       string `json:"scopeId,omitempty"`
	Description   string `json:"description,omitempty"`
	Sensitive     bool   `json:"sensitive,omitempty"`
	InjectionMode string `json:"injectionMode,omitempty"`
	Secret        bool   `json:"secret,omitempty"`
}

type SetEnvVarResponse struct {
	EnvVar  *store.EnvVar `json:"envVar"`
	Created bool          `json:"created"`
}

// resolveEnvSecretAccess resolves the scopeID and enforces authorization for
// env var and secret endpoints. It returns the resolved scopeID and true on
// success, or writes an HTTP error and returns false on failure.
//
// For user scope: extracts the authenticated user's ID as scopeID (ignoring
// any client-supplied value). No CheckAccess call needed — identity enforcement
// is the access control.
//
// For grove scope: verifies the grove exists, then checks authorization. Users
// must pass CheckAccess (with owner bypass). Agents get read-only access to
// their own grove only.
//
// For broker scope: verifies the broker exists. Brokers get self-access via
// BrokerIdentity. Users must pass CheckAccess.
func (s *Server) resolveEnvSecretAccess(w http.ResponseWriter, r *http.Request, scope, clientScopeID string, isWrite bool) (string, bool) {
	ctx := r.Context()

	switch scope {
	case store.ScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			Unauthorized(w)
			return "", false
		}
		return userIdent.ID(), true

	case store.ScopeGrove:
		if clientScopeID == "" {
			BadRequest(w, "scopeId is required for grove scope")
			return "", false
		}
		grove, err := s.store.GetGrove(ctx, clientScopeID)
		if err != nil {
			if err == store.ErrNotFound {
				NotFound(w, "Grove")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return "", false
		}
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if agentIdent, ok := identity.(AgentIdentity); ok {
			if isWrite {
				Forbidden(w)
				return "", false
			}
			if agentIdent.GroveID() != clientScopeID {
				Forbidden(w)
				return "", false
			}
			return clientScopeID, true
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "grove",
				ID:      grove.ID,
				OwnerID: grove.OwnerID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return "", false
			}
			return clientScopeID, true
		}
		Forbidden(w)
		return "", false

	case store.ScopeRuntimeBroker:
		if clientScopeID == "" {
			BadRequest(w, "scopeId is required for runtime_broker scope")
			return "", false
		}
		_, err := s.store.GetRuntimeBroker(ctx, clientScopeID)
		if err != nil {
			if err == store.ErrNotFound {
				NotFound(w, "RuntimeBroker")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return "", false
		}
		// Broker self-access
		if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil {
			if brokerIdent.BrokerID() == clientScopeID {
				return clientScopeID, true
			}
		}
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   clientScopeID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return "", false
			}
			return clientScopeID, true
		}
		Forbidden(w)
		return "", false

	case store.ScopeHub:
		// Hub scope: only admin users can read or write.
		// Agents and brokers retain read access for env/secret injection.
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			if userIdent.Role() != store.UserRoleAdmin {
				Forbidden(w)
				return "", false
			}
		} else if isWrite {
			// Non-user identities (agents, brokers) can only read.
			Forbidden(w)
			return "", false
		}
		return store.ScopeIDHub, true

	default:
		BadRequest(w, "invalid scope: "+scope)
		return "", false
	}
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
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

	// Merge environment-type secrets into the env var list
	if s.secretBackend != nil {
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   scope,
			ScopeID: scopeID,
			Type:    "environment",
		})
		if err != nil {
			s.envSecretLog.Warn("failed to list environment secrets for env var merge", "error", err)
		} else {
			// Build set of secret keys for deduplication
			secretKeys := make(map[string]struct{}, len(metas))
			for _, m := range metas {
				secretKeys[m.Name] = struct{}{}
				envVars = append(envVars, secretMetaToEnvVar(m))
			}
			// Remove stale plain env var records that are shadowed by secrets
			if len(secretKeys) > 0 {
				deduped := make([]store.EnvVar, 0, len(envVars))
				for _, ev := range envVars {
					if _, isShadowed := secretKeys[ev.Key]; isShadowed && !ev.Secret {
						continue
					}
					deduped = append(deduped, ev)
				}
				envVars = deduped
			}
		}
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}

	envVar, err := s.store.GetEnvVar(ctx, key, scope, scopeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
			// Fallback: check if this key exists as an environment secret
			meta, metaErr := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
			if metaErr == nil && meta.SecretType == "environment" {
				ev := secretMetaToEnvVar(*meta)
				writeJSON(w, http.StatusOK, &ev)
				return
			}
		}
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, req.ScopeID, true)
	if !ok {
		return
	}

	var createdBy string
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
	}

	// Secret promotion: route secret-flagged writes to the secret backend
	if req.Secret {
		if s.secretBackend == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"error": "secret storage requires a configured secrets backend",
			})
			return
		}

		input := &secret.SetSecretInput{
			Name:          key,
			Value:         req.Value,
			SecretType:    "environment",
			Target:        key,
			Scope:         scope,
			ScopeID:       scopeID,
			Description:   req.Description,
			InjectionMode: req.InjectionMode,
			CreatedBy:     createdBy,
			UpdatedBy:     createdBy,
		}
		created, meta, err := s.secretBackend.Set(ctx, input)
		if err != nil {
			if errors.Is(err, secret.ErrNoSecretBackend) {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		// Clean up any stale plain env var record for the same key/scope
		_ = s.store.DeleteEnvVar(ctx, key, scope, scopeID)

		syntheticEnvVar := secretMetaToEnvVar(*meta)
		writeJSON(w, http.StatusOK, SetEnvVarResponse{
			EnvVar:  &syntheticEnvVar,
			Created: created,
		})
		return
	}

	// Plain env var write
	injectionMode := req.InjectionMode
	if injectionMode == "" {
		injectionMode = store.InjectionModeAsNeeded
	}

	envVar := &store.EnvVar{
		ID:            api.NewUUID(),
		Key:           key,
		Value:         req.Value,
		Scope:         scope,
		ScopeID:       scopeID,
		Description:   req.Description,
		Sensitive:     req.Sensitive,
		InjectionMode: injectionMode,
		Secret:        false,
	}
	envVar.CreatedBy = createdBy

	created, err := s.store.UpsertEnvVar(ctx, envVar)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Clean up any existing secret with same key (demotion from secret to plain)
	if s.secretBackend != nil {
		_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), true)
	if !ok {
		return
	}

	if err := s.store.DeleteEnvVar(ctx, key, scope, scopeID); err != nil {
		if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
			// Fallback: try deleting from the secret backend
			if secErr := s.secretBackend.Delete(ctx, key, scope, scopeID); secErr == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Also clean up any secret with the same key
	if s.secretBackend != nil {
		_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
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
	Value         string `json:"value"`
	Scope         string `json:"scope,omitempty"`
	ScopeID       string `json:"scopeId,omitempty"`
	Description   string `json:"description,omitempty"`
	InjectionMode string `json:"injectionMode,omitempty"` // "always" or "as_needed" (default: as_needed)
	Type          string `json:"type,omitempty"`          // environment (default), variable, file
	Target        string `json:"target,omitempty"`        // Projection target (defaults to key)
}

type SetSecretResponse struct {
	Secret  *store.Secret `json:"secret"`
	Created bool          `json:"created"`
}

// metaToStoreSecret converts a secret.SecretMeta to a store.Secret for API response compatibility.
func metaToStoreSecret(m secret.SecretMeta) store.Secret {
	return store.Secret{
		ID:            m.ID,
		Key:           m.Name,
		SecretType:    m.SecretType,
		Target:        m.Target,
		Scope:         m.Scope,
		ScopeID:       m.ScopeID,
		Description:   m.Description,
		InjectionMode: m.InjectionMode,
		Version:       m.Version,
		Created:       m.Created,
		Updated:       m.Updated,
		CreatedBy:     m.CreatedBy,
		UpdatedBy:     m.UpdatedBy,
	}
}

// secretMetaToEnvVar converts a secret.SecretMeta (with type "environment") to a store.EnvVar
// for inclusion in unified env var list responses.
func secretMetaToEnvVar(m secret.SecretMeta) store.EnvVar {
	return store.EnvVar{
		ID:            m.ID,
		Key:           m.Name,
		Value:         "********",
		Scope:         m.Scope,
		ScopeID:       m.ScopeID,
		Description:   m.Description,
		Sensitive:     true,
		Secret:        true,
		InjectionMode: store.InjectionModeAsNeeded,
		Created:       m.Created,
		Updated:       m.Updated,
		CreatedBy:     m.CreatedBy,
	}
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}

	// Admin users listing user-scoped secrets can see all users' secrets.
	// This handles the dev-auth case where the dev user has a different ID
	// than OAuth users who may have stored secrets via CLI.
	if scope == store.ScopeUser {
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil && userIdent.Role() == store.UserRoleAdmin {
			scopeID = ""
		}
	}

	metas, err := s.secretBackend.List(ctx, secret.Filter{
		Scope:   scope,
		ScopeID: scopeID,
		Name:    query.Get("key"),
		Type:    query.Get("type"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// Convert to store.Secret for response compatibility
	secrets := make([]store.Secret, len(metas))
	for i, m := range metas {
		secrets[i] = metaToStoreSecret(m)
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}

	meta, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))
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

	// Validate and default secret type
	secretType := req.Type
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	switch secretType {
	case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
		// valid
	default:
		ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{
			"field": "type",
			"value": secretType,
		})
		return
	}

	// Default target to key
	target := req.Target
	if target == "" {
		target = key
	}

	// Validate file-specific constraints
	if secretType == store.SecretTypeFile {
		if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
			ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{
				"field": "target",
				"value": target,
			})
			return
		}
		// Enforce 64 KiB limit for file secrets
		if len(req.Value) > 64*1024 {
			ValidationError(w, "file secret value exceeds 64 KiB limit", map[string]interface{}{
				"field": "value",
				"limit": "65536 bytes",
				"size":  len(req.Value),
			})
			return
		}
	}

	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, req.ScopeID, true)
	if !ok {
		return
	}

	input := &secret.SetSecretInput{
		Name:          key,
		Value:         req.Value,
		SecretType:    secretType,
		Target:        target,
		Scope:         scope,
		ScopeID:       scopeID,
		Description:   req.Description,
		InjectionMode: req.InjectionMode,
	}

	// Populate CreatedBy/UpdatedBy from authenticated user
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		input.CreatedBy = userIdent.ID()
		input.UpdatedBy = userIdent.ID()
		if scope == store.ScopeUser {
			input.UserEmail = userIdent.Email()
		}
	}

	created, meta, err := s.secretBackend.Set(ctx, input)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	result := metaToStoreSecret(*meta)
	writeJSON(w, http.StatusOK, SetSecretResponse{
		Secret:  &result,
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), true)
	if !ok {
		return
	}

	if err := s.secretBackend.Delete(ctx, key, scope, scopeID); err != nil {
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
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.GroveID() != groveID {
			Forbidden(w)
			return
		}
		// Agents only get read access
	} else if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "grove",
			ID:      grove.ID,
			OwnerID: grove.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
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
		// Merge environment-type secrets
		if s.secretBackend != nil {
			metas, err := s.secretBackend.List(ctx, secret.Filter{
				Scope:   store.ScopeGrove,
				ScopeID: groveID,
				Type:    "environment",
			})
			if err != nil {
				s.envSecretLog.Warn("failed to list environment secrets for grove env var merge", "error", err)
			} else {
				secretKeys := make(map[string]struct{}, len(metas))
				for _, m := range metas {
					secretKeys[m.Name] = struct{}{}
					envVars = append(envVars, secretMetaToEnvVar(m))
				}
				if len(secretKeys) > 0 {
					deduped := make([]store.EnvVar, 0, len(envVars))
					for _, ev := range envVars {
						if _, isShadowed := secretKeys[ev.Key]; isShadowed && !ev.Secret {
							continue
						}
						deduped = append(deduped, ev)
					}
					envVars = deduped
				}
			}
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
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if isWrite {
			Forbidden(w)
			return
		}
		if agentIdent.GroveID() != groveID {
			Forbidden(w)
			return
		}
	} else if userIdent, ok := identity.(UserIdentity); ok {
		action := ActionRead
		if isWrite {
			action = ActionUpdate
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "grove",
			ID:      grove.ID,
			OwnerID: grove.OwnerID,
		}, action)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeGrove, groveID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				meta, metaErr := s.secretBackend.GetMeta(ctx, key, store.ScopeGrove, groveID)
				if metaErr == nil && meta.SecretType == "environment" {
					ev := secretMetaToEnvVar(*meta)
					writeJSON(w, http.StatusOK, &ev)
					return
				}
			}
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

		var createdBy string
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
		}

		// Secret promotion
		if req.Secret {
			if s.secretBackend == nil {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			input := &secret.SetSecretInput{
				Name:          key,
				Value:         req.Value,
				SecretType:    "environment",
				Target:        key,
				Scope:         store.ScopeGrove,
				ScopeID:       groveID,
				Description:   req.Description,
				InjectionMode: req.InjectionMode,
				CreatedBy:     createdBy,
				UpdatedBy:     createdBy,
			}
			created, meta, err := s.secretBackend.Set(ctx, input)
			if err != nil {
				if errors.Is(err, secret.ErrNoSecretBackend) {
					writeJSON(w, http.StatusNotImplemented, map[string]string{
						"error": "secret storage requires a configured secrets backend",
					})
					return
				}
				writeErrorFromErr(w, err, "")
				return
			}
			_ = s.store.DeleteEnvVar(ctx, key, store.ScopeGrove, groveID)
			syntheticEnvVar := secretMetaToEnvVar(*meta)
			writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: &syntheticEnvVar, Created: created})
			return
		}

		// Plain env var write
		groveInjectionMode := req.InjectionMode
		if groveInjectionMode == "" {
			groveInjectionMode = store.InjectionModeAsNeeded
		}
		envVar := &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         req.Value,
			Scope:         store.ScopeGrove,
			ScopeID:       groveID,
			Description:   req.Description,
			Sensitive:     req.Sensitive,
			InjectionMode: groveInjectionMode,
			Secret:        false,
		}
		envVar.CreatedBy = createdBy
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Demotion cleanup
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeGrove, groveID)
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeGrove, groveID); err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				if secErr := s.secretBackend.Delete(ctx, key, store.ScopeGrove, groveID); secErr == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeGrove, groveID)
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGroveSecrets(w http.ResponseWriter, r *http.Request, groveID string) {
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

	// Authorize access
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.GroveID() != groveID {
			Forbidden(w)
			return
		}
		// Agents only get read access
	} else if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "grove",
			ID:      grove.ID,
			OwnerID: grove.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   store.ScopeGrove,
			ScopeID: groveID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		secrets := make([]store.Secret, len(metas))
		for i, m := range metas {
			secrets[i] = metaToStoreSecret(m)
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
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if isWrite {
			Forbidden(w)
			return
		}
		if agentIdent.GroveID() != groveID {
			Forbidden(w)
			return
		}
	} else if userIdent, ok := identity.(UserIdentity); ok {
		action := ActionRead
		if isWrite {
			action = ActionUpdate
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "grove",
			ID:      grove.ID,
			OwnerID: grove.OwnerID,
		}, action)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeGrove, groveID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))

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
		secretType := req.Type
		if secretType == "" {
			secretType = store.SecretTypeEnvironment
		}
		switch secretType {
		case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
		default:
			ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{"field": "type", "value": secretType})
			return
		}
		target := req.Target
		if target == "" {
			target = key
		}
		if secretType == store.SecretTypeFile {
			if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
				ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{"field": "target", "value": target})
				return
			}
			if len(req.Value) > 64*1024 {
				ValidationError(w, "file secret value exceeds 64 KiB limit", map[string]interface{}{"field": "value", "limit": "65536 bytes", "size": len(req.Value)})
				return
			}
		}
		input := &secret.SetSecretInput{
			Name:          key,
			Value:         req.Value,
			SecretType:    secretType,
			Target:        target,
			Scope:         store.ScopeGrove,
			ScopeID:       groveID,
			Description:   req.Description,
			InjectionMode: req.InjectionMode,
		}
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			input.CreatedBy = userIdent.ID()
			input.UpdatedBy = userIdent.ID()
		}
		created, meta, err := s.secretBackend.Set(ctx, input)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		result := metaToStoreSecret(*meta)
		writeJSON(w, http.StatusOK, SetSecretResponse{Secret: &result, Created: created})

	case http.MethodDelete:
		if err := s.secretBackend.Delete(ctx, key, store.ScopeGrove, groveID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// autoLinkProviders links brokers with auto_provide enabled as providers for a grove.
// If the grove has no default runtime broker, the first auto-provided broker is set as default.
func (s *Server) autoLinkProviders(ctx context.Context, grove *store.Grove) {
	autoProvideTrue := true
	autoProviders, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{
		AutoProvide: &autoProvideTrue,
	}, store.ListOptions{})
	if err != nil {
		s.envSecretLog.Warn("Failed to query auto-provide brokers", "grove", grove.ID, "error", err)
		return
	}

	for _, autoBroker := range autoProviders.Items {
		provider := &store.GroveProvider{
			GroveID:    grove.ID,
			BrokerID:   autoBroker.ID,
			BrokerName: autoBroker.Name,
			Status:     autoBroker.Status,
			LinkedBy:   "auto-provide",
		}
		if addErr := s.store.AddGroveProvider(ctx, provider); addErr != nil {
			s.envSecretLog.Warn("Failed to auto-link broker to grove",
				"broker", autoBroker.Name, "grove", grove.ID, "error", addErr)
			continue
		}

		// Set first auto-provided broker as default if grove has none
		if grove.DefaultRuntimeBrokerID == "" {
			grove.DefaultRuntimeBrokerID = autoBroker.ID
			if updateErr := s.store.UpdateGrove(ctx, grove); updateErr != nil {
				s.envSecretLog.Warn("Failed to set default runtime broker",
					"broker", autoBroker.Name, "grove", grove.ID, "error", updateErr)
			}
		}
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
				"field":    "brokerId",
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

	// Authorize access: broker self-access or user CheckAccess
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own env vars — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
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
		// Merge environment-type secrets
		if s.secretBackend != nil {
			metas, err := s.secretBackend.List(ctx, secret.Filter{
				Scope:   store.ScopeRuntimeBroker,
				ScopeID: brokerID,
				Type:    "environment",
			})
			if err != nil {
				s.envSecretLog.Warn("failed to list environment secrets for broker env var merge", "error", err)
			} else {
				secretKeys := make(map[string]struct{}, len(metas))
				for _, m := range metas {
					secretKeys[m.Name] = struct{}{}
					envVars = append(envVars, secretMetaToEnvVar(m))
				}
				if len(secretKeys) > 0 {
					deduped := make([]store.EnvVar, 0, len(envVars))
					for _, ev := range envVars {
						if _, isShadowed := secretKeys[ev.Key]; isShadowed && !ev.Secret {
							continue
						}
						deduped = append(deduped, ev)
					}
					envVars = deduped
				}
			}
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

	// Authorize access: broker self-access or user CheckAccess
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own env vars — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				meta, metaErr := s.secretBackend.GetMeta(ctx, key, store.ScopeRuntimeBroker, brokerID)
				if metaErr == nil && meta.SecretType == "environment" {
					ev := secretMetaToEnvVar(*meta)
					writeJSON(w, http.StatusOK, &ev)
					return
				}
			}
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

		var createdBy string
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
		}

		// Secret promotion
		if req.Secret {
			if s.secretBackend == nil {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			input := &secret.SetSecretInput{
				Name:          key,
				Value:         req.Value,
				SecretType:    "environment",
				Target:        key,
				Scope:         store.ScopeRuntimeBroker,
				ScopeID:       brokerID,
				Description:   req.Description,
				InjectionMode: req.InjectionMode,
				CreatedBy:     createdBy,
				UpdatedBy:     createdBy,
			}
			created, meta, err := s.secretBackend.Set(ctx, input)
			if err != nil {
				if errors.Is(err, secret.ErrNoSecretBackend) {
					writeJSON(w, http.StatusNotImplemented, map[string]string{
						"error": "secret storage requires a configured secrets backend",
					})
					return
				}
				writeErrorFromErr(w, err, "")
				return
			}
			_ = s.store.DeleteEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID)
			syntheticEnvVar := secretMetaToEnvVar(*meta)
			writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: &syntheticEnvVar, Created: created})
			return
		}

		// Plain env var write
		brokerInjectionMode := req.InjectionMode
		if brokerInjectionMode == "" {
			brokerInjectionMode = store.InjectionModeAsNeeded
		}
		envVar := &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         req.Value,
			Scope:         store.ScopeRuntimeBroker,
			ScopeID:       brokerID,
			Description:   req.Description,
			Sensitive:     req.Sensitive,
			InjectionMode: brokerInjectionMode,
			Secret:        false,
		}
		envVar.CreatedBy = createdBy
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Demotion cleanup
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID)
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID); err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				if secErr := s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID); secErr == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID)
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

	// Authorize access: broker self-access or user CheckAccess
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own secrets — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		secrets := make([]store.Secret, len(metas))
		for i, m := range metas {
			secrets[i] = metaToStoreSecret(m)
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

	// Authorize access: broker self-access or user CheckAccess
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own secrets — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeRuntimeBroker, brokerID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))

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
		secretType := req.Type
		if secretType == "" {
			secretType = store.SecretTypeEnvironment
		}
		switch secretType {
		case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
		default:
			ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{"field": "type", "value": secretType})
			return
		}
		target := req.Target
		if target == "" {
			target = key
		}
		if secretType == store.SecretTypeFile {
			if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
				ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{"field": "target", "value": target})
				return
			}
			if len(req.Value) > 64*1024 {
				ValidationError(w, "file secret value exceeds 64 KiB limit", map[string]interface{}{"field": "value", "limit": "65536 bytes", "size": len(req.Value)})
				return
			}
		}
		input := &secret.SetSecretInput{
			Name:          key,
			Value:         req.Value,
			SecretType:    secretType,
			Target:        target,
			Scope:         store.ScopeRuntimeBroker,
			ScopeID:       brokerID,
			Description:   req.Description,
			InjectionMode: req.InjectionMode,
		}
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			input.CreatedBy = userIdent.ID()
			input.UpdatedBy = userIdent.ID()
		}
		created, meta, err := s.secretBackend.Set(ctx, input)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		result := metaToStoreSecret(*meta)
		writeJSON(w, http.StatusOK, SetSecretResponse{Secret: &result, Created: created})

	case http.MethodDelete:
		if err := s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID); err != nil {
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

// getHarnessConfigFromTemplate returns the harness config name from a resolved template,
// or the fallback value if no template was resolved.
func (s *Server) getHarnessConfigFromTemplate(template *store.Template, fallback string) string {
	if template != nil {
		if template.Harness != "" {
			return template.Harness
		}
	}
	return fallback
}

// buildAppliedConfig constructs an AgentAppliedConfig from a CreateAgentRequest.
// When req.Config is a ScionConfig, its fields are extracted into the applied config
// and the full ScionConfig is preserved as InlineConfig for threading to the broker.
func (s *Server) buildAppliedConfig(req CreateAgentRequest, harnessConfig string, creatorName string) *store.AgentAppliedConfig {
	ac := &store.AgentAppliedConfig{
		Profile:       req.Profile,
		HarnessConfig: harnessConfig,
		HarnessAuth:   req.HarnessAuth,
		Task:          req.Task,
		Attach:        req.Attach,
		Workspace:     req.Workspace,
		CreatorName:   creatorName,
	}

	if req.Config != nil {
		ac.Image = req.Config.Image
		ac.Env = req.Config.Env
		ac.Model = req.Config.Model

		// Extract ScionConfig-specific fields
		if req.Config.HarnessConfig != "" {
			ac.HarnessConfig = req.Config.HarnessConfig
		}
		if req.Config.AuthSelectedType != "" {
			ac.HarnessAuth = req.Config.AuthSelectedType
		}
		if req.Config.Task != "" && ac.Task == "" {
			ac.Task = req.Config.Task
		}

		// Preserve the full inline config for the broker
		ac.InlineConfig = req.Config
	}

	return ac
}

// populateAgentConfig enriches an agent's AppliedConfig with grove-derived and
// template-derived fields after the initial config block has been set up.
// It populates GitClone config from grove labels for git-anchored groves, and
// sets template ID, hash, and hub access scopes from the resolved template.
func (s *Server) populateAgentConfig(agent *store.Agent, grove *store.Grove, resolvedTemplate *store.Template) {
	if agent.AppliedConfig == nil {
		return
	}

	// Populate GitClone config for git-anchored groves.
	if grove != nil && grove.GitRemote != "" {
		cloneURL := grove.Labels["scion.dev/clone-url"]
		if cloneURL == "" {
			cloneURL = "https://" + grove.GitRemote + ".git"
		}
		defaultBranch := grove.Labels["scion.dev/default-branch"]
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		agent.AppliedConfig.GitClone = &api.GitCloneConfig{
			URL:    cloneURL,
			Branch: defaultBranch,
			Depth:  1,
		}
	}

	// Populate workspace path for hub-native groves (no git remote).
	if grove != nil && grove.GitRemote == "" {
		workspacePath, err := hubNativeGrovePath(grove.Slug)
		if err == nil {
			agent.AppliedConfig.Workspace = workspacePath
		}
	}

	// Populate template ID, hash, and hub access scopes if template was resolved.
	if resolvedTemplate != nil {
		agent.AppliedConfig.TemplateID = resolvedTemplate.ID
		agent.AppliedConfig.TemplateHash = resolvedTemplate.ContentHash
		if resolvedTemplate.Config != nil && resolvedTemplate.Config.HubAccess != nil {
			agent.AppliedConfig.HubAccessScopes = resolvedTemplate.Config.HubAccess.Scopes
		}

		// Merge template-level config values as defaults into AppliedConfig.
		// These act as pre-populated defaults for the advanced config form and
		// ensure the hub agent record reflects the effective configuration.
		// Explicit request values (already set) take precedence.
		if resolvedTemplate.Image != "" && agent.AppliedConfig.Image == "" {
			agent.AppliedConfig.Image = resolvedTemplate.Image
		}
		if resolvedTemplate.Config != nil {
			if resolvedTemplate.Config.Image != "" && agent.AppliedConfig.Image == "" {
				agent.AppliedConfig.Image = resolvedTemplate.Config.Image
			}
			if resolvedTemplate.Config.Model != "" && agent.AppliedConfig.Model == "" {
				agent.AppliedConfig.Model = resolvedTemplate.Config.Model
			}
			// Merge template env vars as defaults (don't overwrite explicit config env)
			if len(resolvedTemplate.Config.Env) > 0 {
				if agent.AppliedConfig.Env == nil {
					agent.AppliedConfig.Env = make(map[string]string)
				}
				for k, v := range resolvedTemplate.Config.Env {
					if _, exists := agent.AppliedConfig.Env[k]; !exists {
						agent.AppliedConfig.Env[k] = v
					}
				}
			}
			// Merge template telemetry config as default (don't overwrite explicit inline telemetry)
			if resolvedTemplate.Config.Telemetry != nil {
				if agent.AppliedConfig.InlineConfig == nil {
					agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
				}
				if agent.AppliedConfig.InlineConfig.Telemetry == nil {
					agent.AppliedConfig.InlineConfig.Telemetry = resolvedTemplate.Config.Telemetry
				}
			}
		}
	}
}

// existingAgentResult describes the outcome of handleExistingAgent.
type existingAgentResult int

const (
	// existingAgentNone means no existing agent was found (or it was nil).
	existingAgentNone existingAgentResult = iota
	// existingAgentDeleted means the stale agent was cleaned up; caller should fall through to create.
	existingAgentDeleted
	// existingAgentStarted means the existing agent was (re)started; response already written.
	existingAgentStarted
	// existingAgentErrored means an error occurred; response already written.
	existingAgentErrored
)

// createNotifySubscription creates a notification subscription for the given agent
// if notify is true and a subscriber has been identified.
func (s *Server) createNotifySubscription(ctx context.Context, agentID, groveID, notifySubscriberType, notifySubscriberID, createdBy string) {
	if notifySubscriberID == "" {
		return
	}
	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           agentID,
		SubscriberType:    notifySubscriberType,
		SubscriberID:      notifySubscriberID,
		GroveID:           groveID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED", "STALLED", "ERROR"},
		CreatedAt:         time.Now(),
		CreatedBy:         createdBy,
	}
	if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
		s.agentLifecycleLog.Warn("Failed to create notification subscription",
			"agentID", agentID, "subscriber", notifySubscriberID, "error", err)
	} else {
		s.agentLifecycleLog.Debug("Created notification subscription",
			"subscriptionID", sub.ID, "agentID", agentID,
			"subscriberType", notifySubscriberType, "subscriberID", notifySubscriberID)
	}
}

// handleExistingAgent encapsulates the full decision tree for an agent that
// already exists when a create/start request arrives.
//
// Phases:
//  1. Stale cleanup (running/stopped/error + not provision-only): dispatch delete, remove from DB → deleted
//  2. Env-gather re-provisioning (provisioning + GatherEnv): dispatch delete, remove from DB → deleted
//  3. Restart (created/provisioning/pending + not provision-only): recover broker ID, update config, dispatch start → started
//  4. Otherwise: none (caller decides what to do)
func (s *Server) handleExistingAgent(
	ctx context.Context,
	w http.ResponseWriter,
	existingAgent *store.Agent,
	grove *store.Grove,
	runtimeBrokerID string,
	req CreateAgentRequest,
	notifySubscriberType, notifySubscriberID, createdBy string,
) existingAgentResult {
	if existingAgent == nil {
		return existingAgentNone
	}
	cleanupMode := req.CleanupMode
	if cleanupMode == "" {
		cleanupMode = "strict"
	}

	// Phase 1: Stale cleanup — agent is running/stopped/error and caller wants a real start.
	if !req.ProvisionOnly &&
		(existingAgent.Phase == string(state.PhaseRunning) ||
			existingAgent.Phase == string(state.PhaseStopped) ||
			existingAgent.Phase == string(state.PhaseError)) {
		dispatcher := s.GetDispatcher()
		if dispatcher != nil && existingAgent.RuntimeBrokerID != "" {
			if err := dispatcher.DispatchAgentDelete(ctx, existingAgent, false, false, false, time.Time{}); err != nil {
				if cleanupMode != "force" {
					RuntimeError(w, "Failed to clean up existing agent before recreate: "+err.Error())
					return existingAgentErrored
				}
				s.agentLifecycleLog.Warn("Proceeding after stale-agent cleanup failure due to cleanupMode=force",
					"agentID", existingAgent.ID, "agentName", existingAgent.Name, "error", err)
			}
		}
		if err := s.store.DeleteAgent(ctx, existingAgent.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return existingAgentErrored
		}
		return existingAgentDeleted
	}

	// Phase 2: Env-gather re-provisioning — provisioning + GatherEnv requested.
	if req.GatherEnv && existingAgent.Phase == string(state.PhaseProvisioning) {
		dispatcher := s.GetDispatcher()
		if dispatcher != nil && existingAgent.RuntimeBrokerID != "" {
			if err := dispatcher.DispatchAgentDelete(ctx, existingAgent, false, false, false, time.Time{}); err != nil {
				if cleanupMode != "force" {
					RuntimeError(w, "Failed to clean up existing provisioning agent before env-gather recreate: "+err.Error())
					return existingAgentErrored
				}
				s.agentLifecycleLog.Warn("Proceeding after env-gather cleanup failure due to cleanupMode=force",
					"agentID", existingAgent.ID, "agentName", existingAgent.Name, "error", err)
			}
		}
		if err := s.store.DeleteAgent(ctx, existingAgent.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return existingAgentErrored
		}
		return existingAgentDeleted
	}

	// Phase 3: Restart — agent was provisioned/created but not yet started.
	if !req.ProvisionOnly &&
		(existingAgent.Phase == string(state.PhaseCreated) ||
			existingAgent.Phase == string(state.PhaseProvisioning)) {

		// Recover RuntimeBrokerID from the freshly-resolved value if the stored one is empty.
		if existingAgent.RuntimeBrokerID == "" && runtimeBrokerID != "" {
			existingAgent.RuntimeBrokerID = runtimeBrokerID
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil || existingAgent.RuntimeBrokerID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"cannot start agent: no runtime broker available", nil)
			return existingAgentErrored
		}

		// Update applied config with the task/attach if provided.
		if req.Task != "" && existingAgent.AppliedConfig != nil {
			existingAgent.AppliedConfig.Task = req.Task
			existingAgent.AppliedConfig.Attach = req.Attach
		}

		// Dispatch start action — DispatchAgentStart applies the broker's
		// response (status, container info) onto existingAgent in-place.
		if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task); err != nil {
			RuntimeError(w, "Failed to start agent: "+err.Error())
			return existingAgentErrored
		}

		// If the broker didn't set a running phase, default to running.
		if existingAgent.Phase == string(state.PhaseCreated) ||
			existingAgent.Phase == string(state.PhaseProvisioning) {
			existingAgent.Phase = string(state.PhaseRunning)
		}
		if err := s.store.UpdateAgent(ctx, existingAgent); err != nil {
			// Log but continue — agent was started.
			s.agentLifecycleLog.Warn("Failed to update agent status after start", "error", err)
		}

		// Create notification subscription if requested.
		if req.Notify {
			s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.GroveID, notifySubscriberType, notifySubscriberID, createdBy)
		}

		// Enrich and return the existing agent.
		s.enrichAgent(ctx, existingAgent, grove, nil)
		writeJSON(w, http.StatusOK, CreateAgentResponse{
			Agent: existingAgent,
		})
		return existingAgentStarted
	}

	return existingAgentNone
}

// resolveRuntimeBroker determines which runtime broker should run the agent.
// Priority order:
//  1. Explicitly specified broker (requestedBrokerID) - verified to be a provider
//  2. Grove's default runtime broker - verified to be available (online)
//  3. Single provider (any status) - used automatically
//  4. Multiple providers with online brokers - returns error requiring explicit selection
//  5. No providers - returns error
//
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

	slog.Debug("Resolving runtime broker",
		"grove", grove.ID, "groveName", grove.Name,
		"requestedBroker", requestedBrokerID,
		"totalProviders", len(allProviders),
		"onlineProviders", len(availableBrokers),
		"defaultBroker", grove.DefaultRuntimeBrokerID,
		"isHubNative", grove.GitRemote == "")

	// Convert to summary for error responses, marking and prioritizing the default broker
	brokerSummaries := make([]RuntimeBrokerSummary, 0, len(availableBrokers))
	var defaultBrokerSummary *RuntimeBrokerSummary
	for _, h := range availableBrokers {
		summary := RuntimeBrokerSummary{
			ID:        h.ID,
			Name:      h.Name,
			Status:    h.Status,
			IsDefault: h.ID == grove.DefaultRuntimeBrokerID,
		}
		if summary.IsDefault {
			defaultBrokerSummary = &summary
		} else {
			brokerSummaries = append(brokerSummaries, summary)
		}
	}
	// Prepend default broker if found (so it appears first in the list)
	if defaultBrokerSummary != nil {
		brokerSummaries = append([]RuntimeBrokerSummary{*defaultBrokerSummary}, brokerSummaries...)
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

		// Broker is not yet a provider — try to auto-link it.
		// The user explicitly selected this broker, so we honor that by linking it
		// to the grove as a provider. This is common for hub-native groves where
		// providers aren't established via CLI registration.
		broker, err := s.findBrokerByIDOrSlug(ctx, requestedBrokerID)
		if err == nil && broker != nil {
			provider := &store.GroveProvider{
				GroveID:    grove.ID,
				BrokerID:   broker.ID,
				BrokerName: broker.Name,
				Status:     broker.Status,
				LinkedBy:   "agent-create",
			}
			if addErr := s.store.AddGroveProvider(ctx, provider); addErr != nil {
				slog.Warn("Failed to auto-link broker during agent creation",
					"broker", broker.Name, "grove", grove.ID, "error", addErr)
				RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
				return "", store.ErrNotFound
			}
			slog.Info("Auto-linked broker as grove provider",
				"broker", broker.Name, "brokerID", broker.ID, "grove", grove.ID)

			// Set as default if grove has none
			if grove.DefaultRuntimeBrokerID == "" {
				grove.DefaultRuntimeBrokerID = broker.ID
				if updateErr := s.store.UpdateGrove(ctx, grove); updateErr != nil {
					slog.Warn("Failed to set default runtime broker",
						"broker", broker.Name, "grove", grove.ID, "error", updateErr)
				}
			}
			return broker.ID, nil
		}

		// Broker doesn't exist at all
		slog.Warn("Requested broker not found during agent creation",
			"requestedBrokerID", requestedBrokerID, "groveID", grove.ID,
			"providerCount", len(allProviders))
		RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
		return "", store.ErrNotFound
	}

	// Case 2: Use grove's default runtime broker (must be online and dispatchable)
	if grove.DefaultRuntimeBrokerID != "" {
		// Check if the default broker is still available
		for _, h := range availableBrokers {
			if h.ID == grove.DefaultRuntimeBrokerID {
				if s.canDispatchToBroker(ctx, &h) {
					return grove.DefaultRuntimeBrokerID, nil
				}
				// Default broker exists but user can't dispatch to it — fall through
				break
			}
		}
		// Default broker is not available or not dispatchable
		if len(availableBrokers) > 0 {
			NoRuntimeBroker(w, "Default runtime broker is unavailable; specify an alternative", brokerSummaries)
		} else {
			NoRuntimeBroker(w, "Default runtime broker is unavailable and no alternatives found", brokerSummaries)
		}
		return "", store.ErrNotFound
	}

	// Case 3: No default and no explicit broker - auto-select only when there is
	// exactly one provider and its broker is online and dispatchable.
	if len(allProviders) == 1 {
		broker, brokerErr := s.store.GetRuntimeBroker(ctx, allProviders[0].BrokerID)
		if brokerErr == nil && broker.Status == store.BrokerStatusOnline && s.canDispatchToBroker(ctx, broker) {
			return allProviders[0].BrokerID, nil
		}
		NoRuntimeBroker(w, "No runtime brokers available for this grove that you have permission to use", brokerSummaries)
		return "", store.ErrNotFound
	}

	// Case 4: Multiple providers - filter to dispatchable brokers, then require selection
	var dispatchable []store.RuntimeBroker
	for _, h := range availableBrokers {
		if s.canDispatchToBroker(ctx, &h) {
			dispatchable = append(dispatchable, h)
		}
	}

	switch len(dispatchable) {
	case 0:
		NoRuntimeBroker(w, "No runtime brokers available for this grove; register a runtime broker first", brokerSummaries)
		return "", store.ErrNotFound
	case 1:
		return dispatchable[0].ID, nil
	default:
		// Multiple dispatchable brokers - require explicit selection
		NoRuntimeBroker(w, "Multiple runtime brokers available for this grove; specify runtimeBrokerId to select one", brokerSummaries)
		return "", store.ErrNotFound
	}
}

// canDispatchToBroker checks whether the current user has dispatch permission on a broker
// without writing an HTTP response. Returns true if allowed (or if no user identity is present).
// Auto-provide brokers are dispatchable by any authenticated user since they are
// shared infrastructure (e.g. a combo hub-broker server's default broker).
func (s *Server) canDispatchToBroker(ctx context.Context, broker *store.RuntimeBroker) bool {
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		return true
	}
	if broker.AutoProvide {
		return true
	}
	decision := s.authzService.CheckAccess(ctx, userIdent, brokerResource(broker), ActionDispatch)
	return decision.Allowed
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

// findBrokerByIDOrSlug looks up a runtime broker by ID, slug, or name.
func (s *Server) findBrokerByIDOrSlug(ctx context.Context, identifier string) (*store.RuntimeBroker, error) {
	// Try by ID first
	broker, err := s.store.GetRuntimeBroker(ctx, identifier)
	if err == nil {
		return broker, nil
	}

	// Try by name (case-insensitive)
	broker, err = s.store.GetRuntimeBrokerByName(ctx, identifier)
	if err == nil {
		return broker, nil
	}

	return nil, store.ErrNotFound
}

// ============================================================================
// Public Settings Endpoint
// ============================================================================

// PublicSettingsResponse contains non-sensitive server settings for the web UI.
type PublicSettingsResponse struct {
	TelemetryEnabled bool `json:"telemetryEnabled"`
}

func (s *Server) handlePublicSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	enabled := false
	if s.config.TelemetryDefault != nil {
		enabled = *s.config.TelemetryDefault
	}

	writeJSON(w, http.StatusOK, PublicSettingsResponse{
		TelemetryEnabled: enabled,
	})
}
