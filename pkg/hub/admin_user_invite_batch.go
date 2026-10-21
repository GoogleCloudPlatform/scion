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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Batch invite: POST /api/v1/admin/users/invite/bulk with per-address
// results and optional project memberships (ptone/scion#4952, design v2).
//
// Order: route guard (unchanged); strict decode and request caps (no
// writes); project preflight, only with projects (may only refuse);
// per-address normalisation and dedup (pure); per-address apply in input
// order (user row, then memberships through SetMemberRoles); aggregate
// audit and one allow-list event; 200 with one result per input entry.
//
// No request field reaches users.role or any system-scoped binding: hub
// roles are assigned at first sign-in by hub policy. Project roles are
// granted only through ProjectMembershipService.SetMemberRoles, the
// members PUT engine, with ExpectedRoleIDs = [] so a batch only ever adds
// a membership to a principal that has none.

// Request caps (design §3.2).
const (
	inviteBulkMaxEmails          = 1000
	inviteBulkMaxProjects        = 20
	inviteBulkMaxRolesPerProject = 10
	// inviteBulkMaxMemberships caps len(emails) × len(projects), which
	// keeps a run inside the server write timeout.
	inviteBulkMaxMemberships = 2000
	// inviteBulkMaxBodyBytes bounds the JSON body. It is sized from the
	// worst case of a valid request as encoding/json writes it: 1000
	// entries with a 254-byte email and a 500-rune note whose every
	// character encodes to its longest escaped form, plus 20 projects of
	// 10 role IDs (TestAdminUserInviteBulk_BodyCapFitsWorstCase computes
	// it).
	inviteBulkMaxBodyBytes = 5 << 20
	// inviteBulkMaxMultipartBytes bounds the multipart (CSV) body.
	inviteBulkMaxMultipartBytes = 2 << 20
	// inviteBulkRunTimeout bounds the apply steps, which run on a context
	// a client disconnect does not cancel.
	inviteBulkRunTimeout = 50 * time.Second
	// inviteBulkInputEchoMaxBytes bounds the raw input echoed for an
	// invalid address.
	inviteBulkInputEchoMaxBytes = 320
	// inviteBatchInternalErrorStopAfter is the number of consecutive
	// internal_error membership results on one project after which the
	// batch stops calling SetMemberRoles for that project. internal_error
	// is a continue code (a single fault on one address does not cancel
	// the project), but a run of them means the project cannot be written
	// right now, and every remaining address would pay for another
	// failing locked transaction.
	inviteBatchInternalErrorStopAfter = 5
)

// Address statuses (design §3.5).
const (
	inviteBatchStatusCreated        = "created"
	inviteBatchStatusAlreadyInvited = "already_invited"
	inviteBatchStatusAlreadyActive  = "already_active"
	inviteBatchStatusSuspended      = "suspended"
	// inviteBatchStatusExists replaces the three statuses above for a
	// caller without user.read (the provision detail-authority rule).
	inviteBatchStatusExists    = "exists"
	inviteBatchStatusInvalid   = "invalid"
	inviteBatchStatusDuplicate = "duplicate"
	inviteBatchStatusFailed    = "failed"
)

// Address reasons that have no provision constant (design §3.5). The
// shared ones reuse provisionReason* (user_exists, pending_user_exists,
// user_suspended_exists, email_not_allowed, reserved_test_identity_domain).
const (
	inviteBatchReasonInvalidEmail          = "invalid_email"
	inviteBatchReasonNoteTooLong           = "note_too_long"
	inviteBatchReasonNoteInvalidCharacters = "note_invalid_characters"
	inviteBatchReasonDuplicateInBatch      = "duplicate_in_batch"
	inviteBatchReasonInternalError         = "internal_error"
	inviteBatchReasonNotProcessed          = "not_processed"
)

// Membership entry statuses and codes (design §3.5).
const (
	inviteBatchMembershipApplied       = "applied"
	inviteBatchMembershipAlreadyMember = "already_member"
	inviteBatchMembershipConflict      = "conflict"
	inviteBatchMembershipFailed        = "failed"
	inviteBatchMembershipSkipped       = "skipped"

	inviteBatchCodeMemberWithOtherRoles = "member_with_other_roles"
	inviteBatchCodeAddressNotInvited    = "address_not_invited"
	inviteBatchCodeNotProcessed         = "not_processed"
)

// Preflight refusal reasons (design §5.2).
const (
	inviteBatchReasonProjectsNotGrantable = "batch_projects_not_grantable"
	inviteBatchReasonProjectNotManageable = "project_not_manageable"
	inviteBatchReasonRoleNotAssignable    = "role_not_assignable"
	inviteBatchReasonRoleNotGrantable     = "role_not_grantable"
)

// inviteBulkRoleLikeFields are top-level fields that would select a hub
// role. They are refused with 422 role_selection_not_supported, the reason
// POST /api/v1/users gives.
var inviteBulkRoleLikeFields = []string{"hubRole", "hubRoles", "role", "roles"}

// --- Response types ---

// InviteBatchCounts counts the address statuses of a run, plus the failed
// memberships. Exists is used only for a caller without user.read, in
// place of the three statuses it collapses.
type InviteBatchCounts struct {
	Created          int `json:"created"`
	AlreadyInvited   int `json:"already_invited"`
	AlreadyActive    int `json:"already_active"`
	Suspended        int `json:"suspended"`
	Exists           int `json:"exists"`
	Invalid          int `json:"invalid"`
	Duplicate        int `json:"duplicate"`
	Failed           int `json:"failed"`
	MembershipFailed int `json:"membership_failed"`
}

// InviteBatchMembership is the outcome of one requested project
// membership for one address.
type InviteBatchMembership struct {
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
	// Code is the SetMemberRoles denial code for failed, the conflict code
	// for conflict, and address_not_invited for skipped.
	Code string `json:"code,omitempty"`
	// Cause copies details.cause of the denial, when present.
	Cause string `json:"cause,omitempty"`
	// CurrentRoleDefinitionIDs is set for conflict.
	CurrentRoleDefinitionIDs []string `json:"currentRoleDefinitionIds,omitempty"`
}

// InviteBatchResult is the outcome for one input entry.
type InviteBatchResult struct {
	Index int `json:"index"`
	// Email is the normalised address, when it normalised.
	Email string `json:"email,omitempty"`
	// Input is the raw input (at most 320 bytes), for an invalid entry.
	Input       string                  `json:"input,omitempty"`
	Status      string                  `json:"status"`
	Reason      string                  `json:"reason,omitempty"`
	UserID      string                  `json:"userId,omitempty"`
	DuplicateOf *int                    `json:"duplicateOf,omitempty"`
	Memberships []InviteBatchMembership `json:"memberships,omitempty"`
}

// UserInviteBulkResponse is the response body for POST
// /api/v1/admin/users/invite/bulk. Invited, Skipped and Total keep their
// earlier meaning; Results has exactly one entry per input entry, in input
// order.
type UserInviteBulkResponse struct {
	BatchID  string              `json:"batchId"`
	Invited  int                 `json:"invited"`
	Skipped  int                 `json:"skipped"`
	Total    int                 `json:"total"`
	Complete bool                `json:"complete"`
	Counts   InviteBatchCounts   `json:"counts"`
	Results  []InviteBatchResult `json:"results"`
}

// InviteBatchProjectRefusal is one refused project entry (or one refused
// role in it) of a 403 batch_projects_not_grantable response.
type InviteBatchProjectRefusal struct {
	Index            int                    `json:"index"`
	ProjectID        string                 `json:"projectId"`
	Reason           string                 `json:"reason"`
	RoleDefinitionID string                 `json:"roleDefinitionId,omitempty"`
	DenialCode       string                 `json:"denialCode,omitempty"`
	Details          map[string]interface{} `json:"details,omitempty"`
}

// --- Decoded request ---

// inviteBatchEntry is one decoded emails[] entry, before validation.
type inviteBatchEntry struct {
	raw  string
	note string
}

// inviteBatchProject is one decoded projects[] entry.
type inviteBatchProject struct {
	ProjectID         string
	RoleDefinitionIDs []string
}

// inviteBatchRequest is a decoded batch request.
type inviteBatchRequest struct {
	entries  []inviteBatchEntry
	projects []inviteBatchProject
}

// inviteBatchRequestError is a request-level refusal (400 or 422). It is
// written before any write and records no audit event.
type inviteBatchRequestError struct {
	status  int
	code    string
	message string
	details map[string]interface{}
}

func (e *inviteBatchRequestError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.message, e.details)
}

func inviteBatchBadRequest(message string, field string) *inviteBatchRequestError {
	e := &inviteBatchRequestError{status: http.StatusBadRequest, code: ErrCodeInvalidRequest, message: message}
	if field != "" {
		e.details = map[string]interface{}{"field": field}
	}
	return e
}

// inviteBatchUnknownField returns the first field of fields (sorted) that
// allowed does not list, or "".
func inviteBatchUnknownField(fields map[string]json.RawMessage, allowed map[string]bool) string {
	var unknown []string
	for name := range fields {
		if !allowed[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	sort.Strings(unknown)
	return unknown[0]
}

// inviteBatchString decodes an optional string field of an object; a
// missing field and null both give "".
func inviteBatchString(fields map[string]json.RawMessage, name, path string) (string, *inviteBatchRequestError) {
	raw, ok := fields[name]
	if !ok || isJSONNull(raw) {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", inviteBatchBadRequest(fmt.Sprintf("invalid %s: must be a string", path), path)
	}
	return v, nil
}

// decodeInviteBatchJSON strictly decodes and validates a JSON batch
// request (design §3.2). Unknown fields at every level, wrong types and
// any cap violation are refused; role-like top-level fields give 422.
func decodeInviteBatchJSON(r *http.Request) (*inviteBatchRequest, *inviteBatchRequestError) {
	if r.Body == nil {
		return nil, inviteBatchBadRequest("invalid request body: a JSON object is required", "")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, inviteBulkMaxBodyBytes+1))
	if err != nil {
		return nil, inviteBatchBadRequest("invalid request body", "")
	}
	if len(body) > inviteBulkMaxBodyBytes {
		return nil, inviteBatchBadRequest("request body too large", "")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, inviteBatchBadRequest("invalid request body: a JSON object is required", "")
	}
	fields, err := decodeStrictJSONObject(trimmed)
	if err != nil {
		return nil, inviteBatchBadRequest("invalid request body: "+err.Error(), "")
	}

	// Role-like fields first, so they get the specific 422 rather than the
	// unknown-field 400.
	for _, name := range inviteBulkRoleLikeFields {
		if raw, ok := fields[name]; ok && !isJSONNull(raw) {
			return nil, &inviteBatchRequestError{status: http.StatusUnprocessableEntity, code: ErrCodeUnprocessable,
				message: "hub roles cannot be set on an invite; the role is assigned at first sign-in",
				details: map[string]interface{}{"field": name, "reason": provisionReasonRoleNotSupported}}
		}
	}
	topAllowed := map[string]bool{"emails": true, "projects": true}
	for _, name := range inviteBulkRoleLikeFields {
		topAllowed[name] = true // only null reaches here
	}
	if name := inviteBatchUnknownField(fields, topAllowed); name != "" {
		return nil, inviteBatchBadRequest(fmt.Sprintf("unknown field %q", name), name)
	}

	req := &inviteBatchRequest{}

	// emails
	var rawEmails []json.RawMessage
	if raw, ok := fields["emails"]; ok && !isJSONNull(raw) {
		rawEmails, err = decodeStrictArray(raw)
		if err != nil {
			return nil, inviteBatchBadRequest("invalid emails: "+err.Error(), "emails")
		}
	}
	if len(rawEmails) == 0 {
		return nil, inviteBatchBadRequest("no emails provided", "emails")
	}
	if len(rawEmails) > inviteBulkMaxEmails {
		return nil, inviteBatchBadRequest(fmt.Sprintf("maximum %d emails per bulk invite", inviteBulkMaxEmails), "emails")
	}
	entryAllowed := map[string]bool{"email": true, "note": true}
	for i, raw := range rawEmails {
		path := fmt.Sprintf("emails[%d]", i)
		trimmedEntry := bytes.TrimSpace(raw)
		if len(trimmedEntry) == 0 || trimmedEntry[0] != '{' {
			return nil, inviteBatchBadRequest(fmt.Sprintf("invalid %s: a JSON object is required", path), path)
		}
		entry, err := decodeStrictJSONObject(trimmedEntry)
		if err != nil {
			return nil, inviteBatchBadRequest(fmt.Sprintf("invalid %s: %v", path, err), path)
		}
		if name := inviteBatchUnknownField(entry, entryAllowed); name != "" {
			return nil, inviteBatchBadRequest(fmt.Sprintf("unknown field %q", name), path+"."+name)
		}
		email, rerr := inviteBatchString(entry, "email", path+".email")
		if rerr != nil {
			return nil, rerr
		}
		note, rerr := inviteBatchString(entry, "note", path+".note")
		if rerr != nil {
			return nil, rerr
		}
		req.entries = append(req.entries, inviteBatchEntry{raw: email, note: note})
	}

	// projects
	var rawProjects []json.RawMessage
	if raw, ok := fields["projects"]; ok && !isJSONNull(raw) {
		rawProjects, err = decodeStrictArray(raw)
		if err != nil {
			return nil, inviteBatchBadRequest("invalid projects: "+err.Error(), "projects")
		}
	}
	if len(rawProjects) > inviteBulkMaxProjects {
		return nil, inviteBatchBadRequest(fmt.Sprintf("maximum %d projects per bulk invite", inviteBulkMaxProjects), "projects")
	}
	projectAllowed := map[string]bool{"projectId": true, "roleDefinitionIds": true}
	seenProject := make(map[string]bool, len(rawProjects))
	for i, raw := range rawProjects {
		path := fmt.Sprintf("projects[%d]", i)
		trimmedEntry := bytes.TrimSpace(raw)
		if len(trimmedEntry) == 0 || trimmedEntry[0] != '{' {
			return nil, inviteBatchBadRequest(fmt.Sprintf("invalid %s: a JSON object is required", path), path)
		}
		entry, err := decodeStrictJSONObject(trimmedEntry)
		if err != nil {
			return nil, inviteBatchBadRequest(fmt.Sprintf("invalid %s: %v", path, err), path)
		}
		if name := inviteBatchUnknownField(entry, projectAllowed); name != "" {
			return nil, inviteBatchBadRequest(fmt.Sprintf("unknown field %q", name), path+"."+name)
		}
		projectID, rerr := inviteBatchString(entry, "projectId", path+".projectId")
		if rerr != nil {
			return nil, rerr
		}
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			return nil, inviteBatchBadRequest(path+".projectId is required", path+".projectId")
		}
		if seenProject[projectID] {
			return nil, inviteBatchBadRequest("duplicate projectId "+strconv.Quote(projectID), path+".projectId")
		}
		seenProject[projectID] = true

		rolesPath := path + ".roleDefinitionIds"
		var rawRoles []json.RawMessage
		if rr, ok := entry["roleDefinitionIds"]; ok && !isJSONNull(rr) {
			rawRoles, err = decodeStrictArray(rr)
			if err != nil {
				return nil, inviteBatchBadRequest(fmt.Sprintf("invalid %s: %v", rolesPath, err), rolesPath)
			}
		}
		if len(rawRoles) == 0 || len(rawRoles) > inviteBulkMaxRolesPerProject {
			return nil, inviteBatchBadRequest(fmt.Sprintf("%s must list 1 to %d role definition IDs", rolesPath, inviteBulkMaxRolesPerProject), rolesPath)
		}
		var roleIDs []string
		seenRole := make(map[string]bool, len(rawRoles))
		for _, rr := range rawRoles {
			var id string
			if err := json.Unmarshal(rr, &id); err != nil || strings.TrimSpace(id) == "" {
				return nil, inviteBatchBadRequest(rolesPath+" must contain non-empty strings", rolesPath)
			}
			id = strings.TrimSpace(id)
			if !seenRole[id] {
				seenRole[id] = true
				roleIDs = append(roleIDs, id)
			}
		}
		req.projects = append(req.projects, inviteBatchProject{ProjectID: projectID, RoleDefinitionIDs: roleIDs})
	}

	if len(req.entries)*len(req.projects) > inviteBulkMaxMemberships {
		return nil, inviteBatchBadRequest("too many memberships in one batch; split the list", "projects")
	}
	return req, nil
}

// decodeInviteBatchMultipart reads the CSV (multipart) form: only the file
// field, no projects (design §3.2).
func decodeInviteBatchMultipart(w http.ResponseWriter, r *http.Request) (*inviteBatchRequest, *inviteBatchRequestError) {
	r.Body = http.MaxBytesReader(w, r.Body, inviteBulkMaxMultipartBytes)
	if err := r.ParseMultipartForm(inviteBulkMaxMultipartBytes); err != nil {
		return nil, inviteBatchBadRequest("failed to parse multipart form", "")
	}
	if r.MultipartForm != nil {
		var extra []string
		for name := range r.MultipartForm.Value {
			extra = append(extra, name)
		}
		for name := range r.MultipartForm.File {
			if name != "file" {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		for _, name := range extra {
			if name == "projects" {
				return nil, inviteBatchBadRequest("projects require a JSON request", "projects")
			}
		}
		if len(extra) > 0 {
			return nil, inviteBatchBadRequest(fmt.Sprintf("unknown form field %q", extra[0]), extra[0])
		}
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return nil, inviteBatchBadRequest("file field is required", "file")
	}
	defer func() { _ = file.Close() }()

	parsed, err := parseCSVEmails(file)
	if err != nil {
		return nil, inviteBatchBadRequest(err.Error(), "file")
	}
	if len(parsed) == 0 {
		return nil, inviteBatchBadRequest("no emails provided", "file")
	}
	if len(parsed) > inviteBulkMaxEmails {
		return nil, inviteBatchBadRequest(fmt.Sprintf("maximum %d emails per bulk invite", inviteBulkMaxEmails), "file")
	}
	req := &inviteBatchRequest{}
	for _, p := range parsed {
		req.entries = append(req.entries, inviteBatchEntry{raw: p.Email, note: p.Note})
	}
	return req, nil
}

// --- Per-address validation (pure, design §3.4) ---

// inviteBatchAddress is one input entry after validation. result is final
// for an invalid or duplicate entry; otherwise email and note are the
// values the user step uses.
type inviteBatchAddress struct {
	result  InviteBatchResult
	email   string
	note    *string
	pending bool // passed validation and dedup: apply it
}

// inviteBatchAddressRefusal returns the reason a normalised-or-raw
// address is refused before any write, or "": the reserved test-identity
// domains (every kind) and every address the project grantee check
// refuses by its shape (granteeAddressRefusal, shared with
// testAdminGranteeRefusal). An address that check would refuse therefore
// never reaches SetMemberRoles, where its "forbidden" code would stop the
// whole project (batchStopsProject).
func inviteBatchAddressRefusal(address string) string {
	if isReservedTestIdentityEmail(address) {
		return provisionReasonReservedTestIdentity
	}
	if granteeAddressRefusal(address) != "" {
		return provisionReasonEmailNotAllowed
	}
	return ""
}

// truncateInviteInput returns raw cut to at most
// inviteBulkInputEchoMaxBytes bytes of valid UTF-8.
func truncateInviteInput(raw string) string {
	s := strings.ToValidUTF8(raw, "�")
	if len(s) <= inviteBulkInputEchoMaxBytes {
		return s
	}
	cut := inviteBulkInputEchoMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// validateInviteBatchAddresses runs the pure per-address checks and the
// dedup, in input order. Only an entry that passes every check registers
// as the first occurrence of its address.
func (s *Server) validateInviteBatchAddresses(actorID string, entries []inviteBatchEntry) []inviteBatchAddress {
	out := make([]inviteBatchAddress, len(entries))
	first := make(map[string]int, len(entries))
	for i, e := range entries {
		a := &out[i]
		a.result.Index = i
		invalid := func(reason string) {
			a.result.Status = inviteBatchStatusInvalid
			a.result.Reason = reason
			a.result.Input = truncateInviteInput(e.raw)
		}
		lowered := strings.TrimSpace(strings.ToLower(e.raw))
		if inviteBatchAddressRefusal(lowered) == provisionReasonReservedTestIdentity {
			invalid(provisionReasonReservedTestIdentity)
			continue
		}
		email, err := NormalizeInviteEmail(e.raw)
		if err != nil {
			invalid(inviteBatchReasonInvalidEmail)
			continue
		}
		a.result.Email = email
		if reason := inviteBatchAddressRefusal(email); reason != "" {
			invalid(reason)
			continue
		}
		if s.proxyDrivers.contains(email) {
			logProxyDriverCreateRefusal("bulk invite", actorID, email)
			invalid(provisionReasonEmailNotAllowed)
			continue
		}
		if e.note != "" {
			if utf8.RuneCountInString(e.note) > provisionNoteMaxRunes {
				invalid(inviteBatchReasonNoteTooLong)
				continue
			}
			if hasControlChar(e.note, true) {
				invalid(inviteBatchReasonNoteInvalidCharacters)
				continue
			}
		}
		if j, dup := first[email]; dup {
			jj := j
			a.result.Status = inviteBatchStatusDuplicate
			a.result.Reason = inviteBatchReasonDuplicateInBatch
			a.result.DuplicateOf = &jj
			continue
		}
		first[email] = i
		a.email = email
		if e.note != "" {
			note := e.note
			a.note = &note
		}
		a.pending = true
	}
	return out
}

// --- Project preflight (design §5.2) ---

// inviteBatchPreflight checks every project entry with the read-only
// functions the members PUT uses, and returns every refused entry. It
// never admits anything: see the comment at its call site.
func (s *Server) inviteBatchPreflight(ctx context.Context, actor UserIdentity, projects []inviteBatchProject) ([]InviteBatchProjectRefusal, error) {
	var refusals []InviteBatchProjectRefusal
	for i, p := range projects {
		notManageable := InviteBatchProjectRefusal{Index: i, ProjectID: p.ProjectID, Reason: inviteBatchReasonProjectNotManageable}
		// Access before existence, as on the members PUT, and the same
		// reason for both, so the batch does not tell whether a project the
		// caller cannot manage exists.
		if d := s.authzService.CheckAccess(ctx, actor, Resource{Type: "project", ID: p.ProjectID}, ActionManage); !d.Allowed {
			refusals = append(refusals, notManageable)
			continue
		}
		if _, err := s.store.GetProject(ctx, p.ProjectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				refusals = append(refusals, notManageable)
				continue
			}
			return nil, fmt.Errorf("get project %s: %w", p.ProjectID, err)
		}
		roles, err := s.membershipService.AssignableRoles(ctx, actor, p.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("assignable roles for project %s: %w", p.ProjectID, err)
		}
		byID := make(map[string]AssignableProjectRole, len(roles))
		for _, r := range roles {
			byID[r.ID] = r
		}
		for _, id := range p.RoleDefinitionIDs {
			r, ok := byID[id]
			switch {
			case !ok:
				// Unknown, system-scoped, or not a membership role.
				refusals = append(refusals, InviteBatchProjectRefusal{Index: i, ProjectID: p.ProjectID,
					Reason: inviteBatchReasonRoleNotAssignable, RoleDefinitionID: id})
			case !r.Grantable:
				refusals = append(refusals, InviteBatchProjectRefusal{Index: i, ProjectID: p.ProjectID,
					Reason: inviteBatchReasonRoleNotGrantable, RoleDefinitionID: id,
					DenialCode: r.DenialCode, Details: r.Details})
			}
		}
	}
	return refusals, nil
}

// --- Stop policy (design §5.3) ---

// batchMembershipStopPolicy maps a SetMemberRoles denial code to whether
// the batch stops calling SetMemberRoles for that project (true) or goes
// on with the next address (false). Design §5.3 gives the reason for each
// row. TestInviteBatchStopPolicy_CoversAllSetMemberRolesCodes fails when
// SetMemberRoles gains a code this table does not classify.
//
// Read it ONLY through batchStopsProject, which stops on an unknown code;
// a plain map lookup would continue on one.
var batchMembershipStopPolicy = map[string]bool{
	ErrCodeMembershipCredentialInsufficient: true,
	ErrCodeForbidden:                        true,
	ErrCodeRoleAssignmentForbidden:          true,
	ErrCodeTargetRoleProtected:              true,
	ErrCodeInvalidRoleSet:                   true,
	ErrCodeEmptyRoleSet:                     true,
	ErrCodeMembershipChanged:                false, // both causes; principal_roles_changed is classified first
	ErrCodePrincipalIneligible:              false,
	ErrCodeInvalidRequest:                   false,
	ErrCodeConflict:                         false,
	ErrCodeInternalError:                    false, // but see inviteBatchInternalErrorStopAfter
	ErrCodeNotFound:                         true,
	ErrCodeLastOwner:                        true,
	ErrCodeConstraintAdminLockout:           true,
}

// batchStopsProject reports whether a SetMemberRoles denial code stops the
// project for the rest of the run. An unclassified code stops: it is most
// likely an actor or role check, which is the same for every address, and
// stopping never writes (a re-run retries it).
func batchStopsProject(code string) bool {
	stop, known := batchMembershipStopPolicy[code]
	return !known || stop
}

// --- Apply (design §4) ---

// inviteBatchMemberSetter is the membership write the batch uses: the
// members PUT engine, ProjectMembershipService.SetMemberRoles. Tests wrap
// it to count calls.
type inviteBatchMemberSetter interface {
	SetMemberRoles(ctx context.Context, req SetMemberRolesRequest) (*SetMemberRolesResult, *MembershipDecision)
}

// errInviteBatchTransportIdentity is returned by inviteBatchUserStep for
// an address that is a transport identity. Validation refuses such an
// address first, so it is not expected here.
var errInviteBatchTransportIdentity = errors.New("bulk invite: address is a transport identity")

// inviteBatchUserStep creates the pending user for one address in its own
// transaction (design §4.2 A). A unique-index race re-reads and
// classifies; a transient store conflict is retried once.
func (s *Server) inviteBatchUserStep(ctx context.Context, actor UserIdentity, email string, note *string) (*store.User, PendingOutcome, error) {
	if s.proxyDrivers.contains(email) {
		return nil, 0, errInviteBatchTransportIdentity
	}
	spec := PendingUserSpec{Email: email, Note: note, InvitedBy: actor.ID()}
	var u *store.User
	var outcome PendingOutcome
	attempt := func() error {
		return s.store.WithTx(ctx, func(tx store.Store) error {
			var err error
			u, outcome, err = createPendingUserTx(ctx, tx, spec)
			return err
		})
	}
	err := attempt()
	if errors.Is(err, store.ErrTransient) {
		err = attempt()
	}
	if errors.Is(err, store.ErrAlreadyExists) {
		u, outcome, err = lookupPendingUser(ctx, s.store, email, spec)
	}
	if err != nil {
		return nil, 0, err
	}
	return u, outcome, nil
}

// inviteBatchProjectState is the per-project run state: whether the
// project is stopped (and with which code and cause), and the current run
// of consecutive internal_error results.
type inviteBatchProjectState struct {
	stopped        bool
	stopCode       string
	stopCause      string
	internalStreak int
	changed        bool
}

// inviteBatchRun is the outcome of the apply steps.
type inviteBatchRun struct {
	results           []InviteBatchResult
	counts            InviteBatchCounts
	membershipApplied int
	changedProjects   []string
}

// sameRoleSet reports whether a and b hold the same IDs, as sets.
func sameRoleSet(a, b []string) bool {
	as := make(map[string]bool, len(a))
	for _, id := range a {
		as[id] = true
	}
	bs := make(map[string]bool, len(b))
	for _, id := range b {
		bs[id] = true
	}
	if len(as) != len(bs) {
		return false
	}
	for id := range as {
		if !bs[id] {
			return false
		}
	}
	return true
}

// decisionRoleIDs reads details.currentRoleDefinitionIds of a decision.
func decisionRoleIDs(details map[string]interface{}) []string {
	switch v := details["currentRoleDefinitionIds"].(type) {
	case []string:
		return append([]string(nil), v...)
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func decisionCause(details map[string]interface{}) string {
	c, _ := details["cause"].(string)
	return c
}

// applyInviteBatchMembership runs one SetMemberRoles call for (user, p)
// unless p is stopped, and updates p's state (design §4.2 C, §5.3).
func applyInviteBatchMembership(ctx context.Context, members inviteBatchMemberSetter, actor UserIdentity, userID string, p inviteBatchProject, st *inviteBatchProjectState) InviteBatchMembership {
	m := InviteBatchMembership{ProjectID: p.ProjectID}
	if st.stopped {
		m.Status, m.Code, m.Cause = inviteBatchMembershipFailed, st.stopCode, st.stopCause
		return m
	}
	if ctx.Err() != nil {
		m.Status, m.Code = inviteBatchMembershipFailed, inviteBatchCodeNotProcessed
		return m
	}
	// The same request a members PUT builds, with ExpectedRoleIDs = []:
	// nothing derived from the preflight is passed in, and every check
	// runs again inside SetMemberRoles.
	result, decision := members.SetMemberRoles(ctx, SetMemberRolesRequest{
		ProjectID:       p.ProjectID,
		PrincipalType:   store.RoleBindingPrincipalUser,
		PrincipalID:     userID,
		Actor:           actor,
		DesiredRoleIDs:  p.RoleDefinitionIDs,
		ExpectedRoleIDs: &[]string{},
	})
	if decision == nil {
		st.internalStreak = 0
		if result != nil && result.Changed {
			m.Status = inviteBatchMembershipApplied
			st.changed = true
		} else {
			m.Status = inviteBatchMembershipAlreadyMember
		}
		return m
	}

	cause := decisionCause(decision.Details)
	if decision.DenialCode == ErrCodeMembershipChanged && cause == causePrincipalRolesChanged {
		st.internalStreak = 0
		current := decisionRoleIDs(decision.Details)
		if sameRoleSet(current, p.RoleDefinitionIDs) {
			m.Status = inviteBatchMembershipAlreadyMember
		} else {
			m.Status = inviteBatchMembershipConflict
			m.Code = inviteBatchCodeMemberWithOtherRoles
			m.CurrentRoleDefinitionIDs = current
		}
		return m
	}

	m.Status, m.Code, m.Cause = inviteBatchMembershipFailed, decision.DenialCode, cause
	stop := batchStopsProject(decision.DenialCode)
	if decision.DenialCode == ErrCodeInternalError {
		st.internalStreak++
		if st.internalStreak >= inviteBatchInternalErrorStopAfter {
			stop = true
		}
	} else {
		st.internalStreak = 0
	}
	if stop {
		st.stopped, st.stopCode, st.stopCause = true, decision.DenialCode, cause
	}
	return m
}

// runInviteBatch applies every validated address in input order: the user
// row, then (for a created or already invited row) each project
// membership. ctx bounds the run: addresses not started before it ends are
// failed/not_processed, and a re-run completes them.
func (s *Server) runInviteBatch(ctx context.Context, actor UserIdentity, batchID string, addrs []inviteBatchAddress, projects []inviteBatchProject, members inviteBatchMemberSetter, detail bool) *inviteBatchRun {
	run := &inviteBatchRun{results: make([]InviteBatchResult, len(addrs))}
	states := make([]inviteBatchProjectState, len(projects))
	skipped := func() []InviteBatchMembership {
		if len(projects) == 0 {
			return nil
		}
		out := make([]InviteBatchMembership, len(projects))
		for j, p := range projects {
			out[j] = InviteBatchMembership{ProjectID: p.ProjectID, Status: inviteBatchMembershipSkipped, Code: inviteBatchCodeAddressNotInvited}
		}
		return out
	}

	for i := range addrs {
		a := &addrs[i]
		res := a.result
		if !a.pending {
			res.Memberships = skipped()
			run.results[i] = res
			continue
		}
		res.Email = a.email
		if ctx.Err() != nil {
			res.Status, res.Reason = inviteBatchStatusFailed, inviteBatchReasonNotProcessed
			res.Memberships = skipped()
			run.results[i] = res
			continue
		}

		u, outcome, err := s.inviteBatchUserStep(ctx, actor, a.email, a.note)
		if err != nil {
			slog.ErrorContext(ctx, "bulk invite: failed to create invited user", "batch_id", batchID, "email", a.email, "error", err)
			res.Status, res.Reason = inviteBatchStatusFailed, inviteBatchReasonInternalError
			if ctx.Err() != nil {
				res.Reason = inviteBatchReasonNotProcessed
			}
			res.Memberships = skipped()
			run.results[i] = res
			continue
		}

		applyMemberships := false
		switch outcome {
		case PendingCreated:
			res.Status = inviteBatchStatusCreated
			res.UserID = u.ID
			applyMemberships = true
			LogInviteAudit(ctx, s.auditLogger, InviteAuditUserInvited, a.email, "", actor.ID(), actor.Email(),
				map[string]string{"user_id": u.ID, "batch_id": batchID})
		case PendingExistingIdentical, PendingExistingDifferent:
			res.Status, res.Reason = inviteBatchStatusAlreadyInvited, provisionReasonPendingUserExists
			if detail {
				res.UserID = u.ID
			}
			applyMemberships = true
		case PendingExistingSuspended:
			res.Status, res.Reason = inviteBatchStatusSuspended, provisionReasonSuspendedUserExists
			if detail {
				res.UserID = u.ID
			}
		default:
			// An active row never carries userId, for any caller.
			res.Status, res.Reason = inviteBatchStatusAlreadyActive, provisionReasonUserExists
		}
		if !detail && res.Status != inviteBatchStatusCreated {
			res.Status, res.Reason, res.UserID = inviteBatchStatusExists, provisionReasonUserExists, ""
		}

		if len(projects) > 0 {
			if applyMemberships {
				res.Memberships = make([]InviteBatchMembership, len(projects))
				for j, p := range projects {
					res.Memberships[j] = applyInviteBatchMembership(ctx, members, actor, u.ID, p, &states[j])
				}
			} else {
				res.Memberships = skipped()
			}
		}
		run.results[i] = res
	}

	for i, res := range run.results {
		switch res.Status {
		case inviteBatchStatusCreated:
			run.counts.Created++
		case inviteBatchStatusAlreadyInvited:
			run.counts.AlreadyInvited++
		case inviteBatchStatusAlreadyActive:
			run.counts.AlreadyActive++
		case inviteBatchStatusSuspended:
			run.counts.Suspended++
		case inviteBatchStatusExists:
			run.counts.Exists++
		case inviteBatchStatusInvalid:
			run.counts.Invalid++
		case inviteBatchStatusDuplicate:
			run.counts.Duplicate++
		case inviteBatchStatusFailed:
			run.counts.Failed++
		default:
			slog.ErrorContext(ctx, "bulk invite: result without a known status", "batch_id", batchID, "index", i, "status", res.Status)
			run.counts.Failed++
		}
		for _, m := range res.Memberships {
			switch m.Status {
			case inviteBatchMembershipFailed:
				run.counts.MembershipFailed++
			case inviteBatchMembershipApplied:
				run.membershipApplied++
			}
		}
	}
	for j, p := range projects {
		if states[j].changed {
			run.changedProjects = append(run.changedProjects, p.ProjectID)
		}
	}
	return run
}

// --- Audit (design §6) ---

func inviteBatchProjectIDs(projects []inviteBatchProject) string {
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ProjectID
	}
	return strings.Join(ids, ",")
}

// logInviteBatchRun writes the one user_invited_bulk record of a run that
// passed the preflight.
func (s *Server) logInviteBatchRun(ctx context.Context, actor UserIdentity, batchID string, projects []inviteBatchProject, run *inviteBatchRun, complete bool) {
	logger := s.auditLogger
	if logger == nil {
		return
	}
	c := run.counts
	details := map[string]string{
		"batch_id":           batchID,
		"already_invited":    strconv.Itoa(c.AlreadyInvited),
		"already_active":     strconv.Itoa(c.AlreadyActive),
		"suspended":          strconv.Itoa(c.Suspended),
		"exists":             strconv.Itoa(c.Exists),
		"invalid":            strconv.Itoa(c.Invalid),
		"duplicate":          strconv.Itoa(c.Duplicate),
		"failed":             strconv.Itoa(c.Failed),
		"membership_applied": strconv.Itoa(run.membershipApplied),
		"membership_failed":  strconv.Itoa(c.MembershipFailed),
		"projects":           inviteBatchProjectIDs(projects),
	}
	event := &InviteAuditEvent{
		EventType:  InviteAuditUserInvitedBulk,
		ActorID:    actor.ID(),
		ActorEmail: actor.Email(),
		Success:    complete,
		Count:      c.Created,
		Timestamp:  time.Now(),
		Details:    details,
	}
	if err := logger.LogInviteAuditEvent(ctx, event); err != nil {
		slog.WarnContext(ctx, "bulk invite: the aggregate audit event failed", "batch_id", batchID, "error", err)
	}
}

// logInviteBatchRefused writes the one user_invited_bulk record of a run
// the preflight refused (design §6, v2). No user row or binding exists for
// it.
func (s *Server) logInviteBatchRefused(ctx context.Context, actor UserIdentity, batchID string, total int, refusals []InviteBatchProjectRefusal) {
	logger := s.auditLogger
	if logger == nil {
		return
	}
	var ids []string
	seen := map[string]bool{}
	items := make([]string, 0, len(refusals))
	for _, r := range refusals {
		if !seen[r.ProjectID] {
			seen[r.ProjectID] = true
			ids = append(ids, r.ProjectID)
		}
		item := fmt.Sprintf("%d:%s:%s", r.Index, r.ProjectID, r.Reason)
		if r.DenialCode != "" {
			item += ":" + r.DenialCode
		}
		items = append(items, item)
	}
	event := &InviteAuditEvent{
		EventType:  InviteAuditUserInvitedBulk,
		ActorID:    actor.ID(),
		ActorEmail: actor.Email(),
		Success:    false,
		FailReason: inviteBatchReasonProjectsNotGrantable,
		Count:      0,
		Timestamp:  time.Now(),
		Details: map[string]string{
			"batch_id":        batchID,
			"total":           strconv.Itoa(total),
			"refused_entries": strconv.Itoa(len(refusals)),
			"projects":        strings.Join(ids, ","),
			"refusals":        strings.Join(items, ";"),
		},
	}
	if err := logger.LogInviteAuditEvent(ctx, event); err != nil {
		slog.WarnContext(ctx, "bulk invite: the refused-run audit event failed", "batch_id", batchID, "error", err)
	}
}
