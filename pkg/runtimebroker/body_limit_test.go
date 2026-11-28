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

package runtimebroker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type brokerCountingReader struct {
	r         io.Reader
	bytesRead atomic.Int64
}

func (c *brokerCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytesRead.Add(int64(n))
	return n, err
}

func (c *brokerCountingReader) Close() error { return nil }

func brokerExactSizeJSON(t *testing.T, prefix, suffix string, targetBytes int) []byte {
	t.Helper()
	padLen := targetBytes - len(prefix) - len(suffix)
	require.GreaterOrEqual(t, padLen, 0, "prefix+suffix exceeds targetBytes")
	out := make([]byte, targetBytes)
	copy(out, prefix)
	for i := len(prefix); i < targetBytes-len(suffix); i++ {
		out[i] = 'a'
	}
	copy(out[targetBytes-len(suffix):], suffix)
	return out
}

func assertBrokerPayloadTooLarge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	assert.Equal(t, "payload_too_large", env.Error.Code)
}

func assertBrokerPassedDecode(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code, "expected body at limit to pass decode: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "Invalid request body", "expected body at limit to decode cleanly: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "invalid request body", "expected body at limit to decode cleanly: %s", rec.Body.String())
}

func TestRuntimeBroker_ReadJSONDefaultBound(t *testing.T) {
	type payload struct {
		Data string `json:"data"`
	}
	const prefix = `{"data":"`
	const suffix = `"}`

	t.Run("exact default limit succeeds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(brokerExactSizeJSON(t, prefix, suffix, int(defaultMaxJSONBodyBytes))))
		var dst payload
		require.NoError(t, readJSON(req, &dst))
	})

	t.Run("one byte over default limit returns MaxBytesError", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(brokerExactSizeJSON(t, prefix, suffix, int(defaultMaxJSONBodyBytes)+1)))
		var dst payload
		err := readJSON(req, &dst)
		require.Error(t, err)
		assert.True(t, isMaxBytesError(err), "expected *http.MaxBytesError, got %T: %v", err, err)
	})
}

func TestRuntimeBroker_UnauthenticatedRejectedBeforeBodyRead(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})
	auth := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:   true,
		SecretKey: []byte("test-secret-key-32-bytes-long-ok"),
	})
	handler := auth.Middleware(srv.mux)

	body := &brokerCountingReader{r: bytes.NewReader(brokerExactSizeJSON(t, `{"image":"`, `"}`, int(defaultMaxJSONBodyBytes)+1024))}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/pull", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Zero(t, body.bytesRead.Load(), "unauthenticated caller must not trigger body reads")
}

func TestRuntimeBroker_HandlerBodyLimits(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		prefix string
		suffix string
		limit  int
	}{
		{
			name:   "createAgent",
			method: http.MethodPost,
			path:   "/api/v1/agents",
			prefix: `{"name":"","pad":"`,
			suffix: `"}`,
		},
		{
			name:   "startAgent",
			method: http.MethodPost,
			path:   "/api/v1/agents/test-agent/start",
			prefix: `{"task":"`,
			suffix: `"}`,
		},
		{
			name:   "restartAgent",
			method: http.MethodPost,
			path:   "/api/v1/agents/test-agent/restart",
			prefix: `{"pad":"`,
			suffix: `"}`,
		},
		{
			name:   "sendMessage",
			method: http.MethodPost,
			path:   "/api/v1/agents/test-agent/message",
			prefix: `{"message":"`,
			suffix: `"}`,
			limit:  int(maxBrokerSendMessageBodyBytes),
		},
		{
			name:   "execCommand",
			method: http.MethodPost,
			path:   "/api/v1/agents/test-agent/exec",
			prefix: `{"command":[],"pad":"`,
			suffix: `"}`,
		},
		{
			name:   "resetAuth",
			method: http.MethodPost,
			path:   "/api/v1/agents/test-agent/reset-auth",
			prefix: `{"token":"","pad":"`,
			suffix: `"}`,
		},
		{
			name:   "handleImagePull",
			method: http.MethodPost,
			path:   "/api/v1/images/pull",
			prefix: `{"image":"","pad":"`,
			suffix: `"}`,
		},
		{
			name:   "handleImageDeleteLocal",
			method: http.MethodDelete,
			path:   "/api/v1/images/local",
			prefix: `{"image":"","pad":"`,
			suffix: `"}`,
		},
		{
			name:   "handleWorkspaceUpload",
			method: http.MethodPost,
			path:   "/api/v1/workspace/upload",
			prefix: `{"slug":"","pad":"`,
			suffix: `"}`,
		},
		{
			name:   "handleWorkspaceApply",
			method: http.MethodPost,
			path:   "/api/v1/workspace/apply",
			prefix: `{"slug":"","pad":"`,
			suffix: `"}`,
		},
	}

	for _, tc := range cases {
		limit := tc.limit
		if limit == 0 {
			limit = int(defaultMaxJSONBodyBytes)
		}
		t.Run(tc.name, func(t *testing.T) {
			cfg := testServerConfig()
			cfg.BrokerID = "test-broker"
			cfg.BrokerName = "test-host"
			cfg.HubEnabled = false
			srv := New(cfg, &mockManager{}, &runtime.MockRuntime{}, config.RuntimeBuildRecord{})

			t.Run("exact limit passes decode", func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(brokerExactSizeJSON(t, tc.prefix, tc.suffix, limit)))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.mux.ServeHTTP(rec, req)
				assertBrokerPassedDecode(t, rec)
			})

			t.Run("one byte over limit returns 413 payload_too_large", func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(brokerExactSizeJSON(t, tc.prefix, tc.suffix, limit+1)))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.mux.ServeHTTP(rec, req)
				assertBrokerPayloadTooLarge(t, rec)
			})
		})
	}
}
