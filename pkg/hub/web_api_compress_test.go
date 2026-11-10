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
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// These tests cover the gzip wrapper MountHubAPI installs around /api/v1/
// (pkg/httpcompress): JSON is compressed, every stream, upgrade, port and
// bypassed route passes through untouched, and nothing outside /api/v1/ is
// wrapped.

// compressClient sends Accept-Encoding: gzip itself and never decodes, so a
// test sees the bytes on the wire.
func compressClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}}
}

// compressGet issues a GET with Accept-Encoding: gzip and the given headers.
func compressGet(t *testing.T, ctx context.Context, rawURL string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := compressClient().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// compressFeedLogQuerier serves tails that deliver the given entries and
// then stay open until the request ends.
type compressFeedLogQuerier struct {
	entries []CloudLogEntry
}

func (q *compressFeedLogQuerier) Query(context.Context, LogQueryOptions) (*LogQueryResult, error) {
	return &LogQueryResult{}, nil
}

func (q *compressFeedLogQuerier) Tail(context.Context, LogQueryOptions) (<-chan CloudLogEntry, func(), error) {
	ch := make(chan CloudLogEntry, len(q.entries))
	for _, e := range q.entries {
		ch <- e
	}
	return ch, func() {}, nil
}

func (q *compressFeedLogQuerier) GCPProjectID() string { return "gcp-proj" }
func (q *compressFeedLogQuerier) Close() error         { return nil }

// compressHubEnv is a real hub mounted on a web server behind a live
// listener, with an event publisher and a log tail.
type compressHubEnv struct {
	srv    *Server
	store  store.Store
	events *ChannelEventPublisher
	ts     *httptest.Server
	auth   map[string]string
}

func newCompressHubEnv(t *testing.T, logs *compressFeedLogQuerier) *compressHubEnv {
	t.Helper()
	srv, st := testServer(t)
	return newCompressHubEnvOn(t, srv, st, logs)
}

func newCompressHubEnvOn(t *testing.T, srv *Server, st store.Store, logs *compressFeedLogQuerier) *compressHubEnv {
	t.Helper()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.events = events
	if logs != nil {
		srv.logQueryService = logs
	}
	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	ws.SetUserTokenService(srv.userTokenService)
	ws.SetAuthzService(srv.authzService)
	ws.MountHubAPI(srv.Handler(), func(context.Context) error { return nil })
	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)
	return &compressHubEnv{
		srv: srv, store: st, events: events, ts: ts,
		auth: map[string]string{"Authorization": "Bearer " + testDevToken},
	}
}

// requireStreamLine reads an open stream until a line starting with want
// arrives, and requires that within 2s and before the stream ends (the
// handler is still running). poke, if set, is called at the start and every
// 50ms while waiting.
func requireStreamLine(t *testing.T, body io.Reader, want string, poke func()) {
	t.Helper()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	if poke != nil {
		poke()
	}
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before %q arrived (the handler must still be running)", want)
			}
			if strings.HasPrefix(line, want) {
				return
			}
		case <-tick.C:
			if poke != nil {
				poke()
			}
		case <-deadline:
			t.Fatalf("%q did not arrive within 2s while the stream was open", want)
		}
	}
}

func requireNoEncoding(t *testing.T, resp *http.Response) {
	t.Helper()
	assert.Empty(t, resp.Header.Get("Content-Encoding"), "Content-Encoding must not be set")
	assert.False(t, headerHasVaryAcceptEncoding(resp.Header), "Vary: Accept-Encoding must not be added, got %q", resp.Header.Values("Vary"))
}

func headerHasVaryAcceptEncoding(h http.Header) bool {
	for _, v := range h.Values("Vary") {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

func TestWebAPI_AgentListGzip(t *testing.T) {
	env := newCompressHubEnv(t, nil)
	ctx := context.Background()
	project := &store.Project{ID: api.NewUUID(), Name: "gzip-project", Slug: "gzip-project"}
	require.NoError(t, env.store.CreateProject(ctx, project))
	for i := 0; i < 20; i++ {
		require.NoError(t, env.store.CreateAgent(ctx, &store.Agent{
			ID: api.NewUUID(), Name: fmt.Sprintf("gzip-agent-%02d", i), Slug: fmt.Sprintf("gzip-agent-%02d", i), ProjectID: project.ID,
		}))
	}

	resp := compressGet(t, ctx, env.ts.URL+"/api/v1/agents", env.auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.True(t, headerHasVaryAcceptEncoding(resp.Header), "Vary = %q", resp.Header.Values("Vary"))
	zr, err := gzip.NewReader(resp.Body)
	require.NoError(t, err)
	body, err := io.ReadAll(zr)
	require.NoError(t, err)
	var list struct {
		Agents []json.RawMessage `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(body, &list), "decoded body: %.200s", body)
	assert.Len(t, list.Agents, 20)

	// A client that does not accept gzip gets the identity body.
	req, err := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/agents", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", env.auth["Authorization"])
	plain, err := compressClient().Do(req)
	require.NoError(t, err)
	defer func() { _ = plain.Body.Close() }()
	require.Equal(t, http.StatusOK, plain.StatusCode)
	assert.Empty(t, plain.Header.Get("Content-Encoding"))
	identity, err := io.ReadAll(plain.Body)
	require.NoError(t, err)
	// Compare by agent ID (list ordering and per-request fields may differ
	// between two calls): the same 20 agents, each with the same name and
	// project, which must be the ones created above.
	gz, plainList := agentListByID(t, body), agentListByID(t, identity)
	assert.Equal(t, gz, plainList)
	names := make([]string, 0, len(gz))
	for _, a := range gz {
		assert.Equal(t, project.ID, a.ProjectID)
		names = append(names, a.Name)
	}
	want := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("gzip-agent-%02d", i))
	}
	assert.ElementsMatch(t, want, names)
}

// agentListEntry holds the stable fields of one agent list item.
type agentListEntry struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	ProjectID string `json:"projectId"`
}

// agentListByID decodes an agent list response body into stable per-agent
// fields keyed by agent ID, and requires 20 distinct agents.
func agentListByID(t *testing.T, body []byte) map[string]agentListEntry {
	t.Helper()
	var list struct {
		Agents []struct {
			ID string `json:"id"`
			agentListEntry
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(body, &list), "list body: %.200s", body)
	out := make(map[string]agentListEntry, len(list.Agents))
	for _, a := range list.Agents {
		out[a.ID] = a.agentListEntry
	}
	require.Len(t, list.Agents, 20)
	require.Len(t, out, 20, "agent IDs must be distinct")
	return out
}

func TestWebAPI_AgentItemNotCompressed(t *testing.T) {
	env := newCompressHubEnv(t, nil)
	ctx := context.Background()
	agent := createTestAgent(t, env.store)
	// A known phase: metadata fields are editable in every known phase, and
	// a fixture agent with no phase refuses PATCH edits.
	agent.Phase = string(state.PhaseStopped)
	agent.Annotations = map[string]string{"note": strings.Repeat("a", 1500)}
	require.NoError(t, env.store.UpdateAgent(ctx, agent))

	for _, p := range []string{
		"/api/v1/agents/" + agent.ID,
		"/api/v1/projects/" + agent.ProjectID + "/agents/" + agent.ID,
	} {
		t.Run("GET "+p, func(t *testing.T) {
			resp := compressGet(t, ctx, env.ts.URL+p, env.auth)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			requireNoEncoding(t, resp)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(body), 1024, "the item body must be large enough to be compressed if it were wrapped")
			assert.Contains(t, string(body), agent.ID)
		})
	}

	t.Run("PATCH", func(t *testing.T) {
		patch, err := json.Marshal(map[string]any{"annotations": map[string]string{"note": strings.Repeat("b", 1500)}})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPatch, env.ts.URL+"/api/v1/agents/"+agent.ID, strings.NewReader(string(patch)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Authorization", env.auth["Authorization"])
		resp, err := compressClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%.300s", body)
		requireNoEncoding(t, resp)
		require.GreaterOrEqual(t, len(body), 1024)
		assert.Contains(t, string(body), strings.Repeat("b", 1500))
	})
}

func TestWebAPI_AgentCreateNotCompressed(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, st, project := setupCreateAgentServer(t, disp)
	env := newCompressHubEnvOn(t, srv, st, nil)
	ctx := context.Background()

	post := func(t *testing.T, path string, body CreateAgentRequest) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, env.ts.URL+path, strings.NewReader(string(payload)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Authorization", env.auth["Authorization"])
		resp, err := compressClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%.300s", got)
		requireNoEncoding(t, resp)
		require.GreaterOrEqual(t, len(got), 1024, "the create body must be large enough to be compressed if it were wrapped")
		assert.True(t, json.Valid(got), "identity JSON expected")
	}
	task := strings.Repeat("t", 1500)
	for _, tc := range []struct{ name, path string }{
		{"global", "/api/v1/agents"},
		{"project", "/api/v1/projects/" + project.ID + "/agents"},
	} {
		t.Run("POST "+tc.name, func(t *testing.T) {
			post(t, tc.path, CreateAgentRequest{
				Name:      "gz-create-" + strings.ReplaceAll(tc.name, " ", "-"),
				ProjectID: project.ID,
				Task:      task,
			})
		})
	}

	// Control: the GET list on the same paths is still compressed. Extra
	// agents keep the list above the compression threshold.
	for i := 0; i < 20; i++ {
		require.NoError(t, st.CreateAgent(ctx, &store.Agent{
			ID: api.NewUUID(), Name: fmt.Sprintf("gz-list-agent-%02d", i), Slug: fmt.Sprintf("gz-list-agent-%02d", i), ProjectID: project.ID,
		}))
	}
	for _, p := range []string{"/api/v1/agents", "/api/v1/projects/" + project.ID + "/agents"} {
		t.Run("GET "+p, func(t *testing.T) {
			resp := compressGet(t, ctx, env.ts.URL+p, env.auth)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
			zr, err := gzip.NewReader(resp.Body)
			require.NoError(t, err)
			body, err := io.ReadAll(zr)
			require.NoError(t, err)
			assert.Contains(t, string(body), "gz-create-global")
		})
	}
}

func TestIsCreateCollectionPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/v1/agents":                      true,
		"/api/v1/templates":                   true,
		"/api/v1/templates/":                  true,
		"/api/v1/users/me/templates":          true,
		"/api/v1/harness-configs":             true,
		"/api/v1/skills":                      true,
		"/api/v1/templates/t1":                false,
		"/api/v1/users/me/templates/t1":       false,
		"/api/v1/skills/s1":                   false,
		"/api/v1/users/templates":             false,
		"/api/v1/agents/":                     true,
		"/api/v1/projects/p1/agents":          true,
		"/api/v1/projects/p1/agents/":         true,
		"/api/v1/agents/a1":                   false,
		"/api/v1/projects/p1/agents/a1":       false,
		"/api/v1/projects//agents":            false,
		"/api/v1/projects":                    false,
		"/api/v1/agent":                       false,
		"/api/v1/agentsX":                     false,
		"/api/v1/projects/p1/agentsX":         false,
		"/api/v1/projects/p1/agents/a1/start": false,
		"/api/v1/brokers/b1/agents":           false,
		"/agents":                             false,
	} {
		assert.Equal(t, want, isCreateCollectionPath(p), p)
	}
}

// TestWebAPI_SignedURLResponsesNotCompressed sends a large JSON response
// through the MountHubAPI mount for every route whose real response carries
// signed object-store or capability URLs: each stays identity, while the
// same body on a neighbouring route is gzipped.
func TestWebAPI_SignedURLResponsesNotCompressed(t *testing.T) {
	body := largeJSON()
	ts := mockHubEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	do := func(t *testing.T, method, path string) (*http.Response, []byte) {
		t.Helper()
		var reqBody io.Reader
		if method != http.MethodGet {
			reqBody = strings.NewReader(`{}`)
		}
		req, err := http.NewRequest(method, ts.URL+path, reqBody)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := compressClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp, got
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/agents/a1/workspace/sync-from"},
		{http.MethodPost, "/api/v1/agents/a1/workspace/sync-to"},
		{http.MethodPost, "/api/v1/templates"},
		{http.MethodPost, "/api/v1/templates/t1/upload"},
		{http.MethodGet, "/api/v1/templates/t1/download"},
		{http.MethodPost, "/api/v1/users/me/templates"},
		{http.MethodPost, "/api/v1/users/me/templates/t1/upload"},
		{http.MethodGet, "/api/v1/users/me/templates/t1/download"},
		{http.MethodPost, "/api/v1/harness-configs"},
		{http.MethodPost, "/api/v1/harness-configs/h1/upload"},
		{http.MethodGet, "/api/v1/harness-configs/h1/download"},
		{http.MethodPost, "/api/v1/skills"},
		{http.MethodPost, "/api/v1/skills/s1/upload"},
		{http.MethodGet, "/api/v1/skills/s1/download"},
		{http.MethodPost, "/api/v1/skills/s1/versions"},
		{http.MethodPost, "/api/v1/skills/s1/versions/v1/deprecate"},
		{http.MethodPost, "/api/v1/skills/resolve"},
		{http.MethodPost, "/api/v1/agents/a1/workspace/sync-to/finalize"},
		{http.MethodPost, "/api/v1/projects/p1/agents/a1/workspace/sync-from"},
		{http.MethodDelete, "/api/v1/artifacts/a1/links/l1"},
		{http.MethodGet, "/api/v1/artifacts/view/cap/index.json"},
		{http.MethodGet, "/api/v1/artifacts/shared/tok"},
		{http.MethodPost, "/api/v1/artifacts/a1/links"},
		{http.MethodGet, "/api/v1/artifacts/a1/links"},
		{http.MethodPost, "/api/v1/artifacts/a1/versions/3/view"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp, got := do(t, tc.method, tc.path)
			requireNoEncoding(t, resp)
			assert.Equal(t, body, got, "the JSON must pass through unencoded")
		})
	}
	// Controls: neighbouring routes are compressed, so the mount is wrapping.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/templates"},
		{http.MethodGet, "/api/v1/templates/t1"},
		{http.MethodGet, "/api/v1/harness-configs"},
		{http.MethodGet, "/api/v1/skills"},
		{http.MethodPost, "/api/v1/artifacts/a1/versions"},
	} {
		t.Run("control "+tc.method+" "+tc.path, func(t *testing.T) {
			resp, got := do(t, tc.method, tc.path)
			assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
			assert.NotEqual(t, body, got)
		})
	}
}

// TestWebAPI_TemplateSignedURLsNotCompressed drives the real template
// handlers with local storage through the web mount: create with files,
// upload and download return signed URLs and stay identity even above the
// compression threshold, while the template list is compressed.
func TestWebAPI_TemplateSignedURLsNotCompressed(t *testing.T) {
	srv, st := newCommitTestServer(t, newCommitTestStorage(t))
	env := newCompressHubEnvOn(t, srv, st, nil)
	ctx := context.Background()

	files := make(map[string]string, 20)
	var reqs []FileUploadRequest
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("docs/file-%02d.md", i)
		files[p] = "content " + p
		reqs = append(reqs, FileUploadRequest{Path: p, Size: int64(len(files[p]))})
	}
	send := func(t *testing.T, method, path string, payload any) (*http.Response, []byte) {
		t.Helper()
		var reqBody io.Reader
		if payload != nil {
			b, err := json.Marshal(payload)
			require.NoError(t, err)
			reqBody = strings.NewReader(string(b))
		}
		req, err := http.NewRequestWithContext(ctx, method, env.ts.URL+path, reqBody)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Authorization", env.auth["Authorization"])
		resp, err := compressClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, got
	}
	requireSignedIdentity := func(t *testing.T, resp *http.Response, got []byte, status int) {
		t.Helper()
		require.Equal(t, status, resp.StatusCode, "%.300s", got)
		requireNoEncoding(t, resp)
		require.GreaterOrEqual(t, len(got), 1024, "the body must be large enough to be compressed if it were wrapped")
		assert.Contains(t, string(got), "file-19.md")
	}

	t.Run("POST /api/v1/templates", func(t *testing.T) {
		resp, got := send(t, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
			Name: "gz-signed-create", Scope: store.TemplateScopeGlobal, Files: reqs,
		})
		requireSignedIdentity(t, resp, got, http.StatusCreated)
		var cr CreateTemplateResponse
		require.NoError(t, json.Unmarshal(got, &cr))
		assert.Len(t, cr.UploadURLs, 20)
	})

	tmpl := seedCommittedTemplate(t, srv, "gz-signed-seeded", store.TemplateScopeGlobal, "", files)
	t.Run("POST upload", func(t *testing.T) {
		resp, got := send(t, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/upload", UploadRequest{Files: reqs})
		requireSignedIdentity(t, resp, got, http.StatusOK)
		var ur UploadResponse
		require.NoError(t, json.Unmarshal(got, &ur))
		assert.Len(t, ur.UploadURLs, 20)
	})
	t.Run("GET download", func(t *testing.T) {
		resp, got := send(t, http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/download", nil)
		requireSignedIdentity(t, resp, got, http.StatusOK)
		var dr DownloadResponse
		require.NoError(t, json.Unmarshal(got, &dr))
		assert.Len(t, dr.Files, 20)
	})

	t.Run("control GET /api/v1/templates", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			seedCommittedTemplate(t, srv, fmt.Sprintf("gz-list-%02d", i), store.TemplateScopeGlobal, "", map[string]string{"a.md": "a"})
		}
		resp, _ := send(t, http.MethodGet, "/api/v1/templates", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	})
}

func TestWebAPI_SmallPublicJSONNotCompressed(t *testing.T) {
	env := newCompressHubEnv(t, nil)
	resp := compressGet(t, context.Background(), env.ts.URL+"/api/v1/settings/public", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Less(t, len(body), 1024, "fixture assumption: the public settings document is under 1 KiB")
	requireNoEncoding(t, resp)
	assert.True(t, json.Valid(body), "identity JSON expected, got %.200q", body)
}

func TestWebAPI_LogStreamNotCompressed(t *testing.T) {
	logs := &compressFeedLogQuerier{entries: []CloudLogEntry{{
		Timestamp: time.Now(), Severity: "INFO", Message: strings.Repeat("log line ", 300), InsertID: "e1",
	}}}
	env := newCompressHubEnv(t, logs)
	agent := createTestAgent(t, env.store)
	paths := []string{
		"/api/v1/agents/" + agent.ID + "/message-logs/stream",
		"/api/v1/agents/" + agent.ID + "/cloud-logs/stream",
		"/api/v1/projects/" + agent.ProjectID + "/agents/" + agent.ID + "/cloud-logs/stream",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// No Accept: text/event-stream, so the path bypass and the
			// response content type are what keep the stream unwrapped.
			resp := compressGet(t, ctx, env.ts.URL+p, env.auth)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
			requireNoEncoding(t, resp)
			requireStreamLine(t, resp.Body, "event: log", nil)
		})
	}
}

func TestWebAPI_MessageStreamNotCompressed(t *testing.T) {
	env := newCompressHubEnv(t, nil)
	// Without operational settings the stream applies no conversation
	// filter, so a published message reaches the dev user directly.
	env.srv.operationalSettings.Store(nil)
	agent := createTestAgent(t, env.store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := compressGet(t, ctx, env.ts.URL+"/api/v1/agents/"+agent.ID+"/messages/stream", env.auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	requireNoEncoding(t, resp)
	// The subscription is installed just after the headers flush, so the
	// message is re-published until the first one arrives.
	requireStreamLine(t, resp.Body, "event: message", func() {
		env.events.PublishRaw("agent."+agent.ID+".message", UserMessageEvent{
			ID: "m1", AgentID: agent.ID, ProjectID: agent.ProjectID, Msg: strings.Repeat("hello ", 300), Type: "instruction",
			SenderID: DevUserID, RecipientID: agent.ID,
		})
	})
}

// mockHubEnv mounts handler as the hub on a dev-auth web server.
func mockHubEnv(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ws := newDevAuthWebServer(t)
	ws.MountHubAPI(handler, func(context.Context) error { return nil })
	ts := httptest.NewServer(devSignedIn(t, ws))
	t.Cleanup(ts.Close)
	return ts
}

// largeJSON is a JSON object comfortably above the compression threshold.
func largeJSON() []byte {
	b, _ := json.Marshal(map[string]string{"data": strings.Repeat("x", 8192)})
	return b
}

func TestWebAPI_PTYRouteNotWrapped(t *testing.T) {
	type seen struct {
		path      string
		hijackErr error
	}
	got := make(chan seen, 4)
	ts := mockHubEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			got <- seen{path: r.URL.Path, hijackErr: err}
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			return
		}
		got <- seen{path: r.URL.Path}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(largeJSON())
	}))

	for _, p := range []string{"/api/v1/agents/a1/pty", "/api/v1/agents/a1/ports/tunnel"} {
		t.Run(p, func(t *testing.T) {
			// An upgrade request reaches the hub with a hijackable writer.
			conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			_, err = io.WriteString(conn, "GET "+p+" HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
			require.NoError(t, err)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			status, err := bufio.NewReader(conn).ReadString('\n')
			require.NoError(t, err)
			s := <-got
			require.NoError(t, s.hijackErr)
			assert.Equal(t, p, s.path)
			assert.True(t, strings.HasPrefix(status, "HTTP/1.1 101"), "status line %q", status)

			// Without an Upgrade header the path bypass alone keeps the
			// route unwrapped: large JSON stays identity.
			resp := compressGet(t, context.Background(), ts.URL+p, nil)
			<-got
			require.Equal(t, http.StatusOK, resp.StatusCode)
			requireNoEncoding(t, resp)
		})
	}
}

func TestWebAPI_PortProxyNotCompressed(t *testing.T) {
	upstreamBody := largeJSON()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamBody)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)

	ts := mockHubEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProxyRoute(r.URL.Path) || strings.Contains(r.URL.Path, "/ports/") {
			proxy.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))

	for _, p := range []string{
		"/api/v1/agents/a1/ports/8080/proxy/",
		"/api/v1/agents/a1/ports/8080/proxy/api/items",
		"/api/v1/agents/a1/ports/",
	} {
		t.Run(p, func(t *testing.T) {
			resp := compressGet(t, context.Background(), ts.URL+p, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			requireNoEncoding(t, resp)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, upstreamBody, body, "the upstream JSON must pass through unencoded")
		})
	}
}

func TestWebSSE_EventsNotCompressed(t *testing.T) {
	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.SSEHeartbeatInterval = 50 * time.Millisecond
		cfg.SSEMaxConnectionAge = time.Minute
	})
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	ws.SetAuthzService(NewAuthzService(mockSuperAdminStore(DevUserID), nil))
	t.Cleanup(pub.Close)
	// The hub API (and with it the gzip wrapper) is mounted, but /events is
	// registered on the web mux itself, outside /api/v1/.
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(largeJSON())
	}), func(context.Context) error { return nil })
	ts := httptest.NewServer(devSignedIn(t, ws))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := compressGet(t, ctx, ts.URL+"/events?sub=project.test.>", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	requireNoEncoding(t, resp)
	requireStreamLine(t, resp.Body, "event: heartbeat", nil)

	// Control: the same server does gzip a JSON response under /api/v1/.
	api := compressGet(t, context.Background(), ts.URL+"/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, api.StatusCode)
	assert.Equal(t, "gzip", api.Header.Get("Content-Encoding"))
}

func TestWebSPA_ShellNotCompressed(t *testing.T) {
	ts := mockHubEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(largeJSON())
	}))
	for _, p := range []string{"/", "/agents", "/agents/a1", "/projects/p1/agents/a1?tab=logs"} {
		t.Run(p, func(t *testing.T) {
			resp := compressGet(t, context.Background(), ts.URL+p, map[string]string{"Accept": "text/html"})
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html"), "Content-Type %q", resp.Header.Get("Content-Type"))
			assert.NotEqual(t, "gzip", resp.Header.Get("Content-Encoding"), "the SPA shell must not be gzipped by the /api/v1/ wrapper")
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Contains(t, string(body), "__SCION_DATA__", "shell body must be readable HTML")
		})
	}
}

func TestWebAPI_ErrorJSONNotCompressed(t *testing.T) {
	ts := mockHubEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusBadRequest
		switch {
		case strings.HasSuffix(r.URL.Path, "/missing"):
			code = http.StatusNotFound
		case strings.HasSuffix(r.URL.Path, "/denied"):
			code = http.StatusForbidden
		case strings.HasSuffix(r.URL.Path, "/conflict"):
			code = http.StatusConflict
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid request",
			"input": r.URL.Query().Get("q"),
		})
	}))
	q := strings.Repeat("abcdefgh", 400)
	for _, p := range []string{"/api/v1/projects/bad", "/api/v1/projects/missing", "/api/v1/projects/denied", "/api/v1/projects/conflict"} {
		require.False(t, isCompressBypassPath(p), "%s must be a compressible path for this test to be meaningful", p)
		t.Run(p, func(t *testing.T) {
			resp := compressGet(t, context.Background(), ts.URL+p+"?q="+q, nil)
			require.GreaterOrEqual(t, resp.StatusCode, 400)
			requireNoEncoding(t, resp)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Contains(t, string(body), q)
		})
	}
}

// placeholderRE matches catalog path parameters such as {id}.
var placeholderRE = regexp.MustCompile(`\{[^}]+\}`)

// TestCompressBypass_StreamRoutesFromCatalog requires every SSE and
// websocket entry point under /api/v1/ in the authz catalog, and every
// agent sub-route row of those kinds, to be covered by isCompressBypassPath.
// A new stream route that the bypass does not cover fails here.
func TestCompressBypass_StreamRoutesFromCatalog(t *testing.T) {
	var checked []string
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if ep.Kind != authzop.EntryPointSSE && ep.Kind != authzop.EntryPointWebSocket {
				continue
			}
			if !strings.HasPrefix(ep.Pattern, "/api/v1/") {
				continue
			}
			path := placeholderRE.ReplaceAllString(ep.Pattern, "x1")
			checked = append(checked, path)
			assert.True(t, isCompressBypassPath(path),
				"%s entry point %s (operation %s) is not covered by isCompressBypassPath; add it to the bypass list",
				ep.Kind, ep.Pattern, spec.ID)
		}
	}
	for _, row := range agentSubRouteTable {
		if row.kind != authzop.EntryPointSSE && row.kind != authzop.EntryPointWebSocket {
			continue
		}
		path := agentRouteSamplePath(row, true)
		checked = append(checked, path)
		assert.True(t, isCompressBypassPath(path),
			"agent sub-route %v (%s, %s) is not covered by isCompressBypassPath", row.id, row.kind, path)
	}
	// Guard against the iteration silently finding nothing.
	for _, want := range []string{
		"/api/v1/agents/x1/pty",
		"/api/v1/agents/x1/ports/tunnel",
		"/api/v1/agents/x1/messages/stream",
		"/api/v1/agents/x1/message-logs/stream",
		"/api/v1/agents/x1/cloud-logs/stream",
		"/api/v1/admin/diagnostics/logs/stream",
	} {
		assert.Contains(t, checked, want, "expected stream or websocket entry point missing from the catalog iteration")
	}
}

func TestCompressBypass_ExcludedEndpoints(t *testing.T) {
	bypassed := []string{
		// Streams, upgrades and ports.
		"/api/v1/agents/a1/pty",
		"/api/v1/agents/a1/ports/",
		"/api/v1/agents/a1/ports/tunnel",
		"/api/v1/agents/a1/ports/3000",
		"/api/v1/agents/a1/ports/3000/proxy/",
		"/api/v1/agents/a1/ports/3000/proxy/deep/path.json",
		"/api/v1/agents/a1/messages/stream",
		"/api/v1/projects/p1/message-logs/stream",
		"/api/v1/projects/p1/agents/a1/cloud-logs/stream",
		"/api/v1/admin/diagnostics/logs/stream",
		"/api/v1/something/events",
		"/api/v1/something/logs/stream/",
		// Credential or secret values in the response.
		"/api/v1/auth/me",
		"/api/v1/auth/token",
		"/api/v1/auth/cli/device/token",
		"/api/v1/agent/gcp-token",
		"/api/v1/agent/gcp-identity-token",
		"/api/v1/agent/identity-token",
		"/api/v1/agent/secrets",
		"/api/v1/agents/a1/token/refresh",
		"/api/v1/agents/a1/refresh-token",
		"/api/v1/agents/a1/delegations/g1/exchange",
		"/api/v1/agents/a1/secrets",
		"/api/v1/agents/a1/secrets/KEY",
		"/api/v1/projects/p1/agents/a1/secrets/KEY",
		"/api/v1/brokers",
		"/api/v1/brokers/join",
		"/api/v1/brokers/b1/rotate-secret",
		"/api/v1/projects/register",
		"/api/v1/admin/invites",
		"/api/v1/admin/invites/i1",
		"/api/v1/test-identities",
		"/api/v1/test-identities/t1/token",
		"/api/v1/test-identities/t1/web-login-code",
		"/api/v1/test-identities/admin",
		"/api/v1/env",
		"/api/v1/env/KEY",
		// Signed object-store or capability URLs.
		"/api/v1/agents/a1/workspace/sync-from",
		"/api/v1/agents/a1/workspace/sync-to",
		"/api/v1/agents/a1/workspace/sync-to/finalize",
		"/api/v1/templates/t1/upload",
		"/api/v1/templates/t1/download",
		"/api/v1/users/me/templates/t1/upload",
		"/api/v1/users/me/templates/t1/download",
		"/api/v1/harness-configs/h1/upload",
		"/api/v1/harness-configs/h1/download",
		"/api/v1/skills/s1/upload",
		"/api/v1/skills/s1/download",
		"/api/v1/skills/s1/versions",
		"/api/v1/skills/s1/versions/v1",
		"/api/v1/skills/resolve",
		"/api/v1/skills/resolve/",
		"/api/v1/skills/s1/resolve",
		"/api/v1/artifacts/a1/links",
		"/api/v1/artifacts/a1/links/",
		"/api/v1/artifacts/a1/links/l1",
		"/api/v1/artifacts/a1/versions/3/view",
		"/api/v1/artifacts/a1/versions/3/view/",
		"/api/v1/artifacts/view/cap/index.html",
		"/api/v1/artifacts/shared/tok",
		// Agent item responses.
		"/api/v1/agents/a1",
		"/api/v1/agents/a1/",
		"/api/v1/projects/p1/agents/a1",
		"/api/v1/projects/p1/env/KEY",
		"/api/v1/secrets",
		"/api/v1/secrets/KEY",
	}
	for _, p := range bypassed {
		assert.True(t, isCompressBypassPath(p), "%s must be bypassed", p)
	}
	compressed := []string{
		"/api/v1/agents",
		"/api/v1/agents/",
		"/api/v1/agents/a1/messages",
		"/api/v1/agents/a1/message-logs",
		"/api/v1/agents/a1/logs",
		"/api/v1/projects",
		"/api/v1/projects/p1",
		"/api/v1/projects/p1/agents",
		"/api/v1/projects/p1/agents/",
		"/api/v1/chat/conversations/c1/messages",
		"/api/v1/settings/public",
		"/api/v1/runtime-brokers",
		"/api/v1/brokers/b1",
		"/api/v1/templates",
		"/api/v1/templates/t1",
		"/api/v1/templates/t1/files",
		"/api/v1/users/me/templates",
		"/api/v1/harness-configs",
		"/api/v1/harness-configs/h1",
		"/api/v1/skills",
		"/api/v1/skills/s1",
		"/api/v1/artifacts",
		"/api/v1/artifacts/a1",
		"/api/v1/artifacts/a1/versions",
		"/api/v1/artifacts/a1/versions/3",
		"/api/v1/users/me/terminal-workspace",
		"/events",
		"/",
	}
	for _, p := range compressed {
		assert.False(t, isCompressBypassPath(p), "%s must not be bypassed", p)
	}
}
