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
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/sessions"
)

// viewAsMemberKey is the single context key marking a request whose browser
// session is operating in "View as member" mode (ptone/scion#8648). Its value
// is the caller's user ID.
type viewAsMemberKey struct{}

// contextWithViewAsMember marks ctx as evaluating in "View as member" mode for
// userID. Only the two session entry points (UnifiedAuthMiddleware's
// tokenTypeUser arm and webSessionContextFromCookie) may call this function;
// an AST test enforces this invariant.
func contextWithViewAsMember(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, viewAsMemberKey{}, userID)
}

// viewAsMemberUserFromContext returns the user ID whose hub-level admin grants
// are clamped in ctx, if any.
func viewAsMemberUserFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	uid, ok := ctx.Value(viewAsMemberKey{}).(string)
	return uid, ok && uid != ""
}

// applyViewAsMemberFromContext returns u with ViewAsMember enabled when ctx
// carries the view-as-member marker for u's user ID. Request-path helpers that
// rebuild an AuthenticatedUser from a stored *store.User row call this so the
// rebuilt identity honours view-as-member mode (ptone/scion#8648).
func applyViewAsMemberFromContext(ctx context.Context, u *AuthenticatedUser) *AuthenticatedUser {
	if u == nil {
		return nil
	}
	if uid, ok := viewAsMemberUserFromContext(ctx); ok && uid == u.ID() {
		return u.WithViewAsMember()
	}
	return u
}

// unclampedUserForMaintenance returns user with view-as-member cleared when
// user is an *AuthenticatedUser in view-as-member mode. Used exclusively by
// maintenance-mode RealByDesign admission gates (D4) so a real admin in
// view-as-member stays admitted during maintenance and can always reach Exit.
func unclampedUserForMaintenance(user UserIdentity) UserIdentity {
	if au, ok := user.(*AuthenticatedUser); ok && au != nil && au.ViewAsMember() {
		return au.WithoutViewAsMember()
	}
	return user
}

// withoutViewAsMember shadows any ambient view-as-member marker on ctx so
// transition audit records own their SessionMode explicitly regardless of
// whether request middleware (such as devAuthMiddleware) attached the marker
// before the transition handler ran.
func withoutViewAsMember(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, viewAsMemberKey{}, "")
}

// viewAsMemberSessionModeFor returns viewAsMemberSessionMode when ctx carries
// the view-as-member marker for principalID (or when principalID is empty).
// Every audit and request-log site uses this helper so the principal-match
// rule stays consistent across call sites.
func viewAsMemberSessionModeFor(ctx context.Context, principalID string) string {
	if uid, ok := viewAsMemberUserFromContext(ctx); ok && (principalID == "" || principalID == uid) {
		return viewAsMemberSessionMode
	}
	return ""
}

var (
	errViewAsMemberDisabled  = errors.New("view as member experiment is disabled")
	errViewAsMemberForbidden = errors.New("view as member is restricted to hub admins")
)

const (
	viewAsMemberAuditEnter = "session.view_as_member.enter"
	viewAsMemberAuditExit  = "session.view_as_member.exit"

	viewAsMemberReasonUser    = "user"
	viewAsMemberReasonSignOut = "sign_out"

	viewAsMemberSessionMode = credentialmeta.SessionModeViewAsMember

	viewAsMemberMaxBodyBytes = 1024
)

// viewAsMemberHub is the narrow interface WebServer uses to delegate
// "View as member" policy, audit, and session-mode notifications to the Hub
// server so that experiment and authorization checks always live in the hub.
type viewAsMemberHub interface {
	// AuthorizeViewAsMemberChange returns nil if the change is allowed.
	//   enter=true:  experiment hub.view_as_member must be on (else
	//                errViewAsMemberDisabled -> 404); caller must be an active,
	//                human, non-test, non-federated real hub admin (else
	//                errViewAsMemberForbidden -> 403).
	//   enter=false: always allowed (even with the experiment off or after
	//                admin demotion), since exit only restores real authority.
	AuthorizeViewAsMemberChange(ctx context.Context, userID string, enter bool) error
	// RecordViewAsMemberTransition persists a synchronous mutation audit record
	// and emits a structured slog event for an enter or exit transition.
	RecordViewAsMemberTransition(ctx context.Context, userID string, enter bool, reason string)
	// PublishSessionModeChanged notifies open streams for userID that the
	// session mode changed so they reconnect and re-evaluate authority.
	PublishSessionModeChanged(ctx context.Context, userID string)
}

// AuthorizeViewAsMemberChange implements viewAsMemberHub on *Server.
func (s *Server) AuthorizeViewAsMemberChange(ctx context.Context, userID string, enter bool) error {
	if !enter {
		// Exiting only restores the user's real authority and is always allowed,
		// even if the experiment was turned off or the user was demoted.
		return nil
	}
	if !s.experimentEnabled(experiments.ViewAsMember) {
		return errViewAsMemberDisabled
	}
	if s.store == nil || userID == "" {
		return errViewAsMemberForbidden
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return errViewAsMemberForbidden
		}
		return err
	}
	if u.Status != store.UserStatusActive || isReservedTestIdentityEmail(u.Email) {
		return errViewAsMemberForbidden
	}
	kind, err := classifyUserKind(u)
	if err != nil || !kind.isHuman() {
		return errViewAsMemberForbidden
	}
	if s.testAdminPolicyRefusesUser(ctx, u.ID, "view_as_member.go:AuthorizeViewAsMemberChange") {
		return errViewAsMemberForbidden
	}

	// Evaluate real (unclamped) admin status using the same predicate as
	// GET /api/v1/auth/admin-status: IsUnscopedLocalPlatformAdmin ||
	// IsSystemAdmin || IsHubAdmin.
	realUser := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
	isAdmin := IsUnscopedLocalPlatformAdmin(realUser)
	if !isAdmin && s.authzService != nil {
		isAdmin = s.authzService.IsSystemAdmin(ctx, u.ID) || s.authzService.IsHubAdmin(ctx, u.ID)
	}
	if !isAdmin {
		return errViewAsMemberForbidden
	}
	return nil
}

// RecordViewAsMemberTransition implements viewAsMemberHub on *Server.
func (s *Server) RecordViewAsMemberTransition(ctx context.Context, userID string, enter bool, reason string) {
	// Shadow any ambient view-as-member marker so transition records own their
	// SessionMode explicitly (enter="view_as_member", exit="") even when
	// request middleware (such as devAuthMiddleware) attached the marker.
	ctx = withoutViewAsMember(ctx)
	mutationType := viewAsMemberAuditExit
	var sessionMode string
	var summaryBytes []byte
	if enter {
		mutationType = viewAsMemberAuditEnter
		sessionMode = viewAsMemberSessionMode
		summaryBytes, _ = json.Marshal(struct {
			ViewAsMember bool `json:"view_as_member"`
		}{ViewAsMember: true})
	} else {
		if reason == "" {
			reason = viewAsMemberReasonUser
		}
		summaryBytes, _ = json.Marshal(struct {
			ViewAsMember bool   `json:"view_as_member"`
			Reason       string `json:"reason"`
		}{ViewAsMember: false, Reason: reason})
	}

	s.emitMutationAuditSync(ctx, &store.MutationAuditRecord{
		MutationType:        mutationType,
		ActorPrincipalKind:  string(PrincipalKindUser),
		ActorPrincipalID:    userID,
		ActorCredentialType: string(CredentialKindInteractive),
		TargetType:          "user",
		TargetID:            userID,
		AfterSummary:        string(summaryBytes),
		SessionMode:         sessionMode,
	})

	attrs := []any{
		"user_id", userID,
		"view_as_member", enter,
		"mutation_type", mutationType,
	}
	if !enter && reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	slog.InfoContext(ctx, "audit.view_as_member", attrs...)
}

// PublishSessionModeChanged implements viewAsMemberHub on *Server.
func (s *Server) PublishSessionModeChanged(_ context.Context, userID string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	subject := membershipChangedSubjectFor(userID)
	if events == nil || subject == "" {
		return
	}
	events.PublishRaw(subject, membershipChangedEvent{
		UserID: userID,
		Reason: membershipChangedReasonSessionMode,
	})
}

// SetViewAsMemberHub connects WebServer to the Hub's view-as-member policy,
// audit, and session-mode notification methods.
func (ws *WebServer) SetViewAsMemberHub(h viewAsMemberHub) {
	ws.viewAsMemberHub = h
}

// sessionViewAsMember reports whether session has view-as-member enabled.
func sessionViewAsMember(session *sessions.Session) bool {
	if session == nil {
		return false
	}
	v, _ := session.Values[sessKeyViewAsMember].(bool)
	return v
}

// webSessionUserFromCookie constructs a webSessionUser from a validated
// scion_sess cookie. When sessKeyViewAsMember is set, Role reports the
// effective role ("viewer" if stored role is "viewer", otherwise "member")
// and ViewAsMember is true; sessKeyUserRole in the cookie keeps the stored role.
func (ws *WebServer) webSessionUserFromCookie(session *sessions.Session, uid, email string) *webSessionUser {
	if email == "" {
		email = sessionString(session, sessKeyUserEmail)
	}
	storedRole := sessionString(session, sessKeyUserRole)
	role := storedRole
	vam := sessionViewAsMember(session)
	if vam && role != store.UserRoleViewer {
		role = store.UserRoleMember
	}
	return &webSessionUser{
		UserID:       uid,
		Email:        email,
		Name:         sessionString(session, sessKeyUserName),
		AvatarURL:    sessionString(session, sessKeyUserAvatar),
		Role:         role,
		ViewAsMember: vam,
		storedRole:   storedRole,
	}
}

// webSessionContextFromCookie attaches the webSessionUser built from session to
// ctx, and installs the view-as-member context marker when the mode is active.
// This is the single web-side entry point that installs contextWithViewAsMember.
func (ws *WebServer) webSessionContextFromCookie(ctx context.Context, session *sessions.Session, uid, email string) context.Context {
	user := ws.webSessionUserFromCookie(session, uid, email)
	ctx = context.WithValue(ctx, webUserContextKey{}, user)
	if user.ViewAsMember {
		ctx = contextWithViewAsMember(ctx, user.UserID)
	}
	return ctx
}

// viewAsMemberRequest is the JSON request body for POST /auth/view-as-member.
type viewAsMemberRequest struct {
	Enabled *bool `json:"enabled"`
}

// viewAsMemberResponse is the JSON response body for POST /auth/view-as-member.
type viewAsMemberResponse struct {
	ViewAsMember bool `json:"viewAsMember"`
}

// handleViewAsMember handles POST /auth/view-as-member.
// It toggles the per-browser-session "View as member" mode in scion_sess and
// re-mints the session's Hub JWT pair with the matching vam claim.
func (ws *WebServer) handleViewAsMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return
	}
	if sfs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); sfs != "" &&
		!strings.EqualFold(sfs, "same-origin") && !strings.EqualFold(sfs, "none") {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "cross-site request is not allowed", nil)
		return
	}

	if ws.sessionStore == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}
	uid, _ := session.Values[sessKeyUserID].(string)
	if uid == "" {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}

	// Refuse test-identity sessions (never eligible).
	if res := ws.classifyTestIdentitySession(r, session, nil); res.isTestIdentitySession() {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "view as member is not available for test identities", nil)
		return
	}

	// Verify session generation and account status against the store when wired.
	if ws.store != nil {
		dbUser, uErr := ws.store.GetUser(r.Context(), uid)
		if uErr != nil {
			if errors.Is(uErr, store.ErrNotFound) || errors.Is(uErr, store.ErrInvalidInput) {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
				return
			}
			ws.logger().Error("view-as-member: user lookup failed", "user_id", uid, "error", uErr)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "internal error", nil)
			return
		}
		cookieGen, _ := session.Values[sessKeySessionGeneration].(int64)
		if dbUser.SessionGeneration != cookieGen {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "session has expired", nil)
			return
		}
		if dbUser.Status != store.UserStatusActive {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "account is not active", nil)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, viewAsMemberMaxBodyBytes+1))
	if err != nil || len(body) > viewAsMemberMaxBodyBytes {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}
	var req viewAsMemberRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "request body must be {\"enabled\": true|false}", nil)
		return
	}
	enter := *req.Enabled

	current := sessionViewAsMember(session)
	if current == enter {
		// Idempotent: no state change, no audit record, no publish.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, viewAsMemberResponse{ViewAsMember: current})
		return
	}

	if ws.viewAsMemberHub == nil {
		if enter {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "view as member is not available", nil)
			return
		}
	} else {
		if err := ws.viewAsMemberHub.AuthorizeViewAsMemberChange(r.Context(), uid, enter); err != nil {
			switch {
			case errors.Is(err, errViewAsMemberDisabled):
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "view as member is not enabled", nil)
			case errors.Is(err, errViewAsMemberForbidden):
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "view as member is restricted to hub admins", nil)
			default:
				ws.logger().Error("view-as-member: authorization check failed", "user_id", uid, "error", err)
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "internal error", nil)
			}
			return
		}
	}

	email := sessionString(session, sessKeyUserEmail)
	name := sessionString(session, sessKeyUserName)
	storedRole := sessionString(session, sessKeyUserRole)
	gen, _ := session.Values[sessKeySessionGeneration].(int64)

	if enter {
		session.Values[sessKeyViewAsMember] = true
	} else {
		delete(session.Values, sessKeyViewAsMember)
	}

	if ws.userTokenSvc != nil {
		accessToken, refreshToken, expiresIn, mintErr := ws.userTokenSvc.MintTokenPair(UserTokenSubject{
			UserID:            uid,
			Email:             email,
			DisplayName:       name,
			Role:              storedRole,
			SessionGeneration: gen,
			ClientType:        ClientTypeWeb,
			ViewAsMember:      enter,
		})
		if mintErr != nil {
			ws.logger().Error("view-as-member: failed to re-mint hub tokens", "user_id", uid, "error", mintErr)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update session tokens", nil)
			return
		}
		session.Values[sessKeyHubAccessToken] = accessToken
		session.Values[sessKeyHubRefreshToken] = refreshToken
		session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	}

	ws.applyDevSessionCookieOptions(session)
	if err := session.Save(r, w); err != nil {
		ws.logger().Error("view-as-member: failed to save session", "user_id", uid, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save session", nil)
		return
	}

	if ws.viewAsMemberHub != nil {
		ws.viewAsMemberHub.RecordViewAsMemberTransition(withoutViewAsMember(r.Context()), uid, enter, viewAsMemberReasonUser)
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, viewAsMemberResponse{ViewAsMember: enter})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Publish only AFTER the response (including Set-Cookie) has been written
	// and flushed so the toggling tab's SSE reconnect/reload cannot race ahead
	// of Set-Cookie and abort the in-flight POST.
	if ws.viewAsMemberHub != nil {
		ws.viewAsMemberHub.PublishSessionModeChanged(r.Context(), uid)
	}
}

// authUserViewAsMember reports whether user is an *AuthenticatedUser operating
// in "View as member" mode.
func authUserViewAsMember(user UserIdentity) bool {
	if au, ok := user.(*AuthenticatedUser); ok && au != nil {
		return au.ViewAsMember()
	}
	return false
}
