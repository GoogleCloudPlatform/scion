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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Harness-config content split and compare-and-swap (ptone/scion#4219).

func newCASHarnessConfig(t *testing.T, ts *TemplateStore) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:      uuid.New().String(),
		Name:    "cas",
		Slug:    "cas-" + uuid.New().String()[:8],
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
		Status:  store.HarnessConfigStatusPending,
	}
	require.NoError(t, ts.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// TestUpdateHarnessConfigContent_CompareAndSwap: two writers that read the
// same row cannot both commit; the loser gets ErrHarnessConfigConflict (an
// ErrVersionConflict), the winner's content stays, and a missing row is
// ErrNotFound.
func TestUpdateHarnessConfigContent_CompareAndSwap(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()
	hc := newCASHarnessConfig(t, ts)

	first := *hc
	first.Files = []store.TemplateFile{{Path: "config.yaml", Size: 1, Hash: "sha256:a"}}
	first.ContentHash = "sha256:h1"
	first.Status = store.HarnessConfigStatusActive
	require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &first, store.HarnessConfigContentPrecondition{}))

	second := *hc
	second.Files = []store.TemplateFile{{Path: "config.yaml", Size: 1, Hash: "sha256:b"}}
	second.ContentHash = "sha256:h2"
	err := ts.UpdateHarnessConfigContent(ctx, &second, store.HarnessConfigContentPrecondition{})
	assert.ErrorIs(t, err, store.ErrHarnessConfigConflict)
	assert.ErrorIs(t, err, store.ErrVersionConflict)

	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "sha256:h1", got.ContentHash)
	assert.Equal(t, store.HarnessConfigStatusActive, got.Status)

	// A writer holding the current hash wins.
	third := *got
	third.ContentHash = "sha256:h3"
	require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &third, store.HarnessConfigContentPrecondition{ContentHash: "sha256:h1"}))

	ghost := *hc
	ghost.ID = uuid.New().String()
	assert.ErrorIs(t, ts.UpdateHarnessConfigContent(ctx, &ghost, store.HarnessConfigContentPrecondition{}), store.ErrNotFound)
}

// TestHarnessConfigContentAndMetadata_DoNotRevertEachOther: the content
// write leaves metadata and built_image alone, and the metadata write
// leaves content alone, so a write from a stale read of either kind does
// not revert the other.
func TestHarnessConfigContentAndMetadata_DoNotRevertEachOther(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()
	hc := newCASHarnessConfig(t, ts)
	stale := *hc

	require.NoError(t, ts.UpdateHarnessConfigBuiltImage(ctx, hc.ID, "registry/built:1"))

	meta := *hc
	meta.Name = "renamed"
	meta.Description = "desc"
	require.NoError(t, ts.UpdateHarnessConfig(ctx, &meta))

	content := stale // stale read: old name, no built image
	content.Files = []store.TemplateFile{{Path: "config.yaml", Size: 1, Hash: "sha256:a"}}
	content.ContentHash = "sha256:h1"
	content.Harness = "gemini"
	content.Status = store.HarnessConfigStatusActive
	content.Config = &store.HarnessConfigData{Harness: "gemini", Image: "img:1"}
	require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &content, store.HarnessConfigContentPrecondition{}))

	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name, "content write must not revert the metadata write")
	assert.Equal(t, "desc", got.Description)
	assert.Equal(t, "registry/built:1", got.BuiltImage, "content write must not touch built_image")
	assert.Equal(t, "gemini", got.Harness)
	require.NotNil(t, got.Config)
	assert.Equal(t, "img:1", got.Config.Image)

	// A metadata write from the stale read leaves the content alone.
	staleMeta := stale
	staleMeta.Name = "renamed-again"
	require.NoError(t, ts.UpdateHarnessConfig(ctx, &staleMeta))
	got, err = ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed-again", got.Name)
	assert.Equal(t, "sha256:h1", got.ContentHash)
	assert.Equal(t, store.HarnessConfigStatusActive, got.Status)
	assert.Equal(t, "gemini", got.Harness)
	require.NotNil(t, got.Config)
	assert.Equal(t, "img:1", got.Config.Image)
	assert.Len(t, got.Files, 1)
	assert.Equal(t, "registry/built:1", got.BuiltImage)
}

// TestHarnessConfigMetadataUpdate_KeepsSourceURLAndImageStatus: a metadata
// write from a stale read does not revert a concurrent commit's source URL
// or a concurrent image check's status and check time (ptone/scion#4967).
func TestHarnessConfigMetadataUpdate_KeepsSourceURLAndImageStatus(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()
	hc := newCASHarnessConfig(t, ts)
	stale := *hc // stale read: no source URL, no image status

	content := *hc
	content.SourceURL = "https://example.com/source/v2"
	content.ContentHash = "sha256:h1"
	content.Status = store.HarnessConfigStatusActive
	require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &content, store.HarnessConfigContentPrecondition{}))

	checkedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, ts.UpdateHarnessConfigImageStatus(ctx, hc.ID, store.HarnessConfigImageStatusValid, checkedAt))

	stale.Name = "renamed"
	require.NoError(t, ts.UpdateHarnessConfig(ctx, &stale))

	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "https://example.com/source/v2", got.SourceURL, "metadata write must not revert the commit's source URL")
	assert.Equal(t, store.HarnessConfigImageStatusValid, got.ImageStatus, "metadata write must not revert the image status")
	require.NotNil(t, got.ImageStatusCheckedAt, "metadata write must not clear the image check time")
	assert.WithinDuration(t, checkedAt, *got.ImageStatusCheckedAt, time.Second)
}

// TestHarnessConfigBuiltImage_RoundTrip: built_image is stored by create,
// read back, written only by the narrow writer, which keeps Updated.
func TestHarnessConfigBuiltImage_RoundTrip(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID: uuid.New().String(), Name: "b", Slug: "b-" + uuid.New().String()[:8], Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, BuiltImage: "registry/created:1",
	}
	require.NoError(t, ts.CreateHarnessConfig(ctx, hc))
	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "registry/created:1", got.BuiltImage)
	before := got.Updated

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, ts.UpdateHarnessConfigBuiltImage(ctx, hc.ID, "registry/built:2"))
	got, err = ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "registry/built:2", got.BuiltImage)
	assert.True(t, got.Updated.Equal(before), "the narrow writer keeps Updated")

	list, err := ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{})
	require.NoError(t, err)
	found := false
	for _, item := range list.Items {
		if item.ID == hc.ID {
			found = true
			assert.Equal(t, "registry/built:2", item.BuiltImage)
		}
	}
	assert.True(t, found)

	assert.ErrorIs(t, ts.UpdateHarnessConfigBuiltImage(ctx, uuid.New().String(), "x"), store.ErrNotFound)
}

// TestHarnessConfigLayout_RoundTrip: Layout and the storage columns are
// written by CreateHarnessConfig and UpdateHarnessConfigContent and read
// back, a metadata UpdateHarnessConfig from a stale read leaves them alone,
// and the StoragePath / StoragePathPrefix filters find the rows on (or
// nested under) a path (ptone/scion#4219).
func TestHarnessConfigLayout_RoundTrip(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID: uuid.New().String(), Name: "layout", Slug: "layout", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
		Status: store.HarnessConfigStatusActive, ContentHash: "sha256:h0", StoragePath: "harness-configs/global/layout",
	}
	require.NoError(t, ts.CreateHarnessConfig(ctx, hc))
	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.Layout, "a row created without a layout is legacy")

	stale := *got

	migrated := *got
	migrated.Layout = store.HarnessConfigLayoutBlobs
	migrated.StoragePath = "harness-configs/global/layout." + hc.ID
	migrated.StorageURI = "gs://b/harness-configs/global/layout." + hc.ID + "/"
	require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &migrated, store.HarnessConfigContentPrecondition{
		ContentHash: "sha256:h0", StoragePath: "harness-configs/global/layout",
	}))

	got, err = ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, store.HarnessConfigLayoutBlobs, got.Layout)
	assert.Equal(t, migrated.StoragePath, got.StoragePath)
	assert.Equal(t, migrated.StorageURI, got.StorageURI)

	// A metadata PUT/PATCH written from the pre-migration read keeps the
	// migrated layout and storage path.
	stale.Name = "layout-renamed"
	require.NoError(t, ts.UpdateHarnessConfig(ctx, &stale))
	got, err = ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "layout-renamed", got.Name)
	assert.Equal(t, store.HarnessConfigLayoutBlobs, got.Layout)
	assert.Equal(t, migrated.StoragePath, got.StoragePath)
	assert.Equal(t, migrated.StorageURI, got.StorageURI)

	blob := &store.HarnessConfig{
		ID: uuid.New().String(), Name: "born-blob", Slug: "born-blob", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
		Status: store.HarnessConfigStatusPending, Layout: store.HarnessConfigLayoutBlobs, StoragePath: "harness-configs/global/born-blob.x",
	}
	require.NoError(t, ts.CreateHarnessConfig(ctx, blob))
	got, err = ts.GetHarnessConfig(ctx, blob.ID)
	require.NoError(t, err)
	assert.Equal(t, store.HarnessConfigLayoutBlobs, got.Layout)

	list, err := ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{StoragePath: migrated.StoragePath}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, hc.ID, list.Items[0].ID)

	list, err = ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{StoragePathPrefix: "harness-configs/global/"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list.Items, 2)
	list, err = ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{StoragePathPrefix: "harness-configs/global/layout/"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
}

// TestUpdateHarnessConfigContent_LayoutAndPathInPredicate: the ABA case
// across layouts and storage paths at the store (ptone/scion#4219). A commit
// that read the legacy row loses to a concurrent migration that kept the
// same content hash, whether the migration changed the layout (a commit
// moving the row to blobs) or only the storage path (the storage
// migration), and a commit that read the current row wins.
func TestUpdateHarnessConfigContent_LayoutAndPathInPredicate(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	newLegacy := func(slug string) *store.HarnessConfig {
		hc := &store.HarnessConfig{
			ID: uuid.New().String(), Name: slug, Slug: slug, Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
			Status: store.HarnessConfigStatusActive, ContentHash: "sha256:h1", StoragePath: "harness-configs/global/" + slug,
			Files: []store.TemplateFile{{Path: "config.yaml", Size: 1, Hash: "sha256:a"}},
		}
		require.NoError(t, ts.CreateHarnessConfig(ctx, hc))
		got, err := ts.GetHarnessConfig(ctx, hc.ID)
		require.NoError(t, err)
		return got
	}
	readPre := func(hc *store.HarnessConfig) store.HarnessConfigContentPrecondition {
		return store.HarnessConfigContentPrecondition{ContentHash: hc.ContentHash, Layout: hc.Layout, StoragePath: hc.StoragePath}
	}

	t.Run("layout", func(t *testing.T) {
		legacyRead := newLegacy("aba-layout")
		migrated := *legacyRead
		migrated.Layout = store.HarnessConfigLayoutBlobs
		migrated.StoragePath = legacyRead.StoragePath + "." + legacyRead.ID
		require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &migrated, readPre(legacyRead)))

		stale := *legacyRead
		stale.SourceURL = "stale writer"
		assert.ErrorIs(t, ts.UpdateHarnessConfigContent(ctx, &stale, readPre(legacyRead)), store.ErrHarnessConfigConflict)

		got, err := ts.GetHarnessConfig(ctx, legacyRead.ID)
		require.NoError(t, err)
		assert.Equal(t, store.HarnessConfigLayoutBlobs, got.Layout, "the losing write changed nothing")
		assert.Equal(t, migrated.StoragePath, got.StoragePath)
		assert.Empty(t, got.SourceURL)

		fresh := *got
		fresh.SourceURL = "fresh writer"
		require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &fresh, readPre(got)))
	})

	t.Run("layout only", func(t *testing.T) {
		// Each column of the predicate is checked on its own: here the
		// stored row changed only its layout (mutant "CAS without layout").
		legacyRead := newLegacy("aba-layout-only")
		moved := *legacyRead
		moved.Layout = store.HarnessConfigLayoutBlobs
		require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &moved, readPre(legacyRead)))

		stale := *legacyRead
		stale.SourceURL = "stale writer"
		assert.ErrorIs(t, ts.UpdateHarnessConfigContent(ctx, &stale, readPre(legacyRead)), store.ErrHarnessConfigConflict)

		got, err := ts.GetHarnessConfig(ctx, legacyRead.ID)
		require.NoError(t, err)
		assert.Equal(t, store.HarnessConfigLayoutBlobs, got.Layout, "the legacy layout was not written back")
		assert.Empty(t, got.SourceURL)
	})

	t.Run("storage path only", func(t *testing.T) {
		legacyRead := newLegacy("aba-path")
		moved := *legacyRead
		moved.StoragePath = "hubs/h1/" + legacyRead.StoragePath
		require.NoError(t, ts.UpdateHarnessConfigContent(ctx, &moved, readPre(legacyRead)))

		// Same hash, same (legacy) layout: only the storage path differs.
		stale := *legacyRead
		stale.SourceURL = "stale writer"
		assert.ErrorIs(t, ts.UpdateHarnessConfigContent(ctx, &stale, readPre(legacyRead)), store.ErrHarnessConfigConflict)

		got, err := ts.GetHarnessConfig(ctx, legacyRead.ID)
		require.NoError(t, err)
		assert.Equal(t, moved.StoragePath, got.StoragePath, "the old path was not written back")
		assert.Empty(t, got.SourceURL)

		require.NoError(t, ts.UpdateHarnessConfigContent(ctx, got, readPre(got)))
	})
}
