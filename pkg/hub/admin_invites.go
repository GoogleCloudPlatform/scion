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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type InviteCreateRequest struct {
	ExpiresIn string `json:"expiresIn"`
	MaxUses   int    `json:"maxUses"`
	Note      string `json:"note"`
	Email     string `json:"email,omitempty"` // Optional: link invite to allow-list entry
}

type InviteCreateResponse struct {
	Code      string            `json:"code"`
	InviteURL string            `json:"inviteUrl"`
	Invite    *store.InviteCode `json:"invite"`
}

type InviteListResponse struct {
	Items      []store.InviteCode `json:"items"`
	TotalCount int                `json:"totalCount"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// handleAdminInvites handles GET/POST /api/v1/admin/invites.
func (s *Server) handleAdminInvites(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleAdminInvitesList(w, r)
	case http.MethodPost:
		s.handleAdminInvitesCreate(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminInviteByID handles GET/DELETE /api/v1/admin/invites/{id} and POST .../revoke.
func (s *Server) handleAdminInviteByID(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// Parse path: /api/v1/admin/invites/{id} or /api/v1/admin/invites/{id}/revoke or stats
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/invites/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]

	if id == "stats" && r.Method == http.MethodGet {
		s.handleAdminInviteStats(w, r)
		return
	}

	if id == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invite ID is required", nil)
		return
	}

	if len(parts) == 2 && parts[1] == "revoke" {
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.handleAdminInviteRevoke(w, r, id, user)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleAdminInviteGet(w, r, id)
	case http.MethodDelete:
		s.handleAdminInviteDelete(w, r, id, user)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleAdminInvitesList(w http.ResponseWriter, r *http.Request) {
	opts := store.ListOptions{
		Limit: 50,
	}
	if q := r.URL.Query(); q.Get("cursor") != "" {
		opts.Cursor = q.Get("cursor")
	}

	result, err := s.store.ListInviteCodes(r.Context(), opts)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, InviteListResponse{
		Items:      result.Items,
		TotalCount: result.TotalCount,
		NextCursor: result.NextCursor,
	})
}

func (s *Server) handleAdminInvitesCreate(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req InviteCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.ExpiresIn == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "expiresIn is required", nil)
		return
	}

	duration, err := time.ParseDuration(req.ExpiresIn)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid expiresIn duration", nil)
		return
	}

	if duration <= 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "expiresIn must be positive", nil)
		return
	}

	expiresAt := time.Now().Add(duration)

	maxUses := req.MaxUses
	if maxUses < 0 {
		maxUses = 0
	}

	if s.inviteService == nil {
		InternalError(w)
		return
	}

	code, invite, err := s.inviteService.CreateInvite(r.Context(), user.ID(), expiresAt, maxUses, req.Note)
	if err != nil {
		if err == ErrInviteExpiryTooLong {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "expiry exceeds maximum of 5 days", nil)
			return
		}
		slog.Error("failed to create invite", "error", err)
		InternalError(w)
		return
	}

	// If email is provided, link the invite to the allow-list entry.
	if req.Email != "" {
		emailLower := strings.ToLower(strings.TrimSpace(req.Email))
		if err := s.store.UpdateAllowListEntryInviteID(r.Context(), emailLower, invite.ID); err != nil {
			// Log but don't fail — the invite was created successfully.
			slog.Warn("failed to link invite to allow-list entry",
				"email", emailLower,
				"invite_id", invite.ID,
				"error", err,
			)
		}
	}

	inviteURL := s.config.HubEndpoint + "/invite?code=" + code

	slog.Info("invite code created",
		"invite_id", invite.ID,
		"prefix", invite.CodePrefix,
		"expires_at", invite.ExpiresAt,
		"max_uses", invite.MaxUses,
		"created_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditInviteCreated, "", invite.ID, user.ID(), user.Email(), map[string]string{
		"prefix":     invite.CodePrefix,
		"expires_at": invite.ExpiresAt.Format(time.RFC3339),
		"max_uses":   fmt.Sprintf("%d", invite.MaxUses),
	})

	s.events.PublishInviteChanged(r.Context(), "created", invite.ID, invite.CodePrefix)

	writeJSON(w, http.StatusCreated, InviteCreateResponse{
		Code:      code,
		InviteURL: inviteURL,
		Invite:    invite,
	})
}

func (s *Server) handleAdminInviteGet(w http.ResponseWriter, r *http.Request, id string) {
	invite, err := s.store.GetInviteCode(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "invite not found", nil)
			return
		}
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, invite)
}

func (s *Server) handleAdminInviteRevoke(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	if err := s.store.RevokeInviteCode(r.Context(), id); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "invite not found", nil)
			return
		}
		InternalError(w)
		return
	}

	slog.Info("invite code revoked",
		"invite_id", id,
		"revoked_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditInviteRevoked, "", id, user.ID(), user.Email(), nil)
	s.events.PublishInviteChanged(r.Context(), "revoked", id, "")

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleAdminInviteDelete(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	if err := s.store.DeleteInviteCode(r.Context(), id); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "invite not found", nil)
			return
		}
		InternalError(w)
		return
	}

	slog.Info("invite code deleted",
		"invite_id", id,
		"deleted_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditInviteDeleted, "", id, user.ID(), user.Email(), nil)
	s.events.PublishInviteChanged(r.Context(), "deleted", id, "")

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleAdminInviteStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetInviteStats(r.Context())
	if err != nil {
		slog.Error("failed to get invite stats", "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, stats)
}
