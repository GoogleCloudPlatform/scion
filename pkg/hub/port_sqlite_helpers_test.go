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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doAgentTokenRequest(t *testing.T, srv *Server, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// requirePortProxyNoCredentials asserts the plain-text 401 a credential-less
// page sub-request to a proxy route gets (ptone/scion#5566).
func requirePortProxyNoCredentials(t *testing.T, code int, h http.Header, body string, wantHint bool) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, code, body)
	assert.Equal(t, "text/plain; charset=utf-8", h.Get("Content-Type"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-store", h.Get("Cache-Control"))
	assert.Equal(t, "*", h.Get("Access-Control-Allow-Origin"))
	assert.Empty(t, h.Values("Access-Control-Allow-Credentials"))
	assert.Equal(t, "no-credentials", h.Get("X-Scion-Proxy-Error"))
	if wantHint {
		assert.Equal(t, "sandboxed-subresource", h.Get("X-Scion-Proxy-Hint"))
	} else {
		assert.Empty(t, h.Values("X-Scion-Proxy-Hint"))
	}
	assert.Empty(t, h.Values("Set-Cookie"))
	assert.Equal(t, portProxyNoCredentialsBody, body)
	assert.Contains(t, body, "#multi-file-apps")
}
