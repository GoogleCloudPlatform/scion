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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// hub: harness-config clone scope handling and failure cleanup.
//
// Covers two independent issues in handleHarnessConfigClone:
//   - scope=user had no authorization branch at all (the switch simply fell
//     through), so an empty or another user's scopeId was accepted verbatim
//     instead of being pinned to the caller like handleTemplateClone already
//     does for templates.
//   - both clone handlers copy files to the destination path before creating
//     the store record, and clean up that path on create failure. If the
//     destination path was already occupied by an existing record (a name
//     collision), the copy silently lands on top of it before the collision
//     is ever detected, and the failure-path cleanup then removes a prefix
//     the request did not create. Both handlers now check for an existing
//     record at the destination before writing anything.
// ============================================================================

func setupCloneScopeSafetyServer(t *testing.T) (*Server, store.Store, *cloneMockStorage) {
	t.Helper()
	srv, s := testServer(t)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)
	return srv, s, stor
}

// --- scope=user authorization (finding a/b) ---------------------------

func TestHarnessConfigClone_UserScope_EmptyScopeIDPinnedToCaller(t *testing.T) {
	srv, s, _ := setupCloneScopeSafetyServer(t)
	ctx := context.Background()

	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "user-clone-source", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	caller := &store.User{ID: api.NewUUID(), Email: "hc-clone-caller@test.com", DisplayName: "Caller", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, caller))
	ensureHubMembership(ctx, s, caller.ID)

	rec := doRequestAsUser(t, srv, caller, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone", map[string]interface{}{
		"name":  "My Own Clone",
		"scope": "user",
		// scopeId deliberately omitted.
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.HarnessConfig
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
	assert.Equal(t, caller.ID, clone.ScopeID, "an empty scopeId must be pinned to the caller, never left ownerless")
	assert.Equal(t, caller.ID, clone.OwnerID)
}

func TestHarnessConfigClone_UserScope_OtherUserScopeIDRejected(t *testing.T) {
	srv, s, _ := setupCloneScopeSafetyServer(t)
	ctx := context.Background()

	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "cross-user-clone-source", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	caller := &store.User{ID: api.NewUUID(), Email: "hc-clone-attacker@test.com", DisplayName: "Attacker", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	otherUser := &store.User{ID: api.NewUUID(), Email: "hc-clone-victim@test.com", DisplayName: "Other", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, caller))
	require.NoError(t, s.CreateUser(ctx, otherUser))
	ensureHubMembership(ctx, s, caller.ID)
	ensureHubMembership(ctx, s, otherUser.ID)

	rec := doRequestAsUser(t, srv, caller, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone", map[string]interface{}{
		"name":    "Planted Clone",
		"scope":   "user",
		"scopeId": otherUser.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"cloning into another user's scope must be rejected, not silently retargeted; got: %s", rec.Body.String())

	// Confirm nothing was planted under the other user.
	list, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.HarnessConfigScopeUser, ScopeID: otherUser.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items, "no row may be created under a user the caller does not own")
}

func TestHarnessConfigClone_UserScope_OwnScopeIDAllowed(t *testing.T) {
	srv, s, _ := setupCloneScopeSafetyServer(t)
	ctx := context.Background()

	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "own-scope-clone-source", Name: "Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	caller := &store.User{ID: api.NewUUID(), Email: "hc-clone-self@test.com", DisplayName: "Self", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, caller))
	ensureHubMembership(ctx, s, caller.ID)

	rec := doRequestAsUser(t, srv, caller, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone", map[string]interface{}{
		"name":    "Explicit Self Clone",
		"scope":   "user",
		"scopeId": caller.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, "cloning into one's own explicit scopeId must succeed: %s", rec.Body.String())
}

// --- copy-before-create ordering (finding c) ---------------------------

// TestHarnessConfigClone_CollisionLeavesExistingConfigIntact is the
// regression for the copy-before-create / failure-path DeletePrefix
// ordering: a clone that collides with an existing config's (scope, slug)
// must be rejected before any storage write, so the existing config's files
// are neither overwritten by the copy nor removed by the failure cleanup.
func TestHarnessConfigClone_CollisionLeavesExistingConfigIntact(t *testing.T) {
	srv, s, stor := setupCloneScopeSafetyServer(t)
	ctx := context.Background()

	// Paths must match exactly what handleHarnessConfigClone itself computes
	// for the destination (storage.HarnessConfigStoragePath), or the clone's
	// copy would land on an unrelated mock-storage key and this test would
	// pass regardless of whether the collision is actually detected.
	existingPath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "collide")
	existingContent := []byte("FROM existing\n")
	stor.seedObject(existingPath+"/Dockerfile", existingContent)
	existing := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "collide", Name: "Existing", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: existingPath, StorageBucket: "test-bucket",
		Files:   []store.TemplateFile{{Path: "Dockerfile", Size: int64(len(existingContent))}},
		Created: time.Now(), Updated: time.Now(),
	}
	existing.ContentHash = computeContentHash(existing.Files)
	require.NoError(t, s.CreateHarnessConfig(ctx, existing))

	sourcePath := storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", "other-source")
	sourceContent := []byte("FROM attacker\n")
	stor.seedObject(sourcePath+"/Dockerfile", sourceContent)
	source := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "other-source", Name: "Other Source", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket",
		Files:   []store.TemplateFile{{Path: "Dockerfile", Size: int64(len(sourceContent))}},
		Created: time.Now(), Updated: time.Now(),
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateHarnessConfig(ctx, source))

	// Clone "other-source" naming it "collide" — same destination as the
	// existing config.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+source.ID+"/clone", map[string]interface{}{
		"name":  "collide",
		"scope": "global",
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "a name collision at the destination must 409 before any storage write; got: %s", rec.Body.String())

	// The existing record must be untouched.
	reloaded, err := s.GetHarnessConfig(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, existing.ContentHash, reloaded.ContentHash)

	// Its storage content must be untouched — not overwritten by the copy,
	// not removed by the failure-path cleanup.
	assert.Equal(t, existingContent, stor.content[existingPath+"/Dockerfile"],
		"the existing config's file content must survive a colliding clone attempt")

	// And it must still be readable via the API with the same content.
	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+existing.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var got HarnessConfigWithCapabilities
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&got))
	assert.Equal(t, existing.ContentHash, got.ContentHash)
}

// TestTemplateClone_CollisionLeavesExistingTemplateIntact is the template
// twin of TestHarnessConfigClone_CollisionLeavesExistingConfigIntact:
// handleTemplateClone has the identical copy-before-create ordering.
func TestTemplateClone_CollisionLeavesExistingTemplateIntact(t *testing.T) {
	srv, s, stor := setupCloneScopeSafetyServer(t)
	ctx := context.Background()

	existingPath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "collide")
	existingContent := []byte("scion-agent-config: existing\n")
	stor.seedObject(existingPath+"/scion-agent.yaml", existingContent)
	existing := &store.Template{
		ID: api.NewUUID(), Slug: "collide", Name: "Existing", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: existingPath, StorageBucket: "test-bucket",
		Files:   []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len(existingContent))}},
		Created: time.Now(), Updated: time.Now(),
	}
	existing.ContentHash = computeContentHash(existing.Files)
	require.NoError(t, s.CreateTemplate(ctx, existing))

	sourcePath := storage.TemplateStoragePath(srv.HubID(), store.TemplateScopeGlobal, "", "other-source")
	sourceContent := []byte("scion-agent-config: attacker\n")
	stor.seedObject(sourcePath+"/scion-agent.yaml", sourceContent)
	source := &store.Template{
		ID: api.NewUUID(), Slug: "other-source", Name: "Other Source", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		StoragePath: sourcePath, StorageBucket: "test-bucket",
		Files:   []store.TemplateFile{{Path: "scion-agent.yaml", Size: int64(len(sourceContent))}},
		Created: time.Now(), Updated: time.Now(),
	}
	source.ContentHash = computeContentHash(source.Files)
	require.NoError(t, s.CreateTemplate(ctx, source))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+source.ID+"/clone", map[string]interface{}{
		"name":  "collide",
		"scope": "global",
	})
	assert.Equal(t, http.StatusConflict, rec.Code, "a name collision at the destination must 409 before any storage write; got: %s", rec.Body.String())

	reloaded, err := s.GetTemplate(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, existing.ContentHash, reloaded.ContentHash)

	assert.Equal(t, existingContent, stor.content[existingPath+"/scion-agent.yaml"],
		"the existing template's file content must survive a colliding clone attempt")

	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/templates/"+existing.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var got TemplateWithCapabilities
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&got))
	assert.Equal(t, existing.ContentHash, got.ContentHash)
}
