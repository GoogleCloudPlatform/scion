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

package runtimebroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// matchesAgent checks whether an agent matches the given id and optional groveID.
// When groveID is provided, it must match for uniqueness across groves.
// When groveID is empty, matching falls back to name/containerID/slug only
// (backward compatible with solo/CLI mode and pre-existing containers).
func matchesAgent(a api.AgentInfo, id, groveID string) bool {
	nameMatch := a.Name == id || a.ContainerID == id || a.Slug == id
	if !nameMatch {
		return false
	}
	if groveID == "" {
		return true
	}
	// Check grove_id label first (authoritative), then GroveID field
	if labelGroveID := a.Labels["scion.grove_id"]; labelGroveID != "" {
		return labelGroveID == groveID
	}
	if a.GroveID != "" {
		return a.GroveID == groveID
	}
	// No grove_id on container — match anyway for backward compatibility
	// with containers created before grove_id labeling was added.
	return true
}

// ============================================================================
// Health Endpoints
// ============================================================================

// GetHealthInfo returns the current health status of the Runtime Broker server.
// This can be called directly by co-located components (e.g., the WebServer)
// to build composite health responses without making an HTTP round-trip.
func (s *Server) GetHealthInfo(ctx context.Context) *HealthResponse {
	checks := make(map[string]string)

	// Check runtime availability
	if s.runtime != nil {
		checks[s.runtime.Name()] = "available"
	} else {
		checks["runtime"] = "unavailable"
	}

	status := "healthy"
	for _, v := range checks {
		if v != "available" && v != "healthy" {
			status = "degraded"
			break
		}
	}

	return &HealthResponse{
		Status:  status,
		Version: s.version,
		Uptime:  time.Since(s.startTime).Round(time.Second).String(),
		Checks:  checks,
	}
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

	// Check if we have a functional runtime
	if s.runtime == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "no runtime available",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	runtimeType := "unknown"
	if s.runtime != nil {
		runtimeType = s.runtime.Name()
	}

	resp := BrokerInfoResponse{
		BrokerID: s.config.BrokerID,
		Name:     s.config.BrokerName,
		Version:  s.version,
		Capabilities: &BrokerCapabilities{
			WebPTY: false, // TODO: Implement WebSocket PTY
			Sync:   true,
			Attach: true,
			Exec:   true,
		},
		Profiles: []BrokerProfile{
			{Name: "default", Type: runtimeType, Available: true},
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleHubConnections returns live status of all hub connections.
func (s *Server) handleHubConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	s.hubMu.RLock()
	defer s.hubMu.RUnlock()

	mode := "single-hub"
	if len(s.hubConnections) > 1 {
		mode = "multi-hub"
	}

	connections := make([]HubConnectionInfo, 0, len(s.hubConnections))
	for _, conn := range s.hubConnections {
		info := HubConnectionInfo{
			Name:              conn.Name,
			HubEndpoint:       conn.HubEndpoint,
			BrokerID:          conn.BrokerID,
			AuthMode:          string(conn.AuthMode),
			Status:            string(conn.GetStatus()),
			IsColocated:       conn.IsColocated,
			HasHeartbeat:      conn.Heartbeat != nil,
			HasControlChannel: conn.ControlChannel != nil,
		}
		connections = append(connections, info)
	}

	resp := HubConnectionStatusResponse{
		Connections: connections,
		Mode:        mode,
	}

	writeJSON(w, http.StatusOK, resp)
}

// ============================================================================
// Agent Endpoints
// ============================================================================

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

	filter := map[string]string{
		"scion.agent": "true",
	}

	// Add optional filters
	if groveID := query.Get("groveId"); groveID != "" {
		filter["scion.grove"] = groveID
	}
	if status := query.Get("status"); status != "" {
		filter["status"] = status
	}

	agents, err := s.manager.List(ctx, filter)
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	// Also list agents from auxiliary runtimes (e.g. Kubernetes)
	s.auxiliaryRuntimesMu.RLock()
	auxRuntimes := make(map[string]auxiliaryRuntime, len(s.auxiliaryRuntimes))
	for k, v := range s.auxiliaryRuntimes {
		auxRuntimes[k] = v
	}
	s.auxiliaryRuntimesMu.RUnlock()

	// Dedup by name+groveID to prevent collision across groves while still
	// deduplicating the same agent found on multiple runtimes.
	agentKey := func(a api.AgentInfo) string {
		gid := a.GroveID
		if gid == "" {
			gid = a.Labels["scion.grove_id"]
		}
		return a.Name + "\x00" + gid
	}
	seen := make(map[string]bool)
	for _, ag := range agents {
		seen[agentKey(ag)] = true
	}
	for _, aux := range auxRuntimes {
		auxAgents, auxErr := aux.Manager.List(ctx, filter)
		if auxErr != nil {
			continue
		}
		for _, ag := range auxAgents {
			k := agentKey(ag)
			if !seen[k] {
				seen[k] = true
				agents = append(agents, ag)
			}
		}
	}

	// Convert to API response format
	responses := make([]AgentResponse, 0, len(agents))
	for _, agent := range agents {
		responses = append(responses, AgentInfoToResponse(agent))
	}

	// Apply pagination
	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	totalCount := len(responses)
	if len(responses) > limit {
		responses = responses[:limit]
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:     responses,
		TotalCount: totalCount,
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

	agentKey := req.ID
	if agentKey == "" {
		agentKey = req.Name
	}

	var attempt *dispatchAttempt
	if req.RequestID != "" {
		s.dispatchAttemptsMu.Lock()
		newAttempt, existingAttempt := s.beginCreateAttempt(req.RequestID, agentKey)
		if existingAttempt != nil {
			switch existingAttempt.Status {
			case dispatchAttemptSucceeded:
				if existingAttempt.CreatedResponse != nil {
					status := existingAttempt.HTTPStatus
					if status == 0 {
						status = http.StatusCreated
					}
					respCopy := *existingAttempt.CreatedResponse
					s.dispatchAttemptsMu.Unlock()
					writeJSON(w, status, respCopy)
					return
				}
				if existingAttempt.EnvResponse != nil {
					respCopy := *existingAttempt.EnvResponse
					s.dispatchAttemptsMu.Unlock()
					writeJSON(w, http.StatusAccepted, respCopy)
					return
				}
			case dispatchAttemptInProgress:
				s.dispatchAttemptsMu.Unlock()
				writeError(w, http.StatusConflict, ErrCodeConflict, "create request already in progress", map[string]interface{}{
					"requestId": req.RequestID,
				})
				return
			case dispatchAttemptFailed:
				existingAttempt.Status = dispatchAttemptInProgress
				existingAttempt.Error = ""
				existingAttempt.UpdatedAt = time.Now()
				s.completeAttempt(existingAttempt, dispatchAttemptInProgress, 0, nil, nil, "")
				attempt = existingAttempt
			}
		} else {
			attempt = newAttempt
		}
		s.dispatchAttemptsMu.Unlock()
	}

	markAttemptFailed := func(httpStatus int, message string) {
		if attempt == nil {
			return
		}
		s.dispatchAttemptsMu.Lock()
		s.completeAttempt(attempt, dispatchAttemptFailed, httpStatus, nil, nil, message)
		s.dispatchAttemptsMu.Unlock()
	}

	// Debug log incoming request
	if s.config.Debug {
		s.agentLifecycleLog.Debug("Creating agent", "agent_id", req.ID, "grove_id", req.GroveID, "name", req.Name, "slug", req.Slug)
		s.agentLifecycleLog.Debug("Hub credentials",
			"agent_id", req.ID,
			"grove_id", req.GroveID,
			"hubEndpoint", req.HubEndpoint,
			"hasToken", req.AgentToken != "",
			"slug", req.Slug,
		)
		if req.Config != nil {
			s.agentLifecycleLog.Debug("Agent configuration",
				"agent_id", req.ID,
				"grove_id", req.GroveID,
				"template", req.Config.Template,
				"image", req.Config.Image,
				"templateID", req.Config.TemplateID,
			)
		}
	}

	// Resolve grove path early for env-gather (needs settings access before buildStartContext)
	if req.GroveSlug != "" && req.GrovePath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to resolve global dir")
			RuntimeError(w, "Failed to get global dir: "+err.Error())
			return
		}
		req.GrovePath = filepath.Join(globalDir, "groves", req.GroveSlug)
	}

	// Env-gather: if GatherEnv is true, evaluate env completeness before building full context.
	// This needs the resolved grove path and merged env to determine which keys are missing.
	if req.GatherEnv {
		// Build a preliminary merged env for env-gather evaluation
		env := make(map[string]string)
		for k, v := range req.ResolvedEnv {
			env[k] = v
		}
		if req.Config != nil {
			for _, e := range req.Config.Env {
				parts := strings.SplitN(e, "=", 2)
				if len(parts) == 2 {
					env[parts[0]] = parts[1]
				}
			}
		}

		required, secretInfo := s.extractRequiredEnvKeys(req)
		if s.config.Debug {
			s.envSecretLog.Debug("Env-gather: evaluating env completeness",
				"gatherEnv", req.GatherEnv,
				"grovePath", req.GrovePath,
				"requiredKeys", len(required),
				"required", required,
			)
		}
		if len(required) > 0 {
			// Build lookup set of keys satisfied by resolved secrets
			secretTargets := make(map[string]struct{})
			for _, s := range req.ResolvedSecrets {
				if s.Type == "environment" || s.Type == "" {
					target := s.Target
					if target == "" {
						target = s.Name
					}
					if target != "" {
						secretTargets[target] = struct{}{}
					}
				}
				if s.Type == "file" {
					secretTargets[s.Name] = struct{}{}
				}
			}

			if s.config.Debug {
				targetKeys := make([]string, 0, len(secretTargets))
				for k := range secretTargets {
					targetKeys = append(targetKeys, k)
				}
				s.envSecretLog.Debug("Env-gather: resolved secret targets available",
					"secretTargetKeys", targetKeys,
					"resolvedSecretsCount", len(req.ResolvedSecrets),
				)
			}

			var hubHas, needs []string
			for _, key := range required {
				val, hasVal := env[key]
				if hasVal && val != "" {
					hubHas = append(hubHas, key)
				} else if _, fromSecret := secretTargets[key]; fromSecret {
					hubHas = append(hubHas, key)
				} else {
					needs = append(needs, key)
				}
			}

			if len(needs) > 0 {
				// Store pending state for finalize-env
				s.pendingEnvGatherMu.Lock()
				now := time.Now()
				s.cleanupExpiredPendingLocked(now)
				s.upsertPendingState(&pendingAgentState{
					AgentID:   agentKey,
					Request:   &req,
					MergedEnv: env,
					CreatedAt: now,
					UpdatedAt: now,
					State:     pendingStatePending,
					RequestID: req.RequestID,
				})
				s.pendingEnvGatherMu.Unlock()

				if s.config.Debug {
					s.envSecretLog.Debug("Env-gather: returning 202 with requirements",
						"required", required,
						"hubHas", hubHas,
						"needs", needs,
					)
				}

				// Build SecretInfo for needed keys only
				var respSecretInfo map[string]api.SecretKeyInfo
				for _, key := range needs {
					if info, ok := secretInfo[key]; ok {
						if respSecretInfo == nil {
							respSecretInfo = make(map[string]api.SecretKeyInfo)
						}
						respSecretInfo[key] = info
					}
				}

				resp := EnvRequirementsResponse{
					AgentID:    agentKey,
					Required:   required,
					HubHas:     hubHas,
					Needs:      needs,
					SecretInfo: respSecretInfo,
				}
				if attempt != nil {
					s.dispatchAttemptsMu.Lock()
					s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusAccepted, nil, &resp, "")
					s.dispatchAttemptsMu.Unlock()
				}
				writeJSON(w, http.StatusAccepted, resp)
				return
			}

			if s.config.Debug {
				s.envSecretLog.Debug("Env-gather: all required keys satisfied, proceeding with start",
					"required", required,
					"hubHas", hubHas,
				)
			}
		}
	}

	// Debug log grove path
	if s.config.Debug && req.GrovePath != "" {
		s.agentLifecycleLog.Debug("Using grove path from Hub", "agent_id", req.ID, "path", req.GrovePath)
	}

	// Reject global groves in multi-hub mode
	if s.isMultiHubMode() && s.isGlobalGrove(req.GroveID, req.GrovePath) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": map[string]string{
				"code":    "global_grove_disabled",
				"message": "Global grove is disabled when broker is connected to multiple hubs",
			},
		})
		return
	}

	// Build unified start context (grove path, env, template, git-clone, secrets, manager)
	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:            req.Name,
		AgentID:         req.ID,
		Slug:            req.Slug,
		GrovePath:       req.GrovePath,
		GroveSlug:       req.GroveSlug,
		GroveID:         req.GroveID,
		Config:          req.Config,
		InlineConfig:    req.InlineConfig,
		SharedDirs:      req.SharedDirs,
		HubEndpoint:     req.HubEndpoint,
		AgentToken:      req.AgentToken,
		CreatorName:     req.CreatorName,
		ResolvedEnv:     req.ResolvedEnv,
		ResolvedSecrets: req.ResolvedSecrets,
		Attach:          req.Attach,
		HTTPRequest:     r,
	})
	if err != nil {
		markAttemptFailed(http.StatusInternalServerError, err.Error())
		if sce, ok := err.(*startContextError); ok && sce.IsHubError {
			if templatecache.IsHubConnectivityError(sce.OriginalErr) {
				HubUnreachableError(w, sce.OriginalErr.Error())
				return
			}
			TemplateError(w, err.Error())
			return
		}
		RuntimeError(w, err.Error())
		return
	}
	opts := sc.Opts

	// If WorkspaceStoragePath is set, download workspace from GCS (non-git bootstrap)
	if req.WorkspaceStoragePath != "" {
		// For hub-native groves (GroveSlug set), use the conventional path
		// ~/.scion/groves/<slug>/ instead of the worktree-based path.
		var workspaceDir string
		if req.GroveSlug != "" {
			globalDir, err := config.GetGlobalDir()
			if err != nil {
				markAttemptFailed(http.StatusInternalServerError, "failed to resolve global dir")
				RuntimeError(w, "Failed to get global dir: "+err.Error())
				return
			}
			workspaceDir = filepath.Join(globalDir, "groves", req.GroveSlug)
		} else {
			workspaceDir = filepath.Join(s.config.WorktreeBase, req.Name, "workspace")
		}
		if err := os.MkdirAll(workspaceDir, 0755); err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to create workspace directory")
			RuntimeError(w, "Failed to create workspace directory: "+err.Error())
			return
		}

		bucket := s.config.StorageBucket
		if bucket == "" {
			markAttemptFailed(http.StatusInternalServerError, "storage bucket not configured")
			RuntimeError(w, "Storage bucket not configured for workspace bootstrap")
			return
		}

		if s.config.Debug {
			s.agentLifecycleLog.Debug("Downloading workspace from GCS", "agent_id", req.ID,
				"bucket", bucket,
				"storagePath", req.WorkspaceStoragePath+"/files",
				"workspaceDir", workspaceDir,
				"groveSlug", req.GroveSlug,
			)
		}

		if err := gcp.SyncFromGCS(ctx, bucket, req.WorkspaceStoragePath+"/files", workspaceDir); err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to download workspace from GCS")
			RuntimeError(w, "Failed to download workspace from GCS: "+err.Error())
			return
		}

		opts.Workspace = workspaceDir
		// Keep opts.GrovePath so that ProvisionAgent resolves the correct
		// agent directory. The explicit workspace takes precedence over the
		// worktree logic in ProvisionAgent, so no worktree will be created.

		// Write a .scion grove marker into the workspace so in-container CLI
		// can discover the grove context and use the Hub API.
		if req.GroveID != "" && req.GroveSlug != "" {
			if writeErr := config.WriteWorkspaceMarker(workspaceDir, req.GroveID, req.GroveSlug, req.GroveSlug); writeErr != nil {
				s.agentLifecycleLog.Warn("Failed to write workspace marker", "agent_id", req.ID, "grove_id", req.GroveID, "error", writeErr)
			}
		}
	}

	// Branch based on provision-only flag
	if req.ProvisionOnly {
		// Provision only: set up dirs, worktree, templates without starting the container
		cfg, err := sc.Manager.Provision(ctx, opts)
		if err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to provision agent")
			RuntimeError(w, "Failed to provision agent: "+err.Error())
			return
		}

		s.agentLifecycleLog.Info("Agent provisioned",
			"agent_id", req.ID, "grove_id", req.GroveID,
			"name", req.Name, "slug", req.Slug,
			"phase", string(state.PhaseCreated))

		// Build a response with "created" status (no container launched)
		agentResp := &AgentResponse{
			ID:     req.ID,
			Slug:   req.Slug,
			Name:   req.Name,
			Status: string(state.PhaseCreated),
			Phase:  string(state.PhaseCreated),
		}
		if cfg != nil {
			agentResp.HarnessConfig = cfg.HarnessConfig
			agentResp.Image = cfg.Image
		}
		if s.runtime != nil {
			agentResp.RuntimeType = s.runtime.Name()
		}

		resp := CreateAgentResponse{
			Agent:   agentResp,
			Created: true,
		}
		if attempt != nil {
			s.dispatchAttemptsMu.Lock()
			s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusCreated, &resp, nil, "")
			s.dispatchAttemptsMu.Unlock()
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	// Full start: provision and launch the container
	agentInfo, err := sc.Manager.Start(ctx, opts)
	if err != nil {
		markAttemptFailed(http.StatusInternalServerError, "failed to create agent")

		s.agentLifecycleLog.Error("Agent create failed",
			"agent_id", req.ID, "grove_id", req.GroveID,
			"name", req.Name, "slug", req.Slug,
			"error", err)

		// Clean up provisioned agent files so they don't become orphans.
		if opts.GrovePath != "" {
			if _, cleanupErr := agent.DeleteAgentFiles(opts.Name, opts.GrovePath, true); cleanupErr != nil {
				s.agentLifecycleLog.Warn("Failed to clean up agent files after start failure",
					"agent_id", req.ID, "grove_id", req.GroveID, "agent", opts.Name, "error", cleanupErr)
			} else {
				s.agentLifecycleLog.Info("Cleaned up provisioned agent files after start failure",
					"agent_id", req.ID, "grove_id", req.GroveID, "agent", opts.Name)
			}
		}
		RuntimeError(w, "Failed to create agent: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Agent created",
		"agent_id", req.ID, "grove_id", req.GroveID,
		"name", req.Name, "slug", req.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	// Log auth resolution info visible in broker logs
	for _, w := range agentInfo.Warnings {
		if strings.HasPrefix(w, "Auth:") {
			s.agentLifecycleLog.Info("Agent auth resolution", "agent_id", req.ID, "grove_id", req.GroveID, "agent", req.Name, "result", w)
		}
	}

	resp := CreateAgentResponse{
		Agent:   agentInfoPtr(AgentInfoToResponse(*agentInfo)),
		Created: true,
	}
	if attempt != nil {
		s.dispatchAttemptsMu.Lock()
		s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusCreated, &resp, nil, "")
		s.dispatchAttemptsMu.Unlock()
	}

	writeJSON(w, http.StatusCreated, resp)
}

// hydrateTemplate fetches and caches a template from the Hub if template info is provided.
// Returns the local template path, or empty string if no Hub template was specified.
// For co-located connections with a TemplatesDir, it resolves the template directly
// from the local filesystem, bypassing the Hub API round-trip entirely.
func (s *Server) hydrateTemplate(ctx context.Context, cfg *CreateAgentConfig, conn *HubConnection) (string, error) {
	// Check if we have template info from Hub
	if cfg.TemplateID == "" && cfg.TemplateHash == "" {
		// No Hub template info provided, use local template handling
		return "", nil
	}

	// Co-located shortcut: resolve directly from the local templates directory.
	// This means edits to ~/.scion/templates/<name> are picked up immediately
	// without needing to re-sync through Hub storage and cache.
	if conn.IsColocated && conn.TemplatesDir != "" && cfg.Template != "" {
		localPath := filepath.Join(conn.TemplatesDir, cfg.Template)
		if info, err := os.Stat(localPath); err == nil && info.IsDir() {
			return localPath, nil
		}
		// Fall through to hydration if local path doesn't exist
	}

	hydrator := conn.Hydrator
	if hydrator == nil {
		return "", nil
	}

	// If we have a template hash, try to use it for cache lookup
	if cfg.TemplateHash != "" && cfg.TemplateID != "" {
		return hydrator.HydrateWithHash(ctx, cfg.TemplateID, cfg.TemplateHash)
	}

	// Just have template ID, do full hydration
	if cfg.TemplateID != "" {
		return hydrator.Hydrate(ctx, cfg.TemplateID)
	}

	return "", nil
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}

	// Extract groveId from query params for grove-scoped agent resolution.
	// This prevents cross-grove agent collision when two agents with the same
	// name exist in different groves on the same broker.
	groveID := r.URL.Query().Get("groveId")

	// Handle WebSocket attach for PTY
	if action == "attach" && isPTYWebSocketUpgrade(r) {
		s.handleAgentAttach(w, r)
		return
	}

	// Handle actions
	if action != "" {
		s.handleAgentAction(w, r, id, groveID, action)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAgent(w, r, id, groveID)
	case http.MethodDelete:
		s.deleteAgent(w, r, id, groveID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	// Resolve the correct manager (checks auxiliary runtimes if needed)
	mgr := s.resolveManagerForAgent(ctx, id, groveID)

	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	for _, agent := range agents {
		if matchesAgent(agent, id, groveID) {
			writeJSON(w, http.StatusOK, AgentInfoToResponse(agent))
			return
		}
	}

	NotFound(w, "Agent")
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"
	softDelete := query.Get("softDelete") == "true"

	// Resolve the correct manager for this agent (may be on an auxiliary runtime)
	mgr := s.resolveManagerForAgent(ctx, id, groveID)

	// Get the agent's grove path and grove ID before stopping (needed for file deletion and logging)
	var grovePath, agentGroveID string
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err == nil {
		for _, a := range agents {
			if matchesAgent(a, id, groveID) {
				grovePath = a.GrovePath
				agentGroveID = a.GroveID
				if agentGroveID == "" {
					agentGroveID = a.Grove
				}
				break
			}
		}
	}

	// If no grove path was found (container missing or no annotation), check
	// hub-native grove directories for the agent's files. Without this,
	// agents in hub-native groves (~/.scion/groves/<slug>/) are silently
	// skipped during file cleanup because the default filesystem scan only
	// checks the CWD-resolved project dir and global ~/.scion.
	if grovePath == "" && deleteFiles {
		if resolved := findAgentInHubNativeGroves(id); resolved != "" {
			grovePath = resolved
			s.agentLifecycleLog.Debug("Resolved agent grove path from hub-native groves",
				"agent_id", id, "path", grovePath)
		}
	}

	// If this is a soft-delete, mark agent-info.json with deleted status before cleanup
	if softDelete && grovePath != "" {
		deletedAtStr := query.Get("deletedAt")
		if err := agent.UpdateAgentConfig(id, grovePath, "deleted", "", ""); err != nil {
			s.agentLifecycleLog.Warn("Failed to mark agent as deleted in agent-info.json", "agent_id", id, "error", err)
		}
		if deletedAtStr != "" {
			if deletedAt, err := time.Parse(time.RFC3339, deletedAtStr); err == nil {
				if err := agent.UpdateAgentDeletedAt(id, grovePath, deletedAt); err != nil {
					s.agentLifecycleLog.Warn("Failed to write deletedAt to agent-info.json", "agent_id", id, "error", err)
				}
			}
		}
	}

	_, err = mgr.Delete(ctx, id, deleteFiles, grovePath, removeBranch)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to delete agent: "+err.Error())
		return
	}

	if softDelete {
		s.agentLifecycleLog.Info("Agent soft-deleted",
			"agent_id", id, "grove_id", agentGroveID,
			"delete_files", deleteFiles, "remove_branch", removeBranch)
	} else {
		s.agentLifecycleLog.Info("Agent deleted",
			"agent_id", id, "grove_id", agentGroveID,
			"delete_files", deleteFiles, "remove_branch", removeBranch)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, groveID, action string) {
	method, ok := api.RuntimeBrokerAgentActionMethod(action)
	if !ok {
		NotFound(w, "Action")
		return
	}
	if r.Method != method {
		MethodNotAllowed(w)
		return
	}

	switch action {
	case api.AgentActionStart:
		s.startAgent(w, r, id, groveID)
	case api.AgentActionStop:
		s.stopAgent(w, r, id, groveID)
	case api.AgentActionRestart:
		s.restartAgent(w, r, id, groveID)
	case api.AgentActionMessage:
		s.sendMessage(w, r, id, groveID)
	case api.AgentActionExec:
		s.execCommand(w, r, id)
	case api.AgentActionLogs:
		s.getLogs(w, r, id, groveID)
	case api.AgentActionStats:
		s.getStats(w, r, id, groveID)
	case api.AgentActionHasPrompt:
		s.checkAgentPrompt(w, r, id, groveID)
	case api.AgentActionFinalizeEnv:
		s.finalizeEnv(w, r, id)
	default:
		NotFound(w, "Action")
	}
}

func (s *Server) startAgent(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	// Read optional task, grovePath, groveSlug, harnessConfig, and resolvedEnv from request body
	var startReq struct {
		Task            string               `json:"task"`
		GrovePath       string               `json:"grovePath"`
		GroveSlug       string               `json:"groveSlug"`
		HarnessConfig   string               `json:"harnessConfig"`
		ResolvedEnv     map[string]string    `json:"resolvedEnv"`
		ResolvedSecrets []api.ResolvedSecret `json:"resolvedSecrets,omitempty"`
		InlineConfig    *api.ScionConfig     `json:"inlineConfig,omitempty"`
		SharedDirs      []api.SharedDir      `json:"sharedDirs,omitempty"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&startReq); err != nil {
			s.agentLifecycleLog.Debug("No task in start request body (ignoring decode error)", "agent_id", id, "error", err)
		}
	}

	s.agentLifecycleLog.Debug("startAgent called", "agent_id", id, "task", startReq.Task, "grovePath", startReq.GrovePath, "groveSlug", startReq.GroveSlug, "harnessConfig", startReq.HarnessConfig, "resolvedEnvCount", len(startReq.ResolvedEnv))

	// Build config for buildStartContext (startAgent uses a subset of CreateAgentConfig)
	var cfg *CreateAgentConfig
	if startReq.Task != "" || startReq.HarnessConfig != "" || len(startReq.SharedDirs) > 0 {
		cfg = &CreateAgentConfig{
			Task:          startReq.Task,
			HarnessConfig: startReq.HarnessConfig,
			SharedDirs:    startReq.SharedDirs,
		}
	}

	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:            id,
		GrovePath:       startReq.GrovePath,
		GroveSlug:       startReq.GroveSlug,
		Config:          cfg,
		ResolvedEnv:     startReq.ResolvedEnv,
		ResolvedSecrets: startReq.ResolvedSecrets,
		SharedDirs:      startReq.SharedDirs,
		HTTPRequest:     r,
	})
	if err != nil {
		RuntimeError(w, err.Error())
		return
	}
	opts := sc.Opts

	// If grove path wasn't in the request, fall back to looking up from an existing container
	if startReq.GrovePath == "" && startReq.GroveSlug == "" && opts.GrovePath == "" {
		agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
		if err != nil {
			RuntimeError(w, "Failed to list agents: "+err.Error())
			return
		}
		for i := range agents {
			if matchesAgent(agents[i], id, groveID) {
				if agents[i].GrovePath != "" {
					opts.GrovePath = agents[i].GrovePath
				}
				break
			}
		}
	}

	// Apply updated InlineConfig to scion-agent.json before starting.
	if startReq.InlineConfig != nil && opts.GrovePath != "" {
		s.applyInlineConfigUpdate(id, opts.GrovePath, startReq.InlineConfig)
	}

	// Resolve saved profile for runtime selection
	if opts.GrovePath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.GrovePath)
	}

	// Re-resolve manager after profile update
	mgr := s.resolveManagerForOpts(opts)
	agentInfo, err := mgr.Start(ctx, opts)
	if err != nil {
		s.agentLifecycleLog.Error("Agent start failed",
			"agent_id", id, "error", err)
		RuntimeError(w, "Failed to start agent: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Agent started",
		"agent_id", id, "grove_id", agentInfo.GroveID,
		"name", agentInfo.Name, "slug", agentInfo.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	// Send an immediate heartbeat so the hub gets the updated container status
	s.forceHeartbeatAll("start", id)

	agentResp := AgentInfoToResponse(*agentInfo)
	writeJSON(w, http.StatusAccepted, CreateAgentResponse{
		Agent:   &agentResp,
		Created: false,
	})
}

// applyInlineConfigUpdate merges the updated InlineConfig into the agent's
// scion-agent.json. This ensures config changes made via the Hub (e.g. limits
// set in the web configure form) are applied before the agent starts.
func (s *Server) applyInlineConfigUpdate(agentName, grovePath string, inlineConfig *api.ScionConfig) {
	projectDir, err := config.GetResolvedProjectDir(grovePath)
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to resolve project dir", "agent", agentName, "error", err)
		return
	}
	agentDir := filepath.Join(projectDir, "agents", agentName)
	cfgPath := filepath.Join(agentDir, "scion-agent.json")

	// Load existing config
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to read scion-agent.json", "agent", agentName, "path", cfgPath, "error", err)
		return
	}
	var existing api.ScionConfig
	if err := json.Unmarshal(data, &existing); err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to parse scion-agent.json", "agent", agentName, "error", err)
		return
	}

	// Merge inline config over existing
	merged := config.MergeScionConfig(&existing, inlineConfig)

	// Write back
	updated, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to marshal updated config", "agent", agentName, "error", err)
		return
	}
	if err := os.WriteFile(cfgPath, updated, 0644); err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to write scion-agent.json", "agent", agentName, "error", err)
		return
	}
	if s.config.Debug {
		s.agentLifecycleLog.Debug("applyInlineConfigUpdate: applied inline config update",
			"agent", agentName, "maxTurns", inlineConfig.MaxTurns, "maxModelCalls", inlineConfig.MaxModelCalls)
	}
}

// isContainerStopTolerable returns true if the error from stopping a container
// indicates the container is already stopped, exited, or doesn't exist. This
// covers both Docker and Podman error messages and exit codes.
func isContainerStopTolerable(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "No such") ||
		strings.Contains(msg, "not running") ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "exit status 125")
}

func (s *Server) stopAgent(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	mgr := s.resolveManagerForAgent(ctx, id, groveID)
	if err := mgr.Stop(ctx, id); err != nil {
		if isContainerStopTolerable(err) {
			// Container doesn't exist, is already stopped, or podman/docker can't find it.
			// Treat as success so the hub can update its state.
			s.agentLifecycleLog.Info("Agent stopped (already stopped/not found)",
				"agent_id", id,
				"phase", string(state.PhaseStopped))
		} else {
			RuntimeError(w, "Failed to stop agent: "+err.Error())
			return
		}
	} else {
		s.agentLifecycleLog.Info("Agent stopped",
			"agent_id", id,
			"phase", string(state.PhaseStopped))
	}

	s.forceHeartbeatAll("stop", id)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "Stop operation accepted",
	})
}

func (s *Server) restartAgent(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	// Read optional resolvedEnv from request body (hub sends fresh auth token)
	var restartReq struct {
		ResolvedEnv map[string]string `json:"resolvedEnv"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&restartReq); err != nil {
			s.agentLifecycleLog.Debug("No resolvedEnv in restart request body (ignoring decode error)", "agent_id", id, "error", err)
		}
	}

	// Look up agent to get its name and grove path
	agentName := id
	var grovePath string
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err == nil {
		for i := range agents {
			if matchesAgent(agents[i], id, groveID) {
				agentName = agents[i].Name
				grovePath = agents[i].GrovePath
				break
			}
		}
	}

	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:        agentName,
		GrovePath:   grovePath,
		ResolvedEnv: restartReq.ResolvedEnv,
		HTTPRequest: r,
	})
	if err != nil {
		RuntimeError(w, err.Error())
		return
	}
	opts := sc.Opts

	if opts.GrovePath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.GrovePath)
	}

	// Stop then start — tolerate stop errors since the container may already
	// be exited and the subsequent start will handle cleanup.
	// Use resolveManagerForAgent to find the agent on auxiliary runtimes.
	stopMgr := s.resolveManagerForAgent(ctx, id, groveID)
	if err := stopMgr.Stop(ctx, id); err != nil {
		if isContainerStopTolerable(err) {
			s.agentLifecycleLog.Warn("Restart: stop target not found or already stopped, proceeding with start", "agent_id", id, "error", err)
		} else {
			s.agentLifecycleLog.Warn("Restart: stop failed, proceeding with start anyway", "agent_id", id, "error", err)
		}
	}

	// Re-resolve manager after profile update
	mgr := s.resolveManagerForOpts(opts)
	agentInfo, err := mgr.Start(ctx, opts)
	if err != nil {
		s.agentLifecycleLog.Error("Agent restart failed",
			"agent_id", id, "error", err)
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to restart agent: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Agent restarted",
		"agent_id", id, "grove_id", agentInfo.GroveID,
		"name", agentInfo.Name, "slug", agentInfo.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	s.forceHeartbeatAll("restart", id)

	agentResp := AgentInfoToResponse(*agentInfo)
	writeJSON(w, http.StatusAccepted, CreateAgentResponse{
		Agent:   &agentResp,
		Created: false,
	})
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Determine the message to deliver.
	// Empty messages (no body) are sent as an empty string, which the agent
	// manager delivers as a plain tmux Enter keypress to trigger confirmations.
	var deliveryText string
	if req.StructuredMessage != nil {
		deliveryText = messages.FormatForDelivery(req.StructuredMessage)
	} else {
		deliveryText = req.Message
	}

	// Resolve the correct manager for this agent (may be on an auxiliary runtime like K8s)
	mgr := s.resolveManagerForAgent(ctx, id, groveID)

	// Raw messages bypass the paste buffer and debounce, sending literal
	// bytes via tmux send-keys with no trailing Enter keypresses.
	isRaw := req.StructuredMessage != nil && req.StructuredMessage.Raw
	if isRaw {
		if err := mgr.MessageRaw(ctx, id, deliveryText); err != nil {
			if strings.Contains(err.Error(), "not found") {
				NotFound(w, "Agent")
				return
			}
			RuntimeError(w, "Failed to send raw message: "+err.Error())
			return
		}
	} else {
		if err := mgr.Message(ctx, id, deliveryText, req.Interrupt); err != nil {
			if strings.Contains(err.Error(), "not found") {
				NotFound(w, "Agent")
				return
			}
			RuntimeError(w, "Failed to send message: "+err.Error())
			return
		}
	}

	// Log message acceptance. Non-interrupt messages are buffered with a
	// debounce delay before actual tmux delivery, so we log "accepted"
	// rather than "delivered". Interrupt messages bypass the buffer and
	// are delivered immediately. Raw messages are always delivered immediately.
	logMsg := "message accepted (buffered)"
	if isRaw {
		logMsg = "message delivered (raw, unbuffered)"
	} else if req.Interrupt {
		logMsg = "message delivered (interrupt, unbuffered)"
	}
	logAttrs := []any{"agent_id", id}
	if req.GroveID != "" {
		logAttrs = append(logAttrs, "grove_id", req.GroveID)
	}
	if req.StructuredMessage != nil {
		logAttrs = append(logAttrs, req.StructuredMessage.LogAttrs()...)
	}
	if s.dedicatedMessageLog != nil {
		s.dedicatedMessageLog.Info(logMsg, logAttrs...)
	} else {
		s.messageLog.Info(logMsg, logAttrs...)
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) execCommand(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req ExecRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Command) == 0 {
		ValidationError(w, "command is required", nil)
		return
	}

	// Resolve the correct runtime for this agent (may be on an auxiliary runtime like K8s).
	// Exec doesn't receive groveID from query params (internal operation).
	rt := s.resolveRuntimeForAgent(ctx, id, "")

	output, err := rt.Exec(ctx, id, req.Command)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to execute command: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ExecResponse{
		Output:   output,
		ExitCode: 0, // TODO: Get actual exit code from runtime
	})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	// Resolve the correct manager for this agent (may be on an auxiliary runtime like K8s)
	mgr := s.resolveManagerForAgent(ctx, id, groveID)

	// Try to read agent.log from the filesystem first (preferred source).
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var found *api.AgentInfo
	for i := range agents {
		if matchesAgent(agents[i], id, groveID) {
			found = &agents[i]
			break
		}
	}

	if found == nil {
		NotFound(w, "Agent")
		return
	}

	if found.GrovePath != "" {
		agentLogPath := filepath.Join(config.GetAgentHomePath(
			filepath.Join(found.GrovePath, ".scion"), found.Slug,
		), "agent.log")
		if data, err := os.ReadFile(agentLogPath); err == nil {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
		// Fall through to container logs if agent.log not found
	}

	// Fallback: read container stdout logs (resolve runtime for auxiliary runtimes)
	rt := s.resolveRuntimeForAgent(ctx, id, groveID)
	logs, err := rt.GetLogs(ctx, id)
	if err != nil {
		RuntimeError(w, "Failed to get logs: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logs))
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request, id, groveID string) {
	// TODO: Implement real stats from runtime
	// For now, return placeholder data
	writeJSON(w, http.StatusOK, StatsResponse{
		CPUUsagePercent:  0.0,
		MemoryUsageBytes: 0,
	})
}

// HasPromptResponse is the response for the has-prompt action.
type HasPromptResponse struct {
	HasPrompt bool `json:"hasPrompt"`
}

func (s *Server) checkAgentPrompt(w http.ResponseWriter, r *http.Request, id, groveID string) {
	ctx := r.Context()

	// Find the agent to get its grove path
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var agent *api.AgentInfo
	for i := range agents {
		if matchesAgent(agents[i], id, groveID) {
			agent = &agents[i]
			break
		}
	}

	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	if agent.GrovePath == "" {
		// No grove path means we can't check prompt.md
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	// Check if prompt.md exists and has content
	// Path: <grovePath>/agents/<agentName>/prompt.md
	promptPath := filepath.Join(agent.GrovePath, "agents", agent.Name, "prompt.md")
	content, err := os.ReadFile(promptPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
			return
		}
		// Log the error but return false
		s.agentLifecycleLog.Warn("Failed to read prompt.md", "agent_id", id, "path", promptPath, "error", err)
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	hasPrompt := len(strings.TrimSpace(string(content))) > 0
	writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: hasPrompt})
}

// extractRequiredEnvKeys determines the set of env keys required by the agent's
// harness, auth type, and settings profile. It uses a multi-phase approach:
//
// Phase 1 (auth-aware): Resolves the harness type and auth_selected_type from
// on-disk harness-config and settings, then calls RequiredAuthEnvKeys() to get
// intrinsic credential requirements for the (harness, authType) pair.
//
// Phase 2 (settings-based): Extracts keys with empty values from settings
// harness_configs[*].env and profiles[*].env, allowing users to declare custom
// env requirements.
//
// Phase 3 (secrets): Collects explicitly-declared secrets from settings and templates.
func (s *Server) extractRequiredEnvKeys(req CreateAgentRequest) ([]string, map[string]api.SecretKeyInfo) {
	required := make(map[string]struct{})

	var settings *config.VersionedSettings
	settingsPath := req.GrovePath
	if settingsPath == "" {
		// Fall back to the broker's global .scion directory for settings
		// resolution. This matches what agent.Start → GetResolvedProjectDir("")
		// does when grovePath is empty (e.g., hub-only git groves without a
		// linked local path on the broker).
		if globalDir, err := config.GetGlobalDir(); err == nil {
			settingsPath = globalDir
			if s.config.Debug {
				s.envSecretLog.Debug("extractRequiredEnvKeys: grovePath empty, using global dir",
					"globalDir", globalDir,
				)
			}
		}
	}
	if settingsPath != "" {
		vs, _, err := config.LoadEffectiveSettings(settingsPath)
		if err == nil {
			settings = vs
			if s.config.Debug {
				s.envSecretLog.Debug("extractRequiredEnvKeys: loaded settings",
					"path", settingsPath,
					"defaultHarnessConfig", vs.DefaultHarnessConfig,
					"harnessConfigCount", len(vs.HarnessConfigs),
				)
			}
		} else if s.config.Debug {
			s.envSecretLog.Debug("extractRequiredEnvKeys: failed to load settings",
				"path", settingsPath,
				"error", err.Error(),
			)
		}
	}

	profileName := ""
	if req.Config != nil {
		profileName = req.Config.Profile
	}
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}

	// Phase 1: Auth-aware env key extraction
	// Resolve harness type and auth_selected_type, then derive required keys.
	secretInfo := make(map[string]api.SecretKeyInfo)
	harnessConfigName := s.resolveHarnessConfigForEnvGather(req, settings)
	if s.config.Debug {
		s.envSecretLog.Debug("extractRequiredEnvKeys: harness resolution",
			"harnessConfigName", harnessConfigName,
			"hasSettings", settings != nil,
			"grovePath", req.GrovePath,
		)
	}
	if harnessConfigName != "" {
		var harnessType, authType string

		// Try on-disk harness-config directory first (check grovePath,
		// then fall back to global dir for hub-dispatched agents without a local grove)
		harnessConfigSearchPath := req.GrovePath
		if harnessConfigSearchPath == "" {
			harnessConfigSearchPath = settingsPath
		}
		if harnessConfigSearchPath != "" {
			if hcDir, err := config.FindHarnessConfigDir(harnessConfigName, harnessConfigSearchPath); err == nil {
				harnessType = hcDir.Config.Harness
				authType = hcDir.Config.AuthSelectedType
			}
		}

		// Settings harness_configs entry can provide/override
		if settings != nil {
			if hcfg, ok := settings.HarnessConfigs[harnessConfigName]; ok {
				if harnessType == "" {
					harnessType = hcfg.Harness
				}
				if authType == "" {
					authType = hcfg.AuthSelectedType
				}
			}
		}

		// Profile harness_overrides can override auth type
		if profileName != "" && settings != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				if override, ok := profile.HarnessOverrides[harnessConfigName]; ok {
					if override.AuthSelectedType != "" {
						authType = override.AuthSelectedType
					}
				}
			}
		}

		// Template-level auth_selectedType takes high precedence
		if req.Config != nil && req.Config.Template != "" && req.GrovePath != "" {
			if tmpl, err := config.FindTemplateInGrovePath(req.Config.Template, req.GrovePath); err == nil {
				if cfg, err := tmpl.LoadConfig(); err == nil && cfg != nil && cfg.AuthSelectedType != "" {
					authType = cfg.AuthSelectedType
				}
			}
		}

		// --harness-auth CLI flag takes ultimate precedence
		if req.Config != nil && req.Config.HarnessAuth != "" {
			authType = req.Config.HarnessAuth
		}

		// Determine if a GCP service account is assigned via identity config.
		gcpSAAssigned := req.Config != nil && req.Config.GCPIdentity != nil &&
			req.Config.GCPIdentity.MetadataMode == "assign"

		// When auth type is unset (auto-detect), check if resolved file secrets
		// or a GCP service account can satisfy an alternative auth method before
		// defaulting to api-key. This mirrors the auto-detect priority in each
		// harness's ResolveAuth.
		if authType == "" {
			fileSecretNames := make(map[string]struct{})
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "file" {
					fileSecretNames[sec.Name] = struct{}{}
				}
			}
			if detected := harness.DetectAuthTypeFromFileSecrets(harnessType, fileSecretNames); detected != "" {
				authType = detected
			}
		}
		if authType == "" {
			resolvedEnvKeys := make(map[string]struct{})
			for k, v := range req.ResolvedEnv {
				if v != "" {
					resolvedEnvKeys[k] = struct{}{}
				}
			}
			if detected := harness.DetectAuthTypeFromEnvVars(harnessType, resolvedEnvKeys); detected != "" {
				authType = detected
			}
		}
		if authType == "" {
			if detected := harness.DetectAuthTypeFromGCPIdentity(harnessType, gcpSAAssigned); detected != "" {
				authType = detected
			}
		}

		// Resolve auth key groups and check satisfaction
		if keyGroups := harness.RequiredAuthEnvKeys(harnessType, authType); len(keyGroups) > 0 {
			// Build lookup of already-satisfied keys
			envKeys := make(map[string]struct{})
			for k, v := range req.ResolvedEnv {
				if v != "" {
					envKeys[k] = struct{}{}
				}
			}
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "environment" || sec.Type == "" {
					target := sec.Target
					if target == "" {
						target = sec.Name
					}
					if target != "" {
						envKeys[target] = struct{}{}
					}
				}
			}

			for _, group := range keyGroups {
				satisfied := false
				for _, key := range group {
					if _, ok := envKeys[key]; ok {
						satisfied = true
						break
					}
				}
				if !satisfied {
					// Add the canonical (first) key as required
					canonicalKey := group[0]
					required[canonicalKey] = struct{}{}
					secretInfo[canonicalKey] = api.SecretKeyInfo{Source: "auth"}
				}
			}
		}

		// Phase 1b: Auth-required file secrets (e.g. ADC for vertex-ai).
		// When a GCP service account is assigned, the metadata server provides
		// credentials, so the ADC file is not required.
		if authSecrets := harness.RequiredAuthSecrets(harnessType, authType, gcpSAAssigned); len(authSecrets) > 0 {
			// Build lookup of file-type resolved secrets by Name and Target suffix
			fileSecrets := make(map[string]struct{})
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "file" {
					fileSecrets[sec.Name] = struct{}{}
					if sec.Target != "" {
						fileSecrets[sec.Target] = struct{}{}
					}
				}
			}

			for _, as := range authSecrets {
				if _, ok := fileSecrets[as.Key]; !ok {
					// Check if any alternative env keys satisfy this requirement.
					altSatisfied := false
					for _, altKey := range as.AlternativeEnvKeys {
						if v, ok := req.ResolvedEnv[altKey]; ok && v != "" {
							altSatisfied = true
							break
						}
					}
					if !altSatisfied {
						required[as.Key] = struct{}{}
						secretInfo[as.Key] = api.SecretKeyInfo{
							Description: as.Description,
							Source:      "auth",
							Type:        "file",
						}
					}
				}
			}
		}
	}

	// Phase 2: Settings-based empty-value env key extraction
	if settings != nil {
		// Get profile env keys
		if profileName != "" && settings.Profiles != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				for k, v := range profile.Env {
					if v == "" {
						required[k] = struct{}{}
					}
				}
				// Check harness overrides within the profile
				for _, override := range profile.HarnessOverrides {
					for k, v := range override.Env {
						if v == "" {
							required[k] = struct{}{}
						}
					}
				}
			}
		}

		// Get harness config env keys
		for _, hcfg := range settings.HarnessConfigs {
			for k, v := range hcfg.Env {
				if v == "" {
					required[k] = struct{}{}
				}
			}
		}
	}

	// Phase 3: Secrets declarations from settings and template

	// 3a: Settings-derived empty-value env keys are secret-eligible
	for k := range required {
		if _, exists := secretInfo[k]; !exists {
			secretInfo[k] = api.SecretKeyInfo{Source: "settings"}
		}
	}

	// 3b: Settings harness_configs[*].secrets
	if settings != nil {
		for _, hcfg := range settings.HarnessConfigs {
			for _, sec := range hcfg.Secrets {
				required[sec.Key] = struct{}{}
				secretInfo[sec.Key] = api.SecretKeyInfo{
					Description: sec.Description,
					Source:      "settings",
					Type:        sec.Type,
				}
			}
		}

		// 3c: Profile secrets
		if profileName != "" && settings.Profiles != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				for _, sec := range profile.Secrets {
					required[sec.Key] = struct{}{}
					secretInfo[sec.Key] = api.SecretKeyInfo{
						Description: sec.Description,
						Source:      "settings",
						Type:        sec.Type,
					}
				}
			}
		}
	}

	// 3d: Template secrets (from request or local template)
	for _, sec := range req.RequiredSecrets {
		required[sec.Key] = struct{}{}
		secretInfo[sec.Key] = api.SecretKeyInfo{
			Description: sec.Description,
			Source:      "template",
			Type:        sec.Type,
		}
	}
	// Also try loading local template config
	if req.Config != nil && req.Config.Template != "" && req.GrovePath != "" {
		if tmpl, err := config.FindTemplateInGrovePath(req.Config.Template, req.GrovePath); err == nil {
			if cfg, err := tmpl.LoadConfig(); err == nil && cfg != nil {
				for _, sec := range cfg.Secrets {
					required[sec.Key] = struct{}{}
					if _, exists := secretInfo[sec.Key]; !exists {
						secretInfo[sec.Key] = api.SecretKeyInfo{
							Description: sec.Description,
							Source:      "template",
							Type:        sec.Type,
						}
					}
				}
			}
		}
	}

	keys := make([]string, 0, len(required))
	for k := range required {
		keys = append(keys, k)
	}
	return keys, secretInfo
}

// resolveHarnessConfigForEnvGather determines the harness-config name for the
// env-gather flow (pre-provisioning secret key extraction). It uses the unified
// config.ResolveHarnessConfigName with a broker-specific fallback: if the
// template name matches a valid harness-config directory or settings entry,
// it is used as the harness-config name.
func (s *Server) resolveHarnessConfigForEnvGather(req CreateAgentRequest, settings *config.VersionedSettings) string {
	// Broker-specific: treat template name as harness-config if it matches a
	// known harness-config directory or settings entry.
	cliFlag := ""
	if req.Config != nil {
		cliFlag = req.Config.HarnessConfig
	}
	if cliFlag == "" && req.Config != nil && req.Config.Template != "" {
		tpl := req.Config.Template
		if req.GrovePath != "" {
			if _, err := config.FindHarnessConfigDir(tpl, req.GrovePath); err == nil {
				cliFlag = tpl
			}
		}
		if cliFlag == "" && settings != nil {
			if _, ok := settings.HarnessConfigs[tpl]; ok {
				cliFlag = tpl
			}
		}
	}

	profileName := ""
	if req.Config != nil {
		profileName = req.Config.Profile
	}

	res, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{
		CLIFlag:     cliFlag,
		Settings:    settings,
		ProfileName: profileName,
	})
	if err != nil {
		return ""
	}
	return res.Name
}

// finalizeEnv handles the second phase of env-gather: receiving gathered env vars
// from the Hub and starting the agent with the complete environment.
func (s *Server) finalizeEnv(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req FinalizeEnvRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Look up pending state
	s.pendingEnvGatherMu.Lock()
	s.cleanupExpiredPendingLocked(time.Now())
	pending, ok := s.pendingEnvGather[id]
	if ok && pending.State == pendingStateFinalizing {
		s.pendingEnvGatherMu.Unlock()
		writeError(w, http.StatusConflict, ErrCodeConflict, "agent finalize-env already in progress", map[string]interface{}{
			"agentId": id,
		})
		return
	}
	if ok {
		pending.State = pendingStateFinalizing
		pending.UpdatedAt = time.Now()
		pending.FinalizeRuns++
		s.upsertPendingState(pending)
	}
	s.pendingEnvGatherMu.Unlock()

	if !ok {
		NotFound(w, "Pending agent")
		return
	}

	// Merge gathered env into the previously merged env
	for k, v := range req.Env {
		pending.MergedEnv[k] = v
	}

	if s.config.Debug {
		s.envSecretLog.Debug("Finalize-env: merging gathered env", "gatheredKeys", len(req.Env), "totalEnv", len(pending.MergedEnv))
	}

	origReq := pending.Request

	// Build unified start context from the original pending request + merged env
	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:            origReq.Name,
		AgentID:         origReq.ID,
		Slug:            origReq.Slug,
		GrovePath:       origReq.GrovePath,
		GroveSlug:       origReq.GroveSlug,
		GroveID:         origReq.GroveID,
		Config:          origReq.Config,
		InlineConfig:    origReq.InlineConfig,
		SharedDirs:      origReq.SharedDirs,
		HubEndpoint:     origReq.HubEndpoint,
		AgentToken:      origReq.AgentToken,
		CreatorName:     origReq.CreatorName,
		ResolvedEnv:     pending.MergedEnv,
		ResolvedSecrets: origReq.ResolvedSecrets,
		Attach:          origReq.Attach,
		HTTPRequest:     r,
	})
	if err != nil {
		TemplateError(w, err.Error())
		return
	}
	opts := sc.Opts

	if s.config.Debug {
		s.envSecretLog.Debug("Finalize-env: StartOptions built from pending request",
			"name", opts.Name,
			"grovePath", opts.GrovePath,
			"template", opts.Template,
			"image", opts.Image,
			"profile", opts.Profile,
			"harnessConfig", opts.HarnessConfig,
			"hasConfig", origReq.Config != nil,
		)
	}

	// Start the agent
	agentInfo, err := sc.Manager.Start(ctx, opts)
	if err != nil {
		// Keep pending state for retry on transient start failures.
		s.pendingEnvGatherMu.Lock()
		if cur, exists := s.pendingEnvGather[id]; exists {
			cur.State = pendingStatePending
			cur.UpdatedAt = time.Now()
			s.upsertPendingState(cur)
		}
		s.pendingEnvGatherMu.Unlock()
		RuntimeError(w, "Failed to create agent: "+err.Error())
		return
	}

	s.pendingEnvGatherMu.Lock()
	s.deletePendingState(id)
	s.pendingEnvGatherMu.Unlock()

	s.agentLifecycleLog.Info("Agent created (finalize-env)",
		"agent_id", origReq.ID, "grove_id", origReq.GroveID,
		"name", origReq.Name, "slug", origReq.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	resp := CreateAgentResponse{
		Agent:   agentInfoPtr(AgentInfoToResponse(*agentInfo)),
		Created: true,
	}

	writeJSON(w, http.StatusCreated, resp)
}

// resolveManagerForAgent returns the appropriate agent.Manager for an existing
// agent by checking the default runtime first, then falling back to auxiliary
// runtimes. This ensures stop/delete/restart operations target the correct
// runtime when agents are launched on non-default runtimes (e.g. K8s pods
// when the broker's default is Docker).
// groveID scopes the lookup to a specific grove to prevent cross-grove collision.
func (s *Server) resolveManagerForAgent(ctx context.Context, id, groveID string) agent.Manager {
	slug := strings.ToLower(id)
	filter := map[string]string{"scion.name": slug}
	if groveID != "" {
		filter["scion.grove_id"] = groveID
	}

	// Try the default manager first
	agents, err := s.manager.List(ctx, filter)
	if err == nil && len(agents) > 0 {
		return s.manager
	}

	// Fall back to auxiliary runtimes
	s.auxiliaryRuntimesMu.RLock()
	auxRuntimes := make(map[string]auxiliaryRuntime, len(s.auxiliaryRuntimes))
	for k, v := range s.auxiliaryRuntimes {
		auxRuntimes[k] = v
	}
	s.auxiliaryRuntimesMu.RUnlock()

	for _, aux := range auxRuntimes {
		auxAgents, auxErr := aux.Manager.List(ctx, filter)
		if auxErr == nil && len(auxAgents) > 0 {
			return aux.Manager
		}
	}

	// If grove-scoped lookup found nothing, retry without grove filter.
	// This handles backward compatibility with containers that lack the
	// scion.grove_id label (pre-existing agents or solo/CLI mode).
	if groveID != "" {
		fallbackFilter := map[string]string{"scion.name": slug}
		agents, err = s.manager.List(ctx, fallbackFilter)
		if err == nil && len(agents) > 0 {
			return s.manager
		}
		for _, aux := range auxRuntimes {
			auxAgents, auxErr := aux.Manager.List(ctx, fallbackFilter)
			if auxErr == nil && len(auxAgents) > 0 {
				return aux.Manager
			}
		}
	}

	// Default fallback — the agent may have already been removed or the
	// runtime is genuinely the default one (e.g. pod already deleted).
	return s.manager
}

// resolveRuntimeForAgent returns the appropriate runtime.Runtime for an
// existing agent by checking the default runtime first, then falling back
// to auxiliary runtimes. This is needed for operations that call runtime
// methods directly (e.g. Exec, GetLogs) rather than going through the manager.
// groveID scopes the lookup to a specific grove to prevent cross-grove collision.
func (s *Server) resolveRuntimeForAgent(ctx context.Context, id, groveID string) scionrt.Runtime {
	slug := strings.ToLower(id)
	filter := map[string]string{"scion.name": slug}
	if groveID != "" {
		filter["scion.grove_id"] = groveID
	}

	// Try the default manager first
	agents, err := s.manager.List(ctx, filter)
	if err == nil && len(agents) > 0 {
		return s.runtime
	}

	// Fall back to auxiliary runtimes
	s.auxiliaryRuntimesMu.RLock()
	auxRuntimes := make(map[string]auxiliaryRuntime, len(s.auxiliaryRuntimes))
	for k, v := range s.auxiliaryRuntimes {
		auxRuntimes[k] = v
	}
	s.auxiliaryRuntimesMu.RUnlock()

	for _, aux := range auxRuntimes {
		auxAgents, auxErr := aux.Manager.List(ctx, filter)
		if auxErr == nil && len(auxAgents) > 0 {
			return aux.Runtime
		}
	}

	// Backward compatibility: retry without grove filter for pre-existing containers
	if groveID != "" {
		fallbackFilter := map[string]string{"scion.name": slug}
		agents, err = s.manager.List(ctx, fallbackFilter)
		if err == nil && len(agents) > 0 {
			return s.runtime
		}
		for _, aux := range auxRuntimes {
			auxAgents, auxErr := aux.Manager.List(ctx, fallbackFilter)
			if auxErr == nil && len(auxAgents) > 0 {
				return aux.Runtime
			}
		}
	}

	return s.runtime
}

// resolveManagerForOpts returns the appropriate agent.Manager for the given
// start options. If opts.Profile resolves to a different runtime than the
// broker's default, a temporary manager is created with the profile-specific
// runtime. Otherwise the broker's shared manager is returned.
func (s *Server) resolveManagerForOpts(opts api.StartOptions) agent.Manager {
	if opts.Profile == "" {
		return s.manager
	}

	// Load settings to check if the profile explicitly specifies a different runtime.
	// If no settings exist or the profile isn't defined, stick with the default manager.
	projectDir, _ := config.GetResolvedProjectDir(opts.GrovePath)
	vs, _, _ := config.LoadEffectiveSettings(projectDir)
	if vs == nil {
		return s.manager
	}

	_, runtimeType, err := vs.ResolveRuntime(opts.Profile)
	if err != nil {
		// Profile or its runtime not found in settings; use default
		return s.manager
	}

	if runtimeType == s.runtime.Name() {
		return s.manager
	}

	// Profile specifies a different runtime - resolve and create a manager.
	// Cache it as an auxiliary manager so LookupContainerID can find agents
	// created on non-default runtimes (e.g. K8s pods when default is docker).
	resolved := agent.ResolveRuntime(opts.GrovePath, opts.Name, opts.Profile)

	if s.config.Debug {
		s.agentLifecycleLog.Debug("Profile resolved to different runtime",
			"agent", opts.Name, "profile", opts.Profile,
			"defaultRuntime", s.runtime.Name(),
			"resolvedRuntime", resolved.Name(),
		)
	}

	mgr := agent.NewManager(resolved)

	if resolved.Name() != "error" {
		s.auxiliaryRuntimesMu.Lock()
		s.auxiliaryRuntimes[resolved.Name()] = auxiliaryRuntime{Runtime: resolved, Manager: mgr}
		s.auxiliaryRuntimesMu.Unlock()
	}

	return mgr
}

// Helper functions

// resolveGroveSettingsDir returns the directory containing settings.yaml for a grove.
// For linked groves, grovePath already points to the .scion directory.
// For hub-native groves, grovePath is the workspace parent, so settings
// live in the .scion subdirectory.
func resolveGroveSettingsDir(grovePath string) string {
	if config.GetSettingsPath(grovePath) != "" {
		return grovePath
	}
	candidate := filepath.Join(grovePath, ".scion")
	if config.GetSettingsPath(candidate) != "" {
		return candidate
	}
	return grovePath // fallback to original
}

// forceHeartbeatAll sends an immediate heartbeat on all hub connections so the
// hub gets updated container status without waiting for the next periodic interval.
func (s *Server) forceHeartbeatAll(action, agentID string) {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	for _, conn := range s.hubConnections {
		if conn.Heartbeat != nil {
			hb := conn.Heartbeat
			go func() {
				if err := hb.ForceHeartbeat(context.Background()); err != nil {
					s.agentLifecycleLog.Error("Failed to send forced heartbeat after "+action, "agent_id", agentID, "error", err)
				}
			}()
		}
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func agentInfoPtr(a AgentResponse) *AgentResponse {
	return &a
}

// ============================================================================
// Grove Endpoints
// ============================================================================

// handleGroveBySlug routes requests to /api/v1/groves/{slug}.
func (s *Server) handleGroveBySlug(w http.ResponseWriter, r *http.Request) {
	slug := extractID(r, "/api/v1/groves")
	if slug == "" {
		NotFound(w, "grove")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.deleteGrove(w, r, slug)
	default:
		MethodNotAllowed(w)
	}
}

// deleteGrove removes the local hub-native grove directory for the given slug.
// Returns 204 on success (including when the directory doesn't exist).
func (s *Server) deleteGrove(w http.ResponseWriter, r *http.Request, slug string) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		RuntimeError(w, "Failed to get global dir: "+err.Error())
		return
	}

	grovePath := filepath.Join(globalDir, "groves", slug)

	// Path traversal protection: ensure the resolved path stays inside the groves directory.
	grovesBase := filepath.Join(globalDir, "groves")
	absGrove, err := filepath.Abs(grovePath)
	if err != nil {
		RuntimeError(w, "Failed to resolve grove path: "+err.Error())
		return
	}
	absBase, err := filepath.Abs(grovesBase)
	if err != nil {
		RuntimeError(w, "Failed to resolve groves base path: "+err.Error())
		return
	}
	if !strings.HasPrefix(absGrove, absBase+string(filepath.Separator)) {
		s.agentLifecycleLog.Warn("grove cleanup path traversal blocked", "slug", slug, "resolved", absGrove)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(grovePath); os.IsNotExist(err) {
		// Already gone — idempotent success
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := os.RemoveAll(grovePath); err != nil {
		s.agentLifecycleLog.Warn("failed to remove grove directory", "slug", slug, "path", grovePath, "error", err)
		RuntimeError(w, "Failed to remove grove directory: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Removed hub-native grove directory", "slug", slug, "path", grovePath)
	w.WriteHeader(http.StatusNoContent)
}

// findAgentInHubNativeGroves scans hub-native grove directories
// (~/.scion/groves/<slug>/.scion/) for an agent directory matching the given
// name. Returns the .scion project dir path if found, or empty string.
// This is used as a fallback when the container is missing and the agent's
// grove path can't be determined from container labels.
func findAgentInHubNativeGroves(agentName string) string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	grovesDir := filepath.Join(globalDir, "groves")
	entries, err := os.ReadDir(grovesDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scionDir := filepath.Join(grovesDir, entry.Name(), ".scion")
		agentDir := filepath.Join(scionDir, "agents", agentName)
		if _, err := os.Stat(agentDir); err == nil {
			return scionDir
		}
	}
	return ""
}

// isLocalhostEndpoint returns true if the given endpoint URL refers to a
// loopback address (localhost, 127.0.0.1, [::1], etc.). This is used to
// decide whether the ContainerHubEndpoint bridge address should be
// substituted — containers can reach external hosts directly but need a
// bridge address to reach services on the host's loopback interface.
func isLocalhostEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
