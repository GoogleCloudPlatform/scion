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
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type ListEnvVarsResponse struct {
	EnvVars []store.EnvVar `json:"envVars"`
	Scope   string         `json:"scope"`
	ScopeID string         `json:"scopeId"`
}

type SetEnvVarRequest struct {
	Value         string `json:"value"`
	Scope         string `json:"scope,omitempty"`
	ScopeID       string `json:"scopeId,omitempty"`
	Description   string `json:"description,omitempty"`
	Sensitive     bool   `json:"sensitive,omitempty"`
	InjectionMode string `json:"injectionMode,omitempty"`
	Secret        bool   `json:"secret,omitempty"`
	AllowProgeny  bool   `json:"allowProgeny,omitempty"` // Allow creator's progeny agents to access (user scope, always mode only)
}

type SetEnvVarResponse struct {
	EnvVar  *store.EnvVar `json:"envVar"`
	Created bool          `json:"created"`
}

// resolveEnvSecretAccess resolves the scopeID and enforces authorization for
// env var and secret endpoints. It returns the resolved scopeID and true on
// success, or writes an HTTP error and returns false on failure.
//
// For user scope: extracts the authenticated user's ID as scopeID (ignoring
// any client-supplied value). The caller must use an interactive session or
// dev credential; a user access token is refused (session-only,
// CREDENTIAL_MANAGEMENT). A federated caller may read (never write) its own
// user-level material, as before (federatedOwnUserRead).
//
// For hub scope: agents may read. A user must be an unscoped local platform
// admin on an interactive session or dev credential; a user access token is
// refused whatever its boundary or scopes (session-only,
// CREDENTIAL_MANAGEMENT).
//
// For project scope: verifies the project exists, then checks authorization. Users
// must pass CheckAccess (with owner bypass). Agents get read-only access to
// their own project only; project-scoped secret metadata endpoints additionally
// require ScopeProjectSecretRead (checkAgentProjectSecretReadScope), and
// project-scoped env read endpoints omit environment-secret metadata unless the
// agent holds ScopeProjectSecretRead (agentLacksProjectSecretReadScope).
//
// For broker scope: verifies the broker exists. Brokers get self-access via
// BrokerIdentity. Users must pass CheckAccess.
func (s *Server) resolveEnvSecretAccess(w http.ResponseWriter, r *http.Request, scope, clientScopeID string, isWrite bool) (string, bool) {
	ctx := r.Context()

	if scope == "project" {
		scope = store.ScopeProject
	}

	switch scope {
	case store.ScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			Unauthorized(w)
			return "", false
		}
		// A federated caller is never a profile writer: its writes are
		// refused here, before the credential check, with the profile
		// write refusal.
		if isWrite && !requireProfileWriter(w, r) {
			return "", false
		}
		// User-level variables and secrets need an interactive session or
		// dev credential; every user access token is refused. The one
		// exception is a federated caller's READ of its own user-level
		// material, kept deliberately as it was before session-only: it is
		// read-only (writes were refused above), limited to the caller's own
		// ID (returned below, never a client-supplied one), and gives no hub
		// scope (the hub branch requires an unscoped local admin).
		if !sessionCredentialAllowed(ctx) && !federatedOwnUserRead(ctx, isWrite) {
			writeEnvSecretSessionRequired(w)
			return "", false
		}
		return userIdent.ID(), true

	case store.ScopeProject:
		if clientScopeID == "" {
			BadRequest(w, "scopeId is required for project scope")
			return "", false
		}
		project, err := s.store.GetProject(ctx, clientScopeID)
		if err != nil {
			if err == store.ErrNotFound {
				NotFound(w, "Project")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return "", false
		}
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if agentIdent, ok := identity.(AgentIdentity); ok {
			if isWrite {
				Forbidden(w)
				return "", false
			}
			if agentIdent.ProjectID() != clientScopeID {
				Forbidden(w)
				return "", false
			}
			if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
				return "", false
			}
			return clientScopeID, true
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "project",
				ID:      project.ID,
				OwnerID: project.OwnerID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return "", false
			}
			return clientScopeID, true
		}
		Forbidden(w)
		return "", false

	case store.ScopeRuntimeBroker:
		if clientScopeID == "" {
			BadRequest(w, "scopeId is required for runtime_broker scope")
			return "", false
		}
		broker, err := s.store.GetRuntimeBroker(ctx, clientScopeID)
		if err != nil {
			if err == store.ErrNotFound {
				NotFound(w, "RuntimeBroker")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return "", false
		}
		if !s.authorizeBrokerMaterial(w, r, broker, isWrite) {
			return "", false
		}
		return clientScopeID, true

	case store.ScopeHub:
		// Hub scope: admin users can read and write; agents can read only.
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if agentIdent, ok := identity.(AgentIdentity); ok {
			if isWrite {
				Forbidden(w)
				return "", false
			}
			if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
				return "", false
			}
			return s.hubID, true
		}
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			// Non-user, non-agent identities (brokers) cannot access hub-scoped
			// secrets directly.
			Forbidden(w)
			return "", false
		}
		// Hub-level variables and secrets need an unscoped admin on an
		// interactive session or dev credential; every user access token is
		// refused, whatever its boundary or scopes.
		if !sessionCredentialAllowed(ctx) {
			writeEnvSecretSessionRequired(w)
			return "", false
		}
		if !IsUnscopedLocalPlatformAdmin(userIdent) {
			Forbidden(w)
			return "", false
		}
		// Hub-scope variables and secrets are not available to test
		// identities with admin roles (test-admin policy).
		if s.testAdminPolicyRefuses(ctx, "handlers_env_secrets.go:resolveEnvSecretAccess") {
			Forbidden(w)
			return "", false
		}
		return s.hubID, true

	default:
		BadRequest(w, "invalid scope: "+scope)
		return "", false
	}
}

// resolveEnvSecretAccessWithAgent resolves scope access for secret endpoints
// (which support store.ScopeAgent when hub.agent_secrets is enabled) and
// returns the loaded *store.Agent when scope == store.ScopeAgent so setSecret
// can attach ProjectID without re-fetching. Plain env-var endpoints call
// resolveEnvSecretAccess directly and never admit ScopeAgent.
func (s *Server) resolveEnvSecretAccessWithAgent(w http.ResponseWriter, r *http.Request, scope, clientScopeID string, isWrite bool) (string, *store.Agent, bool) {
	if scope == store.ScopeAgent {
		return s.resolveAgentSecretAccess(w, r, scope, clientScopeID, isWrite)
	}
	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, clientScopeID, isWrite)
	return scopeID, nil, ok
}

func (s *Server) resolveAgentSecretAccess(w http.ResponseWriter, r *http.Request, scope, clientScopeID string, isWrite bool) (string, *store.Agent, bool) {
	ctx := r.Context()
	// Agent scope is secrets-only (callers are secret handlers via
	// resolveEnvSecretAccessWithAgent) and gated on hub.agent_secrets.
	if !s.experimentEnabled(experiments.AgentSecrets) {
		BadRequest(w, "invalid scope: "+scope)
		return "", nil, false
	}
	if clientScopeID == "" {
		BadRequest(w, "scopeId is required for agent scope")
		return "", nil, false
	}
	agent, err := s.store.GetAgent(ctx, clientScopeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Agent")
		} else {
			writeErrorFromErr(w, err, "")
		}
		return "", nil, false
	}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", nil, false
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		// Agents access their own secrets via /api/v1/agents/{id}/secrets.
		Forbidden(w)
		return "", nil, false
	}
	action := ActionRead
	if isWrite {
		action = ActionUpdate
	}
	decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
		Type:       "agent",
		ID:         agent.ID,
		OwnerID:    agent.OwnerID,
		ParentType: "project",
		ParentID:   agent.ProjectID,
	}, action)
	if !decision.Allowed {
		Forbidden(w)
		return "", nil, false
	}
	if isWrite && !agent.DeletedAt.IsZero() {
		NotFound(w, "Agent")
		return "", nil, false
	}
	return agent.ID, agent, true
}

// envSecretSessionRequiredMessage is the refusal message for a user access
// token on user- or hub-level env and secret operations.
const envSecretSessionRequiredMessage = "user- and hub-level environment variables and secrets require an interactive login (run 'scion hub auth login'); access tokens are not accepted"

// federatedOwnUserRead reports whether the request is a federated caller's
// read of its own user-level env or secret material: a read (not a write),
// on a federation credential, by a federated identity. It admits nothing
// else; in particular a user access token never matches.
func federatedOwnUserRead(ctx context.Context, isWrite bool) bool {
	return !isWrite &&
		GetCredentialContextFromContext(ctx).Kind == CredentialKindFederation &&
		isFederatedCaller(ctx)
}

// writeEnvSecretSessionRequired writes the session-only refusal for user-
// and hub-level env and secret operations.
func writeEnvSecretSessionRequired(w http.ResponseWriter) {
	writeSessionOnlyDenial(w, ErrCodeForbidden, envSecretSessionRequiredMessage, authzop.ReasonCredentialManagement)
}

// authorizeHubEnvVarList decides read-only access to the hub-level
// environment variable list for a user who does not pass the legacy admin
// check in resolveEnvSecretAccess. Such a user is admitted when the standard
// permission decision grants hub.env_vars.read on the hub.
//
// hubScope reports whether the request lists hub-level variables. readOnly is
// true only when the caller was admitted here; the caller then lists plain
// variables without secret metadata. When readOnly is false the caller must
// fall through to resolveEnvSecretAccess, which keeps its existing checks for
// legacy admins, agents, other scopes and refusals. ok is false only after an
// error response has been written.
//
// This covers the list endpoint only. Single-variable reads, writes and every
// secret endpoint keep using resolveEnvSecretAccess unchanged.
func (s *Server) authorizeHubEnvVarList(w http.ResponseWriter, r *http.Request, scope string) (hubScope, readOnly, ok bool) {
	if scope != store.ScopeHub {
		return false, false, true
	}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	userIdent, isUser := identity.(UserIdentity)
	// The hub-level list is session-only like the other hub-level env and
	// secret operations: a user access token is refused here, before any
	// role or permission check.
	if isUser && !isNilIdentity(userIdent) && !sessionCredentialAllowed(ctx) {
		writeEnvSecretSessionRequired(w)
		return true, false, false
	}
	if !isUser || isNilIdentity(userIdent) || userIdent.Role() == store.UserRoleAdmin {
		// Test identities with admin roles do not read hub environment
		// variables (test-admin policy).
		if isUser && s.testAdminPolicyRefuses(ctx, "handlers_env_secrets.go:authorizeHubEnvVarList") {
			Forbidden(w)
			return true, false, false
		}
		// Agents, brokers, anonymous callers and legacy admins keep the
		// existing hub-scope handling in resolveEnvSecretAccess.
		return true, false, true
	}
	if s.authzService == nil {
		return true, false, true
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(userIdent),
		Credential: credentialContextForIdentity(userIdent),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     ActionRead,
		Permission: "hub.env_vars.read",
	})
	if !decision.Allowed {
		Forbidden(w)
		return true, false, false
	}
	return true, true, true
}

// withoutSecretEnvVars drops entries flagged as secrets, so a read-only
// hub-level list never carries secret metadata.
func withoutSecretEnvVars(envVars []store.EnvVar) []store.EnvVar {
	out := make([]store.EnvVar, 0, len(envVars))
	for _, ev := range envVars {
		if ev.Secret {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func (s *Server) handleEnvVars(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEnvVars(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet)
	}
}

func (s *Server) listEnvVars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	// includeSecretMeta stays true for every caller that passes the shared
	// env/secret access check. A caller admitted only by hub.env_vars.read
	// gets plain variables: environment-type secrets are secret metadata
	// and stay behind the secret access check.
	includeSecretMeta := true
	var scopeID string
	if hubScope, readOnly, ok := s.authorizeHubEnvVarList(w, r, scope); !ok {
		return
	} else if hubScope && readOnly {
		scopeID = s.hubID
		includeSecretMeta = false
	} else {
		resolved, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
		if !ok {
			return
		}
		scopeID = resolved
	}

	installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
	if !ok {
		return
	}

	filter := store.EnvVarFilter{
		Scope:   scope,
		ScopeID: scopeID,
		Key:     query.Get("key"),
	}

	envVars, err := s.store.ListEnvVars(ctx, filter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if includeSecretMeta {
		var ok bool
		envVars, ok = s.applyEnvironmentSecretsToEnvVarList(w, ctx, envVars, filter,
			"failed to list environment secrets for env var merge", installationOnly)
		if !ok {
			return
		}
	} else {
		envVars = withoutSecretEnvVars(envVars)
	}
	if installationOnly {
		envVars = withoutInstallationBlockedEnvVars(envVars)
	}

	// Mask sensitive values
	for i := range envVars {
		if envVars[i].Sensitive {
			envVars[i].Value = "********"
		}
	}

	writeJSON(w, http.StatusOK, ListEnvVarsResponse{
		EnvVars: envVars,
		Scope:   scope,
		ScopeID: scopeID,
	})
}

func (s *Server) handleEnvVarByKey(w http.ResponseWriter, r *http.Request) {
	key := extractID(r, "/api/v1/env")

	if key == "" {
		NotFound(w, "EnvVar")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getEnvVar(w, r, key)
	case http.MethodPut:
		s.setEnvVar(w, r, key)
	case http.MethodDelete:
		s.deleteEnvVar(w, r, key)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) getEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	if query.Has("includePending") {
		BadRequest(w, "includePending is only valid on the secret list (GET /api/v1/secrets)")
		return
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}

	s.serveEnvVarByKey(w, ctx, key, scope, scopeID)
}

func (s *Server) serveEnvVarByKey(w http.ResponseWriter, ctx context.Context, key, scope, scopeID string) {
	installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
	if !ok {
		return
	}
	if installationOnly && isBlockedInstallationGitHubEnvKey(key) {
		writeErrorFromErr(w, store.ErrNotFound, "")
		return
	}

	hideProjectSecretMeta := agentLacksProjectSecretReadScope(ctx, scope)

	envVar, err := s.store.GetEnvVar(ctx, key, scope, scopeID)
	if err != nil {
		if !hideProjectSecretMeta && errors.Is(err, store.ErrNotFound) && s.secretBackend != nil && !isSystemOneReservedSecretKey(key) {
			// Fallback: check if this key exists as an environment secret.
			// Defence in depth: agentHiddenHubSecret and installation-only
			// blocked targets are checked regardless of which route reached
			// this helper.
			meta, metaErr := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
			if metaErr == nil && meta.SecretType == store.SecretTypeEnvironment &&
				!agentHiddenHubSecret(ctx, scope, *meta) &&
				(!installationOnly || !isBlockedInstallationGitHubEnvKey(meta.Target)) {
				ev := secretMetaToEnvVar(*meta)
				writeJSON(w, http.StatusOK, &ev)
				return
			}
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if hideProjectSecretMeta {
		if envVar.Secret {
			writeErrorFromErr(w, store.ErrNotFound, "")
			return
		}
		if s.secretBackend != nil && !isSystemOneReservedSecretKey(key) {
			meta, metaErr := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
			if metaErr != nil && !errors.Is(metaErr, store.ErrNotFound) {
				s.envSecretLog.Warn("failed to check environment secret shadow for env var read", "key", key, "error", metaErr)
				writeErrorFromErr(w, metaErr, "")
				return
			}
			if metaErr == nil && meta.SecretType == store.SecretTypeEnvironment {
				writeErrorFromErr(w, store.ErrNotFound, "")
				return
			}
		}
	}

	// Mask sensitive values
	if envVar.Sensitive {
		envVar.Value = "********"
	}

	writeJSON(w, http.StatusOK, envVar)
}

func (s *Server) setEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()

	var req SetEnvVarRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Value == "" {
		ValidationError(w, "value is required", nil)
		return
	}

	// A plain env var's key is itself the container-env name it is projected
	// under (same as an environment-type secret's target), whether or not
	// req.Secret promotes it to the secret backend below.
	if !validateEnvSecretTarget(w, store.SecretTypeEnvironment, key) {
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, req.ScopeID, true)
	if !ok {
		return
	}
	if rejectSystemOneReservedSecretWrite(w, key) {
		return
	}

	// allowProgeny is only valid on user-scoped env vars/secrets.
	// Plain env vars additionally require injection_mode=always;
	// secret promotions (req.Secret=true) do NOT — the secret dispatch
	// flow supports as_needed progeny secrets (design doc §5.7).
	if req.AllowProgeny {
		if scope != store.ScopeUser {
			ValidationError(w, "allowProgeny is only supported on user-scoped env vars", map[string]interface{}{
				"field": "allowProgeny",
				"scope": scope,
			})
			return
		}
		if !req.Secret {
			im := req.InjectionMode
			if im == "" {
				im = store.InjectionModeAsNeeded
			}
			if im != store.InjectionModeAlways {
				ValidationError(w, "allowProgeny requires injectionMode to be 'always'", map[string]interface{}{
					"field":         "allowProgeny",
					"injectionMode": im,
				})
				return
			}
		}
	}

	var createdBy string
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
	}

	// Secret promotion: route secret-flagged writes to the secret backend
	if req.Secret {
		if s.secretBackend == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"error": "secret storage requires a configured secrets backend",
			})
			return
		}
		if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
			return
		}
		if rejectWebhookEndpointReservedSecretWrite(w, key) {
			return
		}

		input := &secret.SetSecretInput{
			Name:          key,
			Value:         req.Value,
			SecretType:    "environment",
			Target:        key,
			Scope:         scope,
			ScopeID:       scopeID,
			Description:   req.Description,
			InjectionMode: req.InjectionMode,
			AllowProgeny:  req.AllowProgeny,
			CreatedBy:     createdBy,
			UpdatedBy:     createdBy,
		}
		created, meta, err := s.secretBackend.Set(ctx, input)
		if err != nil {
			if errors.Is(err, secret.ErrNoSecretBackend) {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		// Clean up any stale plain env var record for the same key/scope
		_ = s.store.DeleteEnvVar(ctx, key, scope, scopeID)

		syntheticEnvVar := secretMetaToEnvVar(*meta)
		writeJSON(w, http.StatusOK, SetEnvVarResponse{
			EnvVar:  &syntheticEnvVar,
			Created: created,
		})
		return
	}

	// Plain env var write
	if rejectWebhookEndpointReservedSecretWrite(w, key) {
		return
	}
	injectionMode := req.InjectionMode
	if injectionMode == "" {
		injectionMode = store.InjectionModeAsNeeded
	}

	envVar := &store.EnvVar{
		ID:            api.NewUUID(),
		Key:           key,
		Value:         req.Value,
		Scope:         scope,
		ScopeID:       scopeID,
		Description:   req.Description,
		Sensitive:     req.Sensitive,
		InjectionMode: injectionMode,
		Secret:        false,
		AllowProgeny:  req.AllowProgeny,
	}
	envVar.CreatedBy = createdBy

	created, err := s.store.UpsertEnvVar(ctx, envVar)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Clean up any existing secret with same key (demotion from secret to plain),
	// except internal secrets which are never managed through env/secrets APIs.
	if s.secretBackend != nil {
		if isInternal, err := s.isStoredSecretInternal(ctx, key, scope, scopeID); err == nil && !isInternal {
			_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
		}
	}

	// Mask sensitive values in response
	if envVar.Sensitive {
		envVar.Value = "********"
	}

	writeJSON(w, http.StatusOK, SetEnvVarResponse{
		EnvVar:  envVar,
		Created: created,
	})
}

func (s *Server) deleteEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), true)
	if !ok {
		return
	}
	if isSystemOneReservedSecretKey(key) {
		writeErrorFromErr(w, store.ErrNotFound, "")
		return
	}

	if err := s.store.DeleteEnvVar(ctx, key, scope, scopeID); err != nil {
		if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
			if isInternal, intErr := s.isStoredSecretInternal(ctx, key, scope, scopeID); intErr == nil && !isInternal {
				// Fallback: try deleting from the secret backend
				if secErr := s.secretBackend.Delete(ctx, key, scope, scopeID); secErr == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Also clean up any non-internal secret with the same key
	if s.secretBackend != nil {
		if isInternal, err := s.isStoredSecretInternal(ctx, key, scope, scopeID); err == nil && !isInternal {
			_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

type ListSecretsResponse struct {
	Secrets []store.Secret `json:"secrets"`
	Scope   string         `json:"scope"`
	ScopeID string         `json:"scopeId"`
	// Items is the merged view of approved and pending user secrets,
	// present only for scope=user&includePending=true
	// (listSecretsWithPending).
	Items []SecretListItem `json:"items,omitempty"`
}

type SetSecretRequest struct {
	Value         string `json:"value"`
	Encoding      string `json:"encoding,omitempty"` // "base64" (default, value is base64-encoded) or "raw" (value is literal text)
	Scope         string `json:"scope,omitempty"`
	ScopeID       string `json:"scopeId,omitempty"`
	Description   string `json:"description,omitempty"`
	InjectionMode string `json:"injectionMode,omitempty"` // "always" or "as_needed" (default: as_needed)
	Type          string `json:"type,omitempty"`          // environment (default), variable, file
	Target        string `json:"target,omitempty"`        // Projection target (defaults to key)
	AllowProgeny  bool   `json:"allowProgeny,omitempty"`  // Allow creator's progeny agents to access (user scope only)
}

type SetSecretResponse struct {
	Secret  *store.Secret `json:"secret"`
	Created bool          `json:"created"`
}

// PatchSecretRequest is the request body for metadata-only secret updates (PATCH).
// Only non-null/non-empty fields are applied. The secret value is never modified.
type PatchSecretRequest struct {
	Description   *string `json:"description"`             // null = no change, "" = clear
	InjectionMode string  `json:"injectionMode,omitempty"` // "" = no change
	Type          string  `json:"type,omitempty"`          // "" = no change
	Target        string  `json:"target,omitempty"`        // "" = no change
	AllowProgeny  *bool   `json:"allowProgeny,omitempty"`  // null = no change
}

// metaToStoreSecret converts a secret.SecretMeta to a store.Secret for API response compatibility.
func metaToStoreSecret(m secret.SecretMeta) store.Secret {
	return store.Secret{
		ID:            m.ID,
		Key:           m.Name,
		SecretRef:     m.SecretRef,
		SecretType:    m.SecretType,
		Target:        m.Target,
		Scope:         m.Scope,
		ScopeID:       m.ScopeID,
		Description:   m.Description,
		InjectionMode: m.InjectionMode,
		AllowProgeny:  m.AllowProgeny,
		Version:       m.Version,
		Created:       m.Created,
		Updated:       m.Updated,
		CreatedBy:     m.CreatedBy,
		UpdatedBy:     m.UpdatedBy,
	}
}

// secretMetaToEnvVar converts a secret.SecretMeta (with type "environment") to a store.EnvVar
// for inclusion in unified env var list responses.
func secretMetaToEnvVar(m secret.SecretMeta) store.EnvVar {
	return store.EnvVar{
		ID:            m.ID,
		Key:           m.Name,
		Value:         "********",
		Scope:         m.Scope,
		ScopeID:       m.ScopeID,
		Description:   m.Description,
		Sensitive:     true,
		Secret:        true,
		InjectionMode: m.InjectionMode,
		Created:       m.Created,
		Updated:       m.Updated,
		CreatedBy:     m.CreatedBy,
	}
}

func (s *Server) mergeEnvironmentSecrets(ctx context.Context, envVars []store.EnvVar, filter store.EnvVarFilter, warning string, installationOnly bool) []store.EnvVar {
	if s.secretBackend == nil {
		return envVars
	}

	metas, err := s.secretBackend.List(ctx, secret.Filter{
		Scope:   filter.Scope,
		ScopeID: filter.ScopeID,
		Type:    "environment",
		Name:    filter.Key,
	})
	if err != nil {
		s.envSecretLog.Warn(warning, "error", err)
		return envVars
	}
	metas = withoutSystemOneReservedSecrets(withoutAgentHiddenHubSecrets(ctx, filter.Scope, metas))
	if installationOnly {
		metas = withoutInstallationBlockedSecrets(metas)
	}
	return mergeEnvironmentSecretMetadata(envVars, metas)
}

// rejectSystemOneReservedSecretWrite rejects writes to the reserved
// "systemone.instance." secret namespace through generic secret and env
// endpoints so provider credentials are managed exclusively through the System
// One instance admin endpoints.
func rejectSystemOneReservedSecretWrite(w http.ResponseWriter, key string) bool {
	if !isSystemOneReservedSecretKey(key) {
		return false
	}
	ValidationError(w, "secret key prefix \"systemone.instance.\" is reserved for System One provider credentials", map[string]interface{}{
		"field": "key",
		"value": key,
	})
	return true
}

// agentHiddenHubSecret reports whether a hub-scope secret is hidden from the
// caller: the caller is an agent and the secret is not one agents may see
// (secret.HubSecretAgentVisible). Names, values and metadata of a hidden
// secret are never returned to an agent. Other scopes and non-agent callers
// are unaffected.
func agentHiddenHubSecret(ctx context.Context, scope string, m secret.SecretMeta) bool {
	if scope != store.ScopeHub {
		return false
	}
	if _, isAgent := GetIdentityFromContext(ctx).(AgentIdentity); !isAgent {
		return false
	}
	m.Scope = store.ScopeHub
	return !secret.HubSecretAgentVisible(m)
}

// withoutInternalSecrets drops rows whose SecretType is SecretTypeInternal.
// Internal secrets (such as conduit grant signing keys and webhook endpoint
// HMAC secrets) are never visible through user- or agent-facing secrets APIs
// at any scope.
func withoutInternalSecrets(metas []secret.SecretMeta) []secret.SecretMeta {
	out := make([]secret.SecretMeta, 0, len(metas))
	for _, m := range metas {
		if m.SecretType == store.SecretTypeInternal {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isStoredSecretInternal reports whether an existing secret at (key, scope,
// scopeID) has SecretType == store.SecretTypeInternal. Both the Hub store and
// the configured secret backend are consulted so hub-resident internal secrets
// are guarded regardless of backend mode.
func (s *Server) isStoredSecretInternal(ctx context.Context, key, scope, scopeID string) (bool, error) {
	if s.store != nil {
		sec, err := s.store.GetSecret(ctx, key, scope, scopeID)
		switch {
		case err == nil:
			if sec.SecretType == store.SecretTypeInternal {
				return true, nil
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			return false, err
		}
	}
	if s.secretBackend != nil {
		meta, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
		switch {
		case err == nil:
			if meta != nil && meta.SecretType == store.SecretTypeInternal {
				return true, nil
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			return false, err
		}
	}
	return false, nil
}

// refuseInternalSecretMutation writes a 409 internal_secret error and returns
// true when an existing secret at (key, scope, scopeID) has SecretTypeInternal.
func (s *Server) refuseInternalSecretMutation(w http.ResponseWriter, ctx context.Context, key, scope, scopeID string) bool {
	isInternal, err := s.isStoredSecretInternal(ctx, key, scope, scopeID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return true
	}
	if isInternal {
		writeError(w, http.StatusConflict, ErrCodeInternalSecret,
			"internal secrets cannot be modified or deleted through the secrets API", nil)
		return true
	}
	return false
}

// withoutAgentHiddenHubSecrets drops the secrets agentHiddenHubSecret hides.
func withoutAgentHiddenHubSecrets(ctx context.Context, scope string, metas []secret.SecretMeta) []secret.SecretMeta {
	out := make([]secret.SecretMeta, 0, len(metas))
	for _, m := range metas {
		if agentHiddenHubSecret(ctx, scope, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// applyEnvironmentSecretsToEnvVarList merges environment-type secret metadata
// into envVars for callers authorized to read secret metadata, or strips
// secret-flagged and secret-shadowed rows when an agent caller lacks
// ScopeProjectSecretRead (ptone/scion#8211). Returns false after writing an
// error response if shadow-check lookup fails closed.
func (s *Server) applyEnvironmentSecretsToEnvVarList(w http.ResponseWriter, ctx context.Context, envVars []store.EnvVar, filter store.EnvVarFilter, warning string, installationOnly bool) ([]store.EnvVar, bool) {
	if !agentLacksProjectSecretReadScope(ctx, filter.Scope) {
		return s.mergeEnvironmentSecrets(ctx, envVars, filter, warning, installationOnly), true
	}

	envVars = withoutSecretEnvVars(envVars)
	if len(envVars) == 0 || s.secretBackend == nil {
		return envVars, true
	}

	metas, err := s.secretBackend.List(ctx, secret.Filter{
		Scope:   filter.Scope,
		ScopeID: filter.ScopeID,
		Type:    store.SecretTypeEnvironment,
		Name:    filter.Key,
	})
	if err != nil {
		s.envSecretLog.Warn(warning, "error", err)
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	return suppressShadowedEnvVars(envVars, withoutSystemOneReservedSecrets(metas)), true
}

// suppressShadowedEnvVars drops env vars whose key matches an environment-type
// secret without appending secret metadata, so callers without
// ScopeProjectSecretRead see neither secret metadata nor shadowed plain values.
func suppressShadowedEnvVars(envVars []store.EnvVar, metas []secret.SecretMeta) []store.EnvVar {
	if len(envVars) == 0 || len(metas) == 0 {
		return envVars
	}

	secretKeys := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		secretKeys[meta.Name] = struct{}{}
	}

	filtered := make([]store.EnvVar, 0, len(envVars))
	for _, envVar := range envVars {
		if _, shadowed := secretKeys[envVar.Key]; shadowed {
			continue
		}
		filtered = append(filtered, envVar)
	}
	return filtered
}

func mergeEnvironmentSecretMetadata(envVars []store.EnvVar, metas []secret.SecretMeta) []store.EnvVar {
	if len(metas) == 0 {
		return envVars
	}

	filtered := suppressShadowedEnvVars(envVars, metas)
	merged := make([]store.EnvVar, 0, len(filtered)+len(metas))
	merged = append(merged, filtered...)
	for _, meta := range metas {
		merged = append(merged, secretMetaToEnvVar(meta))
	}
	return merged
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSecrets(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet)
	}
}

// agentLacksProjectSecretReadScope reports whether the caller is an agent
// reading project-scoped material without ScopeProjectSecretRead
// (ptone/scion#8211). Non-project scopes (e.g. scope=hub) and user callers
// return false.
func agentLacksProjectSecretReadScope(ctx context.Context, scope string) bool {
	if scope != store.ScopeProject {
		return false
	}
	agentIdent := GetAgentIdentityFromContext(ctx)
	return agentIdent != nil && !agentIdent.HasScope(ScopeProjectSecretRead)
}

// checkAgentProjectSecretReadScope verifies that agent callers reading
// project-scoped secret metadata hold ScopeProjectSecretRead (ptone/scion#1331).
// Non-project scopes (e.g. scope=hub) and user callers are unaffected.
func checkAgentProjectSecretReadScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	if agentLacksProjectSecretReadScope(r.Context(), scope) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Missing required scope: "+string(ScopeProjectSecretRead), nil)
		return false
	}
	return true
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	if s.serveSecretListWithPending(w, r, scope) {
		return
	}

	scopeID, _, ok := s.resolveEnvSecretAccessWithAgent(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}
	if !checkAgentProjectSecretReadScope(w, r, scope) {
		return
	}
	installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
	if !ok {
		return
	}

	metas, err := s.secretBackend.List(ctx, secret.Filter{
		Scope:   scope,
		ScopeID: scopeID,
		Name:    query.Get("key"),
		Type:    query.Get("type"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	metas = withoutInternalSecrets(withoutSystemOneReservedSecrets(withoutAgentHiddenHubSecrets(ctx, scope, metas)))
	if installationOnly {
		metas = withoutInstallationBlockedSecrets(metas)
	}
	// Convert to store.Secret for response compatibility
	secrets := make([]store.Secret, len(metas))
	for i, m := range metas {
		secrets[i] = metaToStoreSecret(m)
	}
	writeJSON(w, http.StatusOK, ListSecretsResponse{
		Secrets: secrets,
		Scope:   scope,
		ScopeID: scopeID,
	})
}

func (s *Server) handleSecretByKey(w http.ResponseWriter, r *http.Request) {
	key := extractID(r, "/api/v1/secrets")

	if key == "" {
		NotFound(w, "Secret")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getSecret(w, r, key)
	case http.MethodPut:
		s.setSecret(w, r, key)
	case http.MethodPatch:
		s.patchSecret(w, r, key)
	case http.MethodDelete:
		s.deleteSecret(w, r, key)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	if query.Has("includePending") {
		BadRequest(w, "includePending is only valid on the secret list (GET /api/v1/secrets)")
		return
	}

	scopeID, _, ok := s.resolveEnvSecretAccessWithAgent(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}
	if !checkAgentProjectSecretReadScope(w, r, scope) {
		return
	}
	installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
	if !ok {
		return
	}
	if isSystemOneReservedSecretKey(key) || (installationOnly && isBlockedInstallationGitHubEnvKey(key)) {
		writeErrorFromErr(w, store.ErrNotFound, "")
		return
	}

	meta, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
	if err == nil && (meta.SecretType == store.SecretTypeInternal || agentHiddenHubSecret(ctx, scope, *meta) || (installationOnly && isBlockedInstallationGitHubEnvKey(meta.Target))) {
		// The same response as a key that does not exist.
		err = store.ErrNotFound
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))
}

// validateEnvSecretTarget rejects an environment-type secret whose target
// falls under a reserved control-plane prefix (secret.IsReservedEnvTarget).
// It is a no-op for every other secret type, since only environment-type
// secrets are projected into the container environment by name. On success
// it returns true; on rejection it writes a validation error response
// (matching the shape used for the file-target checks below) and returns
// false, so callers can simply `if !validateEnvSecretTarget(...) { return }`.
func validateEnvSecretTarget(w http.ResponseWriter, secretType, target string) bool {
	if secretType != store.SecretTypeEnvironment && secretType != "" {
		return true
	}
	if !secret.IsReservedEnvTarget(target) {
		return true
	}
	ValidationError(w, "target is reserved for scion's own control-plane environment variables", map[string]interface{}{
		"field": "target",
		"value": target,
	})
	return false
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)

	var req SetSecretRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Value == "" {
		ValidationError(w, "value is required", nil)
		return
	}

	if req.Encoding != "" && req.Encoding != "base64" && req.Encoding != "raw" {
		ValidationError(w, "encoding must be \"base64\" or \"raw\"", map[string]interface{}{
			"field":   "encoding",
			"value":   req.Encoding,
			"allowed": []string{"base64", "raw"},
		})
		return
	}

	var decoded []byte
	if req.Encoding == "raw" {
		// Caller explicitly opted in to raw text — store the value as-is.
		decoded = []byte(req.Value)
	} else {
		// Default: value must be base64-encoded (matches CLI behaviour).
		var decErr error
		decoded, decErr = base64.StdEncoding.DecodeString(req.Value)
		if decErr != nil {
			BadRequest(w, "value must be base64-encoded")
			return
		}
	}

	// Validate and default secret type
	secretType := req.Type
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	switch secretType {
	case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
		// valid
	default:
		ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{
			"field": "type",
			"value": secretType,
		})
		return
	}

	// Default target to key
	target := req.Target
	if target == "" {
		target = key
	}

	if !validateEnvSecretTarget(w, secretType, target) {
		return
	}

	// Validate file-specific constraints
	if secretType == store.SecretTypeFile {
		if strings.Contains(target, "..") {
			BadRequest(w, "target path must not contain '..'")
			return
		}
		if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
			ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{
				"field": "target",
				"value": target,
			})
			return
		}
		if len(decoded) > 64*1024 {
			BadRequest(w, "secret value exceeds 64KB limit")
			return
		}
	}

	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, resolvedAgent, ok := s.resolveEnvSecretAccessWithAgent(w, r, scope, req.ScopeID, true)
	if !ok {
		return
	}
	if rejectSystemOneReservedSecretWrite(w, key) {
		return
	}
	if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
		return
	}
	if rejectWebhookEndpointReservedSecretWrite(w, key) {
		return
	}

	// allowProgeny is only valid on user-scoped secrets.
	// Unlike env vars, secrets do NOT require injectionMode=always when
	// allowProgeny is set — the secret dispatch flow supports as_needed
	// progeny secrets (design doc §5.7).
	if req.AllowProgeny && scope != store.ScopeUser {
		ValidationError(w, "allowProgeny is only supported on user-scoped secrets", map[string]interface{}{
			"field": "allowProgeny",
			"scope": scope,
		})
		return
	}

	input := &secret.SetSecretInput{
		Name:          key,
		Value:         string(decoded),
		SecretType:    secretType,
		Target:        target,
		Scope:         scope,
		ScopeID:       scopeID,
		Description:   req.Description,
		InjectionMode: req.InjectionMode,
		AllowProgeny:  req.AllowProgeny,
	}

	// Populate CreatedBy/UpdatedBy from authenticated user
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		input.CreatedBy = userIdent.ID()
		input.UpdatedBy = userIdent.ID()
		if scope == store.ScopeUser {
			input.UserEmail = userIdent.Email()
		}
	}
	if scope == store.ScopeAgent && resolvedAgent != nil {
		input.ProjectID = resolvedAgent.ProjectID
	}

	created, meta, err := s.secretBackend.Set(ctx, input)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Close the write vs hard-delete (or soft-delete) race: if the agent was
	// deleted while backend.Set was in flight, compensate by removing the
	// just-written secret and answering 404.
	if scope == store.ScopeAgent && s.compensateIfAgentDeleted(ctx, scopeID, []string{key}) {
		NotFound(w, "Agent")
		return
	}

	result := metaToStoreSecret(*meta)
	writeJSON(w, http.StatusOK, SetSecretResponse{
		Secret:  &result,
		Created: created,
	})
}

// patchSecretValidateAndUpdate is the shared helper for all PATCH secret
// handlers. It decodes and validates the PatchSecretRequest, applies the
// metadata update, and writes the response.
func (s *Server) patchSecretValidateAndUpdate(w http.ResponseWriter, r *http.Request, key, scope, scopeID string) {
	if rejectSystemOneReservedSecretWrite(w, key) {
		return
	}
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)

	var req PatchSecretRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate type if provided
	if req.Type != "" {
		switch req.Type {
		case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
			// valid
		default:
			ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{
				"field": "type",
				"value": req.Type,
			})
			return
		}
	}

	// Validate injectionMode if provided
	if req.InjectionMode != "" {
		switch req.InjectionMode {
		case store.InjectionModeAlways, store.InjectionModeAsNeeded:
			// valid
		default:
			ValidationError(w, "injectionMode must be \"always\" or \"as_needed\"", map[string]interface{}{
				"field":   "injectionMode",
				"value":   req.InjectionMode,
				"allowed": []string{"always", "as_needed"},
			})
			return
		}
	}

	if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
		return
	}
	if rejectWebhookEndpointReservedSecretWrite(w, key) {
		return
	}

	// Determine the effective secret type for target validation
	effectiveType := req.Type
	if effectiveType == "" && req.Target != "" {
		// Fetch stored type to validate target against it
		existing, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		effectiveType = existing.SecretType
	}

	// Resolve the effective target (including the stored target when only
	// the type is changing) so the file- and environment-specific checks
	// below see the target the update will actually store.
	effectiveTarget := req.Target
	if (effectiveType == store.SecretTypeFile || effectiveType == store.SecretTypeEnvironment) && effectiveTarget == "" {
		existing, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		effectiveTarget = existing.Target
	}
	if effectiveTarget != "" && effectiveType == store.SecretTypeFile {
		if strings.Contains(effectiveTarget, "..") {
			BadRequest(w, "target path must not contain '..'")
			return
		}
		if !strings.HasPrefix(effectiveTarget, "/") && !strings.HasPrefix(effectiveTarget, "~/") {
			ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{
				"field": "target",
				"value": effectiveTarget,
			})
			return
		}
	}
	if !validateEnvSecretTarget(w, effectiveType, effectiveTarget) {
		return
	}

	// allowProgeny is only valid on user-scoped secrets.
	// Unlike env vars, secrets do NOT require injectionMode=always when
	// allowProgeny is set — the secret dispatch flow supports as_needed
	// progeny secrets (design doc §5.7).
	if req.AllowProgeny != nil && *req.AllowProgeny && scope != store.ScopeUser {
		ValidationError(w, "allowProgeny is only supported on user-scoped secrets", map[string]interface{}{
			"field": "allowProgeny",
			"scope": scope,
		})
		return
	}

	input := &secret.UpdateMetaInput{
		Name:          key,
		Scope:         scope,
		ScopeID:       scopeID,
		Description:   req.Description,
		InjectionMode: req.InjectionMode,
		SecretType:    req.Type,
		Target:        req.Target,
		AllowProgeny:  req.AllowProgeny,
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		input.UpdatedBy = userIdent.ID()
	}

	meta, err := s.secretBackend.UpdateMeta(ctx, input)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	result := metaToStoreSecret(*meta)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) patchSecret(w http.ResponseWriter, r *http.Request, key string) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, _, ok := s.resolveEnvSecretAccessWithAgent(w, r, scope, r.URL.Query().Get("scopeId"), true)
	if !ok {
		return
	}

	s.patchSecretValidateAndUpdate(w, r, key, scope, scopeID)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, _, ok := s.resolveEnvSecretAccessWithAgent(w, r, scope, query.Get("scopeId"), true)
	if !ok {
		return
	}
	if isSystemOneReservedSecretKey(key) {
		writeErrorFromErr(w, store.ErrNotFound, "")
		return
	}
	if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
		return
	}

	if err := s.secretBackend.Delete(ctx, key, scope, scopeID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AgentSetSecretRequest is the request body for agent-initiated secret creation.
type AgentSetSecretRequest struct {
	Value    string `json:"value"`              // Secret value (base64-encoded by default; use Encoding:"raw" for literal text)
	Encoding string `json:"encoding,omitempty"` // "base64" (default) or "raw" (value is literal text, no decoding)
	Type     string `json:"type,omitempty"`     // environment (default), variable, file
	Target   string `json:"target,omitempty"`   // Injection target path
	Force    bool   `json:"force,omitempty"`    // Overwrite existing secret
	Scope    string `json:"scope,omitempty"`    // "project" (default), "user", or "agent"
	// AllowProgeny opts the secret in to progeny inheritance (user scope only).
	// A pointer so an unset field is distinguishable from an explicit false;
	// unset resolves to false (opt-in, per design doc §5.1).
	AllowProgeny *bool `json:"allowProgeny,omitempty"`
}

// LogValue redacts the secret value so AgentSetSecretRequest can never leak
// secret material into structured logs.
func (r AgentSetSecretRequest) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("value", "[REDACTED]"),
		slog.String("encoding", r.Encoding),
		slog.String("type", r.Type),
		slog.String("target", r.Target),
		slog.Bool("force", r.Force),
		slog.String("scope", r.Scope),
	}
	if r.AllowProgeny != nil {
		attrs = append(attrs, slog.Bool("allow_progeny", *r.AllowProgeny))
	}
	return slog.GroupValue(attrs...)
}

// AgentSetSecretResponse is returned on successful agent secret creation.
type AgentSetSecretResponse struct {
	Key     string `json:"key"`
	Scope   string `json:"scope"`
	ScopeID string `json:"scopeId"`
}

// AgentGetSecretResponse is returned when an agent retrieves a single secret.
type AgentGetSecretResponse struct {
	Key    string `json:"key"`
	Value  string `json:"value"` // base64-encoded secret value
	Type   string `json:"type"`  // environment, variable, file
	Target string `json:"target"`
	Scope  string `json:"scope,omitempty"` // agent, project, user
}

// AgentListSecretsResponse is returned when an agent lists available secrets.
type AgentListSecretsResponse struct {
	Secrets []AgentSecretMeta `json:"secrets"`
}

// AgentSecretMeta is secret metadata returned in list responses (no value).
type AgentSecretMeta struct {
	Key    string `json:"key"`
	Type   string `json:"type"`   // environment, variable, file
	Target string `json:"target"` // injection target
	Scope  string `json:"scope,omitempty"`
}

// handleAgentSecrets handles agent secret operations:
//
//	GET    /api/v1/agents/{agentID}/secrets          — list secret metadata
//	GET    /api/v1/agents/{agentID}/secrets/{key}    — retrieve a single secret value
//	PUT    /api/v1/agents/{agentID}/secrets/{key}    — create/update a secret
//	DELETE /api/v1/agents/{agentID}/secrets/{key}    — delete an agent-scoped secret
//
// Only agents may call this endpoint.
func (s *Server) handleAgentSecrets(w http.ResponseWriter, r *http.Request, agentID, subPath string) {
	key := strings.TrimPrefix(subPath, "/")

	if s.secretBackend == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "secret storage requires a configured secrets backend",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		if key == "" {
			s.agentListSecrets(w, r, agentID)
		} else {
			s.agentGetSecret(w, r, agentID, key)
		}
		return
	case http.MethodPut:
		if key == "" {
			BadRequest(w, "Secret key is required in the URL path")
			return
		}
		// Fall through to existing PUT logic below.
	case http.MethodDelete:
		s.agentDeleteSecret(w, r, agentID, key)
		return
	default:
		if s.experimentEnabled(experiments.AgentSecrets) {
			MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		} else {
			MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
		return
	}

	// Validate key characters.
	if strings.ContainsAny(key, "= \t\n") {
		ValidationError(w, "secret key cannot contain spaces, tabs, newlines, or '='", map[string]interface{}{
			"field": "key",
			"value": key,
		})
		return
	}
	if rejectSystemOneReservedSecretWrite(w, key) {
		return
	}

	ctx := r.Context()

	projectID, ok := s.validateAgentSecretAccess(w, r, agentID)
	if !ok {
		return
	}
	// Writing a secret requires good standing (ptone/scion#3433). The read
	// paths check it in materialRuntimePrecheck.
	if s.agentStandingForbidden(ctx, w, agentID) {
		return
	}

	// Limit request body to 128 KiB (64 KiB value limit + headroom for JSON envelope).
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)

	var req AgentSetSecretRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Value == "" {
		ValidationError(w, "value is required", nil)
		return
	}

	if req.Encoding != "" && req.Encoding != "base64" && req.Encoding != "raw" {
		ValidationError(w, "encoding must be \"base64\" or \"raw\"", map[string]interface{}{
			"field":   "encoding",
			"value":   req.Encoding,
			"allowed": []string{"base64", "raw"},
		})
		return
	}

	// Determine scope and scopeID.
	agentSecretsOn := s.experimentEnabled(experiments.AgentSecrets)
	scope := req.Scope
	if scope == "" {
		scope = store.ScopeProject
	}
	var scopeID string
	switch scope {
	case store.ScopeProject:
		scopeID = projectID
	case store.ScopeUser:
		agentIdent := GetAgentIdentityFromContext(ctx)
		if agentIdent == nil {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "This endpoint requires agent authentication", nil)
			return
		}
		scopeID = agentIdent.OriginUserID()
		if scopeID == "" {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent token lacks user context required for user-scoped secrets", nil)
			return
		}
	case store.ScopeAgent:
		if !agentSecretsOn {
			BadRequest(w, "invalid scope: "+scope)
			return
		}
		scopeID = agentID
	default:
		allowed := []string{"project", "user"}
		msg := "scope must be \"project\" or \"user\""
		if agentSecretsOn {
			allowed = []string{"agent", "project", "user"}
			msg = "scope must be \"agent\", \"project\", or \"user\""
		}
		ValidationError(w, msg, map[string]interface{}{
			"field":   "scope",
			"value":   scope,
			"allowed": allowed,
		})
		return
	}

	// Hub admin policy: when agent_secrets.user_scope_only is on, agents may
	// not write project-scope secrets at all. This is a blanket rule on
	// every agent-originated project-scope write (design ptone/scion#2291
	// §6) — it covers harness auth capture and ad-hoc `sciontool secret set`
	// alike. It is checked before allowProgeny/base64-decode/type/conflict/
	// GetMeta, so it cannot be bypassed by `force` and the request never
	// reaches the backend. (Value/Encoding validation above still runs
	// first and fails closed on its own terms — an empty value or an
	// unrecognized encoding gets its own 400/422 either way.)
	if scope == store.ScopeProject && s.agentSecretsUserScopeOnly() {
		slog.Info("agent project-scope secret write rejected by policy",
			"agent_id", agentID, "project_id", projectID, "key", key)
		writeError(w, http.StatusForbidden, ErrCodeSecretScopeRestricted,
			"The hub administrator has restricted agent-written secrets to user (profile) scope; "+
				"project-scope writes are not allowed. Retry with scope \"user\" (sciontool: --scope user).",
			map[string]interface{}{
				"field":         "scope",
				"value":         "project",
				"allowedScopes": []string{"user"},
				"setting":       "agent_secrets.user_scope_only",
			})
		return
	}

	// allowProgeny is only valid on user-scoped secrets. Only an explicit
	// true is rejected; unset on a project-scoped write is fine and simply
	// resolves to false below.
	// Unlike env vars, secrets do NOT require injectionMode=always when
	// allowProgeny is set — the secret dispatch flow supports as_needed
	// progeny secrets (design doc §5.7).
	if req.AllowProgeny != nil && *req.AllowProgeny && scope != store.ScopeUser {
		ValidationError(w, "allowProgeny is only supported on user-scoped secrets", map[string]interface{}{
			"field": "allowProgeny",
			"scope": scope,
		})
		return
	}

	var decoded []byte
	if req.Encoding == "raw" {
		// Caller explicitly opted in to raw text — store the value as-is.
		decoded = []byte(req.Value)
	} else {
		// Default: value must be base64-encoded (matches CLI behaviour).
		var decErr error
		decoded, decErr = base64.StdEncoding.DecodeString(req.Value)
		if decErr != nil {
			BadRequest(w, "value must be base64-encoded")
			return
		}
	}

	// Validate and default secret type.
	secretType := req.Type
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	switch secretType {
	case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
		// valid
	default:
		ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{
			"field": "type",
			"value": secretType,
		})
		return
	}

	// Default target to key name.
	target := req.Target
	if target == "" {
		target = key
	}

	if scope == store.ScopeAgent {
		// Shared agent-scope secret write constraints (reserved env targets,
		// ~/ file-target rule with no '..', System One & webhook reserved
		// keys, and internal secret mutation refusal).
		if !s.validateAgentSecretWriteConstraints(w, ctx, agentID, key, secretType, target) {
			return
		}
		if secretType == store.SecretTypeFile && len(decoded) > 64*1024 {
			BadRequest(w, "secret value exceeds 64KB limit")
			return
		}
	} else {
		if !validateEnvSecretTarget(w, secretType, target) {
			return
		}

		// Validate file-specific constraints for project/user scopes.
		if secretType == store.SecretTypeFile {
			if strings.Contains(target, "..") {
				BadRequest(w, "target path must not contain '..'")
				return
			}
			if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
				ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{
					"field": "target",
					"value": target,
				})
				return
			}
			if len(decoded) > 64*1024 {
				BadRequest(w, "secret value exceeds 64KB limit")
				return
			}
		}

		// A user-scope write by an agent waits for the owning user's approval:
		// it is recorded as a pending secret and never written to the secrets
		// table here, so force does not apply. An approved secret of the same
		// name stays in effect until the owner approves the replacement.
		if scope == store.ScopeUser {
			allowProgeny := req.AllowProgeny != nil && *req.AllowProgeny
			s.agentWritePendingUserSecret(w, r, agentID, projectID, scopeID, key, decoded, secretType, target, allowProgeny)
			return
		}

		if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
			return
		}
		if rejectWebhookEndpointReservedSecretWrite(w, key) {
			return
		}
	}

	// Check for existing secret when force is not set.
	// Note: the backend's UpsertSecret has the same check-then-write pattern
	// internally, so this is consistent with the existing TOCTOU window.
	if !req.Force {
		_, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
		if err == nil {
			Conflict(w, fmt.Sprintf("Secret %q already exists at %s scope. Use force=true to overwrite.", key, scope))
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Progeny opt-in is opt-in per design (§5.1): an unset AllowProgeny
	// resolves to false. Writers that need progeny access — such as harness
	// credential capture — are responsible for setting it explicitly.
	allowProgeny := false
	if req.AllowProgeny != nil {
		allowProgeny = *req.AllowProgeny
	}

	// Attribution: only project- and agent-scope writes reach this point. A
	// user-scope write is attributed when its owner approves it (approvePendingSecret).
	createdBy := fmt.Sprintf("agent:%s", agentID)

	input := &secret.SetSecretInput{
		Name:         key,
		Value:        string(decoded),
		SecretType:   secretType,
		Target:       target,
		Scope:        scope,
		ScopeID:      scopeID,
		AllowProgeny: allowProgeny,
		CreatedBy:    createdBy,
		UpdatedBy:    fmt.Sprintf("agent:%s", agentID),
	}
	if scope == store.ScopeAgent {
		input.ProjectID = projectID
	}

	created, meta, err := s.secretBackend.Set(ctx, input)
	if err != nil {
		s.logAgentSecretWrite(ctx, "set", agentID, projectID, scope, scopeID, key, 0, false, false, "backend_error")
		writeErrorFromErr(w, err, "")
		return
	}

	version := 0
	if meta != nil {
		version = meta.Version
	}

	// Close the write vs hard-delete (or soft-delete) race: if the agent was
	// deleted while backend.Set was in flight, compensate by removing the
	// just-written secret and answering 404.
	if scope == store.ScopeAgent && s.compensateIfAgentDeleted(ctx, agentID, []string{key}) {
		s.logAgentSecretWrite(ctx, "set", agentID, projectID, scope, scopeID, key, version, created, false, "agent_deleted")
		NotFound(w, "Agent")
		return
	}

	s.logAgentSecretWrite(ctx, "set", agentID, projectID, scope, scopeID, key, version, created, true, "")

	if created {
		writeJSON(w, http.StatusCreated, AgentSetSecretResponse{
			Key:     key,
			Scope:   scope,
			ScopeID: scopeID,
		})
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

// agentDeleteSecret handles DELETE /api/v1/agents/{agentID}/secrets/{key}.
// Only agent-scoped secrets may be deleted by the owning agent, and only when
// the hub.agent_secrets experiment is enabled.
func (s *Server) agentDeleteSecret(w http.ResponseWriter, r *http.Request, agentID, key string) {
	ctx := r.Context()
	agentSecretsOn := s.experimentEnabled(experiments.AgentSecrets)
	scope := r.URL.Query().Get("scope")

	// When hub.agent_secrets is off, preserve pre-experiment behavior: DELETE
	// without scope=agent returns 405 Method Not Allowed. An explicit
	// scope=agent request is refused with 400 "invalid scope: agent" after auth.
	if !agentSecretsOn && scope != store.ScopeAgent {
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}

	if key == "" {
		BadRequest(w, "Secret key is required in the URL path")
		return
	}
	if strings.ContainsAny(key, "= \t\n") {
		ValidationError(w, "secret key cannot contain spaces, tabs, newlines, or '='", map[string]interface{}{
			"field": "key",
			"value": key,
		})
		return
	}
	if rejectSystemOneReservedSecretWrite(w, key) {
		return
	}

	projectID, ok := s.validateAgentSecretAccess(w, r, agentID)
	if !ok {
		return
	}
	if s.agentStandingForbidden(ctx, w, agentID) {
		return
	}

	if !agentSecretsOn {
		BadRequest(w, "invalid scope: "+scope)
		return
	}
	if scope != store.ScopeAgent {
		ValidationError(w, "scope must be \"agent\" for agent secret deletion", map[string]interface{}{
			"field":   "scope",
			"value":   scope,
			"allowed": []string{store.ScopeAgent},
		})
		return
	}

	if s.refuseInternalSecretMutation(w, ctx, key, store.ScopeAgent, agentID) {
		return
	}

	meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeAgent, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.logAgentSecretWrite(ctx, "delete", agentID, projectID, store.ScopeAgent, agentID, key, 0, false, false, "not_found")
		} else {
			s.logAgentSecretWrite(ctx, "delete", agentID, projectID, store.ScopeAgent, agentID, key, 0, false, false, "backend_error")
		}
		writeErrorFromErr(w, err, "")
		return
	}

	version := 0
	if meta != nil {
		version = meta.Version
	}

	if err := s.secretBackend.Delete(ctx, key, store.ScopeAgent, agentID); err != nil {
		s.logAgentSecretWrite(ctx, "delete", agentID, projectID, store.ScopeAgent, agentID, key, version, false, false, "backend_error")
		writeErrorFromErr(w, err, "")
		return
	}

	s.logAgentSecretWrite(ctx, "delete", agentID, projectID, store.ScopeAgent, agentID, key, version, false, true, "")
	w.WriteHeader(http.StatusNoContent)
}

// validateAgentSecretAccess checks that the request is from an authenticated agent
// whose JWT subject matches the agentID in the URL, and extracts the project ID.
// On failure it writes an HTTP error response and returns ("", false).
func (s *Server) validateAgentSecretAccess(w http.ResponseWriter, r *http.Request, agentID string) (projectID string, ok bool) {
	ctx := r.Context()

	// Agent-only: require agent identity from JWT.
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "This endpoint requires agent authentication", nil)
		return "", false
	}

	// The agentID in the URL path must match the JWT subject.
	if agentIdent.ID() != agentID {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agent token does not match the agent ID in the URL", nil)
		return "", false
	}

	// Extract project ID from agent token claims.
	projectID = agentIdent.ProjectID()
	if projectID == "" {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agent token lacks project context", nil)
		return "", false
	}

	return projectID, true
}

// agentGetSecret handles GET /api/v1/agents/{agentID}/secrets/{key}.
// Returns the secret value (base64-encoded) along with type and target metadata.
// Supports both project-scoped and user-scoped secrets via the ?scope= query parameter.
//
// The runtime material check sequence (material_runtime.go) runs after
// validateAgentSecretAccess (not modified here): the whole-request precheck
// (checks 1-6), then the per-item check for the requested scope (check 7
// project, check 8 user) and the record-race rule (check 9).
func (s *Server) agentGetSecret(w http.ResponseWriter, r *http.Request, agentID, key string) {
	ctx := r.Context()

	_, ok := s.validateAgentSecretAccess(w, r, agentID)
	if !ok {
		// This path runs before the precheck, so no TargetFacts exist and no
		// MaterialSelectionEvent is emitted: there is no partner event for
		// the compat record to be derived from.
		s.logAgentSecretReadCompat(ctx, agentID, "", "", "", key, false, "auth failed", false, "")
		return
	}

	ident := GetAgentIdentityFromContext(ctx)
	correlationID := newMaterialCorrelationID()
	agentSecretsOn := s.experimentEnabled(experiments.AgentSecrets)

	rawScope := r.URL.Query().Get("scope")
	// Agent-scope reads (or default agent-then-project when experiment is on)
	// do not require project:secret:read at the precheck stage.
	requireProjectReadScope := !agentSecretsOn || rawScope == store.ScopeProject || rawScope == store.ScopeUser
	// A viewer-only root is admitted only for explicit user-scope allowProgeny
	// reads (ptone/scion#8875); project, agent, and default (project or
	// agent-then-project) reads stay refused at check 5.
	allowViewerRoot := rawScope == store.ScopeUser
	facts, reason, status := s.materialRuntimePrecheckAllowViewer(ctx, ident, requireProjectReadScope, allowViewerRoot)
	if status != 0 {
		s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "get", correlationID, nil, reason, nil))
		if status == http.StatusInternalServerError {
			writeError(w, status, ErrCodeRuntimeError, agentSecretAccessErrorMessage, nil)
			return
		}
		writeError(w, status, ErrCodeForbidden, agentSecretAccessDeniedMessage, nil)
		return
	}

	// Determine scope (check 6).
	scope := rawScope
	defaultAgentThenProject := false
	if scope == "" {
		if agentSecretsOn {
			defaultAgentThenProject = true
		} else {
			scope = store.ScopeProject
		}
	}
	if !defaultAgentThenProject {
		valid := false
		switch scope {
		case store.ScopeProject, store.ScopeUser:
			valid = true
		case store.ScopeAgent:
			valid = agentSecretsOn
		}
		if !valid {
			// An invalid scope parameter still emits the request's
			// MaterialSelectionEvent rather than exiting silently.
			s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "get", correlationID, facts, ReasonInvalidScope, nil))
			if scope == store.ScopeAgent && !agentSecretsOn {
				BadRequest(w, "invalid scope: "+scope)
				return
			}
			allowed := []string{"project", "user"}
			msg := "scope must be \"project\" or \"user\""
			if agentSecretsOn {
				allowed = []string{"agent", "project", "user"}
				msg = "scope must be \"agent\", \"project\", or \"user\""
			}
			ValidationError(w, msg, map[string]interface{}{
				"field":   "scope",
				"value":   scope,
				"allowed": allowed,
			})
			return
		}
	}

	var (
		decisionCache projectDecisionCache
		item          ItemResult
		sv            *secret.SecretWithValue
		permission    string
		detail        string
	)
	if defaultAgentThenProject {
		agentItem, agentSV, agentPerm, agentDetail := s.selectRuntimeMaterial(ctx, ident, facts, store.ScopeAgent, key, &decisionCache)
		if agentItem.Selected || agentItem.Allowed || agentItem.Reason == ReasonBackendError {
			item, sv, permission, detail = agentItem, agentSV, agentPerm, agentDetail
		} else {
			item, sv, permission, detail = s.selectRuntimeMaterial(ctx, ident, facts, store.ScopeProject, key, &decisionCache)
		}
	} else {
		item, sv, permission, detail = s.selectRuntimeMaterial(ctx, ident, facts, scope, key, &decisionCache)
	}

	emit := func() {
		items := []MaterialSelectionEventItem{materialSelectionItem(item, permission, detail)}
		s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "get", correlationID, facts, "", items))
	}

	switch {
	case item.Selected:
		s.logAgentSecretReadCompatVersion(ctx, agentID, facts.ProjectID, item.Scope, item.ScopeID, key, sv.Version, true, "", true, correlationID)
		emit()
		writeJSON(w, http.StatusOK, AgentGetSecretResponse{
			Key:    sv.Name,
			Value:  base64.StdEncoding.EncodeToString([]byte(sv.Value)),
			Type:   sv.SecretType,
			Target: sv.Target,
			Scope:  sv.Scope,
		})
	case !item.Allowed && item.Reason != ReasonBackendError:
		// Check 7 or 8 denied the item for a reason other than an
		// infrastructure fault: not_found, never a value.
		s.logAgentSecretReadCompat(ctx, agentID, facts.ProjectID, item.Scope, item.ScopeID, key, false, item.Reason, true, correlationID)
		emit()
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "secret not found", nil)
	default:
		// Either checks 7/8 failed with a backend error, or they allowed the
		// item but check 9 (the record-race rule) did not: both report
		// unavailable, never a value.
		s.logAgentSecretReadCompat(ctx, agentID, facts.ProjectID, item.Scope, item.ScopeID, key, false, item.Reason, true, correlationID)
		emit()
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "secret unavailable", nil)
	}
}

// agentListSecrets handles GET /api/v1/agents/{agentID}/secrets (no key).
// Returns metadata for secrets accessible to the agent.
// Supports both project-scoped and user-scoped secrets via the ?scope= query parameter.
// When no scope is specified, secrets from both project and user scopes are returned.
//
// Applies the whole-request checks 1-6 precheck, then filters metadata: it
// reads no value and has no check-9 step. It lists only keys the agent could
// read.
func (s *Server) agentListSecrets(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()

	_, ok := s.validateAgentSecretAccess(w, r, agentID)
	if !ok {
		return
	}

	ident := GetAgentIdentityFromContext(ctx)
	correlationID := newMaterialCorrelationID()
	agentSecretsOn := s.experimentEnabled(experiments.AgentSecrets)

	scope := r.URL.Query().Get("scope")
	requireProjectReadScope := !agentSecretsOn || scope == store.ScopeProject || scope == store.ScopeUser
	// A viewer-only root is admitted only when the request includes user scope
	// (scope=user, multi-scope empty, or scope=all when agent secrets are
	// enabled) and the token holds project:secret:read; scope=project,
	// scope=agent, and unknown scopes stay refused at check 5 (ptone/scion#8875).
	allowViewerRoot := (scope == "" || (agentSecretsOn && scope == "all") || scope == store.ScopeUser) && ident != nil && ident.HasScope(ScopeProjectSecretRead)
	facts, reason, status := s.materialRuntimePrecheckAllowViewer(ctx, ident, requireProjectReadScope, allowViewerRoot)
	if status != 0 {
		s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "list", correlationID, nil, reason, nil))
		if status == http.StatusInternalServerError {
			writeError(w, status, ErrCodeRuntimeError, agentSecretAccessErrorMessage, nil)
			return
		}
		writeError(w, status, ErrCodeForbidden, agentSecretAccessDeniedMessage, nil)
		return
	}

	// emitListExitItem records the request's MaterialSelectionEvent with a
	// single request-level item before a whole-request exit, so a backend
	// fault is never a silent exit: every exit from this handler leaves a
	// trace, the same way the decision-error branch already did.
	emitListExitItem := func(scope, scopeID string, grant GrantKind, permission string) {
		item := materialSelectionItem(ItemResult{
			Candidate: Candidate{Kind: MaterialKindSecret, Scope: scope, ScopeID: scopeID, Grant: grant},
			Reason:    ReasonBackendError,
		}, permission, "")
		s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "list", correlationID, facts, "", []MaterialSelectionEventItem{item}))
	}

	validScope := false
	switch scope {
	case "", store.ScopeProject, store.ScopeUser:
		validScope = true
	case store.ScopeAgent, "all":
		validScope = agentSecretsOn
	}
	if !validScope {
		// An invalid scope parameter still emits the request's
		// MaterialSelectionEvent rather than exiting silently.
		s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "list", correlationID, facts, ReasonInvalidScope, nil))
		if scope == store.ScopeAgent && !agentSecretsOn {
			BadRequest(w, "invalid scope: "+scope)
			return
		}
		allowed := []string{"project", "user"}
		msg := "scope must be \"project\" or \"user\""
		if agentSecretsOn {
			allowed = []string{"agent", "project", "user", "all"}
			msg = "scope must be \"agent\", \"project\", \"user\", or \"all\""
		}
		ValidationError(w, msg, map[string]interface{}{
			"field":   "scope",
			"value":   scope,
			"allowed": allowed,
		})
		return
	}

	includeAgent := agentSecretsOn && (scope == "" || scope == "all" || scope == store.ScopeAgent)
	includeProject := scope == "" || scope == "all" || scope == store.ScopeProject
	includeUser := scope == "" || scope == "all" || scope == store.ScopeUser

	secrets := make([]AgentSecretMeta, 0)
	items := make([]MaterialSelectionEventItem, 0)

	if includeAgent {
		if facts.RootViewerOnly {
			// A viewer-only root is admitted solely for user-scope progeny
			// secrets (ptone/scion#8875); agent-scope secrets stay refused
			// without reading backend metadata.
			items = append(items, materialSelectionItem(ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Scope: store.ScopeAgent, ScopeID: facts.Agent.ID, Grant: GrantAgentSelfRead},
				Reason:    ReasonMembershipRequired,
			}, "", ""))
		} else {
			metas, err := s.secretBackend.List(ctx, secret.Filter{
				Scope:   store.ScopeAgent,
				ScopeID: facts.Agent.ID,
			})
			if err != nil {
				emitListExitItem(store.ScopeAgent, facts.Agent.ID, GrantAgentSelfRead, "")
				writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to list secrets", nil)
				return
			}
			for _, m := range metas {
				if m.SecretType == store.SecretTypeInternal {
					continue
				}
				if usesInstallationGitHubToken(facts.Agent) &&
					(isBlockedInstallationGitHubEnvKey(m.Name) || isBlockedInstallationGitHubEnvKey(m.Target)) {
					continue
				}
				secrets = append(secrets, AgentSecretMeta{Key: m.Name, Type: m.SecretType, Target: m.Target, Scope: store.ScopeAgent})
				items = append(items, materialSelectionItem(ItemResult{
					Candidate: Candidate{Kind: MaterialKindSecret, Key: m.Name, Scope: store.ScopeAgent, ScopeID: facts.Agent.ID, Grant: GrantAgentSelfRead, Meta: m},
					Allowed:   true,
					Reason:    ReasonAllowed,
				}, "", ""))
			}
		}
	}

	if includeProject {
		if !ident.HasScope(ScopeProjectSecretRead) {
			items = append(items, materialSelectionItem(ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Scope: store.ScopeProject, ScopeID: facts.ProjectID, Grant: GrantProjectSecretRead},
				Reason:    ReasonCapabilityRequired,
			}, "project.secret_read", ""))
		} else if facts.RootViewerOnly {
			// A viewer-only root is admitted solely for user-scope progeny
			// secrets (ptone/scion#8875); project-scope secrets stay refused
			// without reading backend metadata.
			items = append(items, materialSelectionItem(ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Scope: store.ScopeProject, ScopeID: facts.ProjectID, Grant: GrantProjectSecretRead},
				Reason:    ReasonMembershipRequired,
			}, "project.secret_read", ""))
		} else {
			var cache projectDecisionCache
			decision, decErr := s.projectReadDecision(ctx, ident, facts, &cache)
			if decErr != nil {
				// A decision error still emits the request's
				// MaterialSelectionEvent, with a request-level item recording
				// the failure, rather than exiting silently.
				emitListExitItem(store.ScopeProject, facts.ProjectID, GrantProjectSecretRead, "project.secret_read")
				writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to list secrets", nil)
				return
			}
			// Decision first, then metadata: a denial leaves the project part
			// of the list empty and makes no GetMeta/List call.
			if decision.Allowed {
				metas, err := s.secretBackend.List(ctx, secret.Filter{
					Scope:   store.ScopeProject,
					ScopeID: facts.ProjectID,
				})
				if err != nil {
					emitListExitItem(store.ScopeProject, facts.ProjectID, GrantProjectSecretRead, "project.secret_read")
					writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to list secrets", nil)
					return
				}
				for _, m := range metas {
					if m.SecretType == store.SecretTypeInternal {
						continue
					}
					if usesInstallationGitHubToken(facts.Agent) &&
						(isBlockedInstallationGitHubEnvKey(m.Name) || isBlockedInstallationGitHubEnvKey(m.Target)) {
						continue
					}
					secrets = append(secrets, AgentSecretMeta{Key: m.Name, Type: m.SecretType, Target: m.Target, Scope: store.ScopeProject})
					items = append(items, materialSelectionItem(ItemResult{
						Candidate: Candidate{Kind: MaterialKindSecret, Key: m.Name, Scope: store.ScopeProject, ScopeID: facts.ProjectID, Grant: GrantProjectSecretRead, Meta: m},
						Allowed:   true,
						Reason:    ReasonAllowed,
					}, "project.secret_read", ""))
				}
			} else {
				// The project part of the list is empty, but the denial and its
				// Detail are still recorded as a request-level item, rather than
				// leaving no trace of the project scope having been evaluated.
				items = append(items, materialSelectionItem(ItemResult{
					Candidate: Candidate{Kind: MaterialKindSecret, Scope: store.ScopeProject, ScopeID: facts.ProjectID, Grant: GrantProjectSecretRead},
					Reason:    ReasonDeniedByPolicy,
				}, "project.secret_read", decision.Reason))
			}
		}
	}

	if includeUser {
		if ident.HasScope(ScopeProjectSecretRead) {
			var userOK bool
			secrets, items, userOK = s.agentListUserSecrets(ctx, agentID, ident, facts, secrets, items)
			if !userOK {
				emitListExitItem(store.ScopeUser, facts.Root.ID, GrantProgeny, "")
				writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to list secrets", nil)
				return
			}
		}
	}

	s.logMaterialSelection(ctx, s.buildMaterialSelectionEvent(ctx, "list", correlationID, facts, "", items))

	writeJSON(w, http.StatusOK, AgentListSecretsResponse{
		Secrets: secrets,
	})
}

func (s *Server) handleProjectEnvVars(w http.ResponseWriter, r *http.Request, projectID string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ProjectID() != projectID {
			Forbidden(w)
			return
		}
		if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
			return
		}
		// Agents only get read access
	} else if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
		if !ok {
			return
		}
		filter := store.EnvVarFilter{
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		}
		envVars, err := s.store.ListEnvVars(ctx, filter)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		envVars, ok = s.applyEnvironmentSecretsToEnvVarList(w, ctx, envVars, filter,
			"failed to list environment secrets for project env var merge", installationOnly)
		if !ok {
			return
		}
		if installationOnly {
			envVars = withoutInstallationBlockedEnvVars(envVars)
		}
		// Mask sensitive values
		for i := range envVars {
			if envVars[i].Sensitive {
				envVars[i].Value = "********"
			}
		}
		writeJSON(w, http.StatusOK, ListEnvVarsResponse{
			EnvVars: envVars,
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		})
	default:
		MethodNotAllowed(w, http.MethodGet)
	}
}

// handleScopedEnvVarByKey handles an env-var lifecycle after the caller has
// verified the scope resource and authorized the request.
func (s *Server) handleScopedEnvVarByKey(w http.ResponseWriter, r *http.Request, key, scope, scopeID string) {
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		s.serveEnvVarByKey(w, ctx, key, scope, scopeID)

	case http.MethodPut:
		var req SetEnvVarRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Value == "" {
			ValidationError(w, "value is required", nil)
			return
		}

		// See setEnvVar: the key is itself the container-env name.
		if !validateEnvSecretTarget(w, store.SecretTypeEnvironment, key) {
			return
		}
		if rejectSystemOneReservedSecretWrite(w, key) {
			return
		}

		var createdBy string
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
		}

		if req.Secret {
			if s.secretBackend == nil {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
				return
			}
			if rejectWebhookEndpointReservedSecretWrite(w, key) {
				return
			}
			input := &secret.SetSecretInput{
				Name:          key,
				Value:         req.Value,
				SecretType:    "environment",
				Target:        key,
				Scope:         scope,
				ScopeID:       scopeID,
				Description:   req.Description,
				InjectionMode: req.InjectionMode,
				CreatedBy:     createdBy,
				UpdatedBy:     createdBy,
			}
			created, meta, err := s.secretBackend.Set(ctx, input)
			if err != nil {
				if errors.Is(err, secret.ErrNoSecretBackend) {
					writeJSON(w, http.StatusNotImplemented, map[string]string{
						"error": "secret storage requires a configured secrets backend",
					})
					return
				}
				writeErrorFromErr(w, err, "")
				return
			}
			_ = s.store.DeleteEnvVar(ctx, key, scope, scopeID)
			syntheticEnvVar := secretMetaToEnvVar(*meta)
			writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: &syntheticEnvVar, Created: created})
			return
		}

		if rejectWebhookEndpointReservedSecretWrite(w, key) {
			return
		}
		injectionMode := req.InjectionMode
		if injectionMode == "" {
			injectionMode = store.InjectionModeAsNeeded
		}
		envVar := &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         req.Value,
			Scope:         scope,
			ScopeID:       scopeID,
			Description:   req.Description,
			Sensitive:     req.Sensitive,
			InjectionMode: injectionMode,
			Secret:        false,
		}
		envVar.CreatedBy = createdBy
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if s.secretBackend != nil {
			if isInternal, err := s.isStoredSecretInternal(ctx, key, scope, scopeID); err == nil && !isInternal {
				_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
			}
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if isSystemOneReservedSecretKey(key) {
			writeErrorFromErr(w, store.ErrNotFound, "")
			return
		}
		if err := s.store.DeleteEnvVar(ctx, key, scope, scopeID); err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				if isInternal, intErr := s.isStoredSecretInternal(ctx, key, scope, scopeID); intErr == nil && !isInternal {
					if secErr := s.secretBackend.Delete(ctx, key, scope, scopeID); secErr == nil {
						w.WriteHeader(http.StatusNoContent)
						return
					}
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if s.secretBackend != nil {
			if isInternal, err := s.isStoredSecretInternal(ctx, key, scope, scopeID); err == nil && !isInternal {
				_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
			}
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) handleProjectEnvVarByKey(w http.ResponseWriter, r *http.Request, projectID, key string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if isWrite {
			Forbidden(w)
			return
		}
		if agentIdent.ProjectID() != projectID {
			Forbidden(w)
			return
		}
		if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
			return
		}
	} else if userIdent, ok := identity.(UserIdentity); ok {
		action := ActionRead
		if isWrite {
			action = ActionUpdate
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, action)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	s.handleScopedEnvVarByKey(w, r, key, store.ScopeProject, projectID)
}

func (s *Server) handleProjectSecrets(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ProjectID() != projectID {
			Forbidden(w)
			return
		}
		if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
			return
		}
		if !checkAgentProjectSecretReadScope(w, r, store.ScopeProject) {
			return
		}
		// Agents only get read access
	} else if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
		if !ok {
			return
		}
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		metas = withoutInternalSecrets(withoutSystemOneReservedSecrets(metas))
		if installationOnly {
			metas = withoutInstallationBlockedSecrets(metas)
		}
		secrets := make([]store.Secret, len(metas))
		for i, m := range metas {
			secrets[i] = metaToStoreSecret(m)
		}
		writeJSON(w, http.StatusOK, ListSecretsResponse{
			Secrets: secrets,
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		})
	default:
		MethodNotAllowed(w, http.MethodGet)
	}
}

// handleScopedSecretByKey handles a secret lifecycle after the caller has
// verified the scope resource and authorized the request.
func (s *Server) handleScopedSecretByKey(w http.ResponseWriter, r *http.Request, key, scope, scopeID string) {
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
		if !ok {
			return
		}
		if isSystemOneReservedSecretKey(key) || (installationOnly && isBlockedInstallationGitHubEnvKey(key)) {
			writeErrorFromErr(w, store.ErrNotFound, "")
			return
		}
		meta, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
		if err == nil && (meta.SecretType == store.SecretTypeInternal || (installationOnly && isBlockedInstallationGitHubEnvKey(meta.Target))) {
			err = store.ErrNotFound
		}
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))

	case http.MethodPut:
		if rejectSystemOneReservedSecretWrite(w, key) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
		var req SetSecretRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Value == "" {
			ValidationError(w, "value is required", nil)
			return
		}
		if req.Encoding != "" && req.Encoding != "base64" && req.Encoding != "raw" {
			ValidationError(w, "encoding must be \"base64\" or \"raw\"", map[string]interface{}{
				"field":   "encoding",
				"value":   req.Encoding,
				"allowed": []string{"base64", "raw"},
			})
			return
		}
		var decoded []byte
		if req.Encoding == "raw" {
			// Caller explicitly opted in to raw text — store the value as-is.
			decoded = []byte(req.Value)
		} else {
			// Default: value must be base64-encoded (matches CLI behaviour).
			var decErr error
			decoded, decErr = base64.StdEncoding.DecodeString(req.Value)
			if decErr != nil {
				BadRequest(w, "value must be base64-encoded")
				return
			}
		}
		secretType := req.Type
		if secretType == "" {
			secretType = store.SecretTypeEnvironment
		}
		switch secretType {
		case store.SecretTypeEnvironment, store.SecretTypeVariable, store.SecretTypeFile:
		default:
			ValidationError(w, "type must be one of: environment, variable, file", map[string]interface{}{"field": "type", "value": secretType})
			return
		}
		target := req.Target
		if target == "" {
			target = key
		}
		if !validateEnvSecretTarget(w, secretType, target) {
			return
		}
		if secretType == store.SecretTypeFile {
			if strings.Contains(target, "..") {
				BadRequest(w, "target path must not contain '..'")
				return
			}
			if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
				ValidationError(w, "file secret target must be an absolute path (or start with ~/)", map[string]interface{}{"field": "target", "value": target})
				return
			}
			if len(decoded) > 64*1024 {
				BadRequest(w, "secret value exceeds 64KB limit")
				return
			}
		}
		if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
			return
		}
		if rejectWebhookEndpointReservedSecretWrite(w, key) {
			return
		}
		input := &secret.SetSecretInput{
			Name:          key,
			Value:         string(decoded),
			SecretType:    secretType,
			Target:        target,
			Scope:         scope,
			ScopeID:       scopeID,
			Description:   req.Description,
			InjectionMode: req.InjectionMode,
		}
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			input.CreatedBy = userIdent.ID()
			input.UpdatedBy = userIdent.ID()
		}
		created, meta, err := s.secretBackend.Set(ctx, input)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		result := metaToStoreSecret(*meta)
		writeJSON(w, http.StatusOK, SetSecretResponse{Secret: &result, Created: created})

	case http.MethodPatch:
		s.patchSecretValidateAndUpdate(w, r, key, scope, scopeID)

	case http.MethodDelete:
		if isSystemOneReservedSecretKey(key) {
			writeErrorFromErr(w, store.ErrNotFound, "")
			return
		}
		if s.refuseInternalSecretMutation(w, ctx, key, scope, scopeID) {
			return
		}
		if err := s.secretBackend.Delete(ctx, key, scope, scopeID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func (s *Server) handleProjectSecretByKey(w http.ResponseWriter, r *http.Request, projectID, key string) {
	ctx := r.Context()

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if isWrite {
			Forbidden(w)
			return
		}
		if agentIdent.ProjectID() != projectID {
			Forbidden(w)
			return
		}
		if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
			return
		}
		if !checkAgentProjectSecretReadScope(w, r, store.ScopeProject) {
			return
		}
	} else if userIdent, ok := identity.(UserIdentity); ok {
		action := ActionRead
		if isWrite {
			action = ActionUpdate
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, action)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	s.handleScopedSecretByKey(w, r, key, store.ScopeProject, projectID)
}

// autoProvideLinkedBy is the ProjectProvider.LinkedBy value recorded by
// autoLinkProviders. brokerProviderHasOwnerConsent treats it as consent,
// because only brokers whose auto-provide setting was authorized are linked
// this way.
const autoProvideLinkedBy = "auto-provide"

// autoProvideBrokersPageSize is the page size autoLinkProviders lists
// auto-provide brokers with; it follows the cursor through every page.
const autoProvideBrokersPageSize = 100

// listAutoProvideBrokers returns every broker with auto_provide enabled,
// newest first, following the list cursor through all pages so a hub with
// more brokers than one page links them all (ptone/scion#1998). A cursor
// the store has already returned is an error rather than an endless loop.
func (s *Server) listAutoProvideBrokers(ctx context.Context) ([]store.RuntimeBroker, error) {
	autoProvideTrue := true
	var all []store.RuntimeBroker
	cursor := ""
	seen := make(map[string]struct{})
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{
			AutoProvide: &autoProvideTrue,
		}, store.ListOptions{Limit: autoProvideBrokersPageSize, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all, nil
		}
		if _, ok := seen[page.NextCursor]; ok {
			return nil, fmt.Errorf("listing auto-provide brokers: pagination returned repeated cursor %q", page.NextCursor)
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
}

// autoLinkProviders links brokers with auto_provide enabled as providers for a project.
// If the project has no default runtime broker, the first auto-provided broker is set as default.
func (s *Server) autoLinkProviders(ctx context.Context, project *store.Project) {
	autoProviders, err := s.listAutoProvideBrokers(ctx)
	if err != nil {
		s.envSecretLog.Warn("Failed to query auto-provide brokers", "project_id", project.ID, "error", err)
		return
	}

	for _, autoBroker := range autoProviders {
		// A flat Runtime Broker never receives an automatic link or
		// default, whatever the experiment state: it is linked only by an
		// explicit link action.
		if autoBroker.IsFlat() {
			continue
		}
		provider := &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   autoBroker.ID,
			BrokerName: autoBroker.Name,
			Status:     autoBroker.Status,
			LinkedBy:   autoProvideLinkedBy,
		}
		if addErr := s.linkProjectProvider(ctx, provider); addErr != nil {
			s.envSecretLog.Warn("Failed to auto-link broker to project",
				"broker", autoBroker.Name, "project_id", project.ID, "error", addErr)
			continue
		}

		// Set first auto-provided broker as default if project has none
		if project.DefaultRuntimeBrokerID == "" {
			project.DefaultRuntimeBrokerID = autoBroker.ID
			if updateErr := s.store.UpdateProject(ctx, project); updateErr != nil {
				s.envSecretLog.Warn("Failed to set default runtime broker",
					"broker", autoBroker.Name, "project_id", project.ID, "error", updateErr)
			}
		}
	}
}

// handleProjectProviders handles provider operations for a project.
// Path: /api/v1/projects/{projectId}/providers[/{brokerId}]
func (s *Server) handleProjectProviders(w http.ResponseWriter, r *http.Request, projectID, subPath string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the project exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if project.ID != agentIdent.ProjectID() {
			NotFound(w, "Project")
			return
		}
		if s.agentStandingForbidden(ctx, w, agentIdent.ID()) {
			return
		}
	}

	// Listing providers is a read of the project; linking or unlinking a
	// provider changes where the project's agents may run, so it is an update.
	action := ActionRead
	switch r.Method {
	case http.MethodPost, http.MethodDelete:
		action = ActionUpdate
	}

	// SECURITY-GATE: CheckAccess — one check here gates the whole providers
	// subtree (list, link, unlink) before dispatching to the handlers below.
	// Linking additionally requires the broker owner's consent
	// (authorizeBrokerProvide in addProjectProvider). Unlinking one broker
	// is also open to that broker's owner (authorizeProviderUnlink), so an
	// owner can withdraw a broker from a project it does not administer.
	if r.Method == http.MethodDelete && subPath != "" {
		if !s.authorizeProviderUnlink(w, r, project, subPath) {
			return
		}
	} else if !s.authorize(w, r, projectResource(project), action) {
		return
	}

	// No subpath - collection endpoint
	if subPath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listProjectProviders(w, r, projectID)
		case http.MethodPost:
			s.addProjectProvider(w, r, projectID)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}

	// subPath is the brokerId - resource endpoint
	brokerID := subPath
	switch r.Method {
	case http.MethodDelete:
		s.removeProjectProvider(w, r, projectID, brokerID)
	default:
		MethodNotAllowed(w, http.MethodDelete)
	}
}

// projectProviderView decorates a project's provider record with read-only
// broker capacity fields for the providers list response (ptone/scion#2161).
// AgentLimit and AgentCount are computed at request time from the same
// quota primitives checkAndReserveBrokerQuota uses to admit or reject agent
// starts (pkg/hub/broker_quota.go) — they report that decision's inputs,
// they do not make one: nothing here creates, modifies, or releases a
// reservation. Unexported: only pkg/hub constructs this view; clients see
// the wire shape through hubclient.ProjectProvider.
type projectProviderView struct {
	store.ProjectProvider
	// AgentLimit is the effective max_agents_per_broker ceiling for this
	// broker. Unset (nil) when the hub has no quota enforcement configured,
	// no max_agents_per_broker definition exists, resolution failed for
	// this provider, or the broker is unlimited. The field is never 0: a
	// non-positive effective limit means unlimited and is omitted. Older
	// clients that don't know this field are unaffected.
	AgentLimit *int64 `json:"agentLimit,omitempty"`
	// AgentCount is the number of active max_agents_per_broker reservations
	// held by agents in a counted phase (see isBrokerQuotaCountedPhase),
	// from any project on this broker. It is exact when the broker has a
	// limit, since every admission and release goes through
	// QuotaService.Reserve/Release synchronously. When the broker is
	// unlimited, Reserve returns before creating a reservation (quota.go),
	// so newly started agents are only reflected here once the periodic
	// broker-quota-reconcile job backfills them; the value may lag by up to
	// that reconcile interval. This is broker-wide and distinct from the
	// project-level agentCount reported elsewhere (e.g. Project.AgentCount).
	// Unset (nil) when the hub has no quota enforcement configured, no
	// max_agents_per_broker definition exists, or resolution failed for
	// this provider — a zero count is reported as 0, not omitted.
	AgentCount *int64 `json:"agentCount,omitempty"`
	// AgentLimitSource reports which precedence step produced AgentLimit
	// (ptone/scion#2061 P2, design.md §5.9): "broker" (a per-broker setting,
	// pkg/hub/brokersettings), "entitlement" (an entitlement binding),
	// "hub_default" (the limit definition's default value), "unlimited"
	// (resolved with no cap), or "not_enforced" (Amendment A1: the P1b
	// enforcement switch, GoogleCloudPlatform/scion#2115, is off — AgentLimit
	// is then informational only: it is still the resolved cap from whichever
	// step would otherwise apply, but Reserve does not reject agent creates
	// against it). Omitted whenever resolution didn't run or failed — the
	// same conditions that leave AgentLimit and AgentCount unset.
	AgentLimitSource string `json:"agentLimitSource,omitempty"`
	// AdminManaged is the broker's stored admin_managed approval flag
	// (ptone/scion#7829 §4.2).
	AdminManaged bool `json:"adminManaged,omitempty"`
	// PolicyEligible reports whether this provider broker is eligible under
	// broker_policy.admin_managed_only (isAdminManagedBroker when the policy
	// is on, or true when the policy is off; ptone/scion#7829 §4.2).
	PolicyEligible bool `json:"policyEligible"`
}

// listProjectProviders returns all providers for a project.
func (s *Server) listProjectProviders(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Looked up once and reused for every provider: the limit definition
	// row is the same for all of them, so this turns what would otherwise
	// be one lookup (and, on failure, one warning) per provider into one
	// for the whole listing.
	limitDef := s.lookupAgentLimitDefinition(ctx)

	views := make([]projectProviderView, len(providers))
	for i, p := range providers {
		views[i] = projectProviderView{ProjectProvider: p}
		views[i].AgentLimit, views[i].AgentCount, views[i].AgentLimitSource = s.resolveBrokerCapacity(ctx, p.BrokerID, limitDef)
		if b, err := s.store.GetRuntimeBroker(ctx, p.BrokerID); err == nil && b != nil {
			views[i].AdminManaged = b.AdminManaged
			views[i].PolicyEligible = s.brokerPolicyEligible(b)
		} else {
			views[i].PolicyEligible = !s.adminManagedBrokersOnly()
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": views,
	})
}

// lookupAgentLimitDefinition fetches the max_agents_per_broker limit
// definition once for reuse across all providers in a single listing
// request. Returns nil when no quota service is configured or when no such
// limit definition exists (store.ErrNotFound), matching how
// QuotaService.Reserve treats a missing definition as "no limit — no
// enforcement" (quota.go). Other lookup errors are logged.
func (s *Server) lookupAgentLimitDefinition(ctx context.Context) *store.LimitDefinition {
	if s.quotaService == nil {
		return nil
	}

	limitDef, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "providers: failed to look up max_agents_per_broker limit definition", "error", err)
		}
		return nil
	}
	return limitDef
}

// resolveBrokerCapacity is a thin wrapper over brokerCapacity
// (broker_capacity.go) — the one read model shared by enforcement and every
// read path (ptone/scion#2061 P2, design.md §5.9, AC-P2-10) — that adapts it
// to the providers listing's pre-existing (agentLimit, agentCount, source)
// field shape (ptone/scion#2161). It mirrors exactly the primitives
// checkAndReserveBrokerQuota uses to admit or reject an agent start
// (pkg/hub/broker_quota.go): the same limit name, subject, and scope
// (store.QuotaScopeBroker, scoped to the broker itself). This is a read: it
// never creates, updates, or releases a reservation.
//
// limitDef is looked up once by the caller (lookupAgentLimitDefinition) and
// shared across every provider in a listing.
//
// The listing's pre-existing contract is all-or-nothing per provider: if
// either half of BrokerCapacity couldn't be resolved, both agentLimit and
// agentCount come back nil (never "an agentLimit with no matching count to
// compare it against") — so that a failure for one provider never fails the
// whole providers listing (per ptone/scion#2161), while also never reporting
// half a picture for that provider. Count is nil exactly when either the
// limit or the count resolution failed, or nothing is configured at all
// (brokerCapacity skips counting when there's no limitDef/quotaService) —
// all three collapse to the listing's existing "leave both unset" case here.
// Failures are logged inside brokerCapacity, not duplicated here.
func (s *Server) resolveBrokerCapacity(ctx context.Context, brokerID string, limitDef *store.LimitDefinition) (agentLimit, agentCount *int64, source string) {
	bc := s.brokerCapacity(ctx, brokerID, limitDef)
	if bc.Count == nil {
		return nil, nil, ""
	}
	return bc.Limit, bc.Count, bc.Source
}

// addProjectProvider adds a broker as a provider to a project.
func (s *Server) addProjectProvider(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	var req AddProviderRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.BrokerID == "" {
		ValidationError(w, "brokerId is required", nil)
		return
	}

	// Verify broker exists
	broker, err := s.store.GetRuntimeBroker(ctx, req.BrokerID)
	if err != nil {
		if err == store.ErrNotFound {
			ValidationError(w, "brokerId not found", map[string]interface{}{
				"field":    "brokerId",
				"brokerId": req.BrokerID,
			})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: broker-side consent — the caller must hold broker.update
	// on this broker (its owner or a super-admin) in addition to the
	// project.update gate in handleProjectProviders. Checked before any write.
	if !s.authorizeBrokerProvide(w, r, broker) {
		return
	}

	// Get the user who is performing this action
	var linkedBy string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		linkedBy = user.ID()
	}

	// Validate LocalPath before persisting — fail fast before touching the DB.
	// LocalPath names a directory on the broker's host. Every broker's path
	// passes the same checks (checkProviderLocalPath). Only the embedded
	// broker shares the hub's filesystem, so only for it does the hub check
	// that the directory exists and initialize its .scion directory below.
	// For any other broker the path is validated and stored.
	//
	// The global-directory check needs the project; a lookup failure fails
	// the request rather than skipping the check.
	target, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	var cleanPath string
	embedded := s.isEmbeddedBroker(broker.ID)
	if req.LocalPath != "" {
		cleanPath, err = checkProviderLocalPath("localPath", target.Name, target.Slug, req.LocalPath)
		if err != nil {
			ValidationError(w, err.Error(), map[string]interface{}{"field": "localPath"})
			return
		}
		if embedded {
			info, err := os.Stat(cleanPath)
			if err != nil || !info.IsDir() {
				ValidationError(w, "localPath must be an existing directory", nil)
				return
			}
		}
	}

	// A request without a path keeps the path stored for an existing
	// provider when checkProviderLocalPath accepts it for this project, and
	// clears it otherwise. A broker that is not yet a provider gets no path.
	// A store error reading the provider fails the request before any write.
	localPath := cleanPath
	if localPath == "" {
		localPath, err = s.registerProviderLocalPath(ctx, target, broker.ID, "", false)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Create provider record
	provider := &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  localPath,
		Status:     broker.Status,
		LinkedBy:   linkedBy,
	}

	if err := s.linkProjectProvider(ctx, provider); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// For linked projects (local directory) on the embedded broker, initialize
	// the .scion directory so agents and templates directories exist before
	// the first agent starts. Other brokers manage their own filesystem, and
	// the broker's global directory is left to global init.
	if cleanPath != "" && embedded {
		if scionDir, ok := linkedProjectInitDir(cleanPath); ok {
			if err := initLinkedProjectDir(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
				slog.Warn("failed to initialize .scion in linked project",
					"project_id", projectID, "localPath", cleanPath, "error", err.Error())
			}
		}
	}

	// Get the project to check if we should set default runtime broker
	project, err := s.store.GetProject(ctx, projectID)
	if err == nil && project.DefaultRuntimeBrokerID == "" {
		project.DefaultRuntimeBrokerID = broker.ID
		_ = s.store.UpdateProject(ctx, project)
	}

	// Log the link event
	LogLinkEvent(ctx, s.auditLogger, broker.ID, broker.Name, projectID, linkedBy, s.clientIP(r), brokerAuditCredentialDetails(ctx))

	writeJSON(w, http.StatusCreated, AddProviderResponse{
		Provider: provider,
	})
}

// authorizeProviderUnlink decides DELETE /api/v1/projects/{id}/providers/{brokerId}:
// the request credential must be a user credential admitted for broker
// association (an interactive session, a dev credential or a user access
// token, see brokerUserCredentialKindAdmitted); a broker acting on a user's
// behalf, and any other non-user credential, is not admitted by either arm.
// The caller must then hold project.update on the project, or broker.update
// on the named broker (its owner or a super-admin, see
// brokerProvideDecision), which withdraws the owner's consent to the
// association. On denial it logs and writes the project.update 403, and
// returns false.
func (s *Server) authorizeProviderUnlink(w http.ResponseWriter, r *http.Request, project *store.Project, brokerID string) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if !brokerUserCredentialKindAdmitted(ctx, identity, true) {
		logAuthzDenial(r, identity, projectResource(project), ActionUpdate, "credential kind not admitted for broker association")
		writeForbiddenStructuredDenial(w, "", "project", ActionUpdate, "")
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, projectResource(project), ActionUpdate)
	if decision.Allowed {
		return true
	}
	// Unlinking a provider is always allowed under broker_policy.admin_managed_only,
	// so check broker.update directly here (identity and credential kind were
	// already verified above) rather than going through brokerProvideDecision.
	if broker, err := s.store.GetRuntimeBroker(ctx, brokerID); err == nil && broker != nil {
		if s.authzService.CheckAccess(ctx, identity, brokerResource(broker), ActionUpdate).Allowed {
			return true
		}
	}
	logAuthzDenial(r, identity, projectResource(project), ActionUpdate, decision.Reason)
	writeForbiddenStructuredDenial(w, "", "project", ActionUpdate, decision.DeniedBy)
	return false
}

// removeProjectProvider removes a broker from a project's providers.
func (s *Server) removeProjectProvider(w http.ResponseWriter, r *http.Request, projectID, brokerID string) {
	ctx := r.Context()

	// Get the user who is performing this action for audit logging
	var actorID string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		actorID = user.ID()
	}

	if err := s.store.RemoveProjectProvider(ctx, projectID, brokerID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Log the unlink event
	LogUnlinkEvent(ctx, s.auditLogger, brokerID, projectID, actorID, s.clientIP(r), brokerAuditCredentialDetails(ctx))

	w.WriteHeader(http.StatusNoContent)
}

// authorizeBrokerMaterial decides access to a runtime broker's environment
// variables and secrets. It writes the response and returns false when the
// caller is refused.
//   - The broker itself reads and writes its own material.
//   - A test identity with an admin role is refused (test-admin policy).
//   - A write needs an interactive session or dev credential (GOV_PENDING)
//     and broker.update on the broker.
//   - A read needs broker.read on the broker and, beyond it, either the
//     owner relationship (the caller registered the broker) or
//     broker.update. broker.read alone, which every hub member holds for
//     the broker directory, does not reach a broker's material. A user
//     access token needs the broker:read selector on a hub boundary for the
//     first check; it holds no broker.update, so it reads only the material
//     of brokers its user owns.
func (s *Server) authorizeBrokerMaterial(w http.ResponseWriter, r *http.Request, broker *store.RuntimeBroker, isWrite bool) bool {
	ctx := r.Context()
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == broker.ID {
		return true
	}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return false
	}
	// Broker material is not available to test identities with admin
	// roles (test-admin policy), matching the material their agents may
	// receive (testAdminRootScopes).
	if s.testAdminPolicyRefuses(ctx, "handlers_env_secrets.go:authorizeBrokerMaterial") {
		Forbidden(w)
		return false
	}
	resource := brokerResource(broker)
	if isWrite {
		if _, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonGovernancePending); !ok {
			return false
		}
		if decision := s.authzService.CheckAccess(ctx, user, resource, ActionUpdate); !decision.Allowed {
			logAuthzDenial(r, user, resource, ActionUpdate, decision.Reason)
			Forbidden(w)
			return false
		}
		return true
	}
	if decision := s.authzService.CheckAccess(ctx, user, resource, ActionRead); !decision.Allowed {
		logAuthzDenial(r, user, resource, ActionRead, decision.Reason)
		Forbidden(w)
		return false
	}
	if broker.CreatedBy != "" && broker.CreatedBy == user.ID() {
		return true
	}
	if decision := s.authzService.CheckAccess(ctx, user, resource, ActionUpdate); !decision.Allowed {
		logAuthzDenial(r, user, resource, ActionUpdate, "broker material reads need the owner relationship or broker.update: "+decision.Reason)
		Forbidden(w)
		return false
	}
	return true
}

func (s *Server) handleBrokerEnvVars(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeBrokerMaterial(w, r, broker, false) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
		if !ok {
			return
		}
		filter := store.EnvVarFilter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		}
		envVars, err := s.store.ListEnvVars(ctx, filter)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		envVars = s.mergeEnvironmentSecrets(ctx, envVars, filter,
			"failed to list environment secrets for broker env var merge", installationOnly)
		if installationOnly {
			envVars = withoutInstallationBlockedEnvVars(envVars)
		}
		for i := range envVars {
			if envVars[i].Sensitive {
				envVars[i].Value = "********"
			}
		}
		writeJSON(w, http.StatusOK, ListEnvVarsResponse{
			EnvVars: envVars,
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
	default:
		MethodNotAllowed(w, http.MethodGet)
	}
}

func (s *Server) handleBrokerEnvVarByKey(w http.ResponseWriter, r *http.Request, brokerID, key string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	if !s.authorizeBrokerMaterial(w, r, broker, isWrite) {
		return
	}

	s.handleScopedEnvVarByKey(w, r, key, store.ScopeRuntimeBroker, brokerID)
}

func (s *Server) handleBrokerSecrets(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeBrokerMaterial(w, r, broker, false) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		installationOnly, ok := s.callerAgentUsesInstallationGitHubToken(w, ctx)
		if !ok {
			return
		}
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		metas = withoutInternalSecrets(withoutSystemOneReservedSecrets(metas))
		if installationOnly {
			metas = withoutInstallationBlockedSecrets(metas)
		}
		secrets := make([]store.Secret, len(metas))
		for i, m := range metas {
			secrets[i] = metaToStoreSecret(m)
		}
		writeJSON(w, http.StatusOK, ListSecretsResponse{
			Secrets: secrets,
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
	default:
		MethodNotAllowed(w, http.MethodGet)
	}
}

func (s *Server) handleBrokerSecretByKey(w http.ResponseWriter, r *http.Request, brokerID, key string) {
	ctx := r.Context()

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete
	if !s.authorizeBrokerMaterial(w, r, broker, isWrite) {
		return
	}

	s.handleScopedSecretByKey(w, r, key, store.ScopeRuntimeBroker, brokerID)
}
