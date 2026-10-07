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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// POST /api/v1/users — administrative user provisioning (catalog operation
// user.admin.provision, design .design/admin-user-provisioning.md).
//
// The operation pre-registers a person: it creates the same invited record
// POST /api/v1/admin/users/invite creates, through the same creation core
// and email rule (createPendingUserTx, NormalizeInviteEmail), plus an
// optional display name. It stores no role, binds no provider identity and
// writes no grant. Activation happens only when the person completes a
// configured sign-in flow, which treats the record exactly as an
// invite-created one.

// Mutation and audit names of the operation (design §11).
const (
	provisionOperationID  = "user.admin.provision"
	provisionMutationType = "user_provision"
	provisionReplayLogMsg = "user_provision_replay"
	provisionEventAction  = "provisioned"
)

// Field bounds (design §5.3).
const (
	provisionDisplayNameMaxRunes = 128
	provisionNoteMaxRunes        = 500
	// provisionMaxBodyBytes bounds the request body; the largest valid
	// body is far smaller.
	provisionMaxBodyBytes = 64 << 10
)

// details.reason values of POST /api/v1/users (design §8).
const (
	provisionReasonPrivilegedRole      = "privileged_role_not_provisionable"
	provisionReasonRoleNotSupported    = "role_selection_not_supported"
	provisionReasonUserExists          = "user_exists"
	provisionReasonPendingUserExists   = "pending_user_exists"
	provisionReasonSuspendedUserExists = "user_suspended_exists"
)

// Advisory warnings (design §5.3). They never fail the request.
const (
	provisionWarningReservedIdentity   = "reserved_identity"
	provisionWarningDomainUnauthorized = "domain_not_authorized"
	provisionWarningAccessModeBlocked  = "sign_in_currently_blocked_by_access_mode"
)

// ProvisionedUser is the response view of a provisioned (invited) user.
// It has no role: the role of an invited record is decided at first
// sign-in. Only Email and Status are present in the minimal view.
type ProvisionedUser struct {
	ID          string     `json:"id,omitempty"`
	Email       string     `json:"email"`
	Status      string     `json:"status"`
	DisplayName string     `json:"displayName,omitempty"`
	InvitedBy   string     `json:"invitedBy,omitempty"`
	InviteNote  *string    `json:"inviteNote,omitempty"`
	Created     *time.Time `json:"created,omitempty"`
}

// ProvisionUserResponse is the response body of POST /api/v1/users.
type ProvisionUserResponse struct {
	User     ProvisionedUser `json:"user"`
	Created  bool            `json:"created"`
	Warnings []string        `json:"warnings,omitempty"`
}

// provisionRequestError is a request rejection from
// decodeProvisionRequest: status, error code, message and details.
type provisionRequestError struct {
	status  int
	code    string
	message string
	details map[string]interface{}
}

func (e *provisionRequestError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.message, e.details)
}

func provisionFieldError(status int, code, field, message string) *provisionRequestError {
	return &provisionRequestError{status: status, code: code, message: message,
		details: map[string]interface{}{"field": field}}
}

// provisionAllowedFields is the strict request field set. role is
// recognized only so it can be rejected precisely.
var provisionAllowedFields = map[string]bool{
	"email": true, "displayName": true, "note": true, "role": true,
}

// decodeProvisionString decodes an optional string field. A missing field
// and an explicit null both report present=false.
func decodeProvisionString(fields map[string]json.RawMessage, name string) (value string, present bool, rerr *provisionRequestError) {
	raw, ok := fields[name]
	if !ok || isJSONNull(raw) {
		return "", false, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, provisionFieldError(http.StatusBadRequest, ErrCodeInvalidRequest, name,
			fmt.Sprintf("invalid %s: must be a string", name))
	}
	return value, true, nil
}

// hasControlChar reports whether s contains a control character other
// than the allowed ones.
func hasControlChar(s string, allowNewline bool) bool {
	for _, r := range s {
		if allowNewline && r == '\n' {
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// decodeProvisionRequest strictly decodes and validates a provisioning
// request (design §5.3, §8 rows 9-12, in that order). It returns the
// pending-user spec without InvitedBy.
func decodeProvisionRequest(r *http.Request) (PendingUserSpec, *provisionRequestError) {
	invalidBody := func(msg string) *provisionRequestError {
		return &provisionRequestError{status: http.StatusBadRequest, code: ErrCodeInvalidRequest, message: msg}
	}

	if r.Body == nil {
		return PendingUserSpec{}, invalidBody("invalid request body: a JSON object is required")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, provisionMaxBodyBytes+1))
	if err != nil {
		return PendingUserSpec{}, invalidBody("invalid request body")
	}
	if len(body) > provisionMaxBodyBytes {
		return PendingUserSpec{}, invalidBody("request body too large")
	}
	// Row 9: the body must be exactly one JSON object.
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return PendingUserSpec{}, invalidBody("invalid request body: a JSON object is required")
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&fields); err != nil {
		return PendingUserSpec{}, invalidBody("invalid request body: " + err.Error())
	}
	if dec.More() {
		return PendingUserSpec{}, invalidBody("invalid request body: trailing data after the JSON object")
	}

	// Row 9: unknown fields, reported in a stable (sorted) order.
	var unknown []string
	for name := range fields {
		if !provisionAllowedFields[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return PendingUserSpec{}, invalidBody(fmt.Sprintf("unknown field %q; allowed fields are email, displayName, note", unknown[0]))
	}

	// Row 9: wrong JSON types for the known string fields. role has no
	// type check (row 12 covers every non-null value).
	rawEmail, _, rerr := decodeProvisionString(fields, "email")
	if rerr != nil {
		return PendingUserSpec{}, rerr
	}
	displayName, _, rerr := decodeProvisionString(fields, "displayName")
	if rerr != nil {
		return PendingUserSpec{}, rerr
	}
	note, notePresent, rerr := decodeProvisionString(fields, "note")
	if rerr != nil {
		return PendingUserSpec{}, rerr
	}

	// Row 10: the shared invite email rule; same code and message as
	// invite.
	email, err := NormalizeInviteEmail(rawEmail)
	if err != nil {
		return PendingUserSpec{}, provisionFieldError(http.StatusBadRequest, ErrCodeInvalidRequest, "email", "valid email is required")
	}

	// Row 11: displayName is trimmed; empty after trim is absent. note is
	// not trimmed; "" is absent (stored NULL), matching invite.
	displayName = strings.TrimSpace(displayName)
	if utf8.RuneCountInString(displayName) > provisionDisplayNameMaxRunes {
		return PendingUserSpec{}, provisionFieldError(http.StatusBadRequest, ErrCodeValidationError, "displayName",
			fmt.Sprintf("displayName must be at most %d characters", provisionDisplayNameMaxRunes))
	}
	if hasControlChar(displayName, false) {
		return PendingUserSpec{}, provisionFieldError(http.StatusBadRequest, ErrCodeValidationError, "displayName",
			"displayName must not contain control characters")
	}
	var notePtr *string
	if notePresent && note != "" {
		if utf8.RuneCountInString(note) > provisionNoteMaxRunes {
			return PendingUserSpec{}, provisionFieldError(http.StatusBadRequest, ErrCodeValidationError, "note",
				fmt.Sprintf("note must be at most %d characters", provisionNoteMaxRunes))
		}
		if hasControlChar(note, true) {
			return PendingUserSpec{}, provisionFieldError(http.StatusBadRequest, ErrCodeValidationError, "note",
				"note must not contain control characters other than newline")
		}
		notePtr = &note
	}

	// Row 12: any non-null role is refused. No role is chosen or stored
	// at provisioning; the role at activation follows the configured
	// policy.
	if raw, ok := fields["role"]; ok && !isJSONNull(raw) {
		var roleStr string
		if json.Unmarshal(raw, &roleStr) == nil && roleStr == store.UserRoleAdmin {
			return PendingUserSpec{}, &provisionRequestError{status: http.StatusUnprocessableEntity, code: ErrCodeUnprocessable,
				message: "the admin role cannot be provisioned",
				details: map[string]interface{}{"field": "role", "reason": provisionReasonPrivilegedRole}}
		}
		return PendingUserSpec{}, &provisionRequestError{status: http.StatusUnprocessableEntity, code: ErrCodeUnprocessable,
			message: "role cannot be set at provisioning; the role is assigned at first sign-in",
			details: map[string]interface{}{"field": "role", "reason": provisionReasonRoleNotSupported}}
	}

	return PendingUserSpec{Email: email, DisplayName: displayName, Note: notePtr}, nil
}

// provisionWarnings returns the advisory warnings for email under the
// current sign-in configuration (design §5.3). They describe how the
// paths that evaluate checkUserAuthorized will treat the email.
func (s *Server) provisionWarnings(email string) []string {
	var warnings []string
	if isReservedPlatformIdentity(email, s.platformAuthSA) {
		warnings = append(warnings, provisionWarningReservedIdentity)
	}
	// The admin-email and domain checks mirror checkUserAuthorized, so a
	// warning predicts what that check will do.
	isAdminEmail := false
	for _, admin := range s.AdminEmails() {
		if strings.ToLower(admin) == email {
			isAdminEmail = true
			break
		}
	}
	domains := s.AuthorizedDomains()
	if !isAdminEmail && len(domains) > 0 && !isEmailInDomains(email, domains) {
		warnings = append(warnings, provisionWarningDomainUnauthorized)
	}
	if !isAdminEmail && s.UserAccessMode() == "domain_restricted" && len(domains) == 0 {
		warnings = append(warnings, provisionWarningAccessModeBlocked)
	}
	return warnings
}

// provisionedUserView returns the response view of u: the detailed view
// when detailed, otherwise the minimal view {email, status}.
func provisionedUserView(u *store.User, detailed bool) ProvisionedUser {
	if !detailed {
		return ProvisionedUser{Email: u.Email, Status: u.Status}
	}
	v := ProvisionedUser{
		ID:          u.ID,
		Email:       u.Email,
		Status:      u.Status,
		DisplayName: u.DisplayName,
		InviteNote:  normalizedNote(u.InviteNote),
	}
	if u.InvitedBy != nil {
		v.InvitedBy = *u.InvitedBy
	}
	if !u.Created.IsZero() {
		created := u.Created
		v.Created = &created
	}
	return v
}

// provisionAuditSummary is the AfterSummary of the user_provision
// mutation audit record (design §11).
func provisionAuditSummary(u *store.User) string {
	b, _ := json.Marshal(struct {
		Email       string `json:"email"`
		Status      string `json:"status"`
		DisplayName string `json:"displayName"`
	}{u.Email, u.Status, u.DisplayName})
	return string(b)
}

// handleProvisionUser handles POST /api/v1/users (user.admin.provision).
//
// Order (design §8, §9): admission and authorization run on every
// request, including replays, before the body is decoded; then strict
// decoding and validation; then the detail-authority check, which only
// selects the response view; then one transaction holding the user row
// and its mutation audit; then best-effort post-commit side effects.
func (s *Server) handleProvisionUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	target := Resource{Type: "user"}

	// Rows 1, 4, 5: admission through the D.2 session-only gate. A user
	// access token is refused here until token admission is enabled. The
	// gate writes the response; a refused identity is logged here so the
	// log follows the gate's own decision.
	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		if identity := GetIdentityFromContext(ctx); identity != nil {
			logAuthzDenial(r, identity, target, ActionInvite, provisionOperationID+": refused by the session-only gate")
		}
		return
	}

	// Row 8: live user.invite on the hub-level user collection. The
	// collection evidence makes the target the hub scope, so only a grant
	// that applies there counts.
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:      principalContextForIdentity(actor),
		Credential:     credentialContextForIdentity(actor),
		Resource:       target,
		Action:         ActionInvite,
		Permission:     "user.invite",
		TargetEvidence: hubCollectionEvidence("user.invite"),
	})
	if !decision.Allowed {
		logAuthzDenial(r, actor, target, ActionInvite, provisionOperationID+": "+decision.Reason)
		writeForbiddenStructured(w, "requires user.invite permission", "user", ActionInvite)
		return
	}

	// Rows 9-12.
	spec, rerr := decodeProvisionRequest(r)
	if rerr != nil {
		if rerr.status == http.StatusUnprocessableEntity {
			logAuthzDenial(r, actor, target, ActionInvite,
				fmt.Sprintf("%s: %s: %v", provisionOperationID, authzop.DenialRoleAssignmentForbidden, rerr.details["reason"]))
		}
		rerr.write(w)
		return
	}
	spec.InvitedBy = actor.ID()

	// Detail authority (design §5.4): user.read at hub scope, evaluated
	// like user.invite. It never admits or denies; it selects the replay
	// view and the collision detail.
	detail := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   hubScopedResource("user", "hub"),
		Action:     ActionRead,
		Permission: "user.read",
	}).Allowed

	warnings := s.provisionWarnings(spec.Email)
	auditActor := s.buildAuditActorFromContext(ctx)

	var u *store.User
	var outcome PendingOutcome
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		var err error
		u, outcome, err = createPendingUserTx(ctx, tx, spec)
		if err != nil || outcome != PendingCreated {
			return err
		}
		record := &store.MutationAuditRecord{
			MutationType: provisionMutationType,
			TargetType:   "user",
			TargetID:     u.ID,
			AfterSummary: provisionAuditSummary(u),
			Timestamp:    time.Now(),
		}
		auditActor.ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("audit user provision: %w", err)
		}
		return nil
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		// Row 18: a concurrent insert won the unique-index race. Re-read
		// and classify without writing.
		u, outcome, err = lookupPendingUser(ctx, s.store, spec.Email, spec)
	}
	if err != nil {
		// Rows 19-20: the transaction rolled back.
		slog.ErrorContext(ctx, "user provisioning failed", "operation", provisionOperationID, "email", spec.Email, "error", err)
		InternalError(w)
		return
	}

	switch outcome {
	case PendingCreated:
		s.provisionPostCommit(r, actor, u)
		w.Header().Set("Location", "/api/v1/users/"+u.ID)
		writeJSON(w, http.StatusCreated, ProvisionUserResponse{
			User: provisionedUserView(u, true), Created: true, Warnings: warnings,
		})
	case PendingExistingIdentical:
		slog.InfoContext(ctx, provisionReplayLogMsg, "operation", provisionOperationID,
			"email", u.Email, "user_id", u.ID, "actor_id", actor.ID())
		writeJSON(w, http.StatusOK, ProvisionUserResponse{
			User: provisionedUserView(u, detail), Created: false, Warnings: warnings,
		})
	case PendingExistingDifferent:
		if detail {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a pending user with this email exists with different details",
				map[string]interface{}{"reason": provisionReasonPendingUserExists, "userId": u.ID})
			return
		}
		writeProvisionUserExists(w)
	case PendingExistingSuspended:
		if detail {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"this email belongs to a suspended user",
				map[string]interface{}{"reason": provisionReasonSuspendedUserExists})
			return
		}
		writeProvisionUserExists(w)
	default:
		// Row 16 (active user): never carries userId, for any caller.
		writeProvisionUserExists(w)
	}
}

// writeProvisionUserExists writes the undifferentiated collision
// response, matching invite's message.
func writeProvisionUserExists(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists",
		map[string]interface{}{"reason": provisionReasonUserExists})
}

// provisionPostCommit runs the best-effort side effects after a new
// record commits (design §9, row 21): the invite audit event and the
// allow-list change event. A failure is logged and does not change the
// response; the mutation audit is already committed.
func (s *Server) provisionPostCommit(r *http.Request, actor UserIdentity, u *store.User) {
	ctx := r.Context()
	slog.InfoContext(ctx, "user provisioned", "email", u.Email, "invited_by", actor.Email(), "user_id", u.ID)
	if logger := s.auditLogger; logger != nil {
		event := &InviteAuditEvent{
			EventType:  InviteAuditUserProvisioned,
			Email:      u.Email,
			ActorID:    actor.ID(),
			ActorEmail: actor.Email(),
			Success:    true,
			Timestamp:  time.Now(),
			Details:    map[string]string{"user_id": u.ID},
		}
		if err := logger.LogInviteAuditEvent(ctx, event); err != nil {
			slog.WarnContext(ctx, "user provisioned, but the invite audit event failed",
				"email", u.Email, "user_id", u.ID, "error", err)
		}
	}
	if s.events != nil {
		s.events.PublishAllowListChanged(ctx, provisionEventAction, u.Email)
	}
}
