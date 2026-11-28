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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/apierr"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONBodyLimits_WorkspaceSync(t *testing.T) {
	srv, _, owner, _, _, _, runningAgent := setupAgentBodyLimitFixture(t)

	t.Run("sync-from", func(t *testing.T) {
		path := "/api/v1/agents/" + runningAgent.Slug + "/workspace/sync-from"
		prefix := `{"excludePatterns":["`
		suffix := `"]}`

		t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
			body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1024))
			rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, path, body)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			assert.Zero(t, body.bytesRead.Load())
		})

		t.Run("exact 1 MiB limit passes decode", func(t *testing.T) {
			payload := exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes))
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
			assertPassedBodyDecode(t, rec)
		})

		t.Run("1 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
			payload := exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1)
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
			assertPayloadTooLarge(t, rec)
		})
	})

	t.Run("sync-to", func(t *testing.T) {
		path := "/api/v1/agents/" + runningAgent.Slug + "/workspace/sync-to"
		prefix := `{"files":[{"path":"`
		suffix := `","size":1,"hash":"sha256:00"}]}`

		t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
			body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxWorkspaceSyncManifestBodySize+1024))
			rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, path, body)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			assert.Zero(t, body.bytesRead.Load())
		})

		t.Run("exact 16 MiB limit passes decode", func(t *testing.T) {
			payload := exactSizeJSON(t, prefix, suffix, maxWorkspaceSyncManifestBodySize)
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
			assertPassedBodyDecode(t, rec)
		})

		t.Run("16 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
			payload := exactSizeJSON(t, prefix, suffix, maxWorkspaceSyncManifestBodySize+1)
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})
	})

	t.Run("sync-to finalize", func(t *testing.T) {
		path := "/api/v1/agents/" + runningAgent.Slug + "/workspace/sync-to/finalize"
		prefix := `{"manifest":{"version":"1","files":[{"path":"`
		suffix := `","size":1,"hash":"sha256:00"}]}}`

		t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
			body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxWorkspaceSyncManifestBodySize+1024))
			rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, path, body)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			assert.Zero(t, body.bytesRead.Load())
		})

		t.Run("exact 16 MiB limit passes decode", func(t *testing.T) {
			payload := exactSizeJSON(t, prefix, suffix, maxWorkspaceSyncManifestBodySize)
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
			assertPassedBodyDecode(t, rec)
		})

		t.Run("16 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
			payload := exactSizeJSON(t, prefix, suffix, maxWorkspaceSyncManifestBodySize+1)
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})
	})
}

func TestJSONBodyLimits_TemplateFileWrite(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})
	path := "/api/v1/templates/" + tmpl.ID + "/files/notes.txt"
	const prefix = `{"content":"`
	const suffix = `"}`

	t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, maxTemplateFileJSONBodySize+1024))
		req := httptest.NewRequest(http.MethodPut, path, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Zero(t, body.bytesRead.Load())
	})

	t.Run("payload above 1 MiB default and up to exact 16 MiB limit succeeds", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, maxTemplateFileJSONBodySize)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("16 MiB + 1 byte returns 413 payload_too_large", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, maxTemplateFileJSONBodySize+1)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		msg := assertPayloadTooLarge(t, rec)
		assert.Contains(t, msg, "16 MiB")
	})
}

func TestJSONBodyLimits_HarnessConfigFileWrite(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)
	path := "/api/v1/harness-configs/" + hc.ID + "/files/notes.txt"
	const limit = maxHarnessConfigFileSize + 4096
	const prefix = `{"content":"`
	const suffix = `"}`

	t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, limit+1024))
		req := httptest.NewRequest(http.MethodPut, path, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Zero(t, body.bytesRead.Load())
	})

	t.Run("exact harness config JSON cap (1 MiB + 4 KiB) succeeds", func(t *testing.T) {
		// Use padding inside an unused JSON key so the file content stays within
		// maxHarnessConfigFileSize while the JSON body hits the exact cap.
		hcPrefix := `{"content":"hello","pad":"`
		payload := exactSizeJSON(t, hcPrefix, suffix, limit)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("1 byte over harness config JSON cap returns 413 payload_too_large", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, limit+1)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assertPayloadTooLarge(t, rec)
	})

	t.Run("exact harness config raw cap (1 MiB) succeeds", func(t *testing.T) {
		payload := bytes.Repeat([]byte("a"), maxHarnessConfigFileSize)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("oversized raw upload returns 413 payload_too_large and stops at cap", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			size int
		}{
			{name: "1 byte over raw cap", size: maxHarnessConfigFileSize + 1},
			{name: "8 KiB over raw cap", size: maxHarnessConfigFileSize + 8192},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := newBodyLimitCountingReader(bytes.Repeat([]byte("a"), tc.size))
				req := httptest.NewRequest(http.MethodPut, path, body)
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("Authorization", "Bearer "+testDevToken)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				assertPayloadTooLarge(t, rec)
				assert.LessOrEqual(t, body.bytesRead.Load(), int64(maxHarnessConfigFileSize+1),
					"raw upload must stop reading once MaxBytesReader limit is exceeded")
			})
		}
	})

	t.Run("oversized raw upload triggers MaxBytesReader requestTooLarge on direct net/http ResponseWriter", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.handleHarnessConfigFileWrite(w, r, hc, "notes.txt")
		}))
		defer ts.Close()

		req, err := http.NewRequest(http.MethodPut, ts.URL, bytes.NewReader(bytes.Repeat([]byte("a"), maxHarnessConfigFileSize+1)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, string(body))
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(body, &errResp), string(body))
		assert.Equal(t, "payload_too_large", errResp.Error.Code)
		// http.MaxBytesReader marks the HTTP/1.1 connection as close on overflow
		// so the server does not drain the rest of the oversized request body.
		assert.True(t, resp.Close, "expected MaxBytesReader to trigger Connection: close")
	})
}

func TestJSONBodyLimits_DefaultReadJSONCaller(t *testing.T) {
	srv, _, owner, _, _, _, _ := setupAgentBodyLimitFixture(t)
	const path = "/api/v1/groups"
	prefix := fmt.Sprintf(`{"name":"grp-limit-test","description":"`)
	const suffix = `"}`

	t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1024))
		rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, path, body)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Zero(t, body.bytesRead.Load())
	})

	t.Run("exact 1 MiB limit passes decode", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes))
		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
		assertPassedBodyDecode(t, rec)
	})

	t.Run("1 MiB + 1 byte is rejected by default bound", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1)
		body := newBodyLimitCountingReader(payload)
		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, body)
		require.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusRequestEntityTooLarge,
			"expected 400 or 413, got %d: %s", rec.Code, rec.Body.String())
		assert.True(t, strings.Contains(rec.Body.String(), "too large") || rec.Code == http.StatusRequestEntityTooLarge,
			"expected body-too-large rejection, got: %s", rec.Body.String())
	})
}

func build20kFileUploadEntries() []FileUploadRequest {
	files := make([]FileUploadRequest, importMaxTreeFiles)
	for i := range files {
		files[i] = FileUploadRequest{
			Path: fmt.Sprintf("components/module-%02d/subdir-%02d/source-file-%05d.go", i/1000, (i/100)%10, i),
			Size: 128,
		}
	}
	return files
}

func build20kTemplateFileEntries() []store.TemplateFile {
	const sampleHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	files := make([]store.TemplateFile, importMaxTreeFiles)
	files[0] = store.TemplateFile{
		Path: "SKILL.md",
		Size: 64,
		Hash: sampleHash,
		Mode: "0644",
	}
	for i := 1; i < len(files); i++ {
		files[i] = store.TemplateFile{
			Path: fmt.Sprintf("dir-%02d/sub-%02d/file-%05d.txt", i/1000, (i/100)%10, i),
			Size: 128,
			Hash: sampleHash,
			Mode: "0644",
		}
	}
	return files
}

func TestJSONBodyLimits_ManifestEndpoints(t *testing.T) {
	uploadEntries := build20kFileUploadEntries()
	manifestEntries := build20kTemplateFileEntries()

	doDevRequest := func(t *testing.T, srv *Server, method, path string, payload []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	assertManifestPassedDecode := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		assertPassedBodyDecode(t, rec)
		assert.NotContains(t, strings.ToLower(rec.Body.String()), "invalid request body", rec.Body.String())
	}

	t.Run("templates", func(t *testing.T) {
		srv, s, stor := testTemplateFileServer(t)
		tmpl := createTestTemplate(t, s, stor, map[string]string{
			"CLAUDE.md": "# Agent",
		})

		t.Run("createTemplateV2", func(t *testing.T) {
			const path = "/api/v1/templates"
			manifestBody, err := json.Marshal(CreateTemplateRequest{
				Name:    "manifest-20k-tmpl",
				Harness: "claude",
				Scope:   "global",
				Files:   uploadEntries,
			})
			require.NoError(t, err)
			require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes), "20k-entry upload manifest should exceed 1 MiB")
			require.Less(t, len(manifestBody), maxFileManifestBodySize, "20k-entry upload manifest must fit within 16 MiB")

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assertManifestPassedDecode(t, rec)

			prefix := `{"name":"manifest-exact-tmpl","harness":"claude","scope":"global","description":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize))
			assertManifestPassedDecode(t, rec)

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})

		t.Run("handleTemplateUpload", func(t *testing.T) {
			path := "/api/v1/templates/" + tmpl.ID + "/upload"
			manifestBody, err := json.Marshal(UploadRequest{Files: uploadEntries})
			require.NoError(t, err)
			require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			prefix := `{"files":[{"path":"a.txt","size":1}],"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize))
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})

		t.Run("handleTemplateFinalize", func(t *testing.T) {
			path := "/api/v1/templates/" + tmpl.ID + "/finalize"
			manifestBody, err := json.Marshal(FinalizeRequest{
				Manifest: &TemplateManifest{
					Version: "1.0",
					Files:   manifestEntries,
				},
			})
			require.NoError(t, err)
			require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assertManifestPassedDecode(t, rec)

			prefix := `{"manifest":{"version":"1.0","files":[{"path":"a.txt","size":1,"hash":"sha256:00"}]},"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize))
			assertManifestPassedDecode(t, rec)

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})
	})

	t.Run("createUserTemplate", func(t *testing.T) {
		srv, _, owner, _, _, _, _ := setupAgentBodyLimitFixture(t)
		srv.SetStorage(newContentMockStorage("test-bucket"))
		const path = "/api/v1/users/me/templates"

		manifestBody, err := json.Marshal(CreateTemplateRequest{
			Name:    "user-manifest-20k",
			Harness: "claude",
			Files:   uploadEntries,
		})
		require.NoError(t, err)
		require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(manifestBody))
		assertManifestPassedDecode(t, rec)

		prefix := `{"name":"user-manifest-exact","harness":"claude","description":"`
		suffix := `"}`
		rec = doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize)))
		assertManifestPassedDecode(t, rec)

		rec = doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1)))
		msg := assertPayloadTooLarge(t, rec)
		assert.Contains(t, msg, "16 MiB")
	})

	t.Run("harness-configs", func(t *testing.T) {
		srv, s, _ := testHarnessConfigFileServer(t)
		hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

		t.Run("createHarnessConfig", func(t *testing.T) {
			const path = "/api/v1/harness-configs"
			manifestBody, err := json.Marshal(CreateHarnessConfigRequest{
				Name:    "manifest-20k-hc",
				Harness: "claude",
				Scope:   "global",
				Files:   uploadEntries,
			})
			require.NoError(t, err)
			require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assertManifestPassedDecode(t, rec)

			prefix := `{"name":"manifest-exact-hc","harness":"claude","scope":"global","description":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize))
			assertManifestPassedDecode(t, rec)

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})

		t.Run("handleHarnessConfigUpload", func(t *testing.T) {
			path := "/api/v1/harness-configs/" + hc.ID + "/upload"
			manifestBody, err := json.Marshal(UploadRequest{Files: uploadEntries})
			require.NoError(t, err)
			require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			prefix := `{"files":[{"path":"config.yaml","size":1}],"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize))
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})

		t.Run("handleHarnessConfigFinalize", func(t *testing.T) {
			path := "/api/v1/harness-configs/" + hc.ID + "/finalize"
			manifestBody, err := json.Marshal(map[string]any{
				"manifest": map[string]any{
					"version": "1.0",
					"files":   manifestEntries,
				},
			})
			require.NoError(t, err)
			require.Greater(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assertManifestPassedDecode(t, rec)

			prefix := `{"manifest":{"version":"1.0","files":[{"path":"config.yaml","size":1,"hash":"sha256:00"}]},"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize))
			assertManifestPassedDecode(t, rec)

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, maxFileManifestBodySize+1))
			msg := assertPayloadTooLarge(t, rec)
			assert.Contains(t, msg, "16 MiB")
		})
	})

	t.Run("skills", func(t *testing.T) {
		srv, s, _ := testTemplateFileServer(t)
		skill := &store.Skill{
			ID:          api.NewUUID(),
			Name:        "manifest-limit-skill",
			Slug:        "manifest-limit-skill",
			Scope:       store.SkillScopeGlobal,
			Status:      "active",
			StoragePath: "skills/global/manifest-limit-skill",
			Created:     time.Now(),
			Updated:     time.Now(),
		}
		require.NoError(t, s.CreateSkill(context.Background(), skill))

		skillUpload50 := make([]FileUploadRequest, 50)
		skillManifest50 := make([]store.TemplateFile, 50)
		const sampleHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		skillUpload50[0] = FileUploadRequest{Path: "SKILL.md", Size: 64}
		skillManifest50[0] = store.TemplateFile{Path: "SKILL.md", Size: 64, Hash: sampleHash, Mode: "0644"}
		for i := 1; i < 50; i++ {
			p := fmt.Sprintf("scripts/file-%02d.sh", i)
			skillUpload50[i] = FileUploadRequest{Path: p, Size: 128}
			skillManifest50[i] = store.TemplateFile{Path: p, Size: 128, Hash: sampleHash, Mode: "0644"}
		}

		t.Run("publishSkillVersion", func(t *testing.T) {
			path := "/api/v1/skills/" + skill.ID + "/versions"
			manifestBody, err := json.Marshal(PublishVersionRequest{
				Version: "1.0.0",
				Files:   skillUpload50,
			})
			require.NoError(t, err)
			require.Less(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assertManifestPassedDecode(t, rec)

			prefix := `{"version":"1.0.1","files":[{"path":"SKILL.md","size":1}],"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)))
			assertManifestPassedDecode(t, rec)

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1))
			assertPayloadTooLarge(t, rec)
		})

		t.Run("handleSkillUpload", func(t *testing.T) {
			path := "/api/v1/skills/" + skill.ID + "/upload"
			manifestBody, err := json.Marshal(map[string]any{
				"version": "1.0.0",
				"files":   skillUpload50,
			})
			require.NoError(t, err)
			require.Less(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			prefix := `{"version":"1.0.0","files":[{"path":"SKILL.md","size":1}],"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)))
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1))
			assertPayloadTooLarge(t, rec)
		})

		t.Run("handleSkillFinalize", func(t *testing.T) {
			path := "/api/v1/skills/" + skill.ID + "/finalize"
			manifestBody, err := json.Marshal(FinalizeSkillVersionRequest{
				Version: "1.0.0",
				Manifest: &SkillManifest{
					Files: skillManifest50,
				},
			})
			require.NoError(t, err)
			require.Less(t, len(manifestBody), int(apierr.DefaultMaxJSONBodyBytes))

			rec := doDevRequest(t, srv, http.MethodPost, path, manifestBody)
			assertManifestPassedDecode(t, rec)

			prefix := `{"version":"1.0.0","manifest":{"files":[{"path":"SKILL.md","size":1,"hash":"sha256:00"}]},"pad":"`
			suffix := `"}`
			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)))
			assertManifestPassedDecode(t, rec)

			rec = doDevRequest(t, srv, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1))
			assertPayloadTooLarge(t, rec)
		})
	})
}

func TestJSONBodyLimits_BrokerHMACIngest(t *testing.T) {
	srv, s, owner, _, project, _, _ := setupAgentBodyLimitFixture(t)
	ctx := context.Background()

	brokerAuth := seedBodyCoverageBroker(t, s)
	brokerID := brokerAuth.BrokerID

	doSignedBrokerRequest := func(t *testing.T, method, path string, payload []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		require.NoError(t, brokerAuth.ApplyAuth(req))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	assertBrokerPassedDecode := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		assertPassedBodyDecode(t, rec)
		assert.NotContains(t, strings.ToLower(rec.Body.String()), "invalid request body", rec.Body.String())
	}

	t.Run("handleBrokerHeartbeat", func(t *testing.T) {
		path := "/api/v1/runtime-brokers/" + brokerID + "/heartbeat"

		t.Run("2000-agent heartbeat above 1 MiB passes", func(t *testing.T) {
			agents := make([]brokerAgentHeartbeat, 2000)
			for i := range agents {
				agents[i] = brokerAgentHeartbeat{
					RuntimeTarget:   "docker-default",
					Slug:            fmt.Sprintf("agent-worker-%04d-long-identifier-for-scale-test", i),
					Status:          "running",
					Phase:           "running",
					Activity:        "thinking",
					ContainerStatus: "Up 2 hours (healthy)",
					Message:         strings.Repeat("status-detail-payload-", 12),
					HarnessAuth:     "api-key",
					Profile:         "default-sandbox-profile",
					Template:        "developer-default",
				}
			}
			hbBody, err := json.Marshal(brokerHeartbeatRequest{
				Status:  "online",
				Version: "0.0.0-test",
				Projects: []brokerProjectHeartbeat{{
					ProjectID:  project.ID,
					AgentCount: len(agents),
					Agents:     agents,
				}},
			})
			require.NoError(t, err)
			require.Greater(t, len(hbBody), int(apierr.DefaultMaxJSONBodyBytes), "2000-agent heartbeat must exceed 1 MiB default")
			require.Less(t, len(hbBody), int(maxBrokerSignedJSONBodySize), "2000-agent heartbeat must fit within 2 MiB signed cap")

			rec := doSignedBrokerRequest(t, http.MethodPost, path, hbBody)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})

		prefix := `{"status":"online","version":"`
		suffix := `"}`

		t.Run("exact 2 MiB signed heartbeat passes", func(t *testing.T) {
			rec := doSignedBrokerRequest(t, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(maxBrokerSignedJSONBodySize)))
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})

		t.Run("2 MiB + 1 byte signed heartbeat returns 413", func(t *testing.T) {
			rec := doSignedBrokerRequest(t, http.MethodPost, path, exactSizeJSON(t, prefix, suffix, int(maxBrokerSignedJSONBodySize)+1))
			assertPayloadTooLarge(t, rec)
		})

		t.Run("2 MiB + 1 byte handler-direct (bypassing HMAC middleware) returns 413", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(exactSizeJSON(t, prefix, suffix, int(maxBrokerSignedJSONBodySize)+1)))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithBrokerIdentity(ctx, NewBrokerIdentity(brokerID)))
			rec := httptest.NewRecorder()
			srv.handleBrokerHeartbeat(rec, req, brokerID)
			assertPayloadTooLarge(t, rec)
		})
	})

	brokerEndpoints := []struct {
		name         string
		path         string
		prefix       string
		suffix       string
		directInvoke func(w http.ResponseWriter, r *http.Request)
	}{
		{
			name:   "handleBrokerMessageFailures",
			path:   "/api/v1/runtime-brokers/" + brokerID + "/message-failures",
			prefix: `{"failures":[],"pad":"`,
			suffix: `"}`,
			directInvoke: func(w http.ResponseWriter, r *http.Request) {
				srv.handleBrokerMessageFailures(w, r, brokerID)
			},
		},
		{
			name:   "handleBrokerInbound",
			path:   "/api/v1/broker/inbound",
			prefix: `{"topic":"","pad":"`,
			suffix: `"}`,
			directInvoke: func(w http.ResponseWriter, r *http.Request) {
				srv.handleBrokerInbound(w, r)
			},
		},
		{
			name:   "handleBrokerInboundRouted",
			path:   "/api/v1/broker/inbound/routed",
			prefix: `{"project_id":"","pad":"`,
			suffix: `"}`,
			directInvoke: func(w http.ResponseWriter, r *http.Request) {
				srv.handleBrokerInboundRouted(w, r)
			},
		},
		{
			name:   "handleBrokerAttachmentsCheck",
			path:   "/api/v1/broker/inbound/attachments/check",
			prefix: `{"project_id":"","pad":"`,
			suffix: `"}`,
			directInvoke: func(w http.ResponseWriter, r *http.Request) {
				srv.handleBrokerAttachmentsCheck(w, r)
			},
		},
		{
			name:   "handleBrokerCallback",
			path:   "/api/v1/broker/callback",
			prefix: `{"data":{"k":"`,
			suffix: `"}}`,
			directInvoke: func(w http.ResponseWriter, r *http.Request) {
				srv.handleBrokerCallback(w, r)
			},
		},
		{
			name:   "handleBrokerRotateSecret",
			path:   "/api/v1/brokers/" + brokerID + "/rotate-secret",
			prefix: `{"gracePeriod":60000000000,"pad":"`,
			suffix: `"}`,
			directInvoke: func(w http.ResponseWriter, r *http.Request) {
				srv.handleBrokerRotateSecret(w, r, brokerID)
			},
		},
	}

	_ = owner
	for _, tc := range brokerEndpoints {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("exact 2 MiB signed passes decode", func(t *testing.T) {
				rec := doSignedBrokerRequest(t, http.MethodPost, tc.path, exactSizeJSON(t, tc.prefix, tc.suffix, int(maxBrokerSignedJSONBodySize)))
				assertBrokerPassedDecode(t, rec)
			})

			t.Run("2 MiB + 1 byte signed returns 413", func(t *testing.T) {
				rec := doSignedBrokerRequest(t, http.MethodPost, tc.path, exactSizeJSON(t, tc.prefix, tc.suffix, int(maxBrokerSignedJSONBodySize)+1))
				assertPayloadTooLarge(t, rec)
			})

			t.Run("2 MiB + 1 byte handler-direct returns 413", func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(exactSizeJSON(t, tc.prefix, tc.suffix, int(maxBrokerSignedJSONBodySize)+1)))
				req.RemoteAddr = "127.0.0.1:12345"
				req.Header.Set("Content-Type", "application/json")
				req = req.WithContext(contextWithBrokerIdentity(ctx, NewBrokerIdentity(brokerID)))
				rec := httptest.NewRecorder()
				tc.directInvoke(rec, req)
				assertPayloadTooLarge(t, rec)
			})
		})
	}
}

func TestJSONBodyLimits_AuthLogout(t *testing.T) {
	const logoutLimit = 4 << 10
	const path = "/api/v1/auth/logout"
	prefix := `{"refreshToken":"`
	suffix := `"}`

	srv, _ := testServer(t)

	doAuthLogout := func(s *Server, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleAuthLogout(rec, req)
		return rec
	}

	t.Run("unauthenticated oversized through full handler returns 413 and stops at 4 KiB", func(t *testing.T) {
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, logoutLimit+1024))
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assertPayloadTooLarge(t, rec)
		assert.LessOrEqual(t, body.bytesRead.Load(), int64(logoutLimit+1),
			"MaxBytesReader must bound unauthenticated requests to RoutePublic /api/v1/auth/logout")
	})

	t.Run("unauthenticated empty or normal-size body succeeds idempotently", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body io.Reader
		}{
			{name: "empty", body: nil},
			{name: "normal refreshToken", body: strings.NewReader(`{"refreshToken":"tok-123"}`)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, path, tc.body)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			})
		}
	})

	t.Run("empty body succeeds", func(t *testing.T) {
		rec := doAuthLogout(srv, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("malformed small body still succeeds", func(t *testing.T) {
		rec := doAuthLogout(srv, strings.NewReader("{invalid"))
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("exact 4 KiB limit succeeds", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, logoutLimit)
		rec := doAuthLogout(srv, bytes.NewReader(payload))
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("4 KiB + 1 byte returns 413 and stops reading", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, logoutLimit+1)
		body := newBodyLimitCountingReader(payload)
		rec := doAuthLogout(srv, body)
		assertPayloadTooLarge(t, rec)
		assert.LessOrEqual(t, body.bytesRead.Load(), int64(logoutLimit+1),
			"MaxBytesReader must stop reading once the 4 KiB cap is exceeded")
	})

	t.Run("oversized request closes connection via net/http", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.handleAuthLogout(w, r)
		}))
		defer ts.Close()

		payload := exactSizeJSON(t, prefix, suffix, logoutLimit+4096)
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		assert.True(t, resp.Close, "MaxBytesReader overflow must signal Connection: close")
	})

	t.Run("proxy mode returns 200 and reads 0 bytes", func(t *testing.T) {
		proxySrv, _ := testServer(t)
		proxySrv.config.AuthMode = "proxy"
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, logoutLimit+1024))
		rec := doAuthLogout(proxySrv, body)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Zero(t, body.bytesRead.Load())
	})
}

type panicOnSignStorage struct {
	*contentMockStorage
	signCalls int
}

func (p *panicOnSignStorage) GenerateSignedURL(ctx context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	p.signCalls++
	return p.contentMockStorage.GenerateSignedURL(ctx, objectPath, opts)
}

func TestJSONBodyLimits_UploadAndManifestFileCountCap(t *testing.T) {
	const overLimit = importMaxTreeFiles + 1

	overUploadFiles := make([]FileUploadRequest, overLimit)
	for i := range overUploadFiles {
		overUploadFiles[i] = FileUploadRequest{Path: fmt.Sprintf("f%d", i), Size: 1}
	}
	const sampleHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	overManifestFiles := make([]store.TemplateFile, overLimit)
	for i := range overManifestFiles {
		overManifestFiles[i] = store.TemplateFile{Path: fmt.Sprintf("f%d", i), Size: 1, Hash: sampleHash, Mode: "0644"}
	}

	assertTooManyFiles := func(t *testing.T, rec *httptest.ResponseRecorder, wantField string, wantMax int) {
		t.Helper()
		assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 validation_error, got %d: %s", rec.Code, rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), "body: %s", rec.Body.String())
		assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
		assert.Contains(t, strings.ToLower(errResp.Error.Message), "too many files")
		if wantField != "" {
			assert.Equal(t, wantField, errResp.Error.Details["field"])
		}
		if wantMax > 0 {
			assert.Equal(t, float64(wantMax), errResp.Error.Details["max"])
		}
	}

	doDevRequest := func(t *testing.T, srv *Server, method, path string, payload []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("generateUploadURLs rejects over limit before signing", func(t *testing.T) {
		stor := &panicOnSignStorage{contentMockStorage: newContentMockStorage("test-bucket")}
		_, _, err := generateUploadURLs(context.Background(), stor, "skills/global/s/1.0.0", overUploadFiles)
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "too many files")
		assert.Zero(t, stor.signCalls, "must not call GenerateSignedURL when file count exceeds cap")
	})

	t.Run("generateStagingUploadURLs rejects over limit before signing", func(t *testing.T) {
		stor := &panicOnSignStorage{contentMockStorage: newContentMockStorage("test-bucket")}
		_, err := generateStagingUploadURLs(context.Background(), stor, "templates/global/t", "templates", "tid", overUploadFiles, "upload-1", "")
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "too many files")
		assert.Zero(t, stor.signCalls, "must not call GenerateSignedURL when file count exceeds cap")
	})

	t.Run("templates", func(t *testing.T) {
		srv, s, stor := testTemplateFileServer(t)
		tmpl := createTestTemplate(t, s, stor, map[string]string{"CLAUDE.md": "# Agent"})

		t.Run("createTemplateV2", func(t *testing.T) {
			body, err := json.Marshal(CreateTemplateRequest{
				Name:    "over-limit-tmpl",
				Harness: "claude",
				Scope:   "global",
				Files:   overUploadFiles,
			})
			require.NoError(t, err)
			require.Less(t, len(body), maxFileManifestBodySize)
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/templates", body)
			assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
		})

		t.Run("handleTemplateUpload", func(t *testing.T) {
			body, err := json.Marshal(UploadRequest{Files: overUploadFiles})
			require.NoError(t, err)
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/upload", body)
			assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
		})

		t.Run("handleTemplateFinalize", func(t *testing.T) {
			body, err := json.Marshal(FinalizeRequest{
				Manifest: &TemplateManifest{Version: "1.0", Files: overManifestFiles},
			})
			require.NoError(t, err)
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", body)
			assertTooManyFiles(t, rec, "manifest.files", importMaxTreeFiles)
		})
	})

	t.Run("createUserTemplate", func(t *testing.T) {
		srv, _, owner, _, _, _, _ := setupAgentBodyLimitFixture(t)
		stor := &panicOnSignStorage{contentMockStorage: newContentMockStorage("test-bucket")}
		srv.SetStorage(stor)

		body, err := json.Marshal(CreateTemplateRequest{
			Name:    "over-limit-user-tmpl",
			Harness: "claude",
			Files:   overUploadFiles,
		})
		require.NoError(t, err)
		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, "/api/v1/users/me/templates", bytes.NewReader(body))
		assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
		assert.Zero(t, stor.signCalls)
	})

	t.Run("harness-configs", func(t *testing.T) {
		srv, s, _ := testHarnessConfigFileServer(t)
		hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

		t.Run("createHarnessConfig", func(t *testing.T) {
			body, err := json.Marshal(CreateHarnessConfigRequest{
				Name:    "over-limit-hc",
				Harness: "claude",
				Scope:   "global",
				Files:   overUploadFiles,
			})
			require.NoError(t, err)
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", body)
			assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
		})

		t.Run("handleHarnessConfigUpload", func(t *testing.T) {
			body, err := json.Marshal(UploadRequest{Files: overUploadFiles})
			require.NoError(t, err)
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/upload", body)
			assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
		})

		t.Run("handleHarnessConfigFinalize", func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"manifest": map[string]any{"version": "1.0", "files": overManifestFiles},
			})
			require.NoError(t, err)
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", body)
			assertTooManyFiles(t, rec, "manifest.files", importMaxTreeFiles)
		})
	})

	t.Run("skills", func(t *testing.T) {
		srv, s, _ := testTemplateFileServer(t)
		stor := &panicOnSignStorage{contentMockStorage: newContentMockStorage("test-bucket")}
		srv.SetStorage(stor)
		skill := &store.Skill{
			ID:          api.NewUUID(),
			Name:        "over-limit-skill",
			Slug:        "over-limit-skill",
			Scope:       store.SkillScopeGlobal,
			Status:      "active",
			StoragePath: "skills/global/over-limit-skill",
			Created:     time.Now(),
			Updated:     time.Now(),
		}
		require.NoError(t, s.CreateSkill(context.Background(), skill))

		// Use compact JSON objects so 20,001 entries fit well under the 1 MiB
		// skill body cap and exercise the importMaxTreeFiles (20,000) check.
		compactArray := "[" + strings.Repeat("{},", overLimit-1) + "{}]"

		t.Run("publishSkillVersion", func(t *testing.T) {
			body := []byte(`{"version":"1.0.0","files":` + compactArray + `}`)
			require.Less(t, len(body), int(apierr.DefaultMaxJSONBodyBytes))
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions", body)
			assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
			assert.Zero(t, stor.signCalls)
		})

		t.Run("handleSkillUpload", func(t *testing.T) {
			body := []byte(`{"version":"1.0.0","files":` + compactArray + `}`)
			require.Less(t, len(body), int(apierr.DefaultMaxJSONBodyBytes))
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/skills/"+skill.ID+"/upload", body)
			assertTooManyFiles(t, rec, "files", importMaxTreeFiles)
			assert.Zero(t, stor.signCalls)
		})

		t.Run("handleSkillFinalize", func(t *testing.T) {
			body := []byte(`{"version":"1.0.0","manifest":{"files":` + compactArray + `}}`)
			require.Less(t, len(body), int(apierr.DefaultMaxJSONBodyBytes))
			rec := doDevRequest(t, srv, http.MethodPost, "/api/v1/skills/"+skill.ID+"/finalize", body)
			assertTooManyFiles(t, rec, "manifest.files", importMaxTreeFiles)
		})
	})

	t.Run("workspace-sync", func(t *testing.T) {
		srv, _, owner, outsider, _, _, runningAgent := setupAgentBodyLimitFixture(t)
		stor := &panicOnSignStorage{contentMockStorage: newContentMockStorage("test-bucket")}
		srv.SetStorage(stor)

		// Regression: a ~25,000-entry realistic workspace manifest (above
		// importMaxTreeFiles=20,000, below 16 MiB and maxAgentCreateWorkspaceFiles=175,000)
		// must pass the file-count check on both sync-to and sync-to/finalize.
		const mediumWorkspaceCount = 25_000
		mediumWorkspaceFiles := make([]transfer.FileInfo, mediumWorkspaceCount)
		for i := range mediumWorkspaceFiles {
			mediumWorkspaceFiles[i] = transfer.FileInfo{
				Path: fmt.Sprintf("node_modules/pkg-%03d/file-%05d.js", i/100, i),
				Size: 128,
				Hash: sampleHash,
			}
		}

		t.Run("sync-to accepts 25k-entry realistic workspace manifest", func(t *testing.T) {
			body, err := json.Marshal(SyncToRequest{Files: mediumWorkspaceFiles})
			require.NoError(t, err)
			require.Greater(t, len(body), int(apierr.DefaultMaxJSONBodyBytes))
			require.Less(t, len(body), maxWorkspaceSyncManifestBodySize)

			stor.signCalls = 0
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+runningAgent.ID+"/workspace/sync-to", bytes.NewReader(body))
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, mediumWorkspaceCount, stor.signCalls)
		})

		t.Run("sync-to finalize accepts 25k-entry realistic workspace manifest past file-count check", func(t *testing.T) {
			body, err := json.Marshal(SyncToFinalizeRequest{
				Manifest: &transfer.Manifest{Version: "1.0", Files: mediumWorkspaceFiles},
			})
			require.NoError(t, err)
			require.Greater(t, len(body), int(apierr.DefaultMaxJSONBodyBytes))
			require.Less(t, len(body), maxWorkspaceSyncManifestBodySize)

			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+runningAgent.ID+"/workspace/sync-to/finalize", bytes.NewReader(body))
			assert.NotContains(t, strings.ToLower(rec.Body.String()), "too many files", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "File not found in storage:", "25k manifest should pass file-count check and reach storage verification")
		})

		// Over-limit: maxAgentCreateWorkspaceFiles + 1 (175,001) entries must be
		// rejected with the typed tooManyFilesError before any signing. Use compact
		// entries so the JSON body fits under the 16 MiB body cap.
		const overWorkspaceLimit = maxAgentCreateWorkspaceFiles + 1
		overWorkspaceArray := "[" + strings.Repeat(`{"path":"a"},`, overWorkspaceLimit-1) + `{"path":"a"}]`
		syncToBody := []byte(`{"files":` + overWorkspaceArray + `}`)
		require.Less(t, len(syncToBody), maxWorkspaceSyncManifestBodySize)

		t.Run("sync-to rejects over 175k limit for authorized owner", func(t *testing.T) {
			stor.signCalls = 0
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+runningAgent.ID+"/workspace/sync-to", bytes.NewReader(syncToBody))
			assertTooManyFiles(t, rec, "files", maxAgentCreateWorkspaceFiles)
			assert.Zero(t, stor.signCalls)
		})

		t.Run("sync-to authorizes outsider before file-count check", func(t *testing.T) {
			rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, "/api/v1/agents/"+runningAgent.ID+"/workspace/sync-to", bytes.NewReader(syncToBody))
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})

		finalizeBody := []byte(`{"manifest":{"version":"1.0","files":` + overWorkspaceArray + `}}`)
		require.Less(t, len(finalizeBody), maxWorkspaceSyncManifestBodySize)

		t.Run("sync-to finalize rejects over 175k limit for authorized owner", func(t *testing.T) {
			rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+runningAgent.ID+"/workspace/sync-to/finalize", bytes.NewReader(finalizeBody))
			assertTooManyFiles(t, rec, "manifest.files", maxAgentCreateWorkspaceFiles)
		})

		t.Run("sync-to finalize authorizes outsider before file-count check", func(t *testing.T) {
			rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, "/api/v1/agents/"+runningAgent.ID+"/workspace/sync-to/finalize", bytes.NewReader(finalizeBody))
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	})
}

func TestJSONBodyLimits_ProjectBroadcast(t *testing.T) {
	srv, _, owner, outsider, project, _, _ := setupAgentBodyLimitFixture(t)
	path := "/api/v1/projects/" + project.ID + "/broadcast"
	prefix := `{"structured_message":{"version":1,"type":"instruction","msg":"`
	suffix := `"}}`

	t.Run("unauthenticated oversized gets 401 and reads 0 bytes", func(t *testing.T) {
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1024))
		rec := doRawRequestWithUser(t, srv, nil, http.MethodPost, path, body)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Zero(t, body.bytesRead.Load())
	})

	t.Run("unauthorized non-member oversized gets 403 and reads 0 bytes", func(t *testing.T) {
		body := newBodyLimitCountingReader(exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1024))
		rec := doRawRequestWithUser(t, srv, outsider, http.MethodPost, path, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Zero(t, body.bytesRead.Load())
	})

	t.Run("exact 1 MiB limit passes decode", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes))
		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, bytes.NewReader(payload))
		assertPassedBodyDecode(t, rec)
		assert.NotContains(t, strings.ToLower(rec.Body.String()), "invalid request body", rec.Body.String())
	})

	t.Run("1 MiB + 1 byte returns 413 payload_too_large and stops reading", func(t *testing.T) {
		payload := exactSizeJSON(t, prefix, suffix, int(apierr.DefaultMaxJSONBodyBytes)+1)
		body := newBodyLimitCountingReader(payload)
		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, body)
		assertPayloadTooLarge(t, rec)
		assert.LessOrEqual(t, body.bytesRead.Load(), apierr.DefaultMaxJSONBodyBytes+1)
	})

	t.Run("malformed JSON still returns 400 invalid_request", func(t *testing.T) {
		rec := doRawRequestWithUser(t, srv, owner, http.MethodPost, path, strings.NewReader("{invalid"))
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	})
}
