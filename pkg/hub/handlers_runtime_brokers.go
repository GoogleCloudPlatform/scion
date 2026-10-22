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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
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
	// the plugin label (see isPluginBroker) and are not runtime brokers.
	filtered := result.Items[:0]
	for i := range result.Items {
		if !isPluginBroker(&result.Items[i]) {
			filtered = append(filtered, result.Items[i])
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

		// The dispatch capability of a project-filtered listing follows the
		// project-aware rule agent creation uses.
		var project *store.Project
		if p, err := s.store.GetProject(ctx, projectID); err == nil {
			project = p
		}

		// Build extended broker list with provider data
		extendedBrokers := make([]RuntimeBrokerWithProvider, 0, len(result.Items))
		for i, broker := range result.Items {
			if caps != nil && !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			eb := RuntimeBrokerWithProvider{
				RuntimeBroker: brokerForDisplay(broker),
				LocalPath:     brokerLocalPaths[broker.ID],
			}
			eb.RuntimeBroker = withHostIdentity(eb.RuntimeBroker, false)
			if caps != nil && i < len(caps) {
				eb.RuntimeBroker = brokerForDisplayWithCap(broker, caps[i])
				eb.Cap = caps[i]
				s.reportBrokerDispatchCapability(ctx, eb.Cap, &result.Items[i], project)
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
	// One non-blocking snapshot of this process's embedded brokers for the
	// whole page (hostedByThisServer).
	embedded := s.embeddedBrokerSnapshot()

	brokersWithCaps := make([]RuntimeBrokerWithCapabilities, 0, len(result.Items))
	for i, broker := range result.Items {
		if caps != nil && !capabilityAllows(caps[i], ActionRead) {
			continue
		}
		resp := RuntimeBrokerWithCapabilities{RuntimeBroker: withHostIdentity(brokerForDisplay(broker), false)}
		resp.HostedByThisServer = embedded.has(broker.ID)
		if caps != nil && i < len(caps) {
			resp.RuntimeBroker = brokerForDisplayWithCap(broker, caps[i])
			resp.Cap = caps[i]
			// Without a project, the project-independent dispatch rule.
			s.reportBrokerDispatchCapability(ctx, resp.Cap, &result.Items[i], nil)
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
	resp := RuntimeBrokerWithCapabilities{RuntimeBroker: withHostIdentity(brokerForDisplay(*broker), false)}
	resp.HostedByThisServer = s.embeddedBrokerSnapshot().has(broker.ID)
	if ident := GetIdentityFromContext(ctx); ident != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, ident, brokerResource(broker))
		resp.RuntimeBroker = brokerForDisplayWithCap(*broker, resp.Cap)
		s.reportBrokerDispatchCapability(ctx, resp.Cap, broker, nil)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Updating a broker is a governance action on a hub-level record that
	// user access tokens cannot take (GOV_PENDING); broker.update is then
	// checked on the record.
	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		return
	}

	broker, err := s.store.GetRuntimeBroker(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A test identity with an admin role updates only a broker it
	// registered (test-admin policy).
	if broker.CreatedBy != actor.ID() && s.testAdminPolicyRefuses(ctx, "handlers_runtime_brokers.go:updateRuntimeBroker") {
		Forbidden(w)
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
		// GCPHostPinned false hands the GCP host pair back to registration
		// and detection (the values are unchanged); true keeps the current
		// values until the next update. Setting either GCP host field
		// implies true unless false is sent with it.
		GCPHostPinned *bool `json:"gcpHostPinned,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	// Test admin tier: the plugin broker label is hub-reserved.
	if s.refuseTestAdminPluginLabel(w, updates.Labels) {
		return
	}

	before := *broker
	before.Labels = make(map[string]string, len(broker.Labels))
	for k, v := range broker.Labels {
		before.Labels[k] = v
	}

	recordChanged := false
	if updates.Name != "" {
		// A rename may not make a flat row's name or slug collide with
		// another row, or another row's with a flat row (flat contract
		// R2/R4); the refusal carries only the requested name and slug.
		if !strings.EqualFold(updates.Name, broker.Name) {
			slug := slugify(updates.Name)
			conflict, err := store.RuntimeBrokerNameConflict(ctx, s.store, updates.Name, slug, broker.ID, !broker.IsFlat())
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if conflict != nil {
				writeRuntimeTargetRefusal(w, runtimeBrokerNameConflictRefusal(updates.Name, slug))
				return
			}
		}
		broker.Name = updates.Name
		recordChanged = true
	}
	if updates.Labels != nil {
		// Hub-managed labels are never taken from the client: they are
		// dropped from the request and the stored values, if any, kept.
		labels := stripReservedBrokerLabels(updates.Labels)
		for _, k := range reservedBrokerLabels {
			if v, ok := broker.Labels[k]; ok {
				labels[k] = v
			}
		}
		broker.Labels = labels
		recordChanged = true
	}
	priorHostSA := hostSAClaimOf(broker)

	setGCPHost := updates.GCPHostServiceAccountEmail != nil || updates.GCPHostProjectID != nil
	email, project := broker.GCPHostServiceAccountEmail, broker.GCPHostProjectID
	if updates.GCPHostServiceAccountEmail != nil {
		email = strings.ToLower(*updates.GCPHostServiceAccountEmail)
		if email != "" && !isValidServiceAccountEmail(email) {
			ValidationError(w, "gcpHostServiceAccountEmail must be a valid GCP service account email "+
				"(name@project.iam.gserviceaccount.com)", map[string]interface{}{
				"field": "gcpHostServiceAccountEmail",
			})
			return
		}
	}
	if updates.GCPHostProjectID != nil {
		project = *updates.GCPHostProjectID
	}

	// A changed host service account needs actAs for the requesting user on
	// the new value; an unchanged or cleared one needs no check.
	nextHostSA := hostSAClaim{Email: strings.ToLower(email), ProjectID: project}
	hostSAVerdict := s.evaluateHostSAClaim(ctx, priorHostSA, nextHostSA, SurfaceBrokerHostSAUpdate)
	if hostSAClaimWriteAllowed(priorHostSA, nextHostSA, hostSAVerdict) != nil {
		writeHostSAClaimDenied(w, hostSAVerdict)
		return
	}

	// The GCP host pair is written only through the dedicated store
	// writers: a value set or cleared here is kept by registration and
	// detection until the next update. The writers return the values they
	// replaced, which the audit event records as the old values. The pair
	// is written only over the stored value the actAs decision above was
	// made against (priorHostSA); if a registration or another update
	// changed it in between, nothing is written and the request gets 409.
	var gcpPrev *store.GCPHostSetting
	writeErr := func() error {
		if recordChanged {
			if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
				return err
			}
		}
		if setGCPHost {
			prev, applied, err := s.store.SetRuntimeBrokerGCPHostIfPrior(ctx, broker.ID,
				priorHostSA.Email, priorHostSA.ProjectID, email, project)
			if err != nil {
				return err
			}
			if !applied {
				return ErrHostSAClaimStale
			}
			gcpPrev = &prev
		}
		if updates.GCPHostPinned != nil {
			source := store.GCPHostSourceAdmin
			if !*updates.GCPHostPinned {
				source = ""
			}
			prev, err := s.store.SetRuntimeBrokerGCPHostSource(ctx, broker.ID, source)
			if err != nil {
				return err
			}
			if gcpPrev == nil {
				gcpPrev = &prev
			}
		}
		return nil
	}()

	// The update audit event covers what was applied, also when a later
	// write failed.
	after, err := s.store.GetRuntimeBroker(ctx, broker.ID)
	if err == nil {
		old := before
		if gcpPrev != nil {
			old.GCPHostServiceAccountEmail, old.GCPHostProjectID, old.GCPHostSource = gcpPrev.Email, gcpPrev.Project, gcpPrev.Source
		} else {
			// No GCP host writer ran: any difference is not this update's.
			old.GCPHostServiceAccountEmail, old.GCPHostProjectID, old.GCPHostSource = after.GCPHostServiceAccountEmail, after.GCPHostProjectID, after.GCPHostSource
		}
		if details := brokerSettingsChangeDetails(&old, after); details != nil {
			LogBrokerSettingsEvent(ctx, s.auditLogger, after.ID, after.Name, actor.ID(), getClientIP(r),
				mergeBrokerAuditDetails(brokerAuditCredentialDetails(ctx), details...))
		}
	}
	if errors.Is(writeErr, ErrHostSAClaimStale) {
		Conflict(w, "the broker's host service account changed during the update; retry")
		return
	}
	if writeErr != nil {
		writeErrorFromErr(w, writeErr, "")
		return
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// The caller holds broker.update (checked above).
	writeJSON(w, http.StatusOK, withHostIdentity(brokerForDisplay(*after), true))
}

func (s *Server) deleteRuntimeBroker(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Deregistering a broker is a governance action that user access tokens
	// cannot take (GOV_PENDING); broker.delete is then checked on the record.
	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		return
	}

	// Get broker info before deletion for authz and audit logging
	broker, err := s.store.GetRuntimeBroker(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A test identity with an admin role deregisters only a broker it
	// registered (test-admin policy).
	if broker.CreatedBy != actor.ID() && s.testAdminPolicyRefuses(ctx, "handlers_runtime_brokers.go:deleteRuntimeBroker") {
		Forbidden(w)
		return
	}

	// Enforce authorization: only the broker owner or admins can delete
	if !s.authorize(w, r, brokerResource(broker), ActionDelete) {
		return
	}

	// Attribution for the audit events below, read from the identity that
	// passed the checks above.
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
	credentialDetails := brokerAuditCredentialDetails(ctx)
	if projects, err := s.store.GetBrokerProjects(ctx, id); err == nil {
		for _, gp := range projects {
			_ = s.store.RemoveProjectProvider(ctx, gp.ProjectID, id)
			LogUnlinkEvent(ctx, s.auditLogger, id, gp.ProjectID, actorID, clientIP, credentialDetails)

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
	s.forgetBrokerBackfill(id)

	// Clean up the broker's HMAC secret (best-effort, post-delete).
	if err := s.store.DeleteBrokerSecret(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "failed to delete broker secret during deregistration", "brokerId", id, "error", err)
	}
	s.conduitBrokerCredentialChanged(id)
	// Clean up any unconsumed join token (best-effort, post-delete).
	if err := s.store.DeleteJoinToken(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "failed to delete broker join token during deregistration", "brokerId", id, "error", err)
	}

	// Log the deregistration event
	LogDeregisterEvent(ctx, s.auditLogger, id, brokerName, actorID, clientIP)

	w.WriteHeader(http.StatusNoContent)
}

// reportBrokerDispatchCapability adds the dispatch action to caps when the
// caller may have an agent dispatched to broker: canUseBrokerForProject when
// a project is given, canDispatchToBroker otherwise. The capability then
// matches the decision agent creation makes, including auto-provide brokers
// and providers shared with the owner's consent.
func (s *Server) reportBrokerDispatchCapability(ctx context.Context, caps *Capabilities, broker *store.RuntimeBroker, project *store.Project) {
	if caps == nil || broker == nil || capabilityAllows(caps, ActionDispatch) {
		return
	}
	var allowed bool
	if project != nil {
		allowed = s.canUseBrokerForProject(ctx, broker, project)
	} else {
		allowed = s.canDispatchToBroker(ctx, broker)
	}
	if allowed {
		caps.Actions = append(caps.Actions, string(ActionDispatch))
	}
}

// checkBrokerDispatchAccess verifies that the caller may have an agent in
// project dispatched to the given broker. Returns true if access is granted.
// If denied, it writes a 403 response and returns false. If the broker cannot
// be found, it writes an error and returns false.
//
// The decision is canUseBrokerForProject's, called rather than restated: the
// rule lives in one place (brokerDispatchAllowed), and this wrapper only adds
// the response (ptone/scion#591).
func (s *Server) checkBrokerDispatchAccess(ctx context.Context, w http.ResponseWriter, brokerID string, project *store.Project) bool {
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if !s.canUseBrokerForProject(ctx, broker, project) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to create agents on this broker", nil)
		return false
	}
	return true
}

// writeBrokerDispatchForbidden writes the dispatch authorization denial
// (canDispatchToBroker returned false). Every caller of canDispatchToBroker
// that answers the request uses it, so the response cannot drift.
func writeBrokerDispatchForbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"You don't have permission to create agents on this broker", nil)
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
	// ProfileAttach refreshes the Attach field of the broker's stored
	// profiles (see hubclient.BrokerHeartbeat.ProfileAttach). Omitted by
	// an older broker, in which case the stored profiles are left
	// unchanged.
	ProfileAttach []brokerProfileAttach `json:"profileAttach,omitempty"`
	// ProfileSAMappings refreshes the GSA mappings of the broker's stored
	// Kubernetes profiles (see hubclient.ProfileSAMappingsState). Omitted
	// by an older broker, or when unchanged since the broker last sent it,
	// in which case the stored mappings are left unchanged.
	ProfileSAMappings []brokerProfileSAMappings `json:"profileSAMappings,omitempty"`
	// ProfileSAMappingsHashes confirms the stored reports by hash (see
	// hubclient.BrokerHeartbeat.ProfileSAMappingsHashes). Omitted by an
	// older broker.
	ProfileSAMappingsHashes []brokerProfileSAMappingsHash `json:"profileSAMappingsHashes,omitempty"`
	// StartsInFlight lists the agent starts still running on the broker
	// (see hubclient.BrokerHeartbeat.StartsInFlight). Trusted to be complete
	// only when Capabilities.StartsInFlight is set.
	StartsInFlight []brokerStartInFlight `json:"startsInFlight,omitempty"`
	// DefaultProfile refreshes the broker's stored default profile name
	// (see hubclient.BrokerHeartbeat.DefaultProfile). Omitted by an older
	// broker, in which case the stored value is left unchanged.
	DefaultProfile *string `json:"defaultProfile,omitempty"`
	// Health is the broker's report of its own health (see
	// hubclient.BrokerHeartbeat.Health). It is stored normalised to fixed
	// values (api.NormalizeBrokerHealthReport) and never changes the
	// broker's status.
	// Omitted by an older broker, in which case the stored value is left
	// unchanged.
	Health *api.BrokerHealthReport `json:"health,omitempty"`
	// RuntimeTargetPresentation is a flat instance's full FC4-a
	// presentation (see hubclient.BrokerHeartbeat.RuntimeTargetPresentation),
	// with its fence: the instance's runtime target ID, and the reporting
	// process's epoch and per-process sequence. It is stored only when the
	// request identity is the broker itself (ApplyRuntimeTargetPresentation); the wire
	// carries no hash. Omitted by an older broker or a legacy one, in which
	// case the stored value is left unchanged.
	RuntimeTargetPresentation *api.RuntimeTargetPresentation `json:"runtimeTargetPresentation,omitempty"`
	PresentationTargetID      string                         `json:"presentationTargetId,omitempty"`
	PresentationEpoch         int64                          `json:"presentationEpoch,omitempty"`
	PresentationSeq           int64                          `json:"presentationSeq,omitempty"`
}

// brokerStartInFlight mirrors hubclient.StartInFlight.
type brokerStartInFlight struct {
	ProjectID string `json:"projectId"`
	Slug      string `json:"slug"`
}

// brokerProfileAttach is one profile's attach capability in a heartbeat.
type brokerProfileAttach struct {
	Name   string `json:"name"`
	Attach bool   `json:"attach"`
}

// applyProfileAttach sets the Attach field of each stored profile named in
// reported to the reported value, and reports whether any stored value
// changed. Only profiles already registered are updated: a reported name
// with no stored profile is ignored (the profile set itself is still
// recorded at registration), and a stored profile the heartbeat does not
// name keeps its value, which may be nil (read as supported).
func applyProfileAttach(profiles []store.BrokerProfile, reported []brokerProfileAttach) bool {
	if len(reported) == 0 || len(profiles) == 0 {
		return false
	}
	byName := make(map[string]bool, len(reported))
	for _, r := range reported {
		byName[r.Name] = r.Attach
	}
	changed := false
	for i := range profiles {
		v, ok := byName[profiles[i].Name]
		if !ok {
			continue
		}
		if profiles[i].Attach != nil && *profiles[i].Attach == v {
			continue
		}
		profiles[i].Attach = boolPtr(v)
		changed = true
	}
	return changed
}

// brokerProfileSAMappings is one profile's GCP service account report in a
// heartbeat (see hubclient.ProfileSAMappingsState).
type brokerProfileSAMappings struct {
	Name                   string                         `json:"name"`
	ServiceAccountMappings []store.BrokerProfileSAMapping `json:"serviceAccountMappings"`
	Complete               bool                           `json:"complete,omitempty"`
	IncompleteReason       string                         `json:"incompleteReason,omitempty"`
	AmbiguousGSAs          []string                       `json:"ambiguousGSAs,omitempty"`
	ReportVersion          int                            `json:"reportVersion,omitempty"`
}

// brokerProfileSAMappingsHash is one profile's report hash in a heartbeat
// (see hubclient.ProfileSAMappingsHash).
type brokerProfileSAMappingsHash struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// brokerHeartbeatResponse is the heartbeat reply (see
// hubclient.BrokerHeartbeatResponse).
type brokerHeartbeatResponse struct {
	ProfileSAMappingsHashes    bool `json:"profileSAMappingsHashes,omitempty"`
	ProfileSAMappingsRequested bool `json:"profileSAMappingsRequested,omitempty"`
}

// profileSAReportConfirmInterval is the least time between two writes that
// only move a profile's MappingsReportedAt forward because a heartbeat's
// hash confirmed the stored report, so confirmations do not write the
// broker row on every heartbeat.
const profileSAReportConfirmInterval = 5 * time.Minute

// applyProfileSAMappings applies a heartbeat's GCP service account reports
// (full reports and per-profile hashes) to the broker's stored profiles. It
// reports whether anything changed, and whether the Hub needs the full
// report because a hash does not match the stored report.
//
//   - A profile with a full report gets it stored, with the profile's hash
//     from the same heartbeat (empty from a broker that sends no hashes),
//     and MappingsReportedAt set to now.
//   - A profile with only a hash that matches the stored report has its
//     MappingsReportedAt moved to now, at most every
//     profileSAReportConfirmInterval. A hash that does not match (or no
//     stored hash) leaves the stored report as is, to age, and asks for the
//     full report.
//   - The reports and hashes name every Kubernetes profile of the broker,
//     so a stored profile named in neither (for example one switched from
//     Kubernetes to docker) has its report cleared.
//
// Like applyProfileAttach, a reported name with no stored profile is
// ignored, a broker with no stored profiles (a flat Runtime Broker row)
// gets nothing, and a heartbeat with neither field (an older broker, or a
// broker that could not read its settings) changes nothing.
func applyProfileSAMappings(profiles []store.BrokerProfile, reported []brokerProfileSAMappings, hashes []brokerProfileSAMappingsHash, now time.Time) (changed, requested bool) {
	if (len(reported) == 0 && len(hashes) == 0) || len(profiles) == 0 {
		return false, false
	}
	byName := make(map[string]brokerProfileSAMappings, len(reported))
	for _, r := range reported {
		byName[r.Name] = r
	}
	hashByName := make(map[string]string, len(hashes))
	for _, h := range hashes {
		hashByName[h.Name] = h.Hash
	}
	confirm := func(p *store.BrokerProfile) {
		if p.MappingsReportedAt == nil || now.Sub(*p.MappingsReportedAt) >= profileSAReportConfirmInterval {
			p.MappingsReportedAt = timePtr(now)
			changed = true
		}
	}
	for i := range profiles {
		p := &profiles[i]
		r, inReport := byName[p.Name]
		hash, inHashes := hashByName[p.Name]
		switch {
		case inReport:
			if profileSAReportEqual(p, r, hash) {
				confirm(p)
				continue
			}
			p.ServiceAccountMappings = r.ServiceAccountMappings
			if len(p.ServiceAccountMappings) == 0 {
				p.ServiceAccountMappings = nil
			}
			p.MappingsReported = true
			p.MappingsComplete = r.Complete
			p.MappingsIncompleteReason = r.IncompleteReason
			if r.Complete {
				p.MappingsIncompleteReason = ""
			}
			p.AmbiguousGSAs = r.AmbiguousGSAs
			if len(p.AmbiguousGSAs) == 0 {
				p.AmbiguousGSAs = nil
			}
			p.MappingsHash = hash
			p.MappingsReportVersion = r.ReportVersion
			p.MappingsReportedAt = timePtr(now)
			changed = true
		case inHashes:
			if p.MappingsReported && p.MappingsHash != "" && p.MappingsHash == hash {
				confirm(p)
				continue
			}
			requested = true
		default:
			if p.MappingsReported || len(p.ServiceAccountMappings) > 0 || p.MappingsReportedAt != nil {
				clearProfileSAReport(p)
				changed = true
			}
		}
	}
	return changed, requested
}

// profileSAReportEqual reports whether p already stores report r with hash.
func profileSAReportEqual(p *store.BrokerProfile, r brokerProfileSAMappings, hash string) bool {
	reason := r.IncompleteReason
	if r.Complete {
		reason = ""
	}
	return p.MappingsReported &&
		p.MappingsHash == hash &&
		p.MappingsComplete == r.Complete &&
		p.MappingsIncompleteReason == reason &&
		p.MappingsReportVersion == r.ReportVersion &&
		slices.Equal(p.ServiceAccountMappings, r.ServiceAccountMappings) &&
		slices.Equal(p.AmbiguousGSAs, r.AmbiguousGSAs)
}

// clearProfileSAReport removes a profile's stored report, back to unknown.
func clearProfileSAReport(p *store.BrokerProfile) {
	p.ServiceAccountMappings = nil
	p.MappingsReported = false
	p.MappingsComplete = false
	p.MappingsIncompleteReason = ""
	p.AmbiguousGSAs = nil
	p.MappingsHash = ""
	p.MappingsReportedAt = nil
	p.MappingsReportVersion = 0
}

// timePtr returns a pointer to a copy of t.
func timePtr(t time.Time) *time.Time {
	return &t
}

// boolPtr returns a pointer to a copy of b.
func boolPtr(b bool) *bool {
	return &b
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
	ExitReason      string `json:"exitReason,omitempty"`  // Terminal reason: "crashed", "limits_exceeded", "preempted", "evicted", or "oom_killed" (see state.ExitReason)
	// RunID is the entry's scion.run_id label (hubclient.AgentHeartbeat.RunID);
	// empty for an unlabelled entry or an older broker. OtherEntries are
	// the agent's entries on the broker's other targets
	// (hubclient.AgentHeartbeat.OtherEntries). Read only by the
	// stranded-run adoption (broker_heartbeat_run_adoption.go) and the
	// store's run check (AgentStatusUpdate.ReportRunID).
	RunID        string                    `json:"runId,omitempty"`
	OtherEntries []brokerAgentRuntimeEntry `json:"otherEntries,omitempty"`
}

// brokerAgentRuntimeEntry mirrors hubclient.AgentRuntimeEntry.
type brokerAgentRuntimeEntry struct {
	RuntimeTarget string `json:"runtimeTarget,omitempty"`
	RunID         string `json:"runId,omitempty"`
}

// heartbeatBrokerIsFlat reports whether the Runtime Broker sending a
// heartbeat is flat. A broker that cannot be read is treated as before (not
// flat), so legacy heartbeats are unchanged.
func heartbeatBrokerIsFlat(load func() (*store.RuntimeBroker, error)) bool {
	b, err := load()
	return err == nil && b.IsFlat()
}

// authorizeBrokerHeartbeatUser decides the user path of the broker
// heartbeat. Only the broker itself heartbeats as a matter of course; a user
// may do so only on an interactive session or dev credential
// (HOST_OPERATIONS) and only with broker.update granted on the broker by a
// role binding. The check names the broker by ID without reading its row,
// so the owner relationship does not apply here and a heartbeat reads the
// broker row only when its own processing needs it. It writes the response
// and returns false when the caller is refused.
func (s *Server) authorizeBrokerHeartbeatUser(w http.ResponseWriter, r *http.Request, id string) bool {
	ctx := r.Context()
	resource := Resource{Type: permissions.ResourceBroker, ID: id}
	user, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonHostOperations)
	if !ok {
		logAuthzDenial(r, GetIdentityFromContext(ctx), resource, ActionUpdate, "heartbeat user path refused")
		return false
	}
	decision := s.authzService.CheckAccess(ctx, user, resource, ActionUpdate)
	if !decision.Allowed {
		logAuthzDenial(r, user, resource, ActionUpdate, decision.Reason)
		Forbidden(w)
		return false
	}
	return true
}

func (s *Server) handleBrokerHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Authorize: the broker itself, or a user on an interactive session or
	// dev credential holding broker.update on the broker (heartbeats modify
	// agent state and broker liveness). A user access token is refused with
	// the HOST_OPERATIONS session-only reason.
	brokerSelf := false
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == id {
		// Broker sending its own heartbeat — allowed. brokerSelf is set
		// only when the request identity is the broker itself.
		if self, ok := GetIdentityFromContext(ctx).(BrokerIdentity); ok && self.BrokerID() == id {
			brokerSelf = true
		}
	} else if !s.authorizeBrokerHeartbeatUser(w, r, id) {
		return
	}

	var heartbeat brokerHeartbeatRequest
	if err := readJSON(r, &heartbeat); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	// This Hub reads the report hashes and asks for the full report when
	// a hash does not match (applyProfileSAMappings).
	heartbeatResp := brokerHeartbeatResponse{ProfileSAMappingsHashes: true}

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
	// loadHeartbeatBroker the first time an agent's Profile backfill needs its
	// flat-broker check (ptone/scion#2262). Current brokers report
	// Capabilities on every heartbeat, so this is normally the one read the
	// Capabilities refresh already makes, and the Profile backfill reuses it
	// rather than adding another. A heartbeat without Capabilities reads the
	// broker only if some agent needs a Profile backfill. The error (if any) is cached
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
	// WorkspaceStorage and DefaultProfile follow the same rule, so the hub
	// sees share health and default-profile changes within one heartbeat.
	// All are persisted in a single update, and only when something
	// changed.
	//
	// Keeping an omitted descriptor means a broker downgraded to a version
	// that does not report one keeps its last descriptor, Healthy included,
	// indefinitely. That is safe because move eligibility never relies on
	// the descriptor alone: the capability check reads AgentMove from the
	// Capabilities every heartbeat refreshes (an old broker reports none),
	// and the target health check also probes live reachability.
	// ProfileAttach follows the same rule: only profiles the heartbeat
	// names are updated, and only when their stored Attach differs.
	// ProfileSAMappings likewise. Health (the broker's self-reported
	// health) follows the same rule; it is stored next to Status and never
	// changes it, so a degraded broker stays online and keeps reconciling.
	if heartbeat.Capabilities != nil || heartbeat.WorkspaceStorage != nil || heartbeat.DefaultProfile != nil || heartbeat.Health != nil || len(heartbeat.ProfileAttach) > 0 || len(heartbeat.ProfileSAMappings) > 0 || len(heartbeat.ProfileSAMappingsHashes) > 0 {
		if broker, err := loadHeartbeatBroker(); err != nil {
			s.agentLifecycleLog.Warn("heartbeat: failed to load broker to refresh broker state",
				"broker_id", id, "error", err)
		} else {
			// A flat row never stores Runtime Broker Profiles: a reported
			// default profile, profile attach or profile SA mappings are
			// dropped before the write.
			if broker.IsFlat() && (heartbeat.DefaultProfile != nil || len(heartbeat.ProfileAttach) > 0 || len(heartbeat.ProfileSAMappings) > 0 || len(heartbeat.ProfileSAMappingsHashes) > 0) {
				s.agentLifecycleLog.Warn("heartbeat: ignoring Runtime Broker Profile fields reported for a flat Runtime Broker",
					"broker_id", id)
				heartbeat.DefaultProfile = nil
				heartbeat.ProfileAttach = nil
				heartbeat.ProfileSAMappings = nil
				heartbeat.ProfileSAMappingsHashes = nil
			}
			// A broker older than templateTiers ignores the telemetry tiers
			// dispatch sends; warn once (ptone/scion#4224).
			s.warnBrokerWithoutTemplateTiers(id, broker.Name, heartbeat.Capabilities)
			changed := false
			if heartbeat.Capabilities != nil && !reflect.DeepEqual(broker.Capabilities, heartbeat.Capabilities) {
				broker.Capabilities = heartbeat.Capabilities
				changed = true
			}
			if heartbeat.WorkspaceStorage != nil && !reflect.DeepEqual(broker.WorkspaceStorage, heartbeat.WorkspaceStorage) {
				broker.WorkspaceStorage = heartbeat.WorkspaceStorage
				changed = true
			}
			if heartbeat.DefaultProfile != nil && broker.DefaultProfile != *heartbeat.DefaultProfile {
				broker.DefaultProfile = *heartbeat.DefaultProfile
				changed = true
			}
			if heartbeat.Health != nil {
				if health := api.NormalizeBrokerHealthReport(heartbeat.Health); !reflect.DeepEqual(broker.Health, health) {
					// Logged only when the stored report changes, so at
					// most once per distinct report, with counts only.
					if n := countBrokerHealthNormalization(heartbeat.Health, health); n.any() {
						s.agentLifecycleLog.Info("heartbeat: broker health report normalised",
							"broker_id", id,
							"dropped_checks", n.DroppedChecks,
							"unrecognised_values", n.UnrecognisedValues,
							"unrecognised_status", n.UnrecognisedStatus)
					}
					broker.Health = health
					changed = true
				}
			}
			if applyProfileAttach(broker.Profiles, heartbeat.ProfileAttach) {
				changed = true
			}
			saChanged, saRequested := applyProfileSAMappings(broker.Profiles, heartbeat.ProfileSAMappings, heartbeat.ProfileSAMappingsHashes, time.Now())
			if saChanged {
				changed = true
			}
			heartbeatResp.ProfileSAMappingsRequested = saRequested
			if changed {
				if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
					s.agentLifecycleLog.Warn("heartbeat: failed to persist refreshed broker state",
						"broker_id", id, "error", err)
				}
			}
		}
	}

	// A flat instance's presentation is stored only when the request is
	// authenticated as the broker itself (write rule 1): the user path,
	// which this handler also admits, never writes it, so the presentation
	// stays read-only to Hub users. Rules 2-4 (flat row, experiment on,
	// target bound, fence advances) and the Hub-computed hash are
	// ApplyRuntimeTargetPresentation's. An omitted field keeps the stored
	// value.
	if !brokerSelf {
		if heartbeat.RuntimeTargetPresentation != nil {
			s.agentLifecycleLog.Warn("heartbeat: ignoring runtimeTargetPresentation sent with a request identity other than the broker itself",
				"broker_id", id)
		}
	} else if _, err := s.ApplyRuntimeTargetPresentation(ctx, id, heartbeat.PresentationTargetID,
		heartbeat.RuntimeTargetPresentation, heartbeat.PresentationEpoch, heartbeat.PresentationSeq, time.Now()); err != nil {
		s.agentLifecycleLog.Warn("heartbeat: failed to store runtime target presentation",
			"broker_id", id, "error", err)
	}

	// Process agent status updates from each project
	report := newHeartbeatReport()
	// runEntries feeds the stranded-run adoption: the run labels reported
	// for each known agent.
	runEntries := newHeartbeatRunEntries()
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
				runEntries.addOffBroker(agent, agentHB)
				report.unresolvedSlugs[agentHB.Slug] = true
				slog.Warn("Broker attempted to update agent owned by different broker",
					"brokerID", id,
					"agentBrokerID", agent.RuntimeBrokerID,
					"agent_id", agent.ID)
				continue
			}
			report.present[agent.ID] = true
			runEntries.addOwn(agent, agentHB)
			report.observed[agent.ID] = observedAgent{target: agentHB.RuntimeTarget, state: heartbeatObservedState(agentHB)}

			// Build status update with agent status and container status.
			// When the broker sends structured Phase/Activity fields, use
			// them directly. Fall back to container-status derivation for
			// backward compatibility with older brokers.
			// ReportRunID lets the store drop a report of an older run
			// (ptone/scion#4853); Result tells this handler whether the
			// write halted the agent.
			var statusResult store.AgentStatusResult
			statusUpdate := store.AgentStatusUpdate{
				ContainerStatus: agentHB.ContainerStatus,
				Heartbeat:       true, // Ensures LastSeen is updated
				Message:         agentHB.Message,
				ReportSource:    store.StatusReportSourceRuntime,
				ReportRunID:     agentHB.RunID,
				Result:          &statusResult,
			}
			// A stop queued while the broker was offline keeps its
			// stop_queued status and notice while it is still the agent's
			// intent, until the stop is applied or the container is
			// confirmed gone: any report of the agent means it has not been
			// applied yet. A start recorded since supersedes the queued
			// stop, and the report then applies as usual.
			if agent.ContainerStatus == containerStatusStopQueued && agent.RunIntent == store.RunIntentStopped {
				statusUpdate.ContainerStatus = ""
				statusUpdate.Message = ""
			}

			// Guard: a heartbeat must never revert an agent out of a
			// terminal phase (stopped/failed) that was set by an explicit
			// lifecycle action. Only start/restart handlers may
			// transition away from these phases. Without this guard a
			// forced heartbeat fired immediately after a stop dispatch
			// can race and overwrite the stopped state with stale
			// container data.
			agentInTerminalPhase := agent.Phase == string(state.PhaseStopped) ||
				agent.Phase == string(state.PhaseError) ||
				agent.Phase == string(state.PhaseHalted)

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
					// only when no platform-observed reason is stored yet
					// (a reason the agent reported itself does not block
					// it), and leave the phase itself untouched.
					hbExitReason := state.ExitReason(agentHB.ExitReason)
					isDisruption := hbExitReason == state.ExitReasonPreempted || hbExitReason == state.ExitReasonEvicted
					if isDisruption && !store.IsHaltEvidence(agent.ExitReason) {
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
					// An OOM kill is a crash, so the agent is usually already
					// in error when the heartbeat that carries it arrives:
					// its own crash report (for example when only the
					// harness process was killed) or an earlier heartbeat
					// recorded a plain crash. Record the more specific
					// reason over any reason that is not platform-observed
					// (store.IsHaltEvidence), keep the phase, and replace
					// only a message that says nothing more.
					if hbExitReason == state.ExitReasonOOMKilled &&
						!store.IsHaltEvidence(agent.ExitReason) {
						statusUpdate.ExitReason = agentHB.ExitReason
						if agentHB.ExitCode != nil {
							statusUpdate.ExitCode = agentHB.ExitCode
						}
						if isGenericStopMessage(agent.Message) || isCrashExitCodeMessage(agent.Message) {
							exitCode := agentHB.ExitCode
							if exitCode == nil {
								exitCode = agent.ExitCode
							}
							statusUpdate.Message = exitStatusMessage(hbExitReason, exitCode)
						}
					}
					// The agent's own clean-stop report usually reaches the
					// Hub before the heartbeat that carries the disruption,
					// leaving the agent stopped. The broker reports error for
					// a disruption whose workspace did not survive the pod
					// (see k8sDisruptionPhase in pkg/runtime), so a later start
					// cannot resume it: apply that error over the plain stop
					// (ptone/scion#2669). Never over a stop the user asked for
					// (run intent stopped), and never over a different
					// recorded reason.
					if isDisruption && agentHB.Phase == string(state.PhaseError) &&
						agent.Phase == string(state.PhaseStopped) &&
						agent.RunIntent != store.RunIntentStopped &&
						(!store.IsHaltEvidence(agent.ExitReason) || agent.ExitReason == agentHB.ExitReason) {
						statusUpdate.Phase = string(state.PhaseError)
						statusUpdate.ExitReason = agentHB.ExitReason
						if agentHB.ExitCode != nil {
							statusUpdate.ExitCode = agentHB.ExitCode
						}
						if isGenericStopMessage(agent.Message) {
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
						// A preempted or evicted pod whose workspace survives is
						// reported stopped even though its container was killed
						// with a non-zero code (see k8sDisruptionPhase in
						// pkg/runtime); the broker's phase is authoritative then.
						hbReason := state.ExitReason(agentHB.ExitReason)
						hbDisruption := hbReason == state.ExitReasonPreempted || hbReason == state.ExitReasonEvicted
						// A stop the user asked for (run intent stopped, its
						// stopped status not yet written) that races a
						// disruption ends stopped, keeping the reason.
						if hbDisruption && hbPhase == state.PhaseError && agent.RunIntent == store.RunIntentStopped {
							hbPhase = state.PhaseStopped
							agentHB.Phase = string(state.PhaseStopped)
						}
						if agentHB.ExitCode != nil && *agentHB.ExitCode != 0 {
							// crash path
							if hbPhase == state.PhaseStopped && !hbDisruption {
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
						// Starting -> running clears the prior run's exit fields in the store.
						if isDisruption && (!store.IsHaltEvidence(agent.ExitReason) ||
							(curPhase == state.PhaseStarting && hbPhase == state.PhaseRunning)) {
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
				if isDisruption && (!store.IsHaltEvidence(agent.ExitReason) ||
					(agent.Phase == string(state.PhaseStarting) && statusUpdate.Phase == string(state.PhaseRunning))) {
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
			// No Runtime Broker-reported profile is written onto a pinned
			// (flat) agent, or onto any agent of a flat Runtime Broker.
			// The broker row is read last, only when a backfill would
			// otherwise happen.
			if agentHB.Profile != "" && !agent.IsPinned() && (agent.AppliedConfig == nil || agent.AppliedConfig.Profile == "") && !heartbeatBrokerIsFlat(loadHeartbeatBroker) {
				if agent.AppliedConfig == nil {
					agent.AppliedConfig = &store.AgentAppliedConfig{}
				}
				agent.AppliedConfig.Profile = agentHB.Profile
				needsUpdate = true
			}
			// Runtime is not backfilled here. The halt rule trusts the
			// runtime recorded on the agent, so only the broker's own
			// reports write it (the dispatch response, the launch report,
			// a reincarnation move); a guess resolved from the profile is
			// display-only (enrichAgentRuntime) (ptone/scion#4853).
			if needsUpdate {
				if err := s.store.UpdateAgent(ctx, agent); err != nil {
					slog.Warn("Failed to backfill agent config from heartbeat",
						"agent_id", agent.ID, "harnessAuth", agentHB.HarnessAuth, "profile", agentHB.Profile,
						"error", err)
				}
			}
			s.recordHeartbeatRuntimeTarget(ctx, agent, agentHB.RuntimeTarget)

			// While a lifecycle dispatch runs, keep the phase the lifecycle
			// path owns; the rest of the report still applies
			// (ptone/scion#2014).
			if s.heartbeatPhaseGuarded(agent, statusUpdate.Phase) {
				statusUpdate.Phase = ""
			}

			// Reconcile the max_agents_per_broker reservation against the
			// phase this heartbeat will actually persist — e.g. release on an
			// observed crash/exit, or best-effort re-reserve on an observed
			// out-of-band restart (ptone/scion#1963). A report of another
			// run changes nothing (the store's run check), so it leaves the
			// reservation alone too.
			if !reportOfOtherRun(agentHB.RunID, agent.RunID) {
				s.reconcileBrokerQuotaOnPhaseChange(ctx, agent, agent.Phase, statusUpdate.Phase)
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
				// Publish SSE event so the frontend receives activity updates.
				// A row soft-deleted between the slug lookup and this re-read
				// (a delete finishing concurrently) publishes nothing.
				if updated, err := s.store.GetAgent(ctx, agent.ID); err == nil && updated.DeletedAt.IsZero() {
					s.events.PublishAgentStatus(ctx, updated)
					if statusResult.Halted {
						s.onAgentHalted(ctx, updated, statusResult.HaltedFrom)
					}
				}
			}
		}
	}

	// Adopt a stranded run: a row whose run no container holds while this
	// broker reports the agent's entry carrying another run the hub minted
	// for it (broker_heartbeat_run_adoption.go).
	s.adoptStrandedRuns(ctx, id, &heartbeat, runEntries, report)

	// Reconcile running agents of this broker that the heartbeat no longer
	// reports (their container is gone). Gated on a complete inventory and a
	// fresh broker; see broker_heartbeat_reconcile.go.
	s.reconcileMissingAgents(ctx, id, prevBroker, &heartbeat, report)

	// Record runtime observations for start claims, once, outside the
	// per-agent loop and in a single store transaction.
	s.recordRecoveryObservations(ctx, id, prevBroker, &heartbeat, report)

	// Stop agents that run although their run intent is stopped (a start
	// that reached the broker after a stop).
	s.stopAgentsRunningWithIntentStopped(ctx, id, prevBroker, &heartbeat, report)

	// Settle queued stops of a broker reached over HTTP only: confirm the
	// ones its complete inventory shows terminated, and apply the rest.
	s.settleQueuedStops(ctx, id, prevBroker, &heartbeat, report)
	s.drainQueuedStopsFromHeartbeat(ctx, id, prevBroker, &heartbeat, report)

	// Retry a registration reservation backfill that skipped an agent on this
	// replica (BackfillBrokerOnRegistration). Without a pending one this reads
	// nothing: a heartbeat that needs nothing does not read the broker row.
	s.retryBrokerBackfill(ctx, id)

	writeJSON(w, http.StatusOK, heartbeatResp)
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

// isCrashExitCodeMessage reports whether msg is the default plain-crash
// message exitStatusMessage gives ("Agent crashed with exit code N"), which a
// more specific reason (oom_killed) may replace.
func isCrashExitCodeMessage(msg string) bool {
	code, ok := strings.CutPrefix(msg, "Agent crashed with exit code ")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(code)
	return err == nil
}

// exitStatusMessage returns the default human-readable status Message for a
// terminal agent, derived from the resolved ExitReason and the structured
// ExitCode reported by the broker. Kubernetes pod disruptions and OOM kills
// get their own wording so a preempted, evicted or OOM-killed agent is not
// reported as a generic crash; every other reason (including the empty one) keeps the existing
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
	case state.ExitReasonOOMKilled:
		if hasNonZeroExit {
			return fmt.Sprintf("Agent container was killed for exceeding its memory limit, exit code %d", *exitCode)
		}
		return "Agent container was killed for exceeding its memory limit"
	default:
		if hasNonZeroExit {
			return fmt.Sprintf("Agent crashed with exit code %d", *exitCode)
		}
		return ""
	}
}
