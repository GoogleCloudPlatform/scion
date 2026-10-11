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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestLoginRequest is the request body for POST /api/v1/auth/test-login.
type TestLoginRequest struct {
	Email       string `json:"email"`
	Role        string `json:"role"`
	DisplayName string `json:"displayName"`
	// CreateOnly, when true, makes the call fail with 409 Conflict if a
	// user with Email already exists, without changing that user. Default
	// false keeps the find-or-create behaviour.
	CreateOnly bool `json:"createOnly"`
}

// testLoginMutationType is the mutation_audits type recorded for every
// successful test-login call.
const testLoginMutationType = "test_login"

// Actor attribution for test_login audit rows written without an identity
// in the request context.
const (
	testLoginAuditActorID        = "test-login"
	testLoginAuditCredentialType = "test_login_challenge"
)

// TestLoginResponse is the response for POST /api/v1/auth/test-login.
type TestLoginResponse struct {
	User         *UserResponse `json:"user"`
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken"`
	ExpiresIn    int64         `json:"expiresIn"`
	// Created is true when this call created the user row, false when it
	// signed in as an existing user.
	Created bool `json:"created"`
}

// handleTestLogin handles POST /api/v1/auth/test-login.
// It provisions a test user and creates a web session, bypassing OAuth.
// Gated behind --enable-test-login (WebServerConfig.EnableTestLogin).
func (ws *WebServer) handleTestLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	if !ws.config.EnableTestLogin {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "test-login is not enabled", nil)
		return
	}

	if ws.store == nil || ws.userTokenSvc == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "hub services not available", nil)
		return
	}

	// Per-source-IP rate limit, applied before the challenge token is
	// checked so rejected calls count too. The key is the connection's
	// remote address (IPv6 by /64): forwarding headers are client-controlled
	// and the web server has no trusted-proxy configuration to vet them.
	if !ws.testLoginLimiter.Allow(testLoginRateKey(r.RemoteAddr)) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "too many test-login requests", nil)
		return
	}

	// Validate test-login challenge token.
	// Callers must present a short-lived JWT signed with the hub's user
	// signing key and scoped to the "scion-test-login" audience.
	// Per RFC 7235 the auth scheme is case-insensitive; we also tolerate
	// multiple spaces between scheme and token via strings.Fields.
	authHeader := r.Header.Get("Authorization")
	authParts := strings.Fields(authHeader)
	if len(authParts) != 2 || !strings.EqualFold(authParts[0], "bearer") {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authorization required: Bearer <test-login-token>", nil)
		return
	}
	challengeToken := authParts[1]
	if err := ws.userTokenSvc.ValidateTestLoginToken(challengeToken); err != nil {
		slog.Debug("test-login: invalid challenge token", "error", err)
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid test-login token", nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req TestLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.Email == "" {
		ValidationError(w, "email is required", nil)
		return
	}

	if !strings.Contains(req.Email, "@") {
		ValidationError(w, "email must contain @", nil)
		return
	}

	// The reserved hub test-identity domain is created only by the
	// issuance endpoint. Checked on the requested email before any lookup
	// or write, so it refuses creating such a user as well as signing in
	// as an existing one. Other domains, including the one test-login
	// callers use for their own synthetic users, are unaffected.
	if isReservedTestIdentityEmail(req.Email) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "test-login cannot sign in as this user", nil)
		return
	}

	switch req.Role {
	case "admin", "member", "viewer":
	case "":
		req.Role = "member"
	default:
		ValidationError(w, "role must be admin, member, or viewer", nil)
		return
	}

	displayNameProvided := req.DisplayName != ""
	if req.DisplayName == "" {
		req.DisplayName = req.Email
	}

	ctx := r.Context()

	// Find or create user. Every check that can refuse the call runs
	// before the first write, so a refused call leaves the user row, its
	// hub grants and the audit table untouched.
	existing, err := ws.store.GetUserByEmail(ctx, req.Email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("test-login: failed to look up user", "email", req.Email, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to look up user", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		existing = nil
	}
	if existing != nil {
		if req.CreateOnly {
			writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists", nil)
			return
		}
		if testLoginRefusesUser(existing) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "test-login cannot sign in as this user", nil)
			return
		}
	}

	var (
		user    *store.User
		oldRole string
		created = existing == nil
	)
	now := time.Now()
	if created {
		user = &store.User{
			ID:          generateID(),
			Email:       req.Email,
			DisplayName: req.DisplayName,
			Role:        req.Role,
			Status:      "active",
			Created:     now,
			LastLogin:   now,
		}
	} else {
		// Work on a copy so the row read above stays as loaded until the
		// transaction commits.
		u := *existing
		user = &u
		oldRole = user.Role
		user.LastLogin = now
		user.Role = req.Role
		if displayNameProvided {
			user.DisplayName = req.DisplayName
		}
	}

	// The user write and its audit record commit together: a call that
	// returns success always has exactly one test_login audit row.
	auditActor := auditActorFromContext(ctx)
	err = ws.store.WithTx(ctx, func(tx store.Store) error {
		if created {
			if err := tx.CreateUser(ctx, user); err != nil {
				return err
			}
		} else if err := tx.UpdateUser(ctx, user); err != nil {
			return err
		}
		record := testLoginAuditRecord(user, oldRole, created, now)
		if auditActor.PrincipalKind == "" {
			// test-login is not behind the hub's authentication
			// middleware, so there is usually no identity in context.
			// Attribute the write to the endpoint itself, authorised by a
			// test-login challenge credential (recorded by type only).
			record.ActorPrincipalKind = "system"
			record.ActorPrincipalID = testLoginAuditActorID
			record.ActorCredentialType = testLoginAuditCredentialType
		}
		auditActor.ApplyActor(record)
		return tx.CreateMutationAudit(ctx, record)
	})
	if err != nil {
		if created && req.CreateOnly && errors.Is(err, store.ErrAlreadyExists) {
			// A concurrent call created the user first; the transaction
			// rolled back, so nothing was written.
			writeError(w, http.StatusConflict, ErrCodeConflict, "user already exists", nil)
			return
		}
		slog.Error("test-login: failed to save user", "email", req.Email, "created", created, "error", err)
		msg := "failed to update user"
		if created {
			msg = "failed to create user"
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, msg, nil)
		return
	}

	// Make the hub-members group and hub-viewer binding match the stored role
	// (mirrors the other login paths). Best-effort: a failure is logged and
	// the login continues.
	if err := syncHubRoleGrants(ctx, ws.store, user.ID, user.Role, store.SystemReconcileCreatedBy); err != nil {
		slog.Warn("test-login: failed to sync hub role grants", "email", req.Email, "user_id", user.ID, "role", user.Role, "error", err)
	}

	// Generate tokens
	accessToken, refreshToken, expiresIn, err := ws.userTokenSvc.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	if err != nil {
		slog.Error("test-login: failed to generate tokens", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to generate tokens", nil)
		return
	}

	// Populate session cookie (same pattern as handleOAuthCallback)
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		session, _ = ws.sessionStore.New(r, webSessionName)
	}

	session.Values[sessKeyUserID] = user.ID
	session.Values[sessKeyUserEmail] = user.Email
	session.Values[sessKeyUserName] = user.DisplayName
	session.Values[sessKeyUserAvatar] = ""
	session.Values[sessKeyUserRole] = user.Role
	session.Values[sessKeyHubAccessToken] = accessToken
	session.Values[sessKeyHubRefreshToken] = refreshToken
	session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()

	if err := session.Save(r, w); err != nil {
		slog.Error("test-login: failed to save session", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save session", nil)
		return
	}

	writeJSON(w, http.StatusOK, TestLoginResponse{
		User: &UserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
		},
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		Created:      created,
	})
}

// testLoginAuditRecord builds the mutation_audits row for a successful
// test-login call. It records the user and the role before and after the
// call (no before summary when the call created the user). It carries no
// token material and no personal data.
func testLoginAuditRecord(user *store.User, oldRole string, created bool, at time.Time) *store.MutationAuditRecord {
	// Only the role (and, after the call, whether it created the row) is
	// recorded: the user is identified by TargetID, and no personal data
	// is copied into the audit table, which outlives the user.
	type summary struct {
		Role    string `json:"role"`
		Created *bool  `json:"created,omitempty"`
	}
	record := &store.MutationAuditRecord{
		MutationType: testLoginMutationType,
		TargetType:   "user",
		TargetID:     user.ID,
		Timestamp:    at,
	}
	if !created {
		b, _ := json.Marshal(summary{Role: oldRole})
		record.BeforeSummary = string(b)
	}
	b, _ := json.Marshal(summary{Role: user.Role, Created: &created})
	record.AfterSummary = string(b)
	return record
}

// testLoginRefusesUser reports whether test-login must refuse to sign in as
// an existing user row. It is called before any write. A hub test identity
// is refused: it authenticates only with its own hub-issued token, and
// test-login would otherwise overwrite its role. (The reserved test-identity
// domain is refused earlier, on the requested email, so creates are refused
// too.)
func testLoginRefusesUser(u *store.User) bool {
	return u.IsTestFixture()
}
