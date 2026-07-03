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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// matchesAgent checks whether an agent matches the given id and optional projectID.
// When projectID is provided, it must match for uniqueness across projects.
// When projectID is empty, matching falls back to name/containerID/slug only
// (backward compatible with solo/CLI mode and pre-existing containers).
func matchesAgent(a api.AgentInfo, id, projectID string) bool {
	nameMatch := a.Name == id || a.ContainerID == id || a.Slug == id
	if !nameMatch {
		return false
	}
	if projectID == "" {
		return true
	}
	// Check runtime labels first (canonical project_id, then legacy grove_id),
	// then ProjectID field.
	if labelProjectID := projectcompat.ProjectIDFromLabels(a.Labels); labelProjectID != "" {
		return labelProjectID == projectID
	}
	if a.ProjectID != "" {
		return a.ProjectID == projectID
	}
	// No project_id on container — match anyway for backward compatibility
	// with containers created before project_id labeling was added.
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

	// NFS mount health
	if s.nfsMountReconciler != nil {
		checks["nfs_mounts"] = s.nfsMountReconciler.HealthCheckString()
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

	// Add optional filters — support both projectId and legacy groveId.
	if projectID := query.Get("projectId"); projectID != "" {
		filter["scion.project_id"] = projectID
	} else if projectID := query.Get("groveId"); projectID != "" {
		filter["scion.project_id"] = projectID
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

	// Dedup by name+projectID to prevent collision across projects while still
	// deduplicating the same agent found on multiple runtimes.
	agentKey := func(a api.AgentInfo) string {
		pid := a.ProjectID
		if pid == "" {
			pid = projectcompat.ProjectIDFromLabels(a.Labels)
		}
		return a.Name + "\x00" + pid
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
	createStart := time.Now()

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
		s.agentLifecycleLog.Debug("Creating agent", "agent_id", req.ID, "project_id", req.ProjectID, "name", req.Name, "slug", req.Slug)
		s.agentLifecycleLog.Debug("Hub credentials",
			"agent_id", req.ID,
			"project_id", req.ProjectID,
			"hubEndpoint", req.HubEndpoint,
			"hasToken", req.AgentToken != "",
			"slug", req.Slug,
		)
		if req.Config != nil {
			s.agentLifecycleLog.Debug("Agent configuration",
				"agent_id", req.ID,
				"project_id", req.ProjectID,
				"template", req.Config.Template,
				"image", req.Config.Image,
				"templateID", req.Config.TemplateID,
			)
		}
	}

	// Resolve project path early for env-gather (needs settings access before buildStartContext)
	if req.ProjectSlug != "" && req.ProjectPath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to resolve global dir")
			RuntimeError(w, "Failed to get global dir: "+err.Error())
			return
		}
		projectsPath := filepath.Join(globalDir, "projects", req.ProjectSlug)
		if !hasWorkspaceContent(projectsPath) {
			// fallback to groves/ for backward compatibility
			legacyPath := filepath.Join(globalDir, "groves", req.ProjectSlug)
			if hasWorkspaceContent(legacyPath) {
				projectsPath = legacyPath
			}
		}
		req.ProjectPath = projectsPath
	}

	// Env-gather: if GatherEnv is true, evaluate env completeness before building full context.
	// This needs the resolved project path and merged env to determine which keys are missing.
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

		// Hydrate hub-managed harness-config before extracting required keys
		// so that config-driven auth metadata is available during env-gather.
		// Graceful degradation: if hydration fails, fall back to on-disk only.
		var hydratedHCPath string
		if req.Config != nil && (req.Config.HarnessConfigID != "" || req.Config.HarnessConfigHash != "") {
			hubConn := s.resolveHubConnection(r)
			if hubConn != nil {
				hydrateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				hcPath, err := s.hydrateHarnessConfig(hydrateCtx, req.Config, hubConn)
				cancel()
				if err != nil {
					if s.config.Debug {
						s.envSecretLog.Debug("Env-gather: harness-config hydration failed, falling back to on-disk",
							"error", err.Error(),
						)
					}
				} else {
					hydratedHCPath = hcPath
				}
			}
		}

		required, secretInfo := s.extractRequiredEnvKeys(req, hydratedHCPath)
		if s.config.Debug {
			s.envSecretLog.Debug("Env-gather: evaluating env completeness",
				"gatherEnv", req.GatherEnv,
				"projectPath", req.ProjectPath,
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

	// Debug log project path
	if s.config.Debug && req.ProjectPath != "" {
		s.agentLifecycleLog.Debug("Using project path from Hub", "agent_id", req.ID, "path", req.ProjectPath)
	}

	// Reject global project in multi-hub mode
	if s.isMultiHubMode() && s.isGlobalProject(req.ProjectID, req.ProjectPath) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": map[string]string{
				"code":    "global_grove_disabled",
				"message": "Global project is disabled when broker is connected to multiple hubs",
			},
		})
		return
	}

	// Phase 3 broker policy: refuse container-script harness dispatches
	// unless the broker has opted in. We check before buildStartContext so
	// the failure happens before the broker mounts project state, downloads
	// workspaces, or projects secrets.
	if name, entry, ok := s.lookupHarnessConfigForPolicy(req); ok {
		if d := s.evaluateHarnessConfigPolicy(name, entry); !d.OK {
			markAttemptFailed(d.HTTPStatus, d.Message)
			writeError(w, d.HTTPStatus, d.Code, d.Message, nil)
			return
		}
	}

	// N1-7: Ensure NFS shares are mounted before dispatch (no-op when backend=local).
	if err := s.ensureNFSMountsReady(); err != nil {
		markAttemptFailed(http.StatusServiceUnavailable, "NFS mount check failed: "+err.Error())
		writeError(w, http.StatusServiceUnavailable, "nfs_unavailable",
			"NFS workspace storage is not available: "+err.Error(), nil)
		return
	}

	// Build unified start context (project path, env, template, git-clone, secrets, manager)
	s.agentLifecycleLog.Info("Agent dispatch: pre-flight complete",
		"agent_id", req.ID, "name", req.Name, "elapsed", time.Since(createStart).String())
	buildCtxStart := time.Now()
	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:            req.Name,
		AgentID:         req.ID,
		Slug:            req.Slug,
		ProjectPath:     req.ProjectPath,
		ProjectSlug:     req.ProjectSlug,
		ProjectID:       req.ProjectID,
		Config:          req.Config,
		InlineConfig:    req.InlineConfig,
		SharedDirs:      req.SharedDirs,
		HubEndpoint:     req.HubEndpoint,
		AgentToken:      req.AgentToken,
		CreatorName:     req.CreatorName,
		ResolvedEnv:     req.ResolvedEnv,
		ResolvedSecrets: req.ResolvedSecrets,
		NoAuth:          req.NoAuth,
		Attach:          req.Attach,
		WorkspaceMode:   req.WorkspaceMode,
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
	s.agentLifecycleLog.Info("Agent dispatch: buildStartContext complete",
		"agent_id", req.ID, "name", req.Name, "elapsed", time.Since(buildCtxStart).String())

	// If WorkspaceStoragePath is set, download workspace from GCS (non-git bootstrap)
	if req.WorkspaceStoragePath != "" {
		// For hub-managed projects (ProjectSlug set), use the conventional path
		// ~/.scion.groves/<slug>/ instead of the worktree-based path.
		var workspaceDir string
		if req.ProjectSlug != "" {
			globalDir, err := config.GetGlobalDir()
			if err != nil {
				markAttemptFailed(http.StatusInternalServerError, "failed to resolve global dir")
				RuntimeError(w, "Failed to get global dir: "+err.Error())
				return
			}
			workspaceDir = filepath.Join(globalDir, "projects", req.ProjectSlug)
			if !hasWorkspaceContent(workspaceDir) {
				// fallback to groves/ for backward compatibility
				legacyPath := filepath.Join(globalDir, "groves", req.ProjectSlug)
				if hasWorkspaceContent(legacyPath) {
					workspaceDir = legacyPath
				}
			}
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
				"projectSlug", req.ProjectSlug,
			)
		}

		if err := gcp.SyncFromGCS(ctx, bucket, req.WorkspaceStoragePath+"/files", workspaceDir); err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to download workspace from GCS")
			RuntimeError(w, "Failed to download workspace from GCS: "+err.Error())
			return
		}

		opts.Workspace = workspaceDir
		// Keep opts.ProjectPath so that ProvisionAgent resolves the correct
		// agent directory. The explicit workspace takes precedence over the
		// worktree logic in ProvisionAgent, so no worktree will be created.

		// Write a workspace marker so in-container CLI
		// can discover the project context and use the Hub API.
		if req.ProjectID != "" && req.ProjectSlug != "" {
			if writeErr := config.WriteWorkspaceMarker(workspaceDir, req.ProjectID, req.ProjectSlug, req.ProjectSlug); writeErr != nil {
				s.agentLifecycleLog.Warn("Failed to write workspace marker", "agent_id", req.ID, "project_id", req.ProjectID, "error", writeErr)
			}
		}
	}

	// Inject skill resolver from Hub connection for skill provisioning.
	if conn := s.resolveHubConnection(r); conn != nil && conn.HubClient != nil {
		hubResolver := agent.NewHubSkillResolver(conn.HubClient.Skills())
		router := agent.NewRoutingSkillResolver(hubResolver)
		ghResolver := agent.NewGitHubSkillResolver()
		router.Register("gh", ghResolver)

		// GCP resolver uses Hub API for registry alias lookup.
		registrySvc := conn.HubClient.SkillRegistries()
		gcpLookup := func(ctx context.Context, name string) (*agent.RegistryLookupResult, error) {
			reg, err := registrySvc.Get(ctx, name)
			if err != nil {
				return nil, err
			}
			if reg == nil {
				return nil, fmt.Errorf("registry %q not found", name)
			}
			return &agent.RegistryLookupResult{
				Name:     reg.Name,
				Endpoint: reg.Endpoint,
				Type:     reg.Type,
				Status:   reg.Status,
			}, nil
		}
		router.Register("gcp-skill", agent.NewGCPSkillResolver(gcpLookup))

		var resolver agent.SkillResolver = router
		if s.skCache != nil {
			resolver = agent.NewCachingSkillResolver(resolver, s.skCache)
		}
		ctx = agent.ContextWithSkillResolver(ctx, resolver)
		if req.ProjectID != "" {
			ctx = agent.ContextWithResolveProjectID(ctx, req.ProjectID)
		}
		if req.UserID != "" {
			ctx = agent.ContextWithResolveUserID(ctx, req.UserID)
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
			"agent_id", req.ID, "project_id", req.ProjectID,
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
	startOpStart := time.Now()
	agentInfo, err := sc.Manager.Start(ctx, opts)
	if err != nil {
		markAttemptFailed(http.StatusInternalServerError, "failed to create agent")

		s.agentLifecycleLog.Error("Agent create failed",
			"agent_id", req.ID, "project_id", req.ProjectID,
			"name", req.Name, "slug", req.Slug,
			"error", err)

		// Clean up provisioned agent files so they don't become orphans.
		if opts.ProjectPath != "" {
			if _, cleanupErr := agent.DeleteAgentFiles(opts.Name, opts.ProjectPath, true); cleanupErr != nil {
				s.agentLifecycleLog.Warn("Failed to clean up agent files after start failure",
					"agent_id", req.ID, "project_id", req.ProjectID, "agent", opts.Name, "error", cleanupErr)
			} else {
				s.agentLifecycleLog.Info("Cleaned up provisioned agent files after start failure",
					"agent_id", req.ID, "project_id", req.ProjectID, "agent", opts.Name)
			}
		}
		RuntimeError(w, "Failed to create agent: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Agent dispatch: start complete",
		"agent_id", req.ID, "name", req.Name,
		"startElapsed", time.Since(startOpStart).String(),
		"totalElapsed", time.Since(createStart).String())
	s.agentLifecycleLog.Info("Agent created",
		"agent_id", req.ID, "project_id", req.ProjectID,
		"name", req.Name, "slug", req.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	// Log auth resolution info visible in broker logs
	for _, w := range agentInfo.Warnings {
		if strings.HasPrefix(w, "Auth:") {
			s.agentLifecycleLog.Info("Agent auth resolution", "agent_id", req.ID, "project_id", req.ProjectID, "agent", req.Name, "result", w)
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

// hydrateTemplate resolves a Hub template to a local directory for provisioning.
// Returns the local template path, or empty string if no Hub template was specified.
//
// Resolution always goes through the connection's storage backend — there is a
// single read path for every topology. When the backend is the local filesystem
// (co-located workstation mode) the broker reads the resource directly from the
// backend's on-disk location; otherwise it hydrates from remote storage via
// signed URLs and the content-addressed cache.
func (s *Server) hydrateTemplate(ctx context.Context, cfg *CreateAgentConfig, conn *HubConnection) (string, error) {
	// Check if we have template info from Hub
	if cfg.TemplateID == "" && cfg.TemplateHash == "" {
		// No Hub template info provided, use local template handling
		return "", nil
	}

	// Local-backend direct read: the backend is the filesystem, so resolution is
	// a local path read — no HTTP, no cache.
	if conn.LocalStorage != nil {
		ref := cfg.TemplateID
		if ref == "" {
			ref = cfg.Template
		}
		path, err := s.resolveLocalResource(ctx, storage.ResourceKindTemplate, ref, conn)
		if err != nil {
			return "", err
		}
		if path != "" {
			return path, nil
		}
		// Not present in the backend yet — fall through to hydration.
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

// hydrateHarnessConfig resolves a Hub harness-config to a local directory for
// provisioning, mirroring hydrateTemplate. Returns the local directory path, or
// an empty string when no Hub harness-config was specified (the broker then
// falls back to its on-disk harness-config search). This is the §7.3 step-4
// consume path that makes harness-configs usable from a broker that lacks the
// config on its local filesystem.
func (s *Server) hydrateHarnessConfig(ctx context.Context, cfg *CreateAgentConfig, conn *HubConnection) (string, error) {
	if cfg == nil || (cfg.HarnessConfigID == "" && cfg.HarnessConfigHash == "") {
		return "", nil
	}

	// Local-backend direct read (co-located workstation mode).
	if conn.LocalStorage != nil {
		ref := cfg.HarnessConfigID
		if ref == "" {
			ref = cfg.HarnessConfig
		}
		path, err := s.resolveLocalResource(ctx, storage.ResourceKindHarnessConfig, ref, conn)
		if err != nil {
			return "", err
		}
		if path != "" {
			return path, nil
		}
		// Not present in the backend yet — fall through to hydration.
	}

	resolver := conn.HCResolver
	if resolver == nil {
		return "", nil
	}

	if cfg.HarnessConfigHash != "" && cfg.HarnessConfigID != "" {
		return resolver.ResolveWithHash(ctx, cfg.HarnessConfigID, cfg.HarnessConfigHash)
	}
	if cfg.HarnessConfigID != "" {
		return resolver.Resolve(ctx, cfg.HarnessConfigID)
	}

	return "", nil
}

// localObjectResolver is implemented by storage backends (the local filesystem
// backend) that can map an object path to an absolute on-disk path. This is the
// LocalDirBackend seam from §7.3: one assertion, used for every resource kind.
type localObjectResolver interface {
	ObjectFSPath(objectPath string) string
}

// resolveLocalResource resolves a resource of the given kind directly from a
// co-located local storage backend. It returns the on-disk directory backing the
// resource, or an empty string if the backend cannot serve it directly (caller
// then falls back to hydration). Metadata is fetched from the Hub to learn the
// resource's scope/slug; over a co-located loopback connection this is a cheap
// DB read.
func (s *Server) resolveLocalResource(ctx context.Context, kind storage.ResourceKind, ref string, conn *HubConnection) (string, error) {
	resolver, ok := conn.LocalStorage.(localObjectResolver)
	if !ok || conn.HubClient == nil || ref == "" {
		return "", nil
	}

	objectPath, err := s.resourceObjectPath(ctx, kind, ref, conn)
	if err != nil {
		return "", err
	}
	if objectPath == "" {
		return "", nil
	}

	dir := resolver.ObjectFSPath(objectPath)
	info, statErr := os.Stat(dir)
	if statErr != nil || !info.IsDir() {
		// Backend doesn't have the files on disk; let the caller hydrate.
		return "", nil
	}
	return dir, nil
}

// resourceObjectPath fetches resource metadata over the hub connection and
// returns its storage object path, falling back to the kind-keyed scope layout
// when the record carries no explicit StoragePath.
func (s *Server) resourceObjectPath(ctx context.Context, kind storage.ResourceKind, ref string, conn *HubConnection) (string, error) {
	switch kind {
	case storage.ResourceKindHarnessConfig:
		hc, err := conn.HubClient.HarnessConfigs().Get(ctx, ref)
		if err != nil {
			return "", wrapResourceMetaErr(err, "harness-config")
		}
		if hc == nil {
			return "", nil
		}
		if hc.StoragePath != "" {
			return hc.StoragePath, nil
		}
		return storage.ResourceStoragePath(kind, hc.Scope, hc.ScopeID, hc.Slug), nil
	default:
		tmpl, err := conn.HubClient.Templates().Get(ctx, ref)
		if err != nil {
			return "", wrapResourceMetaErr(err, "template")
		}
		if tmpl == nil {
			return "", nil
		}
		if tmpl.StoragePath != "" {
			return tmpl.StoragePath, nil
		}
		scopeID := tmpl.ScopeID
		if scopeID == "" {
			scopeID = tmpl.ProjectID
		}
		return storage.ResourceStoragePath(kind, tmpl.Scope, scopeID, tmpl.Slug), nil
	}
}

// wrapResourceMetaErr normalizes a hub metadata-fetch error, preserving the
// HubConnectivityError signal used by the provision path.
func wrapResourceMetaErr(err error, label string) error {
	if templatecache.IsHubConnectivityError(err) {
		return &templatecache.HubConnectivityError{Cause: err}
	}
	return fmt.Errorf("failed to get %s metadata: %w", label, err)
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}

	// Extract projectId (or legacy groveId) from query params for project-scoped agent resolution.
	// This prevents cross-project agent collision when two agents with the same
	// name exist in different projects on the same broker.
	projectID := r.URL.Query().Get("projectId")
	if projectID == "" {
		projectID = r.URL.Query().Get("groveId")
	}

	// Handle WebSocket attach for PTY
	if action == "attach" && isPTYWebSocketUpgrade(r) {
		s.handleAgentAttach(w, r)
		return
	}

	// Handle actions
	if action != "" {
		s.handleAgentAction(w, r, id, projectID, action)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAgent(w, r, id, projectID)
	case http.MethodDelete:
		s.deleteAgent(w, r, id, projectID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Resolve the correct manager (checks auxiliary runtimes if needed)
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	for _, agent := range agents {
		if matchesAgent(agent, id, projectID) {
			writeJSON(w, http.StatusOK, AgentInfoToResponse(agent))
			return
		}
	}

	NotFound(w, "Agent")
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"
	softDelete := query.Get("softDelete") == "true"

	// Resolve the correct manager for this agent (may be on an auxiliary runtime)
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// Get the agent's project path and project ID before stopping (needed for file deletion and logging)
	var projectPath, agentProjectID string
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err == nil {
		for _, a := range agents {
			if matchesAgent(a, id, projectID) {
				projectPath = a.ProjectPath
				agentProjectID = a.ProjectID
				if agentProjectID == "" {
					agentProjectID = a.Project
				}
				break
			}
		}
	}

	// If no project path was found (container missing or no annotation), check
	// hub-managed project directories for the agent's files. Without this,
	// agents in hub-managed projects (~/.scion.projects/<slug>/) are silently
	// skipped during file cleanup because the default filesystem scan only
	// checks the CWD-resolved project dir and global ~/.scion.
	if projectPath == "" && deleteFiles {
		if resolved := findAgentInHubManagedProjects(id); resolved != "" {
			projectPath = resolved
			s.agentLifecycleLog.Debug("Resolved agent project path from hub-managed projects",
				"agent_id", id, "path", projectPath)
		}
	}

	// If this is a soft-delete, mark agent-info.json with deleted status before cleanup
	if softDelete && projectPath != "" {
		deletedAtStr := query.Get("deletedAt")
		if err := agent.UpdateAgentConfig(id, projectPath, "deleted", "", ""); err != nil {
			s.agentLifecycleLog.Warn("Failed to mark agent as deleted in agent-info.json", "agent_id", id, "error", err)
		}
		if deletedAtStr != "" {
			if deletedAt, err := time.Parse(time.RFC3339, deletedAtStr); err == nil {
				if err := agent.UpdateAgentDeletedAt(id, projectPath, deletedAt); err != nil {
					s.agentLifecycleLog.Warn("Failed to write deletedAt to agent-info.json", "agent_id", id, "error", err)
				}
			}
		}
	}

	_, err = mgr.Delete(ctx, id, deleteFiles, projectPath, removeBranch)
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
			"agent_id", id, "project_id", agentProjectID,
			"delete_files", deleteFiles, "remove_branch", removeBranch)
	} else {
		s.agentLifecycleLog.Info("Agent deleted",
			"agent_id", id, "project_id", agentProjectID,
			"delete_files", deleteFiles, "remove_branch", removeBranch)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, projectID, action string) {
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
		s.startAgent(w, r, id, projectID)
	case api.AgentActionStop:
		s.stopAgent(w, r, id, projectID)
	case api.AgentActionSuspend:
		s.stopAgent(w, r, id, projectID)
	case api.AgentActionRestart:
		s.restartAgent(w, r, id, projectID)
	case api.AgentActionMessage:
		s.sendMessage(w, r, id, projectID)
	case api.AgentActionExec:
		s.execCommand(w, r, id, projectID)
	case api.AgentActionResetAuth:
		s.resetAuth(w, r, id, projectID)
	case api.AgentActionLogs:
		s.getLogs(w, r, id, projectID)
	case api.AgentActionStats:
		s.getStats(w, r, id, projectID)
	case api.AgentActionHasPrompt:
		s.checkAgentPrompt(w, r, id, projectID)
	case api.AgentActionFinalizeEnv:
		s.finalizeEnv(w, r, id)
	}
}

func (s *Server) startAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Read optional task, projectPath, projectSlug, harnessConfig, and resolvedEnv from request body
	var startReq struct {
		Task            string               `json:"task"`
		ProjectPath     string               `json:"grovePath"`
		ProjectSlug     string               `json:"groveSlug"`
		HarnessConfig   string               `json:"harnessConfig"`
		ResolvedEnv     map[string]string    `json:"resolvedEnv"`
		ResolvedSecrets []api.ResolvedSecret `json:"resolvedSecrets,omitempty"`
		InlineConfig    *api.ScionConfig     `json:"inlineConfig,omitempty"`
		SharedDirs      []api.SharedDir      `json:"sharedDirs,omitempty"`
		// SharedWorkspace must be re-sent on every start: hub-project agents
		// share a single git checkout instead of being given a worktree, and
		// without this flag the broker would create a worktree on restart.
		SharedWorkspace bool `json:"sharedWorkspace,omitempty"`
		// Resume requests harness session continuation (e.g. Claude
		// --continue). The hub is the source of truth and sets this from the
		// agent's stored phase; when unset we fall back to GetSavedPhase below.
		Resume bool `json:"resume,omitempty"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&startReq); err != nil {
			s.agentLifecycleLog.Debug("No task in start request body (ignoring decode error)", "agent_id", id, "error", err)
		}
	}

	s.agentLifecycleLog.Debug("startAgent called", "agent_id", id, "task", startReq.Task, "projectPath", startReq.ProjectPath, "projectSlug", startReq.ProjectSlug, "harnessConfig", startReq.HarnessConfig, "resolvedEnvCount", len(startReq.ResolvedEnv))

	// Build config for buildStartContext (startAgent uses a subset of CreateAgentConfig)
	var cfg *CreateAgentConfig
	if startReq.Task != "" || startReq.HarnessConfig != "" || len(startReq.SharedDirs) > 0 || startReq.SharedWorkspace {
		cfg = &CreateAgentConfig{
			Task:            startReq.Task,
			HarnessConfig:   startReq.HarnessConfig,
			SharedDirs:      startReq.SharedDirs,
			SharedWorkspace: startReq.SharedWorkspace,
		}
	}

	// Parity with the create path: populate the dedicated AgentToken field from
	// the hub-minted token in ResolvedEnv so buildStartContext treats it as an
	// explicit token rather than relying on the resolvedEnv-kept fallback. The
	// precedence in buildStartContext step 3 keeps this token regardless, but
	// setting it here makes the start path behave like create.
	startContextAgentToken := startReq.ResolvedEnv["SCION_AUTH_TOKEN"]

	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:            id,
		ProjectPath:     startReq.ProjectPath,
		ProjectSlug:     startReq.ProjectSlug,
		Config:          cfg,
		ResolvedEnv:     startReq.ResolvedEnv,
		ResolvedSecrets: startReq.ResolvedSecrets,
		SharedDirs:      startReq.SharedDirs,
		AgentToken:      startContextAgentToken,
		HTTPRequest:     r,
	})
	if err != nil {
		RuntimeError(w, err.Error())
		return
	}
	opts := sc.Opts

	// If project path wasn't in the request, fall back to looking up from an existing container
	if startReq.ProjectPath == "" && startReq.ProjectSlug == "" && opts.ProjectPath == "" {
		agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
		if err != nil {
			RuntimeError(w, "Failed to list agents: "+err.Error())
			return
		}
		for i := range agents {
			if matchesAgent(agents[i], id, projectID) {
				if agents[i].ProjectPath != "" {
					opts.ProjectPath = agents[i].ProjectPath
				}
				break
			}
		}
	}

	// Apply updated InlineConfig to scion-agent.json before starting.
	if startReq.InlineConfig != nil && opts.ProjectPath != "" {
		s.applyInlineConfigUpdate(id, opts.ProjectPath, startReq.InlineConfig, startReq.SharedWorkspace)
	}

	// Resolve saved profile for runtime selection
	if opts.ProjectPath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.ProjectPath)
	}

	// The hub is the source of truth for resume intent: when it sets
	// req.Resume the harness must continue its prior session. We still fall
	// back to reading the saved phase from disk when the hub did not specify
	// resume (e.g. the local CLI path, which does not send this flag).
	if startReq.Resume {
		opts.Resume = true
	} else if opts.ProjectPath != "" {
		savedPhase := agent.GetSavedPhase(id, opts.ProjectPath)
		if savedPhase == string(state.PhaseSuspended) {
			opts.Resume = true
		}
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
		"agent_id", id, "project_id", agentInfo.ProjectID,
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
//
// sharedWorkspace branches the path: shared-workspace agents store
// scion-agent.json externally (~/.scion.project-configs/<slug>__<uuid>/.scion/
// agents/<name>/) so siblings cannot read it via /workspace.
func (s *Server) applyInlineConfigUpdate(agentName, projectPath string, inlineConfig *api.ScionConfig, sharedWorkspace bool) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to resolve project dir", "agent", agentName, "error", err)
		return
	}
	agentDir := config.GetAgentDir(projectDir, agentName, sharedWorkspace)
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

// projectScopedTarget resolves an agent slug to its project-scoped container
// identifier (container ID for docker/podman, pod name for k8s) via
// LookupContainerID. When multiple projects on this broker have an agent with
// the same slug, this ensures single-container operations act on the agent in
// the requested project rather than whichever the slug matches first.
//
// When a projectID is supplied but the agent cannot be resolved, it returns ""
// — callers must treat that as "not found in this project" rather than falling
// back to the bare slug, which would risk acting on a same-slug agent in a
// different project. Only when no projectID is supplied does it degrade to the
// original id for backward compatibility (solo/CLI mode, unlabeled containers).
// agentsWithoutProjectLabel returns the subset of agents that carry no project
// label (neither scion.grove_id nor scion.project_id). The project-scoped
// lookups fall back to a slug-only search for backward compatibility with
// pre-existing / solo-mode containers that predate project labels; that
// fallback must only match such genuinely unlabeled containers. A container
// labeled for a *different* project must never satisfy a project-scoped
// request, or same-slug agents across projects would collide.
func agentsWithoutProjectLabel(agents []api.AgentInfo) []api.AgentInfo {
	filtered := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		if a.Labels["scion.grove_id"] == "" && a.Labels["scion.project_id"] == "" {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

func (s *Server) projectScopedTarget(ctx context.Context, id, projectID string) string {
	if containerID, err := s.LookupContainerID(ctx, id, projectID); err == nil && containerID != "" {
		return containerID
	}
	if projectID != "" {
		return ""
	}
	return id
}

func (s *Server) stopAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// Resolve the project-scoped container so that same-slug agents in
	// different projects on this broker don't collide. An empty target means
	// the agent isn't present in this project; treat that as an idempotent
	// no-op rather than stopping a same-slug agent in another project.
	target := s.projectScopedTarget(ctx, id, projectID)
	if target == "" {
		s.agentLifecycleLog.Info("Agent stopped (not found in project)",
			"agent_id", id,
			"phase", string(state.PhaseStopped))
		s.forceHeartbeatAll("stop", id)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":  "accepted",
			"message": "Stop operation accepted",
		})
		return
	}
	if err := mgr.Stop(ctx, target, ""); err != nil {
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

func (s *Server) restartAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
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

	// Look up agent to get its name and project path
	agentName := id
	var projectPath string
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err == nil {
		for i := range agents {
			if matchesAgent(agents[i], id, projectID) {
				agentName = agents[i].Name
				projectPath = agents[i].ProjectPath
				break
			}
		}
	}

	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:        agentName,
		ProjectPath: projectPath,
		ResolvedEnv: restartReq.ResolvedEnv,
		HTTPRequest: r,
	})
	if err != nil {
		RuntimeError(w, err.Error())
		return
	}
	opts := sc.Opts

	if opts.ProjectPath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.ProjectPath)
	}

	// Stop then start — tolerate stop errors since the container may already
	// be exited and the subsequent start will handle cleanup.
	// Use resolveManagerForAgent to find the agent on auxiliary runtimes.
	stopMgr := s.resolveManagerForAgent(ctx, id, projectID)
	stopTarget := s.projectScopedTarget(ctx, id, projectID)
	// An empty target means the agent isn't present in this project — skip the
	// stop (don't risk stopping a same-slug agent in another project) and let
	// the start below create it.
	if stopTarget == "" {
		s.agentLifecycleLog.Warn("Restart: agent not found in project, proceeding with start", "agent_id", id)
	} else if err := stopMgr.Stop(ctx, stopTarget, ""); err != nil {
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
		"agent_id", id, "project_id", agentInfo.ProjectID,
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

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, id, projectID string) {
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
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// Raw messages bypass the paste buffer and debounce, sending literal
	// bytes via tmux send-keys with no trailing Enter keypresses.
	isRaw := req.StructuredMessage != nil && req.StructuredMessage.Raw
	if isRaw {
		if err := mgr.MessageRaw(ctx, id, projectID, deliveryText); err != nil {
			if strings.Contains(err.Error(), "not found") {
				NotFound(w, "Agent")
				return
			}
			RuntimeError(w, "Failed to send raw message: "+err.Error())
			return
		}
	} else {
		if err := mgr.Message(ctx, id, projectID, deliveryText, req.Interrupt); err != nil {
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
	if req.ProjectID != "" {
		logAttrs = append(logAttrs, "project_id", req.ProjectID)
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

func (s *Server) execCommand(w http.ResponseWriter, r *http.Request, id, projectID string) {
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

	// Apply timeout if specified.
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
		defer cancel()
	}

	// Resolve the correct runtime for this agent (may be on an auxiliary runtime like K8s).
	// projectID scopes the lookup so that, when multiple projects on this broker
	// have an agent with the same slug, we operate on the agent in the requested
	// project rather than whichever the runtime happens to match by slug first.
	rt := s.resolveRuntimeForAgent(ctx, id, projectID)

	// Resolve the project-scoped container identifier (container ID for
	// docker/podman, pod name for k8s) and exec against that rather than the
	// bare slug. Without this, rt.Exec resolves the slug to a container across
	// all projects and can target the wrong agent — e.g. "scion look
	// coordinator" in project A showing project B's terminal. Mirrors the
	// project-scoped lookup used by the PTY attach handler.
	// LookupContainerID can return an empty identifier without an error (e.g.
	// a matching agent record with no resolvable container), so guard against
	// both — execing an empty target would fall back to slug resolution inside
	// the runtime and reintroduce the cross-project collision.
	target, err := s.LookupContainerID(ctx, id, projectID)
	if err != nil || target == "" {
		NotFound(w, "Agent")
		return
	}

	output, err := rt.Exec(ctx, target, req.Command)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			writeJSON(w, http.StatusOK, ExecResponse{
				Output:   output,
				ExitCode: exitErr.ExitCode(),
			})
			return
		}
		RuntimeError(w, "Failed to execute command: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ExecResponse{
		Output:   output,
		ExitCode: 0,
	})
}

// resetAuth writes a fresh token into a running agent's container and signals
// sciontool init (PID 1) to restart its token refresh loop via SIGUSR2.
func (s *Server) resetAuth(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	var req ResetAuthRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Token == "" {
		ValidationError(w, "token is required", nil)
		return
	}

	rt := s.resolveRuntimeForAgent(ctx, id, projectID)
	target, err := s.LookupContainerID(ctx, id, projectID)
	if err != nil || target == "" {
		NotFound(w, "Agent")
		return
	}

	// Write the token to the canonical file atomically via temp+rename.
	// Write the token to the canonical file atomically via temp+rename.
	// Pass the token as part of the script using a heredoc pattern to avoid
	// exposing it in argv (visible in /proc).
	writeCmd := []string{"sh", "-c",
		"TOKEN_DIR=\"$(getent passwd scion 2>/dev/null | cut -d: -f6 || echo /home/scion)/.scion\" && " +
			"mkdir -p \"$TOKEN_DIR\" && " +
			"cat <<'SCION_TOKEN_EOF' > \"$TOKEN_DIR/scion-token.tmp\"\n" + req.Token + "\nSCION_TOKEN_EOF\n" +
			"mv \"$TOKEN_DIR/scion-token.tmp\" \"$TOKEN_DIR/scion-token\"",
	}

	if _, err := rt.Exec(ctx, target, writeCmd); err != nil {
		s.agentLifecycleLog.Error("reset-auth: failed to write token file", "agent_id", id, "error", err)
		RuntimeError(w, "Failed to write token file: "+err.Error())
		return
	}

	// Signal sciontool init (PID 1) to re-read the token and restart its refresh
	// loop immediately. The token was already written above, and the agent also
	// polls the token file as a UID-safe fallback, so it recovers within a few
	// seconds even if this signal fails. In rootless containers the broker execs
	// as the scion user and `kill -USR2 1` against the root-owned PID 1 fails
	// with EPERM — this is expected and not an error since the token is on disk.
	signalCmd := []string{"kill", "-USR2", "1"}
	signaled := true
	if _, err := rt.Exec(ctx, target, signalCmd); err != nil {
		signaled = false
		s.agentLifecycleLog.Warn("reset-auth: failed to signal PID 1 (token still written, poller will reload)", "agent_id", id, "error", err)
	}

	s.agentLifecycleLog.Info("Auth reset completed", "agent_id", id, "signaled", signaled)

	s.forceHeartbeatAll("reset-auth", id)

	msg := "Auth reset: token written and init signaled"
	if !signaled {
		msg = "Auth reset: token written; signal failed (poller will reload)"
	}
	writeJSON(w, http.StatusOK, ResetAuthResponse{
		Message: msg,
	})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Resolve the correct manager for this agent (may be on an auxiliary runtime like K8s)
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// Try to read agent.log from the filesystem first (preferred source).
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var found *api.AgentInfo
	for i := range agents {
		if matchesAgent(agents[i], id, projectID) {
			found = &agents[i]
			break
		}
	}

	if found == nil {
		NotFound(w, "Agent")
		return
	}

	if found.ProjectPath != "" {
		agentSlug := found.Slug
		if agentSlug == "" {
			agentSlug = found.Name
		}
		agentLogPath := filepath.Join(config.GetAgentHomePath(
			found.ProjectPath, agentSlug,
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
	rt := s.resolveRuntimeForAgent(ctx, id, projectID)
	containerID := found.ContainerID
	if containerID == "" {
		containerID = id
	}
	logs, err := rt.GetLogs(ctx, containerID)
	if err != nil {
		RuntimeError(w, "Failed to get logs: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logs))
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request, id, projectID string) {
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

func (s *Server) checkAgentPrompt(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Find the agent to get its project path
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var agent *api.AgentInfo
	for i := range agents {
		if matchesAgent(agents[i], id, projectID) {
			agent = &agents[i]
			break
		}
	}

	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	if agent.ProjectPath == "" {
		// No project path means we can't check prompt.md
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	// Check if prompt.md exists and has content. The mode (worktree vs
	// shared-workspace) isn't carried on the request, so probe both
	// locations via ResolveAgentDir.
	projectDir, _ := config.GetResolvedProjectDir(agent.ProjectPath)
	if projectDir == "" {
		projectDir = agent.ProjectPath
	}
	promptPath := filepath.Join(config.ResolveAgentDir(projectDir, agent.Name), "prompt.md")
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
//
// hydratedHarnessConfigPath, when non-empty, points to a hub-hydrated harness-
// config directory that supplements the on-disk search. This allows env-gather
// to see auth metadata from hub-managed harness-configs that haven't been
// downloaded to the standard on-disk locations yet.
func (s *Server) extractRequiredEnvKeys(req CreateAgentRequest, hydratedHarnessConfigPath ...string) ([]string, map[string]api.SecretKeyInfo) {
	required := make(map[string]struct{})

	var settings *config.VersionedSettings
	settingsPath := req.ProjectPath
	if settingsPath == "" {
		// Fall back to the broker's global .scion directory for settings
		// resolution. This matches what agent.Start → GetResolvedProjectDir("")
		// does when projectPath is empty (e.g., hub-only git projects without a
		// linked local path on the broker).
		if globalDir, err := config.GetGlobalDir(); err == nil {
			settingsPath = globalDir
			if s.config.Debug {
				s.envSecretLog.Debug("extractRequiredEnvKeys: projectPath empty, using global dir",
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
			"projectPath", req.ProjectPath,
		)
	}
	if harnessConfigName != "" {
		var harnessType, authType string
		// authMeta is the declarative auth block from the resolved harness-
		// config (Phase 1). When non-nil, the Phase-3 config-driven preflight
		// path is used; otherwise the broker falls back to the legacy compiled
		// per-harness tables in pkg/harness/auth.go.
		var authMeta *config.HarnessAuthMetadata

		// Try on-disk harness-config directory first (check projectPath,
		// then fall back to global dir for hub-dispatched agents without a local project)
		harnessConfigSearchPath := req.ProjectPath
		if harnessConfigSearchPath == "" {
			harnessConfigSearchPath = settingsPath
		}
		if harnessConfigSearchPath != "" {
			if hcDir, err := config.FindHarnessConfigDir(harnessConfigName, harnessConfigSearchPath); err == nil {
				harnessType = hcDir.Config.Harness
				authType = hcDir.Config.AuthSelectedType
				if hcDir.Config.Auth != nil {
					authMeta = hcDir.Config.Auth
				}
			}
		}

		// Fall back to hydrated hub-managed harness-config when on-disk
		// search didn't find the config (or didn't populate auth metadata).
		if harnessType == "" && len(hydratedHarnessConfigPath) > 0 && hydratedHarnessConfigPath[0] != "" {
			if hcDir, err := config.LoadHarnessConfigDir(hydratedHarnessConfigPath[0]); err == nil && hcDir != nil {
				harnessType = hcDir.Config.Harness
				authType = hcDir.Config.AuthSelectedType
				if hcDir.Config.Auth != nil {
					authMeta = hcDir.Config.Auth
				}
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
				if authMeta == nil && hcfg.Auth != nil {
					authMeta = hcfg.Auth
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
		if req.Config != nil && req.Config.Template != "" && req.ProjectPath != "" {
			if tmpl, err := config.FindTemplateInProjectPath(req.Config.Template, req.ProjectPath); err == nil {
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
		//
		// Phase 3: when the harness-config carries declarative auth metadata
		// (authMeta != nil), the *FromConfig variants drive detection. Older
		// configs without the `auth:` block still hit the compiled fallbacks.
		useConfigDriven := harness.AuthMetadataAvailable(&config.HarnessConfigEntry{Auth: authMeta})
		if authType == "" {
			fileSecretNames := make(map[string]struct{})
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "file" {
					fileSecretNames[sec.Name] = struct{}{}
				}
			}
			var detected string
			if useConfigDriven {
				detected = harness.DetectAuthTypeFromFileSecretsFromConfig(authMeta, fileSecretNames)
			} else {
				detected = harness.DetectAuthTypeFromFileSecrets(harnessType, fileSecretNames)
			}
			if detected != "" {
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
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "environment" || sec.Type == "" {
					target := sec.Target
					if target == "" {
						target = sec.Name
					}
					if target != "" {
						resolvedEnvKeys[target] = struct{}{}
					}
				}
			}
			var detected string
			if useConfigDriven {
				detected = harness.DetectAuthTypeFromEnvVarsFromConfig(authMeta, resolvedEnvKeys)
			} else {
				detected = harness.DetectAuthTypeFromEnvVars(harnessType, resolvedEnvKeys)
			}
			if detected != "" {
				authType = detected
			}
		}
		if authType == "" {
			var detected string
			if useConfigDriven {
				detected = harness.DetectAuthTypeFromGCPIdentityFromConfig(authMeta, gcpSAAssigned)
			} else {
				detected = harness.DetectAuthTypeFromGCPIdentity(harnessType, gcpSAAssigned)
			}
			if detected != "" {
				authType = detected
			}
		}

		// Resolve auth key groups and check satisfaction
		var keyGroups [][]string
		if useConfigDriven {
			keyGroups = harness.RequiredAuthEnvKeysFromConfig(authMeta, authType)
		} else {
			keyGroups = harness.RequiredAuthEnvKeys(harnessType, authType)
		}
		if len(keyGroups) > 0 {
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
		var authSecrets []api.RequiredSecret
		if useConfigDriven {
			authSecrets = harness.RequiredAuthSecretsFromConfig(authMeta, authType, gcpSAAssigned)
		} else {
			authSecrets = harness.RequiredAuthSecrets(harnessType, authType, gcpSAAssigned)
		}
		if len(authSecrets) > 0 {
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
	if req.Config != nil && req.Config.Template != "" && req.ProjectPath != "" {
		if tmpl, err := config.FindTemplateInProjectPath(req.Config.Template, req.ProjectPath); err == nil {
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
		if req.ProjectPath != "" {
			if _, err := config.FindHarnessConfigDir(tpl, req.ProjectPath); err == nil {
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
		ProjectPath:     origReq.ProjectPath,
		ProjectSlug:     origReq.ProjectSlug,
		ProjectID:       origReq.ProjectID,
		Config:          origReq.Config,
		InlineConfig:    origReq.InlineConfig,
		SharedDirs:      origReq.SharedDirs,
		HubEndpoint:     origReq.HubEndpoint,
		AgentToken:      origReq.AgentToken,
		CreatorName:     origReq.CreatorName,
		ResolvedEnv:     pending.MergedEnv,
		ResolvedSecrets: origReq.ResolvedSecrets,
		NoAuth:          origReq.NoAuth,
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
			"projectPath", opts.ProjectPath,
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
		"agent_id", origReq.ID, "project_id", origReq.ProjectID,
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
// projectID scopes the lookup to a specific project to prevent cross-project collision.
func (s *Server) resolveManagerForAgent(ctx context.Context, id, projectID string) agent.Manager {
	slug := strings.ToLower(id)
	filter := map[string]string{"scion.name": slug}
	if projectID != "" {
		filter["scion.project_id"] = projectID
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

	// If project-scoped lookup found nothing, retry without project filter.
	// This handles backward compatibility with containers that lack the
	// scion.grove_id label (pre-existing agents or solo/CLI mode).
	if projectID != "" {
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
// projectID scopes the lookup to a specific project to prevent cross-project collision.
func (s *Server) resolveRuntimeForAgent(ctx context.Context, id, projectID string) scionrt.Runtime {
	slug := strings.ToLower(id)
	filter := map[string]string{"scion.name": slug}
	if projectID != "" {
		filter["scion.project_id"] = projectID
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

	// Backward compatibility: retry without project filter for pre-existing containers
	if projectID != "" {
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
// start options. It loads the project's settings to determine the effective
// runtime. If the resolved runtime differs from the broker's default, a
// temporary manager is created and cached. Otherwise the broker's shared
// manager is returned.
//
// When opts.Profile is empty, the project's active profile (from settings.yaml)
// is used. This ensures the broker respects the project's configured runtime
// even when no explicit --profile flag is passed.
func (s *Server) resolveManagerForOpts(opts api.StartOptions) agent.Manager {
	if s.config.ForceRuntime != "" {
		if s.config.ForceRuntime == s.runtime.Name() {
			return s.manager
		}
		s.auxiliaryRuntimesMu.RLock()
		aux, ok := s.auxiliaryRuntimes[s.config.ForceRuntime]
		s.auxiliaryRuntimesMu.RUnlock()
		if ok {
			return aux.Manager
		}
		s.agentLifecycleLog.Warn("ForceRuntime does not match default runtime, falling back to settings resolution", "force", s.config.ForceRuntime, "default", s.runtime.Name())
	}

	// Load settings to check if the profile/active-profile specifies a
	// different runtime than the broker's auto-detected default.
	projectDir, _ := config.GetResolvedProjectDir(opts.ProjectPath)
	vs, _, _ := config.LoadEffectiveSettings(projectDir)
	if vs == nil {
		return s.manager
	}

	// ResolveRuntime("") uses vs.ActiveProfile as the fallback.
	_, runtimeType, err := vs.ResolveRuntime(opts.Profile)
	if err != nil {
		// Profile or its runtime not found in settings; use default
		return s.manager
	}

	if runtimeType == s.runtime.Name() {
		return s.manager
	}

	// Settings specify a different runtime - resolve and create a manager.
	// Cache it as an auxiliary manager so LookupContainerID can find agents
	// created on non-default runtimes (e.g. K8s pods when default is docker).
	//
	// Use opts.Profile for ResolveRuntime so it picks up the same profile
	// that was just checked. When empty, GetRuntime falls back to settings
	// the same way ResolveRuntime does.
	resolved := agent.ResolveRuntime(opts.ProjectPath, opts.Name, opts.Profile)

	if s.config.Debug {
		s.agentLifecycleLog.Debug("Settings resolved to different runtime",
			"agent", opts.Name, "profile", opts.Profile,
			"activeProfile", vs.ActiveProfile,
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

// resolveProjectSettingsDir returns the directory containing settings.yaml for a project.
// For linked projects, projectPath already points to the .scion directory.
// For hub-managed projects, projectPath is the workspace parent, so settings
// live in the .scion subdirectory.
func resolveProjectSettingsDir(projectPath string) string {
	if config.GetSettingsPath(projectPath) != "" {
		return projectPath
	}
	candidate := filepath.Join(projectPath, ".scion")
	if config.GetSettingsPath(candidate) != "" {
		return candidate
	}
	return projectPath // fallback to original
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
// Project Endpoints
// ============================================================================

// handleProjectBySlug routes requests to /api/v1/projects/{slug}.
func (s *Server) handleProjectBySlug(w http.ResponseWriter, r *http.Request) {
	slug := extractID(r, "/api/v1/projects")
	if slug == "" {
		NotFound(w, "project")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.deleteProject(w, r, slug)
	default:
		MethodNotAllowed(w)
	}
}

// deleteProject removes the local hub-managed project directory for the given slug.
// Returns 204 on success (including when the directory doesn't exist).
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, slug string) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		RuntimeError(w, "Failed to get global dir: "+err.Error())
		return
	}

	projectPath := filepath.Join(globalDir, "projects", slug)
	if !hasWorkspaceContent(projectPath) {
		// fallback to groves/ for backward compatibility
		oldPath := filepath.Join(globalDir, "groves", slug)
		if hasWorkspaceContent(oldPath) {
			projectPath = oldPath
		}
	}

	// Path traversal protection: ensure the resolved path stays inside
	// one of the two allowed base directories.
	projectsBase := filepath.Join(globalDir, "projects")
	legacyBase := filepath.Join(globalDir, "groves")
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		RuntimeError(w, "Failed to resolve project path: "+err.Error())
		return
	}
	absProjectsBase, err := filepath.Abs(projectsBase)
	if err != nil {
		RuntimeError(w, "Failed to resolve base path: "+err.Error())
		return
	}
	absLegacyBase, err := filepath.Abs(legacyBase)
	if err != nil {
		RuntimeError(w, "Failed to resolve base path: "+err.Error())
		return
	}
	if !strings.HasPrefix(absProject, absProjectsBase+string(filepath.Separator)) &&
		!strings.HasPrefix(absProject, absLegacyBase+string(filepath.Separator)) {
		s.agentLifecycleLog.Warn("project cleanup path traversal blocked", "slug", slug, "resolved", absProject)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		// Already gone — idempotent success
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := os.RemoveAll(projectPath); err != nil {
		s.agentLifecycleLog.Warn("failed to remove project directory", "slug", slug, "path", projectPath, "error", err)
		RuntimeError(w, "Failed to remove project directory: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Removed hub-managed project directory", "slug", slug, "path", projectPath)
	w.WriteHeader(http.StatusNoContent)
}

// findAgentInHubManagedProjects scans hub-managed project directories
// (~/.scion.projects/<slug>/.scion/) for an agent directory matching the given
// name. Returns the .scion dir path if found, or empty string.
// This is used as a fallback when the container is missing and the agent's
// project path can't be determined from container labels.
//
// Probes both the in-project location (worktree-mode agents) and the external
// per-agent state dir under ~/.scion.project-configs/ (shared-workspace agents,
// whose state lives external to the shared checkout).
func findAgentInHubManagedProjects(agentName string) string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	for _, dirName := range []string{"projects", "groves"} {
		baseDir := filepath.Join(globalDir, dirName)
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			scionDir := filepath.Join(baseDir, entry.Name(), ".scion")
			agentDir := filepath.Join(scionDir, "agents", agentName)
			if _, err := os.Stat(agentDir); err == nil {
				return scionDir
			}
			// Shared-workspace agents have no in-project agentDir — probe the
			// external split-storage path.
			if extDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extDir != "" {
				if _, err := os.Stat(filepath.Join(extDir, agentName)); err == nil {
					return scionDir
				}
			}
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

// ensureNFSMountsReady verifies that all configured NFS shares are mounted
// before dispatching an agent. This is a pre-flight check (N1-7):
// the reconciler may have mounted them at startup, but a transient
// unmount (network blip, manual intervention) should block dispatches.
// Returns an error if any configured share cannot be mounted — the caller
// should reject the dispatch to avoid silent fallback to a broken mount.
func (s *Server) ensureNFSMountsReady() error {
	if s.nfsMountReconciler == nil {
		return nil // NFS not configured — local backend, nothing to check.
	}

	nfsCfg := s.config.NFSConfig
	if nfsCfg == nil || len(nfsCfg.Shares) == 0 {
		return nil
	}

	for _, share := range nfsCfg.Shares {
		if err := s.nfsMountReconciler.EnsureShareMounted(share.ID); err != nil {
			return err
		}
	}
	return nil
}
