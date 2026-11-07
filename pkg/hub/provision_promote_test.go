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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AC-L5 at hub start: the promotion pass promotes every project tree on the
// shared Layout whose only sentinel is the workspace-root one, leaves the
// others alone (no state directory created, no write), and a second pass
// changes nothing.
func TestPromoteLegacyProvisionMarkers_SharedLayout(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	const legacyID = "aaaaaaaa-0000-4000-8000-0000000000a1"
	const cleanID = "aaaaaaaa-0000-4000-8000-0000000000a2"
	const noWorkspaceID = "aaaaaaaa-0000-4000-8000-0000000000a3"
	projects := filepath.Join(f.shareBase, "projects")
	legacyWS := filepath.Join(projects, legacyID, "workspace")
	cleanWS := filepath.Join(projects, cleanID, "workspace")
	for _, d := range []string{legacyWS, cleanWS, filepath.Join(projects, noWorkspaceID), filepath.Join(projects, ".not-a-project-id")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(legacyWS, provision.ProvisionSentinelFile), []byte("provisioned_at=x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(legacyWS, "README.md"), []byte("content\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cleanWS, "README.md"), []byte("content\n"), 0o644))

	counts := f.srv.promoteLegacyProvisionMarkers(context.Background())
	assert.Equal(t, legacyPromotionCounts{promoted: 1, unchanged: 2}, counts)
	assert.FileExists(t, filepath.Join(projects, legacyID, provision.ProvisionStateDirName, provision.ProvisionSentinelFile))
	assert.NoFileExists(t, filepath.Join(legacyWS, provision.ProvisionSentinelFile))
	assert.FileExists(t, filepath.Join(legacyWS, "README.md"), "workspace content is untouched")
	assert.NoDirExists(t, filepath.Join(projects, cleanID, provision.ProvisionStateDirName), "nothing to promote: nothing created")
	assert.NoDirExists(t, filepath.Join(projects, noWorkspaceID, provision.ProvisionStateDirName))

	again := f.srv.promoteLegacyProvisionMarkers(context.Background())
	assert.Equal(t, legacyPromotionCounts{unchanged: 3}, again)
}

// On the local workspace backend the pass does nothing.
func TestPromoteLegacyProvisionMarkers_LocalLayoutNoop(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	assert.Equal(t, legacyPromotionCounts{}, f.srv.promoteLegacyProvisionMarkers(context.Background()))
}
