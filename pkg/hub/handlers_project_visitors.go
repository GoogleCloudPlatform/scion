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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project visitor grants by email (ptone/scion#4603).
//
//	GET    /api/v1/projects/{id}/visitors              list the project's visitors
//	POST   /api/v1/projects/{id}/visitors              grant visitor access by (issuer, email)
//	PATCH  /api/v1/projects/{id}/visitors/{visitorId}  replace a grant's window
//	DELETE /api/v1/projects/{id}/visitors/{visitorId}  revoke a grant
//
// {visitorId} is the federated account ID. Every route answers 404 while
// the federation.project_visitors experiment is off, needs an interactive
// session (or dev) credential, and needs project.manage_visitors on the
// project. A grant is a project-visitor role binding; revoking it deletes
// the binding, and authorization reads bindings on every request, so the
// next request of the visitor is refused.

// Mutation audit types of the visitor grant routes and of first contact.
const (
	mutationTypeProjectVisitorGrant  = "project_visitor_grant"
	mutationTypeProjectVisitorRevoke = "project_visitor_revoke"
	mutationTypeProjectVisitorUpdate = "project_visitor_update"
	mutationTypeVisitorLinked        = "visitor_linked"
)

// Refusal messages of a grant whose issuer or email the hub does not
// accept for visitors.
const (
	visitorIssuerNotAcceptedMessage = "This hub does not accept visitors from that issuer"
	visitorEmailNotAcceptedMessage  = "This hub does not accept that email for visitors from that issuer"
)

// maxVisitorRequestBytes caps a visitor grant request body.
const maxVisitorRequestBytes = 16 * 1024

// ProjectVisitor is one visitor grant of a project, as the visitor routes
// return it.
type ProjectVisitor struct {
	// ID is the federated account ID; the item routes address it.
	ID        string `json:"id"`
	BindingID string `json:"bindingId"`
	// Email is the visitor's email as the issuer asserts it (lower case),
	// never the account's synthetic address.
	Email  string `json:"email"`
	Issuer string `json:"issuer"`
	// Status is the account's status: pending until the visitor's first
	// request links the account, then active, or suspended.
	Status    string     `json:"status"`
	GrantedBy string     `json:"grantedBy,omitempty"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ListProjectVisitorsResponse is the GET /visitors response.
type ListProjectVisitorsResponse struct {
	Visitors []ProjectVisitor `json:"visitors"`
}

// AddProjectVisitorRequest is the POST /visitors request.
type AddProjectVisitorRequest struct {
	Issuer    string     `json:"issuer"`
	Email     string     `json:"email"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// UpdateProjectVisitorRequest is the PATCH /visitors/{visitorId} request.
// It replaces the whole window: an omitted or null bound is cleared.
type UpdateProjectVisitorRequest struct {
	NotBefore *time.Time `json:"notBefore"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// Grant outcomes the handlers answer with 409.
var (
	errVisitorAlreadyGranted   = errors.New("the account is already a visitor of this project")
	errVisitorAccountSuspended = errors.New("the account is suspended")
	// errVisitorAccountRace marks a concurrent create of the same
	// federated account; the grant transaction is retried once.
	errVisitorAccountRace = errors.New("federated account created concurrently")
)

// handleProjectVisitors serves /api/v1/projects/{id}/visitors[/{visitorId}].
// rest is the path after "visitors".
func (s *Server) handleProjectVisitors(w http.ResponseWriter, r *http.Request, projectID, rest string) {
	if !s.experimentEnabled(experiments.FederationProjectVisitors) {
		NotFound(w, "route")
		return
	}
	visitorID := strings.TrimPrefix(rest, "/")
	if strings.Contains(visitorID, "/") {
		NotFound(w, "Project visitor")
		return
	}
	if visitorID == "" {
		switch r.Method {
		case http.MethodGet, http.MethodPost:
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
			return
		}
	} else {
		switch r.Method {
		case http.MethodPatch, http.MethodDelete:
		default:
			MethodNotAllowed(w, http.MethodPatch, http.MethodDelete)
			return
		}
	}

	project, actor, ok := s.authorizeProjectVisitorManagement(w, r, projectID)
	if !ok {
		return
	}
	switch {
	case visitorID == "" && r.Method == http.MethodGet:
		s.handleListProjectVisitors(w, r, project)
	case visitorID == "":
		s.handleAddProjectVisitor(w, r, project, actor)
	case r.Method == http.MethodPatch:
		s.handleUpdateProjectVisitor(w, r, project, actor, visitorID)
	default:
		s.handleRemoveProjectVisitor(w, r, project, visitorID)
	}
}

// authorizeProjectVisitorManagement is the common admission of every
// visitor route: an interactive session or dev credential of a user (the
// rule membership changes follow), an existing project, and
// project.manage_visitors on it (project owners and admins, and any
// principal whose system-scope roles carry it, such as the super-admin).
// It writes the refusal and returns false when the request is refused.
func (s *Server) authorizeProjectVisitorManagement(w http.ResponseWriter, r *http.Request, projectID string) (*store.Project, UserIdentity, bool) {
	ctx := r.Context()
	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		return nil, nil, false
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil, nil, false
	}
	if !s.authorize(w, r, projectResource(project), ActionManageVisitors) {
		return nil, nil, false
	}
	return project, actor, true
}

// handleListProjectVisitors answers GET /visitors.
func (s *Server) handleListProjectVisitors(w http.ResponseWriter, r *http.Request, project *store.Project) {
	ctx := r.Context()
	bindings, err := s.store.ListVisitorRoleBindings(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	ids := make([]string, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.PrincipalID)
	}
	users, err := s.store.GetUsersByIDs(ctx, ids)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	resp := ListProjectVisitorsResponse{Visitors: make([]ProjectVisitor, 0, len(bindings))}
	for _, b := range bindings {
		u, ok := users[b.PrincipalID]
		if !ok || !u.IsFederated() {
			continue
		}
		resp.Visitors = append(resp.Visitors, projectVisitorView(u, b))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddProjectVisitor answers POST /visitors.
func (s *Server) handleAddProjectVisitor(w http.ResponseWriter, r *http.Request, project *store.Project, actor UserIdentity) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxVisitorRequestBytes)
	var req AddProjectVisitorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}
	issuer := store.NormalizeFederatedIssuer(req.Issuer)
	email := store.NormalizeFederatedEmail(req.Email)
	if issuer == "" {
		ValidationError(w, "issuer is required", map[string]interface{}{"field": "issuer"})
		return
	}
	if _, ok := domainOf(email); !ok || store.IsFederatedSyntheticEmail(email) {
		ValidationError(w, "a valid email is required", map[string]interface{}{"field": "email"})
		return
	}
	if msg := validateVisitorWindow(req.NotBefore, req.ExpiresAt, time.Now()); msg != "" {
		ValidationError(w, msg, nil)
		return
	}
	if field, msg := s.visitorGrantRefusal(issuer, email); msg != "" {
		ValidationError(w, msg, map[string]interface{}{"field": field})
		return
	}
	if !s.visitorGrantNonAmplification(w, ctx, actor, project.ID) {
		return
	}

	var (
		visitor ProjectVisitor
		err     error
	)
	for attempt := 0; attempt < 2; attempt++ {
		visitor, err = s.grantProjectVisitor(ctx, project.ID, actor.ID(), issuer, email, req.NotBefore, req.ExpiresAt)
		if !errors.Is(err, errVisitorAccountRace) {
			break
		}
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, visitor)
	case errors.Is(err, errVisitorAlreadyGranted):
		Conflict(w, "That email is already a visitor of this project for that issuer; change the grant's window instead")
	case errors.Is(err, errVisitorAccountSuspended):
		Conflict(w, "That visitor account is suspended")
	case errors.Is(err, errVisitorAccountRace):
		Conflict(w, "The visitor account was changed concurrently; try again")
	default:
		slog.Error("project visitor grant failed", "project_id", project.ID, "error", err)
		writeErrorFromErr(w, err, "")
	}
}

// grantProjectVisitor binds the federated account of (issuer, email) to
// the project-visitor role on projectID, creating the account in status
// pending when none exists, and writes the grant audit record, all in one
// transaction.
func (s *Server) grantProjectVisitor(ctx context.Context, projectID, actorID, issuer, email string, notBefore, expiresAt *time.Time) (ProjectVisitor, error) {
	var out ProjectVisitor
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		u, err := tx.GetFederatedUserByEmail(ctx, issuer, email)
		if errors.Is(err, store.ErrNotFound) {
			u = &store.User{
				DisplayName:    email,
				Kind:           store.UserKindFederated,
				Status:         store.UserStatusPending,
				HomeIssuer:     &issuer,
				FederatedEmail: &email,
			}
			if err := tx.CreateFederatedUser(ctx, u); err != nil {
				if errors.Is(err, store.ErrAlreadyExists) {
					return errVisitorAccountRace
				}
				return err
			}
		} else if err != nil {
			return err
		}
		if u.Status == store.UserStatusSuspended {
			return errVisitorAccountSuspended
		}
		rb, err := tx.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
			PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID:   u.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       projectID,
			NotBefore:     notBefore,
			ExpiresAt:     expiresAt,
			CreatedBy:     actorID,
		})
		if errors.Is(err, store.ErrAlreadyExists) {
			return errVisitorAlreadyGranted
		}
		if err != nil {
			return err
		}
		subject, err := federationSubjectOf(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		if err := writeVisitorAudit(ctx, tx, mutationTypeProjectVisitorGrant, projectID, nil,
			visitorAuditSummary(u, rb, subject)); err != nil {
			return err
		}
		out = projectVisitorView(u, rb)
		return nil
	})
	return out, err
}

// visitorGrantNonAmplification is the grant's non-amplification rule: the
// actor must hold every permission of the project-visitor role on the
// project, so a grant never passes on authority the actor lacks. An
// unscoped local platform admin passes, as in CanDelegate. It writes a 403
// and returns false when the actor does not.
func (s *Server) visitorGrantNonAmplification(w http.ResponseWriter, ctx context.Context, actor UserIdentity, projectID string) bool {
	if IsUnscopedLocalPlatformAdmin(actor) {
		return true
	}
	rd, err := s.store.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if d := s.authzService.actorHoldsAllPermissions(ctx, actor, rd.Permissions, store.RoleScopeProject, projectID); !d.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You can grant visitor access only when you hold every permission it gives on this project", nil)
		return false
	}
	return true
}

// handleUpdateProjectVisitor answers PATCH /visitors/{visitorId}.
func (s *Server) handleUpdateProjectVisitor(w http.ResponseWriter, r *http.Request, project *store.Project, actor UserIdentity, visitorID string) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxVisitorRequestBytes)
	var req UpdateProjectVisitorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}
	if msg := validateVisitorWindow(req.NotBefore, req.ExpiresAt, time.Now()); msg != "" {
		ValidationError(w, msg, nil)
		return
	}
	if !s.visitorGrantNonAmplification(w, ctx, actor, project.ID) {
		return
	}
	var out ProjectVisitor
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		u, before, err := projectVisitorBinding(ctx, tx, project.ID, visitorID)
		if err != nil {
			return err
		}
		updated, err := tx.UpdateVisitorRoleBindingWindow(ctx, before.ID, req.NotBefore, req.ExpiresAt)
		if err != nil {
			return err
		}
		subject, err := federationSubjectOf(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		if err := writeVisitorAudit(ctx, tx, mutationTypeProjectVisitorUpdate, project.ID,
			visitorAuditSummary(u, before, subject), visitorAuditSummary(u, updated, subject)); err != nil {
			return err
		}
		out = projectVisitorView(u, updated)
		return nil
	})
	if err != nil {
		writeVisitorItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRemoveProjectVisitor answers DELETE /visitors/{visitorId}.
func (s *Server) handleRemoveProjectVisitor(w http.ResponseWriter, r *http.Request, project *store.Project, visitorID string) {
	ctx := r.Context()
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		u, rb, err := projectVisitorBinding(ctx, tx, project.ID, visitorID)
		if err != nil {
			return err
		}
		if err := tx.DeleteRoleBinding(ctx, rb.ID); err != nil {
			return err
		}
		subject, err := federationSubjectOf(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		return writeVisitorAudit(ctx, tx, mutationTypeProjectVisitorRevoke, project.ID,
			visitorAuditSummary(u, rb, subject), nil)
	})
	if err != nil {
		writeVisitorItemError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// projectVisitorBinding returns the federated account visitorID and its
// project-visitor binding on projectID, or store.ErrNotFound.
func projectVisitorBinding(ctx context.Context, tx store.Store, projectID, visitorID string) (*store.User, *store.RoleBinding, error) {
	bindings, err := tx.ListVisitorRoleBindings(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range bindings {
		if b.PrincipalType != store.RoleBindingPrincipalUser || b.PrincipalID != visitorID {
			continue
		}
		u, err := tx.GetUser(ctx, b.PrincipalID)
		if err != nil {
			return nil, nil, err
		}
		if !u.IsFederated() {
			return nil, nil, store.ErrNotFound
		}
		return u, b, nil
	}
	return nil, nil, store.ErrNotFound
}

// writeVisitorItemError answers an error of a visitor item route.
func writeVisitorItemError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		NotFound(w, "Project visitor")
		return
	}
	writeErrorFromErr(w, err, "")
}

// validateVisitorWindow returns why a grant window is refused, or "": an
// expiry must be in the future and after the start.
func validateVisitorWindow(notBefore, expiresAt *time.Time, now time.Time) string {
	if expiresAt == nil {
		return ""
	}
	if !expiresAt.After(now) {
		return "expiresAt must be in the future"
	}
	if notBefore != nil && !expiresAt.After(*notBefore) {
		return "expiresAt must be after notBefore"
	}
	return ""
}

// visitorGrantRefusal returns the refused field and message when the hub
// does not accept visitor grants for (issuer, email), or "" for both: the
// issuer must be trusted, of type user or hub, with allow_visitor_grants,
// and the email must pass its allowed_emails and allowed_domains.
func (s *Server) visitorGrantRefusal(issuer, email string) (field, message string) {
	cfg, ok := s.visitorGrantIssuer(issuer)
	if !ok {
		return "issuer", visitorIssuerNotAcceptedMessage
	}
	if !visitorEmailAllowedByIssuer(cfg, email) {
		return "email", visitorEmailNotAcceptedMessage
	}
	return "", ""
}

// visitorGrantIssuer returns the trusted issuer configuration of issuer
// when the hub accepts visitor grants for its users: the issuer is
// trusted, its issuer_type is user or hub, and allow_visitor_grants is on.
func (s *Server) visitorGrantIssuer(issuer string) (config.TrustedIssuerConfig, bool) {
	fa := s.federationAuth.Load()
	if fa == nil {
		return config.TrustedIssuerConfig{}, false
	}
	cfg, ok := fa.IssuerConfig(issuer)
	if !ok || !cfg.AllowVisitorGrants || !config.IssuerAllowsVisitorGrantsType(cfg.IssuerType) {
		return config.TrustedIssuerConfig{}, false
	}
	return cfg, true
}

// visitorEmailAllowedByIssuer reports whether email passes the issuer's
// allowed_emails (when set) and allowed_domains (when set).
func visitorEmailAllowedByIssuer(cfg config.TrustedIssuerConfig, email string) bool {
	if len(cfg.AllowedEmails) > 0 && !matchesAllowedEmails(cfg.AllowedEmails, email) {
		return false
	}
	if len(cfg.AllowedDomains) > 0 {
		domain, ok := domainOf(email)
		if !ok || !containsFold(cfg.AllowedDomains, domain) {
			return false
		}
	}
	return true
}

// projectVisitorView renders a visitor grant.
func projectVisitorView(u *store.User, rb *store.RoleBinding) ProjectVisitor {
	v := ProjectVisitor{
		ID:        u.ID,
		BindingID: rb.ID,
		Status:    u.Status,
		GrantedBy: rb.CreatedBy,
		NotBefore: rb.NotBefore,
		ExpiresAt: rb.ExpiresAt,
		CreatedAt: rb.CreatedAt,
	}
	if u.FederatedEmail != nil {
		v.Email = *u.FederatedEmail
	}
	if u.HomeIssuer != nil {
		v.Issuer = *u.HomeIssuer
	}
	return v
}

// federationSubjectOf returns the issuer subject linked to the federated
// account userID, or "" while the account is not linked.
func federationSubjectOf(ctx context.Context, st store.Store, userID string) (string, error) {
	bindings, err := st.GetExternalIdentitiesByUserID(ctx, userID)
	if err != nil {
		return "", err
	}
	for _, b := range bindings {
		if b.Provider == store.FederationIdentityProvider {
			return b.Subject, nil
		}
	}
	return "", nil
}

// visitorAuditSummary is the audit summary of a visitor grant: the account,
// its issuer, email and subject (once linked), and the grant's binding and
// window.
func visitorAuditSummary(u *store.User, rb *store.RoleBinding, subject string) map[string]interface{} {
	v := projectVisitorView(u, rb)
	out := map[string]interface{}{
		"visitor_id": v.ID,
		"binding_id": v.BindingID,
		"issuer":     v.Issuer,
		"email":      v.Email,
	}
	if subject != "" {
		out["subject"] = subject
	}
	if v.NotBefore != nil {
		out["not_before"] = v.NotBefore.UTC().Format(time.RFC3339)
	}
	if v.ExpiresAt != nil {
		out["expires_at"] = v.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

// writeVisitorAudit writes a visitor grant audit record on the project in
// tx, with the request's actor.
func writeVisitorAudit(ctx context.Context, tx store.Store, mutationType, projectID string, before, after map[string]interface{}) error {
	record := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   "project",
		TargetID:     projectID,
		Timestamp:    time.Now(),
	}
	if before != nil {
		b, err := json.Marshal(before)
		if err != nil {
			return fmt.Errorf("visitor audit summary: %w", err)
		}
		record.BeforeSummary = string(b)
	}
	if after != nil {
		a, err := json.Marshal(after)
		if err != nil {
			return fmt.Errorf("visitor audit summary: %w", err)
		}
		record.AfterSummary = string(a)
	}
	auditActorFromContext(ctx).ApplyActor(record)
	return tx.CreateMutationAudit(ctx, record)
}
