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
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type ListRuntimeBrokersResponse struct {
	Brokers    []store.RuntimeBroker `json:"brokers"`
	NextCursor string                `json:"nextCursor,omitempty"`
	TotalCount int                   `json:"totalCount"`
}

// RuntimeBrokerWithProvider extends RuntimeBroker with project-specific provider data.
// This is returned when listing brokers filtered by projectId, providing the local path
// for the project on each broker.
type RuntimeBrokerWithProvider struct {
	store.RuntimeBroker
	LocalPath string        `json:"localPath,omitempty"` // Filesystem path to the project on this broker
	Cap       *Capabilities `json:"_capabilities,omitempty"`
}

// ListRuntimeBrokersWithProviderResponse is returned when filtering by projectId.
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

	projectID := query.Get("projectId")
	filter := store.RuntimeBrokerFilter{
		Status:    query.Get("status"),
		ProjectID: projectID,
		Name:      query.Get("name"),
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

	// If filtering by projectId, include project-specific provider data (like localPath)
	if projectID != "" {
		// Get provider data for this project to include localPath
		providers, err := s.store.GetProjectProviders(ctx, projectID)
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

	// Handle projects action
	if subPath == "projects" && r.Method == http.MethodGet {
		s.getBrokerProjects(w, r, id)
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

//nolint:unused // Kept for legacy route compatibility.
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

	// Explicitly remove all project provider records for this broker.
	// While the DB schema has ON DELETE CASCADE, we do this at the
	// application level to ensure cleanup regardless of DB behavior
	// and to clear default_runtime_broker_id on affected projects.
	clientIP := getClientIP(r)
	if projects, err := s.store.GetBrokerProjects(ctx, id); err == nil {
		for _, gp := range projects {
			_ = s.store.RemoveProjectProvider(ctx, gp.ProjectID, id)
			LogUnlinkEvent(ctx, s.auditLogger, id, gp.ProjectID, actorID, clientIP)

			// Clear default_runtime_broker_id if it points to this broker
			if project, err := s.store.GetProject(ctx, gp.ProjectID); err == nil {
				if project.DefaultRuntimeBrokerID == id {
					project.DefaultRuntimeBrokerID = ""
					_ = s.store.UpdateProject(ctx, project)
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

// enrichProjectOwnerNames batch-resolves OwnerID UUIDs to display names for a slice of projects.
func (s *Server) enrichProjectOwnerNames(ctx context.Context, projects []store.Project) {
	// Collect unique owner IDs
	ownerIDs := make(map[string]struct{})
	for _, g := range projects {
		if g.OwnerID != "" {
			ownerIDs[g.OwnerID] = struct{}{}
		}
	}
	if len(ownerIDs) == 0 {
		return
	}

	// Resolve each unique owner ID to a display name
	nameMap := make(map[string]string, len(ownerIDs))
	for id := range ownerIDs {
		if user, err := s.store.GetUser(ctx, id); err == nil {
			if user.DisplayName != "" {
				nameMap[id] = user.DisplayName
			} else {
				nameMap[id] = user.Email
			}
		}
	}

	// Apply resolved names
	for i := range projects {
		if name, ok := nameMap[projects[i].OwnerID]; ok {
			projects[i].OwnerName = name
		}
	}
}

// resolveUserProjectIDs returns project IDs from the user's group memberships,
// including transitive memberships through nested groups.
func (s *Server) resolveUserProjectIDs(ctx context.Context, userID string) []string {
	groupIDs, err := s.store.GetEffectiveGroups(ctx, userID)
	if err != nil || len(groupIDs) == 0 {
		return nil
	}

	groups, err := s.store.GetGroupsByIDs(ctx, groupIDs)
	if err != nil {
		return nil
	}

	projectIDSet := make(map[string]struct{})
	for _, g := range groups {
		if g.ProjectID != "" {
			projectIDSet[g.ProjectID] = struct{}{}
		}
	}

	projectIDs := make([]string, 0, len(projectIDSet))
	for id := range projectIDSet {
		projectIDs = append(projectIDs, id)
	}
	return projectIDs
}

// brokerHeartbeatRequest is the request body for broker heartbeats.
type brokerHeartbeatRequest struct {
	Status   string                   `json:"status"`
	Projects []brokerProjectHeartbeat `json:"projects,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (h *brokerHeartbeatRequest) UnmarshalJSON(data []byte) error {
	type Alias brokerHeartbeatRequest
	aux := &struct {
		Groves []brokerProjectHeartbeat `json:"groves,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(h),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(h.Projects) == 0 && len(aux.Groves) > 0 {
		h.Projects = aux.Groves
	}
	return nil
}

// brokerProjectHeartbeat is per-project status in a heartbeat.
type brokerProjectHeartbeat struct {
	ProjectID  string                 `json:"projectId"`
	AgentCount int                    `json:"agentCount"`
	Agents     []brokerAgentHeartbeat `json:"agents,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (p *brokerProjectHeartbeat) UnmarshalJSON(data []byte) error {
	type Alias brokerProjectHeartbeat
	aux := &struct {
		GroveID string `json:"groveId,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(p),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if p.ProjectID == "" && aux.GroveID != "" {
		p.ProjectID = aux.GroveID
	}
	return nil
}

// brokerAgentHeartbeat is per-agent status in a heartbeat.
type brokerAgentHeartbeat struct {
	Slug            string `json:"slug"`   // Agent's URL-safe identifier (name)
	Status          string `json:"status"` // Session status (WORKING, THINKING, etc.)
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	Message         string `json:"message,omitempty"`     // Error or status message from agent
	HarnessAuth     string `json:"harnessAuth,omitempty"` // Resolved auth method from container labels
	Profile         string `json:"profile,omitempty"`     // Settings profile used
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

	// Process agent status updates from each project
	for _, project := range heartbeat.Projects {
		for _, agentHB := range project.Agents {
			// Look up the agent by name (slug) within the project
			agent, err := s.store.GetAgentBySlug(ctx, project.ProjectID, agentHB.Slug)
			if err != nil {
				// Agent not found in this project - skip silently
				// This can happen if the agent exists locally but isn't registered on the Hub
				continue
			}

			// Security check: ensure the agent belongs to this broker
			if agent.RuntimeBrokerID != id {
				slog.Warn("Broker attempted to update agent owned by different broker",
					"brokerID", id,
					"agentBrokerID", agent.RuntimeBrokerID,
					"agent_id", agent.ID)
				continue
			}

			// Build status update with agent status and container status.
			// When the broker sends structured Phase/Activity fields, use
			// them directly. Fall back to container-status derivation for
			// backward compatibility with older brokers.
			statusUpdate := store.AgentStatusUpdate{
				ContainerStatus: agentHB.ContainerStatus,
				Heartbeat:       true, // Ensures LastSeen is updated
				Message:         agentHB.Message,
			}

			// Guard: a heartbeat must never revert an agent out of a
			// terminal phase (stopped/failed) that was set by an explicit
			// lifecycle action. Only start/restart handlers may
			// transition away from these phases. Without this guard a
			// forced heartbeat fired immediately after a stop dispatch
			// can race and overwrite the stopped state with stale
			// container data.
			agentInTerminalPhase := agent.Phase == string(state.PhaseStopped) ||
				agent.Phase == string(state.PhaseError)

			// Suspended is sticky: a suspended agent's container is being torn
			// down, so a racing heartbeat reporting stopped/crashed must not
			// revert the suspended phase (which would defeat resume on the next
			// /start). Like the terminal case, suppress any phase change and any
			// terminal activity (crashed, etc.) from the heartbeat. Only explicit
			// start/stop lifecycle actions may leave the suspended phase.
			agentSuspended := agent.Phase == string(state.PhaseSuspended)

			if agentHB.Phase != "" {
				if agentSuspended {
					// Do not let the heartbeat change the phase or propagate
					// terminal activities while suspended; leave statusUpdate.Phase
					// unset so the hub's authoritative suspended phase is kept.
				} else if agentInTerminalPhase {
					// Keep the hub's authoritative terminal phase; only
					// allow the heartbeat to confirm it (not revert it).
					if agentHB.Phase == agent.Phase {
						statusUpdate.Phase = agentHB.Phase
					}
					// Allow terminal activities (crashed, limits_exceeded)
					// to propagate — they carry information about HOW the
					// agent stopped and may arrive via heartbeat if the
					// direct Hub report was slow or failed.
					hbActivity := state.Activity(agentHB.Activity)
					if hbActivity.IsTerminal() && agentHB.Activity != agent.Activity {
						statusUpdate.Activity = agentHB.Activity
						statusUpdate.Message = agentHB.Message
					}
				} else {
					// Structured path: broker sent Phase/Activity directly.
					// Guard against phase regressions: stale heartbeat data
					// must not move a running agent back to starting/etc.
					hbPhase := state.Phase(agentHB.Phase)
					curPhase := state.Phase(agent.Phase)

					// Derive a crash from the container exit code even when the
					// broker reports a plain "stopped" (its phase derivation is
					// based on the container being exited, not on the exit code).
					// A non-zero exit means the agent crashed → error, with the
					// exit code recorded so the UI can show it. This works even
					// if sciontool's own crash report never reached the hub.
					if hbPhase == state.PhaseStopped {
						if code, ok := scionruntime.ExitCodeFromContainerStatus(agentHB.ContainerStatus); ok && code != 0 {
							hbPhase = state.PhaseError
							agentHB.Phase = string(state.PhaseError)
							c := code
							statusUpdate.ExitCode = &c
							if statusUpdate.Message == "" {
								statusUpdate.Message = fmt.Sprintf("Agent crashed with exit code %d", code)
							}
						}
					}

					if curPhase.IsActivePhase() && hbPhase.IsActivePhase() &&
						hbPhase.Ordinal() < curPhase.Ordinal() {
						// Suppress the regression — keep the hub's phase.
					} else {
						statusUpdate.Phase = agentHB.Phase
					}
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
				}
			} else if !agentInTerminalPhase && !agentSuspended {
				// Legacy path: no structured fields, derive from ContainerStatus
				// Derive phase from container status to ensure agents
				// registered via sync (not started via hub) get proper state.
				// Terminal container states (exited/stopped) override agent phase.
				// Skipped when agent is already in a terminal phase or suspended
				// to avoid reverting an authoritative hub-set state.
				if agentHB.ContainerStatus != "" {
					containerStatusLower := strings.ToLower(agentHB.ContainerStatus)
					switch {
					case strings.HasPrefix(containerStatusLower, "up") || containerStatusLower == "running":
						statusUpdate.Phase = string(state.PhaseRunning)
					case strings.HasPrefix(containerStatusLower, "exited") || containerStatusLower == "stopped":
						// A non-zero exit code means the agent crashed → error
						// (restartable); a zero/absent code is a clean stop.
						if code, ok := scionruntime.ExitCodeFromContainerStatus(agentHB.ContainerStatus); ok && code != 0 {
							statusUpdate.Phase = string(state.PhaseError)
							c := code
							statusUpdate.ExitCode = &c
							if statusUpdate.Message == "" {
								statusUpdate.Message = fmt.Sprintf("Agent crashed with exit code %d", code)
							}
						} else {
							statusUpdate.Phase = string(state.PhaseStopped)
						}
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
						"agent_id", agent.ID, "harnessAuth", agentHB.HarnessAuth, "profile", agentHB.Profile, "error", err)
				}
			}

			// Update the agent's status
			if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
				// Log error but continue processing other agents
				slog.Error("Failed to update agent status from heartbeat",
					"agent_id", agent.ID,
					"agentSlug", agentHB.Slug,
					"project_id", project.ProjectID,
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

// BrokerProjectInfo describes a project from a broker's perspective.
type BrokerProjectInfo struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	GitRemote   string `json:"gitRemote,omitempty"`
	AgentCount  int    `json:"agentCount"`
	LocalPath   string `json:"localPath,omitempty"`
}

// ListBrokerProjectsResponse is the response for listing projects a broker provides.
type ListBrokerProjectsResponse struct {
	Projects []BrokerProjectInfo `json:"projects"`
}

func (s *Server) getBrokerProjects(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Get all projects this broker provides for
	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Build response with project details
	projects := make([]BrokerProjectInfo, 0, len(providers))
	for _, p := range providers {
		info := BrokerProjectInfo{
			ProjectID: p.ProjectID,
			LocalPath: p.LocalPath,
		}

		// Fetch project details for name and git remote
		if project, err := s.store.GetProject(ctx, p.ProjectID); err == nil {
			info.ProjectName = project.Name
			info.GitRemote = project.GitRemote
		}

		// Count agents for this project on this broker
		agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{
			ProjectID:       p.ProjectID,
			RuntimeBrokerID: brokerID,
		}, store.ListOptions{Limit: 0})
		if err == nil {
			info.AgentCount = agentResult.TotalCount
		}

		projects = append(projects, info)
	}
	writeJSON(w, http.StatusOK, ListBrokerProjectsResponse{
		Projects: projects,
	})
}
