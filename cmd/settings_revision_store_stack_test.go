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

package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store the hub server runs operational settings on (initStore, as
// passed to initOperationalSettings) implements the optional
// revision-returning delete, so a settings delete on a real hub carries its
// own revision rather than falling back to revision 0 (ptone/scion#4457).
func TestInitStore_ImplementsHubSettingRevisionDeleter(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite"
	cfg.Database.URL = filepath.Join(t.TempDir(), "hub.db")
	s, entClient, err := initStore(context.Background(), cfg)
	require.NoError(t, err, "initStore")
	t.Cleanup(func() { _ = entClient.Close() })

	settingStore, ok := s.(store.HubSettingStore)
	require.True(t, ok, "the hub store implements HubSettingStore")
	rd, ok := settingStore.(store.HubSettingRevisionDeleter)
	require.True(t, ok, "the hub store implements the optional HubSettingRevisionDeleter")

	ctx := context.Background()
	up, err := settingStore.UpsertHubSetting(ctx, "quotas", json.RawMessage(`{}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	rev, err := rd.DeleteHubSettingRevision(ctx, "quotas")
	require.NoError(t, err)
	assert.Greater(t, rev, up.SettingsRevision, "the delete returns the revision it bumped to")
}
