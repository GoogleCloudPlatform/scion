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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// handleProjectGCPServiceAccounts handles /api/v1/projects/{projectId}/gcp-service-accounts
func (s *Server) handleProjectGCPServiceAccounts(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		s.listGCPServiceAccounts(w, r, projectID)
	case http.MethodPost:
		s.createGCPServiceAccount(w, r, projectID)
	default:
		MethodNotAllowed(w)
	}
}

// handleProjectGCPServiceAccountByID handles /api/v1/projects/{projectId}/gcp-service-accounts/{id}[/action]
func (s *Server) handleProjectGCPServiceAccountByID(w http.ResponseWriter, r *http.Request, projectID, saPath string) {
	// Handle collection-level actions first
	if saPath == "mint" && r.Method == http.MethodPost {
		s.mintGCPServiceAccount(w, r, projectID)
		return
	}

	parts := strings.SplitN(saPath, "/", 2)
	saID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if action == "verify" && r.Method == http.MethodPost {
		s.verifyGCPServiceAccount(w, r, projectID, saID)
		return
	}

	if action != "" {
		NotFound(w, "GCP Service Account action")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getGCPServiceAccount(w, r, projectID, saID)
	case http.MethodDelete:
		s.deleteGCPServiceAccount(w, r, projectID, saID)
	default:
		MethodNotAllowed(w)
	}
}

type createGCPServiceAccountRequest struct {
	Email       string   `json:"email"`
	ProjectID   string   `json:"projectId"`
	DisplayName string   `json:"displayName"`
	Scopes      []string `json:"defaultScopes,omitempty"`
}

type verificationFailedDetails struct {
	HubServiceAccountEmail string `json:"hubServiceAccountEmail"`
	TargetEmail            string `json:"targetEmail"`
}

// The reachability predicate these handlers use is
// (*store.GCPServiceAccount).ReachableFromProject. It lives on the store type
// rather than here because the same question is asked outside this package --
// see pkg/lifecyclehooks/validate.go -- and packages that cannot import hub
// would otherwise have to reimplement it. Read its doc comment before changing
// any caller: it answers visibility only, and the authorization that must
// follow it for a hub-scoped account is authorizeGCPServiceAccount below.

// gcpSAVerdict is the answer to the POLICY question about a service account:
// may this caller perform this action on it. It carries no HTTP status,
// because the status is not a property of the verdict.
//
// See gcpServiceAccountVerdict for why the two are separated.
type gcpSAVerdict struct {
	allowed bool

	// reason is the refusal, phrased for a 403 body. Empty when allowed, and
	// empty when noIdentity is set -- an unauthenticated caller is told
	// nothing.
	reason string

	// noIdentity distinguishes "no user on this request" from a scope arm's
	// denial, so a renderer can answer it generically instead of leaking which
	// arm it would have hit.
	noIdentity bool
}

// gcpServiceAccountVerdict decides whether the caller may perform action on sa,
// and WRITES NOTHING.
//
// THE SPLIT IS THE POINT, AND IT IS sa-arch'S FINDING. This function used to
// decide and write in one step. That fused two questions that are genuinely
// different:
//
//   - POLICY: may this caller do this? Answered here, once, for every route.
//   - DISCLOSURE: should a refusal read as 403 or 404? That depends on whether
//     the caller could have established the account's existence some other
//     way, which is a property OF THE ROUTE and not of the policy.
//
// While the two were fused, a route that needed a different disclosure answer
// had no way to get one except by re-deriving the policy test ahead of the
// call -- which is exactly the second, drifting description of who-may-do-what
// that the single function existed to prevent. The flat by-id route needs 404
// for user scope, so the split is what lets it have that without owning a
// second copy of the creator test.
//
// The scope split inside is the older substance and is unchanged. While every
// SA was project-scoped, a project-level ActionManage check was a complete
// statement of authority over one. A hub-scoped SA is reachable from every
// project, so that same check would let any project's owner delete a credential
// the whole hub depends on. Hub-scoped SAs are therefore checked against the SA
// resource itself, which gcpServiceAccountResource leaves parentless for
// non-project scopes (P0.2) so that only a hub-scoped policy can match it. That
// containment relies on matchesResource treating a parentless resource as
// outside every project-scoped policy's reach — the #595 fix, without which a
// project-scoped policy would match a hub-scoped SA and this branch would not
// hold.
func (s *Server) gcpServiceAccountVerdict(ctx context.Context, sa *store.GCPServiceAccount, action Action) (gcpSAVerdict, error) {
	user := GetUserIdentityFromContext(ctx)
	if user == nil {
		return gcpSAVerdict{noIdentity: true}, nil
	}

	switch sa.Scope {
	case store.ScopeHub:
		decision := s.authzService.CheckAccess(ctx, user, gcpServiceAccountResource(sa), action)
		if !decision.Allowed {
			return gcpSAVerdict{
				reason: "You don't have permission to manage hub-scoped GCP service accounts",
			}, nil
		}

	case store.ScopeProject:
		// Unchanged from the pre-Goal-2 behaviour, deliberately: this phase adds
		// hub scope, it does not retune who may manage a project's own SAs.
		project, err := s.store.GetProject(ctx, sa.ScopeID)
		if err != nil {
			return gcpSAVerdict{}, err
		}
		decision := s.authzService.CheckAccess(ctx, user, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionManage)
		if !decision.Allowed {
			return gcpSAVerdict{
				reason: "You don't have permission to manage GCP service accounts in this project",
			}, nil
		}

	case store.ScopeUser:
		// THE ONLY CREATOR TEST FOR USER-SCOPED ACCOUNTS. No admin bypass:
		// every caller but the creator is denied, admins included. Both routes
		// reach this line; neither has a copy of it.
		if sa.CreatedBy != user.ID() {
			return gcpSAVerdict{
				reason: "You don't have permission to manage another user's GCP service account",
			}, nil
		}

	default:
		return gcpSAVerdict{
			reason: "Management is not supported for this service account scope",
		}, nil
	}

	return gcpSAVerdict{allowed: true}, nil
}

// authorizeGCPServiceAccount renders a verdict for the PROJECT-NESTED routes:
// an IDENTIFIED caller's refusal is a 403, exactly as before the
// verdict/renderer split. An identity-less caller gets 404, the same as the
// flat route.
//
// Unchanged behaviour is the requirement for the identified arms, not an
// accident of the refactor. To reach a nested by-id route a caller has already
// named the project the account lives in, so a 403 discloses nothing they did
// not supply themselves. The flat route's renderer is
// authorizeGCPServiceAccountFlat.
//
// THE noIdentity ARM IS THE EXCEPTION, AND IT IS #45, found by sa-arch running
// the #42 invariant against the arm the #42 commit PINNED. Same sentence, one
// route over:
//
//	AN IDENTITY-LESS CALLER MUST NOT BE ABLE TO TELL TWO SCOPES APART BY
//	STATUS CODE.
//
// The rest of this comment is the cause. The "403 discloses nothing they did
// not supply themselves" justification above is TRUE, and it is about the
// PROJECT. What a 403 here discloses is the ACCOUNT: that the id exists, and
// -- because ReachableFromProject returns true unconditionally for hub scope
// (store/models.go, ScopeHub arm) while project scope requires ScopeID ==
// projectID -- which scope it has. Right sentence, wrong noun. Before this
// arm, nested DELETE answered an identity-less caller naming project P:
//
//	id does not exist           -> 404
//	project-scoped SA in Q != P -> 404 (unreachable)
//	project-scoped SA in P      -> 403
//	hub-scoped SA               -> 403, from EVERY P
//
// so the status separated existence from non-existence, hub scope from project
// scope, and -- by iterating P -- named the owning project of a project-scoped
// account. All three refusals now read 404, which is also what the
// unreachable and the nonexistent cases already read.
//
// CHANGING THIS ARM CANNOT ALTER ANY IDENTIFIED CALLER'S EXPERIENCE, which is
// why it does not breach the unchanged-behaviour requirement above:
// gcpServiceAccountVerdict sets noIdentity exactly when
// GetUserIdentityFromContext returns nil, so anyone with a user identity
// leaves through the scope arms below with their reason string intact. The
// callers inside this arm are agents, which authenticate but carry no user.
func (s *Server) authorizeGCPServiceAccount(w http.ResponseWriter, r *http.Request, sa *store.GCPServiceAccount, action Action) bool {
	verdict, err := s.gcpServiceAccountVerdict(r.Context(), sa, action)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if verdict.allowed {
		return true
	}
	if verdict.noIdentity {
		NotFound(w, "GCP Service Account")
		return false
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, verdict.reason, nil)
	return false
}

type createGCPServiceAccountResponse struct {
	store.GCPServiceAccount
	VerificationFailed  bool                       `json:"verificationFailed,omitempty"`
	VerificationDetails *verificationFailedDetails `json:"verificationDetails,omitempty"`
}

func (s *Server) createGCPServiceAccount(w http.ResponseWriter, r *http.Request, projectID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	var req createGCPServiceAccountRequest
	if err := readJSON(r, &req); err != nil {
		slog.Debug("GCP SA create: failed to parse request body",
			"project_id", projectID,
			"error", err,
			"content_type", r.Header.Get("Content-Type"),
		)
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
		slog.Debug("GCP SA create: missing required fields",
			"project_id", projectID,
			"has_email", req.Email != "",
			"has_project_id", req.ProjectID != "",
		)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"could not infer projectId from email; please provide it explicitly", nil)
		return
	}

	// Verify project exists
	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorization: project owners and admins can manage GCP service accounts
	decision := s.authzService.CheckAccess(r.Context(), user, Resource{
		Type:    "project",
		ID:      project.ID,
		OwnerID: project.OwnerID,
	}, ActionManage)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to manage GCP service accounts in this project", nil)
		return
	}

	sa := &store.GCPServiceAccount{
		ID:            uuid.New().String(),
		Scope:         store.ScopeProject,
		ScopeID:       projectID,
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
				"a service account with this email already exists for this project", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Auto-verify impersonation after registration
	resp := createGCPServiceAccountResponse{GCPServiceAccount: *sa}
	if s.gcpTokenGenerator != nil {
		if err := s.gcpTokenGenerator.VerifyImpersonation(r.Context(), sa.Email); err != nil {
			sa.Verified = false
			sa.VerificationStatus = store.GCPVerificationFailed
			sa.VerificationError = err.Error()
			_ = s.store.UpdateGCPServiceAccount(r.Context(), sa)
			resp.GCPServiceAccount = *sa
			resp.VerificationFailed = true
			resp.VerificationDetails = &verificationFailedDetails{
				HubServiceAccountEmail: s.gcpTokenGenerator.ServiceAccountEmail(),
				TargetEmail:            sa.Email,
			}
		} else {
			sa.Verified = true
			sa.VerifiedAt = time.Now()
			sa.VerificationStatus = store.GCPVerificationVerified
			sa.VerificationError = ""
			_ = s.store.UpdateGCPServiceAccount(r.Context(), sa)
			resp.GCPServiceAccount = *sa
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

// GCPServiceAccountWithCapabilities wraps a service account with its per-item capabilities.
type GCPServiceAccountWithCapabilities struct {
	store.GCPServiceAccount
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// GCPMintQuotaInfo provides quota information for minted service accounts.
type GCPMintQuotaInfo struct {
	ProjectMinted int `json:"project_minted"`
	ProjectCap    int `json:"project_cap"` // 0 = unlimited
	GlobalMinted  int `json:"global_minted"`
	GlobalCap     int `json:"global_cap"`
}

// ListGCPServiceAccountsResponse is the response for listing GCP service accounts.
type ListGCPServiceAccountsResponse struct {
	Items        []GCPServiceAccountWithCapabilities `json:"items"`
	Capabilities *Capabilities                       `json:"_capabilities,omitempty"`
	MintQuota    *GCPMintQuotaInfo                   `json:"mint_quota,omitempty"`
}

func (s *Server) listGCPServiceAccounts(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	// This one route serves two callers with opposite needs. The assign picker
	// wants the project's accounts plus the hub-wide ones, because either can
	// be given to an agent. The project settings management view wants only the
	// project's, because listing hub-scoped accounts there offers the user rows
	// the view cannot edit. Neither is more correct, so the caller says which
	// it wants and the default stays project-only: every existing client keeps
	// its current response byte for byte.
	//
	// Unlike the top-level route there is no scope parameter to validate
	// against here -- the route is project-scoped by construction -- so the
	// flag is accepted unconditionally.
	includeHubScoped := r.URL.Query().Get("includeHubScoped") == "true"

	sas, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:            store.ScopeProject,
		ScopeID:          projectID,
		IncludeHubScoped: includeHubScoped,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if sas == nil {
		sas = []store.GCPServiceAccount{}
	}

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
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "project", projectID, "gcp_service_account")
	}

	// Include mint quota info when minting is configured
	var mintQuota *GCPMintQuotaInfo
	if s.gcpIAMAdmin != nil && s.config.GCPProjectID != "" {
		managed := true
		// Deliberately not widened by includeHubScoped. This counts against
		// GCPMintCapPerProject, so it must stay a count of what this project
		// minted; hub-scoped accounts are not charged to any one project and
		// including them would shrink every project's remaining quota by the
		// hub-wide total.
		projectCount, _ := s.store.CountGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
			Scope:   store.ScopeProject,
			ScopeID: projectID,
			Managed: &managed,
		})
		globalCount, _ := s.store.CountGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
			Managed: &managed,
		})
		mintQuota = &GCPMintQuotaInfo{
			ProjectMinted: projectCount,
			ProjectCap:    s.config.GCPMintCapPerProject,
			GlobalMinted:  globalCount,
			GlobalCap:     s.config.GCPMintCapGlobal,
		}
	}

	writeJSON(w, http.StatusOK, ListGCPServiceAccountsResponse{
		Items:        items,
		Capabilities: scopeCap,
		MintQuota:    mintQuota,
	})
}

func (s *Server) getGCPServiceAccount(w http.ResponseWriter, r *http.Request, projectID, saID string) {
	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if !sa.ReachableFromProject(projectID) {
		NotFound(w, "GCP Service Account")
		return
	}

	// Hub-scoped SAs get a read check. Project-scoped reads are left exactly as
	// they were — this handler has never had an authorization call, and adding
	// one here would deny ordinary project members who can read their project's
	// SAs today. That pre-existing gap is the route-authz manifest's problem
	// (#598), not this phase's.
	//
	// ⚠️ WHAT THIS CHECK ACTUALLY DENIES: not much, and knowing that is the
	// point. hub-member-read-all is ResourceType "*" with ScopeType "hub"
	// (seed.go, seedDefaultPoliciesAndGroups), and ScopeType "hub" matches
	// neither arm of the matchesResource scope switch (authz.go), so no scope
	// filter applies and it grants read on hub-scoped accounts to EVERY
	// logged-in user. The only callers this check refuses are identity-less
	// ones. Treat it as an authenticated-caller check, not as a restriction,
	// and do not add a hub-scoped WRITE gate on ActionRead or ActionList
	// expecting it to hold — it would be inert on arrival.
	//
	// #46, sa-arch's wording; the earlier text here claimed this check kept a
	// hub-wide credential from inheriting the gap above, which the wildcard
	// removes. WHO may read hub-scoped accounts is an open policy question with
	// ptone and is deliberately not changed here.
	if sa.Scope == store.ScopeHub {
		if !s.authorizeGCPServiceAccount(w, r, sa, ActionRead) {
			return
		}
	}

	writeJSON(w, http.StatusOK, sa)
}

func (s *Server) deleteGCPServiceAccount(w http.ResponseWriter, r *http.Request, projectID, saID string) {
	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if !sa.ReachableFromProject(projectID) {
		NotFound(w, "GCP Service Account")
		return
	}

	if !s.authorizeGCPServiceAccount(w, r, sa, ActionDelete) {
		return
	}

	if err := s.store.DeleteGCPServiceAccount(r.Context(), saID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Invalidate cached actAs decisions for the deleted SA so that any
	// subsequent check against this email goes to the inner checker.
	s.invalidateActAsCache(sa.Email)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyGCPServiceAccount(w http.ResponseWriter, r *http.Request, projectID, saID string) {
	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if !sa.ReachableFromProject(projectID) {
		NotFound(w, "GCP Service Account")
		return
	}

	if !s.authorizeGCPServiceAccount(w, r, sa, ActionVerify) {
		return
	}

	s.runGCPServiceAccountVerification(w, r, sa)
}

// runGCPServiceAccountVerification performs the impersonation check and
// persists its outcome. It assumes the caller has already located the account
// and authorized ActionVerify on it.
//
// Extracted so that the nested route and the flat by-id route share one body.
// The two differ only in how they find the account -- a project path versus a
// parentless one -- and everything after that point is the same operation. Two
// copies would drift, and the direction they would drift in is the bad one:
// this body writes verification state that the assign gate later trusts, so a
// copy that forgot to persist a FAILURE would leave an account reading as
// verified after a verification that did not pass.
func (s *Server) runGCPServiceAccountVerification(w http.ResponseWriter, r *http.Request, sa *store.GCPServiceAccount) {
	// Fail-closed: if no token generator is configured, we cannot verify.
	if s.gcpTokenGenerator == nil {
		writeError(w, http.StatusServiceUnavailable, "gcp_not_configured",
			"GCP token generation is not configured on this Hub", nil)
		return
	}

	// Attempt to verify impersonation via the GCP token generator
	if err := s.gcpTokenGenerator.VerifyImpersonation(r.Context(), sa.Email); err != nil {
		// Persist the failure status
		sa.Verified = false
		sa.VerificationStatus = store.GCPVerificationFailed
		sa.VerificationError = err.Error()
		_ = s.store.UpdateGCPServiceAccount(r.Context(), sa)

		details := map[string]interface{}{
			"hubServiceAccountEmail": s.gcpTokenGenerator.ServiceAccountEmail(),
			"targetEmail":            sa.Email,
		}
		writeError(w, http.StatusBadGateway, "gcp_verification_failed",
			"Failed to verify impersonation: "+err.Error(), details)
		return
	}

	sa.Verified = true
	sa.VerifiedAt = time.Now()
	sa.VerificationStatus = store.GCPVerificationVerified
	sa.VerificationError = ""

	if err := s.store.UpdateGCPServiceAccount(r.Context(), sa); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, sa)
}

// mintGCPServiceAccountRequest is the request body for POST .../gcp-service-accounts/mint.
type mintGCPServiceAccountRequest struct {
	AccountID      string `json:"account_id"`                  // Optional custom SA account ID (will be prefixed with scion-)
	DisplayName    string `json:"display_name"`                // Optional display name
	Description    string `json:"description"`                 // Optional description
	AllowSelfActAs *bool  `json:"allow_self_act_as,omitempty"` // nil = default true; grant SA serviceAccountUser on itself
}

// gcpSAAccountIDRegexp validates GCP SA account IDs: 6-30 chars, [a-z][a-z0-9-]*[a-z0-9].
var gcpSAAccountIDRegexp = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// slugifyAccountID converts a string to a valid GCP SA account ID component.
func slugifyAccountID(s string) string {
	s = strings.ToLower(s)
	// Replace non-alphanumeric chars with hyphens
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	// Collapse multiple hyphens
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	// Trim leading/trailing hyphens
	result = strings.Trim(result, "-")
	return result
}

// projectIDFromServiceAccountEmail extracts the GCP project ID from a
// service account email of the form <name>@<project>.iam.gserviceaccount.com.
func projectIDFromServiceAccountEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at < 0 {
		return ""
	}
	domain := email[at+1:]
	suffix := ".iam.gserviceaccount.com"
	if !strings.HasSuffix(domain, suffix) {
		return ""
	}
	return domain[:len(domain)-len(suffix)]
}

// generateRandomAccountID generates a random SA account ID: scion-{8-hex-chars}.
func generateRandomAccountID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "scion-" + hex.EncodeToString(b), nil
}

func (s *Server) mintGCPServiceAccount(w http.ResponseWriter, r *http.Request, projectID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	// Check that minting is configured
	if s.gcpIAMAdmin == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"GCP service account minting is not configured on this Hub", nil)
		return
	}

	hubGCPProjectID := s.config.GCPProjectID
	if hubGCPProjectID == "" {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"GCP project ID is not configured for service account minting", nil)
		return
	}

	var req mintGCPServiceAccountRequest
	if r.Body != nil {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body: "+err.Error(), nil)
			return
		}
	}

	// Verify project exists
	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorization: project owners and admins can mint GCP service accounts
	decision := s.authzService.CheckAccess(r.Context(), user, Resource{
		Type:    "project",
		ID:      project.ID,
		OwnerID: project.OwnerID,
	}, ActionManage)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to manage GCP service accounts in this project", nil)
		return
	}

	// Enforce per-project mint cap
	managed := true
	projectCount, err := s.store.CountGCPServiceAccounts(r.Context(), store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Managed: &managed,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if s.config.GCPMintCapPerProject > 0 && projectCount >= s.config.GCPMintCapPerProject {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			fmt.Sprintf("per-project mint limit reached (%d/%d)", projectCount, s.config.GCPMintCapPerProject), nil)
		return
	}

	// Enforce global mint cap
	if s.config.GCPMintCapGlobal > 0 {
		globalCount, err := s.store.CountGCPServiceAccounts(r.Context(), store.GCPServiceAccountFilter{
			Managed: &managed,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if globalCount >= s.config.GCPMintCapGlobal {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				fmt.Sprintf("global mint limit reached (%d/%d)", globalCount, s.config.GCPMintCapGlobal), nil)
			return
		}
	}

	// Generate or validate the account ID
	var accountID string
	if req.AccountID != "" {
		// Custom: prefix with scion-, slugify, validate
		slug := slugifyAccountID(req.AccountID)
		accountID = "scion-" + slug
	} else {
		// Auto-generate
		accountID, err = generateRandomAccountID()
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Validate against GCP rules: 6-30 chars, [a-z][a-z0-9-]*[a-z0-9]
	if len(accountID) < 6 || len(accountID) > 30 {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("account ID %q must be 6-30 characters (got %d)", accountID, len(accountID)), nil)
		return
	}
	if !gcpSAAccountIDRegexp.MatchString(accountID) {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("account ID %q must match [a-z][a-z0-9-]*[a-z0-9]", accountID), nil)
		return
	}

	// Build display name and description
	displayName := req.DisplayName
	if displayName == "" {
		displayName = fmt.Sprintf("Scion agent (%s)", project.Slug)
	}
	description := req.Description
	if description == "" {
		description = fmt.Sprintf("Minted by Scion Hub for project %s (ID: %s) by user %s", project.Slug, projectID, user.ID())
	}

	// Create the SA in GCP
	saEmail, _, err := s.gcpIAMAdmin.CreateServiceAccount(r.Context(), hubGCPProjectID, accountID, displayName, description)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "409") || strings.Contains(errStr, "alreadyExists") {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				fmt.Sprintf("service account %s already exists in project %s", accountID, hubGCPProjectID), nil)
			return
		}
		slog.Error("GCP SA mint: failed to create service account",
			"hub_gcp_project_id", hubGCPProjectID, "account_id", accountID, "error", err)
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
			"failed to create GCP service account: "+err.Error(), nil)
		return
	}

	// ================================================================
	// IAM mutations — all are REQUIRED. If any fails, the mint is a
	// failure: do NOT store Verified=true, best-effort delete the SA.
	// ================================================================

	// Helper to clean up on any required IAM mutation failure.
	cleanupAndFail := func(mutation string, mutErr error) {
		slog.Error("GCP SA mint: required IAM mutation failed",
			"mutation", mutation, "project_id", projectID,
			"sa_email", saEmail, "error", mutErr)
		if delErr := s.gcpIAMAdmin.DeleteServiceAccount(r.Context(), saEmail); delErr != nil {
			slog.Error("GCP SA mint: best-effort cleanup of orphaned SA failed",
				"sa_email", saEmail, "error", delErr)
		}
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
			"service account was created but required IAM grants failed; the account is not usable. Contact your administrator.", nil)
	}

	// 1. Token generator must be configured.
	if s.gcpTokenGenerator == nil {
		slog.Error("GCP SA mint: token generator not configured, cannot grant tokenCreator",
			"project_id", projectID, "sa_email", saEmail)
		if delErr := s.gcpIAMAdmin.DeleteServiceAccount(r.Context(), saEmail); delErr != nil {
			slog.Error("GCP SA mint: best-effort cleanup failed after missing token generator",
				"sa_email", saEmail, "error", delErr)
		}
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
			"service account was created but the Hub has no token generator configured; the account is not usable and has been removed", nil)
		return
	}

	hubEmail := s.gcpTokenGenerator.ServiceAccountEmail()
	if hubEmail == "" {
		slog.Error("GCP SA mint: hub service account email is empty, cannot grant tokenCreator",
			"project_id", projectID, "sa_email", saEmail)
		if delErr := s.gcpIAMAdmin.DeleteServiceAccount(r.Context(), saEmail); delErr != nil {
			slog.Error("GCP SA mint: best-effort cleanup failed after empty hub email",
				"sa_email", saEmail, "error", delErr)
		}
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
			"service account was created but the Hub service account email is not configured; the account is not usable and has been removed", nil)
		return
	}

	// Grant Hub SA tokenCreator on the minted SA.
	hubMember := "serviceAccount:" + hubEmail
	if err := retryIAMGrant(r.Context(), func() error {
		return s.gcpIAMAdmin.SetIAMPolicy(r.Context(), saEmail, hubMember, "roles/iam.serviceAccountTokenCreator")
	}); err != nil {
		cleanupAndFail("tokenCreator grant on minted SA", err)
		return
	}

	// Optionally grant the minted SA serviceAccountUser on itself so it
	// can be used as a project-default SA where agents create sub-agents
	// running as the same identity.
	allowSelfActAs := req.AllowSelfActAs == nil || *req.AllowSelfActAs
	if allowSelfActAs {
		saMember := "serviceAccount:" + saEmail
		if err := retryIAMGrant(r.Context(), func() error {
			return s.gcpIAMAdmin.SetIAMPolicy(r.Context(), saEmail, saMember, "roles/iam.serviceAccountUser")
		}); err != nil {
			cleanupAndFail("self serviceAccountUser grant on minted SA", err)
			return
		}
		slog.Info("GCP SA mint: self-actAs grant succeeded",
			"project_id", projectID, "sa_email", saEmail)
	}

	slog.Info("GCP SA mint: SA created and IAM grants succeeded",
		"project_id", projectID, "sa_email", saEmail, "hub_email", hubEmail,
		"self_act_as", allowSelfActAs)

	// Store the SA record — only reached when ALL required IAM mutations succeeded.
	sa := &store.GCPServiceAccount{
		ID:                 uuid.New().String(),
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              saEmail,
		ProjectID:          hubGCPProjectID,
		DisplayName:        displayName,
		DefaultScopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          user.ID(),
		CreatedAt:          time.Now(),
		Managed:            true,
		ManagedBy:          s.config.HubID,
	}

	if err := s.store.CreateGCPServiceAccount(r.Context(), sa); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a service account with this email already exists for this project", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Audit log the mint
	LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventMintSA,
		"", projectID, saEmail, sa.ID, true, "")

	slog.Info("GCP SA minted",
		"project_id", projectID, "sa_id", sa.ID, "email", saEmail,
		"account_id", accountID, "project", projectID, "user", user.ID(),
		"self_act_as", allowSelfActAs)

	writeJSON(w, http.StatusCreated, sa)
}

// GCPQuotaProjectInfo holds per-project mint quota info for the admin endpoint.
type GCPQuotaProjectInfo struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	Minted      int    `json:"minted"`
}

// GCPQuotaResponse is the response for GET /api/v1/admin/gcp-quota.
type GCPQuotaResponse struct {
	MintingConfigured bool                  `json:"minting_configured"`
	GCPProjectID      string                `json:"gcp_project_id,omitempty"`
	GlobalMinted      int                   `json:"global_minted"`
	GlobalCap         int                   `json:"global_cap"`
	PerProjectCap     int                   `json:"per_project_cap"`
	Projects          []GCPQuotaProjectInfo `json:"projects,omitempty"`
}

// handleAdminGCPQuota handles GET /api/v1/admin/gcp-quota.
func (s *Server) handleAdminGCPQuota(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	resp := GCPQuotaResponse{
		MintingConfigured: s.gcpIAMAdmin != nil && s.config.GCPProjectID != "",
		GCPProjectID:      s.config.GCPProjectID,
		GlobalCap:         s.config.GCPMintCapGlobal,
		PerProjectCap:     s.config.GCPMintCapPerProject,
	}

	if resp.MintingConfigured {
		managed := true
		globalCount, err := s.store.CountGCPServiceAccounts(r.Context(), store.GCPServiceAccountFilter{
			Managed: &managed,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		resp.GlobalMinted = globalCount

		// Get per-project breakdown
		allMinted, err := s.store.ListGCPServiceAccounts(r.Context(), store.GCPServiceAccountFilter{
			Managed: &managed,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		projectCounts := map[string]int{}
		for _, sa := range allMinted {
			projectCounts[sa.ScopeID]++
		}

		for projectID, count := range projectCounts {
			name := projectID
			if g, err := s.store.GetProject(r.Context(), projectID); err == nil {
				name = g.Name
			}
			resp.Projects = append(resp.Projects, GCPQuotaProjectInfo{
				ProjectID:   projectID,
				ProjectName: name,
				Minted:      count,
			})
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleAgentGCPToken handles POST /api/v1/agent/gcp-token.
// Called by the metadata sidecar to obtain a GCP access token for the agent's assigned SA.
func (s *Server) handleAgentGCPToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	start := time.Now()

	agent := GetAgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent authentication required", nil)
		return
	}

	// Rate limit check
	if s.gcpTokenRateLimiter != nil && !s.gcpTokenRateLimiter.Allow(agent.Subject) {
		if s.gcpTokenMetrics != nil {
			s.gcpTokenMetrics.RecordRateLimitRejection()
		}
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "rate limit exceeded for GCP token requests", nil)
		return
	}

	// Look up agent's GCP identity assignment
	agentRecord, err := s.store.GetAgent(r.Context(), agent.Subject)
	if err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent not found", nil)
		return
	}

	if agentRecord.AppliedConfig == nil || agentRecord.AppliedConfig.GCPIdentity == nil ||
		agentRecord.AppliedConfig.GCPIdentity.MetadataMode != store.GCPMetadataModeAssign {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "no GCP identity assigned", nil)
		return
	}

	gcpID := agentRecord.AppliedConfig.GCPIdentity

	// Verify the agent's JWT has the correct scope
	requiredScope := GCPTokenScopeForSA(gcpID.ServiceAccountID)
	if !agent.HasScope(requiredScope) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "missing required GCP token scope", nil)
		return
	}

	// Parse requested scopes (or default)
	var req gcpTokenRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"https://www.googleapis.com/auth/cloud-platform"}
	}

	if s.gcpTokenGenerator == nil {
		writeError(w, http.StatusServiceUnavailable, "gcp_not_configured",
			"GCP token generation is not configured on this Hub", nil)
		return
	}

	token, err := s.gcpTokenGenerator.GenerateAccessToken(r.Context(), gcpID.ServiceAccountEmail, scopes)
	if err != nil {
		if s.gcpTokenMetrics != nil {
			s.gcpTokenMetrics.RecordAccessTokenRequest(false, time.Since(start))
		}
		LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventAccessToken,
			agent.Subject, agentRecord.ProjectID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, false, err.Error())
		writeError(w, http.StatusBadGateway, "gcp_token_failed",
			"token generation failed: "+err.Error(), nil)
		return
	}

	if s.gcpTokenMetrics != nil {
		s.gcpTokenMetrics.RecordAccessTokenRequest(true, time.Since(start))
	}
	LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventAccessToken,
		agent.Subject, agentRecord.ProjectID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, true, "")
	writeJSON(w, http.StatusOK, token)
}

// handleAgentGCPIdentityToken handles POST /api/v1/agent/gcp-identity-token.
// Called by the metadata sidecar to obtain a GCP OIDC identity token.
func (s *Server) handleAgentGCPIdentityToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	start := time.Now()

	agent := GetAgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent authentication required", nil)
		return
	}

	// Rate limit check
	if s.gcpTokenRateLimiter != nil && !s.gcpTokenRateLimiter.Allow(agent.Subject) {
		if s.gcpTokenMetrics != nil {
			s.gcpTokenMetrics.RecordRateLimitRejection()
		}
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "rate limit exceeded for GCP token requests", nil)
		return
	}

	agentRecord, err := s.store.GetAgent(r.Context(), agent.Subject)
	if err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent not found", nil)
		return
	}

	if agentRecord.AppliedConfig == nil || agentRecord.AppliedConfig.GCPIdentity == nil ||
		agentRecord.AppliedConfig.GCPIdentity.MetadataMode != store.GCPMetadataModeAssign {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "no GCP identity assigned", nil)
		return
	}

	gcpID := agentRecord.AppliedConfig.GCPIdentity
	requiredScope := GCPTokenScopeForSA(gcpID.ServiceAccountID)
	if !agent.HasScope(requiredScope) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "missing required GCP token scope", nil)
		return
	}

	var req gcpIdentityTokenRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body: "+err.Error(), nil)
		return
	}
	if req.Audience == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "audience is required", nil)
		return
	}

	if s.gcpTokenGenerator == nil {
		writeError(w, http.StatusServiceUnavailable, "gcp_not_configured",
			"GCP token generation is not configured on this Hub", nil)
		return
	}

	token, err := s.gcpTokenGenerator.GenerateIDToken(r.Context(), gcpID.ServiceAccountEmail, req.Audience)
	if err != nil {
		if s.gcpTokenMetrics != nil {
			s.gcpTokenMetrics.RecordIDTokenRequest(false, time.Since(start))
		}
		LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventIdentityToken,
			agent.Subject, agentRecord.ProjectID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, false, err.Error())
		writeError(w, http.StatusBadGateway, "gcp_token_failed",
			"identity token generation failed: "+err.Error(), nil)
		return
	}

	if s.gcpTokenMetrics != nil {
		s.gcpTokenMetrics.RecordIDTokenRequest(true, time.Since(start))
	}
	LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventIdentityToken,
		agent.Subject, agentRecord.ProjectID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, true, "")
	writeJSON(w, http.StatusOK, token)
}

type gcpTokenRequest struct {
	Scopes []string `json:"scopes,omitempty"`
}

type gcpIdentityTokenRequest struct {
	Audience string `json:"audience"`
}
