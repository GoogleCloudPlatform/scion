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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canGrantReview on the hub's routes (design D24, ptone/scion#4014;
// ptone/scion#4593): the field is stated by the real artifact host and
// authz service, and agrees with POST /grants for every caller.

// reviewGrantArtifact publishes, straight into the artifact store, an
// artifact owned by f.agent and homed in f.home, with the home project's
// read grant, and returns its id.
func reviewGrantArtifact(t *testing.T, f reviewGrantFixture, st artifacts.Store, blobs *storage.LocalStorage) string {
	t.Helper()
	ctx := context.Background()
	content := []byte("# draft")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err := blobs.Upload(ctx, artifacts.BlobPath(f.srv.HubID(), digest), bytes.NewReader(content), storage.UploadOptions{})
	require.NoError(t, err)
	now := time.Now()
	id := tid("rg-ui-artifact")
	a := &artifacts.Artifact{ID: id, ScopeKind: artifacts.ScopeKindProject, ScopeRef: f.home,
		OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: f.agent.ID, Title: "draft", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: tid("rg-ui-artifact-v1"), ArtifactID: id, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "draft.md", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	require.NoError(t, st.CreatePublished(ctx, a, v,
		[]artifacts.File{{VersionID: v.ID, Path: "draft.md", Size: int64(len(content)), SHA256: digest, MediaType: "text/markdown"}},
		[]artifacts.Grant{{ID: tid("rg-ui-home"), ArtifactID: id, SubjectKind: artifacts.SubjectScope,
			SubjectRef: f.home, Permission: artifacts.GrantRead, CreatedAt: now}}))
	return id
}

// TestArtifactCanGrantReviewOnRoutes: GET of the artifact and of a version
// states canGrantReview for the delegating user of the owning agent and for
// admins and owners of the home project, and for nobody else; a write
// grant to a user by the same caller succeeds exactly when canManage or
// canGrantReview is true; a caller that cannot read the artifact gets the
// 404 of a missing one.
func TestArtifactCanGrantReviewOnRoutes(t *testing.T) {
	type caller struct {
		name     string
		identity func(f reviewGrantFixture) Identity
		// adminGrant gives the caller (a user) an admin grant first.
		adminGrant     bool
		manage, review bool
	}
	user := func(pick func(f reviewGrantFixture) string) func(f reviewGrantFixture) Identity {
		return func(f reviewGrantFixture) Identity { return f.user(pick(f)) }
	}
	cases := []caller{
		{name: "delegating user", identity: user(func(f reviewGrantFixture) string { return f.delegator }), review: true},
		{name: "home-project admin", identity: user(func(f reviewGrantFixture) string { return f.admin }), review: true},
		{name: "home-project owner", identity: user(func(f reviewGrantFixture) string { return f.owner }), review: true},
		{name: "home-project member", identity: user(func(f reviewGrantFixture) string { return f.member }), review: false},
		{name: "owning agent", identity: func(f reviewGrantFixture) Identity {
			return artifactTestAgent(f.agent.ID, f.home, ScopeProjectArtifactRead, ScopeProjectArtifactWrite)
		}},
		{name: "admin grantee", identity: user(func(f reviewGrantFixture) string { return f.member }), adminGrant: true, manage: true},
		{name: "delegating user with an admin grant", identity: user(func(f reviewGrantFixture) string { return f.delegator }),
			adminGrant: true, manage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewGrantFixture(t)
			st, blobs := enableArtifactsForTest(t, f.srv)
			id := reviewGrantArtifact(t, f, st, blobs)
			identity := tc.identity(f)
			if tc.adminGrant {
				_, err := st.PutGrant(context.Background(), &artifacts.Grant{ID: tid("rg-ui-admin-" + tc.name), ArtifactID: id,
					SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, identity.ID()),
					Permission: artifacts.GrantAdmin, CreatedAt: time.Now()}, artifacts.MaxGrantsPerArtifact, true)
				require.NoError(t, err)
			}
			var resp artifacts.ArtifactResponse
			for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
				rec := identityArtifactRequest(t, f.srv, identity, http.MethodGet, target, nil)
				require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", target, rec.Body.String())
				resp = artifacts.ArtifactResponse{}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, tc.manage, resp.CanManage, "GET %s canManage", target)
				assert.Equal(t, tc.review, resp.CanGrantReview, "GET %s canGrantReview", target)
				if !tc.review {
					assert.NotContains(t, rec.Body.String(), `"canGrantReview"`, "GET %s", target)
				}
			}
			// Parity: the grant route agrees with the capabilities.
			assert.False(t, resp.CanManage && resp.CanGrantReview, "canManage and canGrantReview both true")
			body := `{"subjectKind":"principal","subjectRef":"user:` + tid("rg-ui-reviewer") + `","permission":"write"}`
			post := identityArtifactRequest(t, f.srv, identity, http.MethodPost, "/api/v1/artifacts/"+id+"/grants", []byte(body))
			granted := post.Code == http.StatusCreated || post.Code == http.StatusOK
			if !granted {
				require.Equal(t, http.StatusForbidden, post.Code, "POST /grants: %s", post.Body.String())
			}
			assert.Equal(t, resp.CanManage || resp.CanGrantReview, granted,
				"canManage=%v canGrantReview=%v but POST /grants answered %d %s",
				resp.CanManage, resp.CanGrantReview, post.Code, post.Body.String())
		})
	}

	t.Run("revoked delegation", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		_, err := f.s.DeactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, f.agent.ID,
			store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "rg-ui-revoke"})
		require.NoError(t, err)
		rec := identityArtifactRequest(t, f.srv, f.user(f.delegator), http.MethodGet, "/api/v1/artifacts/"+id, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), `"canGrantReview"`)
	})

	t.Run("unreadable is missing", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		// otherAdmin administers another project only; outside is a member
		// of another project. Neither can read the artifact.
		for _, uid := range []string{f.otherAdmin, f.outside} {
			missing := identityArtifactRequest(t, f.srv, f.user(uid), http.MethodGet, "/api/v1/artifacts/"+tid("rg-ui-missing"), nil)
			got := identityArtifactRequest(t, f.srv, f.user(uid), http.MethodGet, "/api/v1/artifacts/"+id, nil)
			assert.Equal(t, http.StatusNotFound, got.Code)
			assert.Equal(t, missing.Body.String(), got.Body.String())
			assert.False(t, strings.Contains(got.Body.String(), "canGrantReview"))
		}
	})
}

// TestArtifactCanGrantReviewGrantReadFailureIsLoud: when the artifact's
// grants cannot be read while stating the caller's rights, GET answers 500
// for a caller with review authority, never a 200 without the capability.
func TestArtifactCanGrantReviewGrantReadFailureIsLoud(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, f.srv)
	id := reviewGrantArtifact(t, f, st, blobs)
	f.srv.SetArtifactStore(failListGrantsStore{st})
	for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
		rec := identityArtifactRequest(t, f.srv, f.user(f.delegator), http.MethodGet, target, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "GET %s: %s", target, rec.Body.String())
	}
}

// failListGrantsStore is an artifact store whose ListGrants fails.
type failListGrantsStore struct{ artifacts.Store }

func (failListGrantsStore) ListGrants(context.Context, string) ([]artifacts.Grant, error) {
	return nil, errGrantsUnavailable
}

var errGrantsUnavailable = errors.New("grants unavailable")
