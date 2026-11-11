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
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skillVersionStatusFixture is a project skill with one version in each
// status, each holding a SKILL.md, and callers in each visibility class:
//   - admin: project owner, update access on the skill (not its owner).
//   - owner: project member whose ID is the skill's OwnerID.
//   - creator: project member whose ID is the skill's CreatedBy.
//   - reader: project member with read access only.
type skillVersionStatusFixture struct {
	srv                           *Server
	admin, owner, creator, reader *store.User
	skill                         *store.Skill
	versionIDs                    map[string]string // version number -> ID
}

// skillVersionStatusCases maps each version number of the fixture to its
// status and whether a read-only caller sees it.
var skillVersionStatusCases = []struct {
	version string
	status  string
	listed  bool
}{
	{"1.0.0", store.SkillVersionStatusPublished, true},
	{"1.1.0", store.SkillVersionStatusDeprecated, true},
	{"1.2.0", store.SkillVersionStatusDraft, false},
	{"1.3.0", store.SkillVersionStatusArchived, false},
}

func skillVersionStatusFileContent(version string) []byte {
	return []byte("# status filter " + version + "\n")
}

func setupSkillVersionStatusFixture(t *testing.T) *skillVersionStatusFixture {
	t.Helper()
	ctx := context.Background()
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(stor)

	member := func(name string) *store.User {
		u := createNamedTestUser(t, s, name, store.UserRoleMember)
		ensureHubMembership(ctx, s, u.ID)
		grantProjectRole(t, s, u.ID, project.ID, store.ProjectRoleMember)
		return u
	}
	f := &skillVersionStatusFixture{
		srv:        srv,
		admin:      alice,
		owner:      member("svs-owner"),
		creator:    member("svs-creator"),
		reader:     member("svs-reader"),
		versionIDs: map[string]string{},
	}

	name := "status-filter-" + api.NewUUID()[:8]
	f.skill = &store.Skill{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Scope:       store.SkillScopeProject,
		ScopeID:     project.ID,
		OwnerID:     f.owner.ID,
		CreatedBy:   f.creator.ID,
		Status:      "active",
		StoragePath: "skills/project/" + api.Slugify(name),
	}
	require.NoError(t, s.CreateSkill(ctx, f.skill))

	for _, tc := range skillVersionStatusCases {
		addLocalSkillVersion(t, srv, stor, f.skill, tc.version, map[string][]byte{
			"SKILL.md": skillVersionStatusFileContent(tc.version),
		})
		f.setStatus(t, tc.version, tc.status)
	}
	return f
}

// setStatus moves version of the fixture's skill to status and records its ID.
func (f *skillVersionStatusFixture) setStatus(t *testing.T, version, status string) {
	t.Helper()
	ctx := context.Background()
	sv, err := f.srv.store.GetSkillVersionByNumber(ctx, f.skill.ID, version)
	require.NoError(t, err)
	sv.Status = status
	require.NoError(t, f.srv.store.UpdateSkillVersion(ctx, sv))
	f.versionIDs[version] = sv.ID
}

func (f *skillVersionStatusFixture) fileURL(version string) string {
	return "/api/v1/skills/" + f.skill.ID + "/files/SKILL.md?raw=1&version=" + version
}

func (f *skillVersionStatusFixture) detailsURL(versionID string) string {
	return "/api/v1/skills/" + f.skill.ID + "/versions/" + versionID
}

func (f *skillVersionStatusFixture) listVersions(t *testing.T, user *store.User) []store.SkillVersion {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/skills/"+f.skill.ID+"/versions", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result store.ListResult[store.SkillVersion]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, len(result.Items), result.TotalCount, "totalCount matches the filtered rows")
	return result.Items
}

func versionNumbers(items []store.SkillVersion) []string {
	out := make([]string, 0, len(items))
	for _, sv := range items {
		out = append(out, sv.Version)
	}
	sort.Strings(out)
	return out
}

// TestSkillFileRead_VersionStatusFilter: an unsigned file read by version
// number serves published and deprecated versions to every reader, and draft
// and archived versions only to the skill's owner, its creator, and callers
// with update access. A filtered-out version gets exactly the response of a
// version number that does not exist.
func TestSkillFileRead_VersionStatusFilter(t *testing.T) {
	f := setupSkillVersionStatusFixture(t)
	missing := doRawRequestAsUser(t, f.srv, f.reader, http.MethodGet, f.fileURL("9.9.9"), nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())

	for _, tc := range skillVersionStatusCases {
		t.Run(tc.status+"/reader", func(t *testing.T) {
			rec := doRawRequestAsUser(t, f.srv, f.reader, http.MethodGet, f.fileURL(tc.version), nil)
			if tc.listed {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, skillVersionStatusFileContent(tc.version), rec.Body.Bytes())
				return
			}
			assertSameResponse(t, missing, rec, tc.status)
		})
		for _, c := range []struct {
			name string
			user *store.User
		}{{"owner", f.owner}, {"creator", f.creator}, {"admin", f.admin}} {
			t.Run(tc.status+"/"+c.name, func(t *testing.T) {
				rec := doRawRequestAsUser(t, f.srv, c.user, http.MethodGet, f.fileURL(tc.version), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, skillVersionStatusFileContent(tc.version), rec.Body.Bytes())
			})
		}
	}
}

// TestSkillVersionGet_StatusFilter: the single-version details endpoint
// applies the same status filter, and a filtered-out version, like a
// version of another skill, gets exactly the response of a version ID that
// does not exist.
func TestSkillVersionGet_StatusFilter(t *testing.T) {
	f := setupSkillVersionStatusFixture(t)
	missing := doRequestAsUser(t, f.srv, f.reader, http.MethodGet, f.detailsURL(api.NewUUID()), nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())

	t.Run("version of another skill", func(t *testing.T) {
		other := createTestSkill(t, f.srv.store, "status-filter-other-"+api.NewUUID()[:8],
			store.SkillScopeProject, f.skill.ScopeID, f.owner.ID)
		otherVersion := &store.SkillVersion{
			ID:      api.NewUUID(),
			SkillID: other.ID,
			Version: "1.0.0",
			Status:  store.SkillVersionStatusPublished,
		}
		require.NoError(t, f.srv.store.CreateSkillVersion(context.Background(), otherVersion))
		for _, user := range []*store.User{f.reader, f.admin} {
			rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.detailsURL(otherVersion.ID), nil)
			assertSameResponse(t, missing, rec, "version of another skill")
		}
	})

	for _, tc := range skillVersionStatusCases {
		id := f.versionIDs[tc.version]
		t.Run(tc.status+"/reader", func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.reader, http.MethodGet, f.detailsURL(id), nil)
			if tc.listed {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), `"version":"`+tc.version+`"`)
				return
			}
			assertSameResponse(t, missing, rec, tc.status)
		})
		for _, c := range []struct {
			name string
			user *store.User
		}{{"owner", f.owner}, {"creator", f.creator}, {"admin", f.admin}} {
			t.Run(tc.status+"/"+c.name, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, c.user, http.MethodGet, f.detailsURL(id), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), `"version":"`+tc.version+`"`)
			})
		}
	}
}

// TestSkillVersionList_StatusFilter: the version list applies the same
// status filter in the store, so a filtered-out version is a missing row and
// totalCount counts only the rows returned. Rows that remain are identical
// to the rows an unfiltered caller gets.
func TestSkillVersionList_StatusFilter(t *testing.T) {
	f := setupSkillVersionStatusFixture(t)

	var all, listed []string
	for _, tc := range skillVersionStatusCases {
		all = append(all, tc.version)
		if tc.listed {
			listed = append(listed, tc.version)
		}
	}

	readerItems := f.listVersions(t, f.reader)
	assert.Equal(t, listed, versionNumbers(readerItems))

	for _, c := range []struct {
		name string
		user *store.User
	}{{"owner", f.owner}, {"creator", f.creator}, {"admin", f.admin}} {
		t.Run(c.name, func(t *testing.T) {
			items := f.listVersions(t, c.user)
			assert.Equal(t, all, versionNumbers(items))

			byVersion := map[string]store.SkillVersion{}
			for _, sv := range items {
				byVersion[sv.Version] = sv
			}
			for _, sv := range readerItems {
				want, err := json.Marshal(byVersion[sv.Version])
				require.NoError(t, err)
				got, err := json.Marshal(sv)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(got), sv.Version)
			}
		})
	}
}

// TestSkillFileRead_SignedURLStatus: a signed file URL is bound to the
// version it was issued for and is served without the status filter. Signed
// URLs are issued only for versions that resolve (published or deprecated);
// a URL issued before its version is archived keeps serving that version
// until it expires (skillFileURLTTL), as before the status filter.
func TestSkillFileRead_SignedURLStatus(t *testing.T) {
	f := setupSkillVersionStatusFixture(t)
	signedURL := func(version string) string {
		suffix := f.srv.signDownloadURL(f.skill.ID, version, "SKILL.md", time.Now().Add(time.Minute))
		require.NotEmpty(t, suffix)
		return f.fileURL(version) + "&" + suffix
	}
	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("published", func(t *testing.T) {
		rec := get(signedURL("1.0.0"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, skillVersionStatusFileContent("1.0.0"), rec.Body.Bytes())
	})

	t.Run("archived after issue", func(t *testing.T) {
		target := signedURL("1.0.0")
		f.setStatus(t, "1.0.0", store.SkillVersionStatusArchived)
		t.Cleanup(func() { f.setStatus(t, "1.0.0", store.SkillVersionStatusPublished) })
		rec := get(target)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, skillVersionStatusFileContent("1.0.0"), rec.Body.Bytes())
	})
}
