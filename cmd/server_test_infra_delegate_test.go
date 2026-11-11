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
		testInfraDelegateScope, testInfraDelegateAdminCap, testInfraDelegateAcknowledge = testInfraDelegateScopeMember, 0, false
	})
	testInfraDelegateScope, testInfraDelegateAdminCap, testInfraDelegateAcknowledge = testInfraDelegateScopeMember, 0, false
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

// The flag checks run before anything is opened. --scope selects member or
// hub-admin only; there is no flag for any other tier.
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

	for _, name := range []string{"scopes", "admin", "admin-role", "tier", "super-admin"} {
		assert.Nil(t, serverTestInfraDelegateCmd.Flags().Lookup(name), "no --%s flag", name)
	}
	require.NotNil(t, serverTestInfraDelegateCmd.Flags().Lookup("scope"))
	assert.Equal(t, testInfraDelegateScopeMember, serverTestInfraDelegateCmd.Flags().Lookup("scope").DefValue)
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

// Offline hub-admin delegations (--scope hub-admin).

const dlCmdAdminSub = "100000000000000000082"

// dlCmdAdminSetup is dlCmdSetup with an admin-tier subject, the offline
// admin delegation setting bound to tiaCmdHubID, and the hub-admin flags.
func dlCmdAdminSetup(t *testing.T) (*config.GlobalConfig, *ent.Client) {
	t.Helper()
	cfg, client := dlCmdSetup(t, true)
	cfg.Hub.TestIdentities = &config.TestIdentitiesConfig{
		WorkloadIssuers: []config.TestIdentityWorkloadIssuer{
			{Sub: dlCmdSub, MaxTier: config.WorkloadIssuerTierMember},
			{Sub: dlCmdAdminSub, MaxTier: config.WorkloadIssuerTierAdmin},
		},
		OfflineAdminDelegations: tiaCmdHubID,
	}
	testInfraDelegateWorkloadSub = dlCmdAdminSub
	testInfraDelegateScope, testInfraDelegateAcknowledge = testInfraDelegateScopeHubAdmin, true
	testInfraDelegateTTL, testInfraDelegateMemberCap, testInfraDelegateAdminCap = 0, 0, 0
	return cfg, client
}

func TestTestInfraDelegate_HubAdminCreates(t *testing.T) {
	cfg, client := dlCmdAdminSetup(t)
	testInfraDelegateAdminCap = 2
	require.NoError(t, testInfraDelegateArgs(nil, nil))
	var out bytes.Buffer
	require.NoError(t, delegateTestInfra(context.Background(), client, cfg, &out))
	rows, err := client.TestIdentityDelegation.Query().All(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, store.TestInfraOfflineAdminDelegationScope, rows[0].Scope)
	assert.Equal(t, 2, rows[0].MemberCap)
	assert.Equal(t, store.TestInfraOfflineAdminDelegationDefaultTTL, rows[0].ExpiresAt.Sub(rows[0].CreatedAt))
	s := out.String()
	assert.Contains(t, s, "scope="+store.TestInfraOfflineAdminDelegationScope)
	assert.Contains(t, s, "admin_cap=2")
	assert.NotContains(t, s, "Bearer")
}

// The configuration must carry the offline admin delegation setting for
// --hub-id: refused with the command's own named error (before the
// database writer is reached), nothing written.
func TestTestInfraDelegate_HubAdminRefusedWithoutOptIn(t *testing.T) {
	for name, value := range map[string]string{"unset": "", "another hub": "another-hub", "padded": tiaCmdHubID + " "} {
		t.Run(name, func(t *testing.T) {
			cfg, client := dlCmdAdminSetup(t)
			cfg.Hub.TestIdentities.OfflineAdminDelegations = value
			var out bytes.Buffer
			err := delegateTestInfra(context.Background(), client, cfg, &out)
			require.ErrorIs(t, err, ErrTestInfraDelegateAdminOptIn)
			assert.Contains(t, err.Error(), "nothing was written")
			rows, audits := dlCmdCounts(t, client)
			assert.Zero(t, rows)
			assert.Zero(t, audits)
		})
	}
	// A member delegation needs no opt-in.
	cfg, client := dlCmdSetup(t, true)
	var out bytes.Buffer
	require.NoError(t, delegateTestInfra(context.Background(), client, cfg, &out))
}

func TestTestInfraDelegateArgs_Scopes(t *testing.T) {
	dlCmdAdminSetup(t)
	require.NoError(t, testInfraDelegateArgs(nil, nil))

	for _, v := range []string{"super-admin", "superadmin", "SUPER-ADMIN", "super_admin", "test_identity:issue_super_admin"} {
		testInfraDelegateScope = v
		assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "never delegable offline", v)
	}
	for _, v := range []string{"admin", "owner", "", "Hub-Admin"} {
		testInfraDelegateScope = v
		assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--scope must be", v)
	}
	testInfraDelegateScope = testInfraDelegateScopeHubAdmin

	testInfraDelegateAcknowledge = false
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--"+testInfraMarkAcknowledgeFlag)
	testInfraDelegateAcknowledge = true

	testInfraDelegateTTL = store.TestInfraOfflineAdminDelegationMaxTTL + time.Minute
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--ttl")
	testInfraDelegateTTL = store.TestInfraOfflineAdminDelegationMaxTTL
	require.NoError(t, testInfraDelegateArgs(nil, nil))
	testInfraDelegateTTL = 0

	testInfraDelegateAdminCap = store.TestInfraOfflineAdminDelegationMaxAdminCap + 1
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--admin-cap")
	testInfraDelegateAdminCap = 0

	testInfraDelegateMemberCap = 1
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--member-cap")
	testInfraDelegateMemberCap = 0

	// --admin-cap belongs to hub-admin; the acknowledgement is not needed
	// for member.
	testInfraDelegateScope, testInfraDelegateAcknowledge = testInfraDelegateScopeMember, false
	require.NoError(t, testInfraDelegateArgs(nil, nil))
	testInfraDelegateAdminCap = 1
	assert.ErrorContains(t, testInfraDelegateArgs(nil, nil), "--admin-cap")
}

// The existing revoke command revokes an offline hub-admin delegation.
func TestTestInfraDelegateRevoke_HubAdmin(t *testing.T) {
	cfg, client := dlCmdAdminSetup(t)
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
	row, err := client.TestIdentityDelegation.Get(ctx, rows[0].ID)
	require.NoError(t, err)
	assert.NotNil(t, row.RevokedAt)
}
