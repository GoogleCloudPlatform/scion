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
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
// any client-supplied value). No CheckAccess call needed — identity enforcement
// is the access control.
//
// For project scope: verifies the project exists, then checks authorization. Users
// must pass CheckAccess (with owner bypass). Agents get read-only access to
// their own project only.
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
		_, err := s.store.GetRuntimeBroker(ctx, clientScopeID)
		if err != nil {
			if err == store.ErrNotFound {
				NotFound(w, "RuntimeBroker")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return "", false
		}
		// Broker self-access
		if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil {
			if brokerIdent.BrokerID() == clientScopeID {
				return clientScopeID, true
			}
		}
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   clientScopeID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return "", false
			}
			return clientScopeID, true
		}
		Forbidden(w)
		return "", false

	case store.ScopeHub:
		// Hub scope: admin users can read and write; agents can read only.
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return "", false
		}
		if _, ok := identity.(AgentIdentity); ok {
			if isWrite {
				Forbidden(w)
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
		if userIdent.Role() != store.UserRoleAdmin {
			Forbidden(w)
			return "", false
		}
		return s.hubID, true

	default:
		BadRequest(w, "invalid scope: "+scope)
		return "", false
	}
}

func (s *Server) handleEnvVars(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEnvVars(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listEnvVars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
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

	// Merge environment-type secrets into the env var list
	if s.secretBackend != nil {
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   scope,
			ScopeID: scopeID,
			Type:    "environment",
		})
		if err != nil {
			s.envSecretLog.Warn("failed to list environment secrets for env var merge", "error", err)
		} else {
			// Build set of secret keys for deduplication
			secretKeys := make(map[string]struct{}, len(metas))
			for _, m := range metas {
				secretKeys[m.Name] = struct{}{}
				envVars = append(envVars, secretMetaToEnvVar(m))
			}
			// Remove stale plain env var records that are shadowed by secrets
			if len(secretKeys) > 0 {
				deduped := make([]store.EnvVar, 0, len(envVars))
				for _, ev := range envVars {
					if _, isShadowed := secretKeys[ev.Key]; isShadowed && !ev.Secret {
						continue
					}
					deduped = append(deduped, ev)
				}
				envVars = deduped
			}
		}
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
		MethodNotAllowed(w)
	}
}

func (s *Server) getEnvVar(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}

	envVar, err := s.store.GetEnvVar(ctx, key, scope, scopeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
			// Fallback: check if this key exists as an environment secret
			meta, metaErr := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
			if metaErr == nil && meta.SecretType == "environment" {
				ev := secretMetaToEnvVar(*meta)
				writeJSON(w, http.StatusOK, &ev)
				return
			}
		}
		writeErrorFromErr(w, err, "")
		return
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

	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, req.ScopeID, true)
	if !ok {
		return
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

	// Clean up any existing secret with same key (demotion from secret to plain)
	if s.secretBackend != nil {
		_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
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

	if err := s.store.DeleteEnvVar(ctx, key, scope, scopeID); err != nil {
		if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
			// Fallback: try deleting from the secret backend
			if secErr := s.secretBackend.Delete(ctx, key, scope, scopeID); secErr == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Also clean up any secret with the same key
	if s.secretBackend != nil {
		_ = s.secretBackend.Delete(ctx, key, scope, scopeID)
	}

	w.WriteHeader(http.StatusNoContent)
}

type ListSecretsResponse struct {
	Secrets []store.Secret `json:"secrets"`
	Scope   string         `json:"scope"`
	ScopeID string         `json:"scopeId"`
}

type SetSecretRequest struct {
	Value         string `json:"value"`
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

// progenyPolicyName returns the canonical policy name for a progeny secret policy.
func progenyPolicyName(secretID string) string {
	return "progeny-secret-access:" + secretID
}

// ensureProgenyPolicy creates or deletes the implicit progeny policy for a secret
// based on the allowProgeny flag. It is called after a secret is created or updated.
func (s *Server) ensureProgenyPolicy(ctx context.Context, meta *secret.SecretMeta) {
	if meta.Scope != store.ScopeUser {
		return
	}

	policyName := progenyPolicyName(meta.ID)

	if meta.AllowProgeny {
		// Check if policy already exists
		existing, err := s.store.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
		if err != nil {
			s.envSecretLog.Warn("failed to check for existing progeny policy", "secret", meta.Name, "error", err)
			return
		}
		if existing.TotalCount > 0 {
			return // Policy already exists
		}

		// Create implicit policy
		policy := &store.Policy{
			ID:           api.NewUUID(),
			Name:         policyName,
			Description:  "Implicit policy granting progeny agents read access to secret " + meta.Name,
			ScopeType:    store.PolicyScopeResource,
			ScopeID:      meta.ID,
			ResourceType: "secret",
			ResourceID:   meta.ID,
			Actions:      []string{"read"},
			Effect:       store.PolicyEffectAllow,
			Conditions: &store.PolicyConditions{
				DelegatedFrom: &store.DelegatedFromCondition{
					PrincipalType: "user",
					PrincipalID:   meta.CreatedBy,
				},
			},
			Labels: map[string]string{
				"scion.dev/managed-by":   "progeny-secret-access",
				"scion.dev/secret-key":   meta.Name,
				"scion.dev/secret-id":    meta.ID,
				"scion.dev/secret-scope": meta.Scope,
			},
			CreatedBy: meta.CreatedBy,
		}
		if err := s.store.CreatePolicy(ctx, policy); err != nil {
			s.envSecretLog.Warn("failed to create progeny policy", "secret", meta.Name, "error", err)
		}
	} else {
		// Delete implicit policy if it exists
		s.deleteProgenyPolicy(ctx, meta.ID)
	}
}

// deleteProgenyPolicy removes the implicit progeny policy for a secret by its ID.
func (s *Server) deleteProgenyPolicy(ctx context.Context, secretID string) {
	policyName := progenyPolicyName(secretID)
	existing, err := s.store.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	if err != nil {
		s.envSecretLog.Warn("failed to look up progeny policy for deletion", "secretID", secretID, "error", err)
		return
	}
	for _, p := range existing.Items {
		if err := s.store.DeletePolicy(ctx, p.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.envSecretLog.Warn("failed to delete progeny policy", "policyID", p.ID, "error", err)
		}
	}
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSecrets(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
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
	case http.MethodDelete:
		s.deleteSecret(w, r, key)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	query := r.URL.Query()

	scope := query.Get("scope")
	if scope == "" {
		scope = store.ScopeUser
	}

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, query.Get("scopeId"), false)
	if !ok {
		return
	}

	meta, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))
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

	decoded, err := base64.StdEncoding.DecodeString(req.Value)
	if err != nil {
		BadRequest(w, "value must be base64-encoded")
		return
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

	scopeID, ok := s.resolveEnvSecretAccess(w, r, scope, req.ScopeID, true)
	if !ok {
		return
	}

	// allowProgeny is only valid on user-scoped secrets
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

	created, meta, err := s.secretBackend.Set(ctx, input)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Manage implicit progeny policy lifecycle
	s.ensureProgenyPolicy(ctx, meta)

	result := metaToStoreSecret(*meta)
	writeJSON(w, http.StatusOK, SetSecretResponse{
		Secret:  &result,
		Created: created,
	})
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, key string) {
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

	// Fetch secret metadata before deletion for policy cleanup
	if scope == store.ScopeUser {
		if meta, err := s.secretBackend.GetMeta(ctx, key, scope, scopeID); err == nil && meta.AllowProgeny {
			defer s.deleteProgenyPolicy(ctx, meta.ID)
		}
	}

	if err := s.secretBackend.Delete(ctx, key, scope, scopeID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AgentSetSecretRequest is the request body for agent-initiated secret creation.
type AgentSetSecretRequest struct {
	Value  string `json:"value"`            // Base64-encoded secret value
	Type   string `json:"type,omitempty"`   // environment (default), variable, file
	Target string `json:"target,omitempty"` // Injection target path
	Force  bool   `json:"force,omitempty"`  // Overwrite existing secret
}

// AgentSetSecretResponse is returned on successful agent secret creation.
type AgentSetSecretResponse struct {
	Key     string `json:"key"`
	Scope   string `json:"scope"`
	ScopeID string `json:"scopeId"`
}

// handleAgentSecrets handles PUT /api/v1/agents/{agentID}/secrets/{key}.
// Only agents may call this endpoint. The secret is always scoped to the
// agent's project (derived from the JWT).
func (s *Server) handleAgentSecrets(w http.ResponseWriter, r *http.Request, agentID, subPath string) {
	key := strings.TrimPrefix(subPath, "/")
	if key == "" {
		BadRequest(w, "Secret key is required in the URL path")
		return
	}

	if s.secretBackend == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "secret storage requires a configured secrets backend",
		})
		return
	}

	if r.Method != http.MethodPut {
		MethodNotAllowed(w)
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

	ctx := r.Context()

	// Agent-only: require agent identity from JWT.
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "This endpoint requires agent authentication", nil)
		return
	}

	// The agentID in the URL path must match the JWT subject.
	if agentIdent.ID() != agentID {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agent token does not match the agent ID in the URL", nil)
		return
	}

	// Extract project ID from agent token claims.
	projectID := agentIdent.ProjectID()
	if projectID == "" {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agent token lacks project context", nil)
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

	decoded, err := base64.StdEncoding.DecodeString(req.Value)
	if err != nil {
		BadRequest(w, "value must be base64-encoded")
		return
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

	// Validate file-specific constraints.
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

	// Check for existing secret when force is not set.
	// Note: the backend's UpsertSecret has the same check-then-write pattern
	// internally, so this is consistent with the existing TOCTOU window.
	if !req.Force {
		_, err := s.secretBackend.GetMeta(ctx, key, store.ScopeProject, projectID)
		if err == nil {
			Conflict(w, fmt.Sprintf("Secret %q already exists at project scope. Use force=true to overwrite.", key))
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	input := &secret.SetSecretInput{
		Name:       key,
		Value:      string(decoded),
		SecretType: secretType,
		Target:     target,
		Scope:      store.ScopeProject,
		ScopeID:    projectID,
		CreatedBy:  fmt.Sprintf("agent:%s", agentID),
		UpdatedBy:  fmt.Sprintf("agent:%s", agentID),
	}

	created, _, err := s.secretBackend.Set(ctx, input)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if created {
		writeJSON(w, http.StatusCreated, AgentSetSecretResponse{
			Key:     key,
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		})
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleProjectEnvVars(w http.ResponseWriter, r *http.Request, projectID string) {
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
		envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Merge environment-type secrets
		if s.secretBackend != nil {
			metas, err := s.secretBackend.List(ctx, secret.Filter{
				Scope:   store.ScopeProject,
				ScopeID: projectID,
				Type:    "environment",
			})
			if err != nil {
				s.envSecretLog.Warn("failed to list environment secrets for project env var merge", "error", err)
			} else {
				secretKeys := make(map[string]struct{}, len(metas))
				for _, m := range metas {
					secretKeys[m.Name] = struct{}{}
					envVars = append(envVars, secretMetaToEnvVar(m))
				}
				if len(secretKeys) > 0 {
					deduped := make([]store.EnvVar, 0, len(envVars))
					for _, ev := range envVars {
						if _, isShadowed := secretKeys[ev.Key]; isShadowed && !ev.Secret {
							continue
						}
						deduped = append(deduped, ev)
					}
					envVars = deduped
				}
			}
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
		MethodNotAllowed(w)
	}
}

func (s *Server) handleProjectEnvVarByKey(w http.ResponseWriter, r *http.Request, projectID, key string) {
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

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeProject, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				meta, metaErr := s.secretBackend.GetMeta(ctx, key, store.ScopeProject, projectID)
				if metaErr == nil && meta.SecretType == "environment" {
					ev := secretMetaToEnvVar(*meta)
					writeJSON(w, http.StatusOK, &ev)
					return
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, envVar)

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

		var createdBy string
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
		}

		// Secret promotion
		if req.Secret {
			if s.secretBackend == nil {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			input := &secret.SetSecretInput{
				Name:          key,
				Value:         req.Value,
				SecretType:    "environment",
				Target:        key,
				Scope:         store.ScopeProject,
				ScopeID:       projectID,
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
			_ = s.store.DeleteEnvVar(ctx, key, store.ScopeProject, projectID)
			syntheticEnvVar := secretMetaToEnvVar(*meta)
			writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: &syntheticEnvVar, Created: created})
			return
		}

		// Plain env var write
		projectInjectionMode := req.InjectionMode
		if projectInjectionMode == "" {
			projectInjectionMode = store.InjectionModeAsNeeded
		}
		envVar := &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         req.Value,
			Scope:         store.ScopeProject,
			ScopeID:       projectID,
			Description:   req.Description,
			Sensitive:     req.Sensitive,
			InjectionMode: projectInjectionMode,
			Secret:        false,
		}
		envVar.CreatedBy = createdBy
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Demotion cleanup
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeProject, projectID)
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeProject, projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				if secErr := s.secretBackend.Delete(ctx, key, store.ScopeProject, projectID); secErr == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeProject, projectID)
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
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
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   store.ScopeProject,
			ScopeID: projectID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
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
		MethodNotAllowed(w)
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

	switch r.Method {
	case http.MethodGet:
		meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeProject, projectID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))

	case http.MethodPut:
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
		decoded, err := base64.StdEncoding.DecodeString(req.Value)
		if err != nil {
			BadRequest(w, "value must be base64-encoded")
			return
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
		input := &secret.SetSecretInput{
			Name:          key,
			Value:         string(decoded),
			SecretType:    secretType,
			Target:        target,
			Scope:         store.ScopeProject,
			ScopeID:       projectID,
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

	case http.MethodDelete:
		if err := s.secretBackend.Delete(ctx, key, store.ScopeProject, projectID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// autoLinkProviders links brokers with auto_provide enabled as providers for a project.
// If the project has no default runtime broker, the first auto-provided broker is set as default.
func (s *Server) autoLinkProviders(ctx context.Context, project *store.Project) {
	autoProvideTrue := true
	autoProviders, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{
		AutoProvide: &autoProvideTrue,
	}, store.ListOptions{})
	if err != nil {
		s.envSecretLog.Warn("Failed to query auto-provide brokers", "project_id", project.ID, "error", err)
		return
	}

	for _, autoBroker := range autoProviders.Items {
		provider := &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   autoBroker.ID,
			BrokerName: autoBroker.Name,
			Status:     autoBroker.Status,
			LinkedBy:   "auto-provide",
		}
		if addErr := s.store.AddProjectProvider(ctx, provider); addErr != nil {
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

	// Verify project exists
	_, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
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
			MethodNotAllowed(w)
		}
		return
	}

	// subPath is the brokerId - resource endpoint
	brokerID := subPath
	switch r.Method {
	case http.MethodDelete:
		s.removeProjectProvider(w, r, projectID, brokerID)
	default:
		MethodNotAllowed(w)
	}
}

// listProjectProviders returns all providers for a project.
func (s *Server) listProjectProviders(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": providers,
	})
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

	// Get the user who is performing this action
	var linkedBy string
	if user := GetUserIdentityFromContext(ctx); user != nil {
		linkedBy = user.ID()
	}

	// Create provider record
	provider := &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  req.LocalPath,
		Status:     broker.Status,
		LinkedBy:   linkedBy,
	}

	if err := s.store.AddProjectProvider(ctx, provider); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Get the project to check if we should set default runtime broker
	project, err := s.store.GetProject(ctx, projectID)
	if err == nil && project.DefaultRuntimeBrokerID == "" {
		project.DefaultRuntimeBrokerID = broker.ID
		_ = s.store.UpdateProject(ctx, project)
	}

	// Log the link event
	LogLinkEvent(ctx, s.auditLogger, broker.ID, broker.Name, projectID, linkedBy, getClientIP(r))

	writeJSON(w, http.StatusCreated, AddProviderResponse{
		Provider: provider,
	})
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
	LogUnlinkEvent(ctx, s.auditLogger, brokerID, projectID, actorID, getClientIP(r))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBrokerEnvVars(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access: broker self-access or user CheckAccess
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own env vars — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Merge environment-type secrets
		if s.secretBackend != nil {
			metas, err := s.secretBackend.List(ctx, secret.Filter{
				Scope:   store.ScopeRuntimeBroker,
				ScopeID: brokerID,
				Type:    "environment",
			})
			if err != nil {
				s.envSecretLog.Warn("failed to list environment secrets for broker env var merge", "error", err)
			} else {
				secretKeys := make(map[string]struct{}, len(metas))
				for _, m := range metas {
					secretKeys[m.Name] = struct{}{}
					envVars = append(envVars, secretMetaToEnvVar(m))
				}
				if len(secretKeys) > 0 {
					deduped := make([]store.EnvVar, 0, len(envVars))
					for _, ev := range envVars {
						if _, isShadowed := secretKeys[ev.Key]; isShadowed && !ev.Secret {
							continue
						}
						deduped = append(deduped, ev)
					}
					envVars = deduped
				}
			}
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
		MethodNotAllowed(w)
	}
}

func (s *Server) handleBrokerEnvVarByKey(w http.ResponseWriter, r *http.Request, brokerID, key string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access: broker self-access or user CheckAccess
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own env vars — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		envVar, err := s.store.GetEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				meta, metaErr := s.secretBackend.GetMeta(ctx, key, store.ScopeRuntimeBroker, brokerID)
				if metaErr == nil && meta.SecretType == "environment" {
					ev := secretMetaToEnvVar(*meta)
					writeJSON(w, http.StatusOK, &ev)
					return
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, envVar)

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

		var createdBy string
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
		}

		// Secret promotion
		if req.Secret {
			if s.secretBackend == nil {
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "secret storage requires a configured secrets backend",
				})
				return
			}
			input := &secret.SetSecretInput{
				Name:          key,
				Value:         req.Value,
				SecretType:    "environment",
				Target:        key,
				Scope:         store.ScopeRuntimeBroker,
				ScopeID:       brokerID,
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
			_ = s.store.DeleteEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID)
			syntheticEnvVar := secretMetaToEnvVar(*meta)
			writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: &syntheticEnvVar, Created: created})
			return
		}

		// Plain env var write
		brokerInjectionMode := req.InjectionMode
		if brokerInjectionMode == "" {
			brokerInjectionMode = store.InjectionModeAsNeeded
		}
		envVar := &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         req.Value,
			Scope:         store.ScopeRuntimeBroker,
			ScopeID:       brokerID,
			Description:   req.Description,
			Sensitive:     req.Sensitive,
			InjectionMode: brokerInjectionMode,
			Secret:        false,
		}
		envVar.CreatedBy = createdBy
		created, err := s.store.UpsertEnvVar(ctx, envVar)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Demotion cleanup
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID)
		}
		if envVar.Sensitive {
			envVar.Value = "********"
		}
		writeJSON(w, http.StatusOK, SetEnvVarResponse{EnvVar: envVar, Created: created})

	case http.MethodDelete:
		if err := s.store.DeleteEnvVar(ctx, key, store.ScopeRuntimeBroker, brokerID); err != nil {
			if errors.Is(err, store.ErrNotFound) && s.secretBackend != nil {
				if secErr := s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID); secErr == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if s.secretBackend != nil {
			_ = s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID)
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleBrokerSecrets(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access: broker self-access or user CheckAccess
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own secrets — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		metas, err := s.secretBackend.List(ctx, secret.Filter{
			Scope:   store.ScopeRuntimeBroker,
			ScopeID: brokerID,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
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
		MethodNotAllowed(w)
	}
}

func (s *Server) handleBrokerSecretByKey(w http.ResponseWriter, r *http.Request, brokerID, key string) {
	ctx := r.Context()

	// Verify broker exists
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "RuntimeBroker")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize access: broker self-access or user CheckAccess
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil && brokerIdent.BrokerID() == brokerID {
		// Broker accessing its own secrets — allowed
	} else {
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			Unauthorized(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			action := ActionRead
			if isWrite {
				action = ActionUpdate
			}
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "runtime_broker",
				ID:   brokerID,
			}, action)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeRuntimeBroker, brokerID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, metaToStoreSecret(*meta))

	case http.MethodPut:
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
		decoded, err := base64.StdEncoding.DecodeString(req.Value)
		if err != nil {
			BadRequest(w, "value must be base64-encoded")
			return
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
		input := &secret.SetSecretInput{
			Name:          key,
			Value:         string(decoded),
			SecretType:    secretType,
			Target:        target,
			Scope:         store.ScopeRuntimeBroker,
			ScopeID:       brokerID,
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

	case http.MethodDelete:
		if err := s.secretBackend.Delete(ctx, key, store.ScopeRuntimeBroker, brokerID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}
