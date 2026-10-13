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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errFederatedVisitorRefused is resolveFederatedVisitor's refusal of a
// request whose federated account may not act: the account is suspended.
// The auth middleware answers it with 403 (visitorAccountRefusedMessage).
var errFederatedVisitorRefused = errors.New("federated account may not act")

// errNoVisitorRuleBound rolls back a first contact that created an account
// for visitor rules when none of them binds it any more.
var errNoVisitorRuleBound = errors.New("no visitor rule binds the new account")

// visitorAccountRefusedMessage is the 403 message of a request refused by
// errFederatedVisitorRefused.
const visitorAccountRefusedMessage = "This account may not access this hub"

// errFederatedVisitorIssuerNotAccepted is resolveFederatedVisitor's refusal
// of a linked, active federated account whose issuer the hub no longer
// accepts visitors from (visitorGrantIssuer fails: allow_visitor_grants is
// off, or the issuer_type is one the flag does not apply to). The auth
// middleware answers it with 403 and visitorRouteRefusalMessage. The
// account and its grants are kept, so accepting the issuer again restores
// access with no grant change.
var errFederatedVisitorIssuerNotAccepted = errors.New("hub does not accept visitors from the account's issuer")

// resolveFederatedVisitor is the auth middleware's federated visitor
// lookup (AuthConfig.ResolveFederatedVisitor). After the federation
// authenticator has verified a token, it returns the VisitorIdentity of the
// federated account linked to the token's (issuer, subject) when all of
// these hold:
//   - the federation.project_visitors experiment is enabled now;
//   - the verified identity is a *FederatedUserIdentity;
//   - the pair is linked (store.Store.GetFederatedUserBySubject) to a
//     federated account whose status is active, or it is not linked and
//     first contact (linkPendingVisitor) links it to a pending account or
//     to a new account for a matching visitor rule.
//
// Status policy: an active account acts as its visitor identity while the
// hub accepts visitors from its issuer, and is refused otherwise
// (errFederatedVisitorIssuerNotAccepted; the issuer configuration is read
// on every request, so a configuration change applies to the next one); a
// suspended account is refused (errFederatedVisitorRefused), whatever its
// grants; a pending account never acts as a visitor until first contact
// links and activates it, so a linked account still pending keeps the
// verified identity. In every other case (experiment off, another
// federated identity type, no link and nothing to link) it returns (nil,
// nil), so the request keeps the verified identity exactly as before. Any
// other store error is returned; the middleware answers it with 503.
//
// For an active account linked before this request, the visitor rules
// matching the issuer token's verified email are applied (applyVisitorRules), so
// a rule added after the account's first contact binds it on its next
// request.
func (s *Server) resolveFederatedVisitor(ctx context.Context, identity FederatedIdentity) (*VisitorIdentity, error) {
	fu, ok := identity.(*FederatedUserIdentity)
	if !ok || fu == nil || s.store == nil {
		return nil, nil
	}
	if !s.experimentEnabled(experiments.FederationProjectVisitors) {
		return nil, nil
	}
	linkedNow := false
	u, err := s.store.GetFederatedUserBySubject(ctx, fu.IssuerURL(), fu.Subject())
	if errors.Is(err, store.ErrNotFound) {
		u, err = s.linkPendingVisitor(ctx, fu)
		linkedNow = true
	}
	if err != nil {
		return nil, err
	}
	if u == nil || !u.IsFederated() {
		return nil, nil
	}
	switch u.Status {
	case store.UserStatusActive:
		if _, ok := s.visitorGrantIssuer(store.NormalizeFederatedIssuer(fu.IssuerURL())); !ok {
			return nil, errFederatedVisitorIssuerNotAccepted
		}
		if !linkedNow {
			s.applyVisitorRules(ctx, fu, u)
		}
	case store.UserStatusSuspended:
		return nil, errFederatedVisitorRefused
	default:
		return nil, nil
	}
	email := fu.Email()
	if u.FederatedEmail != nil {
		email = *u.FederatedEmail
	}
	return NewVisitorIdentity(u.ID, fu.IssuerURL(), fu.Subject(), email, fu.DisplayName()), nil
}

// visitorTokenEmail returns the normalized issuer and email of a verified
// issuer token of a federated user whose email a visitor grant or rule may match, and
// true, when all of these hold:
//   - the hub accepts visitor grants for the issuer (visitorGrantIssuer:
//     trusted, issuer_type user or hub, allow_visitor_grants on);
//   - the token carries an email claim, and its email_verified claim, when
//     present, is true;
//   - the email passes the issuer's allowed_emails and allowed_domains.
func (s *Server) visitorTokenEmail(fu *FederatedUserIdentity) (issuer, email string, ok bool) {
	issuer = store.NormalizeFederatedIssuer(fu.IssuerURL())
	cfg, ok := s.visitorGrantIssuer(issuer)
	if !ok {
		return "", "", false
	}
	email = store.NormalizeFederatedEmail(fu.Email())
	if email == "" {
		return "", "", false
	}
	if verified, present := fu.EmailVerifiedClaim(); present && !verified {
		return "", "", false
	}
	if !visitorEmailAllowedByIssuer(cfg, email) {
		return "", "", false
	}
	return issuer, email, true
}

// activeVisitorRulesForEmail returns the visitor rules, of every project,
// of issuer whose domain equals the domain of email exactly (both lower
// case) and whose window contains now. An email with no usable domain
// matches none.
func (s *Server) activeVisitorRulesForEmail(ctx context.Context, issuer, email string) ([]*store.ProjectVisitorRule, error) {
	domain, ok := domainOf(email)
	if !ok {
		return nil, nil
	}
	return s.store.ListActiveProjectVisitorRules(ctx, issuer, domain, time.Now())
}

// linkPendingVisitor is first contact: a verified issuer token for a user
// whose (issuer, subject) is not linked yet is linked to a federated
// account, which becomes active. It links only when visitorTokenEmail
// accepts the token, and then to:
//   - the pending federated account granted to (issuer, lower-cased
//     email), when one exists; or
//   - when none exists, a new account for that (issuer, email), if at
//     least one active visitor rule matches the email's domain.
//
// In the same transaction it writes the visitor_linked audit record and
// applies the matching visitor rules (bindVisitorRules). From then on the
// subject is the account's key: a later email change at the issuer does
// not move the account or its grants. When a concurrent first contact
// links the account first, the store answers ErrAlreadyExists and the
// account is read again by subject. It returns (nil, nil) when nothing is
// linked.
func (s *Server) linkPendingVisitor(ctx context.Context, fu *FederatedUserIdentity) (*store.User, error) {
	issuer, email, ok := s.visitorTokenEmail(fu)
	if !ok {
		return nil, nil
	}
	pending, err := s.store.GetFederatedUserByEmail(ctx, issuer, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		pending = nil
	case err != nil:
		return nil, err
	case pending.Status != store.UserStatusPending:
		return nil, nil
	}
	rules, err := s.activeVisitorRulesForEmail(ctx, issuer, email)
	if err != nil {
		return nil, err
	}
	if pending == nil && len(rules) == 0 {
		return nil, nil
	}

	var linked *store.User
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		source := projectVisitorSourceEmail
		accountID := ""
		if pending != nil {
			accountID = pending.ID
		} else {
			// A new account for the rules: created pending, then linked
			// and activated below like a granted one.
			displayName := fu.DisplayName()
			if displayName == "" {
				displayName = email
			}
			acct := &store.User{
				DisplayName:    displayName,
				Kind:           store.UserKindFederated,
				Status:         store.UserStatusPending,
				HomeIssuer:     &issuer,
				FederatedEmail: &email,
			}
			if err := tx.CreateFederatedUser(ctx, acct); err != nil {
				return err
			}
			accountID = acct.ID
			source = projectVisitorSourceRule
		}
		u, err := tx.LinkPendingFederatedUser(ctx, accountID, issuer, fu.Subject(), email)
		if err != nil {
			return err
		}
		after, err := json.Marshal(map[string]string{
			"issuer":  issuer,
			"email":   email,
			"subject": fu.Subject(),
			"source":  source,
		})
		if err != nil {
			return fmt.Errorf("visitor link audit summary: %w", err)
		}
		if err := tx.CreateMutationAudit(ctx, visitorLinkedAuditRecord(ctx, u.ID, store.UserStatusPending, string(after))); err != nil {
			return err
		}
		bound, err := bindVisitorRules(ctx, tx, u, fu.Subject(), rules)
		if err != nil {
			return err
		}
		if pending == nil && bound == 0 {
			// The account was created for rules that no longer bind it
			// (removed or ended since they were read): keep nothing.
			return errNoVisitorRuleBound
		}
		linked = u
		return nil
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		u, rerr := s.store.GetFederatedUserBySubject(ctx, issuer, fu.Subject())
		if errors.Is(rerr, store.ErrNotFound) {
			return nil, nil
		}
		return u, rerr
	}
	if errors.Is(err, errNoVisitorRuleBound) {
		return nil, nil
	}
	if errors.Is(err, store.ErrInvalidInput) {
		// The account stopped being pending or changed between the read
		// and the link, or a rule changed under a binding; nothing was
		// linked.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return linked, nil
}

// applyVisitorRules binds the active, linked federated account u to every
// project whose active visitor rule matches the verified email of fu
// (visitorTokenEmail, activeVisitorRulesForEmail) and where u has no
// exclusion from that rule and no project-visitor binding, or only an
// inactive visitor grant by email, which the rule binding replaces
// (missingVisitorRules). The bindings and their audit records are one
// transaction. Nothing is written when
// nothing is missing, which is the case on almost every request. A failure
// is logged and does not refuse the request: the account keeps the
// bindings it has, and the next request tries again (a concurrent request
// that created the same binding first is the usual cause).
func (s *Server) applyVisitorRules(ctx context.Context, fu *FederatedUserIdentity, u *store.User) {
	issuer, email, ok := s.visitorTokenEmail(fu)
	if !ok || u.HomeIssuer == nil || *u.HomeIssuer != issuer {
		return
	}
	rules, err := s.activeVisitorRulesForEmail(ctx, issuer, email)
	if err != nil {
		slog.Warn("visitor rules lookup failed", "user_id", u.ID, "error", err)
		return
	}
	if len(rules) == 0 {
		return
	}
	missing, err := missingVisitorRules(ctx, s.store, u.ID, rules, time.Now())
	if err != nil {
		slog.Warn("visitor rules lookup failed", "user_id", u.ID, "error", err)
		return
	}
	if len(missing) == 0 {
		return
	}
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		_, err := bindVisitorRules(ctx, tx, u, fu.Subject(), rules)
		return err
	})
	if err != nil {
		slog.Warn("visitor rule binding failed", "user_id", u.ID, "error", err)
	}
}

// visitorRuleBinding is a rule that would bind a federated account on the
// rule's project, and the inactive visitor grant by email of the account
// on that project that the rule binding replaces, or nil.
type visitorRuleBinding struct {
	rule     *store.ProjectVisitorRule
	replaces *store.RoleBinding
}

// missingVisitorRules returns the rules, of distinct projects, that would
// bind the federated account userID at now: those that have no exclusion
// for it, of a project where it has no project-visitor binding, or only a
// visitor grant by email whose window does not contain now (expired, or
// not started yet), which the rule binding replaces. An active grant by
// email, and a binding of another rule, are kept and the rule is skipped.
// The first rule of a project wins (rules arrive oldest first).
func missingVisitorRules(ctx context.Context, st store.Store, userID string, rules []*store.ProjectVisitorRule, now time.Time) ([]visitorRuleBinding, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	rd, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
	if err != nil {
		return nil, err
	}
	bindings, err := st.ListRoleBindingsForPrincipals(ctx,
		[]store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: userID}},
		[]string{store.RoleScopeProject}, nil)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]*store.RoleBinding, len(bindings))
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID {
			existing[b.ScopeID] = b
		}
	}
	taken := make(map[string]bool, len(rules))
	var out []visitorRuleBinding
	for _, rule := range rules {
		if taken[rule.ProjectID] {
			continue
		}
		var replaces *store.RoleBinding
		if b, ok := existing[rule.ProjectID]; ok {
			if b.SourceRuleID != nil || isBindingActive(b, now) {
				continue
			}
			replaces = b
		}
		excluded, err := st.IsProjectVisitorExcluded(ctx, rule.ID, userID)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		taken[rule.ProjectID] = true
		out = append(out, visitorRuleBinding{rule: rule, replaces: replaces})
	}
	return out, nil
}

// bindVisitorRules creates, in tx, a project-visitor binding of the
// federated account u for each rule of rules that missingVisitorRules
// (read in tx) still reports missing, tagged with the rule's ID, and a
// visitor_linked audit record naming the rule for each. The store gives
// the binding the rule's window as it is in tx. It returns the number of
// bindings created.
//
// An inactive visitor grant by email on the rule's project is deleted
// first (DeleteInactiveVisitorGrant) and the audit record names it
// (replaced_binding_id); when the grant has become active in the meantime
// it is kept and the rule is skipped. A rule the store refuses because it
// was removed, or is no longer active, since it was read is skipped; if a
// grant was already deleted for it, the error is returned so the
// transaction keeps the grant. A binding the account already has,
// whatever its source, is otherwise left as it is.
func bindVisitorRules(ctx context.Context, tx store.Store, u *store.User, subject string, rules []*store.ProjectVisitorRule) (int, error) {
	now := time.Now()
	missing, err := missingVisitorRules(ctx, tx, u.ID, rules, now)
	if err != nil {
		return 0, err
	}
	bound := 0
	for _, m := range missing {
		rule := m.rule
		replacedID := ""
		if m.replaces != nil {
			removed, err := tx.DeleteInactiveVisitorGrant(ctx, m.replaces.ID, now)
			if err != nil {
				return 0, err
			}
			if !removed {
				continue
			}
			replacedID = m.replaces.ID
		}
		ruleID := rule.ID
		rb, err := tx.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
			PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID:   u.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       rule.ProjectID,
			NotBefore:     rule.NotBefore,
			ExpiresAt:     rule.ExpiresAt,
			CreatedBy:     rule.CreatedBy,
			SourceRuleID:  &ruleID,
		})
		if errors.Is(err, store.ErrVisitorBindingRefused) && replacedID == "" {
			continue
		}
		if err != nil {
			return 0, err
		}
		summary := map[string]string{
			"issuer":     rule.Issuer,
			"email":      federatedEmailOf(u),
			"subject":    subject,
			"rule_id":    rule.ID,
			"project_id": rule.ProjectID,
			"binding_id": rb.ID,
		}
		if replacedID != "" {
			summary["replaced_binding_id"] = replacedID
			summary["replaced"] = "inactive visitor grant by email"
		}
		after, err := json.Marshal(summary)
		if err != nil {
			return 0, fmt.Errorf("visitor link audit summary: %w", err)
		}
		if err := tx.CreateMutationAudit(ctx, visitorLinkedAuditRecord(ctx, u.ID, "", string(after))); err != nil {
			return 0, err
		}
		bound++
	}
	return bound, nil
}

// federatedEmailOf returns the federated email of the account u, or "".
func federatedEmailOf(u *store.User) string {
	if u == nil || u.FederatedEmail == nil {
		return ""
	}
	return *u.FederatedEmail
}

// visitorLinkedAuditRecord is a visitor_linked audit record on the
// federated account userID, acting as that visitor: first contact, or a
// binding a visitor rule created.
func visitorLinkedAuditRecord(ctx context.Context, userID, before, after string) *store.MutationAuditRecord {
	record := &store.MutationAuditRecord{
		MutationType:        mutationTypeVisitorLinked,
		TargetType:          "user",
		TargetID:            userID,
		BeforeSummary:       before,
		AfterSummary:        after,
		Timestamp:           time.Now(),
		ActorPrincipalKind:  string(PrincipalKindVisitor),
		ActorPrincipalID:    userID,
		ActorCredentialType: string(CredentialKindFederation),
	}
	auditActorFromContext(ctx).ApplyActor(record)
	return record
}

// visitorRouteRefusalMessage is the one response message for every route a
// visitor is refused, so the response does not distinguish routes.
const visitorRouteRefusalMessage = "You don't have access to this resource"

// refuseVisitorRequest is routeGuard's visitor check. Until per-route visitor
// admission exists, it refuses every request whose identity is a
// *VisitorIdentity with the same 403, whatever the route. It writes the
// refusal and returns true when it refused.
func refuseVisitorRequest(w http.ResponseWriter, r *http.Request) bool {
	if !isVisitorIdentity(GetIdentityFromContext(r.Context())) {
		return false
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, visitorRouteRefusalMessage, nil)
	return true
}
