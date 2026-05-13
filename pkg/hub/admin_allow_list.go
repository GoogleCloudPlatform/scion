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

// handleAdminAllowList handles GET/POST /api/v1/admin/allow-list.
func (s *Server) handleAdminAllowList(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

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
func (s *Server) handleAdminAllowListByEmail(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

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
	email := subPath
	if email == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "email is required", nil)
		return
	}

	if err := s.store.RemoveAllowListEntry(r.Context(), email); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "email not found in allow list", nil)
			return
		}
		InternalError(w)
		return
	}

	slog.Info("allow list entry removed",
		"email", email,
		"removed_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditAllowListRemove, email, "", user.ID(), user.Email(), nil)
	s.events.PublishAllowListChanged(r.Context(), "removed", email)

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handleAdminAllowListGet(w http.ResponseWriter, r *http.Request) {
	opts := store.ListOptions{
		Limit: 50,
	}
	if q := r.URL.Query(); q.Get("cursor") != "" {
		opts.Cursor = q.Get("cursor")
	}

	result, err := s.store.ListAllowListEntriesWithInvites(r.Context(), opts)
	if err != nil {
		InternalError(w)
		return
	}

	now := time.Now()
	for i := range result.Items {
		entry := &result.Items[i]
		if !entry.InviteExpiresAt.IsZero() && now.After(entry.InviteExpiresAt) {
			entry.InviteExpired = true
		}
	}

	writeJSON(w, http.StatusOK, AllowListResponse{
		Items:      result.Items,
		TotalCount: result.TotalCount,
		NextCursor: result.NextCursor,
	})
}

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

	entry := &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   email,
		Note:    req.Note,
		AddedBy: user.ID(),
	}

	if err := s.store.AddAllowListEntry(r.Context(), entry); err != nil {
		if err == store.ErrAlreadyExists {
			writeError(w, http.StatusConflict, ErrCodeConflict, "email already on allow list", nil)
			return
		}
		InternalError(w)
		return
	}

	slog.Info("allow list entry added",
		"email", email,
		"added_by", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditAllowListAdd, email, "", user.ID(), user.Email(), nil)
	s.events.PublishAllowListChanged(r.Context(), "added", email)

	writeJSON(w, http.StatusCreated, entry)
}

// handleAdminAllowListImport handles POST /api/v1/admin/allow-list/import.
// Accepts either a JSON body with an array of emails, or a CSV file upload.
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
		defer file.Close()

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

	// Build entries
	var entries []*store.AllowListEntry
	for _, e := range emails {
		email := strings.TrimSpace(strings.ToLower(e.Email))
		if _, err := mail.ParseAddress(email); err != nil || email == "" {
			continue
		}
		entries = append(entries, &store.AllowListEntry{
			ID:      uuid.New().String(),
			Email:   email,
			Note:    e.Note,
			AddedBy: user.ID(),
		})
	}

	if len(entries) == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "no valid emails found", nil)
		return
	}

	added, skipped, err := s.store.BulkAddAllowListEntries(r.Context(), entries)
	if err != nil {
		slog.Error("bulk allow list import failed", "error", err)
		InternalError(w)
		return
	}

	slog.Info("allow list bulk import",
		"added", added,
		"skipped", skipped,
		"total", len(entries),
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
		Total:   len(entries),
	})
}

// handleAdminAllowListDomains handles GET /api/v1/admin/allow-list/domains.
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
