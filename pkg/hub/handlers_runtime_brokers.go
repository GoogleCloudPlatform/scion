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
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
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
		MethodNotAllowed(w, http.MethodGet)
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

	result, err := s.store.ListRuntimeBrokers(ctx, filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Exclude message broker plugins (e.g. Discord, Telegram) — they carry
	// the "scion.io/plugin" label and are not runtime brokers.
	filtered := result.Items[:0]
	for _, b := range result.Items {
		if _, isPlugin := b.Labels["scion.io/plugin"]; !isPlugin {
			filtered = append(filtered, b)
		}
	}
	result.Items = filtered

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

	// Looked up once and reused for every broker in this listing (design.md
	// §5.9, AC-P2-10): the same convention listProjectProviders uses via
	// lookupAgentLimitDefinition, so a per-broker cap never costs more than
	// one extra limit-definition lookup for the whole page.
	limitDef := s.lookupAgentLimitDefinition(ctx)

	brokersWithCaps := make([]RuntimeBrokerWithCapabilities, 0, len(result.Items))
	for i, broker := range result.Items {
		if caps != nil && !capabilityAllows(caps[i], ActionRead) {
			continue
		}
		resp := RuntimeBrokerWithCapabilities{RuntimeBroker: broker}
		if caps != nil && i < len(caps) {
			resp.Cap = caps[i]
		}
		// Capacity fields carry no visibility check beyond the ActionRead
		// filter above: whoever can already see this broker row sees its
		// capacity too, matching the providers listing's rule
		// (ptone/scion#2061 P2.2, design.md §5.6).
		resp.AgentLimit, resp.AgentCount, resp.AgentLimitSource = s.resolveBrokerCapacity(ctx, broker.ID, limitDef)
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

	// Check for the /settings path (ptone/scion#2061 P2, ptone/scion#2177,
	// design.md §5.4). A dedicated subresource, not an extension of PATCH on
	// the broker itself: it keeps settings out of the heartbeat-contended
	// row and gives a clean per-key authorization point (design.md §5.4).
	if subPath == "settings" {
		s.handleBrokerSettings(w, r, brokerID)
		return
	}

	// The broker->Hub launch report (design §3.2), POST
	// .../agents/{agentId}/launch.
	if agentPath, ok := strings.CutPrefix(subPath, "agents/"); ok {
		if agentID, ok := strings.CutSuffix(agentPath, "/launch"); ok {
			// Matches the sibling path-segment routing: an id that is empty
			// or itself contains a "/" (an extra path segment, e.g.
			// .../agents/x/y/launch) is 404, not passed through to the
			// handler to fail on some other validation.
			if agentID == "" || strings.Contains(agentID, "/") {
				NotFound(w, "Agent")
				return
			}
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleAgentLaunchReport(w, r, brokerID, agentID)
			return
		}
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

	// Handle buffered-delivery failure reports (#1820)
	if subPath == "message-failures" && r.Method == http.MethodPost {
		s.handleBrokerMessageFailures(w, r, id)
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

func (s *Server) getRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Resource type must be permissions.ResourceBroker, not "runtime_broker"
	// — the latter has no registry entry, so it silently denied every user
	// identity.
	//
	// No-identity and non-user/non-broker-self callers are rejected before
	// touching the store: that decision doesn't depend on whether the broker
	// exists, so it stays ahead of the fetch below.
	brokerSelf := false
	var userIdent UserIdentity
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == id {
		brokerSelf = true
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			logAuthzDenial(r, nil, Resource{Type: permissions.ResourceBroker, ID: id}, ActionRead, "no identity")
			Unauthorized(w)
			return
		}
		var ok bool
		userIdent, ok = identity.(UserIdentity)
		if !ok {
			logAuthzDenial(r, identity, Resource{Type: permissions.ResourceBroker, ID: id}, ActionRead, "non-user non-broker identity")
			Forbidden(w)
			return
		}
	}

	// writeStoreErr, not writeErrorFromErr: the CheckAccess denial below
	// writes the same "RuntimeBroker not found" body via NotFound, and a
	// nonexistent broker must be indistinguishable from a denied one on the
	// wire, body included — see the comment on that branch below.
	broker, err := s.store.GetRuntimeBroker(ctx, id)
	if err != nil {
		writeStoreErr(w, err, "RuntimeBroker")
		return
	}

	// Authorize the resolved broker (self-access already established above).
	// Using brokerResource(broker) — rather than a hand-typed literal —
	// carries OwnerID, so the creator's ownership relationship grant applies
	// the same way it does for update/delete.
	//
	// This is a read surface, not a mutation, so a denial is reported as 404
	// rather than 403 — matching getProject (handlers_projects_core.go) and
	// the authorizeRead helper's read surfaces (template_handlers.go,
	// harness_config_handlers.go) elsewhere in this package: a caller who
	// cannot read the broker must not be able to distinguish "exists but
	// denied" from "does not exist" by probing IDs. That only holds if both
	// branches write the identical body, which is why the store lookup just
	// above also uses writeStoreErr(..., "RuntimeBroker") instead of a bare
	// writeErrorFromErr.
	if !brokerSelf {
		decision := s.authzService.CheckAccess(ctx, userIdent, brokerResource(broker), ActionRead)
		if !decision.Allowed {
			logAuthzDenial(r, userIdent, brokerResource(broker), ActionRead, decision.Reason)
			NotFound(w, "RuntimeBroker")
			return
		}
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
	if !s.authorize(w, r, brokerResource(broker), ActionUpdate) {
		return
	}

	var updates struct {
		Name                       string            `json:"name,omitempty"`
		Labels                     map[string]string `json:"labels,omitempty"`
		GCPHostServiceAccountEmail *string           `json:"gcpHostServiceAccountEmail,omitempty"`
		GCPHostProjectID           *string           `json:"gcpHostProjectId,omitempty"`
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
	if updates.GCPHostServiceAccountEmail != nil {
		email := strings.ToLower(*updates.GCPHostServiceAccountEmail)
		if email != "" && !isValidServiceAccountEmail(email) {
			ValidationError(w, "gcpHostServiceAccountEmail must be a valid GCP service account email "+
				"(name@project.iam.gserviceaccount.com)", map[string]interface{}{
				"field": "gcpHostServiceAccountEmail",
			})
			return
		}
		broker.GCPHostServiceAccountEmail = email
	}
	if updates.GCPHostProjectID != nil {
		broker.GCPHostProjectID = *updates.GCPHostProjectID
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
	if !s.authorize(w, r, brokerResource(broker), ActionDelete) {
		return
	}

	// Attribution for the audit events below. Read from the full identity rather
	// than the user identity: a caller that is not a user now has to pass the
	// check above rather than skip it, so the event should name whoever it was.
	var actorID string
	if ident := GetIdentityFromContext(ctx); ident != nil {
		actorID = ident.ID()
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

	// Clean up the broker's HMAC secret (best-effort, post-delete).
	if err := s.store.DeleteBrokerSecret(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "failed to delete broker secret during deregistration", "brokerId", id, "error", err)
	}
	// Clean up any unconsumed join token (best-effort, post-delete).
	if err := s.store.DeleteJoinToken(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "failed to delete broker join token during deregistration", "brokerId", id, "error", err)
	}

	// Log the deregistration event
	LogDeregisterEvent(ctx, s.auditLogger, id, brokerName, actorID, clientIP)

	w.WriteHeader(http.StatusNoContent)
}

// checkBrokerDispatchAccess verifies that the caller has dispatch permission on
// the given broker. Returns true if access is granted. If denied, it writes a
// 403 response and returns false. If the broker cannot be found, it writes an
// error and returns false.
//
// The decision is canDispatchToBroker's, called rather than restated: the two
// were "one decision written twice" and this is the copy that drifted. It opened
// with `if userIdent == nil { return true }` — read as "broker-to-broker, allow",
// but GetUserIdentityFromContext also returns nil for an agent caller and for no
// caller at all, so it handed dispatch to every agent regardless of scope or
// project, and to anything unauthenticated that reached it (ptone/scion#591).
// Delegating leaves one place where the rule can change.
func (s *Server) checkBrokerDispatchAccess(ctx context.Context, w http.ResponseWriter, brokerID string) bool {
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if !s.canDispatchToBroker(ctx, broker) {
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

// subtractProjectIDs returns IDs from all that are NOT in exclude.
// It computes Shared as the authorized project set minus the owner-only set.
func subtractProjectIDs(all, exclude []string) []string {
	if len(exclude) == 0 {
		return all
	}
	excludeSet := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excludeSet[id] = struct{}{}
	}
	result := make([]string, 0, len(all))
	for _, id := range all {
		if _, excluded := excludeSet[id]; !excluded {
			result = append(result, id)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// brokerHeartbeatRequest is the request body for broker heartbeats.
type brokerHeartbeatRequest struct {
	Status   string                   `json:"status"`
	Projects []brokerProjectHeartbeat `json:"projects,omitempty"`
	// Capabilities refreshes the broker's stored capabilities on every
	// heartbeat (design §3.4 Amendment A2.2(b); see
	// hubclient.BrokerHeartbeat.Capabilities). Omitted by an old broker, in
	// which case the store's capabilities are
	// left exactly as CompleteBrokerJoin last set them.
	Capabilities *store.BrokerCapabilities `json:"capabilities,omitempty"`
	// Inventory reports, per runtime target, whether Projects is that
	// target's complete inventory (see hubclient.BrokerInventory). Omitted by
	// an older broker, in which case the missing-container reconcile never
	// runs for it.
	Inventory *brokerInventory `json:"inventory,omitempty"`
	// WorkspaceStorage refreshes the broker's stored workspace storage
	// descriptor (see hubclient.BrokerHeartbeat.WorkspaceStorage). Omitted
	// by an older broker, in which case the stored descriptor is left
	// unchanged.
	WorkspaceStorage *api.BrokerWorkspaceStorage `json:"workspaceStorage,omitempty"`
}

// brokerProjectHeartbeat is per-project status in a heartbeat.
type brokerProjectHeartbeat struct {
	ProjectID  string                 `json:"projectId"`
	AgentCount int                    `json:"agentCount"`
	Agents     []brokerAgentHeartbeat `json:"agents,omitempty"`
}

// brokerAgentHeartbeat is per-agent status in a heartbeat.
type brokerAgentHeartbeat struct {
	// RuntimeTarget is the inventory target that listed the agent.
	RuntimeTarget string `json:"runtimeTarget,omitempty"`

	Slug            string `json:"slug"`   // Agent's URL-safe identifier (name)
	Status          string `json:"status"` // Session status (WORKING, THINKING, etc.)
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	Message         string `json:"message,omitempty"`     // Error or status message from agent
	HarnessAuth     string `json:"harnessAuth,omitempty"` // Resolved auth method from container labels
	Profile         string `json:"profile,omitempty"`     // Settings profile used
	ExitCode        *int   `json:"exitCode,omitempty"`    // Structured exit code from runtime (nil = unknown)
	ExitReason      string `json:"exitReason,omitempty"`  // Terminal reason: "crashed", "limits_exceeded", "preempted", or "evicted" (see state.ExitReason)
}

func (s *Server) handleBrokerHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Authorize: broker self-access or user with CheckAccess (ActionUpdate —
	// heartbeats modify agent state and broker liveness).
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == id {
		// Broker sending its own heartbeat — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			logAuthzDenial(r, nil, Resource{Type: "runtime_broker", ID: id}, ActionUpdate, "no identity")
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent,
				Resource{Type: "runtime_broker", ID: id}, ActionUpdate)
			if !decision.Allowed {
				logAuthzDenial(r, userIdent, Resource{Type: "runtime_broker", ID: id}, ActionUpdate, decision.Reason)
				Forbidden(w)
				return
			}
		} else {
			logAuthzDenial(r, identity, Resource{Type: "runtime_broker", ID: id}, ActionUpdate, "non-user non-broker identity")
			Forbidden(w)
			return
		}
	}

	var heartbeat brokerHeartbeatRequest
	if err := readJSON(r, &heartbeat); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Snapshot the broker row before this heartbeat is stored: the
	// missing-container reconcile needs to know whether the broker was
	// already online and fresh, or is returning from a stale/offline period.
	// Only read when the heartbeat could drive a reconcile.
	var prevBroker *store.RuntimeBroker
	if len(heartbeat.completeTargets()) > 0 {
		if b, err := s.store.GetRuntimeBroker(ctx, id); err == nil {
			prevBroker = b
		}
	}

	// Update the broker's heartbeat status
	if err := s.store.UpdateRuntimeBrokerHeartbeat(ctx, id, heartbeat.Status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// heartbeatBroker is loaded at most once per heartbeat, on first use: either
	// eagerly right below when Capabilities need refreshing, or lazily by
	// loadHeartbeatBroker the first time an agent's Runtime backfill actually
	// needs it (ptone/scion#2262). Current brokers report Capabilities on
	// every heartbeat, so this is normally the one read the Capabilities
	// refresh already makes, and the Runtime backfill reuses it rather than
	// adding another. A heartbeat without Capabilities reads the broker only
	// if some agent needs a Runtime backfill. The error (if any) is cached
	// alongside the broker so each caller can log it with its own message
	// and level.
	var heartbeatBroker *store.RuntimeBroker
	var heartbeatBrokerErr error
	var heartbeatBrokerLoaded bool
	loadHeartbeatBroker := func() (*store.RuntimeBroker, error) {
		if heartbeatBrokerLoaded {
			return heartbeatBroker, heartbeatBrokerErr
		}
		heartbeatBrokerLoaded = true
		heartbeatBroker, heartbeatBrokerErr = s.store.GetRuntimeBroker(ctx, id)
		return heartbeatBroker, heartbeatBrokerErr
	}

	// Design §3.4 Amendment A2.2(b): refresh stored capabilities from every heartbeat that
	// reports them, so an already-registered broker's capabilities are never
	// stuck at whatever CompleteBrokerJoin saw once at join time — the false
	// 412 case that otherwise blocks `scion reincarnate` until a manual
	// --force re-registration. An old broker sends no Capabilities field at
	// all, and the store keeps whatever it already had (nil-safe: a missing
	// field, not an empty struct, is the "don't touch" signal).
	// WorkspaceStorage follows the same rule, so the hub sees share health
	// changes within one heartbeat. Both are persisted in a single update,
	// and only when something changed.
	if heartbeat.Capabilities != nil || heartbeat.WorkspaceStorage != nil {
		if broker, err := loadHeartbeatBroker(); err != nil {
			s.agentLifecycleLog.Warn("heartbeat: failed to load broker to refresh capabilities",
				"broker_id", id, "error", err)
		} else {
			changed := false
			if heartbeat.Capabilities != nil && !reflect.DeepEqual(broker.Capabilities, heartbeat.Capabilities) {
				broker.Capabilities = heartbeat.Capabilities
				changed = true
			}
			if heartbeat.WorkspaceStorage != nil && !reflect.DeepEqual(broker.WorkspaceStorage, heartbeat.WorkspaceStorage) {
				broker.WorkspaceStorage = heartbeat.WorkspaceStorage
				changed = true
			}
			if changed {
				if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
					s.agentLifecycleLog.Warn("heartbeat: failed to persist refreshed capabilities",
						"broker_id", id, "error", err)
				}
			}
		}
	}

	// Process agent status updates from each project
	report := newHeartbeatReport()
	for _, project := range heartbeat.Projects {
		for _, agentHB := range project.Agents {
			// Look up the agent by name (slug) within the project
			agent, err := s.store.GetAgentBySlug(ctx, project.ProjectID, agentHB.Slug)
			if err != nil {
				// Agent not found in this project - skip silently
				// This can happen if the agent exists locally but isn't registered on the Hub
				report.unresolvedSlugs[agentHB.Slug] = true
				continue
			}

			// Defense in depth: skip agents not assigned to the targeted broker.
			// Caller authorization is handled above at the handler level.
			if agent.RuntimeBrokerID != id {
				report.unresolvedSlugs[agentHB.Slug] = true
				slog.Warn("Broker attempted to update agent owned by different broker",
					"brokerID", id,
					"agentBrokerID", agent.RuntimeBrokerID,
					"agent_id", agent.ID)
				continue
			}
			report.present[agent.ID] = true

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

			// Reincarnation in flight is sticky like suspension (design §3.4
			// Amendment A11 item 2): the worker owns Phase/Activity/ExitCode/
			// ExitReason/Message for the duration of a migration, so a racing
			// heartbeat — including one reporting the OLD container being torn
			// down mid-reprovision, or a stale crash from before the restart —
			// must not report a spurious failure while the new generation is
			// still coming up. ContainerStatus and the Heartbeat/LastSeen bump
			// still apply below; only the status fields the worker itself drives
			// are suppressed.
			agentReincarnating := reincarnationInFlight(agent)
			if agentReincarnating {
				statusUpdate.Message = ""
			}

			// A delete in progress is sticky the same way (design
			// ptone/scion#2483 §2.1): the delete engine owns the status
			// fields while its lease is live. ContainerStatus and the
			// Heartbeat/LastSeen bump still apply. UpdateAgentStatus repeats
			// this check inside its transaction, but suppressing the phase
			// here is what keeps reconcileBrokerQuotaOnPhaseChange below from
			// acting on the reported phase. (Soft-deleted rows never get here:
			// GetAgentBySlug skips them.)
			agentDeleting := deletionActive(agent)
			if agentDeleting {
				statusUpdate.Message = ""
			}

			if agentHB.Phase != "" {
				if agentSuspended || agentReincarnating || agentDeleting {
					// Do not let the heartbeat change the phase or propagate
					// terminal activities while suspended or reincarnating; leave
					// statusUpdate.Phase unset so the hub's authoritative phase is
					// kept.
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
					// A Kubernetes pod disruption (preempted/evicted) is a
					// graceful deletion: the pod gets SIGTERM, so sciontool
					// reports a plain clean stop directly to the Hub before
					// the broker's next heartbeat has a chance to observe
					// the pod's disruption signal. By the time that
					// heartbeat arrives the agent is already in a terminal
					// phase, so without this the more specific reason is
					// silently dropped and the agent is stuck reading as a
					// plain stop — the exact symptom in #2528. Back it in
					// only when nothing more specific is stored yet, and
					// leave the phase itself untouched.
					hbExitReason := state.ExitReason(agentHB.ExitReason)
					isDisruption := hbExitReason == state.ExitReasonPreempted || hbExitReason == state.ExitReasonEvicted
					if isDisruption && agent.ExitReason == "" {
						statusUpdate.ExitReason = agentHB.ExitReason
						statusUpdate.ExitCode = agentHB.ExitCode
						if isGenericStopMessage(agent.Message) {
							// The heartbeat's own ExitCode may be nil (for example
							// a disruption observed after the agent container
							// never started), while the agent already has one on
							// record from an earlier update — the store update
							// above leaves that stored value untouched when
							// statusUpdate.ExitCode is nil, so fall back to it
							// here too, or the message would undersell what is
							// actually going to be stored.
							exitCode := agentHB.ExitCode
							if exitCode == nil {
								exitCode = agent.ExitCode
							}
							statusUpdate.Message = exitStatusMessage(hbExitReason, exitCode)
						}
					}
				} else {
					// Structured path: broker sent Phase/Activity directly.
					// Guard against phase regressions: stale heartbeat data
					// must not move a running agent back to starting/etc.
					hbPhase := state.Phase(agentHB.Phase)
					curPhase := state.Phase(agent.Phase)

					// Derive a crash from the exit code even when the broker
					// reports a plain "stopped". Prefer the structured ExitCode
					// field; fall back to parsing ContainerStatus for old brokers.
					if hbPhase == state.PhaseStopped || hbPhase == state.PhaseError {
						if agentHB.ExitCode != nil && *agentHB.ExitCode != 0 {
							// crash path
							if hbPhase == state.PhaseStopped {
								// Promote PhaseStopped→PhaseError when exit code is non-zero.
								hbPhase = state.PhaseError
								agentHB.Phase = string(state.PhaseError)
							}
							statusUpdate.ExitCode = agentHB.ExitCode
							if isValidExitReason(agentHB.ExitReason) {
								statusUpdate.ExitReason = agentHB.ExitReason
							} else if agentHB.ExitReason != "" {
								slog.Debug("dropping invalid ExitReason from heartbeat", "exitReason", agentHB.ExitReason, "agent", agentHB.Slug)
							}
							if statusUpdate.Message == "" {
								statusUpdate.Message = exitStatusMessage(state.ExitReason(statusUpdate.ExitReason), agentHB.ExitCode)
							}
						} else if hbPhase == state.PhaseStopped && agentHB.ExitCode == nil {
							// Legacy fallback: parse from ContainerStatus string (old broker).
							// Only applies to PhaseStopped — PhaseError already has the correct phase.
							if code, ok := scionruntime.ExitCodeFromContainerStatus(agentHB.ContainerStatus); ok && code != 0 { //nolint:staticcheck // legacy fallback for brokers without ExitCode
								hbPhase = state.PhaseError
								agentHB.Phase = string(state.PhaseError)
								c := code
								statusUpdate.ExitCode = &c
							}
							// A structured ExitReason (for example a Kubernetes
							// disruption, which may carry no meaningful exit code
							// when the agent container never started) must still
							// be persisted even when no legacy exit code parses
							// above — a valid reason alone, with no non-zero
							// code, is not something the ContainerStatus-parsing
							// branch above accounts for.
							if isValidExitReason(agentHB.ExitReason) {
								statusUpdate.ExitReason = agentHB.ExitReason
							} else if agentHB.ExitReason != "" {
								slog.Debug("dropping invalid ExitReason from heartbeat", "exitReason", agentHB.ExitReason, "agent", agentHB.Slug)
							}
							if statusUpdate.Message == "" {
								statusUpdate.Message = exitStatusMessage(state.ExitReason(statusUpdate.ExitReason), statusUpdate.ExitCode)
							}
						} else {
							// PhaseStopped with ExitCode == 0 (clean exit) or
							// PhaseError with nil/zero ExitCode: persist what we have.
							statusUpdate.ExitCode = agentHB.ExitCode
							if isValidExitReason(agentHB.ExitReason) {
								statusUpdate.ExitReason = agentHB.ExitReason
							} else if agentHB.ExitReason != "" {
								slog.Debug("dropping invalid ExitReason from heartbeat", "exitReason", agentHB.ExitReason, "agent", agentHB.Slug)
							}
							if statusUpdate.Message == "" {
								statusUpdate.Message = exitStatusMessage(state.ExitReason(statusUpdate.ExitReason), agentHB.ExitCode)
							}
						}
					} else {
						// The pod is still running (not yet Stopped/Error) but
						// the broker already observed a committed Kubernetes
						// disruption (List() reports preempted/evicted ahead of
						// the pod actually terminating — a deletionTimestamp
						// plus a live DisruptionTarget condition, since
						// scheduler preemption and the eviction API usually
						// delete the pod object outright once it does
						// terminate, often before any heartbeat sees a
						// terminal phase). Record the reason now so it is not
						// lost; leave phase and message for the eventual
						// terminal report to set, same as the
						// agentInTerminalPhase backfill above.
						//
						// Known gap: a heartbeat gathered from the old pod
						// while it was still terminating can land after a
						// restart or create-resume has already cleared the
						// reason for the new generation (ClearExit; see
						// store.AgentStatusUpdate), writing the stale reason
						// back onto it. This branch has no way to tell the
						// old pod's identity from the new one. The window is
						// narrow for Kubernetes: Start force-deletes a stale
						// pod with no grace period before creating the
						// replacement.
						hbExitReason := state.ExitReason(agentHB.ExitReason)
						isDisruption := hbExitReason == state.ExitReasonPreempted || hbExitReason == state.ExitReasonEvicted
						if isDisruption && agent.ExitReason == "" {
							statusUpdate.ExitReason = agentHB.ExitReason
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
			} else if !agentInTerminalPhase && !agentSuspended && !agentReincarnating && !agentDeleting {
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
						if code, ok := scionruntime.ExitCodeFromContainerStatus(agentHB.ContainerStatus); ok && code != 0 { //nolint:staticcheck // legacy fallback for brokers without ExitCode
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
				// A non-terminal pod's structured Phase is "" by design (see
				// List()'s committed-disruption branch in k8s_runtime.go,
				// which reports a preempted/evicted reason while a pod is
				// still Running), so that heartbeat lands in this legacy
				// branch too whenever the broker's own phase tracking has
				// nothing more specific to report. Record the reason the
				// same way as the structured non-terminal branch above: only
				// when nothing more specific is stored yet, without
				// touching the phase or message derived from ContainerStatus.
				hbExitReason := state.ExitReason(agentHB.ExitReason)
				isDisruption := hbExitReason == state.ExitReasonPreempted || hbExitReason == state.ExitReasonEvicted
				if isDisruption && agent.ExitReason == "" {
					statusUpdate.ExitReason = agentHB.ExitReason
				}
			}

			// If the broker didn't send a ContainerStatus but we have structured
			// fields, render a display string for backward-compatible clients.
			if statusUpdate.ContainerStatus == "" && agentHB.ContainerStatus == "" {
				if statusUpdate.Phase != "" {
					switch state.Phase(statusUpdate.Phase) {
					case state.PhaseRunning:
						statusUpdate.ContainerStatus = "running"
					case state.PhaseStopped:
						statusUpdate.ContainerStatus = "stopped"
					case state.PhaseError:
						if statusUpdate.ExitCode != nil {
							statusUpdate.ContainerStatus = fmt.Sprintf("exited (%d)", *statusUpdate.ExitCode)
						} else {
							statusUpdate.ContainerStatus = "exited"
						}
					case state.PhaseProvisioning:
						statusUpdate.ContainerStatus = "created"
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
			// Companion to the Profile backfill above: when Runtime is
			// still empty, resolve it from the applied profile (or from the
			// broker's single shared profile type when the profile is
			// unknown or unmatched) (ptone/scion#2262). This only fires for
			// the broker sending this heartbeat, which is the broker the
			// agent is actually running on.
			//
			// Runtime is persisted here (not left purely derived, the way
			// display-time enrichment computes it on every read) because a
			// couple of raw, unenriched reads of store.Agent surface it
			// directly: restoreAgent publishes AgentCreatedEvent and returns
			// agent.ToAPI() without enrichment (handlers_agent_messaging.go),
			// and the agent-create path re-reads the row before publishing
			// its own AgentCreatedEvent (handlers_agents_core.go) — so a
			// heartbeat that backfills Runtime before either of those runs
			// is reflected there.
			//
			// Only fires when Runtime is empty. The only other writer of a
			// non-empty Runtime for a broker-hosted agent is the dispatch
			// response path (httpdispatcher.go's applyBrokerResponse), which
			// sets Runtime from the broker's own AgentInfo for the agent it
			// actually started (recording Profile too when AppliedConfig
			// exists), so it is authoritative and must never be overwritten
			// by a resolveAgentRuntime guess here, even when a
			// newly-backfilled Profile would resolve to a different value.
			if agent.Runtime == "" {
				broker, err := loadHeartbeatBroker()
				if err != nil {
					s.agentLifecycleLog.Debug("heartbeat: failed to load broker for runtime backfill", "broker_id", id, "error", err)
				} else if rt := resolveAgentRuntime(agent, broker); rt != "" {
					agent.Runtime = rt
					needsUpdate = true
				}
			}
			if needsUpdate {
				if err := s.store.UpdateAgent(ctx, agent); err != nil {
					slog.Warn("Failed to backfill agent config from heartbeat",
						"agent_id", agent.ID, "harnessAuth", agentHB.HarnessAuth, "profile", agentHB.Profile,
						"error", err)
				}
			}
			s.recordHeartbeatRuntimeTarget(ctx, agent, agentHB.RuntimeTarget)

			// Reconcile the max_agents_per_broker reservation against the
			// phase this heartbeat will actually persist — e.g. release on an
			// observed crash/exit, or best-effort re-reserve on an observed
			// out-of-band restart (ptone/scion#1963).
			s.reconcileBrokerQuotaOnPhaseChange(ctx, agent, agent.Phase, statusUpdate.Phase)

			// Update the agent's status
			if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
				// Log error but continue processing other agents
				slog.Error("Failed to update agent status from heartbeat",
					"agent_id", agent.ID,
					"agentSlug", agentHB.Slug,
					"project_id", project.ProjectID,
					"error", err)
			} else {
				// Publish SSE event so the frontend receives activity updates.
				// A row soft-deleted between the slug lookup and this re-read
				// (a delete finishing concurrently) publishes nothing.
				if updated, err := s.store.GetAgent(ctx, agent.ID); err == nil && updated.DeletedAt.IsZero() {
					s.events.PublishAgentStatus(ctx, updated)
				}
			}
		}
	}

	// Reconcile running agents of this broker that the heartbeat no longer
	// reports (their container is gone). Gated on a complete inventory and a
	// fresh broker; see broker_heartbeat_reconcile.go.
	s.reconcileMissingAgents(ctx, id, prevBroker, &heartbeat, report)

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

	// Resource type must be permissions.ResourceBroker, not "runtime_broker"
	// — see getRuntimeBroker for the full explanation.
	brokerSelf := false
	var identity Identity
	var userIdent UserIdentity
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		brokerSelf = true
	} else {
		identity = GetIdentityFromContext(ctx)
		if identity == nil {
			logAuthzDenial(r, nil, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead, "no identity")
			Unauthorized(w)
			return
		}
		var ok bool
		userIdent, ok = identity.(UserIdentity)
		if !ok {
			logAuthzDenial(r, identity, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead, "non-user non-broker identity")
			Forbidden(w)
			return
		}
	}

	// Verify broker exists (also gives us OwnerID for the ownership grant).
	// writeStoreErr, not writeErrorFromErr — see getRuntimeBroker: the denial
	// branch below must write the identical body a nonexistent broker gets.
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeStoreErr(w, err, "RuntimeBroker")
		return
	}

	// This is a read surface, not a mutation, so a denial is reported as 404
	// rather than 403 — see getRuntimeBroker for the full explanation.
	if !brokerSelf {
		decision := s.authzService.CheckAccess(ctx, userIdent, brokerResource(broker), ActionRead)
		if !decision.Allowed {
			logAuthzDenial(r, userIdent, brokerResource(broker), ActionRead, decision.Reason)
			NotFound(w, "RuntimeBroker")
			return
		}
	}

	// Get all projects this broker provides for
	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve project records up front: needed both for the response body
	// and for the per-project read filter below. A project that no longer
	// exists (its provider record not yet cleaned up) is left unenriched —
	// listed by ID and LocalPath only, still subject to the read filter
	// below, with no name or git remote — rather than an error; any other
	// store error — a connection failure, for example — is propagated
	// instead of silently producing an incomplete list.
	projectsByID := make(map[string]*store.Project, len(providers))
	for _, p := range providers {
		project, err := s.store.GetProject(ctx, p.ProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			writeErrorFromErr(w, err, "")
			return
		}
		projectsByID[p.ProjectID] = project
	}

	// Cross-project disclosure guard: broker.read authorizes reading the
	// BROKER record, but must not double as project.read for every project
	// the broker happens to serve — an auto-provide broker can serve every
	// project on the hub, so
	// without this filter any hub member could recover the whole hub's
	// project catalogue (names, git remotes) through this endpoint. Filter
	// the provider list down to projects the caller can actually read,
	// through the normal project authz path (so admins keep their usual
	// bypass). The broker's own self-identity needs the unfiltered list to
	// operate and is exempt, matching every other self-access branch in this
	// file.
	readable := make(map[string]bool, len(providers))
	if brokerSelf {
		for _, p := range providers {
			readable[p.ProjectID] = true
		}
	} else {
		resources := make([]Resource, len(providers))
		for i, p := range providers {
			if project, ok := projectsByID[p.ProjectID]; ok {
				resources[i] = projectResource(project)
			} else {
				resources[i] = Resource{Type: permissions.ResourceProject, ID: p.ProjectID}
			}
		}
		allowed, err := s.authzService.AuthorizeReadBatch(ctx, identity, resources)
		if err != nil {
			// Fail closed: an authorization-store error must not leak
			// project names or git remotes.
			writeErrorFromErr(w, err, "")
			return
		}
		for i, p := range providers {
			readable[p.ProjectID] = allowed[i]
		}
	}

	// Build response with project details, respecting the read filter.
	projects := make([]BrokerProjectInfo, 0, len(providers))
	for _, p := range providers {
		if !readable[p.ProjectID] {
			continue
		}

		info := BrokerProjectInfo{
			ProjectID: p.ProjectID,
			LocalPath: p.LocalPath,
		}
		if project, ok := projectsByID[p.ProjectID]; ok {
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

// isValidExitReason reports whether reason is a valid ExitReason value.
func isValidExitReason(reason string) bool {
	return state.ExitReason(reason).IsValid()
}

// isGenericStopMessage reports whether msg is one of the stored agent.Message
// values that carry no information beyond "the agent reported a plain stop":
// empty (nothing recorded yet), or one of the fixed strings sciontool/the
// hook handlers send for an ordinary graceful shutdown. A disruption reason
// learned later from a heartbeat is strictly more informative than any of
// these and may replace them; any other stored message is assumed to already
// carry meaningful, possibly user-relevant text and is left alone.
func isGenericStopMessage(msg string) bool {
	switch msg {
	case "", "Agent stopped", "Session ended":
		return true
	default:
		return false
	}
}

// exitStatusMessage returns the default human-readable status Message for a
// terminal agent, derived from the resolved ExitReason and the structured
// ExitCode reported by the broker. Kubernetes pod disruptions get their own
// wording so a preempted or evicted agent is not reported as a generic
// crash; every other reason (including the empty one) keeps the existing
// "Agent crashed with exit code N" wording, and produces no message at all
// when there is no non-zero exit code to report.
func exitStatusMessage(reason state.ExitReason, exitCode *int) string {
	hasNonZeroExit := exitCode != nil && *exitCode != 0
	switch reason {
	case state.ExitReasonPreempted:
		if hasNonZeroExit {
			return fmt.Sprintf("Agent pod was preempted, exit code %d", *exitCode)
		}
		return "Agent pod was preempted"
	case state.ExitReasonEvicted:
		if hasNonZeroExit {
			return fmt.Sprintf("Agent pod was evicted, exit code %d", *exitCode)
		}
		return "Agent pod was evicted"
	default:
		if hasNonZeroExit {
			return fmt.Sprintf("Agent crashed with exit code %d", *exitCode)
		}
		return ""
	}
}
