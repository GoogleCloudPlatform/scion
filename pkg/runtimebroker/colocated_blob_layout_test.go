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

package runtimebroker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// TestHydrateTemplate_BlobLayoutMissesDirectRead is acceptance 3 of
// ptone/scion#4221 on the broker side: a blob-layout template's storage path
// (<slug path>.<id>) is never a directory, only its .blobs sibling is, so a
// co-located broker's direct read misses and the template is hydrated
// through the hub's download URLs. A legacy row whose tree is still on disk
// keeps resolving directly, even next to a migrated row of the same slug.
func TestHydrateTemplate_BlobLayoutMissesDirectRead(t *testing.T) {
	stor, legacyDir := newLocalStorageWithTemplate(t, "shared-slug", true)
	local := stor.(*storage.LocalStorage)

	blobPath := storage.TemplateStoragePath("", "global", "", "shared-slug") + ".tmpl-blob"
	blobsDir := local.ObjectFSPath(blobPath + ".blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, "0000000000000000000000000000000000000000000000000000000000000000"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t)
	connFor := func(tmpl *hubclient.Template) *HubConnection {
		return &HubConnection{
			Name:         "local",
			IsColocated:  true,
			LocalStorage: stor,
			HubClient: &stubHubClient{templates: &stubTemplateService{
				getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) { return tmpl, nil },
			}},
		}
	}

	blobRow := &hubclient.Template{ID: "tmpl-blob", Slug: "shared-slug", Scope: "global", StoragePath: blobPath}
	path, err := srv.hydrateTemplate(context.Background(), &CreateAgentConfig{Template: "shared-slug", TemplateID: "tmpl-blob"}, connFor(blobRow))
	if err != nil {
		t.Fatalf("hydrateTemplate (blob row) failed: %v", err)
	}
	if path != "" {
		t.Errorf("blob-layout row must miss the direct read, got %q", path)
	}

	legacyRow := &hubclient.Template{ID: "tmpl-legacy", Slug: "shared-slug", Scope: "global"}
	path, err = srv.hydrateTemplate(context.Background(), &CreateAgentConfig{Template: "shared-slug", TemplateID: "tmpl-legacy"}, connFor(legacyRow))
	if err != nil {
		t.Fatalf("hydrateTemplate (legacy row) failed: %v", err)
	}
	if path != legacyDir {
		t.Errorf("legacy row should resolve directly to %q, got %q", legacyDir, path)
	}
}
