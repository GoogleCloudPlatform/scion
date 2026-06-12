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
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CreateSkillRegistryRequest is the request body for creating a skill registry.
type CreateSkillRegistryRequest struct {
	Name        string `json:"name"`
	Endpoint    string `json:"endpoint"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	TrustLevel  string `json:"trustLevel,omitempty"`
	AuthToken   string `json:"authToken,omitempty"`
	ResolvePath string `json:"resolvePath,omitempty"`
}

// UpdateSkillRegistryRequest is the request body for updating a skill registry.
type UpdateSkillRegistryRequest struct {
	Endpoint    string `json:"endpoint,omitempty"`
	Description string `json:"description,omitempty"`
	TrustLevel  string `json:"trustLevel,omitempty"`
	AuthToken   string `json:"authToken,omitempty"`
	ResolvePath string `json:"resolvePath,omitempty"`
	Status      string `json:"status,omitempty"`
}

// PinSkillHashRequest is the request body for pinning a skill hash.
type PinSkillHashRequest struct {
	URI  string `json:"uri"`
	Hash string `json:"hash"`
}

var registryNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func (s *Server) handleSkillRegistries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSkillRegistries(w, r)
	case http.MethodPost:
		s.createSkillRegistry(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleSkillRegistryByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/skill-registries/")
	if id == "" {
		NotFound(w, "Skill Registry")
		return
	}

	if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
		if parts[1] == "pin" {
			s.pinSkillHash(w, r, parts[0])
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		s.getSkillRegistry(w, r, id)
	case http.MethodPut:
		s.updateSkillRegistry(w, r, id)
	case http.MethodDelete:
		s.deleteSkillRegistry(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (UserIdentity, bool) {
	identity := GetUserIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return nil, false
	}
	if identity.Role() != store.UserRoleAdmin {
		Forbidden(w)
		return nil, false
	}
	return identity, true
}

func (s *Server) listSkillRegistries(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	result, err := s.store.ListSkillRegistries(r.Context(), store.ListOptions{})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createSkillRegistry(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	var req CreateSkillRegistryRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if !registryNameRegex.MatchString(req.Name) {
		ValidationError(w, "name must be lowercase alphanumeric with hyphens and dots", nil)
		return
	}
	if len(req.Name) > 64 {
		ValidationError(w, "name must be at most 64 characters", nil)
		return
	}

	if req.Endpoint == "" {
		ValidationError(w, "endpoint is required", nil)
		return
	}
	u, err := url.Parse(req.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		ValidationError(w, "endpoint must be a valid HTTPS URL", nil)
		return
	}

	if req.TrustLevel == "" {
		req.TrustLevel = store.SkillRegistryTrustPinned
	}
	if req.TrustLevel != store.SkillRegistryTrustTrusted && req.TrustLevel != store.SkillRegistryTrustPinned {
		ValidationError(w, "trustLevel must be 'trusted' or 'pinned'", nil)
		return
	}

	if req.Type == "" {
		req.Type = store.SkillRegistryTypeHub
	}
	if req.Type != store.SkillRegistryTypeHub && req.Type != store.SkillRegistryTypeGCP {
		ValidationError(w, "type must be 'hub' or 'gcp'", nil)
		return
	}

	resolvePath := req.ResolvePath
	if resolvePath == "" {
		resolvePath = "/api/v1/skills/resolve"
	}

	registry := &store.SkillRegistry{
		Name:        req.Name,
		Endpoint:    req.Endpoint,
		Description: req.Description,
		Type:        req.Type,
		TrustLevel:  req.TrustLevel,
		AuthToken:   req.AuthToken,
		ResolvePath: resolvePath,
		Status:      store.SkillRegistryStatusActive,
		CreatedBy:   identity.ID(),
	}

	if err := s.store.CreateSkillRegistry(r.Context(), registry); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, registry)
}

func (s *Server) getSkillRegistry(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	registry, err := s.store.GetSkillRegistry(r.Context(), id)
	if err != nil {
		// Try by name
		registry, err = s.store.GetSkillRegistryByName(r.Context(), id)
		if err != nil {
			NotFound(w, "Skill Registry")
			return
		}
	}

	writeJSON(w, http.StatusOK, registry)
}

func (s *Server) updateSkillRegistry(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	ctx := r.Context()
	registry, err := s.store.GetSkillRegistry(ctx, id)
	if err != nil {
		registry, err = s.store.GetSkillRegistryByName(ctx, id)
		if err != nil {
			NotFound(w, "Skill Registry")
			return
		}
	}

	var req UpdateSkillRegistryRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Endpoint != "" {
		u, err := url.Parse(req.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			ValidationError(w, "endpoint must be a valid HTTPS URL", nil)
			return
		}
		registry.Endpoint = req.Endpoint
	}
	if req.Description != "" {
		registry.Description = req.Description
	}
	if req.TrustLevel != "" {
		if req.TrustLevel != store.SkillRegistryTrustTrusted && req.TrustLevel != store.SkillRegistryTrustPinned {
			ValidationError(w, "trustLevel must be 'trusted' or 'pinned'", nil)
			return
		}
		registry.TrustLevel = req.TrustLevel
	}
	if req.AuthToken != "" {
		registry.AuthToken = req.AuthToken
	}
	if req.ResolvePath != "" {
		registry.ResolvePath = req.ResolvePath
	}
	if req.Status != "" {
		if req.Status != store.SkillRegistryStatusActive && req.Status != store.SkillRegistryStatusDisabled {
			ValidationError(w, "status must be 'active' or 'disabled'", nil)
			return
		}
		registry.Status = req.Status
	}

	if err := s.store.UpdateSkillRegistry(ctx, registry); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, registry)
}

func (s *Server) deleteSkillRegistry(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	ctx := r.Context()
	registry, err := s.store.GetSkillRegistry(ctx, id)
	if err != nil {
		registry, err = s.store.GetSkillRegistryByName(ctx, id)
		if err != nil {
			NotFound(w, "Skill Registry")
			return
		}
	}

	if err := s.store.DeleteSkillRegistry(ctx, registry.ID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) pinSkillHash(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	ctx := r.Context()
	registry, err := s.store.GetSkillRegistry(ctx, id)
	if err != nil {
		registry, err = s.store.GetSkillRegistryByName(ctx, id)
		if err != nil {
			NotFound(w, "Skill Registry")
			return
		}
	}

	var req PinSkillHashRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.URI == "" {
		ValidationError(w, "uri is required", nil)
		return
	}
	if req.Hash == "" {
		ValidationError(w, "hash is required", nil)
		return
	}

	if err := s.store.PinSkillHash(ctx, registry.ID, req.URI, req.Hash); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "pinned",
		"uri":    req.URI,
		"hash":   req.Hash,
	})
}
