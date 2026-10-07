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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func artifactsOps(t *testing.T, raw string) *OperationalSettings {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	if raw != "" {
		fakeStore.seed("artifacts", json.RawMessage(raw))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	return ops
}

func TestArtifactsSettings_AbsentRowUsesDefaults(t *testing.T) {
	assert.Equal(t, opsettings.DefaultArtifactsConfig(), artifactsOps(t, "").Artifacts())
}

func TestArtifactsSettings_ExplicitValues(t *testing.T) {
	got := artifactsOps(t, `{"enabled":false,"max_files":10,"remote_image_max_count":10,"link_default_ttl_hours":24}`).Artifacts()
	want := opsettings.DefaultArtifactsConfig()
	want.Enabled = false
	want.MaxFiles = 10
	want.RemoteImageMaxCount = 10
	want.LinkDefaultTTLHours = 24
	assert.Equal(t, want, got)
}

// Both an unreadable row and a readable row with an invalid value fail
// closed: the service is disabled even when the row says enabled.
func TestArtifactsSettings_MalformedFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid JSON":           `not valid json`,
		"wrong type":             `{"max_files":"many"}`,
		"invalid value":          `{"enabled":true,"max_file_bytes":0}`,
		"file above bundle":      `{"enabled":true,"max_file_bytes":2048,"max_bundle_bytes":1024}`,
		"default TTL above max":  `{"enabled":true,"link_default_ttl_hours":721}`,
		"negative retention day": `{"enabled":true,"default_retention_days":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := artifactsOps(t, raw).Artifacts()
			assert.Equal(t, opsettings.MalformedArtifactsConfig(), got)
			assert.False(t, got.Enabled)
		})
	}
}

func TestArtifactsSettings_HotReload(t *testing.T) {
	ops := artifactsOps(t, "")
	require.True(t, ops.Artifacts().Enabled)
	_, err := ops.Update(context.Background(), "artifacts", []byte(`{"enabled":false}`), "test", 0, "managed")
	require.NoError(t, err)
	assert.False(t, ops.Artifacts().Enabled, "Update must take effect without a restart")
}

// TestArtifactLimitsCarryRemoteImageSettings: the remote image settings
// reach the artifact service's limits, in seconds converted to durations.
func TestArtifactLimitsCarryRemoteImageSettings(t *testing.T) {
	ops := artifactsOps(t, `{"remote_images_enabled":false,"remote_image_max_count":4,"remote_image_max_bytes":2048,"remote_image_fetch_timeout_s":3,"remote_image_total_budget_s":9}`)
	srv := &Server{}
	srv.SetOperationalSettings(ops)
	l := srv.artifactLimits(context.Background())
	assert.Equal(t, artifacts.RemoteImageLimits{
		Enabled: false, MaxCount: 4, MaxBytes: 2048,
		FetchTimeout: 3 * time.Second, TotalBudget: 9 * time.Second,
	}, l.RemoteImages)

	def := (&Server{}).artifactLimits(context.Background()).RemoteImages
	assert.Equal(t, artifacts.RemoteImageLimits{
		Enabled: true, MaxCount: 32, MaxBytes: 5 << 20,
		FetchTimeout: 10 * time.Second, TotalBudget: 30 * time.Second,
	}, def)
}
