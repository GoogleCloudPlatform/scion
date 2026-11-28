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
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
)

const (
	maxProjectSystemOneInstancesBodyBytes = 32 * 1024
	projectSystemOneManageRuleMessage     = "Only active project owners, project administrators, or Hub administrators can manage a project's System One instance allowlist"
)

// ProjectSystemOneInstancesResponse is the response body for
// GET/PUT /api/v1/projects/{id}/system-one/instances and
// PUT/DELETE /api/v1/projects/{id}/system-one/instances/{name}.
type ProjectSystemOneInstancesResponse struct {
	ProjectID string   `json:"projectId"`
	Instances []string `json:"instances"`
}

// ProjectSystemOneAvailableInstanceSummary is the credential-free, read-only
// discovery projection of a Hub System One provider instance returned by
// GET /api/v1/projects/{id}/system-one/available-instances.
// It intentionally omits endpoints, auth metadata, secret names/values,
// options, limits, and internal IDs.
type ProjectSystemOneAvailableInstanceSummary struct {
	Name                     string                         `json:"name"`
	Adapter                  string                         `json:"adapter"`
	Model                    string                         `json:"model,omitempty"`
	Enabled                  bool                           `json:"enabled"`
	QuestionTypes            []string                       `json:"questionTypes"`
	DistributionCompleteness map[string]string              `json:"distributionCompleteness"`
	NativeMetrics            []systemoneprovider.MetricDecl `json:"nativeMetrics"`
	Abstains                 bool                           `json:"abstains"`
}

// ProjectSystemOneAvailableInstancesResponse is the response body for
// GET /api/v1/projects/{id}/system-one/available-instances.
type ProjectSystemOneAvailableInstancesResponse struct {
	ProjectID string                                     `json:"projectId"`
	Instances []ProjectSystemOneAvailableInstanceSummary `json:"instances"`
}

// SetProjectSystemOneInstancesRequest is the request body for
// PUT /api/v1/projects/{id}/system-one/instances.
// Instances is a pointer so missing "instances" (e.g. `{}`) is rejected as a
// validation error rather than silently clearing the allowlist; pass
// `{"instances": []}` to clear explicitly.
type SetProjectSystemOneInstancesRequest struct {
	Instances *[]string `json:"instances"`
}

// handleProjectSystemOneInstances routes:
//   - GET /api/v1/projects/{id}/system-one/instances
//   - PUT /api/v1/projects/{id}/system-one/instances
//   - PUT /api/v1/projects/{id}/system-one/instances/{name}
//   - DELETE /api/v1/projects/{id}/system-one/instances/{name}
//
// All routes return 404 when the hub.system_one experiment is disabled.
func (s *Server) handleProjectSystemOneInstances(w http.ResponseWriter, r *http.Request, projectID, rest string) {
	if !s.experimentEnabled(experiments.SystemOne) {
		NotFound(w, "route")
		return
	}

	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimSuffix(rest, "/")

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.handleGetProjectSystemOneInstances(w, r, projectID)
		case http.MethodPut:
			s.handleSetProjectSystemOneInstances(w, r, projectID)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
		return
	}

	if strings.Contains(rest, "/") {
		NotFound(w, "route")
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.handleAddProjectSystemOneInstance(w, r, projectID, rest)
	case http.MethodDelete:
		s.handleRemoveProjectSystemOneInstance(w, r, projectID, rest)
	default:
		MethodNotAllowed(w, http.MethodPut, http.MethodDelete)
	}
}

// projectSystemOneUserCredentialAllowed reports whether the request's identity
// and credential kind may read or mutate a project's System One allowlist:
//   - a local user or dev identity with an interactive session or dev credential;
//   - a scoped user identity with a user access token (UAT).
//
// Agents, agent-delegated credentials, broker credentials (including broker
// on-behalf-of), hub-delivery credentials, federated credentials, and visitors
// are refused with 403 (aligned with messagePolicyManageCredentialAllowed).
func projectSystemOneUserCredentialAllowed(ctx context.Context, identity Identity) bool {
	if isNilIdentity(identity) {
		return false
	}
	if _, federated := identity.(FederatedIdentity); federated {
		return false
	}
	kind := requestCredentialKind(ctx, identity)
	if IsScopedUserIdentity(identity) {
		if identity.Type() != "user" {
			return false
		}
		_, ok := identity.(UserIdentity)
		return ok && kind == CredentialKindUAT
	}
	switch identity.Type() {
	case "user", "dev":
		_, ok := identity.(UserIdentity)
		return ok && allowedMutationCredentials[kind]
	default:
		return false
	}
}

// projectSystemOneOwnerOrAdminRule reports whether identity may mutate a
// project's System One instance allowlist: an active project owner, an active
// project admin, or an unscoped Hub/platform admin.
func (s *Server) projectSystemOneOwnerOrAdminRule(ctx context.Context, identity Identity, projectID string) bool {
	if s.testAdminPolicyRefuses(ctx, "handlers_project_system_one_instances.go:projectSystemOneOwnerOrAdminRule") {
		return false
	}
	if !projectSystemOneUserCredentialAllowed(ctx, identity) {
		return false
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		return false
	}
	if IsUnscopedLocalPlatformAdmin(userIdent) {
		return true
	}
	if !IsScopedUserIdentity(userIdent) && s.authzService != nil && s.authzService.IsHubAdmin(ctx, userIdent.ID()) {
		return true
	}
	return s.authzService != nil && s.authzService.isProjectOwnerOrAdmin(ctx, userIdent.ID(), projectID)
}

func (s *Server) authorizeProjectSystemOneRead(w http.ResponseWriter, r *http.Request, project *store.Project) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return false
	}
	res := projectResource(project)
	if !projectSystemOneUserCredentialAllowed(ctx, identity) {
		logAuthzDenial(r, identity, res, ActionRead, "credential or principal kind cannot read project System One instance allowlist")
		writeForbiddenStructured(w, "", res.Type, ActionRead)
		return false
	}
	if s.authzService == nil {
		Forbidden(w)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, res, ActionRead)
	if decision.Allowed {
		return true
	}
	if IsScopedUserIdentity(identity) {
		manageDec := s.authzService.CheckAccess(ctx, identity, res, ActionManageSystemOneInstances)
		if manageDec.Allowed && s.projectSystemOneOwnerOrAdminRule(ctx, identity, project.ID) {
			return true
		}
	}
	logAuthzDenial(r, identity, res, ActionRead, decision.Reason)
	writeForbiddenStructuredDenialCause(w, "", res.Type, ActionRead, decision.DeniedBy, decision.adoptionDetailsCause())
	return false
}

func (s *Server) authorizeProjectSystemOneManage(w http.ResponseWriter, r *http.Request, project *store.Project) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return false
	}
	res := projectResource(project)
	if !projectSystemOneUserCredentialAllowed(ctx, identity) {
		logAuthzDenial(r, identity, res, ActionManageSystemOneInstances, "credential or principal kind cannot manage project System One instance allowlist")
		writeForbiddenStructured(w, projectSystemOneManageRuleMessage, res.Type, ActionManageSystemOneInstances)
		return false
	}
	if !s.authorizeMsg(w, r, res, ActionManageSystemOneInstances, projectSystemOneManageRuleMessage) {
		return false
	}
	if !s.projectSystemOneOwnerOrAdminRule(ctx, identity, project.ID) {
		logAuthzDenial(r, identity, res, ActionManageSystemOneInstances, "caller is not an active project owner, project admin, or local hub admin")
		writeForbiddenStructured(w, projectSystemOneManageRuleMessage, res.Type, ActionManageSystemOneInstances)
		return false
	}
	return true
}

func (s *Server) handleGetProjectSystemOneInstances(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeProjectSystemOneRead(w, r, project) {
		return
	}
	instances, err := s.store.GetProjectSystemOneInstances(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if instances == nil {
		instances = []string{}
	}
	writeJSON(w, http.StatusOK, ProjectSystemOneInstancesResponse{
		ProjectID: project.ID,
		Instances: instances,
	})
}

func (s *Server) handleSetProjectSystemOneInstances(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeProjectSystemOneManage(w, r, project) {
		return
	}

	var req SetProjectSystemOneInstancesRequest
	if !readBoundedJSONStrict(w, r, maxProjectSystemOneInstancesBodyBytes, &req) {
		return
	}
	if req.Instances == nil {
		ValidationError(w, "instances field is required (pass [] to clear the allowlist)", nil)
		return
	}
	instances := *req.Instances
	if len(instances) > store.MaxProjectSystemOneInstances {
		ValidationError(w, fmt.Sprintf("instances exceeds maximum of %d entries", store.MaxProjectSystemOneInstances), nil)
		return
	}
	for _, raw := range instances {
		name := strings.TrimSpace(raw)
		if !store.IsValidSystemOneInstanceName(name) {
			ValidationError(w, fmt.Sprintf("invalid instance name %q: must be a lowercase alphanumeric slug (1..63 characters)", raw), nil)
			return
		}
	}

	before, updated, err := s.store.SetProjectSystemOneInstances(ctx, project.ID, instances)
	if err != nil {
		if errors.Is(err, store.ErrSystemOneInstanceNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "unknown_instance", err.Error(), nil)
			return
		}
		if errors.Is(err, store.ErrSystemOneAllowlistLimitExceeded) {
			ValidationError(w, fmt.Sprintf("instances exceeds maximum of %d entries", store.MaxProjectSystemOneInstances), nil)
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, "invalid instance allowlist", nil)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		slog.Error("failed to set project System One instance allowlist", "project_id", project.ID, "error", err)
		RuntimeError(w, "Failed to update project System One instance allowlist")
		return
	}
	if updated == nil {
		updated = []string{}
	}

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  "project_system_one_instances_set",
		TargetType:    "project",
		TargetID:      project.ID,
		BeforeSummary: strings.Join(before, ","),
		AfterSummary:  strings.Join(updated, ","),
	})

	writeJSON(w, http.StatusOK, ProjectSystemOneInstancesResponse{
		ProjectID: project.ID,
		Instances: updated,
	})
}

func (s *Server) handleAddProjectSystemOneInstance(w http.ResponseWriter, r *http.Request, projectID, instanceName string) {
	ctx := r.Context()
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeProjectSystemOneManage(w, r, project) {
		return
	}

	name := strings.TrimSpace(instanceName)
	if !store.IsValidSystemOneInstanceName(name) {
		ValidationError(w, fmt.Sprintf("invalid instance name %q: must be a lowercase alphanumeric slug (1..63 characters)", instanceName), nil)
		return
	}

	before, updated, err := s.store.AddProjectSystemOneInstance(ctx, project.ID, name)
	if err != nil {
		if errors.Is(err, store.ErrSystemOneInstanceNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "unknown_instance", fmt.Sprintf("System One instance %q does not exist", name), nil)
			return
		}
		if errors.Is(err, store.ErrSystemOneAllowlistLimitExceeded) {
			ValidationError(w, fmt.Sprintf("instances exceeds maximum of %d entries", store.MaxProjectSystemOneInstances), nil)
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, "invalid instance name", nil)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		slog.Error("failed to add project System One instance", "project_id", project.ID, "instance", name, "error", err)
		RuntimeError(w, "Failed to update project System One instance allowlist")
		return
	}
	if updated == nil {
		updated = []string{}
	}

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  "project_system_one_instances_add",
		TargetType:    "project",
		TargetID:      project.ID,
		BeforeSummary: strings.Join(before, ","),
		AfterSummary:  strings.Join(updated, ","),
	})

	writeJSON(w, http.StatusOK, ProjectSystemOneInstancesResponse{
		ProjectID: project.ID,
		Instances: updated,
	})
}

func (s *Server) handleRemoveProjectSystemOneInstance(w http.ResponseWriter, r *http.Request, projectID, instanceName string) {
	ctx := r.Context()
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeProjectSystemOneManage(w, r, project) {
		return
	}

	name := strings.TrimSpace(instanceName)
	if !store.IsValidSystemOneInstanceName(name) {
		ValidationError(w, fmt.Sprintf("invalid instance name %q: must be a lowercase alphanumeric slug (1..63 characters)", instanceName), nil)
		return
	}

	before, updated, err := s.store.RemoveProjectSystemOneInstance(ctx, project.ID, name)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, "invalid instance name", nil)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		slog.Error("failed to remove project System One instance", "project_id", project.ID, "instance", name, "error", err)
		RuntimeError(w, "Failed to update project System One instance allowlist")
		return
	}
	if updated == nil {
		updated = []string{}
	}

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  "project_system_one_instances_remove",
		TargetType:    "project",
		TargetID:      project.ID,
		BeforeSummary: strings.Join(before, ","),
		AfterSummary:  strings.Join(updated, ","),
	})

	writeJSON(w, http.StatusOK, ProjectSystemOneInstancesResponse{
		ProjectID: project.ID,
		Instances: updated,
	})
}

// handleProjectSystemOneAvailableInstances serves
// GET /api/v1/projects/{id}/system-one/available-instances.
//
// Returns a read-only, credential-free summary of Hub System One instances for
// callers who already pass the project's System One binding authorization
// (authorizeProjectSystemOneManage). Answers 404 unless both hub.system_one
// and hub.message_policies experiments are enabled.
func (s *Server) handleProjectSystemOneAvailableInstances(w http.ResponseWriter, r *http.Request, projectID string) {
	if !s.experimentEnabled(experiments.SystemOne) || !s.experimentEnabled(experiments.MessagePolicies) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeProjectSystemOneManage(w, r, project) {
		return
	}

	reg := s.SystemOneProviderRegistry()
	summaries := make([]ProjectSystemOneAvailableInstanceSummary, 0)
	cursor := ""
	seen := make(map[string]struct{})
	for {
		res, err := s.store.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{}, store.ListOptions{
			Limit:          systemOneInstancesListPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			slog.Error("failed to list available System One instances", "project_id", project.ID, "error", err)
			RuntimeError(w, "Failed to list available System One instances")
			return
		}
		for i := range res.Items {
			summaries = append(summaries, toProjectSystemOneAvailableInstanceSummary(reg, &res.Items[i]))
		}
		if res.NextCursor == "" {
			break
		}
		if _, dup := seen[res.NextCursor]; dup {
			slog.Error("System One instance pagination returned repeated cursor", "project_id", project.ID, "cursor", res.NextCursor)
			RuntimeError(w, "Failed to list available System One instances")
			return
		}
		seen[res.NextCursor] = struct{}{}
		cursor = res.NextCursor
	}

	writeJSON(w, http.StatusOK, ProjectSystemOneAvailableInstancesResponse{
		ProjectID: project.ID,
		Instances: summaries,
	})
}

func toProjectSystemOneAvailableInstanceSummary(
	reg *systemoneprovider.Registry,
	inst *store.SystemOneInstance,
) ProjectSystemOneAvailableInstanceSummary {
	summary := ProjectSystemOneAvailableInstanceSummary{
		Name:                     inst.Name,
		Adapter:                  inst.Adapter,
		Model:                    inst.Model,
		Enabled:                  inst.Enabled,
		QuestionTypes:            []string{},
		DistributionCompleteness: map[string]string{},
		NativeMetrics:            []systemoneprovider.MetricDecl{},
	}
	if reg == nil {
		return summary
	}
	adapter, ok := reg.Lookup(inst.Adapter)
	if !ok {
		return summary
	}
	caps, err := adapter.Capabilities(systemoneprovider.InstanceView{
		Name:     inst.Name,
		Revision: inst.Revision,
		Model:    inst.Model,
		Endpoint: inst.Endpoint,
		Location: inst.Location,
		Options:  inst.Options,
	})
	if err != nil {
		return summary
	}
	questionTypes := make([]string, 0, len(caps.Primitives))
	completeness := make(map[string]string, len(caps.Primitives))
	for prim, lim := range caps.Primitives {
		if !lim.Supported {
			continue
		}
		questionTypes = append(questionTypes, prim)
		completeness[prim] = lim.DistributionCompleteness
	}
	sort.Strings(questionTypes)
	summary.QuestionTypes = questionTypes
	summary.DistributionCompleteness = completeness
	if len(caps.NativeMetrics) > 0 {
		summary.NativeMetrics = slices.Clone(caps.NativeMetrics)
	}
	summary.Abstains = caps.Abstains
	return summary
}
