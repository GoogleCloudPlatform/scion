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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Top-level, scope-addressed GCP service account routes (P4 item C).
//
// /api/v1/projects/{id}/gcp-service-accounts can only ever name one project's
// accounts, which was sufficient while a project was the only thing an account
// could belong to. Hub-scoped accounts have no project to be nested under, so
// they need a route that takes the scope as a parameter instead of encoding it
// in the path. The nested routes stay exactly as they are: they remain the
// natural address for a project's own accounts and every existing client uses
// them.
//
// The API contract here is sa-arch's, binding on P4 and P5:
//   - flat /api/v1/gcp-service-accounts, param spelled scopeId
//   - hub scope is the value "hub", matching store.ScopeHub. It is deliberately
//     NOT normalised to "global" to match TemplateScopeGlobal -- the two
//     vocabularies stay distinct rather than being papered over here
//   - scopeId is omitted on hub scope and resolved by the server

// gcpScopeRequest is a parsed and validated scope selector from the query
// string.
type gcpScopeRequest struct {
	scope   string
	scopeID string

	// includeHubScoped widens a project-scoped read to also return hub-scoped
	// accounts. Only meaningful with scope=project.
	includeHubScoped bool
}

// parseGCPScopeRequest reads scope/scopeId/includeHubScoped from the query
// string and validates them, writing the error response itself and returning
// false when the caller must stop.
//
// Validation, not coercion. Every rejected combination below could instead be
// silently repaired -- default the scope, ignore the stray parameter, overwrite
// the client's scopeId -- and each repair would turn a caller's mistake into a
// wrong answer delivered with a 200. The specific hazard is a request that
// means one thing to the client and another to the server: a hub-scope read
// carrying a stale scopeId, quietly ignored, returns a list the client believes
// is filtered.
func (s *Server) parseGCPScopeRequest(w http.ResponseWriter, r *http.Request) (gcpScopeRequest, bool) {
	query := r.URL.Query()
	scope := query.Get("scope")
	scopeID := query.Get("scopeId")
	_, scopeIDPresent := query["scopeId"]
	includeHubScoped := query.Get("includeHubScoped") == "true"

	// scope is required rather than defaulting to "everything". An unfiltered
	// list here would be a cross-project enumeration of every service account
	// on the hub, which is not something any existing route offers and not
	// something this phase should quietly introduce.
	switch scope {
	case "":
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"missing required parameter: scope (expected \"project\" or \"hub\")", nil)
		return gcpScopeRequest{}, false

	case store.ScopeProject:
		if scopeID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"scopeId is required when scope=project", nil)
			return gcpScopeRequest{}, false
		}

	case store.ScopeHub:
		// The hub's scope ID is the hub instance ID, which the server knows and
		// the client does not. Accepting one from the client would let a
		// request name a hub that is not this one; the server resolving it
		// makes that unrepresentable rather than merely discouraged.
		if scopeIDPresent {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"scopeId must be omitted when scope=hub; the server resolves it", nil)
			return gcpScopeRequest{}, false
		}

		// Resolved for capability computation and, once P4 item A opens the
		// write path, for the value stored on a new hub-scoped account.
		//
		// INVARIANT (sa-arch, binding across all three tracks): "hub-scoped" is
		// determined by Scope ALONE. No code compares a service account's
		// ScopeID against the hub ID. On a hub-scoped account ScopeID is
		// PROVENANCE -- a record of which hub instance registered it -- and
		// never a predicate. s.hubID comes from config or a hostname hash and
		// is not stable across a redeploy, so a filter keyed on it would orphan
		// every hub-scoped account the first time a hostname changed, and would
		// do it silently, as an empty list.
		//
		// It is still written rather than left empty: an empty ScopeID collides
		// with the parentless overload in #604.
		scopeID = s.hubID

	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"invalid scope: expected \"project\" or \"hub\"", nil)
		return gcpScopeRequest{}, false
	}

	// Rejected rather than ignored: with scope=hub the flag is already implied,
	// and with any other scope it asks for a union that is not defined. Either
	// way the client has said something it does not mean, and silence would
	// hide that.
	if includeHubScoped && scope != store.ScopeProject {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"includeHubScoped is only valid with scope=project", nil)
		return gcpScopeRequest{}, false
	}

	return gcpScopeRequest{scope: scope, scopeID: scopeID, includeHubScoped: includeHubScoped}, true
}

// handleGCPServiceAccounts handles /api/v1/gcp-service-accounts.
func (s *Server) handleGCPServiceAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listGCPServiceAccountsScoped(w, r)
	case http.MethodPost:
		s.createGCPServiceAccountScoped(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// listGCPServiceAccountsScoped lists service accounts for an explicit scope.
func (s *Server) listGCPServiceAccountsScoped(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, ok := s.parseGCPScopeRequest(w, r)
	if !ok {
		return
	}

	filter := store.GCPServiceAccountFilter{
		Scope:            req.scope,
		IncludeHubScoped: req.includeHubScoped,
	}
	if req.scope == store.ScopeProject {
		filter.ScopeID = req.scopeID

		// A missing project is a 404 rather than an empty list. The two are
		// indistinguishable to a caller otherwise, and a typo'd project ID
		// silently reading as "this project has no service accounts" is the
		// kind of answer that gets believed.
		if _, err := s.store.GetProject(ctx, req.scopeID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
	}
	// Note what is NOT set for hub scope: ScopeID stays empty, so the filter
	// matches on Scope alone. This is deliberate and matches the OR arm of
	// IncludeHubScoped, so the hub list and the hub half of a project union
	// always agree. Pinning it to s.hubID instead would make the two disagree
	// whenever a stored row's ScopeID differed -- and hubID is derived from
	// config or a hostname hash, so it is not guaranteed stable across a
	// redeploy. Accounts orphaned by their own hub changing hostname is a worse
	// failure than a filter that is one term looser.

	sas, err := s.store.ListGCPServiceAccounts(ctx, filter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if sas == nil {
		sas = []store.GCPServiceAccount{}
	}

	// No read authorization call, matching the nested list exactly. That route
	// has never had one, and adding a check on only this surface would mean the
	// same data is readable or not depending on which URL asked for it -- so a
	// caller denied here would simply use the other route. The gap is real and
	// belongs to the route-authz manifest (#598); what this phase must not do
	// is create a second, differently-behaved door to the same rows.
	identity := GetIdentityFromContext(ctx)

	items := make([]GCPServiceAccountWithCapabilities, len(sas))
	if identity != nil {
		resources := make([]Resource, len(sas))
		for i := range sas {
			resources[i] = gcpServiceAccountResource(&sas[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "gcp_service_account")
		for i := range sas {
			items[i] = GCPServiceAccountWithCapabilities{GCPServiceAccount: sas[i], Cap: caps[i]}
		}
	} else {
		for i := range sas {
			items[i] = GCPServiceAccountWithCapabilities{GCPServiceAccount: sas[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, req.scope, req.scopeID, "gcp_service_account")
	}

	// MintQuota is omitted here. It is a per-project quota surfaced for the
	// project settings view; this route is scope-general and has no single
	// project to report against when scope=hub.
	writeJSON(w, http.StatusOK, ListGCPServiceAccountsResponse{
		Items:        items,
		Capabilities: scopeCap,
	})
}

// handleGCPServiceAccountByID handles /api/v1/gcp-service-accounts/{id} and
// /api/v1/gcp-service-accounts/{id}/verify.
//
// WHY THIS ROUTE EXISTS. A hub-scoped account has no project, so the nested
// address /api/v1/projects/{pid}/gcp-service-accounts/{id} can only reach one
// by borrowing an unrelated project's ID -- which works today only because
// ReachableFromProject returns true from everywhere for hub scope. A UI
// rendering a hub-level account detail page would have to invent a project to
// name it with, and a CLI outside any project could not name it at all.
//
// IT NEEDS LESS AUTHORIZATION THAN THE NESTED ROUTE, NOT MORE (sa-arch).
// authorizeGCPServiceAccount takes no project coordinate: it switches on the
// account's own scope. The project ID in the nested path feeds only
// ReachableFromProject, which is a ROUTING check -- "may this account be
// addressed from here" -- not a permission one. Dropping the project from the
// address therefore drops a routing question that has no answer for a
// parentless account, and leaves the authorization exactly as it was.
//
// No POST to a member and no mint. Creation is the flat COLLECTION's business
// (and hub-scoped creation is still refused there), and mint is a per-project
// quota operation with no meaning at hub scope.
//
// Both fall out of the dispatch below as a 405 with no store lookup at all,
// including /api/v1/gcp-service-accounts/mint -- "mint" parses as an account
// ID, not as the collection-level action the NESTED dispatcher makes it. That
// is the trap in this handler: copying the nested dispatcher would give the
// flat route a mint endpoint at hub scope. The 405 arrives before any lookup,
// so it also cannot be used to probe whether an account named "mint" exists.
func (s *Server) handleGCPServiceAccountByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/gcp-service-accounts/")
	if rest == "" {
		NotFound(w, "GCP Service Account")
		return
	}

	parts := strings.SplitN(rest, "/", 2)
	saID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "verify" && r.Method == http.MethodPost:
		s.verifyGCPServiceAccountByID(w, r, saID)
	case action != "":
		// Covers /mint too. The nested route treats "mint" as a
		// collection-level action; here it is an unknown action on an account
		// whose ID happens to read as "mint", and it must stay that way.
		NotFound(w, "GCP Service Account action")
	case r.Method == http.MethodGet:
		s.getGCPServiceAccountByID(w, r, saID)
	case r.Method == http.MethodDelete:
		s.deleteGCPServiceAccountByID(w, r, saID)
	default:
		MethodNotAllowed(w)
	}
}

// loadParentlessGCPServiceAccount loads an account for the flat by-id route and
// enforces that this route serves PARENTLESS accounts only -- hub scope and
// user scope. A project-scoped account is not found here; it has a project, so
// it has a nested address, and that address is the one that carries the
// project-level authorization the nested handlers apply.
//
// THE 404 IS BEFORE THE AUTHORIZATION CALL AND MUST STAY THERE (sa-arch). A
// 403 in this position would confirm that an account with this ID exists to a
// caller who has no other way to establish that -- the flat route takes no
// project, so there is no prior step in which the caller had to already know
// where the account lives. Ordering the checks the other way round turns this
// route into an existence oracle for every project-scoped account on the hub.
// That is also why the refusal is the same NotFound the missing-row branch
// writes, with no detail distinguishing "wrong scope" from "no such account".
//
// THIS FUNCTION TESTS SCOPE AND NOTHING ELSE. It does not ask who the caller is
// or who created the account -- that is authorizeGCPServiceAccountFlat's job,
// and a copy of it here would be a second description of who may do what,
// drifting from the first. Project scope is a ROUTING fact: such an account has
// a nested address that carries the project-level authorization, so it does not
// need and must not get a second, project-free one.
func (s *Server) loadParentlessGCPServiceAccount(w http.ResponseWriter, r *http.Request, saID string) (*store.GCPServiceAccount, bool) {
	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}

	if sa.Scope == store.ScopeProject {
		NotFound(w, "GCP Service Account")
		return nil, false
	}

	return sa, true
}

// authorizeGCPServiceAccountFlat renders a policy verdict for the flat by-id
// route, where a refusal is sometimes a 404 and sometimes a 403.
//
// THE RULE, WHICH IS WHAT IS ENCODED HERE (sa-arch): render 404 when the caller
// COULD NOT OTHERWISE HAVE ESTABLISHED that the account exists; render 403 when
// they could. Applied per scope it does not come out uniform, and the
// non-uniformity is the correct answer rather than an inconsistency to tidy.
//
// A per-scope table of statuses would be easier to read and would be the wrong
// thing to write down, because the table is a CONSEQUENCE of the rule and the
// premises underneath it are expected to move -- see the hub arm. Encode the
// map and a premise shifting under it is invisible; encode the rule, with each
// arm naming the specific path that makes existence establishable, and the
// sentence that has become false is sitting right there to be read.
//
// The policy question itself is not answered here. It is answered once, in
// gcpServiceAccountVerdict, which both routes call.
func (s *Server) authorizeGCPServiceAccountFlat(w http.ResponseWriter, r *http.Request, sa *store.GCPServiceAccount, action Action) bool {
	verdict, err := s.gcpServiceAccountVerdict(r.Context(), sa, action)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if verdict.allowed {
		return true
	}

	// NO IDENTITY -> 404, BEFORE THE SCOPE SWITCH. #42, found by sa-arch.
	//
	// THE INVARIANT, WHICH IS THE THING TO TEST FOR AND THE THING THAT SURVIVES
	// A REWRITE OF THIS FUNCTION: an identity-less caller must not be able to
	// tell two scopes apart by status code.
	//
	// The rest of this comment is the CAUSE, which is only the reason the
	// invariant was broken in one particular arrangement of the code (aid-em).
	// Read the sentence above; the paragraphs below explain why it once failed.
	//
	// Without this arm the switch below runs anyway and answers 403 for hub
	// scope, 404 for user scope -- which is precisely the leak the noIdentity
	// flag exists to prevent. A caller carrying no user identity could tell the
	// two apart by status alone, on a route where it has established nothing.
	//
	// AND THE CALLER IS NOT HYPOTHETICAL: GetUserIdentityFromContext returns nil
	// for an AGENT, and agents authenticate perfectly well -- UnifiedAuthMiddle-
	// ware admits them -- so every authenticated agent reaching this route lands
	// here. The hub arm's 403 rests on "every user is joined to hub-members on
	// login", which is false for agents: agent principals never include
	// hub-members. It would have been granting a disclosure on a premise
	// explicitly false for the caller receiving it. (The reason string is also
	// empty on a noIdentity verdict, so that 403 carried no message at all.)
	//
	// 404, AND THE NESTED RENDERER NOW ANSWERS 404 HERE TOO (#45). This commit
	// originally reasoned that the two routes should differ, because a nested
	// caller supplied the project themselves. That is true of the PROJECT and
	// beside the point: what the refusal discloses is the ACCOUNT -- its
	// existence and its scope -- which a nested caller supplied nothing about.
	// The invariant is a property of the resource, so it is enforced in both
	// renderers or in neither. See authorizeGCPServiceAccount's doc.
	if verdict.noIdentity {
		NotFound(w, "GCP Service Account")
		return false
	}

	switch sa.Scope {
	case store.ScopeHub:
		// 403. Existence is ALREADY ESTABLISHABLE by any authenticated caller:
		// every user is joined to hub-members on login, and the seeded
		// hub-member-read-all policy grants read+list on ResourceType "*" at
		// hub scope, so listing hub-scoped accounts is open to all of them.
		// There is no existence left to protect, and a 404 would be a lie that
		// protects nothing while costing the debuggability of exactly the
		// surface this phase is adding.
		//
		// THIS ARM'S PREMISE IS THE ONE EXPECTED TO MOVE. If hub-scoped
		// creation becomes admin-gated (#19), the listability it rests on may
		// be narrowed in the same change -- and then this arm should become a
		// 404 and this comment is what says so. Check hub-member-read-all
		// before assuming this line is still right.
		writeError(w, http.StatusForbidden, ErrCodeForbidden, verdict.reason, nil)

	case store.ScopeUser:
		// 404. Nothing makes existence establishable: a user-scoped account is
		// not reachable from any project, so the nested route has always 404'd
		// it, and this route is the first HTTP address one has ever had. A 403
		// here would therefore be a brand-new oracle created by this route
		// rather than one inherited from an older surface.
		//
		// It costs no caller an operation. The verdict's user arm denies
		// everyone but the creator, admins included, so this changes the status
		// seen by callers who were refused either way.
		NotFound(w, "GCP Service Account")

	default:
		// 404, fail-closed. Project scope never reaches here -- the loader
		// already refused it -- and any scope added later arrives with no
		// disclosure analysis done, so it gets the answer that reveals least
		// until someone does that analysis and adds an arm.
		NotFound(w, "GCP Service Account")
	}

	return false
}

// getGCPServiceAccountByID returns one parentless account with the caller's
// capabilities on it.
//
// The capabilities are the point of the response shape, not decoration. A
// detail view that renders Delete and Verify from the account's EXISTENCE
// offers buttons that 403 on click, and for a hub-scoped account the caller
// who gets that 403 is the common case, not the edge one. Returning the
// computed actions lets the client render authority instead of guessing at it.
func (s *Server) getGCPServiceAccountByID(w http.ResponseWriter, r *http.Request, saID string) {
	sa, ok := s.loadParentlessGCPServiceAccount(w, r, saID)
	if !ok {
		return
	}

	if !s.authorizeGCPServiceAccountFlat(w, r, sa, ActionRead) {
		return
	}

	// A read check on EVERY scope this route serves, where the nested GET runs
	// one for hub scope only. That is not a tightening of the nested route's
	// behaviour by the back door: the scope it leaves unchecked is project
	// scope, and project scope 404s above. For user scope the check is the
	// whole reason the route is safe to expose -- authorizeGCPServiceAccount's
	// user arm is what stops one user reading another's account, and the
	// nested route never had to answer that question because a user-scoped
	// account is not reachable from any project at all.
	item := GCPServiceAccountWithCapabilities{GCPServiceAccount: *sa}
	if identity := GetIdentityFromContext(r.Context()); identity != nil {
		item.Cap = s.authzService.ComputeCapabilities(r.Context(), identity, gcpServiceAccountResource(sa))
	}

	writeJSON(w, http.StatusOK, item)
}

// deleteGCPServiceAccountByID removes one parentless account.
//
// DELETING A HUB-SCOPED ACCOUNT REACHES THE CREATOR GRANT (sa-arch). Whoever
// created a hub-wide credential can destroy it, admin or not, because the
// owner grant matches the resource. That is a property of the existing policy
// set and this route neither introduces nor widens it -- the nested DELETE has
// the same reach today. It is recorded here because this route makes the
// operation easy to find, and because the client-side consequence is binding:
// render the Delete affordance from the computed capabilities, never from the
// account being visible.
func (s *Server) deleteGCPServiceAccountByID(w http.ResponseWriter, r *http.Request, saID string) {
	sa, ok := s.loadParentlessGCPServiceAccount(w, r, saID)
	if !ok {
		return
	}

	if !s.authorizeGCPServiceAccountFlat(w, r, sa, ActionDelete) {
		return
	}

	if err := s.store.DeleteGCPServiceAccount(r.Context(), saID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Invalidate cached actAs decisions for the deleted SA so that any
	// subsequent check against this email goes to the inner checker.
	// Mirrors the project-nested delete in handlers_gcp_identity.go.
	s.invalidateActAsCache(sa.Email)

	w.WriteHeader(http.StatusNoContent)
}

// verifyGCPServiceAccountByID re-runs the impersonation check for one
// parentless account. The body is shared with the nested route so the two
// cannot drift; see runGCPServiceAccountVerification.
func (s *Server) verifyGCPServiceAccountByID(w http.ResponseWriter, r *http.Request, saID string) {
	sa, ok := s.loadParentlessGCPServiceAccount(w, r, saID)
	if !ok {
		return
	}

	if !s.authorizeGCPServiceAccountFlat(w, r, sa, ActionVerify) {
		return
	}

	s.runGCPServiceAccountVerification(w, r, sa)
}

// createGCPServiceAccountScoped registers a service account at an explicit
// scope.
func (s *Server) createGCPServiceAccountScoped(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseGCPScopeRequest(w, r)
	if !ok {
		return
	}

	if req.includeHubScoped {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"includeHubScoped is not valid on create", nil)
		return
	}

	switch req.scope {
	case store.ScopeProject:
		// Same handler as the nested route, so the two addresses for the same
		// operation cannot drift apart in validation, authorization, or the
		// auto-verify step that follows creation.
		s.createGCPServiceAccount(w, r, req.scopeID)

	case store.ScopeHub:
		s.createHubScopedGCPServiceAccount(w, r)

	default:
		// parseGCPScopeRequest admits no other value; this is here so that
		// widening it later cannot silently fall through to a success path.
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"unsupported scope for service account creation", nil)
	}
}

// createHubScopedGCPServiceAccount registers a BYO (bring-your-own) hub-scoped
// service account. This is D7: non-admin users may register an SA resource by
// providing the email of an SA they own/control. The admin gate is on MINTING,
// not on BYO registration.
//
// Authorization: any current hub member may register a hub-scoped SA. The
// assignment gate (authorizeSAAssignment + mode coupling) prevents the SA from
// being assigned until gcpIamCheckMode=enforce and the caller passes actAs.
func (s *Server) createHubScopedGCPServiceAccount(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	var req createGCPServiceAccountRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body: "+err.Error(), nil)
		return
	}

	if req.Email == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "missing required field(s): email", nil)
		return
	}

	if req.ProjectID == "" {
		req.ProjectID = projectIDFromServiceAccountEmail(req.Email)
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"could not infer projectId from email; please provide it explicitly", nil)
		return
	}

	// Authorization: current hub member may register (BYO) a hub-scoped SA.
	// Users with gcp_service_account.create permission bypass the hub-member
	// check; others must be current hub members.
	isAdmin := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "gcp_service_account", ID: "hub"},
		Action:     Action("create"),
		Permission: "gcp_service_account.create",
	}).Allowed
	if !isAdmin {
		if !s.authzService.isCurrentHubMember(r.Context(), user.ID()) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You must be a hub member to register a hub-scoped service account", nil)
			return
		}
	}

	sa := &store.GCPServiceAccount{
		ID:            uuid.New().String(),
		Scope:         store.ScopeHub,
		ScopeID:       s.HubID(),
		Email:         req.Email,
		ProjectID:     req.ProjectID,
		DisplayName:   req.DisplayName,
		DefaultScopes: req.Scopes,
		CreatedBy:     user.ID(),
		CreatedAt:     time.Now(),
	}

	if len(sa.DefaultScopes) == 0 {
		sa.DefaultScopes = []string{"https://www.googleapis.com/auth/cloud-platform"}
	}

	if err := s.store.CreateGCPServiceAccount(r.Context(), sa); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a service account with this email already exists", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("hub-scoped GCP SA registered (BYO)",
		"sa_id", sa.ID, "email", sa.Email, "createdBy", user.ID())

	// Auto-verify impersonation after registration
	resp := createGCPServiceAccountResponse{GCPServiceAccount: *sa}
	if s.gcpTokenGenerator != nil {
		if err := s.gcpTokenGenerator.VerifyImpersonation(r.Context(), sa.Email); err != nil {
			sa.Verified = false
			sa.VerificationStatus = store.GCPVerificationFailed
			sa.VerificationError = err.Error()
			resp.VerificationFailed = true
			resp.VerificationDetails = &verificationFailedDetails{
				HubServiceAccountEmail: s.gcpTokenGenerator.ServiceAccountEmail(),
				TargetEmail:            sa.Email,
			}
		} else {
			sa.Verified = true
			sa.VerifiedAt = time.Now()
			sa.VerificationStatus = store.GCPVerificationVerified
		}
		if updateErr := s.store.UpdateGCPServiceAccount(r.Context(), sa); updateErr != nil {
			slog.Error("failed to update SA verification status", "sa_id", sa.ID, "error", updateErr)
		}
		resp.GCPServiceAccount = *sa
	}

	writeJSON(w, http.StatusCreated, resp)
}
