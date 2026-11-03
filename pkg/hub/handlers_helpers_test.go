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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSpaceMembersLogs routes the default slog logger (warnings and
// above) into a buffer for the duration of the test.
//
// It swaps package-level state, so it must not be used from parallel tests.
func captureSpaceMembersLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prevLogger := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	return &buf
}

// setSpaceMembersMaxAgents overrides the members cap for the duration of the
// test.
//
// It swaps package-level state, so it must not be used from parallel tests.
func setSpaceMembersMaxAgents(t *testing.T, maxAgents int) {
	t.Helper()
	prevCap := spaceMembersMaxAgents
	spaceMembersMaxAgents = maxAgents
	t.Cleanup(func() { spaceMembersMaxAgents = prevCap })
}

const spaceMembersCapWarning = "agent list truncated at safety cap"

// tempSettingsHome points config.GetGlobalDir() at a temp directory holding a
// minimal settings.yaml, and returns the path to that file. It also isolates
// the shared log level state, because server-config saves and reloads
// apply server.log_level to it.
func tempSettingsHome(t *testing.T) string {
	t.Helper()
	isolateLogLevelState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte("schema_version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return settingsPath
}

const githubAppUpdateBody = `{"app_id":999,"api_base_url":"https://ghe.example.com/api/v3",` +
	`"webhooks_enabled":true,"installation_url":"https://github.com/apps/x"}`

// doTestLogin posts body to the test-login handler with a valid challenge
// token from remoteAddr ("" keeps httptest's default).
func doTestLogin(t *testing.T, ws *WebServer, svc *UserTokenService, body, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, svc))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	ws.handleTestLogin(rec, req)
	return rec
}

type testLoginStore struct {
	store.Store
	users       map[string]*store.User
	errOnLookup error
	errOnCreate error
}

// newTestLoginWebServer creates a WebServer for test-login tests and returns
// the UserTokenService so callers can mint challenge tokens.
func newTestLoginWebServer(t *testing.T, enableTestLogin bool) (*WebServer, *UserTokenService) {
	t.Helper()
	cfg := WebServerConfig{
		EnableTestLogin: enableTestLogin,
	}
	ws := NewWebServer(cfg)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetStore(newTestLoginStore())
	return ws, tokenSvc
}

// testLoginAuthHeader mints a valid test-login challenge token and returns
// the value for the Authorization header ("Bearer <token>").
func testLoginAuthHeader(t *testing.T, svc *UserTokenService) string {
	t.Helper()
	token, err := svc.GenerateTestLoginToken("test")
	require.NoError(t, err)
	return "Bearer " + token
}

// assertTestLoginJSONError checks that the handler wrote the hub's standard
// JSON error envelope with the given status, error code and message.
func assertTestLoginJSONError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	assert.Equal(t, status, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body must be a JSON error: %q", rec.Body.String())
	assert.Equal(t, code, resp.Error.Code)
	assert.Equal(t, message, resp.Error.Message)
}

func (s *testLoginStore) WithTx(_ context.Context, fn func(tx store.Store) error) error {
	return fn(s)
}

func (s *testLoginStore) CreateMutationAudit(context.Context, *store.MutationAuditRecord) error {
	return nil
}

func (s *testLoginStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if s.errOnLookup != nil {
		return nil, s.errOnLookup
	}
	if u, ok := s.users[email]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func (s *testLoginStore) CreateUser(_ context.Context, user *store.User) error {
	if s.errOnCreate != nil {
		return s.errOnCreate
	}
	s.users[user.Email] = user
	return nil
}

func (s *testLoginStore) UpdateUser(_ context.Context, user *store.User) error {
	s.users[user.Email] = user
	return nil
}

func (s *testLoginStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, fmt.Errorf("not found")
}

func (s *testLoginStore) GetRoleDefinitionByName(_ context.Context, _ string, _ string) (*store.RoleDefinition, error) {
	return nil, store.ErrNotFound
}

func newTestLoginStore() *testLoginStore {
	return &testLoginStore{users: make(map[string]*store.User)}
}
