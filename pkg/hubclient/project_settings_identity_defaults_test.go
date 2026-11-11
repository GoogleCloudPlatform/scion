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

package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The identity-defaults PUT body carries only the fields that are set: a
// nil field is left out (keep), a pointer to "" or to an empty map is sent
// (clear), and no other setting ever appears in the body.
func TestProjectGCPIdentityDefaultsUpdate_Marshalling(t *testing.T) {
	str := func(s string) *string { return &s }

	b, err := json.Marshal(ProjectGCPIdentityDefaultsUpdate{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(b))

	b, err = json.Marshal(ProjectGCPIdentityDefaultsUpdate{Mode: str(""), ServiceAccountID: str("")})
	require.NoError(t, err)
	assert.JSONEq(t, `{"defaultGCPIdentityMode":"","defaultGCPIdentityServiceAccountID":""}`, string(b))

	empty := map[string]string{}
	b, err = json.Marshal(ProjectGCPIdentityDefaultsUpdate{ServiceAccountIDByProfile: &empty})
	require.NoError(t, err)
	assert.JSONEq(t, `{"defaultGCPIdentityServiceAccountIDByProfile":{}}`, string(b))
}

func TestProjectService_IdentityDefaultsRoutes(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"project":{"defaultGCPIdentityMode":"passthrough"},"settings":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"defaultGCPIdentityMode":"block"}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	require.NoError(t, err)

	res, err := c.Projects().GetResolvedSettings(context.Background(), "p1")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/projects/p1/settings/resolved", gotPath)
	assert.Equal(t, "passthrough", res.Project.DefaultGCPIdentityMode)

	mode := "block"
	ps, err := c.Projects().UpdateGCPIdentityDefaults(context.Background(), "p1", &ProjectGCPIdentityDefaultsUpdate{Mode: &mode})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/api/v1/projects/p1/settings", gotPath)
	assert.JSONEq(t, `{"defaultGCPIdentityMode":"block"}`, gotBody)
	assert.Equal(t, "block", ps.DefaultGCPIdentityMode)
}
