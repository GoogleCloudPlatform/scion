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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Chat-plugin request matrix (ptone/scion#3197)
//
// Chat plugins (Slack, Telegram, Teams, Discord) call the hub as a broker
// using the credentials a hub-managed plugin is given. Each request is sent
// either without a linked user, or with the linked user in the
// X-Scion-On-Behalf-Of header ("user:<email>"). This matrix records which
// plugin request patterns the hub answers, for each of those cases, so a
// change to access rules that breaks a chat plugin is caught here.
// =============================================================================

// chatMatrixEnv is the shared fixture for the matrix: one project with an
// owner, an active outsider with no role in it, a second project the
// outsider owns, a suspended user, and plugin credentials for a hub-managed
// plugin.
type chatMatrixEnv struct {
	srv *Server

	projectID      string
	ownerEmail     string
	outsiderEmail  string
	outsiderProjID string
	suspendedEmail string
	agentSlug      string

	auth *apiclient.HMACAuth
}

func setupChatMatrixEnv(t *testing.T) *chatMatrixEnv {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	ownerID := tid("chat-matrix-owner")
	projectID := tid("chat-matrix-project")
	rs4Project(t, s, projectID, ownerID)

	// The outsider is an active hub user with no role in projectID, but owns
	// a project of their own.
	outsiderID := tid("chat-matrix-outsider")
	outsiderProjID := tid("chat-matrix-outsider-project")
	rs4Project(t, s, outsiderProjID, outsiderID)

	// A suspended hub user, used as a linked user and as a message sender.
	suspendedID := tid("chat-matrix-suspended")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: suspendedID, Email: suspendedID + "@test.com",
		DisplayName: "Suspended", Role: store.UserRoleMember, Status: store.UserStatusSuspended,
	}))

	// A stopped agent: an inbound message that passes the sender check is
	// then answered with 409 (agent not running), which keeps the test
	// independent of message dispatch.
	agentSlug := "chat-matrix-agent"
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:           tid("chat-matrix-agent"),
		Slug:         agentSlug,
		Name:         "Chat Matrix Agent",
		ProjectID:    projectID,
		OwnerID:      ownerID,
		Phase:        string(state.PhaseStopped),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}))

	// A project-scoped template, visible to project members.
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID:        tid("chat-matrix-template"),
		Name:      "chat-matrix-template",
		Slug:      "chat-matrix-template",
		Harness:   "claude",
		Scope:     store.TemplateScopeProject,
		ScopeID:   projectID,
		ProjectID: projectID,
		Status:    store.TemplateStatusActive,
		OwnerID:   ownerID,
		CreatedBy: ownerID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}))

	// A global template, visible to every caller.
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID:        tid("chat-matrix-global-template"),
		Name:      "chat-matrix-global-template",
		Slug:      "chat-matrix-global-template",
		Harness:   "claude",
		Scope:     store.TemplateScopeGlobal,
		Status:    store.TemplateStatusActive,
		CreatedBy: ownerID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}))

	// Plugin credentials, obtained the way a hub-managed plugin gets them.
	creds := srv.getPluginHubCreds(ctx, "chat-matrix-plugin")
	brokerID := creds["broker_id"]
	require.NotEmpty(t, brokerID)
	require.NotEmpty(t, creds["hmac_key"], "plugin credentials must include a key")
	key, err := base64.StdEncoding.DecodeString(creds["hmac_key"])
	require.NoError(t, err)

	return &chatMatrixEnv{
		srv:            srv,
		projectID:      projectID,
		ownerEmail:     ownerID + "@test.com",
		outsiderEmail:  outsiderID + "@test.com",
		outsiderProjID: outsiderProjID,
		suspendedEmail: suspendedID + "@test.com",
		agentSlug:      agentSlug,
		auth:           &apiclient.HMACAuth{BrokerID: brokerID, SecretKey: key},
	}
}

// do sends a plugin request through the full hub handler. linkedUser is the
// linked user's email, or "" for a request without the linked user.
func (env *chatMatrixEnv) do(t *testing.T, method, path, linkedUser string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if linkedUser != "" {
		// Same header set as the Discord plugin's hub client.
		req.Header.Set(HeaderOnBehalfOf, "user:"+linkedUser)
		req.Header.Set(HeaderSignedHeaders, "x-scion-on-behalf-of")
	}
	require.NoError(t, env.auth.ApplyAuth(req))
	w := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w, req)
	return w
}

// chatMatrixListIDs decodes a list response and returns the "id" of each item in the
// named array field.
func chatMatrixListIDs(t *testing.T, w *httptest.ResponseRecorder, field string) []string {
	t.Helper()
	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	raw, ok := resp[field]
	if !ok || string(raw) == "null" {
		return nil
	}
	var items []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &items), string(raw))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// chatMatrixSecretKeys returns the keys in a secret list response.
func chatMatrixSecretKeys(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp ListSecretsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	keys := make([]string, 0, len(resp.Secrets))
	for _, sec := range resp.Secrets {
		keys = append(keys, sec.Key)
	}
	return keys
}

// assertChatMatrixError asserts a hub error response's status, code and,
// when wantMessagePrefix is non-empty, the start of its message.
func assertChatMatrixError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode, wantMessagePrefix string) ErrorResponse {
	t.Helper()
	require.Equal(t, wantStatus, w.Code, w.Body.String())
	resp := parseErrorResponse(t, w.Body.Bytes())
	assert.Equal(t, wantCode, resp.Error.Code, w.Body.String())
	if wantMessagePrefix != "" {
		assert.True(t, strings.HasPrefix(resp.Error.Message, wantMessagePrefix),
			"message %q should start with %q", resp.Error.Message, wantMessagePrefix)
	}
	return resp
}

func TestChatPluginAuthzMatrix(t *testing.T) {
	env := setupChatMatrixEnv(t)
	const noUser = ""

	agentsPath := "/api/v1/projects/" + env.projectID + "/agents"

	t.Run("project agent list", func(t *testing.T) {
		t.Run("request without the linked user is denied", func(t *testing.T) {
			w := env.do(t, http.MethodGet, agentsPath, noUser, nil)
			resp := assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "")
			assertStructuredDenial(t, resp, "agent", "list")
		})
		t.Run("request with the linked owner lists agents", func(t *testing.T) {
			w := env.do(t, http.MethodGet, agentsPath, env.ownerEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, chatMatrixListIDs(t, w, "agents"), tid("chat-matrix-agent"))
		})
		t.Run("request with a linked outsider is denied", func(t *testing.T) {
			w := env.do(t, http.MethodGet, agentsPath, env.outsiderEmail, nil)
			resp := assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "")
			assertStructuredDenial(t, resp, "agent", "list")
		})
	})

	t.Run("user project list", func(t *testing.T) {
		t.Run("request without the linked user returns no projects", func(t *testing.T) {
			w := env.do(t, http.MethodGet, "/api/v1/projects", noUser, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Empty(t, chatMatrixListIDs(t, w, "projects"))
		})
		t.Run("request with the linked owner returns the owner's projects", func(t *testing.T) {
			w := env.do(t, http.MethodGet, "/api/v1/projects", env.ownerEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			ids := chatMatrixListIDs(t, w, "projects")
			assert.Contains(t, ids, env.projectID)
			assert.NotContains(t, ids, env.outsiderProjID)
		})
		t.Run("request with a linked outsider returns only the outsider's projects", func(t *testing.T) {
			w := env.do(t, http.MethodGet, "/api/v1/projects", env.outsiderEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			ids := chatMatrixListIDs(t, w, "projects")
			assert.Contains(t, ids, env.outsiderProjID)
			assert.NotContains(t, ids, env.projectID)
		})
	})

	t.Run("broker project list returns all projects", func(t *testing.T) {
		for name, user := range map[string]string{
			"without the linked user": noUser,
			"with the linked owner":   env.ownerEmail,
			"with a linked outsider":  env.outsiderEmail,
		} {
			t.Run(name, func(t *testing.T) {
				w := env.do(t, http.MethodGet, "/api/v1/broker/projects", user, nil)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				ids := chatMatrixListIDs(t, w, "projects")
				assert.Contains(t, ids, env.projectID)
				assert.Contains(t, ids, env.outsiderProjID)
			})
		}
	})

	t.Run("global template list is allowed", func(t *testing.T) {
		for name, user := range map[string]string{
			"without the linked user": noUser,
			"with the linked owner":   env.ownerEmail,
			"with a linked outsider":  env.outsiderEmail,
		} {
			t.Run(name, func(t *testing.T) {
				w := env.do(t, http.MethodGet, "/api/v1/templates?scope=global&status=active", user, nil)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Contains(t, chatMatrixListIDs(t, w, "templates"), tid("chat-matrix-global-template"))
			})
		}
	})

	t.Run("project template list", func(t *testing.T) {
		path := "/api/v1/templates?scope=project&projectId=" + env.projectID + "&status=active"
		t.Run("request without the linked user returns no templates", func(t *testing.T) {
			w := env.do(t, http.MethodGet, path, noUser, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Empty(t, chatMatrixListIDs(t, w, "templates"))
		})
		t.Run("request with the linked owner returns the project template", func(t *testing.T) {
			w := env.do(t, http.MethodGet, path, env.ownerEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, chatMatrixListIDs(t, w, "templates"), tid("chat-matrix-template"))
		})
		t.Run("request with a linked outsider does not return the project template", func(t *testing.T) {
			w := env.do(t, http.MethodGet, path, env.outsiderEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.NotContains(t, chatMatrixListIDs(t, w, "templates"), tid("chat-matrix-template"))
		})
	})

	t.Run("project secrets", func(t *testing.T) {
		scopeQuery := "scope=project&scopeId=" + env.projectID
		putBody := map[string]string{
			"value": "v", "encoding": "raw", "scope": "project", "scopeId": env.projectID,
		}
		putPath := "/api/v1/secrets/CHAT_MATRIX_KEY"
		getPath := "/api/v1/secrets/CHAT_MATRIX_KEY?" + scopeQuery
		listPath := "/api/v1/secrets?" + scopeQuery

		t.Run("put, get and list with the linked owner", func(t *testing.T) {
			// One subtest, so it does not depend on another subtest's put.
			w := env.do(t, http.MethodPut, putPath, env.ownerEmail, putBody)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var putResp SetSecretResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &putResp), w.Body.String())
			assert.True(t, putResp.Created, w.Body.String())

			w = env.do(t, http.MethodGet, getPath, env.ownerEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var got store.Secret
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), w.Body.String())
			assert.Equal(t, "CHAT_MATRIX_KEY", got.Key)

			w = env.do(t, http.MethodGet, listPath, env.ownerEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, chatMatrixSecretKeys(t, w), "CHAT_MATRIX_KEY")
		})

		calls := []struct {
			name   string
			method string
			path   string
			body   interface{}
		}{
			{"list", http.MethodGet, listPath, nil},
			{"get", http.MethodGet, getPath, nil},
			{"put", http.MethodPut, putPath, putBody},
		}
		// Pins the current response shape for secret denials.
		for _, c := range calls {
			t.Run(c.name+" without the linked user is denied", func(t *testing.T) {
				w := env.do(t, c.method, c.path, noUser, c.body)
				resp := assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "")
				assert.Empty(t, resp.Error.Details, w.Body.String())
			})
			t.Run(c.name+" with a linked outsider is denied", func(t *testing.T) {
				w := env.do(t, c.method, c.path, env.outsiderEmail, c.body)
				resp := assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "")
				assert.Empty(t, resp.Error.Details, w.Body.String())
			})
		}
	})

	t.Run("account link endpoints accept requests without the linked user", func(t *testing.T) {
		t.Run("discord link", func(t *testing.T) {
			w := env.do(t, http.MethodPost, "/api/v1/discord/link", noUser,
				map[string]string{"code": "CHATMATRIX1", "discordUserId": "d-123"})
			assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		})
		t.Run("telegram link", func(t *testing.T) {
			w := env.do(t, http.MethodPost, "/api/v1/telegram/link", noUser,
				map[string]string{"code": "CHATMATRIX2", "telegramUserId": "t-123"})
			assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		})
		t.Run("discord link status", func(t *testing.T) {
			w := env.do(t, http.MethodGet, "/api/v1/discord/link/status?discord_user_id=d-123", noUser, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.JSONEq(t, `{"status":"pending"}`, w.Body.String())
		})
		t.Run("telegram link status", func(t *testing.T) {
			w := env.do(t, http.MethodGet, "/api/v1/telegram/link/status?telegram_user_id=t-123", noUser, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.JSONEq(t, `{"status":"pending"}`, w.Body.String())
		})
	})

	t.Run("request with an unknown linked user is denied", func(t *testing.T) {
		w := env.do(t, http.MethodGet, agentsPath, "nobody-chat-matrix@test.com", nil)
		resp := assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "")
		assert.Equal(t, "on-behalf-of principal not found", resp.Error.Message)
	})

	t.Run("request with a suspended linked user is denied", func(t *testing.T) {
		w := env.do(t, http.MethodGet, agentsPath, env.suspendedEmail, nil)
		assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "on-behalf-of principal is not active")
	})

	t.Run("inbound message sender", func(t *testing.T) {
		send := func(t *testing.T, sender string) *httptest.ResponseRecorder {
			return env.do(t, http.MethodPost, "/api/v1/broker/inbound", noUser, inboundMessageRequest{
				Topic: "scion.project." + env.projectID + ".agent." + env.agentSlug + ".messages",
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   "slack",
					Sender:    sender,
					Recipient: "agent:" + env.agentSlug,
					Msg:       "hello from chat",
					Type:      messages.TypeInstruction,
				},
			})
		}
		t.Run("sender without the user prefix is denied", func(t *testing.T) {
			w := send(t, "slack:U123")
			assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeMessageDenied, "")
		})
		t.Run("unknown user sender is denied", func(t *testing.T) {
			w := send(t, "user:nobody-chat-matrix@test.com")
			assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "sender identity could not be resolved")
		})
		t.Run("suspended user sender is denied", func(t *testing.T) {
			w := send(t, "user:"+env.suspendedEmail)
			assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeForbidden, "sender identity is not active")
		})
		t.Run("owner sender passes the sender check", func(t *testing.T) {
			w := send(t, "user:"+env.ownerEmail)
			// The agent is stopped, so an allowed sender gets 409.
			assertChatMatrixError(t, w, http.StatusConflict, ErrCodeAgentNotRunning, "")
		})
		t.Run("outsider sender is denied", func(t *testing.T) {
			w := send(t, "user:"+env.outsiderEmail)
			assertChatMatrixError(t, w, http.StatusForbidden, ErrCodeMessageDenied, "")
		})
	})
}
