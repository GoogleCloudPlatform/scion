/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/spf13/cobra"
)

// Seams for runNetnsSetup tests.
var (
	netnsSetupEUID         = os.Geteuid
	netnsSetupInstallRules = metadata.InstallInterceptionRules
)

var netnsSetupCmd = &cobra.Command{
	Use:    "netns-setup",
	Short:  "Configure network namespace metadata interception and optional firewall",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runNetnsSetup()
	},
}

func init() {
	rootCmd.AddCommand(netnsSetupCmd)
}

// runNetnsSetup runs inside a short-lived setup container sharing the agent
// container's network namespace. It installs the opt-in egress firewall (if
// requested) before installing the metadata iptables rules, then exits.
func runNetnsSetup() error {
	if netnsSetupEUID() != 0 {
		return fmt.Errorf("netns-setup requires root (euid 0)")
	}
	metaCfg := metadata.ConfigFromEnv()
	if metaCfg == nil {
		return fmt.Errorf("netns-setup requires SCION_METADATA_MODE to be configured (block or assign)")
	}
	if _, err := setupInitFirewall(); err != nil {
		return err
	}
	if err := netnsSetupInstallRules(metaCfg.Mode, metaCfg.Port); err != nil {
		return err
	}
	return nil
}
