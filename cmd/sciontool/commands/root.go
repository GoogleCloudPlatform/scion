/*
Copyright 2025 The Scion Authors.
*/
package commands

import (
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/spf13/cobra"
)

var (
	logLevel string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "sciontool",
	Short: "Scion container initialization and lifecycle tool",
	Long: `sciontool is a unified binary designed to run inside Scion agent containers.
It serves as the container's specialized init process (PID 1), lifecycle manager,
and telemetry forwarder.

Commands:
  init      Run as container init (PID 1) and spawn child processes
  version   Print version information
  hook      Process harness hook events from stdin
  status    Update agent status (ask_user, task_completed)`,
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if logLevel == "debug" {
			log.SetDebug(true)
		}
		if isHookSubcommand(cmd) {
			log.SetQuiet(true)
		}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	log.Init()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func isHookSubcommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "hook", "status":
			return true
		}
	}
	return false
}

func init() {
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Logging verbosity: debug, info, warn, error")
}
