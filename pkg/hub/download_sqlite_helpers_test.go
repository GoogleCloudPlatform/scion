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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func addLocalSkillVersion(t *testing.T, srv *Server, stor storage.Storage, skill *store.Skill, version string, files map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	var manifest []store.TemplateFile
	for p, c := range files {
		_, err := stor.Upload(ctx, skill.StoragePath+"/"+version+"/"+p, bytes.NewReader(c), storage.UploadOptions{})
		require.NoError(t, err)
		manifest = append(manifest, store.TemplateFile{Path: p, Size: int64(len(c)), Hash: sha256Hex(c)})
	}
	require.NoError(t, srv.store.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:          api.NewUUID(),
		SkillID:     skill.ID,
		Version:     version,
		ContentHash: "sha256:" + version,
		Status:      store.SkillVersionStatusPublished,
		Files:       manifest,
		Created:     time.Now(),
	}))
}
