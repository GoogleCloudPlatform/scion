package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

type CLIMode string

const (
	ModeHuman     CLIMode = "human"
	ModeAssistant CLIMode = "assistant"
	ModeAgent     CLIMode = "agent"
)

// assistantDenied lists commands removed in assistant mode (relative to human).
// Uses dot-separated command paths: "hub.auth", "config.migrate", etc.
var assistantDenied = map[string]bool{
	"hub.auth":         true,
	"hub.token":        true,
	"grove.reconnect":  true,
	"config.migrate":   true,
	"config.cd-config": true,
	"config.cd-grove":  true,
	"cdw":              true,
	"clean":            true,
}

// agentAllowed lists commands available in agent mode.
// Parent commands are implicitly allowed when any child is allowed.
var agentAllowed = map[string]bool{
	"create":                      true,
	"delete":                      true,
	"list":                        true,
	"logs":                        true,
	"look":                        true,
	"message":                     true,
	"resume":                      true,
	"start":                       true,
	"stop":                        true,
	"suspend":                     true,
	"version":                     true,
	"whoami":                      true,
	"notifications":               true,
	"notifications.ack":           true,
	"notifications.subscribe":     true,
	"notifications.unsubscribe":   true,
	"notifications.update":        true,
	"notifications.subscriptions": true,
	"schedule":                    true,
	"schedule.list":               true,
	"schedule.get":                true,
	"schedule.cancel":             true,
	"schedule.history":            true,
	"shared-dir":                  true,
	"shared-dir.list":             true,
	"shared-dir.info":             true,
}

// resolveMode determines the active CLI mode from environment and settings.
// Priority: SCION_CLI_MODE env var > cli.mode setting > default (human).
func resolveMode() CLIMode {
	if envMode := os.Getenv("SCION_CLI_MODE"); envMode != "" {
		switch CLIMode(envMode) {
		case ModeHuman, ModeAssistant, ModeAgent:
			return CLIMode(envMode)
		default:
			fmt.Fprintf(os.Stderr, "Warning: unrecognized SCION_CLI_MODE=%q, defaulting to %q\n", envMode, ModeHuman)
			return ModeHuman
		}
	}

	settings, err := config.LoadSettings("")
	if err == nil && settings != nil && settings.CLI != nil && settings.CLI.Mode != "" {
		switch CLIMode(settings.CLI.Mode) {
		case ModeHuman, ModeAssistant, ModeAgent:
			return CLIMode(settings.CLI.Mode)
		default:
			fmt.Fprintf(os.Stderr, "Warning: unrecognized cli.mode=%q in settings, defaulting to %q\n", settings.CLI.Mode, ModeHuman)
			return ModeHuman
		}
	}

	return ModeHuman
}

// applyModeRestrictions removes commands from the Cobra tree that are not
// permitted in the current CLI mode.
func applyModeRestrictions(root *cobra.Command) {
	mode := resolveMode()
	if mode == ModeHuman {
		return
	}

	switch mode {
	case ModeAssistant:
		applyAssistantMode(root)
	case ModeAgent:
		applyAgentMode(root)
	}
}

func applyAssistantMode(root *cobra.Command) {
	removeCommands(root, "", func(path string) bool {
		return assistantDenied[path]
	})
}

func applyAgentMode(root *cobra.Command) {
	removeCommands(root, "", func(path string) bool {
		return !agentAllowed[path]
	})
}

// removeCommands walks the command tree and removes commands where shouldRemove
// returns true. It processes children recursively before deciding whether to
// remove a parent.
func removeCommands(parent *cobra.Command, prefix string, shouldRemove func(string) bool) {
	for _, child := range parent.Commands() {
		name := child.Name()
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		if name == "help" {
			continue
		}

		if child.HasSubCommands() {
			removeCommands(child, path, shouldRemove)
		}

		if shouldRemove(path) {
			parent.RemoveCommand(child)
		}
	}
}
