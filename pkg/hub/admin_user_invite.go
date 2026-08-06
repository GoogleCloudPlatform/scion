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
	"net/mail"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// --- Request/Response types ---

// UserInviteRequest is the request body for POST /api/v1/admin/users/invite.
type UserInviteRequest struct {
	Email string `json:"email"`
	Note  string `json:"note"`
}

// UserInviteResponse is the response body for POST /api/v1/admin/users/invite.
type UserInviteResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	InvitedBy string    `json:"invitedBy"`
	Created   time.Time `json:"created"`
}

// UserInviteBulkRequest is the JSON request body for bulk invite.
type UserInviteBulkRequest struct {
	Emails []UserInviteRequest `json:"emails"`
}

// UserInviteBulkResponse is the response body for POST /api/v1/admin/users/invite/bulk.
type UserInviteBulkResponse struct {
	Invited int      `json:"invited"`
	Skipped int      `json:"skipped"`
	Total   int      `json:"total"`
	Errors  []string `json:"errors"`
}

// --- Handlers ---

// handleAdminUserInvite handles POST /api/v1/admin/users/invite.
func (s *Server) handleAdminUserInvite(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	var req UserInviteRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	if _, err := mail.ParseAddress(email); err != nil || email == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "valid email is required", nil)
		return
	}

	// Check if user already exists
	_, err := s.store.GetUserByEmail(r.Context(), email)
	if err == nil {
		writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists", nil)
		return
	}
	if err != store.ErrNotFound {
		slog.Error("failed to check existing user", "email", email, "error", err)
		InternalError(w)
		return
	}

	invitedBy := user.ID()
	var note *string
	if req.Note != "" {
		note = &req.Note
	}

	newUser := &store.User{
		ID:         uuid.New().String(),
		Email:      email,
		Status:     store.UserStatusInvited,
		Role:       store.UserRoleMember,
		InvitedBy:  &invitedBy,
		InviteNote: note,
	}

	if err := s.store.CreateUser(r.Context(), newUser); err != nil {
		if err == store.ErrAlreadyExists {
			writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists", nil)
			return
		}
		slog.Error("failed to create invited user", "email", email, "error", err)
		InternalError(w)
		return
	}

	slog.Info("user invited",
		"email", email,
		"invited_by", user.Email(),
		"user_id", newUser.ID,
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditUserInvited, email, "", user.ID(), user.Email(), nil)
	s.events.PublishAllowListChanged(r.Context(), "invited", email)

	writeJSON(w, http.StatusCreated, UserInviteResponse{
		ID:        newUser.ID,
		Email:     newUser.Email,
		Status:    newUser.Status,
		InvitedBy: invitedBy,
		Created:   newUser.Created,
	})
}

// handleAdminUserInviteBulk handles POST /api/v1/admin/users/invite/bulk.
// Accepts JSON or CSV (multipart/form-data) input.
func (s *Server) handleAdminUserInviteBulk(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	contentType := r.Header.Get("Content-Type")

	var emails []UserInviteRequest

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// CSV file upload — reuse the parseCSVEmails helper from admin_allow_list.go
		if err := r.ParseMultipartForm(2 << 20); err != nil { // 2MB max
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "failed to parse multipart form", nil)
			return
		}

		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "file field is required", nil)
			return
		}
		defer func() { _ = file.Close() }()

		parsed, err := parseCSVEmails(file)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
			return
		}
		// Convert AllowListAddRequest to UserInviteRequest
		for _, p := range parsed {
			emails = append(emails, UserInviteRequest{Email: p.Email, Note: p.Note})
		}
	} else {
		// JSON body
		var req UserInviteBulkRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
			return
		}
		emails = req.Emails
	}

	if len(emails) == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "no emails provided", nil)
		return
	}

	if len(emails) > 1000 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "maximum 1000 emails per bulk invite", nil)
		return
	}

	invitedBy := user.ID()
	var invited, skipped int
	var errMsgs []string

	for _, e := range emails {
		email := strings.TrimSpace(strings.ToLower(e.Email))
		if _, err := mail.ParseAddress(email); err != nil || email == "" {
			continue // skip invalid emails
		}

		// Check if user already exists
		_, err := s.store.GetUserByEmail(r.Context(), email)
		if err == nil {
			skipped++
			continue
		}
		if err != store.ErrNotFound {
			slog.Error("bulk invite: failed to check existing user", "email", email, "error", err)
			errMsgs = append(errMsgs, fmt.Sprintf("failed to process %s", email))
			continue
		}

		var note *string
		if e.Note != "" {
			note = &e.Note
		}

		newUser := &store.User{
			ID:         uuid.New().String(),
			Email:      email,
			Status:     store.UserStatusInvited,
			Role:       store.UserRoleMember,
			InvitedBy:  &invitedBy,
			InviteNote: note,
		}

		if err := s.store.CreateUser(r.Context(), newUser); err != nil {
			if err == store.ErrAlreadyExists {
				skipped++
				continue
			}
			slog.Error("bulk invite: failed to create user", "email", email, "error", err)
			errMsgs = append(errMsgs, fmt.Sprintf("failed to process %s", email))
			continue
		}

		invited++
	}

	slog.Info("bulk user invite",
		"invited", invited,
		"skipped", skipped,
		"total", invited+skipped+len(errMsgs),
		"errors", len(errMsgs),
		"invited_by", user.Email(),
	)

	if logger := s.auditLogger; logger != nil {
		event := &InviteAuditEvent{
			EventType:  InviteAuditUserInvitedBulk,
			ActorID:    user.ID(),
			ActorEmail: user.Email(),
			Success:    true,
			Count:      invited,
			Timestamp:  time.Now(),
			Details:    map[string]string{"skipped": fmt.Sprintf("%d", skipped)},
		}
		_ = logger.LogInviteAuditEvent(r.Context(), event)
	}

	s.events.PublishAllowListChanged(r.Context(), "bulk_invited", "")

	writeJSON(w, http.StatusOK, UserInviteBulkResponse{
		Invited: invited,
		Skipped: skipped,
		Total:   invited + skipped + len(errMsgs),
		Errors:  errMsgs,
	})
}
