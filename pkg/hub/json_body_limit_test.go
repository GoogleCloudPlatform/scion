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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/apierr"
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
