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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reimport sourceUrl override gets the same validation as finalize
// (ptone/scion#3962): an override that finalize would not record is refused
// with 400 before it is normalized or fetched.

// invalidRecordedSourceURLs is the set finalize rejects
// (TestInstallSourceURL_NonRemoteSourceURLRejected); the reimport override
// must reject the same values.
var invalidRecordedSourceURLs = []string{
	"/home/me/harness-configs/claude",
	"file:///home/me/claude",
	"claude",
	"javascript:void(0)",
	"data:text/plain,hello",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/claude\n\x1b[31mred",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/claude\rextra",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\x00claude",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\tclaude",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\u202eclaude",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2028claude",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2029claude",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\u200bclaude",
	"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2066claude\u2069",
	"https://example.com/configs/" + strings.Repeat("a", maxRecordedSourceURLBytes),
}

// claudeTarGzServer serves the bundled claude files, with "name: claude"
// added to config.yaml so the import keeps the row's name, as a .tar.gz.
func claudeTarGzServer(t *testing.T) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range embeddedClaudeFiles(t) {
		data := f.data
		if f.path == "config.yaml" {
			data = append([]byte("name: claude\n"), data...)
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.path, Mode: 0o644, Size: int64(len(data))}))
		_, err := tw.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archive := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reimportErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	return resp.Error.Code
}

func TestReimportSourceURL_InvalidOverrideRejected(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)

	for _, bad := range invalidRecordedSourceURLs {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
			map[string]interface{}{"sourceUrl": bad})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", bad, rec.Body.String())
		assert.Equal(t, "validation_error", reimportErrorCode(t, rec.Body.Bytes()),
			"%q must be refused by validation, before any fetch", bad)
	}
	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ID, after.ID)
	assert.Equal(t, installPinnedClaudeURL, after.SourceURL, "a refused reimport must not change the source URL")
	assert.Equal(t, hc.ContentHash, after.ContentHash, "a refused reimport must not change the files")
}

func TestReimportSourceURL_ValidOverrideReimports(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
	src := claudeTarGzServer(t)
	override := src.URL + "/configs/claude.tar.gz"

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
		map[string]interface{}{"sourceUrl": override})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ImportHarnessConfigsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"claude"}, resp.HarnessConfigs)

	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ID, after.ID, "reimport must update the existing config")
	assert.Equal(t, override, after.SourceURL, "reimport records the override as the source URL")
	assert.NotEqual(t, hc.ContentHash, after.ContentHash, "reimport must replace the files")
}

func TestReimportSourceURL_OmittedOverrideUsesStoredURL(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	src := claudeTarGzServer(t)
	stored := src.URL + "/configs/claude.tar.gz"
	hc := installClaudeViaHub(t, srv, s, stored)
	require.Equal(t, stored, hc.SourceURL)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport", map[string]interface{}{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ID, after.ID)
	assert.Equal(t, stored, after.SourceURL)
	assert.NotEqual(t, hc.ContentHash, after.ContentHash, "reimport from the stored URL must replace the files")

	// A whitespace-only override is treated as omitted, matching finalize:
	// reimport uses the stored URL again.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
		map[string]interface{}{"sourceUrl": "  \t "})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	again := globalClaude(t, s)
	require.NotNil(t, again)
	assert.Equal(t, hc.ID, again.ID)
	assert.Equal(t, stored, again.SourceURL, "a whitespace-only override must leave the stored source URL in use")
}

func TestReimportSourceURL_OmittedOverrideWithoutStoredURL(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, "")
	require.Empty(t, hc.SourceURL)

	for _, body := range []map[string]interface{}{{}, {"sourceUrl": "   "}} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, "no_source_url", reimportErrorCode(t, rec.Body.Bytes()))
	}
}

// TestValidReimportSourceURL covers the reimport override check directly,
// including the documented scheme-less github.com/ shorthand for
// 'scion harness-config update --url', which passes validation in its
// https:// form (exercising it through the handler would fetch from GitHub).
func TestValidReimportSourceURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/GoogleCloudPlatform/scion/harnesses/claude",
		"http://127.0.0.1:8080/configs/claude.tar.gz",
		":gcs:my-bucket/harness-configs/claude",
		"github.com/myorg/scion-harnesses/tree/main/hermes",
		"GitHub.com/myorg/scion-harnesses/tree/main/hermes",
	} {
		assert.True(t, validReimportSourceURL(ok), "%q must be accepted", ok)
	}
	rejected := append([]string{
		"github.com/myorg/scion-harnesses/\u2028hermes",
		"github.com/myorg/scion-harnesses/\nhermes",
		"githubxcom/myorg/repo",
		"example.com/github.com/myorg/repo",
		"github.com.example.com/myorg/repo",
		"github.com@example.com/myorg/repo",
	}, invalidRecordedSourceURLs...)
	for _, bad := range rejected {
		assert.False(t, validReimportSourceURL(bad), "%q must be refused", bad)
	}
	// The shorthand allowance is reimport-only; finalize does not accept it.
	assert.False(t, validRecordedSourceURL("github.com/myorg/scion-harnesses/tree/main/hermes"))
}
