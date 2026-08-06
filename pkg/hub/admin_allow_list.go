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
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

type AllowListAddRequest struct {
	Email string `json:"email"`
	Note  string `json:"note"`
}

type AllowListBulkAddRequest struct {
	Emails []AllowListAddRequest `json:"emails"`
}

type AllowListBulkAddResponse struct {
	Added   int `json:"added"`
	Skipped int `json:"skipped"`
	Total   int `json:"total"`
}

type AllowListResponse struct {
	Items      []store.AllowListEntryWithInvite `json:"items"`
	TotalCount int                              `json:"totalCount"`
	NextCursor string                           `json:"nextCursor,omitempty"`
}

// setDeprecationHeader adds the Deprecation header to the response, signaling
// that this endpoint is deprecated in favor of the new invite endpoints.
func setDeprecationHeader(w http.ResponseWriter) {
	w.Header().Set("Deprecation", "true")
}

// handleAdminAllowList handles GET/POST /api/v1/admin/allow-list.
// DEPRECATED: Use POST /api/v1/admin/users/invite and GET /api/v1/users?status=invited instead.
func (s *Server) handleAdminAllowList(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	setDeprecationHeader(w)

	switch r.Method {
	case http.MethodGet:
		s.handleAdminAllowListGet(w, r)
	case http.MethodPost:
		s.handleAdminAllowListAdd(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminAllowListByEmail handles sub-paths under /api/v1/admin/allow-list/.
// DEPRECATED: Use the new invite endpoints instead.
func (s *Server) handleAdminAllowListByEmail(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	setDeprecationHeader(w)

	// Extract sub-path
	subPath := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/allow-list/")

	// Route special sub-paths
	switch subPath {
	case "import":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.handleAdminAllowListImport(w, r, user)
		return
	case "domains":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.handleAdminAllowListDomains(w, r)
		return
	}

	if r.Method != http.MethodDelete {
		MethodNotAllowed(w)
		return
	}

	// Extract email from path: /api/v1/admin/allow-list/{email}
	email := strings.TrimSpace(strings.ToLower(subPath))
	if email == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "email is required", nil)
		return
	}

	// DEPRECATED: Delete User(invited) record instead of AllowListEntry.
	existingUser, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "email not found in allow list", nil)
			return
		}
		InternalError(w)
		return
	}

	// Only delete users with invited status via this deprecated endpoint
	if existingUser.Status != store.UserStatusInvited {
		writeError(w, http.StatusConflict, ErrCodeConflict, "user is not in invited status; cannot remove via allow-list endpoint", nil)
		return
	}

	if err := s.store.DeleteUser(r.Context(), existingUser.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "email not found in allow list", nil)
			return
		}
		InternalError(w)
		return
	}

	slog.Info("allow list entry removed (deprecated: deleted invited user)",
		"email", email,
		"removed_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditAllowListRemove, email, "", user.ID(), user.Email(), nil)
	s.events.PublishAllowListChanged(r.Context(), "removed", email)

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handleAdminAllowListGet returns User(status=invited) records formatted as
// AllowListEntry responses for backward compatibility.
// DEPRECATED: Use GET /api/v1/users?status=invited instead.
func (s *Server) handleAdminAllowListGet(w http.ResponseWriter, r *http.Request) {
	opts := store.ListOptions{
		Limit: 50,
	}
	if q := r.URL.Query(); q.Get("cursor") != "" {
		opts.Cursor = q.Get("cursor")
	}

	filter := store.UserFilter{
		Status: store.UserStatusInvited,
	}

	result, err := s.store.ListUsers(r.Context(), filter, opts)
	if err != nil {
		InternalError(w)
		return
	}

	// Convert User(invited) records to AllowListEntryWithInvite format
	items := make([]store.AllowListEntryWithInvite, 0, len(result.Items))
	for _, u := range result.Items {
		addedBy := ""
		if u.InvitedBy != nil {
			addedBy = *u.InvitedBy
		}
		note := ""
		if u.InviteNote != nil {
			note = *u.InviteNote
		}

		items = append(items, store.AllowListEntryWithInvite{
			AllowListEntry: store.AllowListEntry{
				ID:      u.ID,
				Email:   u.Email,
				Note:    note,
				AddedBy: addedBy,
				Created: u.Created,
			},
		})
	}

	writeJSON(w, http.StatusOK, AllowListResponse{
		Items:      items,
		TotalCount: result.TotalCount,
		NextCursor: result.NextCursor,
	})
}

// handleAdminAllowListAdd creates a User(status=invited) record instead of an AllowListEntry.
// DEPRECATED: Use POST /api/v1/admin/users/invite instead.
func (s *Server) handleAdminAllowListAdd(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req AllowListAddRequest
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
		writeError(w, http.StatusConflict, ErrCodeConflict, "email already on allow list", nil)
		return
	}
	if err != store.ErrNotFound {
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
			writeError(w, http.StatusConflict, ErrCodeConflict, "email already on allow list", nil)
			return
		}
		InternalError(w)
		return
	}

	slog.Info("allow list entry added (deprecated: created invited user)",
		"email", email,
		"added_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditAllowListAdd, email, "", user.ID(), user.Email(), nil)
	s.events.PublishAllowListChanged(r.Context(), "added", email)

	// Return response in AllowListEntry format for backward compatibility
	addedBy := ""
	if newUser.InvitedBy != nil {
		addedBy = *newUser.InvitedBy
	}
	noteStr := ""
	if newUser.InviteNote != nil {
		noteStr = *newUser.InviteNote
	}

	writeJSON(w, http.StatusCreated, &store.AllowListEntry{
		ID:      newUser.ID,
		Email:   newUser.Email,
		Note:    noteStr,
		AddedBy: addedBy,
		Created: newUser.Created,
	})
}

// handleAdminAllowListImport handles POST /api/v1/admin/allow-list/import.
// Creates User(invited) records instead of AllowListEntry records.
// DEPRECATED: Use POST /api/v1/admin/users/invite/bulk instead.
func (s *Server) handleAdminAllowListImport(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	contentType := r.Header.Get("Content-Type")

	var emails []AllowListAddRequest

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// CSV file upload
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
		emails = parsed
	} else {
		// JSON body
		var req AllowListBulkAddRequest
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
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "maximum 1000 emails per import", nil)
		return
	}

	invitedBy := user.ID()
	var added, skipped int

	for _, e := range emails {
		email := strings.TrimSpace(strings.ToLower(e.Email))
		if _, err := mail.ParseAddress(email); err != nil || email == "" {
			continue
		}

		// Check if user already exists
		_, err := s.store.GetUserByEmail(r.Context(), email)
		if err == nil {
			skipped++
			continue
		}
		if err != store.ErrNotFound {
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
			}
			continue
		}

		added++
	}

	slog.Info("allow list bulk import (deprecated: created invited users)",
		"added", added,
		"skipped", skipped,
		"total", added+skipped,
		"imported_by", user.Email(),
	)

	if logger := s.auditLogger; logger != nil {
		event := &InviteAuditEvent{
			EventType:  InviteAuditAllowListBulkAdd,
			ActorID:    user.ID(),
			ActorEmail: user.Email(),
			Success:    true,
			Count:      added,
			Timestamp:  time.Now(),
			Details:    map[string]string{"skipped": fmt.Sprintf("%d", skipped)},
		}
		_ = logger.LogInviteAuditEvent(r.Context(), event)
	}

	s.events.PublishAllowListChanged(r.Context(), "bulk_added", "")

	writeJSON(w, http.StatusOK, AllowListBulkAddResponse{
		Added:   added,
		Skipped: skipped,
		Total:   added + skipped,
	})
}

// handleAdminAllowListDomains handles GET /api/v1/admin/allow-list/domains.
// This endpoint is kept as-is (not deprecated).
func (s *Server) handleAdminAllowListDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := s.store.ListEmailDomains(r.Context())
	if err != nil {
		slog.Error("failed to list email domains", "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"domains": domains,
	})
}

// parseCSVEmails parses a CSV file with email,note columns.
func parseCSVEmails(r io.Reader) ([]AllowListAddRequest, error) {
	reader := csv.NewReader(bufio.NewReader(r))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	var emails []AllowListAddRequest
	lineNum := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV parse error at line %d: %w", lineNum+1, err)
		}
		lineNum++

		if len(record) == 0 {
			continue
		}

		email := strings.TrimSpace(record[0])

		// Skip header row
		if lineNum == 1 && (strings.EqualFold(email, "email") || strings.EqualFold(email, "e-mail")) {
			continue
		}

		if _, err := mail.ParseAddress(email); err != nil || email == "" {
			continue
		}

		var note string
		if len(record) > 1 {
			note = strings.TrimSpace(record[1])
		}

		emails = append(emails, AllowListAddRequest{Email: email, Note: note})
	}

	return emails, nil
}
