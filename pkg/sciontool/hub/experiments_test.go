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

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetExperiments: the client reads GET /api/v1/experiments and returns
// the enabled names sorted; an error status and a malformed body are
// errors, and an unconfigured client sends no request (ptone/scion#4573).
func TestGetExperiments(t *testing.T) {
	scrubHubEnv(t)
	status, body := http.StatusOK, `{"experiments":{"hub.conduit":false,"hub.artifacts":true,"agent.edit":true}}`
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization") + r.Header.Get("X-Scion-Agent-Token")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithConfig(srv.URL, "agent-token", "agent-1")

	got, err := c.GetExperiments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.edit", "hub.artifacts"}, got)
	assert.Equal(t, "/api/v1/experiments", gotPath)
	assert.Contains(t, gotAuth, "agent-token")

	body = `{"experiments":{"hub.artifacts":false}}`
	got, err = c.GetExperiments(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	status, body = http.StatusUnauthorized, `{"error":{"code":"unauthorized"}}`
	_, err = c.GetExperiments(context.Background())
	assert.ErrorContains(t, err, "401")

	status, body = http.StatusOK, `not json`
	_, err = c.GetExperiments(context.Background())
	assert.Error(t, err)

	_, err = NewClientWithConfig("", "", "").GetExperiments(context.Background())
	assert.ErrorContains(t, err, "not configured")
}
