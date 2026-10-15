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
