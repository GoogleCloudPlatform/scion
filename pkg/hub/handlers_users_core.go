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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// loadUserPreferences reads a user's preferences live from the store (no
// caching), for /auth/me on both the web server and the Hub API: a PATCH
// from another tab, device or client is visible on the very next load. A nil
// store or a store.ErrNotFound degrades to nil preferences rather than
// failing the request (the caller's response then falls back to the session
// or token fields alone, and the UI treats the display timezone as Auto).
// Any other store error also degrades, but is logged, so a broken store does
// not silently masquerade as "no preferences set".
func loadUserPreferences(ctx context.Context, st store.Store, uid string) *store.UserPreferences {
	if st == nil || uid == "" {
		return nil
	}
	dbUser, err := st.GetUser(ctx, uid)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "loadUserPreferences: store error reading user; degrading to no preferences",
				"user_id", uid, "error", err)
		}
		return nil
	}
	if dbUser == nil {
		return nil
	}
	return dbUser.Preferences
}

type ListUsersResponse struct {
	Users        []UserWithCapabilities `json:"users"`
	NextCursor   string                 `json:"nextCursor,omitempty"`
	TotalCount   int                    `json:"totalCount"`
	Capabilities *Capabilities          `json:"_capabilities,omitempty"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUsers(w, r)
	case http.MethodPost:
		s.handleProvisionUser(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	// A user access token lists users only with user.list at the hub
	// collection, and then sees only the users it may read: each row is
	// checked as a hub-level user record, so the token also needs
	// user.read.
	_, isToken := GetIdentityFromContext(ctx).(*ScopedUserIdentity)
	if isToken && !s.authorizeWithEvidence(w, r, Resource{Type: "user"}, ActionList, hubCollectionEvidence("user.list")) {
		return
	}

	filter := store.UserFilter{
		Role:   query.Get("role"),
		Status: query.Get("status"),
		Search: query.Get("search"),
	}

	listOptions := listOptionsFromQuery(query)
	listOptions.SortBy = query.Get("sort")
	listOptions.SortDir = query.Get("dir")
	result, err := s.store.ListUsers(ctx, filter, listOptions)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Compute per-item capabilities (users have no scope-level create action)
	identity := GetIdentityFromContext(ctx)
	users := make([]UserWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = userResource(&result.Items[i])
			if isToken {
				resources[i] = hubScopedResource("user", result.Items[i].ID)
			}
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "user")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			users = append(users, UserWithCapabilities{User: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			users = append(users, UserWithCapabilities{User: result.Items[i]})
		}
	}

	totalCount := result.TotalCount
	if identity != nil && len(users) < len(result.Items) {
		totalCount = len(users)
	}

	for i := range users {
		stripPreferencesForViewer(ctx, &users[i].User, users[i].Cap)
	}

	writeJSON(w, http.StatusOK, ListUsersResponse{
		Users:      users,
		NextCursor: result.NextCursor,
		TotalCount: totalCount,
	})
}

func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/users")

	if id == "" {
		NotFound(w, "User")
		return
	}

	// Sub-resource actions. Only the bare ID and the sub-routes named here
	// are resources; any other sub-path, including a further segment below
	// a known sub-route, answers 404 and never reaches the user handlers
	// below.
	switch action {
	case "":
		// The user itself: handled by the method switch below.
	case "revoke-sessions":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.revokeUserSessions(w, r, id)
		return
	case "delete-blockers":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		s.getUserDeleteBlockers(w, r, id)
		return
	case "deletion":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		s.requireExperiment(experiments.UserDeleteForceCascade, func(w http.ResponseWriter, r *http.Request) {
			s.getUserDeletion(w, r, id)
		})(w, r)
		return
	default:
		NotFound(w, "User resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getUser(w, r, id)
	case http.MethodPatch:
		s.updateUser(w, r, id)
	case http.MethodDelete:
		s.deleteUser(w, r, id)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

// revokeUserSessions increments the user's session generation, invalidating
// all existing cookie-based sessions for that user.
func (s *Server) revokeUserSessions(w http.ResponseWriter, r *http.Request, id string) {
	admin, ok := s.requireAdminAt(w, r, authzop.ReasonSessionRecovery, "users.sessions.revoke")
	if !ok {
		return
	}

	if err := s.store.IncrementSessionGeneration(r.Context(), id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: id})
	slog.Info("Admin revoked all sessions for user",
		"user_id", id,
		"admin_id", admin.ID(),
		"admin_email", admin.Email())
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := UserWithCapabilities{User: *user}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, userResource(user))
	}
	stripPreferencesForViewer(ctx, &resp.User, resp.Cap)

	writeJSON(w, http.StatusOK, resp)
}

// stripPreferencesForViewer clears u.Preferences in place unless the caller
// in ctx is that same user, or cap (the capability set already computed by
// the caller for this same resource — listUsers and getUser each compute it
// once per user) includes ActionUpdate. That is the same permission
// (user.update) that gates a cross-user PATCH, so read and write visibility
// of preferences agree, and this does not issue a second Decide call or
// duplicate its deny-audit record. Preferences (including the
// display-timezone field) are personal: a member listing or viewing another
// user must not see them (AC6).
func stripPreferencesForViewer(ctx context.Context, u *store.User, cap *Capabilities) {
	if u.Preferences == nil {
		return
	}
	if userIdentity, ok := GetIdentityFromContext(ctx).(UserIdentity); ok && userIdentity.ID() == u.ID {
		return
	}
	// capabilityAllows already treats a nil cap as "no actions allowed", so
	// this is a redundant, zero-risk guard, not a behavior change.
	if cap != nil && capabilityAllows(cap, ActionUpdate) {
		return
	}
	u.Preferences = nil
}

// ---------------------------------------------------------------------------
// Credential boundary enforcement (R4-C5)
//
// User mutation endpoints (PATCH, DELETE) and the deletion reads
// (delete-blockers, deletion) require an interactive session JWT or dev
// credential, or a user access token: PATCH for the fields
// updateUserTokenFields allows, the others with user:delete. Broker, agent
// and federation credentials are rejected at the boundary by
// requireSessionCredentialFor (session_only_gate.go), which reports the
// session-only reason.
// ---------------------------------------------------------------------------

// allowedMutationCredentials is the closed set of credential kinds permitted
// for user mutation endpoints.
var allowedMutationCredentials = map[CredentialKind]bool{
	CredentialKindInteractive: true,
	CredentialKindDev:         true,
}

// updateUserSessionOnlyReason is the session-only reason of PATCH
// /api/v1/users/{id}: INTERACTIVE_STATE when the path names the caller's
// own record, GOV_PENDING for any other record. It depends only on the
// path, so the credential check still runs before the body is read.
func updateUserSessionOnlyReason(ctx context.Context, id string) authzop.SessionOnlyReason {
	if identity := GetIdentityFromContext(ctx); identity != nil && identity.ID() == id {
		return authzop.ReasonInteractiveState
	}
	return authzop.ReasonGovernancePending
}

// updateUserCredential checks the credential of PATCH /api/v1/users/{id}
// and returns the acting user (sessionOrUserTokenCredential, with the
// path's session-only reason). A user access token is returned as token
// too; updateUserTokenFields checks its fields, and the caller checks its
// permissions on the hub-level user record.
func (s *Server) updateUserCredential(w http.ResponseWriter, ctx context.Context, id string) (actor UserIdentity, token *ScopedUserIdentity, ok bool) {
	return s.sessionOrUserTokenCredential(w, ctx, updateUserSessionOnlyReason(ctx, id))
}

// sessionOrUserTokenCredential admits an interactive session or dev
// credential of a user (requireSessionCredentialFor), or a user access
// token: a token identity whose request carries the user access token
// credential kind. It returns the acting user, and for a token the token
// identity too, so the caller can check the token's selectors and
// boundary. A token identity with any other credential kind, and every
// other credential kind, is refused with the session-only reason.
func (s *Server) sessionOrUserTokenCredential(w http.ResponseWriter, ctx context.Context, reason authzop.SessionOnlyReason) (actor UserIdentity, token *ScopedUserIdentity, ok bool) {
	identity := GetIdentityFromContext(ctx)
	scoped, isToken := identity.(*ScopedUserIdentity)
	if !isToken || isNilIdentity(identity) {
		actor, ok = s.requireSessionCredentialFor(w, ctx, reason)
		return actor, nil, ok
	}
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return nil, nil, false
	}
	if kind := requestCredentialKind(ctx, identity); kind != CredentialKindUAT {
		writeSessionOnlyDenial(w, ErrCodeForbidden,
			fmt.Sprintf("this operation requires an interactive session; credential kind %q is not allowed", kind),
			reason)
		return nil, nil, false
	}
	return scoped, scoped, true
}

// userCrossProfileDenialMessage is the refusal message of a PATCH of
// another user's profile without user.update.
const userCrossProfileDenialMessage = "requires user.update permission to modify another user's profile"

// userPatchTokenRefusalMessage is the refusal message of a PATCH of
// another user by a token that may use none of user.update, user.suspend
// and user.promote on that user.
const userPatchTokenRefusalMessage = "requires user.update, user.suspend or user.promote permission to modify another user"

// tokenMayUpdateUser reports whether the token identity actor may use
// user.update, user.suspend or user.promote on the hub-level record of
// user id. Each
// permission is one authorization decision (Decide, which audits a deny),
// evaluated on the token identity, so the holder's current authority and
// the token's selectors and boundary all apply. The decision uses only the
// path id: it reads no user record and no request body, so its outcome is
// the same whether or not user id exists.
func (s *Server) tokenMayUpdateUser(ctx context.Context, actor UserIdentity, id string) bool {
	target := hubScopedResource("user", id)
	for _, p := range []struct {
		action     Action
		permission string
	}{
		{Action("update"), "user.update"},
		{Action("suspend"), "user.suspend"},
		{Action("promote"), "user.promote"},
	} {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   target,
			Action:     p.action,
			Permission: p.permission,
		})
		if decision.Allowed {
			return true
		}
	}
	return false
}

// writeUserPatchTokenRefusal writes the refusal of PATCH
// /api/v1/users/{id} for a token that may use none of user.update,
// user.suspend and user.promote on another user's id. Its status, code, message and
// details are the same for every such token and every id.
func writeUserPatchTokenRefusal(w http.ResponseWriter) {
	writeForbiddenStructured(w, userPatchTokenRefusalMessage, "user", Action("update"))
}

// updateUserTokenFields applies the field rules of PATCH
// /api/v1/users/{id} for a user access token, before any permission check
// or write. It writes the refusal and returns false when the path names the
// token holder's own record and the body names a profile field, or no
// status field: the caller's own record is session-only
// (INTERACTIVE_STATE), its role included.
//
// A status field on the holder's own record passes here and is refused by
// the self-status guard after its user.suspend check. A role field on
// another user's record passes here and is checked by
// checkUserPromotePermission.
func updateUserTokenFields(w http.ResponseWriter, updates userPatchPayload, prefsPatch *userPreferencesPatch, isSelf bool) bool {
	profile := updates.DisplayName != nil || prefsPatch != nil || updates.Status == nil
	if isSelf && profile {
		writeSessionOnlyDenial(w, ErrCodeForbidden,
			"changing your own profile requires an interactive session",
			authzop.ReasonInteractiveState)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/users/{id} — per-field permission enforcement (R3/R4)
//
// Each mutable field category requires a distinct permission:
//   - role:                 user.promote + CanDelegate(super-admin)
//   - status:               user.suspend
//   - displayName/prefs:    user.update (self-service for own record)
//
// A user access token is checked on the hub-level user record
// (hubScopedResource): status needs user:suspend, another user's profile
// needs user:update, role needs user:promote, and a token PATCH that names
// neither status nor role is checked as a profile update. The role field
// keeps CanDelegate for super-admin bindings, which a token never passes
// (enforceUATDelegation denies system-scoped grants), and the
// last-super-admin, self-demotion, invited-user and binding-drift guards.
// A token PATCH that changes its holder's own record other than its status
// is refused (updateUserTokenFields). A token PATCH of another user by a
// token that may use none of user:update, user:suspend and user:promote
// gets one refusal for every id (writeUserPatchTokenRefusal).
//
// A mixed PATCH must hold ALL required permissions before any write.
// Unknown JSON fields are rejected. ALL mutations (bindings + User record +
// audit) execute in a single atomic transaction (R4-C1). Audit records are
// synchronous and transactional (R4-C3).
// ---------------------------------------------------------------------------

// userPatchPayload is the strict set of allowed fields for PATCH /api/v1/users/{id}.
type userPatchPayload struct {
	DisplayName *string `json:"displayName,omitempty"`
	Role        *string `json:"role,omitempty"`
	Status      *string `json:"status,omitempty"`
}

// userPreferencesPatch is a per-key partial update to store.UserPreferences.
// A nil field means "leave unchanged"; a non-nil field (including a pointer
// to "") is applied verbatim, so an explicit "" clears that preference.
type userPreferencesPatch struct {
	DefaultTemplate *string
	DefaultProfile  *string
	Theme           *string
	Timezone        *string
}

// decodeStringPref unmarshals one preferences sub-field's raw JSON value
// into a string, for the per-key preferences PATCH merge. A JSON null is a
// no-op onto the freshly zero-valued result, so it decodes to "" — the same
// as an explicit "" (both clear the preference). A non-string JSON value
// (e.g. a number or object) is a decode error, which the caller reports as a
// 400.
func decodeStringPref(key string, rv json.RawMessage) (string, error) {
	var v string
	if err := json.Unmarshal(rv, &v); err != nil {
		return "", fmt.Errorf("invalid preferences.%s: %w", key, err)
	}
	return v, nil
}

// validateUserTimezone validates a user display-timezone preference value.
// "" means Auto (the browser-detected zone) and is always valid.
//
// Delegates the actual check to validateIANATimezone (timezone_validate.go),
// shared with the hub-wide agent_defaults.default_timezone validator
// (admin_settings.go's validateDefaultTimezone), so the two can't drift.
// Each validator keeps its own wrapping here, because the right message
// differs: this one points users at "" for Auto, which means nothing for
// the hub-wide default.
func validateUserTimezone(tz string) error {
	if tz == "" {
		return nil
	}
	if err := validateIANATimezone(tz); err != nil {
		if errors.Is(err, errNonPortableTimezone) {
			return fmt.Errorf("timezone %q is not allowed; use an IANA zone name, or \"\" for Auto", tz)
		}
		return fmt.Errorf("invalid timezone %q: %v", tz, err)
	}
	return nil
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Credential boundary + identity check (R4-C5).
	actor, token, ok := s.updateUserCredential(w, ctx, id)
	if !ok {
		return
	}
	// A token on another user's id needs user.update, user.suspend or
	// user.promote on that user before the record is read; otherwise it
	// gets one fixed refusal for every id and body.
	if token != nil && actor.ID() != id && !s.tokenMayUpdateUser(ctx, actor, id) {
		writeUserPatchTokenRefusal(w)
		return
	}

	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Parse with strict field rejection: decode into raw map to check for
	// unknown fields, then decode into the typed struct.
	var rawFields map[string]json.RawMessage
	if err := readJSON(r, &rawFields); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	allowedFields := map[string]bool{
		"displayName": true, "role": true, "status": true, "preferences": true,
	}
	for field := range rawFields {
		if !allowedFields[field] {
			BadRequest(w, fmt.Sprintf("unknown field %q; allowed fields are displayName, role, status, preferences", field))
			return
		}
	}

	// Re-parse into typed struct.
	var updates userPatchPayload
	var prefsPatch *userPreferencesPatch
	for field, raw := range rawFields {
		switch field {
		case "displayName":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid displayName: "+err.Error())
				return
			}
			updates.DisplayName = &v
		case "role":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid role: "+err.Error())
				return
			}
			updates.Role = &v
		case "status":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid status: "+err.Error())
				return
			}
			updates.Status = &v
		case "preferences":
			// Decode as a raw map, not the typed struct, so that an absent
			// key (leave unchanged) can be told apart from an explicit ""
			// (clear). The PATCH merges per-key onto the stored preferences
			// rather than replacing the whole struct.
			var rawPrefs map[string]json.RawMessage
			if err := json.Unmarshal(raw, &rawPrefs); err != nil {
				BadRequest(w, "invalid preferences: "+err.Error())
				return
			}
			// hasFields tracks whether rawPrefs contained at least one
			// recognized key. An empty object, a top-level null (which
			// decodes to a nil rawPrefs and an empty loop below) and a body
			// containing only unknown keys must all be true no-ops: they
			// must not set prefsPatch, so they neither force a DB write nor
			// initialize an empty store.UserPreferences record for a user
			// that had none.
			patch := &userPreferencesPatch{}
			var hasFields bool
			for key, rv := range rawPrefs {
				switch key {
				case "defaultTemplate":
					v, err := decodeStringPref(key, rv)
					if err != nil {
						BadRequest(w, err.Error())
						return
					}
					patch.DefaultTemplate = &v
					hasFields = true
				case "defaultProfile":
					v, err := decodeStringPref(key, rv)
					if err != nil {
						BadRequest(w, err.Error())
						return
					}
					patch.DefaultProfile = &v
					hasFields = true
				case "theme":
					v, err := decodeStringPref(key, rv)
					if err != nil {
						BadRequest(w, err.Error())
						return
					}
					patch.Theme = &v
					hasFields = true
				case "timezone":
					v, err := decodeStringPref(key, rv)
					if err != nil {
						BadRequest(w, err.Error())
						return
					}
					if err := validateUserTimezone(v); err != nil {
						BadRequest(w, err.Error())
						return
					}
					patch.Timezone = &v
					hasFields = true
				default:
					// Unknown preferences keys are silently ignored (200, no
					// change). This keeps older hubs and newer clients
					// compatible, unlike the top-level field switch above,
					// which rejects unknown fields outright.
				}
			}
			if hasFields {
				prefsPatch = patch
			}
		}
	}

	isSelf := actor.ID() == user.ID

	// Token field rules run before the field values are validated.
	if token != nil && !updateUserTokenFields(w, updates, prefsPatch, isSelf) {
		return
	}

	// Validate role field eagerly — reject unsupported values even if the
	// role matches the current value.
	if updates.Role != nil {
		switch *updates.Role {
		case store.UserRoleAdmin, store.UserRoleMember, store.UserRoleViewer:
			// valid canonical roles
		default:
			BadRequest(w, fmt.Sprintf("unsupported role %q; valid values are \"admin\", \"member\" and \"viewer\"", *updates.Role))
			return
		}
	}

	// A test identity's role is fixed at issuance (member or viewer); no
	// update path changes it, and in particular none can make it admin.
	// Refused for every row that is not positively human (a test identity
	// of any kind, or an unrecognized kind).
	if updates.Role != nil && !user.IsHumanKind() && *updates.Role != user.Role {
		writeError(w, http.StatusConflict, ErrCodeConflict, errRoleOnTestFixture.Error(),
			map[string]interface{}{"reason": "test_identity_role_fixed"})
		return
	}

	// Validate status field.
	if updates.Status != nil {
		switch *updates.Status {
		case "active", "suspended":
			// valid
		default:
			BadRequest(w, fmt.Sprintf("unsupported status %q; valid values are \"active\" and \"suspended\"", *updates.Status))
			return
		}
	}

	// ── Permission pre-check: require ALL permissions before any write ──

	needsPromote := updates.Role != nil
	needsSuspend := updates.Status != nil
	needsUpdate := updates.DisplayName != nil || prefsPatch != nil

	needsCrossUserUpdate := needsUpdate && !isSelf
	// A token PATCH of another user that names neither status nor role is
	// checked as a profile update, so it needs user:update even with no
	// field set.
	if token != nil && !isSelf && !needsSuspend && !needsPromote {
		needsCrossUserUpdate = true
	}

	// The record the field permissions are checked on: a token's selectors
	// name the hub-level user record; a session keeps the user record.
	userTarget := Resource{Type: "user", ID: user.ID}
	if token != nil {
		userTarget = hubScopedResource("user", user.ID)
	}

	// Pre-resolve super-admin role definition and canonical binding state
	// before authorization. Canonical state comes from bindings, not User.Role,
	// to prevent stale-state bypass. Both active AND any-lifecycle states are
	// queried: CanDelegate requires HasAny, self-lockout requires HasActive
	// (R4-fix lifecycle).
	var superAdminRD *store.RoleDefinition
	var preAuthBindingState superAdminBindingState
	if needsPromote {
		superAdminRD, err = s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"super-admin role definition not found", nil)
			return
		}
		if superAdminRD == nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"super-admin role definition not found", nil)
			return
		}
		preAuthBindingState, err = s.superAdminBindingStateForUser(ctx, s.store, user.ID, superAdminRD)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to check canonical binding state", nil)
			return
		}
	}

	if needsPromote {
		if err := s.checkUserPromotePermission(ctx, w, actor, userTarget, *updates.Role, preAuthBindingState); err != nil {
			return
		}
	}

	if needsSuspend {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   userTarget,
			Action:     Action("suspend"),
			Permission: "user.suspend",
		})
		if !decision.Allowed {
			writeForbiddenStructured(w, "requires user.suspend permission", "user", Action("suspend"))
			return
		}
		if isSelf {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot change your own status", nil)
			return
		}
	}

	if needsCrossUserUpdate {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   userTarget,
			Action:     Action("update"),
			Permission: "user.update",
		})
		if !decision.Allowed {
			writeForbiddenStructured(w, userCrossProfileDenialMessage, "user", Action("update"))
			return
		}
	}

	// Self-lockout guard: uses canonical binding state, not User.Role (R4-fix).
	// Only applies when an ACTIVE binding exists — removing a scheduled-only
	// binding on yourself is not self-demotion of current authority.
	if needsPromote && preAuthBindingState.HasActive && *updates.Role != "admin" && isSelf {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot demote yourself; ask another admin to change your role", nil)
		return
	}

	// Invited users have no real role yet: the stored role is a placeholder and
	// the role is assigned at first sign-in (see determineUserRole). Reject role
	// changes so an admin cannot set a role that activation would silently
	// overwrite. Suspending an invited user remains allowed; activation is rejected below.
	if needsPromote && user.Status == store.UserStatusInvited {
		writeError(w, http.StatusConflict, ErrCodeConflict, errRoleOnInvitedUser.Error(), nil)
		return
	}
	// Likewise, activating an invited user directly would leave the placeholder
	// role in place and skip the invited->active branch at first sign-in, so
	// the hub default would never be applied. Suspending remains allowed.
	if activatesInvitedUser(updates.Status, user.Status) {
		writeError(w, http.StatusConflict, ErrCodeConflict, errActivateInvitedUser.Error(), nil)
		return
	}

	// ── Execute ALL mutations in a single atomic transaction (R4-C1) ──

	// Build actor audit metadata outside the transaction.
	auditActor := s.buildAuditActorFromContext(ctx)

	// suspended is set when the transaction changes the status to
	// suspended.
	suspended := false
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		suspended = false
		// Re-read user inside the transaction for consistency.
		txUser, err := tx.GetUser(ctx, id)
		if err != nil {
			return fmt.Errorf("re-read user in tx: %w", err)
		}

		// Capture canonical before-state from the transactional read for
		// truthful audit records and change detection (R4-fix: not stale pre-tx).
		beforeRole := txUser.Role
		beforeStatus := txUser.Status

		// Re-check the invited guard against the transactional read.
		if needsPromote && beforeStatus == store.UserStatusInvited {
			return errRoleOnInvitedUser
		}
		if activatesInvitedUser(updates.Status, beforeStatus) {
			return errActivateInvitedUser
		}

		// Suspension must not leave a scope with no active constraint
		// admin (the same check as role-binding removal and deletion).
		if updates.Status != nil && *updates.Status == store.UserStatusSuspended && beforeStatus != store.UserStatusSuspended {
			if err := checkUserRemovalAdminLockout(ctx, tx, time.Now(), txUser.ID, userRemovalSuspend); err != nil {
				return err
			}
		}

		// Role transition: derives classification from canonical binding state
		// inside the transaction, not from User.Role (R4-fix).
		var bindingMutation bindingMutationKind
		if needsPromote {
			var err error
			bindingMutation, err = s.executeRoleTransition(ctx, tx, txUser, *updates.Role, superAdminRD, actor.ID(), preAuthBindingState)
			if err != nil {
				return err
			}
		}

		// Status change.
		if updates.Status != nil {
			txUser.Status = *updates.Status
		}

		// Profile metadata.
		if updates.DisplayName != nil {
			txUser.DisplayName = *updates.DisplayName
		}
		if prefsPatch != nil {
			if txUser.Preferences == nil {
				txUser.Preferences = &store.UserPreferences{}
			}
			if prefsPatch.DefaultTemplate != nil {
				txUser.Preferences.DefaultTemplate = *prefsPatch.DefaultTemplate
			}
			if prefsPatch.DefaultProfile != nil {
				txUser.Preferences.DefaultProfile = *prefsPatch.DefaultProfile
			}
			if prefsPatch.Theme != nil {
				txUser.Preferences.Theme = *prefsPatch.Theme
			}
			if prefsPatch.Timezone != nil {
				txUser.Preferences.Timezone = *prefsPatch.Timezone
			}
		}

		// Persist all User record changes in the same transaction.
		if needsSuspend || needsUpdate || needsPromote {
			if err := tx.UpdateUser(ctx, txUser); err != nil {
				return fmt.Errorf("persist user changes: %w", err)
			}
		}

		// Synchronous audit records (R4-C3): written inside the transaction
		// so they roll back if the transaction fails. beforeRole/beforeStatus
		// come from the in-tx re-read for truthful audit (R4-fix).
		//
		// Audit fires on EITHER a User.Role change OR a binding mutation,
		// so same-role canonical repairs (creating a missing binding or
		// removing a stale one) are always recorded (R4-fix audit).
		if needsPromote && (beforeRole != txUser.Role || bindingMutation != bindingMutationNone) {
			mutationType := "user_role_change"
			if beforeRole == txUser.Role && bindingMutation != bindingMutationNone {
				// Same-role but binding changed — use a repair-specific type.
				mutationType = "user_role_binding_" + string(bindingMutation)
			}
			record := &store.MutationAuditRecord{
				MutationType:  mutationType,
				TargetType:    "user",
				TargetID:      txUser.ID,
				BeforeSummary: fmt.Sprintf(`{"role":%q,"binding":%q}`, beforeRole, bindingMutation),
				AfterSummary:  fmt.Sprintf(`{"role":%q}`, txUser.Role),
				Timestamp:     time.Now(),
			}
			auditActor.ApplyActor(record)
			if err := tx.CreateMutationAudit(ctx, record); err != nil {
				return fmt.Errorf("audit role change: %w", err)
			}
		}

		if needsSuspend && beforeStatus != txUser.Status {
			suspended = txUser.Status == store.UserStatusSuspended
			mutationType := "user_suspend"
			if txUser.Status == "active" {
				mutationType = "user_reactivate"
			}
			record := &store.MutationAuditRecord{
				MutationType:  mutationType,
				TargetType:    "user",
				TargetID:      txUser.ID,
				BeforeSummary: fmt.Sprintf(`{"status":%q}`, beforeStatus),
				AfterSummary:  fmt.Sprintf(`{"status":%q}`, txUser.Status),
				Timestamp:     time.Now(),
			}
			auditActor.ApplyActor(record)
			if err := tx.CreateMutationAudit(ctx, record); err != nil {
				return fmt.Errorf("audit status change: %w", err)
			}
		}

		// Copy the transactional state back for the response.
		*user = *txUser
		return nil
	})

	if err != nil {
		if writeConstraintAdminLockoutError(w, err) {
			return
		}
		if errors.Is(err, store.ErrUserDeleting) {
			// The user's forced deletion is in progress: its status
			// leaves deleting only when the deletion completes
			// (ptone/scion#2143).
			writeUserDeletionInProgress(w, userDeletionInProgressFor(ctx, s.store, user.ID))
			return
		}
		if errors.Is(err, errLastSuperAdmin) || errors.Is(err, errSelfDemotion) || errors.Is(err, errRoleOnInvitedUser) || errors.Is(err, errActivateInvitedUser) || errors.Is(err, errRoleOnTestFixture) {
			writeError(w, http.StatusConflict, ErrCodeConflict, err.Error(), nil)
		} else if errors.Is(err, errBindingStateDrift) {
			writeBindingStateDrift(w, err)
		} else {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"user update failed: "+err.Error(), nil)
		}
		return
	}

	if needsPromote || needsSuspend {
		s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: user.ID})
	}
	// A suspended issuer's agent delegation grants end, best effort:
	// exchange and use already deny them (.design/agent-delegation.md
	// §16.3).
	if suspended {
		s.revokeAgentDelegationGrantsBestEffort(ctx, store.AgentDelegationGrantFilter{IssuerUserID: user.ID}, revokeReasonIssuerSuspended)
	}

	// This response applies the same per-viewer preferences visibility rule
	// as GET (stripPreferencesForViewer; used by getUser and listUsers).
	// A token's capabilities are computed on the hub-scoped user record;
	// other callers' on the user record. The token branch builds the shape
	// hubScopedResource builds, written as a literal so the resource-type
	// guard resolves its type.
	var cap *Capabilities
	if token != nil {
		cap = s.authzService.ComputeCapabilities(ctx, actor,
			Resource{Type: "user", ID: user.ID, ParentType: "system"})
	} else {
		cap = s.authzService.ComputeCapabilities(ctx, actor, userResource(user))
	}
	stripPreferencesForViewer(ctx, user, cap)

	writeJSON(w, http.StatusOK, user)
}

// buildAuditActorFromContext extracts actor identity and credential metadata
// from the request context for use in transactional audit records. E.2a: thin
// wrapper over the shared auditActorFromContext helper (plan §3.3), which
// also carries the credential snapshot, correlation ID, and executor fields
// this file's call sites apply via AuditActor.ApplyActor.
func (s *Server) buildAuditActorFromContext(ctx context.Context) AuditActor {
	return auditActorFromContext(ctx)
}

// superAdminBindingState describes the lifecycle state of a user's super-admin
// bindings. Used by pre-auth and in-tx classification to handle all lifecycle
// states correctly (R4-fix lifecycle).
type superAdminBindingState struct {
	// HasAny is true when ANY matching binding exists (active, scheduled, or expired).
	// Used for CanDelegate determination: any involvement with super-admin bindings
	// requires delegation authority.
	HasAny bool
	// HasActive is true when an ACTIVE binding exists (within its valid time window).
	// Used for self-lockout guard (only when removing active authority) and
	// last-admin count (only active users with active bindings count).
	HasActive bool
}

// superAdminBindingStateForUser queries both the active and any-lifecycle
// binding state for a user's super-admin bindings.
func (s *Server) superAdminBindingStateForUser(
	ctx context.Context, st store.Store,
	userID string, rd *store.RoleDefinition,
) (superAdminBindingState, error) {
	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return superAdminBindingState{}, fmt.Errorf("list bindings for user: %w", err)
	}
	now := time.Now()
	var state superAdminBindingState
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem || b.RoleDefinitionID != rd.ID {
			continue
		}
		state.HasAny = true
		// Check if this particular binding is currently active.
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			continue // expired
		}
		if b.NotBefore != nil && now.Before(*b.NotBefore) {
			continue // scheduled, not yet active
		}
		state.HasActive = true
	}
	return state, nil
}

// checkUserPromotePermission verifies the actor has user.promote on target
// (the user record, or for a token the hub-level user record) and
// CanDelegate authority for the role transition. Writes HTTP error and
// returns non-nil on failure.
func (s *Server) checkUserPromotePermission(
	ctx context.Context, w http.ResponseWriter,
	actor UserIdentity, target Resource, newRole string,
	bindingState superAdminBindingState,
) error {
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   target,
		Action:     Action("promote"),
		Permission: "user.promote",
	})
	if !decision.Allowed {
		writeForbiddenStructured(w, "requires user.promote permission", "user", Action("promote"))
		return fmt.Errorf("user.promote denied")
	}

	// Determine if this operation involves super-admin bindings.
	// CanDelegate is required whenever the target has ANY binding (active,
	// scheduled, or expired) or the new role is admin. This prevents the
	// stale-state bypass AND ensures scheduled/expired bindings cannot be
	// silently manipulated without delegation authority (R4-fix lifecycle).
	wantsBinding := newRole == "admin"
	involvesSuper := bindingState.HasAny || wantsBinding

	if involvesSuper {
		superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"super-admin role definition not found", nil)
			return err
		}
		canDel := s.authzService.CanDelegate(ctx, actor, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: superAdminRD.ID,
			ScopeType:        store.RoleScopeSystem,
		})
		if !canDel.Allowed {
			// Report the truthful binding action: "create" when promoting
			// (wantsBinding), "delete" when demoting an existing binding.
			bindingAction := Action("delete")
			if wantsBinding {
				bindingAction = Action("create")
			}
			writeForbiddenStructured(w, "insufficient authority to modify super-admin role binding: "+canDel.Reason, "role_binding", bindingAction)
			return fmt.Errorf("CanDelegate denied: %s", canDel.Reason)
		}
	}

	return nil
}

// errLastSuperAdmin is returned when demoting/deleting the last active
// super-admin would leave zero authenticatable system-scoped super-admin users.
var errLastSuperAdmin = errors.New("cannot remove the last super-admin; promote another user first")

// errSelfDemotion is returned when the transactional canonical-state recheck
// detects the actor is removing their own super-admin binding. This catches
// the TOCTOU window between the pre-tx self-lockout guard and the in-tx
// binding mutation.
var errSelfDemotion = errors.New("cannot demote yourself; ask another admin to change your role")

// errRoleOnTestFixture is returned when a role change targets a hub test
// identity, whose role is fixed at issuance.
var errRoleOnTestFixture = errors.New("the role of a test identity is fixed at issuance")

// errRoleOnInvitedUser is returned when PATCH sets a role on an invited user.
var errRoleOnInvitedUser = errors.New("role is assigned when the user first signs in")

// errActivateInvitedUser is returned when PATCH sets status=active on an
// invited user.
var errActivateInvitedUser = errors.New("invited users are activated at first sign-in")

// activatesInvitedUser reports whether a PATCH status would move an invited
// user straight to active, bypassing first sign-in.
func activatesInvitedUser(newStatus *string, currentStatus string) bool {
	return newStatus != nil && *newStatus == store.UserStatusActive && currentStatus == store.UserStatusInvited
}

// bindingMutationKind describes what happened to super-admin bindings during
// a role transition. Used for truthful audit records even when User.Role
// doesn't change (R4-fix audit).
type bindingMutationKind string

const (
	bindingMutationNone      bindingMutationKind = ""
	bindingMutationCreated   bindingMutationKind = "grant"   // new active binding created
	bindingMutationRevoked   bindingMutationKind = "revoke"  // active binding removed
	bindingMutationRepaired  bindingMutationKind = "repair"  // stale replaced with active
	bindingMutationCleanedUp bindingMutationKind = "cleanup" // expired/scheduled removed, no active existed
)

// errBindingStateDrift is returned when the canonical binding state inside the
// transaction differs from the pre-authorization check in a way that would
// bypass CanDelegate. The caller must retry (R6 TOCTOU fix).
var errBindingStateDrift = errors.New("binding state changed since authorization check; retry the operation")

// executeRoleTransition performs the binding mutations inside a store
// transaction. Classification is derived from canonical binding state (whether
// super-admin bindings exist and their lifecycle state) inside the transaction,
// NOT from User.Role which may be stale.
//
// Lifecycle handling (R4-fix lifecycle):
//   - member/viewer: removes ALL matching bindings (active, scheduled, expired)
//   - admin: ensures exactly one active binding exists; stale rows are
//     deleted then a fresh active binding is created
//
// TOCTOU safety (R6): preAuthState records the binding state that was
// authorized before the transaction. If the in-tx state has bindings that
// weren't present at preauth (txState.HasAny && !preAuthState.HasAny), the
// operation is rejected to prevent concurrent promotion from creating a
// binding that is then silently removed without CanDelegate verification.
//
// After the super-admin bookkeeping, syncHubRoleGrants makes the hub-members
// group membership and hub-viewer binding match newRole in the same
// transaction (fail closed).
//
// Returns what binding mutation occurred for truthful audit (R4-fix audit).
func (s *Server) executeRoleTransition(
	ctx context.Context,
	tx store.Store,
	user *store.User,
	newRole string,
	superAdminRD *store.RoleDefinition,
	actorID string,
	preAuthState superAdminBindingState,
) (bindingMutationKind, error) {
	// Containment: a test identity's role never changes, so it can never
	// become admin. Checked on the transactional row.
	if !user.IsHumanKind() && newRole != user.Role {
		return bindingMutationNone, errRoleOnTestFixture
	}
	// Determine canonical binding state inside the transaction (R4-fix).
	txState, err := s.superAdminBindingStateForUser(ctx, tx, user.ID, superAdminRD)
	if err != nil {
		return bindingMutationNone, fmt.Errorf("check canonical binding state in tx: %w", err)
	}

	// TOCTOU guard (R6): if the in-tx state has super-admin bindings that
	// weren't present at pre-authorization time, a concurrent operation
	// created them between the preauth check and this transaction. The
	// CanDelegate check may not have covered this new state, so fail
	// closed rather than silently mutating bindings without authorization.
	if txState.HasAny && !preAuthState.HasAny {
		return bindingMutationNone, errBindingStateDrift
	}

	wantsBinding := newRole == "admin"
	mutation := bindingMutationNone

	switch {
	case wantsBinding && !txState.HasActive:
		// Need an active binding. If stale (expired/scheduled) bindings exist,
		// delete them first to avoid unique constraint conflicts, then create
		// a fresh active binding (R4-fix lifecycle).
		if txState.HasAny {
			if err := s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
				return bindingMutationNone, fmt.Errorf("clean up stale bindings: %w", err)
			}
			mutation = bindingMutationRepaired
		} else {
			mutation = bindingMutationCreated
		}
		if err := s.createSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return bindingMutationNone, fmt.Errorf("create super-admin binding: %w", err)
		}

	case !wantsBinding && txState.HasAny:
		// Remove ALL super-admin bindings (active + scheduled + expired).
		// Governance guards only apply when an active binding is being removed.
		if txState.HasActive {
			// Self-lockout re-check inside tx (catches TOCTOU).
			if user.ID == actorID {
				return bindingMutationNone, errSelfDemotion
			}
			if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
				return bindingMutationNone, err
			}
			mutation = bindingMutationRevoked
		} else {
			// Only expired/scheduled bindings — no governance needed, just cleanup.
			mutation = bindingMutationCleanedUp
		}
		if err := s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return bindingMutationNone, fmt.Errorf("delete super-admin binding: %w", err)
		}

	case wantsBinding && txState.HasActive:
		// Active binding already exists — idempotent. No mutation to audit.
		mutation = bindingMutationNone

		// !wantsBinding && !txState.HasAny: nothing to do.
	}

	// Make hub-level grants (hub-members group, hub-viewer binding) match
	// the target role inside the same transaction. Fails closed: any error
	// rolls back the role change.
	if err := syncHubRoleGrants(ctx, tx, user.ID, newRole, store.AdminAPICreatedBy); err != nil {
		return bindingMutationNone, fmt.Errorf("sync hub role grants: %w", err)
	}

	user.Role = newRole
	return mutation, nil
}

// createSuperAdminBindingTx idempotently creates a system-scoped super-admin
// role binding. Uses AdminAPICreatedBy sentinel so that UI/API-granted
// promotions are protected from demotion by ReconcileSuperAdminBindings.
func (s *Server) createSuperAdminBindingTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	_, err := tx.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.AdminAPICreatedBy,
	})
	if err != nil && errors.Is(err, store.ErrAlreadyExists) {
		return nil
	}
	return err
}

// deleteSuperAdminBindingTx removes all system-scoped super-admin role
// bindings for the given user.
func (s *Server) deleteSuperAdminBindingTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	bindings, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return fmt.Errorf("list bindings for deletion: %w", err)
	}
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
				return fmt.Errorf("delete binding %s: %w", b.ID, err)
			}
			slog.Info("deleted super-admin binding via admin role mutation",
				"user_id", userID, "binding_id", b.ID)
			// The user's system authority ended: re-evaluate the user's
			// project standing (ptone/scion#3433).
			if err := enqueueMembershipLossTx(ctx, tx, userID, "", store.MembershipLossTriggerSystemScopeChange, auditActorFromContext(ctx)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkLastSuperAdminTx verifies that removing the given user's super-admin
// binding would not leave zero authenticatable system-scoped super-admin users.
//
// Concurrency: acquires a SELECT FOR UPDATE lock on the super-admin role
// definition row before reading bindings. This serializes concurrent
// demotion/deletion transactions that both try to verify the last-admin
// invariant, preventing the READ COMMITTED race where two demotions both
// observe the other admin and both commit (R4-fix concurrency).
//
// R4-C4: resolves each unique binding principal to a User record and counts
// only users with status "active". Suspended/invited users are not counted as
// surviving admins. Fails closed on user lookup errors.
func (s *Server) checkLastSuperAdminTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	// Acquire serialization lock on the role definition row to prevent
	// concurrent last-admin checks from racing (R4-fix concurrency).
	if err := tx.LockRoleDefinitionForAdminGuard(ctx, rd.ID); err != nil {
		return fmt.Errorf("acquire admin-guard lock: %w", err)
	}

	// This read is UNCLAMPED (it does not go through the authorization
	// service's test-identity grant clamp), so a hub test identity, which
	// can never hold super-admin authority, is excluded explicitly below.
	systemBindings, err := tx.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
	if err != nil {
		return fmt.Errorf("failed to verify admin count: %w", err)
	}

	now := time.Now()
	candidateUserIDs := make(map[string]bool)
	var targetHasActiveBinding bool

	for _, b := range systemBindings {
		if b.RoleDefinitionID != rd.ID || b.PrincipalType != store.RoleBindingPrincipalUser {
			continue
		}
		// Skip expired bindings.
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			continue
		}
		// Skip scheduled (not yet active) bindings.
		if b.NotBefore != nil && now.Before(*b.NotBefore) {
			continue
		}
		candidateUserIDs[b.PrincipalID] = true
		if b.PrincipalID == userID {
			targetHasActiveBinding = true
		}
	}

	if !targetHasActiveBinding {
		return nil
	}

	// Resolve each candidate to verify they are active (can authenticate).
	// Only users with status "active" count as surviving admins (R4-C4).
	activeAdminCount := 0
	for uid := range candidateUserIDs {
		if uid == userID {
			continue // exclude the user being removed
		}
		u, err := tx.GetUser(ctx, uid)
		if err != nil {
			// Fail closed: if we can't verify a user exists and is active,
			// don't count them as a surviving admin.
			slog.Warn("checkLastSuperAdminTx: failed to resolve binding principal",
				"principal_id", uid, "error", err)
			continue
		}
		// A hub test identity never counts as a surviving super-admin: the
		// member clamp drops a fixture's super-admin grant, and an
		// admin-kind identity's authority expires with it.
		// Only a positively human row counts: an unrecognized kind never
		// counts as a surviving super-admin (fail closed).
		if u.Status == "active" && u.IsHumanKind() {
			activeAdminCount++
		}
	}

	if activeAdminCount == 0 {
		return errLastSuperAdmin
	}

	return nil
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/users/{id} — user deletion (R3/R4)
//
// Authorization: requires user.delete permission via session credential,
// or via a user access token with user:delete on a hub boundary, checked
// on the hub-level user record. A token is refused while the user holds a
// direct system-scope role binding of any lifecycle state other than the
// canonical hub-viewer binding of a viewer
// (refuseTokenDeleteOfSystemBindingHolder, 403 with details.reason
// system_role_binding_held). Inside the transaction the rule is judged
// again on the exact bindings the cascade deletes
// (tokenDeleteBindingsCheck), which answers 409 conflict with
// details.reason binding_state_drift. A token's forced delete follows the
// same rules.
// Guards: self-deletion and last-active-super-admin are prevented based on
// bindings (not User.Role). All operations — last-admin check, skill cleanup,
// user deletion, and audit — execute in a single atomic transaction (R4-C2).
// A user whom agents or active delegations depend on is refused with
// 409 user_delete_blocked (ptone/scion#2769, ptone/scion#2143); GET
// /api/v1/users/{id}/delete-blockers lists the same blockers. The user's
// user-scope secrets and env vars are removed after commit, best effort.
// With force=true and hub.user_delete_force_cascade on, a blocked user is
// deleted through the forced cascade instead (forceDeleteUser, 202 with
// the deletion resource); while a forced deletion is in progress a plain
// delete answers 409 user_deletion_in_progress.
// ---------------------------------------------------------------------------

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Credential boundary + identity check (R4-C5).
	actor, token, ok := s.sessionOrUserTokenCredential(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		return
	}
	// force is parsed before any side effect (ptone/scion#2143).
	force := s.forceUserDeletionEnabled(r)

	// Self-deletion guard.
	if actor.ID() == id {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot delete your own account", nil)
		return
	}

	// Authorization: require user.delete permission.
	if !s.authorizeUserDelete(ctx, w, actor, token, id) {
		return
	}

	// Load the target user (needed for binding-based last-admin check).
	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve super-admin role definition (needed for binding-based check).
	superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"super-admin role definition not found", nil)
		return
	}
	if superAdminRD == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"super-admin role definition not found", nil)
		return
	}

	// A token may not delete a user holding a direct system-scope role
	// binding other than its role's canonical representation.
	if token != nil && s.refuseTokenDeleteOfSystemBindingHolder(ctx, w, user) {
		return
	}

	// A forced delete of a user whom agents or delegations depend on
	// revokes and records a tombstone instead (forceDeleteUser); with
	// nothing blocking, it falls through to the plain delete.
	if force && s.forceDeleteUser(w, r, actor, token != nil, user, superAdminRD) {
		return
	}

	auditActor := s.buildAuditActorFromContext(ctx)

	// Execute everything in a single atomic transaction (R4-C2).
	// For a token, the cascade judges the system-binding rule on the
	// bindings it deletes.
	var bindingsCheck userBindingsCheck
	if token != nil {
		bindingsCheck = tokenDeleteBindingsCheck
	}
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		// Last-admin guard based on bindings, not User.Role (R4-C2).
		if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return err
		}
		// Constraint-admin lockout guard, shared with suspension and
		// role-binding removal.
		if err := checkUserRemovalAdminLockout(ctx, tx, time.Now(), user.ID, userRemovalDelete); err != nil {
			return err
		}

		// Agent guard, binding cascade (with the token rule for a
		// token), skill injections, group memberships and the user row.
		if err := deleteUserRowCascadeModeTx(ctx, tx, user.ID, s.membershipNow(), false, bindingsCheck); err != nil {
			return err
		}

		// Synchronous audit record (R4-C3).
		record := &store.MutationAuditRecord{
			MutationType:  "user_delete",
			TargetType:    "user",
			TargetID:      id,
			BeforeSummary: fmt.Sprintf(`{"email":%q,"role":%q,"status":%q}`, user.Email, user.Role, user.Status),
			Timestamp:     time.Now(),
		}
		auditActor.ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("audit delete: %w", err)
		}

		return nil
	})

	if err != nil {
		if errors.Is(err, errLastSuperAdmin) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot delete the last super-admin; promote another user first", nil)
		} else if errors.Is(err, errBindingStateDrift) {
			writeBindingStateDrift(w, err)
		} else if errors.Is(err, errDeleteTargetMissing) {
			// Only the user itself gone by the transaction is a 404; a
			// not-found from any other read keeps the 500 below.
			NotFound(w, "User")
		} else if !writeConstraintAdminLockoutError(w, err) && !writeUserDeleteCascadeError(w, err) {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"user deletion failed: "+err.Error(), nil)
		}
		return
	}

	s.afterUserDeleted(ctx, id)

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// GET /api/v1/users/{id}/delete-blockers — what blocks a user deletion
//
// Authorization: the same as DELETE /api/v1/users/{id} (a session
// credential with user.delete on the user, or a user access token with
// user:delete on the hub-level user record). It returns the lists a plain delete would
// refuse with (listUserDeleteBlockers): {agents, edges, truncated}, each
// list capped. It reads outside a transaction, takes no lock and writes
// nothing, so a delete can find a blocker added after the listing.
// ---------------------------------------------------------------------------

func (s *Server) getUserDeleteBlockers(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	actor, token, ok := s.sessionOrUserTokenCredential(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		return
	}
	if !s.authorizeUserDelete(ctx, w, actor, token, id) {
		return
	}

	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	blockers, err := listUserDeleteBlockers(ctx, s.store, user.ID)
	if err != nil {
		slog.ErrorContext(ctx, "list user delete blockers failed", "user_id", user.ID, "error", err)
		InternalError(w)
		return
	}
	resp := userDeleteBlockersResponse{userDeleteBlockers: *blockers}
	// A forced deletion in progress (ptone/scion#2143) is named, so a
	// client can show it instead of offering a delete.
	if d, err := s.store.GetUserDeletionByUser(ctx, user.ID); err == nil && d.State == store.UserDeletionStateStopping {
		resp.InProgress = &userDeletionRef{DeletionID: d.ID, State: d.State}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "read user deletion failed", "user_id", user.ID, "error", err)
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// userDeletionRef names a forced deletion in progress.
type userDeletionRef struct {
	DeletionID string `json:"deletionId"`
	State      string `json:"state"`
}

// userDeleteBlockersResponse is the body of GET
// /api/v1/users/{id}/delete-blockers: the blockers, and the forced
// deletion in progress (omitted when there is none, so the body is
// unchanged for a user with no deletion).
type userDeleteBlockersResponse struct {
	userDeleteBlockers
	InProgress *userDeletionRef `json:"inProgress,omitempty"`
}

// authorizeUserDelete decides user.delete for DELETE /api/v1/users/{id} and
// its deletion reads, on the user record for a session and on the
// hub-level user record (hubScopedResource) for a user access token
// (token non-nil). The decision uses only the path id, so its outcome is
// the same whether or not user id exists. It writes the refusal and
// returns false when the permission is denied.
func (s *Server) authorizeUserDelete(ctx context.Context, w http.ResponseWriter, actor UserIdentity, token *ScopedUserIdentity, id string) bool {
	target := Resource{Type: "user", ID: id}
	if token != nil {
		target = hubScopedResource("user", id)
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   target,
		Action:     Action("delete"),
		Permission: "user.delete",
	})
	if !decision.Allowed {
		writeForbiddenStructured(w, "requires user.delete permission", "user", Action("delete"))
		return false
	}
	return true
}

// userDeleteSystemBindingReason is the details.reason of the refusal of a
// token's user delete while the user holds a direct system-scope role
// binding.
const userDeleteSystemBindingReason = "system_role_binding_held"

// tokenRefusedSystemBinding reports whether bindings, the direct role
// bindings of user of any lifecycle state (active, scheduled or expired),
// hold a system-scope binding other than the canonical representation of
// the user's role: the unconditional hub-viewer binding of a user whose
// role is viewer (syncHubRoleGrantsTx). The role is governed by
// user.promote, not conferred as a separate grant; the hub-members group
// membership that represents the member role is not a role binding of the
// user.
func tokenRefusedSystemBinding(ctx context.Context, st store.Store, user *store.User, bindings []*store.RoleBinding) (bool, error) {
	var hubViewerID string
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem {
			continue
		}
		if user.Role == store.UserRoleViewer && b.NotBefore == nil && b.ExpiresAt == nil {
			if hubViewerID == "" {
				rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
				if err != nil {
					return false, fmt.Errorf("hub-viewer role definition lookup: %w", err)
				}
				if rd == nil {
					return false, fmt.Errorf("hub-viewer role definition not found")
				}
				hubViewerID = rd.ID
			}
			if b.RoleDefinitionID == hubViewerID {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

// userHoldsSystemRoleBinding reports whether user holds a direct
// system-scope role binding that tokenRefusedSystemBinding refuses.
func userHoldsSystemRoleBinding(ctx context.Context, st store.Store, user *store.User) (bool, error) {
	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	if err != nil {
		return false, fmt.Errorf("list bindings for user: %w", err)
	}
	return tokenRefusedSystemBinding(ctx, st, user, bindings)
}

// refuseTokenDeleteOfSystemBindingHolder applies the user delete rule for
// a user access token: a token confers and removes no system-scope grant
// beyond the representation of the role that user.promote governs, so it
// may not delete a user who holds any other direct system-scope role
// binding of any lifecycle state (userHoldsSystemRoleBinding), whose
// bindings the delete removes. It writes 403 forbidden with details.reason
// system_role_binding_held, or a 500 when the bindings cannot be read, and
// returns true when it wrote a response.
func (s *Server) refuseTokenDeleteOfSystemBindingHolder(ctx context.Context, w http.ResponseWriter, user *store.User) bool {
	held, err := userHoldsSystemRoleBinding(ctx, s.store, user)
	if err != nil {
		slog.ErrorContext(ctx, "user delete: list role bindings failed", "user_id", user.ID, "error", err)
		InternalError(w)
		return true
	}
	if !held {
		return false
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"a user access token cannot delete a user who holds a system-scope role binding",
		map[string]interface{}{"reason": userDeleteSystemBindingReason})
	return true
}

// userBindingsCheck judges a user's role bindings inside a delete
// transaction: bindings are exactly the bindings
// guardAndCascadeUserRoleBindingsCheckTx lists and then deletes, read after
// the user row is locked. A non-nil error aborts the delete.
type userBindingsCheck func(ctx context.Context, tx store.Store, userID string, bindings []*store.RoleBinding) error

// tokenDeleteBindingsCheck is the user delete rule for a user access token
// (refuseTokenDeleteOfSystemBindingHolder), judged on the bindings the
// cascade deletes: it returns errBindingStateDrift when they hold a
// system-scope binding the rule refuses, so a token delete never removes
// such a binding. It reads the user in tx, under the user row lock that
// the delete holds, so the role it judges the hub-viewer binding against
// cannot change before the delete commits.
func tokenDeleteBindingsCheck(ctx context.Context, tx store.Store, userID string, bindings []*store.RoleBinding) error {
	user, err := tx.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	refused, err := tokenRefusedSystemBinding(ctx, tx, user, bindings)
	if err != nil {
		return err
	}
	if refused {
		return errBindingStateDrift
	}
	return nil
}

// bindingStateDriftReason is the details.reason of the 409 conflict that a
// role change or a token's user delete answers when the user's role
// bindings changed after the authorization check.
const bindingStateDriftReason = "binding_state_drift"

// writeBindingStateDrift writes the 409 conflict for errBindingStateDrift,
// with details.reason binding_state_drift.
func writeBindingStateDrift(w http.ResponseWriter, err error) {
	writeError(w, http.StatusConflict, ErrCodeConflict, err.Error(),
		map[string]interface{}{"reason": bindingStateDriftReason})
}

// deleteUserRowCascadeTx deletes the user row of userID and what hangs off
// it, inside the caller's transaction. It is the part of a user deletion
// shared by DELETE /api/v1/users/{id} and DELETE
// /api/v1/test-identities/{id}. In order it:
//
//   - refuses while agents or active delegations depend on the user
//     (checkUserDeleteBlockersTx; returns *userDeleteBlockedError, and
//     store.ErrNotFound when the user does not exist);
//   - runs the last-project-owner guard and the role-binding cascade
//     (guardAndCascadeUserRoleBindingsTx);
//   - deletes the user-scope skill injections and the group memberships;
//   - deletes the user row.
//
// The caller runs any caller-specific guard (the last super-admin check)
// before it, writes its audit record after it in the same transaction, maps
// errors with writeUserDeleteCascadeError, and calls afterUserDeleted once
// the transaction commits.
func deleteUserRowCascadeTx(ctx context.Context, tx store.Store, userID string, at time.Time) error {
	return deleteUserRowCascadeModeTx(ctx, tx, userID, at, false, nil)
}

// deleteUserRowCascadeModeTx is deleteUserRowCascadeTx. completing is set
// only by the completion of a forced deletion (completeUserDeletion),
// which deletes a user whose status is deleting; every other caller
// refuses such a user with *userDeletionInProgressError. A non-nil check
// judges the bindings the cascade deletes before any is deleted
// (guardAndCascadeUserRoleBindingsCheckTx).
func deleteUserRowCascadeModeTx(ctx context.Context, tx store.Store, userID string, at time.Time, completing bool, check userBindingsCheck) error {
	// Refuse while agents or active delegations depend on the user
	// (ptone/scion#2769, ptone/scion#2143), or while a forced deletion
	// of the user is in progress.
	if err := checkUserDeleteBlockersModeTx(ctx, tx, userID, completing); err != nil {
		return err
	}

	// Last-project-owner guard plus role-binding cascade
	// (ptone/scion#2598). Runs before the user row is deleted, in the
	// same transaction; a concurrent grant or role change to the
	// user's bindings that commits before the cascade aborts the
	// delete with 409 conflict (a concurrent revoke does not; residual
	// race: ptone/scion#2769).
	if err := guardAndCascadeUserRoleBindingsCheckTx(ctx, tx, userID, at, check); err != nil {
		return err
	}

	// Clean up user-scoped skill injections.
	if _, err := tx.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeUser, userID); err != nil {
		return fmt.Errorf("delete skill injections: %w", err)
	}

	// Remove the user's group memberships before the user row: the
	// FK is ON DELETE SET NULL, so afterwards they would be orphans
	// that still count toward group roles (ptone/scion#2769). Residual
	// race on PostgreSQL: a concurrent AddGroupMember for this user can
	// insert a row this delete does not see, which the FK then nulls.
	// It is harmless: orphaned rows are excluded from counts and
	// listings, and the startup sweep removes them. On PostgreSQL the
	// call first locks the groups this user owns, so the transaction
	// takes owned group rows before membership rows, like a concurrent
	// project delete's group cascade (no 40P01 between the two).
	if _, err := tx.DeleteGroupMembershipsForUser(ctx, userID); err != nil {
		return fmt.Errorf("delete group memberships: %w", err)
	}

	// Deactivate every edge with the user as the delegator that is active
	// when the sweep runs: after the check passed, only edges that do not
	// block, or one reactivated after the check read but before the sweep
	// (deactivateUserDelegatorEdgesTx).
	if err := deactivateUserDelegatorEdgesTx(ctx, tx, userID); err != nil {
		return err
	}

	// Delete the user record.
	if err := tx.DeleteUser(ctx, userID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}

	return nil
}

// writeUserDeleteCascadeError writes the response for an error returned by
// deleteUserRowCascadeTx and reports whether it did. It returns false for
// any other error, which the caller answers itself.
func writeUserDeleteCascadeError(w http.ResponseWriter, err error) bool {
	var lastOwnerErr *lastProjectOwnerDeleteError
	var blockedErr *userDeleteBlockedError
	var inProgressErr *userDeletionInProgressError
	switch {
	case errors.As(err, &lastOwnerErr):
		writeLastProjectOwnerDeleteError(w, lastOwnerErr)
	case errors.As(err, &blockedErr):
		writeUserDeleteBlockedError(w, blockedErr)
	case errors.As(err, &inProgressErr):
		writeUserDeletionInProgress(w, inProgressErr)
	case errors.Is(err, errUserRoleBindingsChanged):
		writeUserRoleBindingsChangedError(w)
	default:
		return false
	}
	return true
}

// afterUserDeleted runs once a user deletion has committed: it tells
// conduit subscribers the user's authorization changed, then removes the
// user's user-scope secrets and env vars and revokes the agent delegation
// grants the user issued, as a best effort (ptone/scion#2769), and pauses
// the schedules whose recorded authority is the user (ptone/scion#2143).
// Failures are logged, not returned.
func (s *Server) afterUserDeleted(ctx context.Context, userID string) {
	s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: userID})
	s.removeUserScopedData(ctx, userID)
	// The deleted issuer's agent delegation grants end, best effort: a
	// grant never blocks the delete, and exchange and use deny a missing
	// issuer (.design/agent-delegation.md §16.3).
	s.revokeAgentDelegationGrantsBestEffort(ctx, store.AgentDelegationGrantFilter{IssuerUserID: userID}, revokeReasonIssuerDeleted)
	// The user's schedules would be refused at every fire: pause them with
	// creator_deleted (they never block the delete). The evaluator
	// backstop pauses any left active.
	s.pauseSchedulesAuthoredByUser(ctx, userID)
}

// lastOwnerProjectRef identifies a project that deleting a user would leave
// without an owner.
type lastOwnerProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// lastProjectOwnerDeleteError is returned by
// guardAndCascadeUserRoleBindingsTx when the user is the last owner of one
// or more projects. The surrounding transaction rolls back, so neither the
// user nor any binding is changed.
type lastProjectOwnerDeleteError struct {
	projects []lastOwnerProjectRef
}

func (e *lastProjectOwnerDeleteError) Error() string {
	return lastProjectOwnerDeleteMessage
}

const lastProjectOwnerDeleteMessage = "cannot delete the last owner of a project — transfer ownership or add another usable (active, existing) owner first"

// writeLastProjectOwnerDeleteError writes the 409 last_owner response for a
// denied user deletion. The code and status match the members API last-owner
// denial; details.projects lists the projects that would be left ownerless.
func writeLastProjectOwnerDeleteError(w http.ResponseWriter, e *lastProjectOwnerDeleteError) {
	writeError(w, http.StatusConflict, ErrCodeLastOwner, lastProjectOwnerDeleteMessage,
		map[string]interface{}{"projects": e.projects})
}

// guardAndCascadeUserRoleBindingsTx enforces the last-project-owner rule for
// a user that is about to be deleted, then deletes every role binding held by
// that user (system, hub and project scope). It must run inside WithTx before
// the user row is deleted (ptone/scion#2598).
//
// For each project where userID holds a project-owner binding — including an
// expired or not-yet-active one, since deleting it could otherwise take the
// project to zero owner bindings and let the startup backfill re-grant the
// creator — the deletion is denied when it would remove the project's last
// usable (active, existing) owner, or its last owner binding of any kind
// (userDeleteOrphansProjectTx, ptone/scion#2769). Each such project is
// locked with LockProjectForMembership (in ID order) before the check, which
// serializes against concurrent members-API mutations on those projects.
//
// The binding list is read before any lock, so a binding granted to userID
// concurrently (for example a new owner binding on a project that was never
// locked or checked, or the owner half of a TransferOwnership-style swap) is
// not seen by the guard. The cascade therefore checks the set, not a count:
// it first deletes each listed binding by ID (a listed binding that is
// already gone was revoked concurrently; that is harmless, because every
// project the user owns is locked, so it is ignored), then runs a predicate
// delete by principal, which must remove nothing. If it removes any row, a
// binding the guard did not check was committed in the meantime; the
// function returns errUserRoleBindingsChanged and the caller rolls back the
// whole transaction (409 conflict, retry).
//
// The by-ID pass relies on role bindings being immutable: a change to a
// binding's role, principal or scope is always a delete plus a create with a
// new ID (replaceBindingTx, SetMemberRoles and TransferOwnership all work this
// way, and the store has no UpdateRoleBinding). The only in-place UPDATE of
// role_bindings today is the startup membership_kind backfill in
// runMembershipMigration, which runs before the server serves requests and
// changes neither role, principal nor scope, so it is harmless. As hardening,
// each listed binding is re-read in the transaction just before its by-ID
// delete; if its role definition, principal or scope no longer matches the
// listed one (an in-place change under the same ID), the function returns
// errUserRoleBindingsChanged instead of deleting a binding the guard never
// checked. The validity window (NotBefore/ExpiresAt) is not compared, although
// it does feed the guard: whether the target's own owner binding is usable
// (removedUsable in userDeleteOrphansProjectTx) depends on its window, read
// from the pre-lock list. That is safe only because bindings are immutable: a
// window change is a delete plus a create with a new ID, which the by-ID
// re-read catches (the listed ID is gone, and the predicate delete below then
// finds the new ID and aborts with errUserRoleBindingsChanged). On
// PostgreSQL an in-place change that commits between that re-read and the
// delete is still not detected, so the immutability invariant remains the
// primary guarantee.
//
// The by-ID pass deletes in binding ID order. On PostgreSQL a concurrent
// change that deletes several of the user's bindings (for example
// replaceBindingTx on a multi-role member) can still deadlock with this pass;
// the database aborts one side, so either the delete returns 500 or the
// other change fails, and no data is corrupted.
//
// What is guaranteed: a concurrent grant or role change to the user's
// bindings that commits before the predicate delete aborts the delete with
// 409 (a concurrent revoke is ignored and the delete proceeds). A grant
// that commits after that statement but before the delete transaction
// commits is not detected and can leave a stale binding on the deleted user;
// that residual race is tracked in ptone/scion#2769.
//
// On denial it returns *lastProjectOwnerDeleteError listing every affected
// project. role_bindings.principal_id has no foreign key, so without the
// cascade the bindings would dangle after the user is deleted.
func guardAndCascadeUserRoleBindingsTx(ctx context.Context, tx store.Store, userID string, now time.Time) error {
	return guardAndCascadeUserRoleBindingsCheckTx(ctx, tx, userID, now, nil)
}

// guardAndCascadeUserRoleBindingsCheckTx is guardAndCascadeUserRoleBindingsTx
// with a check of the listed bindings. A non-nil check runs on exactly the
// bindings this function then deletes by ID, before any is deleted; its
// error aborts the delete. A binding added after the list is never deleted:
// the predicate delete below finds it and aborts the delete with
// errUserRoleBindingsChanged, or, when it commits after that statement, it
// is left in place.
func guardAndCascadeUserRoleBindingsCheckTx(ctx context.Context, tx store.Store, userID string, now time.Time, check userBindingsCheck) error {
	bindings, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return fmt.Errorf("list role bindings: %w", err)
	}
	if check != nil {
		if err := check(ctx, tx, userID, bindings); err != nil {
			return err
		}
	}

	var ownerProjectIDs []string
	var ownerRD *store.RoleDefinition
	if len(bindings) > 0 {
		ownerRD, err = tx.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
		if err != nil {
			return fmt.Errorf("resolve project-owner role definition: %w", err)
		}
		if ownerRD == nil {
			return fmt.Errorf("resolve project-owner role definition: not found")
		}
		seen := make(map[string]bool)
		for _, b := range bindings {
			if b.ScopeType != store.RoleScopeProject || b.RoleDefinitionID != ownerRD.ID || seen[b.ScopeID] {
				continue
			}
			seen[b.ScopeID] = true
			ownerProjectIDs = append(ownerProjectIDs, b.ScopeID)
		}
		sort.Strings(ownerProjectIDs)
	}

	var orphaned []lastOwnerProjectRef
	for _, projectID := range ownerProjectIDs {
		if err := tx.LockProjectForMembership(ctx, projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Binding on a project that no longer exists: nothing to
				// orphan, the cascade below removes the stale binding.
				continue
			}
			return fmt.Errorf("lock project %s: %w", projectID, err)
		}
		denied, err := userDeleteOrphansProjectTx(ctx, tx, projectID, ownerRD.ID, bindings, userID, now)
		if err != nil {
			return err
		}
		if !denied {
			continue
		}
		ref := lastOwnerProjectRef{ID: projectID}
		if p, err := tx.GetProject(ctx, projectID); err == nil && p != nil {
			ref.Name = p.Name
		} else if err != nil {
			slog.Debug("last-owner delete guard: project name lookup failed",
				"project_id", projectID, "error", err)
		}
		orphaned = append(orphaned, ref)
	}
	if len(orphaned) > 0 {
		return &lastProjectOwnerDeleteError{projects: orphaned}
	}

	// Delete the listed bindings by ID, then require the predicate delete to
	// find nothing: any row it removes is a binding the guard never saw.
	// Always run the predicate delete, even when the list was empty, so a
	// binding granted concurrently after the list is detected.
	// Deterministic lock order for the by-ID pass (see the doc comment).
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	for _, b := range bindings {
		// Re-read the binding so an in-place change under the same ID is
		// not deleted unchecked (see the immutability note above).
		cur, err := tx.GetRoleBinding(ctx, b.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Revoked concurrently; see below.
				continue
			}
			return fmt.Errorf("re-read role binding %s: %w", b.ID, err)
		}
		if cur.RoleDefinitionID != b.RoleDefinitionID || cur.ScopeType != b.ScopeType ||
			cur.ScopeID != b.ScopeID || cur.PrincipalType != b.PrincipalType || cur.PrincipalID != b.PrincipalID {
			return fmt.Errorf("%w: binding %s changed in place", errUserRoleBindingsChanged, b.ID)
		}
		if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Revoked concurrently; it can only be on a project the
				// user does not own (owned projects are locked above).
				continue
			}
			return fmt.Errorf("delete role binding %s: %w", b.ID, err)
		}
	}
	n, err := tx.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return fmt.Errorf("delete role bindings: %w", err)
	}
	if n != 0 {
		return fmt.Errorf("%w: %d unlisted binding(s) found", errUserRoleBindingsChanged, n)
	}
	return nil
}

// userDeleteOrphansProjectTx applies the last-owner rule of
// ptone/scion#2769 to the deletion of userID, for one project it owns,
// inside the delete transaction and after LockProjectForMembership on that
// project. userBindings are userID's role bindings; ownerRDID is the
// project-owner role definition. The deletion orphans the project, and is
// denied, when:
//
//   - one of userID's owner bindings on the project is usable (active window,
//     user exists and is active) and no other usable owner remains (I1), or
//   - no other owner binding of any kind remains (I2, the ptone/scion#2554
//     floor: zero owner bindings re-arms the startup creator backfill).
//
// So deleting a suspended co-owner is allowed while another owner binding
// remains, even when the project has no usable owner left. Lookup errors
// are returned (500, nothing changed).
func userDeleteOrphansProjectTx(ctx context.Context, tx store.Store, projectID, ownerRDID string, userBindings []*store.RoleBinding, userID string, now time.Time) (bool, error) {
	var own []*store.RoleBinding
	for _, b := range userBindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == ownerRDID {
			own = append(own, b)
		}
	}
	// ownerRDID is passed through, so the role definition is not resolved
	// again for every owned project.
	removedUsable := false
	for _, b := range own {
		ok, err := bindingIsUsableOwner(ctx, tx, b, ownerRDID, now)
		if err != nil {
			return false, fmt.Errorf("check owner bindings of project %s: %w", projectID, err)
		}
		if ok {
			removedUsable = true
			break
		}
	}
	if removedUsable {
		ok, err := projectHasUsableOwner(ctx, tx, projectID, now, userID)
		if err != nil {
			return false, fmt.Errorf("check usable owners of project %s: %w", projectID, err)
		}
		// A usable other owner is itself another owner binding, so I2
		// holds too and the binding count is not needed.
		return !ok, nil
	}
	others, err := projectOwnerBindingCount(ctx, tx, projectID, userID)
	if err != nil {
		return false, fmt.Errorf("count owner bindings of project %s: %w", projectID, err)
	}
	return others == 0, nil
}

// errUserRoleBindingsChanged is returned by guardAndCascadeUserRoleBindingsTx
// when the cascade finds a binding the guard did not list, meaning the
// user's bindings changed concurrently. Callers map it to
// 409 conflict; the transaction rolls back so nothing is deleted.
var errUserRoleBindingsChanged = errors.New("the user's role bindings changed concurrently; retry")

// writeUserRoleBindingsChangedError writes the 409 conflict response for
// errUserRoleBindingsChanged.
func writeUserRoleBindingsChangedError(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, ErrCodeConflict, errUserRoleBindingsChanged.Error(), nil)
}

// membershipNow returns the membership service clock, so the delete guard
// and the members API agree on which bindings are active, including under an
// injected clock. It falls back to the wall clock if the service is unset.
func (s *Server) membershipNow() time.Time {
	if s.membershipService != nil && s.membershipService.nowFunc != nil {
		return s.membershipService.nowFunc()
	}
	return time.Now()
}

// writeConstraintAdminLockoutError writes a constraint-admin lockout
// refusal with the status the role-binding path uses
// (governanceErrorStatus) and reports whether err was one.
func writeConstraintAdminLockoutError(w http.ResponseWriter, err error) bool {
	var ge *GovernanceError
	if !errors.As(err, &ge) || ge.Code != ErrCodeConstraintAdminLockout {
		return false
	}
	writeError(w, governanceErrorStatus(ge.Code), ge.Code, ge.Message, ge.Details)
	return true
}
