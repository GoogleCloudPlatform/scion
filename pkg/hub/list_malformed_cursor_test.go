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
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestPaginatedLists_MalformedCursorReturns400 sends a malformed ?cursor to
// every paginated hub list endpoint backed by the entadapter
// decodeListCursor and asserts 400, never 500 (ptone/scion#1957). Some
// endpoints validate or unseal the cursor before the store sees it; others
// (runtime brokers, skills) hand it straight to the store, which is where
// decodeListCursor's store.ErrInvalidInput wrapping matters.
func TestPaginatedLists_MalformedCursorReturns400(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Cursor Project", Slug: "cursor-project",
	}))

	enc := func(raw string) string { return base64.URLEncoding.EncodeToString([]byte(raw)) }
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	cursors := map[string]string{
		"not base64":    "not-base64-!!!",
		"padding only":  "====",
		"wrong shape":   enc("some-garbage"),
		"bad timestamp": enc("not-a-timestamp," + uuid.NewString()),
		"bad id":        enc(ts + ",not-a-uuid"),
	}

	endpoints := map[string]string{
		"agents":          "/api/v1/agents",
		"projects":        "/api/v1/projects",
		"project agents":  "/api/v1/projects/" + projectID + "/agents",
		"templates":       "/api/v1/templates",
		"harness configs": "/api/v1/harness-configs",
		"groups":          "/api/v1/groups",
		"skills":          "/api/v1/skills",
		"runtime brokers": "/api/v1/runtime-brokers",
	}

	for epName, path := range endpoints {
		for cName, cursor := range cursors {
			t.Run(epName+"/"+cName, func(t *testing.T) {
				rec := doRequest(t, srv, http.MethodGet, path+"?"+url.Values{"cursor": {cursor}}.Encode(), nil)
				assert.Equal(t, http.StatusBadRequest, rec.Code,
					"malformed cursor must be a 400, not a %d; body: %s", rec.Code, rec.Body.String())
			})
		}
	}
}
