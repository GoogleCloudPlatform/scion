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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactsReviewOnRoutes: a project member with a write grant reviews
// an agent's artifact through the hub routes. A review with an unmarked
// edit is refused with 422 and announces nothing; an accepted review
// becomes current, ?resolve=clean returns the parent, and the owning agent
// receives one artifact-review system notice from the hub whose body names
// the version and how to fetch it.
func TestArtifactsReviewOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	// A store on a database this test can also write grants to.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "review.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	srv.SetArtifactStore(st)
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	ctx := context.Background()
	p1 := artifactProject(t, s, "review-p1")
	owner, ownerTok := artifactAgent(t, srv, s, p1.ID, "review-owner", AgentRoleBaseline)
	createTestUserWithProjectRole(t, s, tid("art-reviewer"), "art-reviewer@test.com", p1.ID, store.ProjectRoleMember)
	reviewer, err := s.GetUser(ctx, tid("art-reviewer"))
	require.NoError(t, err)

	const parent = "# Plan\n\nWe ship in Q3.\n"
	manifest := func(kind, key, body string) []byte {
		sum := sha256.Sum256([]byte(body))
		b, err := json.Marshal(artifacts.CreateVersionRequest{Kind: kind, Key: key, Entry: "plan.md",
			Files: []artifacts.ManifestFile{{Path: "plan.md", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}})
		require.NoError(t, err)
		return b
	}
	// The owner agent publishes v1.
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest("", "plan", parent), ownerTok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
	id := pend.Artifact.ID
	require.Equal(t, http.StatusNoContent, doRawAgentRequest(t, srv, http.MethodPut, "/api/v1/artifacts/"+id+"/versions/1/files/plan.md", []byte(parent), ownerTok).Code)
	require.Equal(t, http.StatusOK, doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts/"+id+"/versions/1/finalize", nil, ownerTok).Code)

	_, err = db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"g-review", id, artifacts.SubjectPrincipal, artifacts.PrincipalRef(artifacts.PrincipalKindUser, reviewer.ID), artifacts.GrantWrite,
		time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	require.NoError(t, err)

	review := func(body string) *httpRecorder {
		t.Helper()
		rec := userArtifactRequest(t, srv, reviewer, http.MethodPost, "/api/v1/artifacts/"+id+"/versions", manifest(artifacts.VersionKindReview, "", body))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var p artifacts.PendingVersionResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		base := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", id, p.Version.Seq)
		require.Equal(t, http.StatusNoContent, userArtifactRequest(t, srv, reviewer, http.MethodPut, base+"/files/plan.md", []byte(body)).Code)
		fin := userArtifactRequest(t, srv, reviewer, http.MethodPost, base+"/finalize", nil)
		return &httpRecorder{code: fin.Code, body: fin.Body.String()}
	}

	rejected := review("# Plan\n\nWe ship in Q4.{>>sure?<<}\n")
	require.Equal(t, http.StatusUnprocessableEntity, rejected.code, rejected.body)
	assert.Contains(t, rejected.body, artifacts.CodeUnmarkedChanges)

	accepted := review("# Plan\n\nWe {~~ship~>launch~~} in Q3.\n")
	require.Equal(t, http.StatusOK, accepted.code, accepted.body)
	assert.Contains(t, accepted.body, `"currentSeq":3`)

	rec = userArtifactRequest(t, srv, reviewer, http.MethodGet, "/api/v1/artifacts/"+id+"/files/plan.md?resolve=clean", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, parent, rec.Body.String())
	assert.Equal(t, "clean", rec.Header().Get(artifacts.HeaderResolve))

	require.Eventually(t, func() bool { return len(dispatcher.getCalls()) > 0 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "one notice, for the accepted review only")
	c := calls[0]
	assert.Equal(t, owner.ID, c.Agent.ID)
	m := c.StructuredMessage
	require.NotNil(t, m)
	assert.Equal(t, messages.TypeSystem, m.Type)
	assert.Equal(t, "system", m.Sender)
	assert.Equal(t, messages.SystemCategoryArtifactReview, m.Metadata["system_category"])
	assert.Empty(t, m.Metadata[artifacts.MessageMetadataKey], "the reference travels in the body, not the admitted metadata key")
	ref := artifacts.FormatRef(id, 3)
	assert.Contains(t, m.Msg, "Artifact: v3 - scion artifact get "+ref)
	assert.Contains(t, m.Msg, ref+" --clean")
	assert.Contains(t, m.Msg, "scion artifact publish <file> --version-of "+artifacts.FormatRef(id, 0))
	assert.False(t, strings.Contains(m.Msg, reviewer.Email) || strings.Contains(m.Msg, reviewer.ID), "the notice names no reviewer identity")
}

type httpRecorder struct {
	code int
	body string
}
