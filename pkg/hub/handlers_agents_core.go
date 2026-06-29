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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	gouuid "github.com/google/uuid"
)

// parseLabelFilters parses label=key=value query parameters into a map and
// validates the resulting labels against constraint rules.
func parseLabelFilters(params []string) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(params))
	for _, lp := range params {
		k, v, ok := strings.Cut(lp, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label filter %q: must be key=value", lp)
		}
		m[k] = v
	}
	if err := labels.Validate(m); err != nil {
		return nil, fmt.Errorf("invalid label filter: %w", err)
	}
	return m, nil
}

type ListAgentsResponse struct {
	Agents       []AgentWithCapabilities `json:"agents"`
	NextCursor   string                  `json:"nextCursor,omitempty"`
	TotalCount   int                     `json:"totalCount"`
	ServerTime   time.Time               `json:"serverTime"`
	Capabilities *Capabilities           `json:"_capabilities,omitempty"`
}

type CreateAgentRequest struct {
	Name            string            `json:"name"`
	ProjectID       string            `json:"projectId"`
	RuntimeBrokerID string            `json:"runtimeBrokerId,omitempty"` // Optional: uses project's default if not specified
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
	// Resume signals that the caller wants to resume an existing stopped agent
	// rather than create a brand-new one. When true and a stopped agent with
	// the same name exists, the Hub recovers it instead of creating fresh.
	Resume bool `json:"resume,omitempty"`
	// NoAuth indicates the agent should start with zero injected credentials.
	// When true, the Hub skips secret resolution and the broker skips credential injection.
	NoAuth bool `json:"noAuth,omitempty"`
	// GCPIdentity specifies the GCP identity assignment for the agent.
	// Controls metadata server behavior and optional service account binding.
	GCPIdentity *GCPIdentityAssignment `json:"gcp_identity,omitempty"`
}

// GCPIdentityAssignment specifies GCP identity configuration for agent creation.
type GCPIdentityAssignment struct {
	MetadataMode     string `json:"metadata_mode"`                // "block", "passthrough", "assign"
	ServiceAccountID string `json:"service_account_id,omitempty"` // Required when mode is "assign"
}

type CreateAgentResponse struct {
	Agent    *store.Agent `json:"agent"`
	Warnings []string     `json:"warnings,omitempty"`
	// UploadURLs is populated during workspace bootstrap (non-git projects).
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

	projectID := query.Get("projectId")
	if projectID != "" && gouuid.Validate(projectID) != nil {
		if project, err := s.store.GetProjectBySlug(ctx, projectID); err == nil && project != nil {
			projectID = project.ID
		}
	}

	filter := store.AgentFilter{
		ProjectID:       projectID,
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
		Phase:           query.Get("phase"),
		IncludeDeleted:  query.Get("includeDeleted") == "true",
	}

	if labelParams := query["label"]; len(labelParams) > 0 {
		parsed, err := parseLabelFilters(labelParams)
		if err != nil {
			BadRequest(w, err.Error())
			return
		}
		filter.Labels = parsed
	}

	// scope=mine: agents the current user created
	// scope=shared: agents in projects the user is a member of, but not created by them
	// mine=true (legacy): agents the user created or in projects they own/are a member of
	switch query.Get("scope") {
	case "mine":
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			filter.OwnerID = userIdent.ID()
		}
	case "shared":
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			if projectIDs := s.resolveUserProjectIDs(ctx, userIdent.ID()); len(projectIDs) > 0 {
				filter.MemberProjectIDs = projectIDs
				filter.ExcludeOwnerID = userIdent.ID()
			} else {
				filter.MemberProjectIDs = []string{"__none__"}
			}
		}
	default:
		if query.Get("mine") == "true" {
			if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
				filter.OwnerID = userIdent.ID()
				if projectIDs := s.resolveUserProjectIDs(ctx, userIdent.ID()); len(projectIDs) > 0 {
					filter.MemberOrOwnerProjectIDs = projectIDs
				}
			}
		}
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

	// Enrich agents with project and broker names
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
	if req.ProjectID == "" {
		ValidationError(w, "projectId is required", nil)
		return
	}
	if req.CleanupMode != "" && req.CleanupMode != "strict" && req.CleanupMode != "force" {
		ValidationError(w, "cleanupMode must be 'strict' or 'force'", nil)
		return
	}

	// Validate GCP identity assignment structure (field-level; SA resolution happens in createAgentInProject)
	if req.GCPIdentity != nil {
		switch req.GCPIdentity.MetadataMode {
		case store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough:
			if req.GCPIdentity.ServiceAccountID != "" {
				ValidationError(w, "service_account_id must be empty when metadata_mode is '"+req.GCPIdentity.MetadataMode+"'", nil)
				return
			}
		case store.GCPMetadataModeAssign:
			if req.GCPIdentity.ServiceAccountID == "" {
				ValidationError(w, "service_account_id is required when metadata_mode is 'assign'", nil)
				return
			}
		default:
			ValidationError(w, "metadata_mode must be 'block', 'passthrough', or 'assign'", nil)
			return
		}
	}

	if err := labels.Validate(req.Labels); err != nil {
		ValidationError(w, "Invalid labels: "+err.Error(), nil)
		return
	}

	// Check if the caller is an agent (sub-agent creation)
	var createdBy string
	var creatorName string
	var ancestry []string
	var notifySubscriberType, notifySubscriberID string // For --notify subscription
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		// Agent callers must have the project:agent:create scope
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return
		}
		// Enforce project isolation: agents can only create sub-agents in their own project
		if req.ProjectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only create sub-agents within their own project", nil)
			return
		}
		createdBy = agentIdent.ID()
		// Resolve human-readable creator name and ancestry from the calling agent
		if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
			creatorName = creatorAgent.Name
			notifySubscriberType = store.SubscriberTypeAgent
			notifySubscriberID = creatorAgent.Slug
			// Build ancestry: creator's ancestry + creator's ID
			ancestry = append(ancestry, creatorAgent.Ancestry...)
			ancestry = append(ancestry, creatorAgent.ID)
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
		creatorName = userIdent.Email()
		notifySubscriberType = store.SubscriberTypeUser
		notifySubscriberID = userIdent.ID()
		// User-created agents: ancestry is [userID]
		ancestry = []string{userIdent.ID()}
		// Enforce policy-based authorization: user must have permission to create agents in this project
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "agent",
			ParentType: "project",
			ParentID:   req.ProjectID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to create agents in this project", nil)
			return
		}
	}

	s.createAgentInProject(w, r, req, req.ProjectID, createdBy, creatorName, ancestry, notifySubscriberType, notifySubscriberID)
}

func (s *Server) createAgentInProject(
	w http.ResponseWriter,
	r *http.Request,
	req CreateAgentRequest,
	projectID string,
	createdBy string,
	creatorName string,
	ancestry []string,
	notifySubscriberType string,
	notifySubscriberID string,
) {
	ctx := r.Context()
	hubCreateStart := time.Now()

	// Verify project exists and get its configuration
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve the runtime broker
	runtimeBrokerID, err := s.resolveRuntimeBroker(ctx, w, req.RuntimeBrokerID, project)
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

	// Validate GCP passthrough mode: only the broker owner (or admin) may use passthrough,
	// because it exposes the broker's own GCP identity to the agent container.
	if req.GCPIdentity != nil && req.GCPIdentity.MetadataMode == store.GCPMetadataModePassthrough && runtimeBrokerID != "" {
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			broker, err := s.store.GetRuntimeBroker(ctx, runtimeBrokerID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if userIdent.Role() != "admin" && broker.CreatedBy != userIdent.ID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"GCP identity passthrough requires broker ownership. Only the broker owner can expose the broker's GCP identity to agents.", nil)
				return
			}
		}
	}

	// Validate GCP identity SA assignment: verify the SA exists, belongs to this project, and is verified.
	var resolvedGCPSA *store.GCPServiceAccount
	if req.GCPIdentity != nil && req.GCPIdentity.MetadataMode == store.GCPMetadataModeAssign {
		sa, err := s.store.GetGCPServiceAccount(ctx, req.GCPIdentity.ServiceAccountID)
		if err != nil {
			if err == store.ErrNotFound {
				ValidationError(w, "GCP service account not found", nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if sa.ScopeID != projectID {
			ValidationError(w, "GCP service account does not belong to this project", nil)
			return
		}
		if !sa.Verified {
			ValidationError(w, "GCP service account is not verified; verify it before assigning to agents", nil)
			return
		}

		// Authorization: any project member who can see the SA can assign it.
		// SA management (create/mint/delete) is gated on ActionManage elsewhere.
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent, gcpServiceAccountResource(sa), ActionRead)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"You don't have permission to assign GCP service accounts in this project", nil)
				return
			}
		}

		resolvedGCPSA = sa
	}

	// Check if the agent already exists (e.g. created via "scion create" for later start).
	// If it exists in "created" status, start it instead of creating a duplicate.
	// If it doesn't exist, fall through to create it.
	slug, err := api.ValidateAgentName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error(), nil)
		return
	}
	existingAgent, err := s.store.GetAgentBySlug(ctx, projectID, slug)
	if err != nil && err != store.ErrNotFound {
		writeErrorFromErr(w, err, "")
		return
	}

	switch s.handleExistingAgent(ctx, w, existingAgent, project, runtimeBrokerID, req, notifySubscriberType, notifySubscriberID, createdBy) {
	case existingAgentStarted, existingAgentErrored:
		return // Response already written.
	case existingAgentConflict:
		Conflict(w, fmt.Sprintf("agent %q already exists in this project", slug))
		return
	case existingAgentDeleted:
		// Fall through to create a new agent below.
	case existingAgentNone:
		// No existing agent — fall through to create.
	}

	// Apply project-level default template if no template specified in request
	if req.Template == "" && project != nil && project.Annotations != nil {
		if dt := project.Annotations[projectSettingDefaultTemplate]; dt != "" {
			req.Template = dt
		}
	}

	// Resolve template if specified - the client may pass either a template ID or name
	var resolvedTemplate *store.Template
	if req.Template != "" {
		resolvedTemplate, err = s.resolveTemplate(ctx, req.Template, projectID)
		if err != nil && err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
		// If template was requested but not found, check if the broker has local access
		if resolvedTemplate == nil {
			brokerHasLocal := false
			if runtimeBrokerID != "" {
				provider, err := s.store.GetProjectProvider(ctx, projectID, runtimeBrokerID)
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

	// Resolve harness config: prefer the user's explicit choice, then template default.
	// Do NOT use req.Template as fallback since it may contain a UUID.
	harnessConfig := req.HarnessConfig
	if harnessConfig == "" {
		harnessConfig = s.getHarnessConfigFromTemplate(resolvedTemplate, "")
	}

	agent := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            slug,
		Name:            slug,
		Template:        req.Template,
		ProjectID:       projectID,
		RuntimeBrokerID: runtimeBrokerID,
		Phase:           string(state.PhaseCreated),
		Labels:          req.Labels,
		Visibility:      store.VisibilityPrivate,
		CreatedBy:       createdBy,
		OwnerID:         createdBy,
		Ancestry:        ancestry,
	}

	// Store human-friendly slug instead of UUID for display
	if resolvedTemplate != nil && resolvedTemplate.Slug != "" {
		agent.Template = resolvedTemplate.Slug
	}

	agent.AppliedConfig = s.buildAppliedConfig(req, harnessConfig, creatorName)

	// Populate GCP identity in applied config.
	// Default to "block" mode when no GCP identity is specified, so agents
	// cannot access the underlying compute identity via the GCE metadata
	// server unless explicitly opted into "passthrough" or "assign".
	if req.GCPIdentity != nil {
		switch req.GCPIdentity.MetadataMode {
		case store.GCPMetadataModeAssign:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    resolvedGCPSA.ID,
				ServiceAccountEmail: resolvedGCPSA.Email,
				ProjectID:           resolvedGCPSA.ProjectID,
			}
		case store.GCPMetadataModePassthrough:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModePassthrough,
			}
		case store.GCPMetadataModeBlock:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		}
	} else {
		// No explicit GCP identity — check project default, then fall back to block.
		projectSettings := projectSettingsFromAnnotations(project)
		switch projectSettings.DefaultGCPIdentityMode {
		case store.GCPMetadataModePassthrough:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModePassthrough,
			}
		case store.GCPMetadataModeAssign:
			if projectSettings.DefaultGCPIdentityServiceAccountID != "" {
				sa, err := s.store.GetGCPServiceAccount(ctx, projectSettings.DefaultGCPIdentityServiceAccountID)
				if err == nil && sa.ScopeID == projectID && sa.Verified {
					agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
						MetadataMode:        store.GCPMetadataModeAssign,
						ServiceAccountID:    sa.ID,
						ServiceAccountEmail: sa.Email,
						ProjectID:           sa.ProjectID,
					}
				} else {
					// SA not found/invalid — fall back to block
					agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
						MetadataMode: store.GCPMetadataModeBlock,
					}
				}
			} else {
				agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
					MetadataMode: store.GCPMetadataModeBlock,
				}
			}
		default:
			// No project default or explicit "block" — secure default
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		}
	}

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

	// Apply project-level defaults (harness config, limits, resources) from annotations
	applyProjectDefaults(agent.AppliedConfig, project)

	s.populateAgentConfig(ctx, agent, project, resolvedTemplate)

	if err := s.store.CreateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Create notification subscription if requested
	if req.Notify {
		s.createNotifySubscription(ctx, agent.ID, projectID, notifySubscriberType, notifySubscriberID, createdBy)
	}

	// Workspace bootstrap mode: if WorkspaceFiles are provided with a task,
	// generate signed upload URLs instead of dispatching immediately.
	// The CLI will upload files, then call finalize to trigger dispatch.
	//
	// Exception: if the target broker has a LocalPath for this project, the broker
	// can access the workspace directly from the filesystem — skip the upload
	// and fall through to the normal dispatch path.
	if len(req.WorkspaceFiles) > 0 && req.Task != "" {
		// Check if the target broker has local filesystem access to this project
		hasLocalPath := false
		if runtimeBrokerID != "" {
			provider, err := s.store.GetProjectProvider(ctx, projectID, runtimeBrokerID)
			if err == nil && provider.LocalPath != "" {
				hasLocalPath = true
				s.agentLifecycleLog.Debug("Workspace bootstrap: broker has local path, skipping upload",
					"agent_id", agent.ID,
					"broker", runtimeBrokerID, "localPath", provider.LocalPath)
			}
		}

		if !hasLocalPath && !s.isEmbeddedBroker(runtimeBrokerID) {
			stor := s.GetStorage()
			if stor == nil {
				RuntimeError(w, "Storage not configured for workspace bootstrap")
				return
			}

			storagePath := storage.WorkspaceStoragePath(agent.ProjectID, agent.ID)
			uploadURLs, existingFiles, err := generateWorkspaceUploadURLs(ctx, stor, storagePath, req.WorkspaceFiles)
			if err != nil {
				RuntimeError(w, "Failed to generate upload URLs: "+err.Error())
				return
			}

			// Set agent to provisioning phase (not dispatched yet)
			agent.Phase = string(state.PhaseProvisioning)
			if err := s.store.UpdateAgent(ctx, agent); err != nil {
				s.agentLifecycleLog.Warn("Failed to update agent status to provisioning", "agent_id", agent.ID, "error", err)
			}

			s.events.PublishAgentCreated(ctx, agent)

			expires := time.Now().Add(SignedURLExpiry)
			s.enrichAgent(ctx, agent, project, nil)

			var warnings []string
			if len(existingFiles) > 0 {
				s.agentLifecycleLog.Debug("Workspace bootstrap: files already in storage", "agent_id", agent.ID, "count", len(existingFiles))
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

	// Hub-native/shared-workspace project remote broker support: if the project has
	// a managed workspace and the workspace path is set, upload it to GCS so
	// a remote broker can download it.
	if (project.GitRemote == "" || project.IsSharedWorkspace()) && agent.AppliedConfig != nil && agent.AppliedConfig.Workspace != "" {
		hasLocalPath := false
		if runtimeBrokerID != "" {
			provider, err := s.store.GetProjectProvider(ctx, project.ID, runtimeBrokerID)
			if err == nil && provider.LocalPath != "" {
				hasLocalPath = true
			}
		}

		if !hasLocalPath && !s.isEmbeddedBroker(runtimeBrokerID) {
			stor := s.GetStorage()
			if stor != nil {
				storagePath := storage.ProjectWorkspaceStoragePath(project.ID)
				if err := gcp.SyncToGCS(ctx, agent.AppliedConfig.Workspace, stor.Bucket(), storagePath+"/files"); err != nil {
					s.agentLifecycleLog.Warn("Failed to upload hub-managed project workspace to GCS",
						"agent_id", agent.ID,
						"project_id", project.ID, "error", err)
				} else {
					// Swap workspace to storage path for remote broker
					agent.AppliedConfig.Workspace = ""
					agent.AppliedConfig.WorkspaceStoragePath = storagePath
					if err := s.store.UpdateAgent(ctx, agent); err != nil {
						s.agentLifecycleLog.Warn("Failed to update agent with workspace storage path", "agent_id", agent.ID, "error", err)
					}
				}
			}
		}
	}

	// Dispatch to runtime broker if available.
	// Unless provision-only is requested, do a full create+start via DispatchAgentCreate.
	// Otherwise provision only — set up dirs, worktree, templates without launching the container.
	s.agentLifecycleLog.Info("Hub: pre-dispatch setup complete",
		"agent_id", agent.ID, "agent", agent.Name, "elapsed", time.Since(hubCreateStart).String())
	var warnings []string
	if dispatcher := s.GetDispatcher(); dispatcher != nil {
		if !req.ProvisionOnly {
			// Use env-gather dispatch if requested
			if req.GatherEnv {
				s.agentLifecycleLog.Debug("Hub: env-gather requested, using DispatchAgentCreateWithGather",
					"agent_id", agent.ID,
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
					if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
						s.agentLifecycleLog.Warn("Failed to update agent phase for env-gather", "agent_id", agent.ID, "error", err)
					}

					s.events.PublishAgentCreated(ctx, agent)

					s.enrichAgent(ctx, agent, project, nil)
					hubEnvGather := s.buildEnvGatherResponse(ctx, agent, envReqs)

					writeJSON(w, http.StatusAccepted, CreateAgentResponse{
						Agent:     agent,
						Warnings:  warnings,
						EnvGather: hubEnvGather,
					})
					return
				} else {
					s.preserveTerminalPhase(ctx, agent)
					if agent.Phase == string(state.PhaseCreated) {
						agent.Phase = string(state.PhaseProvisioning)
					}
					if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
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
					s.preserveTerminalPhase(ctx, agent)
					if agent.Phase == string(state.PhaseCreated) {
						agent.Phase = string(state.PhaseProvisioning)
					}
					if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
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
				if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
					warnings = append(warnings, "Failed to update agent phase: "+err.Error())
				}
			}
		}
	}

	s.agentLifecycleLog.Info("Hub: dispatch complete",
		"agent_id", agent.ID, "agent", agent.Name, "totalElapsed", time.Since(hubCreateStart).String())

	// Re-read the agent from the database before publishing the "created" event.
	// A concurrent status update (e.g. sciontool reporting a clone error) may have
	// changed the phase between our last UpdateAgent and now. Publishing the stale
	// in-memory object would send a "created" SSE event with the wrong phase,
	// and since the frontend may have already dropped the earlier "status" event
	// (it ignores status events for agents not yet in state), the UI would never
	// reflect the error.
	if latest, err := s.store.GetAgent(ctx, agent.ID); err == nil {
		s.events.PublishAgentCreated(ctx, latest)
	} else {
		s.events.PublishAgentCreated(ctx, agent)
	}

	// Enrich agent with project and broker names for display
	s.enrichAgent(ctx, agent, project, nil)

	writeJSON(w, http.StatusCreated, CreateAgentResponse{
		Agent:    agent,
		Warnings: warnings,
	})
}

// preserveTerminalPhase re-reads the agent from the database and, if a
// concurrent status update has moved the agent to a terminal phase (error or
// stopped), preserves that phase on the in-memory agent so the subsequent
// UpdateAgent call does not overwrite it with the broker-reported phase.
// This prevents a race where sciontool reports an error (e.g. git clone
// failure) while the broker dispatch is still in flight.
func (s *Server) preserveTerminalPhase(ctx context.Context, agent *store.Agent) {
	current, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil {
		return
	}
	p := state.Phase(current.Phase)
	if p == state.PhaseError || p == state.PhaseStopped {
		agent.Phase = current.Phase
		agent.Activity = current.Activity
		agent.Message = current.Message
		agent.StateVersion = current.StateVersion
	}
}

func (s *Server) updateAgentAfterDispatch(ctx context.Context, agent *store.Agent) error {
	// One retry is intentional here: we only need to recover the common case
	// where a single concurrent status update bumps StateVersion while dispatch
	// is in flight. If a second write wins the race too, return the conflict to
	// the caller rather than spinning in a longer CAS loop inside the request.
	err := s.store.UpdateAgent(ctx, agent)
	if err == nil || !errors.Is(err, store.ErrVersionConflict) {
		return err
	}

	latest, getErr := s.store.GetAgent(ctx, agent.ID)
	if getErr != nil {
		return getErr
	}

	mergeDispatchedAgent(latest, agent)
	return s.store.UpdateAgent(ctx, latest)
}

func mergeDispatchedAgent(dst, src *store.Agent) {
	if src.Template != "" {
		dst.Template = src.Template
	}
	if src.Image != "" {
		dst.Image = src.Image
	}
	if src.Runtime != "" {
		dst.Runtime = src.Runtime
	}
	if src.AppliedConfig != nil {
		dst.AppliedConfig = src.AppliedConfig
	}
	if src.Message != "" {
		dst.Message = src.Message
	}
	if src.TaskSummary != "" {
		dst.TaskSummary = src.TaskSummary
	}

	if isTerminalAgentPhase(dst.Phase) {
		return
	}
	if src.Phase != "" {
		dst.Phase = src.Phase
	}
	if src.Activity != "" {
		dst.Activity = src.Activity
	}
	if src.ContainerStatus != "" {
		dst.ContainerStatus = src.ContainerStatus
	}
	if src.RuntimeState != "" {
		dst.RuntimeState = src.RuntimeState
	}
}

func isTerminalAgentPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseError:
		return true
	case state.PhaseCreated,
		state.PhaseProvisioning,
		state.PhaseCloning,
		state.PhaseStarting,
		state.PhaseRunning,
		state.PhaseStopping:
		return false
	default:
		return false
	}
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
		if source.Scope == "hub" && agent.ProjectID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID, Key: key})
			if err == nil && len(vars) > 0 {
				source.Scope = "project"
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
			if source.Scope == "hub" && agent.ProjectID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{
					Scope: "project", ScopeID: agent.ProjectID, Name: key,
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
		if agent.ProjectID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID, Key: key})
			if err == nil && len(vars) > 0 {
				resp.HubWarnings = append(resp.HubWarnings,
					fmt.Sprintf("%s is stored in Hub env storage (project scope) but was not included in the dispatch — this may indicate a resolution issue", key))
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
			if agent.ProjectID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{Scope: "project", ScopeID: agent.ProjectID, Name: key})
				if err == nil && len(metas) > 0 {
					resp.HubWarnings = append(resp.HubWarnings,
						fmt.Sprintf("%s is stored in Hub secrets (project scope) but was not included in the dispatch — this may indicate a resolution issue", key))
					continue
				}
			}
		}
	}

	return resp
}

// submitAgentEnv handles POST /api/v1/projects/{projectId}/agents/{agentId}/env
// CLI submits gathered env vars after receiving a 202 env-gather response.
func (s *Server) submitAgentEnv(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
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
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.ProjectID != projectID {
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
	if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
		s.agentLifecycleLog.Warn("Failed to update agent phase after env submit", "agent_id", agent.ID, "error", err)
	}

	// Enrich and return
	project, _ := s.store.GetProject(ctx, projectID)
	s.enrichAgent(ctx, agent, project, nil)

	writeJSON(w, http.StatusOK, CreateAgentResponse{
		Agent: agent,
	})
}

// enrichAgents populates Project and RuntimeBrokerName fields for a slice of agents.
// This provides human-readable names from the related IDs for display purposes.
func (s *Server) enrichAgents(ctx context.Context, agents []store.Agent) {
	if len(agents) == 0 {
		return
	}

	// Collect unique project, broker, and template IDs
	projectIDs := make(map[string]struct{})
	brokerIDs := make(map[string]struct{})
	templateIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.ProjectID != "" {
			projectIDs[a.ProjectID] = struct{}{}
		}
		if a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
		if a.AppliedConfig != nil && a.AppliedConfig.TemplateID != "" {
			templateIDs[a.AppliedConfig.TemplateID] = struct{}{}
		}
	}

	// Fetch projects
	projectNames := make(map[string]string)
	for id := range projectIDs {
		if project, err := s.store.GetProject(ctx, id); err == nil {
			projectNames[id] = project.Name
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
		if name, ok := projectNames[agents[i].ProjectID]; ok {
			agents[i].Project = name
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

// enrichAgent populates Project and RuntimeBrokerName fields for a single agent.
// project and broker parameters are optional pre-fetched values to avoid redundant lookups.
func (s *Server) enrichAgent(ctx context.Context, agent *store.Agent, project *store.Project, broker *store.RuntimeBroker) {
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

	// Populate project name
	if project != nil {
		agent.Project = project.Name
	} else if agent.ProjectID != "" {
		if g, err := s.store.GetProject(ctx, agent.ProjectID); err == nil {
			agent.Project = g.Name
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
			s.agentLifecycleLog.Debug("failed to get runtime broker for enrichment", "agent_id", agent.ID, "brokerID", agent.RuntimeBrokerID, "error", err)
		} else {
			agent.RuntimeBrokerName = b.Name
			s.agentLifecycleLog.Debug("enriched agent with broker name", "agent_id", agent.ID, "slug", agent.Slug, "brokerName", b.Name)
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

	// Handle agent logs relay (GET, proxied to broker)
	if action == "logs" {
		s.handleAgentLogs(w, r, id)
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
	if action == api.AgentActionMessageLogs {
		s.handleAgentMessageLogs(w, r, id)
		return
	}
	if action == api.AgentActionMessageLogsStream {
		s.handleAgentMessageLogsStream(w, r, id)
		return
	}

	// Handle per-agent messages (GET endpoints, handled before the
	// POST-only action gate). Both the list and the real-time stream
	// are backed by the hub message store / event bus and work without
	// Cloud Logging being configured.
	if action == api.AgentActionMessagesStream {
		s.handleAgentMessagesStream(w, r, id)
		return
	}
	if action == api.AgentActionMessages {
		s.handleAgentMessages(w, r, id)
		return
	}

	// Handle agent-scoped secret creation: PUT /api/v1/agents/{id}/secrets/{key}
	if action == "secrets" || strings.HasPrefix(action, "secrets/") {
		s.handleAgentSecrets(w, r, id, strings.TrimPrefix(action, "secrets"))
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

	// If the caller is an agent, enforce project isolation
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
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

	// Enrich agent with project and broker names
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
		Name         string                 `json:"name,omitempty"`
		Labels       map[string]string      `json:"labels,omitempty"`
		Annotations  map[string]string      `json:"annotations,omitempty"`
		TaskSummary  string                 `json:"taskSummary,omitempty"`
		Config       *api.ScionConfig       `json:"config,omitempty"`
		GCPIdentity  *GCPIdentityAssignment `json:"gcp_identity,omitempty"`
		StateVersion int64                  `json:"stateVersion"`
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
		if err := labels.Validate(updates.Labels); err != nil {
			ValidationError(w, "Invalid labels: "+err.Error(), nil)
			return
		}
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
		if agent.Phase != string(state.PhaseCreated) {
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

	// Apply GCP identity update (only allowed for agents in 'created' phase)
	if updates.GCPIdentity != nil {
		if agent.Phase != string(state.PhaseCreated) {
			Conflict(w, "GCP identity can only be updated for agents in 'created' phase")
			return
		}
		if agent.AppliedConfig == nil {
			agent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		switch updates.GCPIdentity.MetadataMode {
		case store.GCPMetadataModeBlock:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		case store.GCPMetadataModePassthrough:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModePassthrough,
			}
		case store.GCPMetadataModeAssign:
			if updates.GCPIdentity.ServiceAccountID == "" {
				ValidationError(w, "service_account_id is required when metadata_mode is 'assign'", nil)
				return
			}
			sa, err := s.store.GetGCPServiceAccount(ctx, updates.GCPIdentity.ServiceAccountID)
			if err != nil {
				writeErrorFromErr(w, err, "GCP service account not found")
				return
			}
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    sa.ID,
				ServiceAccountEmail: sa.Email,
				ProjectID:           sa.ProjectID,
			}
		default:
			ValidationError(w, "invalid metadata_mode: must be 'block', 'passthrough', or 'assign'", nil)
			return
		}
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
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionDelete)
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
					"agent_id", agent.ID, "error", err)
			} else {
				// Normal mode: fail the operation to avoid orphaning the agent on the broker
				s.agentLifecycleLog.Error("Failed to dispatch agent delete to broker", "agent_id", agent.ID, "error", err)
				writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
					"Failed to delete agent on runtime broker: "+err.Error(), nil)
				return
			}
		}
	}

	// Cancel pending scheduled events targeting this agent
	s.cancelScheduledEventsForAgent(ctx, agent)

	if softDelete {
		// Soft delete: mark agent as deleted with timestamp
		agent.Phase = string(state.PhaseStopped)
		agent.DeletedAt = now
		agent.Updated = now
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		s.events.PublishAgentDeleted(ctx, agent.ID, agent.ProjectID)
	} else {
		// Hard delete: publish deletion event BEFORE removing the record so
		// notification subscribers can be resolved while subscriptions still exist.
		s.events.PublishAgentDeleted(ctx, agent.ID, agent.ProjectID)
		if err := s.store.DeleteAgent(ctx, agent.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// cancelScheduledEventsForAgent cancels all pending scheduled events that
// target the given agent, preventing orphaned events from firing after deletion.
func (s *Server) cancelScheduledEventsForAgent(ctx context.Context, agent *store.Agent) {
	result, err := s.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
		ProjectID: agent.ProjectID,
		Status:    store.ScheduledEventPending,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.agentLifecycleLog.Warn("Failed to list scheduled events for cleanup",
			"agent_id", agent.ID, "error", err)
		return
	}

	var cancelled int
	for _, evt := range result.Items {
		if !eventTargetsAgent(evt, agent) {
			continue
		}
		if err := s.store.UpdateScheduledEventStatus(ctx, evt.ID,
			store.ScheduledEventCancelled, nil, "target agent deleted"); err != nil {
			s.agentLifecycleLog.Warn("Failed to cancel scheduled event",
				"event_id", evt.ID, "agent_id", agent.ID, "error", err)
			continue
		}
		if s.scheduler != nil {
			if cancelErr := s.scheduler.CancelEvent(ctx, evt.ID); cancelErr != nil {
				s.agentLifecycleLog.Warn("Failed to cancel in-memory scheduler timer",
					"event_id", evt.ID, "error", cancelErr)
			}
		}
		cancelled++
	}

	if cancelled > 0 {
		s.agentLifecycleLog.Info("Cancelled scheduled events for deleted agent",
			"agent_id", agent.ID, "agent_name", agent.Name, "cancelled", cancelled)
	}
}

// eventTargetsAgent checks whether a scheduled event's payload targets the
// given agent by matching agent ID or name/slug.
func eventTargetsAgent(evt store.ScheduledEvent, agent *store.Agent) bool {
	var payload struct {
		AgentID   string `json:"agentId"`
		AgentName string `json:"agentName"`
	}
	if err := json.Unmarshal([]byte(evt.Payload), &payload); err != nil {
		return false
	}
	if payload.AgentID != "" && payload.AgentID == agent.ID {
		return true
	}
	if payload.AgentName != "" && (payload.AgentName == agent.Name || payload.AgentName == agent.Slug) {
		return true
	}
	return false
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// For actions other than status/token refresh and outbound-message
	// (self-access), we require user or agent authentication
	// with appropriate scopes. Self-access endpoints enforce their own auth checks.
	if action != api.AgentActionStatus &&
		action != api.AgentActionTokenRefresh &&
		action != api.AgentActionRefreshToken &&
		action != api.AgentActionOutboundMessage {
		userIdent := GetUserIdentityFromContext(r.Context())
		agentIdent := GetAgentIdentityFromContext(r.Context())
		if userIdent == nil && agentIdent == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user or agent authentication", nil)
			return
		}
		// If the caller is an agent, verify scope and project isolation for lifecycle actions
		if agentIdent != nil && userIdent == nil {
			if !agentIdent.HasScope(ScopeAgentLifecycle) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:lifecycle", nil)
				return
			}
			// Look up target agent for project isolation check
			targetAgent, err := s.store.GetAgent(r.Context(), id)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if targetAgent.ProjectID != agentIdent.ProjectID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage agents within their own project", nil)
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
			decision := s.authzService.CheckAccess(r.Context(), userIdent, agentResource(targetAgent), ActionAttach)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"Only the agent's creator can interact with it", nil)
				return
			}
		}
	}

	switch action {
	case api.AgentActionStatus:
		s.updateAgentStatus(w, r, id)
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend, api.AgentActionRestart:
		s.handleAgentLifecycle(w, r, id, action)
	case api.AgentActionMessage:
		s.handleAgentMessage(w, r, id)
	case api.AgentActionExec:
		s.handleAgentExec(w, r, id)
	case api.AgentActionResetAuth:
		s.handleAgentResetAuth(w, r, id)
	case api.AgentActionRestore:
		s.restoreAgent(w, r, id)
	case api.AgentActionTokenRefresh:
		s.handleAgentTokenRefresh(w, r, id)
	case api.AgentActionRefreshToken:
		s.handleAgentGitHubTokenRefresh(w, r, id)
	case api.AgentActionOutboundMessage:
		s.handleAgentOutboundMessage(w, r, id)
	case api.AgentActionMessages:
		// Defence-in-depth: this action is normally intercepted earlier in
		// handleAgentRoute (before the POST-only gate) so that GET requests
		// are served. This case handles the unlikely path where the request
		// reaches handleAgentAction directly.
		s.handleAgentMessages(w, r, id)
	default:
		NotFound(w, "Action")
	}
}

func (s *Server) handleAgentExec(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req struct {
		Command []string `json:"command"`
		Timeout int      `json:"timeout,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if len(req.Command) == 0 {
		ValidationError(w, "command is required", nil)
		return
	}
	if req.Timeout < 0 {
		ValidationError(w, "timeout must be non-negative", nil)
		return
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		ServiceNotReady(w, "Exec dispatch is not available yet — the server may still be starting up")
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		ServiceNotReady(w, "Agent has no runtime broker assigned — the server may still be starting up")
		return
	}

	output, exitCode, err := dispatcher.DispatchAgentExec(ctx, agent, req.Command, req.Timeout)
	if err != nil {
		RuntimeError(w, "Failed to execute command on runtime broker: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}{
		Output:   output,
		ExitCode: exitCode,
	})
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

	// Build the generalized tokens[] array.
	// App tokens are always present; transport tokens are added when
	// the hub has a transport minter configured.
	tokens := []RefreshTokenEntry{
		{
			Layer:     "app",
			Type:      "scion_access",
			Value:     newToken,
			ExpiresIn: int(time.Until(expiresAt).Seconds()),
		},
	}

	// Mint a transport token if transport auth is configured
	if s.transportMinter != nil && s.transportAudience != "" {
		tToken, tExpiry, tErr := s.transportMinter.MintIDToken(r.Context(), s.transportAudience)
		if tErr != nil {
			// Log but don't fail the refresh — app token is still valid
			slog.Warn("Failed to mint transport token during refresh",
				"agent_id", id, "error", tErr)
		} else if tToken != "" {
			tokens = append(tokens, RefreshTokenEntry{
				Layer:     "transport",
				Type:      "google_oidc",
				Value:     tToken,
				ExpiresIn: int(time.Until(tExpiry).Seconds()),
				Audience:  s.transportAudience,
			})
		}
	}

	// Response includes both the legacy single-token fields (backward compat)
	// and the generalized tokens[] array. Old clients ignore tokens[];
	// new clients prefer tokens[].
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      newToken,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"tokens":     tokens,
	})
}

// handleAgentResetAuth handles POST /api/v1/agents/{id}/reset-auth.
// It generates a fresh token and pushes it into the running agent container
// via the runtime broker, restarting the agent's token refresh loop without
// a full container restart.
func (s *Server) handleAgentResetAuth(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if s.dispatcher == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"agent dispatcher not configured", nil)
		return
	}

	if err := s.dispatcher.DispatchAgentResetAuth(ctx, agent); err != nil {
		slog.Error("Failed to reset agent auth", "agent_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"auth reset failed: "+err.Error(), nil)
		return
	}

	slog.Info("Agent auth reset dispatched", "agent_id", id)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Auth reset dispatched successfully",
	})
}
