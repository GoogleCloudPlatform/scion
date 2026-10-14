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
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWebKeyTestClient(t *testing.T) *ent.Client {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	client, err := entc.OpenSQLite("file:"+name+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// DEC-8 (5): the startup read never fails startup and never yields a key
// for an unusable marker; each state is named.
func TestLoadTestIdentityWebSessionKey_FailsClosed(t *testing.T) {
	ctx := context.Background()

	k := loadTestIdentityWebSessionKey(ctx, nil, "hub-1", false)
	assert.Empty(t, k.Key)
	assert.Equal(t, "test_identities_disabled", k.Unavailable)

	k = loadTestIdentityWebSessionKey(ctx, nil, "hub-1", true)
	assert.Empty(t, k.Key)
	assert.Equal(t, "no_database", k.Unavailable)

	client := newWebKeyTestClient(t)
	k = loadTestIdentityWebSessionKey(ctx, client, "hub-1", true)
	assert.Empty(t, k.Key)
	assert.Equal(t, "marker_absent", k.Unavailable)

	_, err := testinfra.Mark(ctx, client, testinfra.MarkInput{HubID: "hub-1", Operator: "op"})
	require.NoError(t, err)
	k = loadTestIdentityWebSessionKey(ctx, client, "hub-2", true)
	assert.Empty(t, k.Key)
	assert.Equal(t, "marker_hub_mismatch", k.Unavailable)

	k = loadTestIdentityWebSessionKey(ctx, client, "hub-1", true)
	assert.Len(t, k.Key, testinfra.WebSessionKeyLen)
	assert.Empty(t, k.Unavailable)
	// The key never shows in a formatted config.
	assert.NotContains(t, k.String(), string(k.Key))
	assert.Equal(t, "TestIdentityWebSessionKey{present}", k.String())

	// Member level on but the key is off: never a key derived from a
	// session secret, whatever the environment holds.
	t.Setenv("SCION_SERVER_SESSION_SECRET", "synthetic-test-secret")
	other := newWebKeyTestClientNamed(t, "nokey")
	_, err = other.TestInfraMarker.Create().SetID(testinfra.MarkerID).SetHubID("hub-1").SetClass("test").SetCreatedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	k = loadTestIdentityWebSessionKey(ctx, other, "hub-1", true)
	assert.Empty(t, k.Key)
	assert.Equal(t, "key_missing", k.Unavailable)

	corrupt := newWebKeyTestClientNamed(t, "corrupt")
	_, err = corrupt.TestInfraMarker.Create().SetID(testinfra.MarkerID).SetHubID("hub-1").SetClass("test").SetCreatedAt(time.Now()).SetWebSessionKey([]byte("short")).Save(ctx)
	require.NoError(t, err)
	k = loadTestIdentityWebSessionKey(ctx, corrupt, "hub-1", true)
	assert.Empty(t, k.Key)
	assert.Equal(t, "key_invalid", k.Unavailable)

	closed := newWebKeyTestClientNamed(t, "closed")
	require.NoError(t, closed.Close())
	k = loadTestIdentityWebSessionKey(ctx, closed, "hub-1", true)
	assert.Empty(t, k.Key)
	assert.Equal(t, "marker_unreadable", k.Unavailable)
}

func newWebKeyTestClientNamed(t *testing.T, suffix string) *ent.Client {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_") + "_" + suffix
	client, err := entc.OpenSQLite("file:"+name+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// The mark command output never contains the key; unmark refuses another
// hub's marker and destroys the key for its own.
func TestTestInfraMarkUnmark_KeyLifecycle(t *testing.T) {
	ctx := context.Background()
	client := newWebKeyTestClient(t)
	t.Cleanup(func() {
		testInfraMarkHubID, testInfraMarkAcknowledge = "", false
		testInfraUnmarkHubID, testInfraUnmarkAcknowledge = "", false
	})

	testInfraMarkHubID, testInfraMarkAcknowledge = "hub-1", true
	var out bytes.Buffer
	require.NoError(t, markTestInfra(ctx, client, &config.GlobalConfig{}, &out))
	key, err := testinfra.ReadWebSessionKey(ctx, client, "hub-1")
	require.NoError(t, err)
	assert.Contains(t, out.String(), "web session key was generated")
	assert.NotContains(t, out.String(), string(key))

	testInfraUnmarkHubID = "hub-other"
	out.Reset()
	require.ErrorContains(t, unmarkTestInfra(ctx, client, &out), "nothing was changed")
	_, err = testinfra.ReadWebSessionKey(ctx, client, "hub-1")
	require.NoError(t, err, "a refused unmark keeps the marker")

	testInfraUnmarkHubID = "hub-1"
	require.NoError(t, unmarkTestInfra(ctx, client, &out))
	assert.Contains(t, out.String(), "web session key is destroyed")
	_, err = testinfra.ReadWebSessionKey(ctx, client, "hub-1")
	assert.ErrorIs(t, err, testinfra.ErrMarkerAbsent)
}

func TestTestInfraUnmark_HumanCLIOnlyAndArgs(t *testing.T) {
	assert.False(t, agentAllowed["server.test-infra.unmark"], "server test-infra unmark must not be allowed in agent mode")
	t.Cleanup(func() { testInfraUnmarkHubID, testInfraUnmarkAcknowledge = "", false })

	testInfraUnmarkHubID, testInfraUnmarkAcknowledge = "", false
	assert.ErrorContains(t, testInfraUnmarkArgs(nil, nil), "--hub-id is required")
	testInfraUnmarkHubID = "hub-1"
	assert.ErrorContains(t, testInfraUnmarkArgs(nil, nil), testInfraMarkAcknowledgeFlag)
	testInfraUnmarkAcknowledge = true
	assert.NoError(t, testInfraUnmarkArgs(nil, nil))
	assert.ErrorContains(t, testInfraUnmarkArgs(nil, []string{"x"}), "unexpected arguments")

	var found *cobra.Command
	for _, c := range serverTestInfraCmd.Commands() {
		if c.Name() == "unmark" {
			found = c
		}
	}
	require.NotNil(t, found, "server test-infra unmark is registered")
}

// Unmark through the guarded open path (openTestInfraDatabase →
// openTestInfraClient → migrateHubStore, the path runTestInfraUnmark uses)
// destroys the hub-local web session key that mark generated, and the
// startup read then reports web sessions unavailable.
func TestTestInfraUnmark_GuardedOpenDestroysKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	t.Cleanup(func() {
		testInfraMarkHubID, testInfraMarkAcknowledge = "", false
		testInfraUnmarkHubID, testInfraUnmarkAcknowledge = "", false
	})

	// Mark through the same guarded opener the mark command uses.
	cfg, markClient := tiaMarkDB(t)
	testInfraMarkHubID, testInfraMarkAcknowledge = tiaCmdHubID, true
	var out bytes.Buffer
	require.NoError(t, markTestInfra(ctx, markClient, cfg, &out))
	before := loadTestIdentityWebSessionKey(ctx, markClient, tiaCmdHubID, true)
	require.Len(t, before.Key, testinfra.WebSessionKeyLen, "mark generated the key")

	// Unmark through openTestInfraDatabase, as runTestInfraUnmark does.
	_, client, err := openTestInfraDatabase(ctx, "", cfg.Database.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	testInfraUnmarkHubID, testInfraUnmarkAcknowledge = tiaCmdHubID, true
	out.Reset()
	require.NoError(t, unmarkTestInfra(ctx, client, &out))

	_, err = testinfra.ReadWebSessionKey(ctx, client, tiaCmdHubID)
	assert.ErrorIs(t, err, testinfra.ErrMarkerAbsent, "the key is gone with the marker")
	after := loadTestIdentityWebSessionKey(ctx, client, tiaCmdHubID, true)
	assert.Empty(t, after.Key)
	assert.Equal(t, "marker_absent", after.Unavailable, "web sessions report unavailable")
	assert.Equal(t, "TestIdentityWebSessionKey{unavailable: \"marker_absent\"}", after.String())
}
