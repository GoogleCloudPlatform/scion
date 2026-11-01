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

package hub

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A DB-backed snapshot hands the Kubernetes placement findings of the
// stored runtimes and profiles to the co-located Runtime Broker's settings
// overlay (populateMapSections, then ApplySnapshot): an invalid placement
// field and a runtimes document that does not decode are both recorded, so
// Kubernetes starts are refused, and valid sections clear them. The
// undecodable document leaves every map of the snapshot nil, and its
// finding is recorded all the same.
func TestApplySnapshotRecordsPlacementFindings(t *testing.T) {
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	overlay := config.NewSettingsOverlay()
	config.SetGlobalSettingsOverlay(overlay)

	ctx := context.Background()
	fake := newFakeHubSettingStore()
	ops := NewOperationalSettings(fake, emptyKoanf(), emptyKoanf())
	// The production shape: the server holds its operational settings, so
	// ApplySnapshot also takes its image_registry feed, which activates the
	// overlay as on a real co-located Hub.
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	apply := func(runtimes string) []string {
		t.Helper()
		_, err := fake.UpsertHubSetting(ctx, "runtimes", json.RawMessage(runtimes), "test", -1, "managed")
		require.NoError(t, err)
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		ApplySnapshot(srv, ops.Snapshot())
		return overlay.PlacementErrors()
	}

	got := apply(`{"gke":{"type":"kubernetes","namespace":"agents","runtime_class_name":"Not_Valid"}}`)
	require.True(t, slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "runtimes.gke.") }),
		"invalid placement field: findings %v", got)

	got = apply(`{"gke":{"type":"kubernetes","namespace":"agents","runtime_class_name":"gvisor"}}`)
	require.Empty(t, got, "valid runtimes: findings %v", got)

	got = apply(`{"gke":"not an entry"}`)
	require.Equal(t, []string{"runtimes"}, got, "undecodable runtimes document")

	got = apply(`{"gke":{"type":"kubernetes","namespace":"agents"}}`)
	require.Empty(t, got, "valid runtimes again: findings %v", got)
}
