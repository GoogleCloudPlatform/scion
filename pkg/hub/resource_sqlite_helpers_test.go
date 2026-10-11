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
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
	"github.com/stretchr/testify/require"
)

// ensureAdminRoleBinding grants a super-admin role binding to the given user
// (CO1 cutover: role bindings are required for authorization).
func ensureAdminRoleBinding(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create admin role binding: %v", err)
	}
}

// testFS creates a minimal in-memory fs.FS with the given file contents.
func testFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for path, content := range files {
		m[path] = &fstest.MapFile{Data: []byte(content), Mode: 0644}
	}
	return m
}

// testBundledResource creates a BundledResource for testing.
func testBundledResource(kind storage.ResourceKind, name string, files map[string]string) resources.BundledResource {
	return resources.BundledResource{
		Kind:      kind,
		Name:      name,
		Scope:     "global",
		ScopeID:   "",
		SourceURL: "builtin://scion/dev/" + string(kind) + "/" + name,
		FS:        testFS(files),
		Root:      ".",
	}
}
