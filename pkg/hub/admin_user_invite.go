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

// UserInviteBulkRequest is the JSON request body for bulk invite. The
// handler decodes the body strictly (decodeInviteBatchJSON); this type is
// the shape callers in Go build.
type UserInviteBulkRequest struct {
	Emails   []UserInviteRequest     `json:"emails"`
	Projects []UserInviteBulkProject `json:"projects,omitempty"`
}

// UserInviteBulkProject is one projects[] entry of a bulk invite: the
// project roles every created or already invited address receives.
type UserInviteBulkProject struct {
	ProjectID         string   `json:"projectId"`
	RoleDefinitionIDs []string `json:"roleDefinitionIds"`
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
	if s.proxyDrivers.contains(email) {
		logProxyDriverCreateRefusal("invite", user.ID(), email)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, proxyDriverPrincipalUserMessage, nil)
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

// handleAdminUserInviteBulk handles POST /api/v1/admin/users/invite/bulk
// (batch invite, ptone/scion#4952). It accepts a strict
// JSON body, or a CSV file (multipart/form-data) without projects, and
// answers with one result per input entry. See admin_user_invite_batch.go.
func (s *Server) handleAdminUserInviteBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	actor := GetUserIdentityFromContext(r.Context())
	if actor == nil {
		Unauthorized(w)
		return
	}

	// Decode and request-level validation: no writes, no audit.
	var req *inviteBatchRequest
	var rerr *inviteBatchRequestError
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		req, rerr = decodeInviteBatchMultipart(w, r)
	} else {
		req, rerr = decodeInviteBatchJSON(r)
	}
	if rerr != nil {
		rerr.write(w)
		return
	}

	ctx := r.Context()
	batchID := uuid.New().String()

	if len(req.projects) > 0 {
		if s.membershipService == nil {
			slog.ErrorContext(ctx, "bulk invite: membership service not configured", "batch_id", batchID)
			InternalError(w)
			return
		}
		// Preflight: may only REFUSE the whole request before any write.
		// A pass here is NOT permission. Every authoritative check, including
		// the in-lock recheck, runs again inside SetMemberRoles for each
		// address. Do not skip, cache or weaken any SetMemberRoles check
		// because this preflight passed, and do not treat its result as a
		// grant decision anywhere else.
		refusals, err := s.inviteBatchPreflight(ctx, actor, req.projects)
		if err != nil {
			slog.ErrorContext(ctx, "bulk invite: project preflight failed", "batch_id", batchID, "error", err)
			InternalError(w)
			return
		}
		if len(refusals) > 0 {
			s.logInviteBatchRefused(context.WithoutCancel(ctx), actor, batchID, len(req.entries), refusals)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"one or more requested project memberships cannot be granted by the caller; nothing was created",
				map[string]interface{}{
					"reason":   inviteBatchReasonProjectsNotGrantable,
					"batchId":  batchID,
					"projects": refusals,
				})
			return
		}
	}

	addrs := s.validateInviteBatchAddresses(actor.ID(), req.entries)

	// Detail authority (the provision rule): user.read at hub scope, once.
	// It selects the view of existing rows; it never admits or denies.
	detail := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   hubScopedResource("user", "hub"),
		Action:     ActionRead,
		Permission: "user.read",
	}).Allowed

	// The apply steps and their side effects are not cut short by a client
	// disconnect, but are bounded by the run deadline.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inviteBulkRunTimeout)
	defer cancel()
	var members inviteBatchMemberSetter
	if s.membershipService != nil {
		members = s.membershipService
	}
	run := s.runInviteBatch(runCtx, actor, batchID, addrs, req.projects, members, detail)

	// Post-run side effects run on a context without the run deadline, so
	// a run that used all of it still records its audit.
	postCtx := context.WithoutCancel(ctx)
	for _, projectID := range run.changedProjects {
		s.publishConduitAuthzChanged(conduitAuthzMatch{ProjectID: projectID})
	}
	complete := run.counts.Failed == 0 && run.counts.MembershipFailed == 0
	slog.InfoContext(postCtx, "bulk user invite",
		"batch_id", batchID,
		"created", run.counts.Created,
		"failed", run.counts.Failed,
		"membership_failed", run.counts.MembershipFailed,
		"total", len(req.entries),
		"invited_by", actor.Email(),
	)
	s.logInviteBatchRun(postCtx, actor, batchID, req.projects, run, complete)
	if run.counts.Created > 0 && s.events != nil {
		s.events.PublishAllowListChanged(postCtx, "bulk_invited", "")
	}

	c := run.counts
	writeJSON(w, http.StatusOK, UserInviteBulkResponse{
		BatchID:  batchID,
		Invited:  c.Created,
		Skipped:  c.AlreadyInvited + c.AlreadyActive + c.Suspended + c.Exists,
		Total:    len(req.entries),
		Complete: complete,
		Counts:   c,
		Results:  run.results,
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
	// The reserved test-identity domain is created only by the issuance
	// endpoint, never through an invite.
	if isReservedTestIdentityEmail(email) {
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
