// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyBrokerCredentials(t *testing.T) {
	dir := t.TempDir()
	multiStore := brokercredentials.NewMultiStore(filepath.Join(dir, "hub-credentials"))
	legacyStore := brokercredentials.NewStore(filepath.Join(dir, "broker-credentials.json"))
	require.NoError(t, legacyStore.Save(&brokercredentials.BrokerCredentials{
		BrokerID:    "broker-1",
		SecretKey:   base64.StdEncoding.EncodeToString([]byte("secret")),
		HubEndpoint: "https://hub.example.com",
	}))

	migrated, err := migrateLegacyBrokerCredentials(multiStore, legacyStore)
	require.NoError(t, err)
	require.True(t, migrated)
	require.False(t, legacyStore.Exists())
	require.True(t, multiStore.Exists(brokercredentials.DeriveHubName("https://hub.example.com")))

	migrated, err = migrateLegacyBrokerCredentials(multiStore, legacyStore)
	require.NoError(t, err)
	require.False(t, migrated)
}
