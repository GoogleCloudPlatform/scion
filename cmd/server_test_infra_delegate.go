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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
)

// ErrTestInfraDelegateHubClass refuses an offline delegation on a hub whose
// configuration is not server.hub.class: test.
var ErrTestInfraDelegateHubClass = errors.New(`test-infra delegate: server.hub.class must be "test"`)

var (
	testInfraDelegateHubID       string
	testInfraDelegateWorkloadSub string
	testInfraDelegateTTL         time.Duration
	testInfraDelegateMemberCap   int
	testInfraDelegateLabel       string
	testInfraDelegateOperator    string
	testInfraDelegateConfigPath  string
	testInfraDelegateDBURL       string

	testInfraDelegateRevokeHubID      string
	testInfraDelegateRevokeID         string
	testInfraDelegateRevokeOperator   string
	testInfraDelegateRevokeConfigPath string
	testInfraDelegateRevokeDBURL      string
)

// serverTestInfraDelegateCmd creates an offline member-tier workload issuer
// delegation (ptone/scion#4240, phase DL).
var serverTestInfraDelegateCmd = &cobra.Command{
	Use:   "delegate",
	Short: "Delegate member-tier test identity issuance to one allow-listed workload (offline)",
	Long: `Create a member-tier workload issuer delegation directly in the configured hub
database, for a test-designated hub, without a human grantor.

The delegation lets ONE workload identity (a Google service account subject on
server.hub.test_identities.workload_issuers) issue member and viewer test
identities on this hub. It never covers an admin tier: test hub-admin and test
super-admin delegations still require a live human super-admin session.

The command refuses, writing nothing, unless:
  - the hub id is pinned (server.hub.hub_id or SCION_SERVER_HUB_HUBID) and
    --hub-id equals the hub id resolved from that same configuration
  - server.hub.class is "test"
  - the test infrastructure marker is present and names --hub-id
  - --workload-sub is on server.hub.test_identities.workload_issuers with a
    max_tier covering member
  - --ttl and --member-cap are within bounds, and the hub holds fewer than the
    maximum number of live offline delegations

It writes one delegation row, bound to --hub-id and to the current marker, and
a test_identity_delegation_create_offline audit row (operator, host, time) in
the same transaction. It prints the delegation id (not a secret) and never a
token.

The running hub honours the delegation only while ALL of these hold, checked on
every request: the marker is still the one the delegation was bound to and
names the hub's own pinned hub id, the hub runs with --test-mode at member or
above, and the delegation is neither revoked nor expired. Unmarking (or
re-marking) the database, restarting the hub with --test-mode off, or revoking
the delegation ends it at the next request.

Example:
  scion server test-infra delegate --hub-id <hub-id> --workload-sub <subject> --ttl 8h`,
	Args: testInfraDelegateArgs,
	RunE: runTestInfraDelegate,
}

// serverTestInfraDelegateRevokeCmd revokes an offline delegation.
var serverTestInfraDelegateRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke an offline member-tier workload issuer delegation (offline)",
	Long: `Revoke an offline workload issuer delegation in the configured hub database.

The delegation must be an offline delegation bound to --hub-id; delegations
created by a human through the hub API are revoked through the hub API. The
command writes a test_identity_delegation_revoke audit row in the same
transaction. Revoking a revoked delegation changes nothing. The next request
that presents the delegation is refused.

Example:
  scion server test-infra delegate revoke --hub-id <hub-id> --id <delegation-id>`,
	Args: testInfraDelegateRevokeArgs,
	RunE: runTestInfraDelegateRevoke,
}

func init() {
	serverTestInfraCmd.AddCommand(serverTestInfraDelegateCmd)
	serverTestInfraDelegateCmd.AddCommand(serverTestInfraDelegateRevokeCmd)

	f := serverTestInfraDelegateCmd.Flags()
	f.StringVar(&testInfraDelegateHubID, "hub-id", "", "Hub id the delegation is bound to (the hub's pinned, resolved hub id)")
	f.StringVar(&testInfraDelegateWorkloadSub, "workload-sub", "", "Google subject (decimal unique id) of the allow-listed workload service account")
	f.DurationVar(&testInfraDelegateTTL, "ttl", store.TestInfraOfflineDelegationDefaultTTL,
		fmt.Sprintf("Delegation lifetime (%s to %s)", store.TestInfraOfflineDelegationMinTTL, store.TestInfraOfflineDelegationMaxTTL))
	f.IntVar(&testInfraDelegateMemberCap, "member-cap", store.TestInfraOfflineDelegationMaxMemberCap,
		fmt.Sprintf("Live member or viewer test identities the delegation may hold at once (1 to %d)", store.TestInfraOfflineDelegationMaxMemberCap))
	f.StringVar(&testInfraDelegateLabel, "label", "", "Optional short label for the test window")
	f.StringVar(&testInfraDelegateOperator, "operator", "", "Operator identity for the audit record (defaults to $USER@hostname)")
	f.StringVar(&testInfraDelegateConfigPath, "config", "", "Path to server configuration file")
	f.StringVar(&testInfraDelegateDBURL, "db", "", "Database URL/path (overrides config)")

	rf := serverTestInfraDelegateRevokeCmd.Flags()
	rf.StringVar(&testInfraDelegateRevokeHubID, "hub-id", "", "Hub id the delegation is bound to (the hub's pinned, resolved hub id)")
	rf.StringVar(&testInfraDelegateRevokeID, "id", "", "Delegation id")
	rf.StringVar(&testInfraDelegateRevokeOperator, "operator", "", "Operator identity for the audit record (defaults to $USER@hostname)")
	rf.StringVar(&testInfraDelegateRevokeConfigPath, "config", "", "Path to server configuration file")
	rf.StringVar(&testInfraDelegateRevokeDBURL, "db", "", "Database URL/path (overrides config)")
}

// testInfraDelegateArgs validates the delegate flags before anything is
// opened.
func testInfraDelegateArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	if testInfraDelegateHubID == "" {
		return errors.New("--hub-id is required")
	}
	if strings.TrimSpace(testInfraDelegateHubID) != testInfraDelegateHubID {
		return errors.New("--hub-id must not carry surrounding whitespace")
	}
	if testInfraDelegateWorkloadSub == "" {
		return errors.New("--workload-sub is required")
	}
	if testInfraDelegateTTL < store.TestInfraOfflineDelegationMinTTL || testInfraDelegateTTL > store.TestInfraOfflineDelegationMaxTTL {
		return fmt.Errorf("--ttl must be between %s and %s", store.TestInfraOfflineDelegationMinTTL, store.TestInfraOfflineDelegationMaxTTL)
	}
	if testInfraDelegateMemberCap < 1 || testInfraDelegateMemberCap > store.TestInfraOfflineDelegationMaxMemberCap {
		return fmt.Errorf("--member-cap must be between 1 and %d", store.TestInfraOfflineDelegationMaxMemberCap)
	}
	return nil
}

// testInfraDelegateRevokeArgs validates the revoke flags before anything is
// opened.
func testInfraDelegateRevokeArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	if testInfraDelegateRevokeHubID == "" {
		return errors.New("--hub-id is required")
	}
	if strings.TrimSpace(testInfraDelegateRevokeHubID) != testInfraDelegateRevokeHubID {
		return errors.New("--hub-id must not carry surrounding whitespace")
	}
	if testInfraDelegateRevokeID == "" {
		return errors.New("--id is required")
	}
	return nil
}

// checkTestInfraDelegateConfig applies the configuration-side guards of an
// offline delegation before the database is opened: the MK-h hub id guards
// (pinned, equal to --hub-id) and server.hub.class: test. The marker and
// allowlist are checked by testinfra.Delegate.
func checkTestInfraDelegateConfig(cfg *config.GlobalConfig, hubID string) error {
	if err := checkTestInfraMarkHubID(cfg, hubID); err != nil {
		return err
	}
	if !config.IsTestHubClass(cfg.Hub.HubClass) {
		return fmt.Errorf("%w (%s is %q); nothing was written", ErrTestInfraDelegateHubClass, config.HubClassKey, cfg.Hub.HubClass)
	}
	return nil
}

func runTestInfraDelegate(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	cfg, err := loadTestInfraConfig(testInfraDelegateConfigPath, testInfraDelegateDBURL)
	if err != nil {
		return err
	}
	if err := checkTestInfraDelegateConfig(cfg, testInfraDelegateHubID); err != nil {
		return err
	}
	client, err := openTestInfraClient(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = client.Close() }()

	return delegateTestInfra(ctx, client, cfg, cmd.OutOrStdout())
}

// delegateTestInfra re-checks the configuration guards, creates the
// delegation through pkg/testinfra and reports it. It prints no token.
func delegateTestInfra(ctx context.Context, client *ent.Client, cfg *config.GlobalConfig, out io.Writer) error {
	if err := checkTestInfraDelegateConfig(cfg, testInfraDelegateHubID); err != nil {
		return err
	}
	operator := testInfraDelegateOperator
	if operator == "" {
		operator = resolveOperatorIdentity()
	}
	host, _ := os.Hostname()
	d, err := testinfra.Delegate(ctx, client, testinfra.DelegateInput{
		HubID:           testInfraDelegateHubID,
		WorkloadSub:     testInfraDelegateWorkloadSub,
		WorkloadIssuers: cfg.Hub.TestIdentityWorkloadIssuers(),
		TTL:             testInfraDelegateTTL,
		MemberCap:       testInfraDelegateMemberCap,
		Label:           testInfraDelegateLabel,
		Operator:        operator,
		Host:            host,
		Now:             time.Now(),
	})
	if err != nil {
		if errors.Is(err, testinfra.ErrMarkerAbsent) {
			return fmt.Errorf("%w; run \"scion server test-infra mark\" for this hub first; nothing was written", err)
		}
		return fmt.Errorf("%w; nothing was written", err)
	}
	_, _ = fmt.Fprintf(out, "Offline issuer delegation created: id=%s hub_id=%s workload_sub=%s scope=%s member_cap=%d expires_at=%s\n",
		d.ID, d.HubID, d.WorkloadSub, d.Scope, d.MemberCap, d.ExpiresAt.UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintln(out, "The hub honours it only while the marker is unchanged and the hub runs with --test-mode at member or above.")
	return nil
}

func runTestInfraDelegateRevoke(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	cfg, err := loadTestInfraConfig(testInfraDelegateRevokeConfigPath, testInfraDelegateRevokeDBURL)
	if err != nil {
		return err
	}
	if err := checkTestInfraMarkHubID(cfg, testInfraDelegateRevokeHubID); err != nil {
		return err
	}
	client, err := openTestInfraClient(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = client.Close() }()

	return revokeTestInfraDelegation(ctx, client, cmd.OutOrStdout())
}

// revokeTestInfraDelegation revokes an offline delegation through
// pkg/testinfra and reports it.
func revokeTestInfraDelegation(ctx context.Context, client *ent.Client, out io.Writer) error {
	operator := testInfraDelegateRevokeOperator
	if operator == "" {
		operator = resolveOperatorIdentity()
	}
	host, _ := os.Hostname()
	d, changed, err := testinfra.RevokeDelegation(ctx, client, testinfra.RevokeDelegationInput{
		HubID:    testInfraDelegateRevokeHubID,
		ID:       testInfraDelegateRevokeID,
		Operator: operator,
		Host:     host,
		Now:      time.Now(),
	})
	if err != nil {
		return fmt.Errorf("%w; nothing was changed", err)
	}
	if !changed {
		_, _ = fmt.Fprintf(out, "Offline issuer delegation %s was already revoked; nothing was changed.\n", d.ID)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Offline issuer delegation revoked: id=%s hub_id=%s\n", d.ID, d.HubID)
	return nil
}
