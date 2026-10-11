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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
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

// The gate reads the marker the offline command wrote.
func TestTestAdminGate_ReadsMarkerFromConfiguredStore(t *testing.T) {
	_, client := tiaMarkDB(t)
	ctx := context.Background()
	hubCfg := hub.ServerConfig{HubID: tiaCmdHubID, EnableTestIdentities: true}
	gateCfg := &config.GlobalConfig{}
	gateCfg.Hub.HubClass = config.HubClassTest

	t.Cleanup(func() { testAdminFlagSet, enableTestAdminIdentities, enableHub = false, "", false })
	testAdminFlagSet, enableTestAdminIdentities, enableHub = true, tiaCmdHubID, true

	// No marker yet.
	_, err := validateTestAdminIdentities(testAdminGateInputsFor(ctx, gateCfg, hubCfg, true, client))
	assert.ErrorIs(t, err, hub.ErrTestAdminMarkerAbsent)

	// A marker for another hub.
	_, err = testinfra.Mark(ctx, client, testinfra.MarkInput{HubID: "another-hub", Operator: "op"})
	require.NoError(t, err)
	_, err = validateTestAdminIdentities(testAdminGateInputsFor(ctx, gateCfg, hubCfg, true, client))
	assert.ErrorIs(t, err, hub.ErrTestAdminMarkerHubMismatch)

	// This hub's marker.
	_, client2 := tiaMarkDB(t)
	_, err = testinfra.Mark(ctx, client2, testinfra.MarkInput{HubID: tiaCmdHubID, Operator: "op"})
	require.NoError(t, err)
	gate, err := validateTestAdminIdentities(testAdminGateInputsFor(ctx, gateCfg, hubCfg, true, client2))
	require.NoError(t, err)
	assert.True(t, gate.HubAdmin)
	assert.Equal(t, tiaCmdHubID, gate.MarkerHubID)
}
