/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	projectPath      string
	globalMode     bool
	profile        string
	outputFormat   string
	hubEndpoint    string // Hub API endpoint override
	noHub          bool   // Disable Hub integration for this invocation
	autoConfirm    bool   // Auto-confirm prompts (--yes flag)
	nonInteractive bool   // Full non-interactive mode (implies --yes, errors on ambiguous prompts)
	autoHelp       = true // Default to true, updated in PersistentPreRunE
	debugMode      bool   // Enable debug output
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "scion",
	Short: "A container-based orchestration tool for managing concurrent LLM agents",
	Long: `Scion is a container-based orchestration tool for managing
concurrent LLM agents. It enables parallel execution of specialized
sub-agents with isolated identities, credentials, and workspaces.

Use --non-interactive for scripted/automated usage. This implies --yes
and causes any prompt that cannot be resolved without user input to
return an error instead of blocking.`,
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// --non-interactive implies --yes
		if nonInteractive {
			autoConfirm = true
		}

		// Enable debug mode if --debug flag is set
		if debugMode {
			util.EnableDebug()
		}

		// Detect agent container context without a reachable Hub endpoint.
		// SCION_HOST_UID is set by the runtime when launching agent containers.
		// If present but no non-localhost Hub endpoint is configured, the CLI
		// cannot do anything useful — warn the agent and abort.
		if err := checkAgentContainerContext(cmd); err != nil {
			return err
		}

		if globalMode && projectPath == "" {
			projectPath = "global"
		}

		if util.IsGitRepo() {
			if err := util.CheckGitVersion(); err != nil {
				return fmt.Errorf("git check failed: %w", err)
			}
		}

		// Determine if this command requires explicit project context
		// Commands that don't require project context:
		// - help, version, completion (built-in or explicit)
		// - init, project init (creates project)
		// - server (runs hub server, doesn't need local project)
		cmdName := cmd.Name()
		parentName := ""
		if cmd.Parent() != nil {
			parentName = cmd.Parent().Name()
		}

		requiresProject := true
		switch cmdName {
		case "help", "version", "completion", "doctor", "whoami":
			requiresProject = false
		case "init":
			// Both top-level init and project init don't require existing project
			requiresProject = false
		case "scion":
			// Root command itself doesn't require project
			requiresProject = false
		}
		// Server subcommands run the hub server and don't need a local project
		if commandInSubtree(cmd, "server") {
			requiresProject = false
		}
		// Project subcommands operate on all projects, not just the current one
		if parentName == "project" || parentName == "grove" {
			requiresProject = false
		}

		// For commands that require project context, use RequireProjectPath
		// to error if no project found and --global not specified
		if requiresProject && projectPath == "" {
			if _, _, err := config.RequireProjectPath(projectPath); err != nil {
				return err
			}
		}

		// Load settings to get cli.autohelp
		settings, err := config.LoadSettings(projectPath)
		if err == nil && settings.CLI != nil && settings.CLI.AutoHelp != nil {
			autoHelp = *settings.CLI.AutoHelp
		}

		// Check versioned settings for cli.interactive_disabled
		if vs, _, vsErr := config.LoadEffectiveSettings(projectPath); vsErr == nil && vs != nil {
			if vs.CLI != nil && vs.CLI.InteractiveDisabled != nil && *vs.CLI.InteractiveDisabled {
				nonInteractive = true
				autoConfirm = true
			}
		}

		// Agent mode implies non-interactive: prompts that require stdin
		// will hang indefinitely inside an unattended agent container.
		if !nonInteractive && resolveMode() == ModeAgent {
			nonInteractive = true
			autoConfirm = true
			util.Debugf("agent mode detected, non-interactive mode auto-enabled")
		}

		if outputFormat != "" {
			if outputFormat != "json" && outputFormat != "plain" {
				return fmt.Errorf("invalid format: %s (allowed: json, plain)", outputFormat)
			}
			// Reject --format json for interactive/streaming commands
			if outputFormat == "json" {
				if reason, ok := interactiveOnlyCommands[cmd.CommandPath()]; ok {
					return fmt.Errorf("--format json is not supported for '%s' because %s", cmd.CommandPath(), reason)
				}
				// Silently ignore --format json for commands that don't support structured output
				if jsonNoOpCommands[cmd.CommandPath()] {
					outputFormat = ""
				}
			}
		}

		// Check image_registry is configured for commands that need it.
		// Skip for config commands (users need those to set the registry).
		// Skip in hub context (inside a container, the agent is already
		// running — image_registry is not needed).
		requiresRegistry := requiresProject
		if commandInSubtree(cmd, "config") {
			requiresRegistry = false
		}
		if commandInSubtree(cmd, "hub") || commandInSubtree(cmd, "server") {
			requiresRegistry = false
		}
		if requiresRegistry && config.IsHubContext() {
			requiresRegistry = false
		}
		if requiresRegistry {
			if err := config.RequireImageRegistry(projectPath, profile); err != nil {
				return err
			}
		}

		// Check for dev auth usage and warn if Hub is enabled
		printDevAuthWarningIfNeeded(projectPath)

		return nil
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	// Early settings load to determine autoHelp behavior
	// This handles cases where ExecuteC fails during flag parsing or unknown commands
	tempProjectPath:= ""
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--project" || arg == "--grove" || arg == "-g" {
			if i+1 < len(os.Args) {
				tempProjectPath = os.Args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--project=") {
			tempProjectPath = strings.TrimPrefix(arg, "--project=")
		} else if strings.HasPrefix(arg, "--grove=") {
			tempProjectPath = strings.TrimPrefix(arg, "--grove=")
		} else if arg == "--global" {
			tempProjectPath = "global"
		}
	}
	settings, _ := config.LoadSettings(tempProjectPath)
	if settings != nil && settings.CLI != nil && settings.CLI.AutoHelp != nil {
		autoHelp = *settings.CLI.AutoHelp
	}

	applyModeRestrictions(rootCmd)

	cmd, err := rootCmd.ExecuteC()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n%s%s%sError: %v%s\n\n", util.BgRed, util.White, util.Bold, err, util.Reset)
		if cmd != nil && autoHelp {
			cmd.Usage()
		}
		os.Exit(1)
	}
}

func commandInSubtree(cmd *cobra.Command, name string) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current.Name() == name {
			return true
		}
	}
	return false
}

func init() {
	rootCmd.Long = util.GetBanner() + "\n" + rootCmd.Long
	rootCmd.PersistentFlags().StringVarP(&projectPath, "project", "g", "", "Project identifier: path, slug (with Hub), or git URL (with Hub)")
	rootCmd.PersistentFlags().StringVar(&projectPath, "grove", "", "Deprecated alias for --project")
	_ = rootCmd.PersistentFlags().MarkDeprecated("grove", "use --project instead")
	_ = rootCmd.PersistentFlags().MarkHidden("grove")

	rootCmd.PersistentFlags().BoolVar(&globalMode, "global", false, "Use the global project (equivalent to --project global)")
	rootCmd.PersistentFlags().StringVarP(&profile, "profile", "p", "", "Configuration profile to use")
	rootCmd.PersistentFlags().StringVar(&outputFormat, "format", "", "Output format (e.g., json)")

	// Hub integration flags
	rootCmd.PersistentFlags().StringVar(&hubEndpoint, "hub", "", "Hub API endpoint URL (overrides SCION_HUB_ENDPOINT)")
	rootCmd.PersistentFlags().BoolVar(&noHub, "no-hub", false, "Disable Hub integration for this invocation (local-only mode)")

	// Confirmation and non-interactive flags
	rootCmd.PersistentFlags().BoolVarP(&autoConfirm, "yes", "y", false, "Skip confirmation prompt")
	rootCmd.PersistentFlags().BoolVar(&nonInteractive, "non-interactive", false, "Non-interactive mode: implies --yes, errors on ambiguous prompts")

	// Debug mode flag
	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "Enable debug output (equivalent to SCION_DEBUG=1)")
}

// GetHubEndpoint returns the effective Hub endpoint based on flags, settings,
// and environment variables.
// Returns empty string if Hub is disabled or not configured.
func GetHubEndpoint(settings interface{ GetHubEndpoint() string }) string {
	if noHub {
		return ""
	}
	if hubEndpoint != "" {
		return hubEndpoint
	}
	if settings != nil {
		if ep := settings.GetHubEndpoint(); ep != "" {
			return ep
		}
	}
	// Fall back to env vars — covers the case where settings loading didn't
	// populate the Hub struct (e.g., inside a hub-connected container where
	// the project path resolves to a synthetic/empty directory).
	if ep := os.Getenv("SCION_HUB_ENDPOINT"); ep != "" {
		return ep
	}
	if ep := os.Getenv("SCION_HUB_URL"); ep != "" {
		return ep
	}
	return ""
}

// IsHubEnabled returns true if Hub integration is enabled for this invocation.
func IsHubEnabled() bool {
	return !noHub
}

// IsNoHub returns true if Hub integration is explicitly disabled for this invocation.
func IsNoHub() bool {
	return noHub
}

// IsAutoConfirm returns true if prompts should be auto-confirmed.
func IsAutoConfirm() bool {
	return autoConfirm
}

// IsNonInteractive returns true if the CLI is running in full non-interactive mode.
// This implies autoConfirm but additionally causes ambiguous prompts (those without
// a deterministic default) to return errors instead of blocking on stdin.
func IsNonInteractive() bool {
	return nonInteractive
}

// printDevAuthWarningIfNeeded checks if dev auth is being used with Hub and prints a warning.
// This function is called on every command invocation via PersistentPreRunE.
func printDevAuthWarningIfNeeded(projectPath string) {
	// Skip if --no-hub flag is set
	if noHub {
		return
	}

	// Try to load settings to check if Hub is enabled
	settings, err := config.LoadSettings(projectPath)
	if err != nil {
		// If we can't load settings, skip the warning
		return
	}

	// Check if Hub is enabled (either via settings or --hub flag override)
	hubEnabled := settings.IsHubEnabled() || hubEndpoint != ""
	if !hubEnabled {
		return
	}

	// Check if explicit auth is configured in settings
	if settings.Hub != nil {
		if settings.Hub.Token != "" || settings.Hub.APIKey != "" || settings.Hub.BrokerToken != "" {
			// Explicit auth configured, not using dev auth
			return
		}
	}

	// Check if OAuth credentials are available (from scion hub auth login)
	endpoint := GetHubEndpoint(settings)
	if endpoint != "" && credentials.IsAuthenticated(endpoint) {
		// OAuth credentials available, not using dev auth
		return
	}

	// Check if a dev token would be used
	devToken, devTokenSource := apiclient.ResolveDevTokenWithSource()
	if devToken == "" {
		// No dev token available
		return
	}

	// Only warn if the dev token is explicitly set via environment variable,
	// or if the hub endpoint is a local address. A stale ~/.scion/dev-token file
	// should not trigger a warning when connecting to a remote hub, since
	// the remote hub likely doesn't use dev-auth.
	if devTokenSource != "SCION_DEV_TOKEN env var" {
		if !isLocalEndpoint(endpoint) {
			return
		}
	}

	// Dev auth is being used with Hub enabled - print warning to stderr
	fmt.Fprintf(os.Stderr, "\n%s%s WARNING: Development authentication enabled - not for production use %s\n\n",
		util.Bold, util.Yellow, util.Reset)
}

// checkAgentContainerContext detects when the CLI is running inside an agent
// container (SCION_HOST_UID is set) but has no reachable Hub endpoint. In that
// scenario the CLI cannot manage agents, projects, or any other resources, so we
// print a prominent banner and return an error to prevent confusion.
// A small set of informational commands (version, help, completion, doctor,
// config) are exempted so the agent can still inspect its environment.
func checkAgentContainerContext(cmd *cobra.Command) error {
	if os.Getenv("SCION_HOST_UID") == "" {
		// Not inside an agent container — nothing to check.
		return nil
	}

	cmdName := cmd.Name()
	switch cmdName {
	case "help", "version", "completion", "doctor", "config", "whoami", "scion":
		return nil
	}
	if cmd.Parent() != nil && cmd.Parent().Name() == "config" {
		return nil
	}

	// Resolve the hub endpoint from flags and env vars (settings may not
	// load cleanly inside a container, so check env vars directly too).
	endpoint := hubEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("SCION_HUB_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = os.Getenv("SCION_HUB_URL")
	}

	if endpoint != "" && !isLocalEndpoint(endpoint) {
		// A reachable (non-localhost) Hub endpoint is configured — all good.
		return nil
	}

	// With --network=host, the container shares the host's network namespace,
	// so localhost endpoints are reachable.
	if endpoint != "" && os.Getenv("SCION_NETWORK_MODE") == "host" {
		return nil
	}

	reason := "no Hub endpoint is configured"
	if endpoint != "" {
		reason = fmt.Sprintf("the Hub endpoint (%s) points to localhost, which is not reachable from inside this container", endpoint)
	}

	return fmt.Errorf(
		"%s%s╔══════════════════════════════════════════════════════════════════╗%s\n"+
			"%s%s║  SCION CLI — Running inside an agent container                 ║%s\n"+
			"%s%s╠══════════════════════════════════════════════════════════════════╣%s\n"+
			"%s%s║                                                                  ║%s\n"+
			"%s%s║  The scion CLI cannot be used from within an agent container     ║%s\n"+
			"%s%s║  because %s.%s\n"+
			"%s%s║                                                                  ║%s\n"+
			"%s%s║  To use the CLI, configure a reachable Hub endpoint:             ║%s\n"+
			"%s%s║    • Set SCION_HUB_ENDPOINT to a non-localhost URL               ║%s\n"+
			"%s%s║    • Or pass --hub <url> on the command line                     ║%s\n"+
			"%s%s║                                                                  ║%s\n"+
			"%s%s║  Allowed commands: version, help, doctor, config                 ║%s\n"+
			"%s%s╚══════════════════════════════════════════════════════════════════╝%s",
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, reason, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
		util.Bold, util.Yellow, util.Reset,
	)
}

// isLocalEndpoint returns true if the given endpoint URL points to a local address
// (localhost, 127.0.0.1, ::1, or 0.0.0.0).
func isLocalEndpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
}
