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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errFederatedVisitorRefused is resolveFederatedVisitor's refusal of a
// request whose federated account may not act: the account is suspended.
// The auth middleware answers it with 403 (visitorAccountRefusedMessage).
var errFederatedVisitorRefused = errors.New("federated account may not act")

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
//     first contact (linkPendingVisitor) links it to a pending account.
//
// Status policy: an active account acts as its visitor identity while the
// hub accepts visitors from its issuer, and is refused otherwise
// (errFederatedVisitorIssuerNotAccepted; the issuer configuration is read
// on every request, so a configuration change applies to the next one); a
// suspended account is refused (errFederatedVisitorRefused), whatever its
// grants; a pending account never acts as a visitor until first contact
// links and activates it, so a linked account still pending keeps the
// verified identity. In every other case (experiment off, another
// federated identity type, no link and no pending account to link) it
// returns (nil, nil), so the request keeps the verified identity exactly as
// before. Any other store error is returned; the middleware answers it with
// 503.
func (s *Server) resolveFederatedVisitor(ctx context.Context, identity FederatedIdentity) (*VisitorIdentity, error) {
	fu, ok := identity.(*FederatedUserIdentity)
	if !ok || fu == nil || s.store == nil {
		return nil, nil
	}
	if !s.experimentEnabled(experiments.FederationProjectVisitors) {
		return nil, nil
	}
	u, err := s.store.GetFederatedUserBySubject(ctx, fu.IssuerURL(), fu.Subject())
	if errors.Is(err, store.ErrNotFound) {
		u, err = s.linkPendingVisitor(ctx, fu)
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

// linkPendingVisitor is first contact: a verified issuer token for a user whose
// (issuer, subject) is not linked yet is linked to the pending federated
// account granted to its email, which becomes active. It links only when
// all of these hold:
//   - the hub accepts visitor grants for the issuer (visitorGrantIssuer:
//     trusted, issuer_type user or hub, allow_visitor_grants on);
//   - the token carries an email claim, and its email_verified claim, when
//     present, is true;
//   - the email passes the issuer's allowed_emails and allowed_domains;
//   - a pending federated account exists for (issuer, lower-cased email).
//
// The link, the activation and the visitor_linked audit record are one
// transaction. From then on the subject is the account's key: a later
// email change at the issuer does not move the account or its grants. When
// a concurrent first contact links the account first, the store answers
// ErrAlreadyExists and the account is read again by subject. It returns
// (nil, nil) when nothing is linked.
func (s *Server) linkPendingVisitor(ctx context.Context, fu *FederatedUserIdentity) (*store.User, error) {
	issuer := store.NormalizeFederatedIssuer(fu.IssuerURL())
	cfg, ok := s.visitorGrantIssuer(issuer)
	if !ok {
		return nil, nil
	}
	email := store.NormalizeFederatedEmail(fu.Email())
	if email == "" {
		return nil, nil
	}
	if verified, present := fu.EmailVerifiedClaim(); present && !verified {
		return nil, nil
	}
	if !visitorEmailAllowedByIssuer(cfg, email) {
		return nil, nil
	}
	pending, err := s.store.GetFederatedUserByEmail(ctx, issuer, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pending.Status != store.UserStatusPending {
		return nil, nil
	}

	var linked *store.User
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		u, err := tx.LinkPendingFederatedUser(ctx, pending.ID, issuer, fu.Subject(), email)
		if err != nil {
			return err
		}
		after, err := json.Marshal(map[string]string{
			"issuer":  issuer,
			"email":   email,
			"subject": fu.Subject(),
		})
		if err != nil {
			return fmt.Errorf("visitor link audit summary: %w", err)
		}
		record := &store.MutationAuditRecord{
			MutationType:        mutationTypeVisitorLinked,
			TargetType:          "user",
			TargetID:            u.ID,
			BeforeSummary:       store.UserStatusPending,
			AfterSummary:        string(after),
			Timestamp:           time.Now(),
			ActorPrincipalKind:  string(PrincipalKindVisitor),
			ActorPrincipalID:    u.ID,
			ActorCredentialType: string(CredentialKindFederation),
		}
		auditActorFromContext(ctx).ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return err
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
	if errors.Is(err, store.ErrInvalidInput) {
		// The account stopped being pending or changed between the read
		// and the link; nothing was linked.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return linked, nil
}

// visitorRouteRefusalMessage is the one response message for every route a
// visitor is refused, so the response does not distinguish routes.
const visitorRouteRefusalMessage = "You don't have access to this resource"

// refuseVisitorRequest is routeGuard's visitor check. A request whose
// identity is a *VisitorIdentity is admitted only on a route whose metadata
// sets VisitorAllowed; on every other route, whatever its classification,
// it is refused with the same 403 before the handler runs. A route that
// does not set the flag refuses visitors. Requests with any other identity
// are left alone. It writes the refusal and returns true when it refused.
func refuseVisitorRequest(w http.ResponseWriter, r *http.Request, meta RouteMetadata) bool {
	if !isVisitorIdentity(GetIdentityFromContext(r.Context())) {
		return false
	}
	if meta.VisitorAllowed {
		return false
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, visitorRouteRefusalMessage, nil)
	return true
}
