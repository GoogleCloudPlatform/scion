package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

type CLIMode string

const (
	ModeHuman CLIMode = "human"
	ModeAgent CLIMode = "agent"
)

// removedModeAssistant is the value of a CLI mode that has been removed.
// A SCION_CLI_MODE or cli.mode value that still names it is ignored with
// a single warning, and the CLI runs in human mode.
const removedModeAssistant = "assistant"

// removedModeWarnOnce makes sure the removed-mode warning is printed at
// most once per process, even though resolveMode is called many times.
var removedModeWarnOnce = &sync.Once{}

func warnRemovedMode(source string) {
	removedModeWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "Warning: %s=%q: the %q CLI mode has been removed and is ignored; running in %q mode\n",
			source, removedModeAssistant, removedModeAssistant, ModeHuman)
	})
}

// agentAllowed lists commands available in agent mode.
// Uses dot-separated command paths: "notifications.ack", "schedule.list", etc.
// Every entry — including parent commands — must be listed explicitly;
// parents are NOT implicitly allowed when a child is listed.
var agentAllowed = map[string]bool{
	"create":                      true,
	"delete":                      true,
	"keys":                        true,
	"list":                        true,
	"logs":                        true,
	"look":                        true,
	"message":                     true,
	"reincarnate":                 true,
	"resume":                      true,
	"start":                       true,
	"stop":                        true,
	"suspend":                     true,
	"version":                     true,
	"whoami":                      true,
	"global-flags":                true,
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
	"schedule.create":             true,
	"schedule.create-recurring":   true,
	"schedule.pause":              true,
	"schedule.resume":             true,
	"schedule.delete":             true,
	"schedule.history":            true,
	"shared-dir":                  true,
	"shared-dir.list":             true,
	"shared-dir.info":             true,
	"templates":                   true,
	"templates.list":              true,
	"templates.show":              true,
	"templates.create":            true,
	"templates.clone":             true,
	"templates.delete":            true,
	"templates.update-default":    true,
	"templates.import":            true,
	"templates.sync":              true,
	"templates.push":              true,
	"templates.pull":              true,
	"templates.status":            true,
	"template":                    true,
	"template.list":               true,
	"template.show":               true,
	"template.clone":              true,
	"template.delete":             true,
	"template.import":             true,
	"template.sync":               true,
	"template.push":               true,
	"template.pull":               true,
	"template.status":             true,
	"harness-config":              true,
	"harness-config.list":         true,
	"harness-config.show":         true,
	"harness-config.install":      true,
	"harness-config.sync":         true,
	"harness-config.push":         true,
	"harness-config.pull":         true,
	"harness-config.delete":       true,
	"harness-config.reset":        true,
	"harness-config.upgrade":      true,
	"user":                        true,
	"user.skills":                 true,
	"user.skills.list":            true,
	"user.skills.add":             true,
	"user.skills.remove":          true,
	"skills":                      true,
	"skills.list":                 true,
	"skills.show":                 true, // text output also lists versions (skills.versions itself is not allowed)
	"skill":                       true,
	"skill.list":                  true,
	"project":                     true,
	"project.skills":              true,
	"project.skills.list":         true,
	"project.skills.add":          true,
	"project.skills.remove":       true,
	// Read-only status of one service account; the mutating subcommands
	// (add, mint, verify, remove) and list stay out (ptone, ptone/scion#4018).
	"project.service-accounts":      true,
	"project.service-accounts.show": true,
	"set-message-mode":              true,
	"conversation":                  true,
	"conversation.list":             true,
	"conversation.messages":         true,
	"conversation.get":              true,
	"conversation.get-message":      true,
	"conversation.create":           true,
	"conversation.set-default":      true,
	"conversation.participants":     true,
	"conversation.join":             true,
	"conversation.leave":            true,
	"conversation.catch-up":         true,
	"artifact":                      true,
	"artifact.publish":              true,
	"artifact.get":                  true,
	"artifact.versions":             true,
}

// resolveMode determines the active CLI mode from environment and settings.
// Priority: SCION_CLI_MODE env var > cli.mode setting > default (human).
func resolveMode() CLIMode {
	if envMode := os.Getenv("SCION_CLI_MODE"); envMode != "" {
		switch CLIMode(envMode) {
		case ModeHuman, ModeAgent:
			return CLIMode(envMode)
		case removedModeAssistant:
			warnRemovedMode("SCION_CLI_MODE")
			return ModeHuman
		default:
			fmt.Fprintf(os.Stderr, "Warning: unrecognized SCION_CLI_MODE=%q, defaulting to %q\n", envMode, ModeHuman)
			return ModeHuman
		}
	}

	settings, err := config.LoadSettings("")
	if err == nil && settings != nil && settings.CLI != nil && settings.CLI.Mode != "" {
		switch CLIMode(settings.CLI.Mode) {
		case ModeHuman, ModeAgent:
			return CLIMode(settings.CLI.Mode)
		case removedModeAssistant:
			warnRemovedMode("cli.mode")
			return ModeHuman
		default:
			fmt.Fprintf(os.Stderr, "Warning: unrecognized cli.mode=%q in settings, defaulting to %q\n", settings.CLI.Mode, ModeHuman)
			return ModeHuman
		}
	}

	return ModeHuman
}

// applyModeRestrictions removes commands from the Cobra tree that are not
// permitted in mode (normally the result of resolveMode).
func applyModeRestrictions(root *cobra.Command, mode CLIMode) {
	if mode == ModeAgent {
		applyAgentMode(root)
	}
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
			parent.Long = dropCommandLines(parent.Long, parent.CommandPath()+" "+name)
		}
	}
}

// dropCommandLines removes the lines of a command's long help that show
// the usage of a removed subcommand, so the help does not list a command
// that is not available. A line matches when, after leading spaces, it is
// usage (for example "scion artifact share") or starts with usage and a
// space.
func dropCommandLines(long, usage string) string {
	if !strings.Contains(long, usage) {
		return long
	}
	lines := strings.Split(long, "\n")
	kept := lines[:0]
	for _, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if t == usage || strings.HasPrefix(t, usage+" ") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
