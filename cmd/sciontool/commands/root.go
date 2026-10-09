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
		if isHookSubcommand(cmd) {
			log.SetQuiet(true)
		}
		applyLogLevelFlag(cmd)
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	// Decide quiet mode before Init: Init resolves SCION_LOG_LEVEL /
	// SCION_DEBUG, and the one-time deprecation warning must not reach the
	// captured stderr of every short-lived hook/status invocation.
	if isHookInvocation(os.Args[1:]) {
		log.SetQuiet(true)
	}
	log.Init()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// applyLogLevelFlag routes an explicitly given --log-level through the
// shared level parser at flag precedence (flag > SCION_LOG_LEVEL >
// SCION_DEBUG). When the flag is not given, the environment decides, which
// keeps the historical default (info) when nothing is set.
func applyLogLevelFlag(cmd *cobra.Command) {
	f := cmd.Flags().Lookup("log-level")
	if f == nil || !f.Changed {
		return
	}
	if err := log.ApplyLogLevel(logLevel); err != nil {
		log.Warn("--log-level: %v; using info", err)
	}
}

// isHookInvocation reports whether args (without the program name) select
// a hook/status subcommand, before cobra has parsed them.
func isHookInvocation(args []string) bool {
	target, _, err := rootCmd.Find(args)
	return err == nil && isHookSubcommand(target)
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
		"Logging verbosity: debug, info, warn, error, optionally with per-component levels (e.g. info,hooks=debug); overrides SCION_LOG_LEVEL")
}
