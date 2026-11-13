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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// TestProvisionInProcessPluginBrokerSecret_ReusesSecretAcrossHubReplicas
// verifies that two Hub server startups sharing one backing store
// (multi-replica DeploymentModePlugin, ptone/scion#7625) provision the same
// active HMAC secret for the deterministic plugin UUIDv5 without revoking
// replica 1's key, while recreating a deleted plugin row clears orphaned
// credentials via CreateRuntimeBrokerClearingCredentials.
func TestProvisionInProcessPluginBrokerSecret_ReusesSecretAcrossHubReplicas(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	srv1 := flatAuthTestHub(t, s)
	hubTS1 := httptest.NewServer(srv1.Handler())
	t.Cleanup(hubTS1.Close)

	srv2 := flatAuthTestHub(t, s)
	hubTS2 := httptest.NewServer(srv2.Handler())
	t.Cleanup(hubTS2.Close)

	id1, secret1, err := provisionInProcessPluginBrokerSecret(ctx, s, srv1, "discord")
	require.NoError(t, err)
	require.NotEmpty(t, id1)
	require.NotEmpty(t, secret1)
	assert.Equal(t, http.StatusOK, hubSelfReadStatus(t, hubTS1.URL, id1, secret1))

	// Replica 2 starts against the same backing store: must reuse secret1 so
	// Replica 1's running plugin subprocess continues authenticating on both replicas.
	id2, secret2, err := provisionInProcessPluginBrokerSecret(ctx, s, srv2, "discord")
	require.NoError(t, err)
	assert.Equal(t, id1, id2)
	assert.Equal(t, secret1, secret2, "second Hub replica must reuse the active plugin secret rather than revoking replica 1")
	assert.Equal(t, http.StatusOK, hubSelfReadStatus(t, hubTS1.URL, id1, secret1), "replica 1 plugin key still authenticates against replica 1")
	assert.Equal(t, http.StatusOK, hubSelfReadStatus(t, hubTS2.URL, id1, secret1), "replica 1 plugin key authenticates against replica 2")

	// Deleting the plugin broker row and re-provisioning clears any orphaned
	// secret via CreateRuntimeBrokerClearingCredentials and mints a fresh key.
	require.NoError(t, s.DeleteRuntimeBroker(ctx, id1))
	id3, secret3, err := provisionInProcessPluginBrokerSecret(ctx, s, srv1, "discord")
	require.NoError(t, err)
	assert.Equal(t, id1, id3)
	assert.NotEqual(t, secret1, secret3, "recreating deleted plugin row clears orphaned secret and mints a fresh key")
	assert.Equal(t, http.StatusUnauthorized, hubSelfReadStatus(t, hubTS1.URL, id1, secret1))
	assert.Equal(t, http.StatusOK, hubSelfReadStatus(t, hubTS1.URL, id1, secret3))
}

// TestProvisionInProcessPluginBrokerSecret_RefusesPreRegisteredNonPluginRow
// verifies that a non-plugin RuntimeBroker row occupying the deterministic
// UUIDv5 (for example from a remote POST /api/v1/brokers registration before
// the plugin was enabled) is refused without returning or rotating its secret.
func TestProvisionInProcessPluginBrokerSecret_RefusesPreRegisteredNonPluginRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatAuthTestHub(t, s)
	hubTS := httptest.NewServer(srv.Handler())
	t.Cleanup(hubTS.Close)

	detID := uuid.NewSHA1(pluginBrokerUUIDNamespace, []byte("plugin-broker-slack")).String()
	reg, err := srv.GetBrokerAuthService().CreateBrokerRegistration(ctx, hub.CreateBrokerRegistrationRequest{
		BrokerID: detID,
		Name:     "pre-registered-remote",
	}, "test")
	require.NoError(t, err)
	joined, err := srv.GetBrokerAuthService().CompleteBrokerJoin(ctx, hub.BrokerJoinRequest{
		BrokerID:  detID,
		JoinToken: reg.JoinToken,
	}, hubTS.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, hubSelfReadStatus(t, hubTS.URL, detID, joined.SecretKey))

	gotID, gotSecret, err := provisionInProcessPluginBrokerSecret(ctx, s, srv, "slack")
	require.Error(t, err)
	assert.Empty(t, gotID)
	assert.Empty(t, gotSecret)
	assert.Equal(t, http.StatusOK, hubSelfReadStatus(t, hubTS.URL, detID, joined.SecretKey), "non-plugin row secret is untouched")
}
