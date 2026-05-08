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

// handleGroveGCPServiceAccounts handles /api/v1/groves/{groveId}/gcp-service-accounts
func (s *Server) handleGroveGCPServiceAccounts(w http.ResponseWriter, r *http.Request, groveID string) {
	switch r.Method {
	case http.MethodGet:
		s.listGCPServiceAccounts(w, r, groveID)
	case http.MethodPost:
		s.createGCPServiceAccount(w, r, groveID)
	default:
		MethodNotAllowed(w)
	}
}

// handleGroveGCPServiceAccountByID handles /api/v1/groves/{groveId}/gcp-service-accounts/{id}[/action]
func (s *Server) handleGroveGCPServiceAccountByID(w http.ResponseWriter, r *http.Request, groveID, saPath string) {
	// Handle collection-level actions first
	if saPath == "mint" && r.Method == http.MethodPost {
		s.mintGCPServiceAccount(w, r, groveID)
		return
	}

	parts := strings.SplitN(saPath, "/", 2)
	saID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if action == "verify" && r.Method == http.MethodPost {
		s.verifyGCPServiceAccount(w, r, groveID, saID)
		return
	}

	if action != "" {
		NotFound(w, "GCP Service Account action")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getGCPServiceAccount(w, r, groveID, saID)
	case http.MethodDelete:
		s.deleteGCPServiceAccount(w, r, groveID, saID)
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

type createGCPServiceAccountResponse struct {
	store.GCPServiceAccount
	VerificationFailed  bool                       `json:"verificationFailed,omitempty"`
	VerificationDetails *verificationFailedDetails `json:"verificationDetails,omitempty"`
}

func (s *Server) createGCPServiceAccount(w http.ResponseWriter, r *http.Request, groveID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	var req createGCPServiceAccountRequest
	if err := readJSON(r, &req); err != nil {
		slog.Debug("GCP SA create: failed to parse request body",
			"grove_id", groveID,
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
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"could not infer projectId from email; please provide it explicitly", nil)
		return
	}

	// Verify grove exists
	grove, err := s.store.GetGrove(r.Context(), groveID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorization: grove owners and admins can manage GCP service accounts
	decision := s.authzService.CheckAccess(r.Context(), user, Resource{
		Type:    "grove",
		ID:      grove.ID,
		OwnerID: grove.OwnerID,
	}, ActionManage)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to manage GCP service accounts in this grove", nil)
		return
	}

	sa := &store.GCPServiceAccount{
		ID:            uuid.New().String(),
		Scope:         store.ScopeGrove,
		ScopeID:       groveID,
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
				"a service account with this email already exists for this grove", nil)
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
			sa.VerificationStatus = "failed"
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
			sa.VerificationStatus = "verified"
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
	GroveMinted  int `json:"grove_minted"`
	GroveCap     int `json:"grove_cap"` // 0 = unlimited
	GlobalMinted int `json:"global_minted"`
	GlobalCap    int `json:"global_cap"` // 0 = unlimited
}

// ListGCPServiceAccountsResponse is the response for listing GCP service accounts.
type ListGCPServiceAccountsResponse struct {
	Items        []GCPServiceAccountWithCapabilities `json:"items"`
	Capabilities *Capabilities                       `json:"_capabilities,omitempty"`
	MintQuota    *GCPMintQuotaInfo                   `json:"mint_quota,omitempty"`
}

func (s *Server) listGCPServiceAccounts(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()
	sas, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeGrove,
		ScopeID: groveID,
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
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "grove", groveID, "gcp_service_account")
	}

	// Include mint quota info when minting is configured
	var mintQuota *GCPMintQuotaInfo
	if s.gcpIAMAdmin != nil && s.config.GCPProjectID != "" {
		managed := true
		groveCount, _ := s.store.CountGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
			Scope:   store.ScopeGrove,
			ScopeID: groveID,
			Managed: &managed,
		})
		globalCount, _ := s.store.CountGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
			Managed: &managed,
		})
		mintQuota = &GCPMintQuotaInfo{
			GroveMinted:  groveCount,
			GroveCap:     s.config.GCPMintCapPerGrove,
			GlobalMinted: globalCount,
			GlobalCap:    s.config.GCPMintCapGlobal,
		}
	}

	writeJSON(w, http.StatusOK, ListGCPServiceAccountsResponse{
		Items:        items,
		Capabilities: scopeCap,
		MintQuota:    mintQuota,
	})
}

func (s *Server) getGCPServiceAccount(w http.ResponseWriter, r *http.Request, groveID, saID string) {
	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if sa.ScopeID != groveID {
		NotFound(w, "GCP Service Account")
		return
	}

	writeJSON(w, http.StatusOK, sa)
}

func (s *Server) deleteGCPServiceAccount(w http.ResponseWriter, r *http.Request, groveID, saID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if sa.ScopeID != groveID {
		NotFound(w, "GCP Service Account")
		return
	}

	// Authorization: grove owners and admins can manage GCP service accounts
	grove, err := s.store.GetGrove(r.Context(), groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	decision := s.authzService.CheckAccess(r.Context(), user, Resource{
		Type:    "grove",
		ID:      grove.ID,
		OwnerID: grove.OwnerID,
	}, ActionManage)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to manage GCP service accounts in this grove", nil)
		return
	}

	if err := s.store.DeleteGCPServiceAccount(r.Context(), saID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyGCPServiceAccount(w http.ResponseWriter, r *http.Request, groveID, saID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	sa, err := s.store.GetGCPServiceAccount(r.Context(), saID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if sa.ScopeID != groveID {
		NotFound(w, "GCP Service Account")
		return
	}

	// Authorization: grove owners and admins can manage GCP service accounts
	grove, err := s.store.GetGrove(r.Context(), groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	decision := s.authzService.CheckAccess(r.Context(), user, Resource{
		Type:    "grove",
		ID:      grove.ID,
		OwnerID: grove.OwnerID,
	}, ActionManage)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to manage GCP service accounts in this grove", nil)
		return
	}

	// Attempt to verify impersonation via the GCP token generator
	if s.gcpTokenGenerator != nil {
		if err := s.gcpTokenGenerator.VerifyImpersonation(r.Context(), sa.Email); err != nil {
			// Persist the failure status
			sa.Verified = false
			sa.VerificationStatus = "failed"
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
	}

	sa.Verified = true
	sa.VerifiedAt = time.Now()
	sa.VerificationStatus = "verified"
	sa.VerificationError = ""

	if err := s.store.UpdateGCPServiceAccount(r.Context(), sa); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, sa)
}

// mintGCPServiceAccountRequest is the request body for POST .../gcp-service-accounts/mint.
type mintGCPServiceAccountRequest struct {
	AccountID   string `json:"account_id"`   // Optional custom SA account ID (will be prefixed with scion-)
	DisplayName string `json:"display_name"` // Optional display name
	Description string `json:"description"`  // Optional description
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

func (s *Server) mintGCPServiceAccount(w http.ResponseWriter, r *http.Request, groveID string) {
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

	projectID := s.config.GCPProjectID
	if projectID == "" {
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

	// Verify grove exists
	grove, err := s.store.GetGrove(r.Context(), groveID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorization: grove owners and admins can mint GCP service accounts
	decision := s.authzService.CheckAccess(r.Context(), user, Resource{
		Type:    "grove",
		ID:      grove.ID,
		OwnerID: grove.OwnerID,
	}, ActionManage)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to manage GCP service accounts in this grove", nil)
		return
	}

	// Enforce per-grove mint cap
	managed := true
	groveCount, err := s.store.CountGCPServiceAccounts(r.Context(), store.GCPServiceAccountFilter{
		Scope:   store.ScopeGrove,
		ScopeID: groveID,
		Managed: &managed,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if s.config.GCPMintCapPerGrove > 0 && groveCount >= s.config.GCPMintCapPerGrove {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			fmt.Sprintf("per-grove mint limit reached (%d/%d)", groveCount, s.config.GCPMintCapPerGrove), nil)
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
		displayName = fmt.Sprintf("Scion agent (%s)", grove.Slug)
	}
	description := req.Description
	if description == "" {
		description = fmt.Sprintf("Minted by Scion Hub for grove %s (ID: %s) by user %s", grove.Slug, groveID, user.ID())
	}

	// Create the SA in GCP
	saEmail, _, err := s.gcpIAMAdmin.CreateServiceAccount(r.Context(), projectID, accountID, displayName, description)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "409") || strings.Contains(errStr, "alreadyExists") {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				fmt.Sprintf("service account %s already exists in project %s", accountID, projectID), nil)
			return
		}
		slog.Error("GCP SA mint: failed to create service account",
			"grove_id", groveID, "account_id", accountID, "error", err)
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
			"failed to create GCP service account: "+err.Error(), nil)
		return
	}

	// Grant token creator role to Hub SA on the new SA
	if s.gcpTokenGenerator != nil {
		hubEmail := s.gcpTokenGenerator.ServiceAccountEmail()
		if hubEmail != "" {
			member := "serviceAccount:" + hubEmail
			if err := s.gcpIAMAdmin.SetIAMPolicy(r.Context(), saEmail, member, "roles/iam.serviceAccountTokenCreator"); err != nil {
				slog.Error("GCP SA mint: failed to set IAM policy",
					"grove_id", groveID, "sa_email", saEmail, "hub_email", hubEmail, "error", err)
				// SA was created but policy failed — still store it but log the issue
				// The user can verify later
			}
		}
	}

	// Store the SA record
	sa := &store.GCPServiceAccount{
		ID:                 uuid.New().String(),
		Scope:              store.ScopeGrove,
		ScopeID:            groveID,
		Email:              saEmail,
		ProjectID:          projectID,
		DisplayName:        displayName,
		DefaultScopes:      []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: "verified",
		CreatedBy:          user.ID(),
		CreatedAt:          time.Now(),
		Managed:            true,
		ManagedBy:          s.config.HubID,
	}

	if err := s.store.CreateGCPServiceAccount(r.Context(), sa); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a service account with this email already exists for this grove", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Audit log the mint
	LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventMintSA,
		"", groveID, saEmail, sa.ID, true, "")

	slog.Info("GCP SA minted",
		"grove_id", groveID, "sa_id", sa.ID, "email", saEmail,
		"account_id", accountID, "project", projectID, "user", user.ID())

	writeJSON(w, http.StatusCreated, sa)
}

// GCPQuotaGroveInfo holds per-grove mint quota info for the admin endpoint.
type GCPQuotaGroveInfo struct {
	GroveID   string `json:"grove_id"`
	GroveName string `json:"grove_name"`
	Minted    int    `json:"minted"`
}

// GCPQuotaResponse is the response for GET /api/v1/admin/gcp-quota.
type GCPQuotaResponse struct {
	MintingConfigured bool                `json:"minting_configured"`
	GCPProjectID      string              `json:"gcp_project_id,omitempty"`
	GlobalMinted      int                 `json:"global_minted"`
	GlobalCap         int                 `json:"global_cap"`
	PerGroveCap       int                 `json:"per_grove_cap"`
	Groves            []GCPQuotaGroveInfo `json:"groves,omitempty"`
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
		PerGroveCap:       s.config.GCPMintCapPerGrove,
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

		// Get per-grove breakdown
		allMinted, err := s.store.ListGCPServiceAccounts(r.Context(), store.GCPServiceAccountFilter{
			Managed: &managed,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		groveCounts := map[string]int{}
		for _, sa := range allMinted {
			groveCounts[sa.ScopeID]++
		}

		for groveID, count := range groveCounts {
			name := groveID
			if g, err := s.store.GetGrove(r.Context(), groveID); err == nil {
				name = g.Name
			}
			resp.Groves = append(resp.Groves, GCPQuotaGroveInfo{
				GroveID:   groveID,
				GroveName: name,
				Minted:    count,
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
		json.NewDecoder(r.Body).Decode(&req)
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
			agent.Subject, agentRecord.GroveID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, false, err.Error())
		writeError(w, http.StatusBadGateway, "gcp_token_failed",
			"token generation failed: "+err.Error(), nil)
		return
	}

	if s.gcpTokenMetrics != nil {
		s.gcpTokenMetrics.RecordAccessTokenRequest(true, time.Since(start))
	}
	LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventAccessToken,
		agent.Subject, agentRecord.GroveID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, true, "")
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
			agent.Subject, agentRecord.GroveID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, false, err.Error())
		writeError(w, http.StatusBadGateway, "gcp_token_failed",
			"identity token generation failed: "+err.Error(), nil)
		return
	}

	if s.gcpTokenMetrics != nil {
		s.gcpTokenMetrics.RecordIDTokenRequest(true, time.Since(start))
	}
	LogGCPTokenGeneration(r.Context(), s.auditLogger, GCPTokenEventIdentityToken,
		agent.Subject, agentRecord.GroveID, gcpID.ServiceAccountEmail, gcpID.ServiceAccountID, true, "")
	writeJSON(w, http.StatusOK, token)
}

type gcpTokenRequest struct {
	Scopes []string `json:"scopes,omitempty"`
}

type gcpIdentityTokenRequest struct {
	Audience string `json:"audience"`
}
