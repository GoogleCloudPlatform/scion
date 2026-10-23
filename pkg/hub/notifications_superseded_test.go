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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A managed notifications row that differs from the bootstrap channels is
// reported under the full koanf path server.notification_channels, which
// the admin UI labels and badges, not under the bare section key
// (ptone/scion#4167).
func TestGetServerConfigDB_SupersededNotificationChannelsUsesKoanfPath(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.notification_channels": []interface{}{
			map[string]interface{}{"type": "slack", "params": map[string]interface{}{"channel": "#file"}},
		},
	})
	ops := NewOperationalSettings(fakeStore, bootstrapK, emptyKoanf())
	fakeStore.seedWithOrigin("notifications",
		json.RawMessage(`{"notification_channels":[{"type":"slack","params":{"channel":"#db"}}]}`), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	resp := getServerConfigDB(t, srv, ops)
	sup := resp.SupersededKeys["notifications"]
	require.Len(t, sup, 1, "%+v", resp.SupersededKeys)
	assert.Equal(t, "server.notification_channels", sup[0].Key)
	assert.Equal(t, "yaml", sup[0].Source)
}

// A managed notifications row equal to the bootstrap channels supersedes
// nothing (ptone/scion#4167).
func TestGetServerConfigDB_NotificationChannelsEqualToBootstrapNotSuperseded(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.notification_channels": []interface{}{
			map[string]interface{}{"type": "slack", "params": map[string]interface{}{"channel": "#same"}},
		},
	})
	ops := NewOperationalSettings(fakeStore, bootstrapK, emptyKoanf())
	fakeStore.seedWithOrigin("notifications",
		json.RawMessage(`{"notification_channels":[{"type":"slack","params":{"channel":"#same"}}]}`), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	resp := getServerConfigDB(t, srv, ops)
	assert.Empty(t, resp.SupersededKeys["notifications"])
}
