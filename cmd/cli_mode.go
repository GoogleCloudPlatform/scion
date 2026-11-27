package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
//
// Deliberately absent: "runtime-broker.convert" in every mode (--plan,
// --apply, --status, --rollback). It is human mode only: it acts on this
// host's Runtime Broker identity, settings and Hub rows, which an agent in
// a container must not change. cmd/platform_gate.go keeps it off Windows.
//
// Deliberately absent: "admin" and "admin.system-one.instance.*" (create,
// list, delete, probe). Decision system-one-admin-cli v1 keeps Hub System One
// provider instance and credential administration human-mode only.
//
// Deliberately absent: "project.system-one.*" (list, add, remove, set).
// Per-project System One instance allowlists (ptone/scion#8045 D1) control
// external model egress and are managed by project owners/admins and Hub
// admins only; agents must not be able to modify the allowlist.
//
// Deliberately absent: "exec-container" (show, set, clear). Human-mode only
// per ptone/scion#8812 decision D4 (the Hub endpoint is session-only too);
// cmd/platform_gate.go keeps it off Windows in v1.
//
// Deliberately absent: "inbox". Human-mode only (ptone/scion#9008, parent
// ptone/scion#8962): GET /api/v1/users/me/inbox/summary refuses agent
// identities, and agents already have scion decisions and scion notifications.
//
// Deliberately absent: "label". Human-mode only (ptone/scion#9521): agent
// identities are refused label edits on the Hub, and only human callers with
// agent.update may curate agent labels.
var agentAllowed = map[string]bool{
	"create":                      true,
	"delete":                      true,
	"expose":                      true, // acts only as the calling agent (ptone/scion#4728)
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
	"unexpose":                    true, // acts only as the calling agent (ptone/scion#4728)
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
	"schedule.events":             true,
	"schedule.events.output":      true,
	"schedule.cancel":             true,
	"schedule.create":             true,
	"schedule.create-recurring":   true,
	"schedule.pause":              true,
	"schedule.resume":             true,
	"schedule.run":                true,
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
	"templates.import-url":        true,
	"templates.update":            true,
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
	"template.import-url":         true,
	"template.update":             true,
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
	"skills.publish":              true, // own-project skill publish only; hub enforces project:skill:write (ptone/scion#8734)
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
	"artifact.list":                 true,
	"artifact.view":                 true,
	"artifact.comments":             true,
	"artifact.review-requests":      true,
	// artifact share, artifact grants, artifact transfer and artifact delete
	// are human-only and deliberately absent (ptone/scion#4529, ptone/scion#4579,
	// ptone/scion#9337): agents may not share, transfer or delete artifacts, or
	// manage their grants.
	// artifact request-review and artifact dismiss-review are also human-only
	// in Phase 1 (ptone/scion#9306): agents are never requested reviewers, and
	// agent requesters arrive in Phase 4.
	// Target-hub profiles and test identities (ptone/scion#4240).
	// These run in agent mode only against a target-hub profile whose
	// endpoint is not the agent's own hub; the run-time refusal is in
	// checkHubProfileGate and the profile commands (hub_profile.go).
	// Every other hub child stays removed, and hub.test-identity.delegate
	// (the human consent step) is human only.
	"hub":                        true,
	"hub.status":                 true,
	"hub.profile":                true,
	"hub.profile.add":            true,
	"hub.profile.list":           true,
	"hub.profile.show":           true,
	"hub.profile.remove":         true,
	"hub.test-identity":          true,
	"hub.test-identity.issue":    true,
	"hub.test-identity.token":    true,
	"hub.test-identity.list":     true,
	"hub.test-identity.delete":   true,
	"hub.test-identity.teardown": true,
	"hub.test-identity.web-code": true,
	// Issuer cleanup and provenance (Phase 3c, DEC-7): the same gate, so
	// an agent runner can verify a run by absence on a target hub.
	"hub.test-identity.cleanup":    true,
	"hub.test-identity.provenance": true,
	// issue-admin (X4b, DEC-7) issues through the profile's admin
	// delegation; it needs a profile in every mode.
	"hub.test-identity.issue-admin": true,

	// Project thread groups (ptone/scion#4424, design section 4 and
	// decision Q3): agents may list, read, create, rename and re-file
	// threads. conversation.group.delete is deliberately absent: only
	// users may delete a thread group, and the hub refuses an agent.
	"conversation.group":        true,
	"conversation.group.list":   true,
	"conversation.group.get":    true,
	"conversation.group.create": true,
	"conversation.group.rename": true,
	"conversation.group.add":    true,
	"conversation.group.remove": true,

	// Secret reads, writes, and agent self-service secret requests as the
	// calling agent (ptone/scion#5013, ptone/scion#8747, ptone/scion#9297).
	// Hub routes enforce AgentIdentity and hub.agent_secrets on --scope agent
	// and secret.request.*.
	"secret":                  true,
	"secret.get":              true,
	"secret.list":             true,
	"secret.set":              true,
	"secret.request":          true,
	"secret.request.list":     true,
	"secret.request.withdraw": true,

	// Command-request self-service list for agents (ptone/scion#9278 §4.5).
	// command.request.approve is human-only and intentionally omitted.
	"command":              true,
	"command.request":      true,
	"command.request.list": true,

	// Decision self-service as the calling agent (ptone/scion#7797,
	// decision Q1; withdraw ptone/scion#8190). Hub routes enforce
	// AgentIdentity and web.decisions.
	"decisions":          true,
	"decisions.list":     true,
	"decisions.show":     true,
	"decisions.record":   true,
	"decisions.withdraw": true,

	// Event triggers and webhook endpoints (ptone/scion#8028, Stage 1 §12):
	// Hub routes enforce hub.triggers and caller-scoped ownership for agents.
	"trigger":         true,
	"trigger.create":  true,
	"trigger.get":     true,
	"trigger.list":    true,
	"trigger.delete":  true,
	"trigger.pause":   true,
	"trigger.resume":  true,
	"trigger.render":  true,
	"trigger.history": true,
	// trigger.update is deliberately absent: agent callers cannot PATCH
	// trigger definitions (agentTriggerEditAllowed = false).
	"trigger.event":                 true,
	"trigger.event.list":            true,
	"trigger.event.get":             true,
	"trigger.webhook":               true,
	"trigger.webhook.create":        true,
	"trigger.webhook.get":           true,
	"trigger.webhook.list":          true,
	"trigger.webhook.update":        true,
	"trigger.webhook.disable":       true,
	"trigger.webhook.enable":        true,
	"trigger.webhook.rotate-secret": true,
	"trigger.webhook.delete":        true,
	"trigger.preset":                true,
	"trigger.preset.list":           true,
	"trigger.preset.show":           true,
	"trigger.preset.apply":          true,
	// trigger.github (status, settings, primary.*) are deliberately absent
	// (ptone/scion#8092, Stage 3 rev 1.11 §9.5): human-mode only, as the
	// project GitHub trigger settings route denies agent identities.

	// Project message policy read-only inspection (ptone/scion#8047,
	// maintainer decision m1-agent-cli v1): agents may inspect active project
	// policies with list and get. Authoring, deletion, dry-run testing, and
	// decision log queries (create, delete, test, decisions) are deliberately
	// absent and remain human-mode only.
	"message-policy":      true,
	"message-policy.list": true,
	"message-policy.get":  true,
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

	// With a hub profile selected, read only the global settings: a
	// profile command never reads local project settings.
	settingsPath := ""
	if hubProfileRequested() {
		settingsPath = "global"
	}
	settings, err := config.LoadSettings(settingsPath)
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
	hideOtherModeFlags(root, mode)
}

// cliModeFlagAnnotation marks a flag that only one CLI mode accepts. Its
// value is the mode (ModeHuman or ModeAgent). In the other mode the flag is
// hidden from help (applyModeRestrictions) and refused when set
// (checkFlagsForMode).
const cliModeFlagAnnotation = "scion_cli_mode"

// markFlagsForMode marks cmd's local flags names as accepted only in mode.
//
// Hiding is automatic (applyModeRestrictions), but refusing is opt-in: a
// command that marks flags must call checkFlagsForMode(cmd, resolveMode())
// at the top of its RunE, before acting on any flag (as runDecisionsList
// does). Without that call a marked flag set in the other mode is hidden
// from help yet still accepted.
func markFlagsForMode(cmd *cobra.Command, mode CLIMode, names ...string) {
	for _, name := range names {
		if err := cmd.Flags().SetAnnotation(name, cliModeFlagAnnotation, []string{string(mode)}); err != nil {
			panic(fmt.Sprintf("markFlagsForMode: %s: %v", cmd.CommandPath(), err))
		}
	}
}

// flagModeOf returns the only mode that accepts f, or "" for every mode.
func flagModeOf(f *pflag.Flag) CLIMode {
	if v := f.Annotations[cliModeFlagAnnotation]; len(v) == 1 {
		return CLIMode(v[0])
	}
	return ""
}

// hideOtherModeFlags hides, in every command under root, the flags marked
// for a mode other than mode.
func hideOtherModeFlags(root *cobra.Command, mode CLIMode) {
	root.Flags().VisitAll(func(f *pflag.Flag) {
		if m := flagModeOf(f); m != "" && m != mode {
			f.Hidden = true
		}
	})
	for _, child := range root.Commands() {
		hideOtherModeFlags(child, mode)
	}
}

// checkFlagsForMode returns a usage error naming the first flag set on cmd
// that mode does not accept.
func checkFlagsForMode(cmd *cobra.Command, mode CLIMode) error {
	var err error
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		if m := flagModeOf(f); m != "" && m != mode {
			err = newUsageError("--%s is available only in %s mode; this CLI is in %s mode", f.Name, m, mode)
		}
	})
	return err
}

func applyAgentMode(root *cobra.Command) {
	// Prevent Cobra's ExecuteC from lazily re-adding the built-in "completion"
	// command after removeCommands strips it in agent mode.
	root.CompletionOptions.DisableDefaultCmd = true
	removeCommands(root, "", func(path string) bool {
		return !agentAllowed[path]
	})
}

// removeCommands walks the command tree and removes commands where shouldRemove
// returns true. It processes children recursively before deciding whether to
// remove a parent. Surviving non-Runnable command groups get
// rejectUnknownSubcommands so that invoking a removed or unknown subcommand
// fails with an unknown-command error instead of printing parent help and
// exiting 0.
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

		hadSubcommands := child.HasSubCommands()
		if hadSubcommands {
			removeCommands(child, path, shouldRemove)
		}

		if shouldRemove(path) {
			parent.RemoveCommand(child)
			parent.Long = dropCommandLines(parent.Long, parent.CommandPath()+" "+name)
		} else if hadSubcommands && !child.Runnable() {
			rejectUnknownSubcommands(child)
		}
	}
}

// rejectUnknownSubcommands makes a non-Runnable command group Runnable with an
// Args validator (matching hubCmd, configCmd, artifactCmd, and messagePolicyCmd)
// so Cobra rejects removed or unknown subcommands with "unknown command" and a
// non-zero exit instead of falling through to parent help with exit 0. Bare
// "<group>" and "<group> help ..." still print help via pflag.ErrHelp before
// PersistentPreRunE runs.
func rejectUnknownSubcommands(cmd *cobra.Command) {
	cmd.Args = func(c *cobra.Command, args []string) error {
		if len(args) == 0 || args[0] == "help" {
			return pflag.ErrHelp
		}
		return cobra.NoArgs(c, args)
	}
	cmd.Run = func(*cobra.Command, []string) {}
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
