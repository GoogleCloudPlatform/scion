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
}

// TestLoginResponse is the response for POST /api/v1/auth/test-login.
type TestLoginResponse struct {
	User         *UserResponse `json:"user"`
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken"`
	ExpiresIn    int64         `json:"expiresIn"`
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
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.Email == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "email is required", nil)
		return
	}

	if !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "email must contain @", nil)
		return
	}

	switch req.Role {
	case "admin", "member", "viewer":
	case "":
		req.Role = "member"
	default:
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "role must be admin, member, or viewer", nil)
		return
	}

	displayNameProvided := req.DisplayName != ""
	if req.DisplayName == "" {
		req.DisplayName = req.Email
	}

	ctx := r.Context()

	// Find or create user
	user, err := ws.store.GetUserByEmail(ctx, req.Email)
	syncGrants := true
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("test-login: failed to look up user", "email", req.Email, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to look up user", nil)
		return
	}
	if err != nil {
		user = &store.User{
			ID:          generateID(),
			Email:       req.Email,
			DisplayName: req.DisplayName,
			Role:        req.Role,
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := ws.store.CreateUser(ctx, user); err != nil {
			slog.Error("test-login: failed to create user", "email", req.Email, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to create user", nil)
			return
		}
	} else {
		user.LastLogin = time.Now()
		user.Role = req.Role
		if displayNameProvided {
			user.DisplayName = req.DisplayName
		}
		if err := ws.store.UpdateUser(ctx, user); err != nil {
			slog.Warn("test-login: failed to update user", "email", req.Email, "error", err)
			syncGrants = false
		}
	}

	// Make the hub-members group and hub-viewer binding match the stored role
	// (mirrors the other login paths). Skipped when UpdateUser failed, so the
	// grants keep following the persisted role. Best-effort: a failure is
	// logged and the login continues.
	if syncGrants {
		if err := syncHubRoleGrants(ctx, ws.store, user.ID, user.Role, store.SystemReconcileCreatedBy); err != nil {
			slog.Warn("test-login: failed to sync hub role grants", "email", req.Email, "user_id", user.ID, "role", user.Role, "error", err)
		}
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
	})
}
