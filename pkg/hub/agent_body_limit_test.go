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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bodyLimitCountingReader struct {
	r         io.Reader
	bytesRead atomic.Int64
}

func newBodyLimitCountingReader(b []byte) *bodyLimitCountingReader {
	return &bodyLimitCountingReader{r: bytes.NewReader(b)}
}

func (c *bodyLimitCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytesRead.Add(int64(n))
	return n, err
}

func (c *bodyLimitCountingReader) Close() error { return nil }

func exactSizeJSON(t *testing.T, prefix, suffix string, targetBytes int) []byte {
	t.Helper()
	padLen := targetBytes - len(prefix) - len(suffix)
	require.GreaterOrEqual(t, padLen, 0, "prefix+suffix exceeds targetBytes")
	out := make([]byte, 0, targetBytes)
	out = append(out, prefix...)
	out = append(out, bytes.Repeat([]byte("a"), padLen)...)
	out = append(out, suffix...)
	require.Equal(t, targetBytes, len(out))
	return out
}

func assertPayloadTooLarge(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	assert.Equal(t, "payload_too_large", env.Error.Code)
	return env.Error.Message
}

func assertCreatePayloadTooLarge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	msg := assertPayloadTooLarge(t, rec)
	assert.Contains(t, msg, "16 MiB", "create 413 message should name the 16 MiB limit")
	assert.Contains(t, msg, "workspace file manifest", "create 413 message should mention the workspace file manifest")
}

func assertPassedBodyDecode(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code, "expected body within limit to pass decode: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "Invalid request body", "expected body within limit to decode cleanly: %s", rec.Body.String())
}

func doRawRequestWithUser(t *testing.T, srv *Server, user *store.User, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		token, _, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func setupAgentBodyLimitFixture(t *testing.T) (*Server, store.Store, *store.User, *store.User, *store.Project, *store.Agent, *store.Agent) {
	t.Helper()
	srv, s, owner, outsider, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	provAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "prov-body-limit",
		Name:      "prov-body-limit",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Phase:     string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, provAgent))

	runningAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "run-body-limit",
		Name:      "run-body-limit",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, runningAgent))

	return srv, s, owner, outsider, project, provAgent, runningAgent
}

func buildManifestCreateRequestBytes(t *testing.T, projectID, name string, targetBytes int, wantOver bool) []byte {
	t.Helper()
	hash := "sha256:" + strings.Repeat("a", 64)
	sample := transfer.FileInfo{
		Path: "node_modules/@scope/pkg-000000/dist/cjs/index.js",
		Size: 12345,
		Hash: hash,
		Mode: "0644",
	}
	sampleBytes, err := json.Marshal(sample)
	require.NoError(t, err)
	perEntry := len(sampleBytes) + 1 // +1 for JSON array comma

	baseReq := CreateAgentRequest{
		Name:      name,
		ProjectID: projectID,
		Template:  "default",
	}
	baseBytes, err := json.Marshal(baseReq)
	require.NoError(t, err)

	// Overhead for `"workspaceFiles":[]` plus comma.
	count := (targetBytes - len(baseBytes) - 32) / perEntry
	if wantOver {
		count += 5
	} else {
		count -= 2
	}
	require.Greater(t, count, 0)

	files := make([]transfer.FileInfo, count)
	for i := range files {
		files[i] = transfer.FileInfo{
			Path: fmt.Sprintf("node_modules/@scope/pkg-%06d/dist/cjs/index.js", i),
			Size: 12345,
			Hash: hash,
			Mode: "0644",
		}
	}
	baseReq.WorkspaceFiles = files
	raw, err := json.Marshal(baseReq)
	require.NoError(t, err)

	if wantOver {
		require.Greater(t, len(raw), targetBytes, "expected oversized manifest to exceed targetBytes")
		require.Less(t, len(raw), targetBytes+4096, "expected oversized manifest to be just over targetBytes")
	} else {
		require.LessOrEqual(t, len(raw), targetBytes, "expected under-limit manifest to fit within targetBytes")
		require.Greater(t, len(raw), targetBytes-4096, "expected under-limit manifest to be just under targetBytes")
	}
	return raw
}

func TestAgentBodyLimits_CreateAgent(t *testing.T) {
	srv, s, owner, outsider, project, _, _ := setupAgentBodyLimitFixture(t)

	routes := []struct {
		name              string
		path              string
		projectIDInBody   string
		authzBeforeDecode bool
	}{
		{
			name:              "top-level createAgent",
			path:              "/api/v1/agents",
			projectIDInBody:   project.ID,
			authzBeforeDecode: false,
		},
		{
			name:              "project-scoped createProjectAgent",
			path:              "/api/v1/projects/" + project.ID + "/agents",
			projectIDInBody:   project.ID,
			authzBeforeDecode: true,
		},
	}

	// Pin the 16 MiB ceiling behaviorally rather than deriving the test size from
	// maxAgentCreateBodySize, so lowering the constant (e.g. to 5 MiB) fails the
	// under-limit manifest test and raising it fails the over-limit test.
	const pinnedCreateBodyLimit = 16 << 20
	underManifest := buildManifestCreateRequestBytes(t, project.ID, "manifest-under-limit", pinnedCreateBodyLimit, false)
	overManifest := buildManifestCreateRequestBytes(t, project.ID, "manifest-over-limit", pinnedCreateBodyLimit, true)

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			prefix := fmt.Sprintf(`{"name":"body-limit-agent","projectId":%q,"template":"default","task":"`, rt.projectIDInBody)
			suffix := `"}`

			t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
				body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxAgentCreateBodySize+1024))
				rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, rt.path, body)
				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.Equal(t, int64(0), body.bytesRead.Load(), "unauthenticated request must not consume body")
			})

			if rt.authzBeforeDecode {
				t.Run("outsider oversized gets 403 and reads 0 bytes", func(t *testing.T) {
					body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxAgentCreateBodySize+1024))
					rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, rt.path, body)
					assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
					assert.Equal(t, int64(0), body.bytesRead.Load(), "unauthorized outsider must not consume body")
				})
			}

			t.Run("at-limit 16 MiB body passes decode", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxAgentCreateBodySize)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPassedBodyDecode(t, rec)
			})

			t.Run("just-above-limit 16 MiB + 1 byte returns actionable 413 payload_too_large", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxAgentCreateBodySize+1)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertCreatePayloadTooLarge(t, rec)
			})

			t.Run("large workspace manifest just under 16 MiB passes decode", func(t *testing.T) {
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(underManifest))
				assertPassedBodyDecode(t, rec)
			})

			t.Run("workspace manifest just over 16 MiB returns actionable 413 with no side effects", func(t *testing.T) {
				ctx := context.Background()
				before, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
				require.NoError(t, err)

				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(overManifest))
				assertCreatePayloadTooLarge(t, rec)

				after, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
				require.NoError(t, err)
				assert.Equal(t, len(before.Items), len(after.Items), "rejected over-limit manifest must not create an agent")
				_, err = s.GetAgentBySlug(ctx, project.ID, "manifest-over-limit")
				assert.ErrorIs(t, err, store.ErrNotFound, "rejected over-limit manifest must not persist agent slug")
			})

			t.Run("largest realistic payload with 50x64KiB base64 secrets passes decode", func(t *testing.T) {
				encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("s"), maxCreateAgentSecretValueBytes))
				secrets := make([]AgentSecretInput, maxCreateAgentSecrets)
				for i := range secrets {
					secrets[i] = AgentSecretInput{
						Key:   fmt.Sprintf("SECRET_%02d", i),
						Value: encoded,
					}
				}
				reqBody := CreateAgentRequest{
					Name:      "max-secrets-agent",
					ProjectID: rt.projectIDInBody,
					Template:  "default",
					Secrets:   secrets,
					Config: &api.ScionConfig{
						Env: map[string]string{
							"LARGE_CONFIG_VAR": strings.Repeat("c", 32*1024),
						},
					},
				}
				raw, err := json.Marshal(reqBody)
				require.NoError(t, err)
				require.LessOrEqual(t, len(raw), maxAgentCreateBodySize, "realistic max create payload must fit within maxAgentCreateBodySize")
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(raw))
				assertPassedBodyDecode(t, rec)
			})
		})
	}
}

func TestAgentBodyLimits_SubmitAgentEnv(t *testing.T) {
	srv, _, owner, outsider, project, provAgent, _ := setupAgentBodyLimitFixture(t)

	routes := []struct {
		name string
		path string
	}{
		{"top-level", "/api/v1/agents/" + provAgent.ID + "/env"},
		{"project-scoped", "/api/v1/projects/" + project.ID + "/agents/" + provAgent.Slug + "/env"},
	}

	prefix := `{"env":{"LARGE_VAR":"`
	suffix := `"}}`

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
				body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1024))
				rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, rt.path, body)
				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.Equal(t, int64(0), body.bytesRead.Load())
			})

			t.Run("outsider oversized is rejected and reads 0 bytes", func(t *testing.T) {
				body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1024))
				rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, rt.path, body)
				assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound,
					"expected 403 or 404 for outsider, got %d: %s", rec.Code, rec.Body.String())
				assert.Equal(t, int64(0), body.bytesRead.Load(), "unauthorized outsider must not consume body")
			})

			t.Run("at-limit 1 MiB body passes decode", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxSettingsBodySize)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPassedBodyDecode(t, rec)
			})

			t.Run("just-above-limit 1 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPayloadTooLarge(t, rec)
			})

			t.Run("realistic large env submission passes decode", func(t *testing.T) {
				envMap := make(map[string]string, 50)
				for i := 0; i < 50; i++ {
					envMap[fmt.Sprintf("APP_VAR_%02d", i)] = strings.Repeat("v", 2048)
				}
				raw, err := json.Marshal(SubmitEnvRequest{Env: envMap})
				require.NoError(t, err)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(raw))
				assertPassedBodyDecode(t, rec)
			})
		})
	}
}

func TestAgentBodyLimits_AgentExec(t *testing.T) {
	srv, _, owner, outsider, project, _, runningAgent := setupAgentBodyLimitFixture(t)

	routes := []struct {
		name string
		path string
	}{
		{"top-level", "/api/v1/agents/" + runningAgent.ID + "/exec"},
		{"project-scoped", "/api/v1/projects/" + project.ID + "/agents/" + runningAgent.Slug + "/exec"},
	}

	prefix := `{"command":["echo","`
	suffix := `"]}`

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
				body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1024))
				rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, rt.path, body)
				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.Equal(t, int64(0), body.bytesRead.Load())
			})

			t.Run("outsider oversized is rejected and reads 0 bytes", func(t *testing.T) {
				body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1024))
				rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, rt.path, body)
				assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound,
					"expected 403 or 404 for outsider, got %d: %s", rec.Code, rec.Body.String())
				assert.Equal(t, int64(0), body.bytesRead.Load(), "unauthorized outsider must not consume body")
			})

			t.Run("at-limit 1 MiB body passes decode", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxSettingsBodySize)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPassedBodyDecode(t, rec)
			})

			t.Run("just-above-limit 1 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPayloadTooLarge(t, rec)
			})

			t.Run("largest realistic 16 KiB exec argv passes decode", func(t *testing.T) {
				arg := strings.Repeat("x", agentcontrol.MaxExecArgBytes)
				raw, err := json.Marshal(map[string]any{
					"command": []string{"sh", "-c", arg, arg, arg},
					"timeout": 30,
				})
				require.NoError(t, err)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(raw))
				assertPassedBodyDecode(t, rec)
			})
		})
	}
}

func TestAgentBodyLimits_AgentMessage(t *testing.T) {
	srv, _, owner, _, project, _, runningAgent := setupAgentBodyLimitFixture(t)

	routes := []struct {
		name string
		path string
	}{
		{"top-level", "/api/v1/agents/" + runningAgent.ID + "/message"},
		{"project-scoped", "/api/v1/projects/" + project.ID + "/agents/" + runningAgent.Slug + "/message"},
	}

	prefix := `{"message":"hello","pad":"`
	suffix := `"}`

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
				body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, rawTombstonePreAuthMaxBodyBytes+1024))
				rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, rt.path, body)
				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.Equal(t, int64(0), body.bytesRead.Load())
			})

			t.Run("at-limit 1 MiB body passes decode", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxSettingsBodySize)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPassedBodyDecode(t, rec)
			})

			t.Run("just-above-limit 1 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, maxSettingsBodySize+1)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPayloadTooLarge(t, rec)
			})

			t.Run("above 2 MiB tombstone bound still returns 413 payload_too_large", func(t *testing.T) {
				payload := exactSizeJSON(t, prefix, suffix, rawTombstonePreAuthMaxBodyBytes+1)
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(payload))
				assertPayloadTooLarge(t, rec)
			})

			t.Run("largest legal message (MaxMessageLength chars) passes decode", func(t *testing.T) {
				raw, err := json.Marshal(MessageRequest{
					Message: strings.Repeat("<", messages.MaxMessageLength),
				})
				require.NoError(t, err)
				require.LessOrEqual(t, len(raw), maxSettingsBodySize, "worst-case legal message must fit within maxSettingsBodySize")
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(raw))
				assertPassedBodyDecode(t, rec)
				assert.NotContains(t, rec.Body.String(), "character limit", "legal MaxMessageLength message must not fail length validation")
			})
		})
	}
}
