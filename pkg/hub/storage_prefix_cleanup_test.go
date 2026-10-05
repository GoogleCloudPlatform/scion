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
	"net/http"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// ptone/scion#2045: DeletePrefix on a directory-like storage path must pass
// the path with a trailing "/". Object stores such as GCS match prefixes as
// plain strings, so DeletePrefix("a/foo") also deletes "a/foo-bar/...".
// cloneMockStorage.DeletePrefix does the same plain strings.HasPrefix match,
// so each test seeds a sibling key that extends the cleaned-up path and
// asserts it survives.
// ============================================================================

// siblingSeedingStorage seeds a sibling of the clone's destination directory
// (dir + "-sibling/keep.yaml") on the first Copy, i.e. once the handler has
// computed its request-unique storage path, so the failure cleanup that
// follows has a key extending its path to (wrongly) match.
type siblingSeedingStorage struct {
	*cloneMockStorage
	once    sync.Once
	dir     string // the clone's destination directory
	sibling string
}

func (m *siblingSeedingStorage) Copy(ctx context.Context, srcPath, dstPath string) (*storage.Object, error) {
	m.once.Do(func() {
		m.dir = path.Dir(dstPath)
		m.sibling = m.dir + "-sibling/keep.yaml"
		m.seedObject(m.sibling, []byte("keep"))
	})
	return m.cloneMockStorage.Copy(ctx, srcPath, dstPath)
}

func (m *cloneMockStorage) hasObject(objectPath string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[objectPath]
	return ok
}

// cleanupTestFiles is a two-file manifest whose second file is never seeded,
// so the clone's second Copy fails after the first one wrote into the
// destination directory.
var cleanupTestFiles = []store.TemplateFile{
	{Path: "a.yaml", Size: 1, Hash: "a"},
	{Path: "missing.yaml", Size: 1, Hash: "b"},
}

func TestTemplateClone_CopyFailureCleanupKeepsSiblingPrefix(t *testing.T) {
	srv, s := testServer(t)
	stor := &siblingSeedingStorage{cloneMockStorage: newCloneMockStorage("test-bucket")}
	srv.SetStorage(stor)
	ctx := context.Background()

	srcPath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "cleanup-src")
	source := &store.Template{
		ID: api.NewUUID(), Name: "cleanup-src", Slug: "cleanup-src", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: srcPath, Files: cleanupTestFiles,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateTemplate(ctx, source))
	stor.seedObject(srcPath+"/a.yaml", []byte("a"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+source.ID+"/clone",
		map[string]interface{}{"name": "Cleanup Clone", "scope": "global"})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String()) // RuntimeError

	require.NotEmpty(t, stor.sibling, "clone never reached Copy")
	assert.True(t, stor.hasObject(stor.sibling), "failure cleanup deleted sibling %q", stor.sibling)
	assert.False(t, stor.hasObject(stor.dir+"/a.yaml"), "failure cleanup must still remove the clone's own partial copy")
}

func TestHarnessConfigClone_CopyFailureCleanupKeepsSiblingPrefix(t *testing.T) {
	srv, s := testServer(t)
	stor := &siblingSeedingStorage{cloneMockStorage: newCloneMockStorage("test-bucket")}
	srv.SetStorage(stor)
	ctx := context.Background()

	srcPath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "cleanup-src")
	source := &store.HarnessConfig{
		ID: api.NewUUID(), Name: "cleanup-src", Slug: "cleanup-src", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: srcPath, Files: cleanupTestFiles,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, source))
	stor.seedObject(srcPath+"/a.yaml", []byte("a"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone",
		map[string]interface{}{"name": "Cleanup Clone", "scope": "global"})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String()) // RuntimeError

	require.NotEmpty(t, stor.sibling, "clone never reached Copy")
	assert.True(t, stor.hasObject(stor.sibling), "failure cleanup deleted sibling %q", stor.sibling)
	assert.False(t, stor.hasObject(stor.dir+"/a.yaml"), "failure cleanup must still remove the clone's own partial copy")
}

func TestTemplateDelete_DeleteFilesKeepsSiblingPrefix(t *testing.T) {
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)
	ctx := context.Background()

	tplPath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "foo")
	tpl := &store.Template{
		ID: api.NewUUID(), Name: "foo", Slug: "foo", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: tplPath, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateTemplate(ctx, tpl))
	own := tplPath + "/template.yaml"
	sibling := tplPath + "-bar/template.yaml" // template "foo-bar"'s files
	stor.seedObject(own, []byte("foo"))
	stor.seedObject(sibling, []byte("foo-bar"))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tpl.ID+"?deleteFiles=true", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	assert.False(t, stor.hasObject(own), "template's own files must be deleted")
	assert.True(t, stor.hasObject(sibling), "deleting template foo must not delete foo-bar's files")
}

// TestUserTemplateDelete_DeleteFilesKeepsSiblingPrefix is the user-template
// counterpart (DELETE /api/v1/users/me/templates/{id}?deleteFiles=true): the
// owner's template "foo" must not take "foo-bar"'s files with it.
func TestUserTemplateDelete_DeleteFilesKeepsSiblingPrefix(t *testing.T) {
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)
	ctx := context.Background()

	owner := &store.User{
		ID: api.NewUUID(), Email: "user-tpl-owner@test.com", DisplayName: "Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))

	tplPath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeUser, owner.ID, "foo")
	tpl := &store.Template{
		ID: api.NewUUID(), Name: "foo", Slug: "foo", Harness: "claude",
		Scope: store.TemplateScopeUser, ScopeID: owner.ID, OwnerID: owner.ID,
		Status: store.TemplateStatusActive, StoragePath: tplPath,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateTemplate(ctx, tpl))
	own := tplPath + "/template.yaml"
	sibling := tplPath + "-bar/template.yaml" // user template "foo-bar"'s files
	stor.seedObject(own, []byte("foo"))
	stor.seedObject(sibling, []byte("foo-bar"))

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete,
		"/api/v1/users/me/templates/"+tpl.ID+"?deleteFiles=true", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	assert.False(t, stor.hasObject(own), "user template's own files must be deleted")
	assert.True(t, stor.hasObject(sibling), "deleting user template foo must not delete foo-bar's files")
}

func TestHarnessConfigDelete_DeleteFilesKeepsSiblingPrefix(t *testing.T) {
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)
	ctx := context.Background()

	hcPath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "foo")
	hc := &store.HarnessConfig{
		ID: api.NewUUID(), Name: "foo", Slug: "foo", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: hcPath, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	own := hcPath + "/config.yaml"
	sibling := hcPath + "-bar/config.yaml"
	stor.seedObject(own, []byte("foo"))
	stor.seedObject(sibling, []byte("foo-bar"))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"?deleteFiles=true", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	assert.False(t, stor.hasObject(own), "harness config's own files must be deleted")
	assert.True(t, stor.hasObject(sibling), "deleting harness config foo must not delete foo-bar's files")
}

// TestProjectDelete_StorageCleanupKeepsSiblingPrefix covers the broadest
// delete in #2045: deleting a project removes each project-scoped template's
// and harness config's storage directory (deleteStorageFiles). A key that
// merely extends one of those paths ("<path>-bar/...") belongs to something
// else and must survive.
func TestProjectDelete_StorageCleanupKeepsSiblingPrefix(t *testing.T) {
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)
	ctx := context.Background()

	projectID := api.NewUUID()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Delete Me", Slug: "delete-me", OwnerID: DevUserID, CreatedBy: DevUserID,
	}))

	tplPath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeProject, projectID, "foo")
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: api.NewUUID(), Name: "foo", Slug: "foo", Harness: "claude",
		Scope: store.TemplateScopeProject, ScopeID: projectID, Status: store.TemplateStatusActive,
		StoragePath: tplPath, Created: time.Now(), Updated: time.Now(),
	}))
	hcPath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeProject, projectID, "foo")
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: api.NewUUID(), Name: "foo", Slug: "foo", Harness: "claude",
		Scope: store.HarnessConfigScopeProject, ScopeID: projectID, Status: store.HarnessConfigStatusActive,
		StoragePath: hcPath, Created: time.Now(), Updated: time.Now(),
	}))

	ownTpl, siblingTpl := tplPath+"/template.yaml", tplPath+"-bar/template.yaml"
	ownHC, siblingHC := hcPath+"/config.yaml", hcPath+"-bar/config.yaml"
	for _, p := range []string{ownTpl, siblingTpl, ownHC, siblingHC} {
		stor.seedObject(p, []byte("x"))
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	assert.False(t, stor.hasObject(ownTpl), "project template's own files must be deleted")
	assert.False(t, stor.hasObject(ownHC), "project harness config's own files must be deleted")
	assert.True(t, stor.hasObject(siblingTpl), "project delete must not delete %q", siblingTpl)
	assert.True(t, stor.hasObject(siblingHC), "project delete must not delete %q", siblingHC)
}
