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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project visitor rules: visitor grants by email domain (ptone/scion#4603).
//
//	GET    /api/v1/projects/{id}/visitors/rules           list the project's visitor rules
//	POST   /api/v1/projects/{id}/visitors/rules           add a rule for (issuer, domain)
//	PATCH  /api/v1/projects/{id}/visitors/rules/{ruleId}  replace a rule's window
//	DELETE /api/v1/projects/{id}/visitors/rules/{ruleId}  remove a rule
//
// A rule gives visitor access to every user of a trusted issuer whose
// verified email's domain equals the rule's domain (case-insensitive; no
// wildcard, no subdomain). It is not itself a grant: on a matching user's
// request the hub creates a project-visitor binding for that user tagged
// with the rule's ID (resolveFederatedVisitor). The binding carries the
// rule's window, so a rule's expiry ends its visitors' access at the same
// time; changing the window changes theirs, and removing the rule deletes
// every binding it created, in the same transaction. Visitor grants by
// email are never changed by a rule. The routes share the visitor routes'
// admission (authorizeProjectVisitorManagement) and non-amplification rule.

// Mutation audit types of the visitor rule routes.
const (
	mutationTypeProjectVisitorRuleAdd    = "project_visitor_rule_add"
	mutationTypeProjectVisitorRuleRemove = "project_visitor_rule_remove"
	mutationTypeProjectVisitorRuleUpdate = "project_visitor_rule_update"
)

// visitorDomainNotAcceptedMessage is the refusal of a rule whose domain the
// issuer's allowed_emails or allowed_domains do not admit as a whole.
const visitorDomainNotAcceptedMessage = "This hub does not accept that email domain for visitors from that issuer"

// projectVisitorRulesSegment is the path segment, after "visitors", of the
// visitor rule routes. A visitor ID is a UUID, so it never equals it.
const projectVisitorRulesSegment = "rules"

// ProjectVisitorRuleView is one visitor rule of a project, as the rule
// routes return it.
type ProjectVisitorRuleView struct {
	ID     string `json:"id"`
	Issuer string `json:"issuer"`
	Domain string `json:"domain"`
	// IssuerAccepted reports whether the hub accepts visitors from Issuer
	// now (visitorGrantIssuer). While it is false the rule is kept, but it
	// creates no binding and its visitors' requests are refused.
	IssuerAccepted bool       `json:"issuerAccepted"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	NotBefore      *time.Time `json:"notBefore,omitempty"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// ListProjectVisitorRulesResponse is the GET /visitors/rules response.
type ListProjectVisitorRulesResponse struct {
	Rules []ProjectVisitorRuleView `json:"rules"`
}

// AddProjectVisitorRuleRequest is the POST /visitors/rules request.
type AddProjectVisitorRuleRequest struct {
	Issuer    string     `json:"issuer"`
	Domain    string     `json:"domain"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// UpdateProjectVisitorRuleRequest is the PATCH /visitors/rules/{ruleId}
// request. It replaces the whole window: an omitted or null bound is
// cleared.
type UpdateProjectVisitorRuleRequest struct {
	NotBefore *time.Time `json:"notBefore"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// handleProjectVisitorRules serves /api/v1/projects/{id}/visitors/rules
// and /visitors/rules/{ruleId}; ruleID is the path after "rules/" ("" for
// the collection). The caller has checked the experiment.
func (s *Server) handleProjectVisitorRules(w http.ResponseWriter, r *http.Request, projectID, ruleID string) {
	if strings.Contains(ruleID, "/") {
		NotFound(w, "Project visitor rule")
		return
	}
	if ruleID == "" {
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
	case ruleID == "" && r.Method == http.MethodGet:
		s.handleListProjectVisitorRules(w, r, project)
	case ruleID == "":
		s.handleAddProjectVisitorRule(w, r, project, actor)
	case r.Method == http.MethodPatch:
		s.handleUpdateProjectVisitorRule(w, r, project, actor, ruleID)
	default:
		s.handleRemoveProjectVisitorRule(w, r, project, ruleID)
	}
}

// handleListProjectVisitorRules answers GET /visitors/rules.
func (s *Server) handleListProjectVisitorRules(w http.ResponseWriter, r *http.Request, project *store.Project) {
	rules, err := s.store.ListProjectVisitorRules(r.Context(), project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	resp := ListProjectVisitorRulesResponse{Rules: make([]ProjectVisitorRuleView, 0, len(rules))}
	for _, rule := range rules {
		resp.Rules = append(resp.Rules, s.projectVisitorRuleResponse(rule))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddProjectVisitorRule answers POST /visitors/rules.
func (s *Server) handleAddProjectVisitorRule(w http.ResponseWriter, r *http.Request, project *store.Project, actor UserIdentity) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxVisitorRequestBytes)
	var req AddProjectVisitorRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}
	issuer := store.NormalizeFederatedIssuer(req.Issuer)
	domain := store.NormalizeVisitorRuleDomain(req.Domain)
	if issuer == "" {
		ValidationError(w, "issuer is required", map[string]interface{}{"field": "issuer"})
		return
	}
	if !store.ValidVisitorRuleDomain(domain) {
		ValidationError(w, "a valid email domain is required (for example corp.example; no wildcard, no @)",
			map[string]interface{}{"field": "domain"})
		return
	}
	if msg := validateVisitorWindow(req.NotBefore, req.ExpiresAt, time.Now()); msg != "" {
		ValidationError(w, msg, map[string]interface{}{"field": "expiresAt"})
		return
	}
	if field, msg := s.visitorRuleRefusal(issuer, domain); msg != "" {
		ValidationError(w, msg, map[string]interface{}{"field": field})
		return
	}
	if !s.visitorGrantNonAmplification(w, ctx, actor, project.ID) {
		return
	}

	rule := &store.ProjectVisitorRule{
		ProjectID: project.ID,
		Issuer:    issuer,
		Domain:    domain,
		NotBefore: req.NotBefore,
		ExpiresAt: req.ExpiresAt,
		CreatedBy: actor.ID(),
	}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateProjectVisitorRule(ctx, rule); err != nil {
			return err
		}
		return writeVisitorAudit(ctx, tx, mutationTypeProjectVisitorRuleAdd, project.ID, nil, visitorRuleAuditSummary(rule, nil))
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, s.projectVisitorRuleResponse(rule))
	case errors.Is(err, store.ErrAlreadyExists):
		Conflict(w, "This project already has a visitor rule for that issuer and domain; change the rule's window instead")
	default:
		slog.Error("project visitor rule add failed", "project_id", project.ID, "error", err)
		writeErrorFromErr(w, err, "")
	}
}

// handleUpdateProjectVisitorRule answers PATCH /visitors/rules/{ruleId}.
// The new window applies to the rule and to every binding it created, in
// one transaction. As for a grant, only an extension needs every
// project-visitor permission.
func (s *Server) handleUpdateProjectVisitorRule(w http.ResponseWriter, r *http.Request, project *store.Project, actor UserIdentity, ruleID string) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxVisitorRequestBytes)
	var req UpdateProjectVisitorRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}
	if msg := validateVisitorWindow(req.NotBefore, req.ExpiresAt, time.Now()); msg != "" {
		ValidationError(w, msg, map[string]interface{}{"field": "expiresAt"})
		return
	}
	holdsAll, err := s.actorHoldsVisitorPermissions(ctx, actor, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	var (
		out      ProjectVisitorRuleView
		changed  []*store.RoleBinding
		narrowed bool
	)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		before, err := projectVisitorRule(ctx, tx, project.ID, ruleID)
		if err != nil {
			return err
		}
		if !holdsAll && visitorWindowExtends(before.NotBefore, before.ExpiresAt, req.NotBefore, req.ExpiresAt) {
			return errVisitorWindowNotAmplifiable
		}
		// The old window admits a time the new one does not: a later or
		// added start, or an earlier or added expiry.
		narrowed = visitorWindowExtends(req.NotBefore, req.ExpiresAt, before.NotBefore, before.ExpiresAt)
		updated, bindings, err := tx.UpdateProjectVisitorRuleWindow(ctx, before.ID, req.NotBefore, req.ExpiresAt)
		if err != nil {
			return err
		}
		changed = bindings
		if err := writeVisitorAudit(ctx, tx, mutationTypeProjectVisitorRuleUpdate, project.ID,
			visitorRuleAuditSummary(before, nil), visitorRuleAuditSummary(updated, nil)); err != nil {
			return err
		}
		out = s.projectVisitorRuleResponse(updated)
		return nil
	})
	if errors.Is(err, errVisitorWindowNotAmplifiable) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, visitorGrantNonAmplificationMessage, nil)
		return
	}
	if err != nil {
		writeVisitorRuleItemError(w, err)
		return
	}
	// A narrower window can end the authorization of the rule's
	// visitors, so once the change has committed their conduit streams on
	// the project are re-checked, as for a grant. A window that only
	// widens ends nothing.
	if narrowed {
		for _, rb := range changed {
			s.publishConduitAuthzChanged(conduitAuthzMatchForRoleBinding(rb))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRemoveProjectVisitorRule answers DELETE /visitors/rules/{ruleId}:
// the rule, every binding it created and its exclusions are deleted with
// the audit record, in one transaction. Removal only reduces access, so it
// needs no non-amplification.
func (s *Server) handleRemoveProjectVisitorRule(w http.ResponseWriter, r *http.Request, project *store.Project, ruleID string) {
	ctx := r.Context()
	var removed []*store.RoleBinding
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		rule, err := projectVisitorRule(ctx, tx, project.ID, ruleID)
		if err != nil {
			return err
		}
		deleted, err := tx.DeleteProjectVisitorRule(ctx, rule.ID)
		if err != nil {
			return err
		}
		if err := writeVisitorAudit(ctx, tx, mutationTypeProjectVisitorRuleRemove, project.ID,
			visitorRuleAuditSummary(rule, deleted), nil); err != nil {
			return err
		}
		removed = deleted
		return nil
	})
	if err != nil {
		writeVisitorRuleItemError(w, err)
		return
	}
	// Once the removal has committed, the conduit streams on the project
	// of each visitor whose binding it deleted are re-checked.
	for _, rb := range removed {
		s.publishConduitAuthzChanged(conduitAuthzMatchForRoleBinding(rb))
	}
	w.WriteHeader(http.StatusNoContent)
}

// projectVisitorRule returns the visitor rule ruleID of projectID, or
// store.ErrNotFound (also for a rule of another project).
func projectVisitorRule(ctx context.Context, tx store.Store, projectID, ruleID string) (*store.ProjectVisitorRule, error) {
	rule, err := tx.GetProjectVisitorRule(ctx, ruleID)
	if err != nil {
		return nil, err
	}
	if rule.ProjectID != projectID {
		return nil, store.ErrNotFound
	}
	return rule, nil
}

// writeVisitorRuleItemError answers an error of a visitor rule item route.
func writeVisitorRuleItemError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		NotFound(w, "Project visitor rule")
		return
	}
	writeErrorFromErr(w, err, "")
}

// visitorRuleRefusal returns the refused field and message when the hub
// does not accept a visitor rule for (issuer, domain), or "" for both: the
// issuer must accept visitor grants (visitorGrantIssuer), and the domain
// must pass its allowed_emails and allowed_domains (visitorDomainAllowedByIssuer).
func (s *Server) visitorRuleRefusal(issuer, domain string) (field, message string) {
	cfg, ok := s.visitorGrantIssuer(issuer)
	if !ok {
		return "issuer", visitorIssuerNotAcceptedMessage
	}
	if !visitorDomainAllowedByIssuer(cfg, domain) {
		return "domain", visitorDomainNotAcceptedMessage
	}
	return "", ""
}

// visitorDomainAllowedByIssuer reports whether every email in domain passes
// the issuer's allowed_emails (when set) and allowed_domains (when set):
// allowed_domains must list the domain (case-insensitive), and
// allowed_emails must hold a leading-wildcard pattern whose suffix ends
// "@"+domain (such as "*@corp.example" or "*"), since an exact address
// admits only one person. Each matching request still checks its own email
// (visitorEmailAllowedByIssuer).
func visitorDomainAllowedByIssuer(cfg config.TrustedIssuerConfig, domain string) bool {
	domain = strings.ToLower(domain)
	if len(cfg.AllowedDomains) > 0 && !containsFold(cfg.AllowedDomains, domain) {
		return false
	}
	if len(cfg.AllowedEmails) > 0 {
		for _, pattern := range cfg.AllowedEmails {
			// Normalized exactly as matchesAllowedEmails does (lower
			// case only), so a pattern admits a rule only when it also
			// admits each email of the domain.
			p := strings.ToLower(pattern)
			if strings.HasPrefix(p, "*") && strings.HasSuffix("@"+domain, p[1:]) {
				return true
			}
		}
		return false
	}
	return true
}

// projectVisitorRuleView renders a visitor rule.
func projectVisitorRuleView(rule *store.ProjectVisitorRule) ProjectVisitorRuleView {
	return ProjectVisitorRuleView{
		ID:        rule.ID,
		Issuer:    rule.Issuer,
		Domain:    rule.Domain,
		CreatedBy: rule.CreatedBy,
		NotBefore: rule.NotBefore,
		ExpiresAt: rule.ExpiresAt,
		CreatedAt: rule.CreatedAt,
	}
}

// projectVisitorRuleResponse renders a visitor rule as the rule routes
// return it: projectVisitorRuleView plus whether the hub accepts visitors
// from its issuer now.
func (s *Server) projectVisitorRuleResponse(rule *store.ProjectVisitorRule) ProjectVisitorRuleView {
	v := projectVisitorRuleView(rule)
	_, v.IssuerAccepted = s.visitorGrantIssuer(v.Issuer)
	return v
}

// visitorRuleAuditSummary is the audit summary of a visitor rule: its ID,
// issuer, domain and window, and, for a removal, the bindings it deleted.
func visitorRuleAuditSummary(rule *store.ProjectVisitorRule, deleted []*store.RoleBinding) map[string]interface{} {
	out := map[string]interface{}{
		"rule_id": rule.ID,
		"issuer":  rule.Issuer,
		"domain":  rule.Domain,
	}
	if rule.NotBefore != nil {
		out["not_before"] = rule.NotBefore.UTC().Format(time.RFC3339)
	}
	if rule.ExpiresAt != nil {
		out["expires_at"] = rule.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if deleted != nil {
		ids := make([]string, 0, len(deleted))
		for _, rb := range deleted {
			ids = append(ids, rb.PrincipalID)
		}
		out["removed_visitor_ids"] = ids
	}
	return out
}
