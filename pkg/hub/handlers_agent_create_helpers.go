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
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// resolveTemplate looks up a template by ID or name/slug.
// It tries: 1) by ID, 2) by slug in project scope, 3) by slug in global scope.
// Returns nil if not found, or an error for actual failures.
func (s *Server) resolveTemplate(ctx context.Context, templateRef, projectID string) (*store.Template, error) {
	// Try looking up by ID first (the CLI typically resolves names to IDs)
	template, err := s.store.GetTemplate(ctx, templateRef)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	if template != nil {
		return template, nil
	}

	// Try by slug/name within project scope
	template, err = s.store.GetTemplateBySlug(ctx, templateRef, "project", projectID)
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
// or the fallback value if no template was resolved. Prefers the template's
// DefaultHarnessConfig (e.g. "claude-web") over the generic Harness type (e.g. "claude").
func (s *Server) getHarnessConfigFromTemplate(template *store.Template, fallback string) string {
	if template != nil {
		if template.DefaultHarnessConfig != "" {
			return template.DefaultHarnessConfig
		}
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
		Branch:        req.Branch,
		Workspace:     req.Workspace,
		CreatorName:   creatorName,
	}

	ac.NoAuth = req.NoAuth

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

	if ac.HarnessAuth == "none" {
		ac.NoAuth = true
	}

	return ac
}

// populateAgentConfig enriches an agent's AppliedConfig with project-derived and
// template-derived fields after the initial config block has been set up.
// It populates GitClone config from project labels for git-anchored projects, and
// sets template ID, hash, and hub access scopes from the resolved template.
func (s *Server) populateAgentConfig(ctx context.Context, agent *store.Agent, project *store.Project, resolvedTemplate *store.Template) {
	if agent.AppliedConfig == nil {
		return
	}

	// Populate GitClone config for git-anchored projects (per-agent clone mode).
	// Shared-workspace git projects skip clone — agents mount the shared workspace instead.
	if project != nil && project.GitRemote != "" && !project.IsSharedWorkspace() {
		cloneURL := resolveCloneURL(project.Labels["scion.dev/clone-url"], project.GitRemote)
		defaultBranch := project.Labels["scion.dev/default-branch"]
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		agent.AppliedConfig.GitClone = &api.GitCloneConfig{
			URL:    cloneURL,
			Branch: defaultBranch,
			Depth:  1,
		}
	}

	// Populate workspace path for hub-managed projects and shared-workspace git projects.
	if project != nil && (project.GitRemote == "" || project.IsSharedWorkspace()) {
		workspacePath, err := hubManagedProjectPath(project.Slug)
		if err == nil {
			agent.AppliedConfig.Workspace = workspacePath
		}
	}

	// For shared-workspace git projects, default the branch to the project's
	// default branch (the workspace's current branch) instead of the agent slug.
	if project != nil && project.IsSharedWorkspace() && agent.AppliedConfig.Branch == "" {
		defaultBranch := project.Labels["scion.dev/default-branch"]
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		agent.AppliedConfig.Branch = defaultBranch
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

	// Populate harness config ID and hash for broker hydration.
	// Mirrors the template ID/hash stamping above: resolve the harness config
	// by slug (project scope first, then global) and stamp its ID and content
	// hash so the broker can fetch it from Hub storage.
	hcName := agent.AppliedConfig.HarnessConfig
	if hcName == "" && resolvedTemplate != nil {
		hcName = s.getHarnessConfigFromTemplate(resolvedTemplate, "")
	}
	if hcName != "" && agent.AppliedConfig.HarnessConfigID == "" {
		var hc *store.HarnessConfig
		if project != nil {
			var err error
			hc, err = s.store.GetHarnessConfigBySlug(ctx, hcName, store.HarnessConfigScopeProject, project.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.agentLifecycleLog.Warn("failed to get project harness config by slug", "slug", hcName, "project_id", project.ID, "error", err)
			}
		}
		if hc == nil {
			var err error
			hc, err = s.store.GetHarnessConfigBySlug(ctx, hcName, store.HarnessConfigScopeGlobal, "")
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.agentLifecycleLog.Warn("failed to get global harness config by slug", "slug", hcName, "error", err)
			}
		}
		if hc != nil {
			agent.AppliedConfig.HarnessConfigID = hc.ID
			agent.AppliedConfig.HarnessConfigHash = hc.ContentHash
		}
	}

	// Merge hub-level telemetry config as lowest-priority default.
	// Only applies when no per-agent or template telemetry config is set.
	s.mu.RLock()
	hubTelemetry := s.config.TelemetryConfig
	s.mu.RUnlock()
	if hubTelemetry != nil {
		if agent.AppliedConfig.InlineConfig == nil {
			agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
		}
		if agent.AppliedConfig.InlineConfig.Telemetry == nil {
			// Deep copy to avoid sharing the pointer with the server config.
			copied := *hubTelemetry
			agent.AppliedConfig.InlineConfig.Telemetry = &copied
		}
	}

	// Apply project-level TelemetryEnabled override. This takes effect regardless
	// of where the telemetry config came from (inline, template, or hub), so
	// project admins can enable/disable telemetry for all agents in the project.
	if project != nil && project.Annotations != nil {
		if val, ok := project.Annotations[projectSettingTelemetryEnabled]; ok {
			if b, err := strconv.ParseBool(val); err == nil {
				if agent.AppliedConfig.InlineConfig == nil {
					agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
				}
				if agent.AppliedConfig.InlineConfig.Telemetry == nil {
					agent.AppliedConfig.InlineConfig.Telemetry = &api.TelemetryConfig{}
				}
				agent.AppliedConfig.InlineConfig.Telemetry.Enabled = &b
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
	// existingAgentConflict means an active agent with the same slug exists; caller should return 409.
	existingAgentConflict
)

// createNotifySubscription creates a notification subscription for the given agent
// if notify is true and a subscriber has been identified.
func (s *Server) createNotifySubscription(ctx context.Context, agentID, projectID, notifySubscriberType, notifySubscriberID, createdBy string) {
	if notifySubscriberID == "" {
		return
	}
	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    notifySubscriberType,
		SubscriberID:      notifySubscriberID,
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED", "STALLED", "ERROR"},
		CreatedAt:         time.Now(),
		CreatedBy:         createdBy,
	}
	if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
		s.agentLifecycleLog.Warn("Failed to create notification subscription",
			"agent_id", agentID, "subscriber", notifySubscriberID, "error", err)
	} else {
		s.agentLifecycleLog.Debug("Created notification subscription",
			"subscriptionID", sub.ID, "agent_id", agentID,
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
	project *store.Project,
	runtimeBrokerID string,
	req CreateAgentRequest,
	notifySubscriberType, notifySubscriberID, createdBy string,
) existingAgentResult {
	if existingAgent == nil {
		return existingAgentNone
	}
	s.agentLifecycleLog.Info("handleExistingAgent: found existing agent",
		"slug", existingAgent.Slug,
		"existing_agent_id", existingAgent.ID,
		"existing_owner_id", existingAgent.OwnerID,
		"existing_phase", existingAgent.Phase,
		"caller_id", createdBy,
	)
	cleanupMode := req.CleanupMode
	if cleanupMode == "" {
		cleanupMode = "strict"
	}

	// Suspended agents are restarted in-place (not deleted), preserving harness state.
	if !req.ProvisionOnly && existingAgent.Phase == string(state.PhaseSuspended) {
		if existingAgent.RuntimeBrokerID == "" && runtimeBrokerID != "" {
			existingAgent.RuntimeBrokerID = runtimeBrokerID
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil || existingAgent.RuntimeBrokerID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"cannot resume agent: no runtime broker available", nil)
			return existingAgentErrored
		}

		if req.Task != "" {
			if existingAgent.AppliedConfig == nil {
				existingAgent.AppliedConfig = &store.AgentAppliedConfig{}
			}
			existingAgent.AppliedConfig.Task = req.Task
			existingAgent.AppliedConfig.Attach = req.Attach
		}

		// This branch only runs for suspended agents, so resume the harness
		// session (Claude --continue) rather than starting fresh.
		resume := existingAgent.Phase == string(state.PhaseSuspended)
		if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task, resume); err != nil {
			RuntimeError(w, "Failed to resume suspended agent: "+err.Error())
			return existingAgentErrored
		}

		if existingAgent.Phase == string(state.PhaseSuspended) {
			existingAgent.Phase = string(state.PhaseRunning)
		}
		if err := s.store.UpdateAgent(ctx, existingAgent); err != nil {
			s.agentLifecycleLog.Warn("Failed to update agent status after resume", "agent_id", existingAgent.ID, "error", err)
		}

		if req.Notify {
			s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
		}

		s.enrichAgent(ctx, existingAgent, project, nil)
		writeJSON(w, http.StatusOK, CreateAgentResponse{
			Agent: existingAgent,
		})
		return existingAgentStarted
	}

	// Phase 1: Agent is running/stopped/error.
	// Resume=true for stopped agents restarts in-place; otherwise reject as duplicate.
	if !req.ProvisionOnly &&
		(existingAgent.Phase == string(state.PhaseRunning) ||
			existingAgent.Phase == string(state.PhaseStopped) ||
			existingAgent.Phase == string(state.PhaseError)) {

		// Resume a stopped agent in-place when explicitly requested.
		if req.Resume && existingAgent.Phase == string(state.PhaseStopped) {
			if existingAgent.RuntimeBrokerID == "" && runtimeBrokerID != "" {
				existingAgent.RuntimeBrokerID = runtimeBrokerID
			}

			dispatcher := s.GetDispatcher()
			if dispatcher == nil || existingAgent.RuntimeBrokerID == "" {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					"cannot resume agent: no runtime broker available", nil)
				return existingAgentErrored
			}

			if req.Task != "" {
				if existingAgent.AppliedConfig == nil {
					existingAgent.AppliedConfig = &store.AgentAppliedConfig{}
				}
				existingAgent.AppliedConfig.Task = req.Task
				existingAgent.AppliedConfig.Attach = req.Attach
			}

			// A stopped agent restarts with a fresh harness session even when
			// resume was requested (mirrors the local CLI's effectiveResume).
			if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task, false); err != nil {
				RuntimeError(w, "Failed to resume stopped agent: "+err.Error())
				return existingAgentErrored
			}

			existingAgent.Phase = string(state.PhaseRunning)
			if err := s.updateAgentAfterDispatch(ctx, existingAgent); err != nil {
				s.agentLifecycleLog.Warn("Failed to update agent status after resume", "agent_id", existingAgent.ID, "error", err)
			}

			if req.Notify {
				s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
			}

			s.enrichAgent(ctx, existingAgent, project, nil)
			writeJSON(w, http.StatusOK, CreateAgentResponse{
				Agent: existingAgent,
			})
			return existingAgentStarted
		}

		return existingAgentConflict
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
					"agent_id", existingAgent.ID, "agentName", existingAgent.Name, "error", err)
			}
		}
		if err := s.store.DeleteAgent(ctx, existingAgent.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return existingAgentErrored
		}
		return existingAgentDeleted
	}

	// Phase 3: Restart — agent was provisioned/created and needs to be started.
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
		if req.Task != "" {
			if existingAgent.AppliedConfig == nil {
				existingAgent.AppliedConfig = &store.AgentAppliedConfig{}
			}
			existingAgent.AppliedConfig.Task = req.Task
			existingAgent.AppliedConfig.Attach = req.Attach
		}

		// Dispatch start action — DispatchAgentStart applies the broker's
		// response (status, container info) onto existingAgent in-place.
		// A created/provisioning agent has no prior session to resume.
		if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task, false); err != nil {
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
			s.agentLifecycleLog.Warn("Failed to update agent status after start", "agent_id", existingAgent.ID, "error", err)
		}

		// Create notification subscription if requested.
		if req.Notify {
			s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
		}

		// Enrich and return the existing agent.
		s.enrichAgent(ctx, existingAgent, project, nil)
		writeJSON(w, http.StatusOK, CreateAgentResponse{
			Agent: existingAgent,
		})
		return existingAgentStarted
	}

	return existingAgentConflict
}

// resolveRuntimeBroker determines which runtime broker should run the agent.
// Priority order:
//  1. Explicitly specified broker (requestedBrokerID) - verified to be a provider
//  2. Project's default runtime broker - verified to be available (online)
//  3. Single provider (any status) - used automatically
//  4. Multiple providers with online brokers - returns error requiring explicit selection
//  5. No providers - returns error
//
// Returns the runtime broker ID or an error (after writing the HTTP error response).
func (s *Server) resolveRuntimeBroker(ctx context.Context, w http.ResponseWriter, requestedBrokerID string, project *store.Project) (string, error) {
	// Get ALL providers for this project (regardless of status)
	allProviders, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	// Get available (online) brokers for fallback logic
	availableBrokers, err := s.getAvailableBrokersForProject(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	slog.Debug("Resolving runtime broker",
		"project_id", project.ID, "projectName", project.Name,
		"requestedBroker", requestedBrokerID,
		"totalProviders", len(allProviders),
		"onlineProviders", len(availableBrokers),
		"defaultBroker", project.DefaultRuntimeBrokerID,
		"isHubNative", project.GitRemote == "")

	// Convert to summary for error responses, marking and prioritizing the default broker
	brokerSummaries := make([]RuntimeBrokerSummary, 0, len(availableBrokers))
	var defaultBrokerSummary *RuntimeBrokerSummary
	for _, h := range availableBrokers {
		summary := RuntimeBrokerSummary{
			ID:        h.ID,
			Name:      h.Name,
			Status:    h.Status,
			IsDefault: h.ID == project.DefaultRuntimeBrokerID,
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
		// Check if the requested broker is a provider to this project (by ID, Name, or Slug)
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
		// to the project as a provider. This is common for hub-managed projects where
		// providers aren't established via CLI registration.
		broker, err := s.findBrokerByIDOrSlug(ctx, requestedBrokerID)
		if err == nil && broker != nil {
			provider := &store.ProjectProvider{
				ProjectID:  project.ID,
				BrokerID:   broker.ID,
				BrokerName: broker.Name,
				Status:     broker.Status,
				LinkedBy:   "agent-create",
			}
			if addErr := s.store.AddProjectProvider(ctx, provider); addErr != nil {
				slog.Warn("Failed to auto-link broker during agent creation",
					"broker", broker.Name, "project_id", project.ID, "error", addErr)
				RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
				return "", store.ErrNotFound
			}
			slog.Info("Auto-linked broker as project provider",
				"broker", broker.Name, "brokerID", broker.ID, "project_id", project.ID)

			// Set as default if project has none
			if project.DefaultRuntimeBrokerID == "" {
				project.DefaultRuntimeBrokerID = broker.ID
				if updateErr := s.store.UpdateProject(ctx, project); updateErr != nil {
					slog.Warn("Failed to set default runtime broker",
						"broker", broker.Name, "project_id", project.ID, "error", updateErr)
				}
			}
			return broker.ID, nil
		}

		// Broker doesn't exist at all
		slog.Warn("Requested broker not found during agent creation",
			"requestedBrokerID", requestedBrokerID, "project_id", project.ID,
			"providerCount", len(allProviders))
		RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
		return "", store.ErrNotFound
	}

	// Case 2: Use project's default runtime broker (must be online and dispatchable)
	if project.DefaultRuntimeBrokerID != "" {
		// Check if the default broker is still available
		for _, h := range availableBrokers {
			if h.ID == project.DefaultRuntimeBrokerID {
				if s.canDispatchToBroker(ctx, &h) {
					return project.DefaultRuntimeBrokerID, nil
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
		NoRuntimeBroker(w, "No runtime brokers available for this project that you have permission to use", brokerSummaries)
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
		NoRuntimeBroker(w, "No runtime brokers available for this project; register a runtime broker first", brokerSummaries)
		return "", store.ErrNotFound
	case 1:
		return dispatchable[0].ID, nil
	default:
		// Multiple dispatchable brokers - require explicit selection
		NoRuntimeBroker(w, "Multiple runtime brokers available for this project; specify runtimeBrokerId to select one", brokerSummaries)
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

// getAvailableBrokersForProject returns online runtime brokers that are providers to the project.
func (s *Server) getAvailableBrokersForProject(ctx context.Context, projectID string) ([]store.RuntimeBroker, error) {
	// Get providers for this project
	providers, err := s.store.GetProjectProviders(ctx, projectID)
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
