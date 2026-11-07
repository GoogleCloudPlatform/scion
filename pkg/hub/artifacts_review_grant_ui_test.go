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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/stretchr/testify/require"
)

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
