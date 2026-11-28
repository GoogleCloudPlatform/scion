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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingSlugLookupStore struct {
	store.Store
	fault       *storeFaultSwitch
	slugLookups atomic.Int64
}

func (c *countingSlugLookupStore) GetProjectBySlug(ctx context.Context, slug string) (*store.Project, error) {
	if c.fault.Active() {
		c.slugLookups.Add(1)
	}
	return c.Store.GetProjectBySlug(ctx, slug)
}

type failingTrackingReader struct {
	bytesRead atomic.Int64
	readCalls atomic.Int64
}

func (f *failingTrackingReader) Read(_ []byte) (int, error) {
	f.readCalls.Add(1)
	return 0, errors.New("body must not be read for unauthorized caller")
}

func (f *failingTrackingReader) Close() error { return nil }

func doCreateLimitRawAgentRequest(t *testing.T, f *bypassAgentsFixture, method, path string, body io.Reader, scopes ...AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []AgentTokenScope{ScopeAgentStatusUpdate}
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", f.token(t, scopes...))
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func repeatJSONEmptyObjects(count int) string {
	if count <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(count * 3)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("{}")
	}
	return b.String()
}

func repeatJSONEmptyStrings(count int) string {
	if count <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(count * 3)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`""`)
	}
	return b.String()
}

func TestCreateAgent_AgentUnauthorizedRejectedBeforeBodyRead(t *testing.T) {
	t.Run("agent without ScopeAgentCreate reads 0 bytes even on failing reader", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		body := &failingTrackingReader{}
		rec := doCreateLimitRawAgentRequest(t, f, http.MethodPost, "/api/v1/agents", body, ScopeAgentStatusUpdate)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, "Missing required scope: "+string(ScopeAgentCreate), decodeTargetAPIError(t, rec).Message)
		assert.Equal(t, int64(0), body.readCalls.Load(), "unauthorized agent must not trigger any body read")
	})

	t.Run("agent without ScopeAgentCreate reads 0 bytes on oversized malformed body", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		body := newBodyLimitCountingReader(bytes.Repeat([]byte("{"), maxAgentCreateBodySize+1024))
		rec := doCreateLimitRawAgentRequest(t, f, http.MethodPost, "/api/v1/agents", body, ScopeAgentStatusUpdate)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, int64(0), body.bytesRead.Load(), "unauthorized agent must not consume body bytes")
	})

	t.Run("agent with ScopeAgentCreate but unauthorized delegator reads 0 bytes", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		markEdgeBackfillComplete(t, f.store)
		// f.owner has project access only, not agent.create, so f.caller is denied on its own project.
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)

		body := &failingTrackingReader{}
		rec := doCreateLimitRawAgentRequest(t, f, http.MethodPost, "/api/v1/agents", body, ScopeAgentCreate)
		assertAgentCreateDenied(t, rec, true)
		assert.Equal(t, int64(0), body.readCalls.Load(), "agent denied on its own project must not trigger any body read")
	})

	t.Run("authorized agent targeting another project is rejected before reading tail", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		markEdgeBackfillComplete(t, f.store)
		grantFixtureRole(t, f, f.owner.ID, store.ProjectRoleMember)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)

		payload := fmt.Sprintf(`{"projectId":%q,"name":"cross-proj","config":{"volumes":[%s]}}`,
			f.other.ID, repeatJSONEmptyObjects(50_000))
		body := newBodyLimitCountingReader([]byte(payload))
		rec := doCreateLimitRawAgentRequest(t, f, http.MethodPost, "/api/v1/agents", body, ScopeAgentCreate)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, "Agents can only create sub-agents within their own project", decodeTargetAPIError(t, rec).Message)
		assert.Less(t, body.bytesRead.Load(), int64(32*1024), "cross-project agent create must stop reading right after projectId")
	})

	t.Run("broker identity reads 0 bytes", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		body := &failingTrackingReader{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", body)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), &brokerIdentityImpl{brokerID: f.broker.ID}))
		rec := httptest.NewRecorder()
		f.srv.createAgent(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, int64(0), body.readCalls.Load(), "broker identity must not trigger any body read")
	})
}

func TestCreateAgent_NonAgentUnauthorizedRejectedBeforeFullDecode(t *testing.T) {
	srv, s, owner, outsider, project, _, _ := setupAgentBodyLimitFixture(t)

	t.Run("outsider with projectId first stops before reading pathological tail", func(t *testing.T) {
		payload := fmt.Sprintf(`{"projectId":%q,"name":"unauth-tail","config":{"volumes":[%s]}}`,
			project.ID, repeatJSONEmptyObjects(50_000))
		body := newBodyLimitCountingReader([]byte(payload))
		rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Less(t, body.bytesRead.Load(), int64(32*1024), "unauthorized user must be rejected before reading/decoding tail")
	})

	t.Run("outsider with project slug stops before reading tail", func(t *testing.T) {
		payload := fmt.Sprintf(`{"projectId":%q,"name":"unauth-slug","config":{"volumes":[%s]}}`,
			project.Slug, repeatJSONEmptyObjects(50_000))
		body := newBodyLimitCountingReader([]byte(payload))
		rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Less(t, body.bytesRead.Load(), int64(32*1024), "unauthorized user using project slug must be rejected early")
	})

	t.Run("outsider with pathological array before projectId is rejected before amplification", func(t *testing.T) {
		payload := fmt.Sprintf(`{"config":{"volumes":[%s]},"projectId":%q,"name":"unauth-prefix"}`,
			repeatJSONEmptyObjects(50_000), project.ID)
		body := newBodyLimitCountingReader([]byte(payload))
		rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, "/api/v1/agents", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "maximum element count")
		assert.Less(t, body.bytesRead.Load(), int64(64*1024), "pathological prefix must fail fast without reading full body")
	})

	t.Run("duplicate projectId is rejected and cannot bypass authorization", func(t *testing.T) {
		ctx := context.Background()
		outsiderProj := &store.Project{
			ID:      api.NewUUID(),
			Name:    "Outsider Owned Project",
			Slug:    "outsider-owned-proj",
			OwnerID: outsider.ID,
		}
		require.NoError(t, s.CreateProject(ctx, outsiderProj))
		grantProjectRole(t, s, outsider.ID, outsiderProj.ID, store.ProjectRoleMember)

		payload := fmt.Sprintf(`{"projectId":%q,"projectId":%q,"name":"dup-project-bypass"}`,
			outsiderProj.ID, project.ID)
		rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, "/api/v1/agents", strings.NewReader(payload))
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "duplicate projectId")

		_, err := s.GetAgentBySlug(ctx, project.ID, "dup-project-bypass")
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetAgentBySlug(ctx, outsiderProj.ID, "dup-project-bypass")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("repeated authorized projectId alternating UUID and slug with case variants is rejected with at most one authz and slug lookup", func(t *testing.T) {
		ctx := context.Background()
		countSrv, countStore, slugCounter, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *countingSlugLookupStore {
			return &countingSlugLookupStore{Store: inner, fault: f}
		})
		testOwner := &store.User{
			ID:          api.NewUUID(),
			Email:       "dup-owner@example.com",
			DisplayName: "Dup Owner",
			Role:        store.UserRoleMember,
			Status:      "active",
		}
		require.NoError(t, countStore.CreateUser(ctx, testOwner))
		testProj := &store.Project{
			ID:      api.NewUUID(),
			Name:    "Dup Authz Project",
			Slug:    "dup-authz-proj",
			OwnerID: testOwner.ID,
		}
		require.NoError(t, countStore.CreateProject(ctx, testProj))
		grantProjectRole(t, countStore, testOwner.ID, testProj.ID, store.ProjectRoleMember)

		emitter := &recordingDecisionAuditEmitter{}
		countSrv.authzService.SetDecisionAuditEmitter(emitter)
		t.Cleanup(func() { countSrv.authzService.SetDecisionAuditEmitter(nil) })

		keyVariants := []string{"projectId", "PROJECTID", "projectid", "ProjectId"}
		buildAlternating := func(firstVal, secondVal string, pairs int) []byte {
			var b strings.Builder
			b.WriteByte('{')
			for i := 0; i < pairs; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				k1 := keyVariants[(2*i)%len(keyVariants)]
				k2 := keyVariants[(2*i+1)%len(keyVariants)]
				fmt.Fprintf(&b, `%q:%q,%q:%q`, k1, firstVal, k2, secondVal)
			}
			b.WriteString(`,"name":"dup-amp"}`)
			return []byte(b.String())
		}

		cases := []struct {
			name            string
			payload         []byte
			wantMaxAuthz    int
			wantSlugLookups int64
		}{
			{
				name:            "UUID first alternating with slug",
				payload:         buildAlternating(testProj.ID, testProj.Slug, 1024),
				wantMaxAuthz:    1,
				wantSlugLookups: 0,
			},
			{
				name:            "slug first alternating with UUID",
				payload:         buildAlternating(testProj.Slug, testProj.ID, 1024),
				wantMaxAuthz:    1,
				wantSlugLookups: 1,
			},
			{
				name:            "null followed by projectId is rejected as duplicate",
				payload:         []byte(fmt.Sprintf(`{"projectId":null,"PROJECTID":%q,"name":"dup-null"}`, testProj.ID)),
				wantMaxAuthz:    0,
				wantSlugLookups: 0,
			},
		}

		fault.Arm()
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				emitter.records = nil
				slugCounter.slugLookups.Store(0)

				body := newBodyLimitCountingReader(tc.payload)
				rec := doRawRequestWithUser(t, countSrv, testOwner, http.MethodPost, "/api/v1/agents", body)
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "duplicate projectId")
				assert.Less(t, body.bytesRead.Load(), int64(32*1024), "duplicate projectId must fail fast without scanning tail")
				assert.LessOrEqual(t, len(emitter.records), tc.wantMaxAuthz, "duplicate projectId must not amplify authorization evaluations")
				assert.Equal(t, tc.wantSlugLookups, slugCounter.slugLookups.Load(), "duplicate projectId must not amplify slug lookups")
			})
		}
	})

	t.Run("missing or invalid projectId and malformed JSON return clean 4xx with no side effects", func(t *testing.T) {
		cases := []struct {
			name       string
			body       string
			wantStatus int
		}{
			{"missing projectId", `{"name":"no-proj"}`, http.StatusBadRequest},
			{"empty projectId", `{"name":"empty-proj","projectId":""}`, http.StatusBadRequest},
			{"null projectId", `{"name":"null-proj","projectId":null}`, http.StatusBadRequest},
			{"non-string projectId", `{"name":"num-proj","projectId":123}`, http.StatusBadRequest},
			{"nonexistent slug", `{"name":"bad-slug","projectId":"does-not-exist-slug"}`, http.StatusNotFound},
			{"malformed JSON", `{"projectId":`, http.StatusBadRequest},
			{"non-object JSON array", `[]`, http.StatusBadRequest},
			{"empty body", ``, http.StatusBadRequest},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, "/api/v1/agents", strings.NewReader(tc.body))
				assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			})
		}
	})
}

func TestCreateAgent_ElementCountLimits(t *testing.T) {
	srv, s, owner, _, project, _, _ := setupAgentBodyLimitFixture(t)

	routes := []struct {
		name string
		path string
	}{
		{"top-level createAgent", "/api/v1/agents"},
		{"project-scoped createProjectAgent", "/api/v1/projects/" + project.ID + "/agents"},
	}

	// Build a labels object with maxAgentCreateObjectKeys+1 keys.
	var labelsBuf strings.Builder
	labelsBuf.WriteByte('{')
	for i := 0; i <= maxAgentCreateObjectKeys; i++ {
		if i > 0 {
			labelsBuf.WriteByte(',')
		}
		fmt.Fprintf(&labelsBuf, `"k%d":"v"`, i)
	}
	labelsBuf.WriteByte('}')
	tooManyLabels := labelsBuf.String()

	deepNested := strings.Repeat(`{"a":`, maxAgentCreateJSONDepth+1) + `1` + strings.Repeat(`}`, maxAgentCreateJSONDepth+1)

	// Build a nested array-of-arrays that stays under per-array limits (4,096)
	// and depth limits (32) but exceeds maxAgentCreateTotalValues (1,000,000):
	// 500 inner arrays of 2,001 zeros = 1,000,500 scalar values.
	innerArray := "[" + strings.Repeat("0,", 2000) + "0]"
	var totalValsBuf strings.Builder
	totalValsBuf.WriteByte('[')
	for i := 0; i < 500; i++ {
		if i > 0 {
			totalValsBuf.WriteByte(',')
		}
		totalValsBuf.WriteString(innerArray)
	}
	totalValsBuf.WriteByte(']')
	tooManyTotalValues := totalValsBuf.String()

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			cases := []struct {
				name        string
				payload     string
				wantContain string
			}{
				{
					name:        "config.volumes exceeds array element limit",
					payload:     fmt.Sprintf(`{"name":"amp-vol","projectId":%q,"config":{"volumes":[%s]}}`, project.ID, repeatJSONEmptyObjects(maxAgentCreateArrayElements+1)),
					wantContain: "maximum element count",
				},
				{
					name:        "secrets exceeds array element limit",
					payload:     fmt.Sprintf(`{"name":"amp-sec","projectId":%q,"secrets":[%s]}`, project.ID, repeatJSONEmptyObjects(maxAgentCreateArrayElements+1)),
					wantContain: "maximum element count",
				},
				{
					name:        "config.command_args exceeds array element limit",
					payload:     fmt.Sprintf(`{"name":"amp-args","projectId":%q,"config":{"command_args":[%s]}}`, project.ID, repeatJSONEmptyStrings(maxAgentCreateArrayElements+1)),
					wantContain: "maximum element count",
				},
				{
					name:        "workspaceFiles exceeds workspace file element limit",
					payload:     fmt.Sprintf(`{"name":"amp-ws","projectId":%q,"workspaceFiles":[%s]}`, project.ID, repeatJSONEmptyObjects(maxAgentCreateWorkspaceFiles+1)),
					wantContain: "maximum element count",
				},
				{
					name:        "labels exceeds object key limit",
					payload:     fmt.Sprintf(`{"name":"amp-lbl","projectId":%q,"labels":%s}`, project.ID, tooManyLabels),
					wantContain: "maximum key count",
				},
				{
					name:        "deeply nested JSON exceeds depth limit",
					payload:     fmt.Sprintf(`{"name":"amp-depth","projectId":%q,"extra":%s}`, project.ID, deepNested),
					wantContain: "nesting depth",
				},
				{
					name:        "nested arrays exceed total JSON value limit",
					payload:     fmt.Sprintf(`{"name":"amp-total","projectId":%q,"extra":%s}`, project.ID, tooManyTotalValues),
					wantContain: "maximum element count",
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					before, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
					require.NoError(t, err)

					rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, strings.NewReader(tc.payload))
					assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
					assert.Contains(t, rec.Body.String(), tc.wantContain)

					after, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
					require.NoError(t, err)
					assert.Equal(t, len(before.Items), len(after.Items), "rejected payload must not create an agent")
				})
			}

			t.Run("multi-MiB pathological volumes array fails fast with bounded memory", func(t *testing.T) {
				// ~3 MiB of empty objects in config.volumes (~1,000,000 objects).
				payload := []byte(fmt.Sprintf(`{"name":"amp-mem","projectId":%q,"config":{"volumes":[%s]}}`,
					project.ID, repeatJSONEmptyObjects(1_000_000)))
				body := newBodyLimitCountingReader(payload)

				runtime.GC()
				var beforeMem, afterMem runtime.MemStats
				runtime.ReadMemStats(&beforeMem)

				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, body)

				runtime.ReadMemStats(&afterMem)
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "maximum element count")
				assert.Less(t, body.bytesRead.Load(), int64(128*1024), "decoder must stop reading shortly after hitting element limit")
				allocDelta := afterMem.TotalAlloc - beforeMem.TotalAlloc
				assert.Less(t, allocDelta, uint64(32<<20), "pathological payload decode must not amplify heap (allocated %d bytes)", allocDelta)
			})
		})
	}
}

func TestCreateAgent_MaximalLegalPayloadSucceeds(t *testing.T) {
	srv, _, owner, _, project, _, _ := setupAgentBodyLimitFixture(t)

	// Combine worst-case 50 x 64 KiB base64 secrets (~4.37 MB) with ~70,000
	// workspaceFiles entries (~11.9 MB) so the combined legal payload sits near
	// the 16 MiB body ceiling.
	encodedSecret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("s"), maxCreateAgentSecretValueBytes))
	secrets := make([]AgentSecretInput, maxCreateAgentSecrets)
	for i := range secrets {
		secrets[i] = AgentSecretInput{
			Key:   fmt.Sprintf("SECRET_%02d", i),
			Value: encodedSecret,
		}
	}

	hash := "sha256:" + strings.Repeat("b", 64)
	const fileCount = 80_000
	files := make([]transfer.FileInfo, fileCount)
	for i := range files {
		files[i] = transfer.FileInfo{
			Path: "src/pkg-" + strconv.Itoa(1_000_000 + i)[1:] + "/index.ts",
			Size: 4096,
			Hash: hash,
			Mode: "0644",
		}
	}

	reqBody := CreateAgentRequest{
		Name:           "maximal-legal-agent",
		ProjectID:      project.ID,
		Template:       "default",
		Secrets:        secrets,
		WorkspaceFiles: files,
		Labels: map[string]string{
			"env":  "test",
			"tier": "maximal",
		},
		Config: &api.ScionConfig{
			Env: map[string]string{
				"APP_MODE": "production",
			},
		},
	}
	raw, err := json.Marshal(reqBody)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), maxAgentCreateBodySize, "combined legal payload must fit within 16 MiB")
	require.Greater(t, len(raw), 14<<20, "combined legal payload should exercise near-limit sizing")

	routes := []struct {
		name string
		path string
	}{
		{"top-level createAgent", "/api/v1/agents"},
		{"project-scoped createProjectAgent", "/api/v1/projects/" + project.ID + "/agents"},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(raw))
			assertPassedBodyDecode(t, rec)
		})
	}

	// Verify a 16 MiB-fitting manifest of 155,000 short-path FileInfo entries
	// (with omitempty mode omitted) passes decode cleanly rather than hitting
	// the workspaceFiles structural cap.
	t.Run("155k short-path workspaceFiles fitting in 16 MiB passes decode", func(t *testing.T) {
		const shortFileCount = 155_000
		shortFiles := make([]transfer.FileInfo, shortFileCount)
		for i := range shortFiles {
			shortFiles[i] = transfer.FileInfo{
				Path: strconv.FormatInt(int64(i), 36),
				Size: 1,
				Hash: hash,
			}
		}
		shortReq := CreateAgentRequest{
			Name:           "short-path-manifest-agent",
			ProjectID:      project.ID,
			Template:       "default",
			WorkspaceFiles: shortFiles,
		}
		shortRaw, err := json.Marshal(shortReq)
		require.NoError(t, err)
		require.LessOrEqual(t, len(shortRaw), maxAgentCreateBodySize, "155k short-path manifest must fit within 16 MiB")
		require.Greater(t, len(shortRaw), 15<<20, "155k short-path manifest should sit near the 16 MiB boundary")

		for _, rt := range routes {
			t.Run(rt.name, func(t *testing.T) {
				rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, rt.path, bytes.NewReader(shortRaw))
				assertPassedBodyDecode(t, rec)
			})
		}
	})
}
