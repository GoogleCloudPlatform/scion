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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Offline member-tier delegations (ptone/scion#4240, phase DL).

const dlCmdSub = "100000000000000000081"

// dlCmdSetup returns a pinned, class-test configuration with dlCmdSub on
// the workload issuer allowlist, its database, and a marker for
// tiaCmdHubID; it resets the delegate flags.
func dlCmdSetup(t *testing.T, mark bool) (*config.GlobalConfig, *ent.Client) {
	t.Helper()
	isolateTestInfraMarkHubIDEnv(t)
	cfg, client := tiaMarkDB(t)
	cfg.Hub.HubID = tiaCmdHubID
	cfg.Hub.HubClass = config.HubClassTest
	cfg.Hub.TestIdentities = &config.TestIdentitiesConfig{
		WorkloadIssuers: []config.TestIdentityWorkloadIssuer{{Sub: dlCmdSub, MaxTier: config.WorkloadIssuerTierMember}},
	}
	if mark {
		_, err := testinfra.Mark(context.Background(), client, testinfra.MarkInput{HubID: tiaCmdHubID, Operator: "op", Host: "h", Now: time.Now()})
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		testInfraDelegateHubID, testInfraDelegateWorkloadSub, testInfraDelegateLabel, testInfraDelegateOperator = "", "", "", ""
		testInfraDelegateTTL, testInfraDelegateMemberCap = store.TestInfraOfflineDelegationDefaultTTL, store.TestInfraOfflineDelegationMaxMemberCap
		testInfraDelegateRevokeHubID, testInfraDelegateRevokeID, testInfraDelegateRevokeOperator = "", "", ""
	})
	testInfraDelegateHubID, testInfraDelegateWorkloadSub, testInfraDelegateOperator = tiaCmdHubID, dlCmdSub, "operator@example.invalid"
	testInfraDelegateTTL, testInfraDelegateMemberCap = store.TestInfraOfflineDelegationDefaultTTL, store.TestInfraOfflineDelegationMaxMemberCap
	return cfg, client
}

func dlCmdCounts(t *testing.T, client *ent.Client) (rows, audits int) {
	t.Helper()
	ctx := context.Background()
	rows, err := client.TestIdentityDelegation.Query().Count(ctx)
	require.NoError(t, err)
	audits, err = client.MutationAudit.Query().Where(mutationaudit.MutationType(testinfra.DelegateMutationType)).Count(ctx)
	require.NoError(t, err)
	return rows, audits
}

func TestTestInfraDelegate_CreatesAndPrintsNoToken(t *testing.T) {
	cfg, client := dlCmdSetup(t, true)
	var out bytes.Buffer
	require.NoError(t, delegateTestInfra(context.Background(), client, cfg, &out))
	rows, audits := dlCmdCounts(t, client)
	assert.Equal(t, 1, rows)
	assert.Equal(t, 1, audits)
	s := out.String()
	assert.Contains(t, s, "Offline issuer delegation created: id=")
	assert.Contains(t, s, "hub_id="+tiaCmdHubID)
	assert.Contains(t, s, "scope="+store.TestInfraOfflineDelegationScope)
	assert.NotContains(t, s, "token=")
	assert.NotContains(t, s, "Bearer")
}

func TestTestInfraDelegate_Refusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mark   bool
		mutate func(cfg *config.GlobalConfig)
		want   error
	}{
		{"unpinned hub id", true, func(cfg *config.GlobalConfig) { cfg.Hub.HubID = "" }, ErrTestInfraMarkHubIDNotPinned},
		{"hub id mismatch", true, func(*config.GlobalConfig) { testInfraDelegateHubID = "another-hub" }, ErrTestInfraMarkHubIDMismatch},
		{"hub class not test", true, func(cfg *config.GlobalConfig) { cfg.Hub.HubClass = "" }, ErrTestInfraDelegateHubClass},
		{"no marker", false, func(*config.GlobalConfig) {}, testinfra.ErrMarkerAbsent},
		{"off the allowlist", true, func(*config.GlobalConfig) { testInfraDelegateWorkloadSub = "100000000000000000099" }, testinfra.ErrDelegateNotAllowlisted},
		{"no allowlist", true, func(cfg *config.GlobalConfig) { cfg.Hub.TestIdentities = nil }, testinfra.ErrDelegateNotAllowlisted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, client := dlCmdSetup(t, tc.mark)
			tc.mutate(cfg)
			var out bytes.Buffer
			err := delegateTestInfra(ctx, client, cfg, &out)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), "nothing was written")
			rows, audits := dlCmdCounts(t, client)
			assert.Zero(t, rows)
			assert.Zero(t, audits)
		})
	}

	t.Run("marker for another hub", func(t *testing.T) {
		cfg, client := dlCmdSetup(t, false)
		_, err := testinfra.Mark(ctx, client, testinfra.MarkInput{HubID: "another-hub", Operator: "op", Now: time.Now()})
		require.NoError(t, err)
		var out bytes.Buffer
		require.ErrorIs(t, delegateTestInfra(ctx, client, cfg, &out), testinfra.ErrDelegateMarkerHubMismatch)
		rows, _ := dlCmdCounts(t, client)
		assert.Zero(t, rows)
	})
}

// The flag checks run before anything is opened. There is no flag that
// selects a scope: the command can only create a member-tier delegation.
func TestTestInfraDelegateArgs(t *testing.T) {
	dlCmdSetup(t, false)
	require.NoError(t, testInfraDelegateArgs(nil, nil))
	assert.Error(t, testInfraDelegateArgs(nil, []string{"extra"}))

	testInfraDelegateTTL = store.TestInfraOfflineDelegationMaxTTL + time.Minute
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--ttl")
	testInfraDelegateTTL = store.TestInfraOfflineDelegationDefaultTTL

	testInfraDelegateMemberCap = store.TestInfraOfflineDelegationMaxMemberCap + 1
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--member-cap")
	testInfraDelegateMemberCap = 1

	testInfraDelegateWorkloadSub = ""
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--workload-sub")
	testInfraDelegateWorkloadSub = dlCmdSub

	testInfraDelegateHubID = " " + tiaCmdHubID
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "whitespace")

	for _, name := range []string{"scope", "scopes", "admin", "admin-role", "tier"} {
		assert.Nil(t, serverTestInfraDelegateCmd.Flags().Lookup(name), "no --%s flag", name)
	}
}

func TestTestInfraDelegateRevoke(t *testing.T) {
	cfg, client := dlCmdSetup(t, true)
	ctx := context.Background()
	var out bytes.Buffer
	require.NoError(t, delegateTestInfra(ctx, client, cfg, &out))
	rows, err := client.TestIdentityDelegation.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	testInfraDelegateRevokeHubID, testInfraDelegateRevokeID, testInfraDelegateRevokeOperator = tiaCmdHubID, rows[0].ID.String(), "op"
	out.Reset()
	require.NoError(t, revokeTestInfraDelegation(ctx, client, &out))
	assert.Contains(t, out.String(), "Offline issuer delegation revoked")
	out.Reset()
	require.NoError(t, revokeTestInfraDelegation(ctx, client, &out))
	assert.Contains(t, out.String(), "already revoked")

	testInfraDelegateRevokeHubID = "another-hub"
	require.ErrorIs(t, revokeTestInfraDelegation(ctx, client, &out), testinfra.ErrDelegationOtherHub)

	require.NoError(t, testInfraDelegateRevokeArgs(nil, nil))
	testInfraDelegateRevokeID = ""
	assert.ErrorContains(t, testInfraDelegateRevokeArgs(nil, nil), "--id")
}

// Host commands like mark: never available in agent mode.
func TestTestInfraDelegate_HumanCLIOnly(t *testing.T) {
	for _, name := range []string{"server.test-infra.delegate", "server.test-infra.delegate.revoke"} {
		assert.False(t, agentAllowed[name], "%s must not be allowed in agent mode", name)
	}
	build := func() *cobra.Command {
		root := buildTestTree()
		for _, child := range root.Commands() {
			if child.Name() == "server" {
				ti := &cobra.Command{Use: "test-infra"}
				del := &cobra.Command{Use: "delegate"}
				del.AddCommand(&cobra.Command{Use: "revoke"})
				ti.AddCommand(del)
				child.AddCommand(ti)
			}
		}
		return root
	}
	t.Setenv("SCION_CLI_MODE", "agent")
	root := build()
	applyModeRestrictions(root, resolveMode())
	names := collectCommandNames(root)
	assert.NotContains(t, names, "server.test-infra.delegate")
	assert.NotContains(t, names, "server.test-infra.delegate.revoke")

	t.Setenv("SCION_CLI_MODE", "human")
	root = build()
	applyModeRestrictions(root, resolveMode())
	names = collectCommandNames(root)
	assert.Contains(t, names, "server.test-infra.delegate")
	assert.Contains(t, names, "server.test-infra.delegate.revoke")

	// The real commands are registered under server test-infra.
	var found *cobra.Command
	for _, c := range serverTestInfraCmd.Commands() {
		if c.Name() == "delegate" {
			found = c
		}
	}
	require.NotNil(t, found)
	require.Len(t, found.Commands(), 1)
	assert.Equal(t, "revoke", found.Commands()[0].Name())
}
