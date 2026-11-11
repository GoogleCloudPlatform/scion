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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactDecisionLogThroughHub: the logger set with SetArtifactLogger
// receives the artifact service's decisions, served through the hub's full
// handler, each with the request id of the request log line of the same
// request (ptone/scion#5120). The share token never reaches it.
func TestArtifactDecisionLogThroughHub(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "declog-p1")
	createTestUserWithProjectRole(t, s, tid("declog-owner"), "declog-owner@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("declog-member"), "declog-member@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, tid("declog-owner"))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, tid("declog-member"))
	require.NoError(t, err)

	srv.SetRequestLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	var out syncBuffer
	srv.SetArtifactLogger(slog.New(logging.NewRequestIDHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))))

	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=log.md&scope="+p1.ID, []byte("# decision log"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	publishReq := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, publishReq)

	rec = userArtifactRequest(t, srv, member, http.MethodPost, "/api/v1/artifacts/"+id+"/links", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	refusedReq := rec.Header().Get("X-Request-ID")

	rec = userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts/"+id+"/links", nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var link struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &link))
	token := strings.TrimPrefix(link.URL, "/api/v1/artifacts/shared/")
	require.NotEmpty(t, token)

	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		entries = append(entries, m)
	}
	require.Len(t, entries, 3, out.String())

	assert.Equal(t, "published", entries[0]["action"])
	assert.Equal(t, id, entries[0]["artifact_id"])
	assert.Equal(t, p1.ID, entries[0]["project_id"])
	assert.Equal(t, owner.ID, entries[0]["principal_id"])
	assert.Equal(t, publishReq, entries[0]["request_id"], "the entry joins to the request log line")

	assert.Equal(t, "WARN", entries[1]["level"])
	assert.Equal(t, "refused", entries[1]["outcome"])
	assert.Equal(t, "not_owner_or_admin_grant", entries[1]["reason"])
	assert.Equal(t, refusedReq, entries[1]["request_id"])

	assert.Equal(t, "link_created", entries[2]["action"])
	assert.Equal(t, "POST /api/v1/artifacts/{id}/links", entries[2]["route"])
	assert.NotContains(t, out.String(), token, "the share token reached the decision log")
}
