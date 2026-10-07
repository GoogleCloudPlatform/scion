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
	"errors"
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

	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	var req UserInviteRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	email, err := NormalizeInviteEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "valid email is required", nil)
		return
	}

	invitedBy := user.ID()
	var note *string
	if req.Note != "" {
		note = &req.Note
	}

	newUser, outcome, err := createPendingUserTx(r.Context(), s.store, PendingUserSpec{
		Email:     email,
		Note:      note,
		InvitedBy: invitedBy,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists", nil)
			return
		}
		slog.Error("failed to create invited user", "email", email, "error", err)
		InternalError(w)
		return
	}
	// Invite reports every existing record the same way, whatever its
	// state (design §5.2): one undifferentiated 409.
	if outcome != PendingCreated {
		writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists", nil)
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

	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
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
			emails = append(emails, UserInviteRequest(p))
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
		email, err := NormalizeInviteEmail(e.Email)
		if err != nil {
			continue // skip invalid emails
		}

		// Check if user already exists
		_, err = s.store.GetUserByEmail(r.Context(), email)
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
			Role:       store.UserRoleMember, // placeholder; the real role is assigned at first sign-in (see determineUserRole)
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

// --- Shared pending-user creation core ---

// errInvalidInviteEmail is returned by NormalizeInviteEmail for an email
// the invite rule rejects.
var errInvalidInviteEmail = errors.New("valid email is required")

// NormalizeInviteEmail is the one email rule of every pending-user entry
// point: single invite, bulk invite and POST /api/v1/users. It trims and
// lowercases raw, and accepts the result when it is non-empty and
// mail.ParseAddress accepts it.
//
// mail.ParseAddress also accepts RFC 5322 display-name forms such as
// "Bob <bob@x.com>", and the whole lowercased string is then the stored
// email. Every entry point keeps that behaviour, so they stay at parity;
// tightening it would change all of them together.
func NormalizeInviteEmail(raw string) (string, error) {
	email := strings.TrimSpace(strings.ToLower(raw))
	if email == "" {
		return "", errInvalidInviteEmail
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return "", errInvalidInviteEmail
	}
	return email, nil
}

// PendingUserSpec describes the pending (invited) user record a
// pending-user entry point asks createPendingUserTx to create.
type PendingUserSpec struct {
	// Email is the requested email. It is normalized with
	// NormalizeInviteEmail inside the core.
	Email string
	// DisplayName is the profile name to store; invite passes "".
	DisplayName string
	// Note is the admin note, stored as InviteNote. A nil Note and a Note
	// pointing to "" both store NULL.
	Note *string
	// InvitedBy is the human user ID of the actor.
	InvitedBy string
}

// PendingOutcome is the result of a pending-user creation attempt.
type PendingOutcome int

const (
	// PendingCreated: a new invited record was inserted.
	PendingCreated PendingOutcome = iota
	// PendingExistingIdentical: an invited record exists whose display
	// name and note equal the normalized request.
	PendingExistingIdentical
	// PendingExistingDifferent: an invited record exists with a different
	// display name or note.
	PendingExistingDifferent
	// PendingExistingActive: an active user has the email.
	PendingExistingActive
	// PendingExistingSuspended: a suspended user has the email.
	PendingExistingSuspended
)

// normalizedNote returns note with "" mapped to nil, the stored form.
func normalizedNote(note *string) *string {
	if note == nil || *note == "" {
		return nil
	}
	return note
}

// classifyExistingPendingUser classifies an existing user record against
// spec. It never writes.
func classifyExistingPendingUser(existing *store.User, spec PendingUserSpec) PendingOutcome {
	switch existing.Status {
	case store.UserStatusSuspended:
		return PendingExistingSuspended
	case store.UserStatusInvited:
		want := normalizedNote(spec.Note)
		got := normalizedNote(existing.InviteNote)
		sameNote := (want == nil && got == nil) || (want != nil && got != nil && *want == *got)
		if sameNote && existing.DisplayName == spec.DisplayName {
			return PendingExistingIdentical
		}
		return PendingExistingDifferent
	default:
		// active, and any status this core does not know, is never
		// treated as pending.
		return PendingExistingActive
	}
}

// lookupPendingUser reads the user with spec's (normalized) email and
// classifies it. It returns store.ErrNotFound when there is none.
func lookupPendingUser(ctx context.Context, st store.Store, email string, spec PendingUserSpec) (*store.User, PendingOutcome, error) {
	existing, err := st.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, 0, err
	}
	return existing, classifyExistingPendingUser(existing, spec), nil
}

// createPendingUserTx looks up spec's normalized email in st and, when no
// user has it, creates an invited record (placeholder role member; the
// real role is assigned at first sign-in, see determineUserRole). st may
// be a transaction. It never modifies an existing record: an existing
// user is returned with its classification and a nil error.
//
// A concurrent insert that wins the unique-index race surfaces as
// store.ErrAlreadyExists from CreateUser; the caller re-reads and
// classifies (inside a transaction that error aborts the transaction).
func createPendingUserTx(ctx context.Context, st store.Store, spec PendingUserSpec) (*store.User, PendingOutcome, error) {
	email, err := NormalizeInviteEmail(spec.Email)
	if err != nil {
		return nil, 0, err
	}

	existing, outcome, err := lookupPendingUser(ctx, st, email, spec)
	if err == nil {
		return existing, outcome, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, 0, fmt.Errorf("check existing user: %w", err)
	}

	invitedBy := spec.InvitedBy
	newUser := &store.User{
		ID:          uuid.New().String(),
		Email:       email,
		DisplayName: spec.DisplayName,
		Status:      store.UserStatusInvited,
		Role:        store.UserRoleMember, // placeholder; the real role is assigned at first sign-in (see determineUserRole)
		InvitedBy:   &invitedBy,
		InviteNote:  normalizedNote(spec.Note),
	}
	if err := st.CreateUser(ctx, newUser); err != nil {
		return nil, 0, err
	}
	return newUser, PendingCreated, nil
}
