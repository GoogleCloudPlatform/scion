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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tiaMarkDB opens a migrated file-backed SQLite database the way the mark
// command does and returns its config and client.
func tiaMarkDB(t *testing.T) (*config.GlobalConfig, *ent.Client) {
	t.Helper()
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite"
	cfg.Database.URL = filepath.Join(t.TempDir(), "hub.db")
	client, err := openTestInfraClient(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return cfg, client
}

func TestTestInfraMark_WritesMarkerAndAuditRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { testInfraMarkHubID, testInfraMarkAttested, testInfraMarkOperator = "", "", "" })
	cfg, client := tiaMarkDB(t)
	cfg.Hub.HubID = tiaCmdHubID // pinned, and equal to --hub-id
	ctx := context.Background()

	testInfraMarkHubID = tiaCmdHubID
	testInfraMarkAttested = `{"bucket":"synthetic-bucket","broker_ids":["b1"]}`
	testInfraMarkOperator = "operator@example.invalid"
	var out bytes.Buffer
	require.NoError(t, markTestInfra(ctx, client, cfg, &out))
	assert.Contains(t, out.String(), "hub_id="+tiaCmdHubID)

	m, err := testinfra.Read(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, tiaCmdHubID, m.HubID)
	assert.Equal(t, config.HubClassTest, m.Class)
	assert.False(t, m.CreatedAt.IsZero())
	assert.JSONEq(t, `{"bucket":"synthetic-bucket","broker_ids":["b1"]}`, string(m.Attested))

	audits, err := client.MutationAudit.Query().Where(mutationaudit.MutationType(testinfra.MarkMutationType)).All(ctx)
	require.NoError(t, err)
	require.Len(t, audits, 1, "one test_infra_mark audit row")
	assert.Equal(t, "operator", audits[0].ActorPrincipalKind)
	assert.Equal(t, "operator@example.invalid", audits[0].ActorPrincipalID)
	assert.Equal(t, testinfra.MarkerID, audits[0].TargetID)

	// A second mark is refused and writes nothing more.
	err = markTestInfra(ctx, client, cfg, &out)
	assert.ErrorIs(t, err, testinfra.ErrMarkerExists)
	n, err := client.MutationAudit.Query().Where(mutationaudit.MutationType(testinfra.MarkMutationType)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// isolateTestInfraMarkHubIDEnv clears every source of a hub id outside the
// config struct (the container may export SCION_* variables).
func isolateTestInfraMarkHubIDEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.HubIDEnvVar, "")
	t.Setenv("K_SERVICE", "")
	return home
}

// assertNoTestInfraMarkWritten checks that no marker row and no
// test_infra_mark audit row exist.
func assertNoTestInfraMarkWritten(t *testing.T, client *ent.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := testinfra.Read(ctx, client)
	assert.ErrorIs(t, err, testinfra.ErrMarkerAbsent, "a refused mark writes no marker row")
	n, err := client.MutationAudit.Query().Where(mutationaudit.MutationType(testinfra.MarkMutationType)).Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a refused mark writes no audit row")
}

// MK-h: mark refuses an unpinned (derived) hub id, names server.hub.hub_id,
// prints the CURRENT resolved id to pin, and writes nothing.
func TestTestInfraMark_RefusesUnpinnedHubID(t *testing.T) {
	t.Cleanup(func() { testInfraMarkHubID, testInfraMarkOperator = "", "" })
	testInfraMarkHubID, testInfraMarkOperator = tiaCmdHubID, "operator@example.invalid"
	ctx := context.Background()

	t.Run("no hub id file", func(t *testing.T) {
		home := isolateTestInfraMarkHubIDEnv(t)
		cfg, client := tiaMarkDB(t)
		var out bytes.Buffer
		err := markTestInfra(ctx, client, cfg, &out)
		require.ErrorIs(t, err, ErrTestInfraMarkHubIDNotPinned)
		assert.Contains(t, err.Error(), "server.hub.hub_id")
		assert.Contains(t, err.Error(), "GET /healthz")
		assert.Contains(t, err.Error(), "nothing was written")
		assertNoTestInfraMarkWritten(t, client)
		_, statErr := os.Stat(filepath.Join(home, config.GlobalDir, "hub-id"))
		assert.True(t, os.IsNotExist(statErr), "the check never persists a derived hub id")
	})

	t.Run("derived id from the hub id file", func(t *testing.T) {
		home := isolateTestInfraMarkHubIDEnv(t)
		require.NoError(t, os.MkdirAll(filepath.Join(home, config.GlobalDir), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, config.GlobalDir, "hub-id"), []byte(tiaCmdHubID+"\n"), 0o600))
		cfg, client := tiaMarkDB(t)
		var out bytes.Buffer
		err := markTestInfra(ctx, client, cfg, &out)
		require.ErrorIs(t, err, ErrTestInfraMarkHubIDNotPinned, "a derived id is refused even when it equals --hub-id")
		assert.Contains(t, err.Error(), "server.hub.hub_id")
		assert.Contains(t, err.Error(), `CURRENT resolved hub id ("`+tiaCmdHubID+`"; `)
		assert.Contains(t, err.Error(), "confirm it against the running hub's GET /healthz")
		assertNoTestInfraMarkWritten(t, client)
	})

	t.Run("hub id checked before attested", func(t *testing.T) {
		// Check-first order: an unpinned id is refused before the
		// (invalid) --attested value is parsed.
		isolateTestInfraMarkHubIDEnv(t)
		cfg, client := tiaMarkDB(t)
		t.Cleanup(func() { testInfraMarkAttested = "" })
		testInfraMarkAttested = "[]"
		var out bytes.Buffer
		err := markTestInfra(ctx, client, cfg, &out)
		require.ErrorIs(t, err, ErrTestInfraMarkHubIDNotPinned)
		assert.NotContains(t, err.Error(), "--attested")
		assertNoTestInfraMarkWritten(t, client)
	})

	t.Run("refused before the database is opened", func(t *testing.T) {
		isolateTestInfraMarkHubIDEnv(t)
		t.Chdir(t.TempDir())
		dbPath := filepath.Join(t.TempDir(), "hub.db")
		t.Cleanup(func() { testInfraMarkConfigPath, testInfraMarkDBURL = "", "" })
		testInfraMarkConfigPath, testInfraMarkDBURL = "", dbPath
		c := &cobra.Command{}
		c.SetContext(ctx)
		c.SetOut(&bytes.Buffer{})
		require.ErrorIs(t, runTestInfraMark(c, nil), ErrTestInfraMarkHubIDNotPinned)
		_, statErr := os.Stat(dbPath)
		assert.True(t, os.IsNotExist(statErr), "the database is neither created nor migrated")
	})
}

// MK-h: mark refuses a --hub-id that differs from the hub id resolved from
// the same configuration, and writes nothing; the env-var pin is accepted.
func TestTestInfraMark_RefusesHubIDMismatch(t *testing.T) {
	t.Cleanup(func() { testInfraMarkHubID, testInfraMarkOperator = "", "" })
	testInfraMarkOperator = "operator@example.invalid"
	ctx := context.Background()

	t.Run("pinned in server.hub.hub_id", func(t *testing.T) {
		isolateTestInfraMarkHubIDEnv(t)
		cfg, client := tiaMarkDB(t)
		cfg.Hub.HubID = tiaCmdHubID
		testInfraMarkHubID = "another-hub"
		var out bytes.Buffer
		err := markTestInfra(ctx, client, cfg, &out)
		require.ErrorIs(t, err, ErrTestInfraMarkHubIDMismatch)
		assert.Contains(t, err.Error(), `"another-hub"`)
		assert.Contains(t, err.Error(), `"`+tiaCmdHubID+`"`)
		assert.Contains(t, err.Error(), "nothing was written")
		assertNoTestInfraMarkWritten(t, client)
	})

	t.Run("pinned in SCION_SERVER_HUB_HUBID", func(t *testing.T) {
		isolateTestInfraMarkHubIDEnv(t)
		t.Setenv(config.HubIDEnvVar, tiaCmdHubID)
		cfg, client := tiaMarkDB(t)
		testInfraMarkHubID = "another-hub"
		var out bytes.Buffer
		require.ErrorIs(t, markTestInfra(ctx, client, cfg, &out), ErrTestInfraMarkHubIDMismatch)
		assertNoTestInfraMarkWritten(t, client)

		// The same env-var pin with a matching --hub-id is accepted.
		testInfraMarkHubID = tiaCmdHubID
		require.NoError(t, markTestInfra(ctx, client, cfg, &out))
		m, err := testinfra.Read(ctx, client)
		require.NoError(t, err)
		assert.Equal(t, tiaCmdHubID, m.HubID)
	})
}

// The gate reads the marker the offline command wrote.
func TestTestAdminGate_ReadsMarkerFromConfiguredStore(t *testing.T) {
	_, client := tiaMarkDB(t)
	ctx := context.Background()
	hubCfg := hub.ServerConfig{HubID: tiaCmdHubID, HubIDPinned: true}
	gateCfg := &config.GlobalConfig{}
	gateCfg.Hub.HubClass = config.HubClassTest

	t.Cleanup(func() { testModeLevel, testModeHubID, enableHub = "", "", false })
	testModeLevel, testModeHubID, enableHub = hub.TestModeAdmin, tiaCmdHubID, true

	// No marker yet.
	_, err := resolveTestMode(testModeInputsFor(ctx, gateCfg, hubCfg, true, client))
	assert.ErrorIs(t, err, hub.ErrTestAdminMarkerAbsent)

	// A marker for another hub.
	_, err = testinfra.Mark(ctx, client, testinfra.MarkInput{HubID: "another-hub", Operator: "op"})
	require.NoError(t, err)
	_, err = resolveTestMode(testModeInputsFor(ctx, gateCfg, hubCfg, true, client))
	assert.ErrorIs(t, err, hub.ErrTestAdminMarkerHubMismatch)

	// This hub's marker.
	_, client2 := tiaMarkDB(t)
	_, err = testinfra.Mark(ctx, client2, testinfra.MarkInput{HubID: tiaCmdHubID, Operator: "op"})
	require.NoError(t, err)
	res, err := resolveTestMode(testModeInputsFor(ctx, gateCfg, hubCfg, true, client2))
	require.NoError(t, err)
	assert.True(t, res.TestAdminGate.HubAdmin)
	assert.Equal(t, tiaCmdHubID, res.TestAdminGate.MarkerHubID)

	// The member level reads the same marker.
	testModeLevel = hub.TestModeMember
	_, err = resolveTestMode(testModeInputsFor(ctx, gateCfg, hubCfg, true, client))
	assert.ErrorIs(t, err, hub.ErrTestAdminMarkerHubMismatch)
	res, err = resolveTestMode(testModeInputsFor(ctx, gateCfg, hubCfg, true, client2))
	require.NoError(t, err)
	assert.True(t, res.EnableTestIdentities)
	assert.False(t, res.TestAdminGate.HubAdmin)

	// TM-h: the same hub with a derived (unpinned) id is refused before
	// the marker is read.
	hubCfg.HubIDPinned = false
	_, err = resolveTestMode(testModeInputsFor(ctx, gateCfg, hubCfg, true, client2))
	assert.ErrorIs(t, err, hub.ErrTestModeHubIDNotPinned)
}
