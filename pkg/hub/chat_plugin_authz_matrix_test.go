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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
// outsider owns, and plugin credentials for a hub-managed plugin.
type chatMatrixEnv struct {
	srv *Server

	projectID      string
	ownerEmail     string
	outsiderEmail  string
	outsiderProjID string
	agentSlug      string

	auth *apiclient.HMACAuth
}

func setupChatMatrixEnv(t *testing.T) *chatMatrixEnv {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("chat-matrix-owner")
	projectID := tid("chat-matrix-project")
	rs4Project(t, s, projectID, ownerID)

	// The outsider is an active hub user with no role in projectID, but owns
	// a project of their own.
	outsiderID := tid("chat-matrix-outsider")
	outsiderProjID := tid("chat-matrix-outsider-project")
	rs4Project(t, s, outsiderProjID, outsiderID)

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

// chatMatrixErrorBody decodes a hub error response.
func chatMatrixErrorBody(t *testing.T, w *httptest.ResponseRecorder) (code, message string, details map[string]interface{}) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp.Error.Code, resp.Error.Message, resp.Error.Details
}

func TestChatPluginAuthzMatrix(t *testing.T) {
	env := setupChatMatrixEnv(t)
	const noUser = ""

	agentsPath := "/api/v1/projects/" + env.projectID + "/agents"

	t.Run("project agent list", func(t *testing.T) {
		t.Run("request without the linked user is denied", func(t *testing.T) {
			w := env.do(t, http.MethodGet, agentsPath, noUser, nil)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			_, _, details := chatMatrixErrorBody(t, w)
			assert.Equal(t, "list", details["denied_action"], w.Body.String())
		})
		t.Run("request with the linked owner lists agents", func(t *testing.T) {
			w := env.do(t, http.MethodGet, agentsPath, env.ownerEmail, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, chatMatrixListIDs(t, w, "agents"), tid("chat-matrix-agent"))
		})
		t.Run("request with a linked outsider is denied", func(t *testing.T) {
			w := env.do(t, http.MethodGet, agentsPath, env.outsiderEmail, nil)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
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
	})

	t.Run("project secrets", func(t *testing.T) {
		scopeQuery := "scope=project&scopeId=" + env.projectID
		putBody := map[string]string{
			"value": "v", "encoding": "raw", "scope": "project", "scopeId": env.projectID,
		}
		calls := []struct {
			name   string
			method string
			path   string
			body   interface{}
		}{
			{"list", http.MethodGet, "/api/v1/secrets?" + scopeQuery, nil},
			{"get", http.MethodGet, "/api/v1/secrets/CHAT_MATRIX_KEY?" + scopeQuery, nil},
			{"put", http.MethodPut, "/api/v1/secrets/CHAT_MATRIX_KEY", putBody},
		}
		for _, c := range calls {
			t.Run(c.name+" without the linked user is denied", func(t *testing.T) {
				w := env.do(t, c.method, c.path, noUser, c.body)
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			})
			t.Run(c.name+" with the linked owner is not denied", func(t *testing.T) {
				// Tests run without a secret backend, so a permitted request
				// may still fail later; it must not be a 403.
				w := env.do(t, c.method, c.path, env.ownerEmail, c.body)
				assert.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
				assert.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
			})
			t.Run(c.name+" with a linked outsider is denied", func(t *testing.T) {
				w := env.do(t, c.method, c.path, env.outsiderEmail, c.body)
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
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
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
		t.Run("telegram link status", func(t *testing.T) {
			w := env.do(t, http.MethodGet, "/api/v1/telegram/link/status?telegram_user_id=t-123", noUser, nil)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	})

	t.Run("request with an unknown linked user is denied", func(t *testing.T) {
		w := env.do(t, http.MethodGet, agentsPath, "nobody-chat-matrix@test.com", nil)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		_, msg, _ := chatMatrixErrorBody(t, w)
		assert.Equal(t, "on-behalf-of principal not found", msg)
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
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		})
		t.Run("owner sender passes the sender check", func(t *testing.T) {
			w := send(t, "user:"+env.ownerEmail)
			// The agent is stopped, so an allowed sender gets 409.
			assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		})
		t.Run("outsider sender is denied", func(t *testing.T) {
			w := send(t, "user:"+env.outsiderEmail)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		})
	})
}
