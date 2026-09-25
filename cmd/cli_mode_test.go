package cmd

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveMode(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected CLIMode
	}{
		{
			name:     "default is human when unset",
			envValue: "",
			expected: ModeHuman,
		},
		{
			name:     "assistant mode from env",
			envValue: "assistant",
			expected: ModeAssistant,
		},
		{
			name:     "agent mode from env",
			envValue: "agent",
			expected: ModeAgent,
		},
		{
			name:     "human mode from env",
			envValue: "human",
			expected: ModeHuman,
		},
		{
			name:     "unrecognized value defaults to human",
			envValue: "bogus",
			expected: ModeHuman,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv("SCION_CLI_MODE", tt.envValue)
			} else {
				t.Setenv("SCION_CLI_MODE", "")
				_ = os.Unsetenv("SCION_CLI_MODE")
			}
			mode := resolveMode()
			assert.Equal(t, tt.expected, mode)
		})
	}
}

// buildTestTree creates a command tree mimicking a subset of the real scion CLI
// for testing mode filtering.
func buildTestTree() *cobra.Command {
	root := &cobra.Command{Use: "scion"}

	// Top-level commands
	for _, name := range []string{
		"broadcast",
		"create", "delete", "list", "start", "stop", "attach", "look", "logs",
		"message", "resume", "restore", "sync", "clean", "cdw", "init",
		"doctor", "version",
	} {
		root.AddCommand(&cobra.Command{Use: name})
	}

	// messages with subcommand
	messages := &cobra.Command{Use: "messages"}
	messages.AddCommand(&cobra.Command{Use: "read"})
	root.AddCommand(messages)

	// config with subcommands
	cfg := &cobra.Command{Use: "config"}
	for _, name := range []string{"list", "set", "get", "validate", "migrate", "dir", "cd-config", "schema"} {
		cfg.AddCommand(&cobra.Command{Use: name})
	}
	// cd-project's canonical name is "cd-project"; "cd-grove" is a legacy alias.
	cfg.AddCommand(&cobra.Command{Use: "cd-project", Aliases: []string{"cd-grove"}})
	root.AddCommand(cfg)

	// hub with subcommands
	hub := &cobra.Command{Use: "hub"}
	hub.AddCommand(&cobra.Command{Use: "status"})
	hub.AddCommand(&cobra.Command{Use: "enable"})
	hub.AddCommand(&cobra.Command{Use: "disable"})
	hub.AddCommand(&cobra.Command{Use: "link"})
	hub.AddCommand(&cobra.Command{Use: "unlink"})

	hubAuth := &cobra.Command{Use: "auth"}
	hubAuth.AddCommand(&cobra.Command{Use: "login"})
	hubAuth.AddCommand(&cobra.Command{Use: "logout"})
	hub.AddCommand(hubAuth)

	hubToken := &cobra.Command{Use: "token"}
	hubToken.AddCommand(&cobra.Command{Use: "create"})
	hubToken.AddCommand(&cobra.Command{Use: "list"})
	hubToken.AddCommand(&cobra.Command{Use: "revoke"})
	hubToken.AddCommand(&cobra.Command{Use: "delete"})
	hub.AddCommand(hubToken)

	hubGrv := &cobra.Command{Use: "groves"}
	hubGrv.AddCommand(&cobra.Command{Use: "info"})
	hubGrv.AddCommand(&cobra.Command{Use: "delete"})
	hub.AddCommand(hubGrv)

	hubBrk := &cobra.Command{Use: "brokers"}
	hubBrk.AddCommand(&cobra.Command{Use: "info"})
	hubBrk.AddCommand(&cobra.Command{Use: "delete"})
	hub.AddCommand(hubBrk)

	hubEnv := &cobra.Command{Use: "env"}
	hubEnv.AddCommand(&cobra.Command{Use: "set"})
	hubEnv.AddCommand(&cobra.Command{Use: "get"})
	hub.AddCommand(hubEnv)

	hubSecret := &cobra.Command{Use: "secret"}
	hubSecret.AddCommand(&cobra.Command{Use: "set"})
	hubSecret.AddCommand(&cobra.Command{Use: "get"})
	hub.AddCommand(hubSecret)

	hubNotif := &cobra.Command{Use: "notifications"}
	hub.AddCommand(hubNotif)

	root.AddCommand(hub)

	// project with subcommands; canonical name is "project", "grove" (and
	// "group") are legacy aliases that must resolve to the same command.
	project := &cobra.Command{Use: "project", Aliases: []string{"grove", "group"}}
	for _, name := range []string{"init", "list", "prune", "reconnect"} {
		project.AddCommand(&cobra.Command{Use: name})
	}
	projectSA := &cobra.Command{Use: "service-accounts"}
	projectSA.AddCommand(&cobra.Command{Use: "add"})
	projectSA.AddCommand(&cobra.Command{Use: "list"})
	project.AddCommand(projectSA)
	root.AddCommand(project)

	// server with subcommands
	server := &cobra.Command{Use: "server"}
	for _, name := range []string{"start", "stop", "restart", "status", "install"} {
		server.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(server)

	// broker with subcommands
	broker := &cobra.Command{Use: "broker"}
	for _, name := range []string{"register", "deregister", "start", "provide", "withdraw"} {
		broker.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(broker)

	// schedule with subcommands
	sched := &cobra.Command{Use: "schedule"}
	for _, name := range []string{"list", "get", "cancel", "create", "create-recurring", "pause", "resume", "delete", "history"} {
		sched.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(sched)

	// notifications with subcommands
	notif := &cobra.Command{Use: "notifications"}
	for _, name := range []string{"ack", "subscribe", "unsubscribe", "update", "subscriptions"} {
		notif.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(notif)

	// shared-dir with subcommands
	sd := &cobra.Command{Use: "shared-dir"}
	for _, name := range []string{"list", "create", "remove", "info"} {
		sd.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(sd)

	// templates with subcommands
	templates := &cobra.Command{Use: "templates"}
	for _, name := range []string{"list", "show", "create", "delete", "clone", "update-default", "import", "sync", "push", "pull", "status"} {
		templates.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(templates)

	// template (singular alias)
	template := &cobra.Command{Use: "template"}
	for _, name := range []string{"list", "show", "delete", "clone", "import", "sync", "push", "pull", "status"} {
		template.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(template)

	// harness-config
	hc := &cobra.Command{Use: "harness-config"}
	for _, name := range []string{"list", "set", "get", "install"} {
		hc.AddCommand(&cobra.Command{Use: name})
	}
	root.AddCommand(hc)

	// Built-in commands that should always be kept
	root.AddCommand(&cobra.Command{Use: "help"})
	root.AddCommand(&cobra.Command{Use: "completion"})

	return root
}

// collectCommandNames returns a sorted list of dot-separated command paths
// in the given command tree (excluding the root itself).
func collectCommandNames(root *cobra.Command) []string {
	var names []string
	var walk func(cmd *cobra.Command, prefix string)
	walk = func(cmd *cobra.Command, prefix string) {
		for _, child := range cmd.Commands() {
			path := child.Name()
			if prefix != "" {
				path = prefix + "." + child.Name()
			}
			names = append(names, path)
			walk(child, path)
		}
	}
	walk(root, "")
	sort.Strings(names)
	return names
}

func TestApplyModeRestrictions_Human(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "human")
	root := buildTestTree()
	before := collectCommandNames(root)
	applyModeRestrictions(root)
	after := collectCommandNames(root)
	assert.Equal(t, before, after, "human mode should not remove any commands")
}

func TestApplyModeRestrictions_Assistant(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "assistant")
	root := buildTestTree()
	applyModeRestrictions(root)
	remaining := collectCommandNames(root)

	// These commands should be removed
	removed := []string{
		"hub.auth", "hub.auth.login", "hub.auth.logout",
		"hub.token", "hub.token.create", "hub.token.list", "hub.token.revoke", "hub.token.delete",
		"project.reconnect",
		"config.migrate", "config.cd-config", "config.cd-project",
		"cdw",
		"clean",
	}
	for _, cmd := range removed {
		assert.NotContains(t, remaining, cmd, "assistant mode should remove %s", cmd)
	}

	// These commands should still be present
	present := []string{
		"create", "delete", "list", "start", "stop", "attach",
		"config", "config.list", "config.set", "config.get", "config.validate", "config.dir", "config.schema",
		"hub", "hub.status", "hub.enable", "hub.disable", "hub.link", "hub.unlink",
		"hub.groves", "hub.brokers", "hub.env", "hub.secret",
		"project", "project.init", "project.list", "project.prune", "project.service-accounts",
		"server", "server.start", "server.stop",
		"broker",
		"templates",
		"help", "completion",
	}
	for _, cmd := range present {
		assert.Contains(t, remaining, cmd, "assistant mode should keep %s", cmd)
	}
}

func TestApplyModeRestrictions_Agent(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root)
	remaining := collectCommandNames(root)

	// These commands should be present in agent mode
	expected := []string{
		"create", "delete",
		"harness-config", "harness-config.install", "harness-config.list",
		"help",
		"list", "logs", "look",
		"message",
		"notifications",
		"notifications.ack", "notifications.subscribe", "notifications.subscriptions",
		"notifications.unsubscribe", "notifications.update",
		// "project" itself stays allowed (mirrors the real agentAllowed map,
		// which permits bare "project" so "project.skills" routes through
		// it), even though none of its subcommands in this fake tree are
		// agent-allowed and so are stripped below.
		"project",
		"resume",
		"schedule", "schedule.cancel", "schedule.create", "schedule.create-recurring",
		"schedule.delete", "schedule.get", "schedule.history", "schedule.list",
		"schedule.pause", "schedule.resume",
		"shared-dir", "shared-dir.info", "shared-dir.list",
		"start", "stop",
		"template",
		"template.clone", "template.delete", "template.import",
		"template.list", "template.pull", "template.push",
		"template.show", "template.status", "template.sync",
		"templates",
		"templates.clone", "templates.create", "templates.delete", "templates.import",
		"templates.list", "templates.pull", "templates.push",
		"templates.show", "templates.status", "templates.sync",
		"templates.update-default",
		"version",
	}
	assert.Equal(t, expected, remaining)

	// These should be removed
	absent := []string{
		"attach", "broadcast", "broker", "cdw", "clean", "completion", "config", "doctor",
		"hub",
		"init", "messages", "restore", "server", "sync",
		// "project" itself remains (see expected list above), but none of
		// its subcommands are agent-allowed.
		"project.init", "project.list", "project.prune", "project.reconnect",
		"project.service-accounts", "project.service-accounts.add", "project.service-accounts.list",
	}
	for _, cmd := range absent {
		assert.NotContains(t, remaining, cmd, "agent mode should remove %s", cmd)
	}
}

func TestApplyModeRestrictions_AgentConfigRemoved(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root)
	remaining := collectCommandNames(root)

	assert.NotContains(t, remaining, "config")
	assert.NotContains(t, remaining, "config.list")
	assert.NotContains(t, remaining, "config.get")
	assert.NotContains(t, remaining, "config.set")
}

func TestApplyModeRestrictions_AgentScheduleSubcommands(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root)
	remaining := collectCommandNames(root)

	assert.Contains(t, remaining, "schedule")
	assert.Contains(t, remaining, "schedule.list")
	assert.Contains(t, remaining, "schedule.get")
	assert.Contains(t, remaining, "schedule.cancel")
	assert.Contains(t, remaining, "schedule.history")

	assert.Contains(t, remaining, "schedule.create")
	assert.Contains(t, remaining, "schedule.create-recurring")
	assert.Contains(t, remaining, "schedule.pause")
	assert.Contains(t, remaining, "schedule.resume")
	assert.Contains(t, remaining, "schedule.delete")
}

func TestApplyModeRestrictions_HelpAlwaysKept(t *testing.T) {
	for _, mode := range []string{"human", "assistant", "agent"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", mode)
			root := buildTestTree()
			applyModeRestrictions(root)
			remaining := collectCommandNames(root)
			assert.Contains(t, remaining, "help")
		})
	}
}

func TestApplyModeRestrictions_CompletionRemovedInAgentMode(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root)
	remaining := collectCommandNames(root)
	assert.NotContains(t, remaining, "completion")

	t.Setenv("SCION_CLI_MODE", "assistant")
	root = buildTestTree()
	applyModeRestrictions(root)
	remaining = collectCommandNames(root)
	assert.Contains(t, remaining, "completion")
}

func TestApplyModeRestrictions_TemplateAlias(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root)
	remaining := collectCommandNames(root)

	assert.Contains(t, remaining, "template")
	assert.Contains(t, remaining, "template.list")
	assert.Contains(t, remaining, "template.show")
	assert.Contains(t, remaining, "templates")
	assert.Contains(t, remaining, "templates.list")
	assert.Contains(t, remaining, "templates.show")
}

func TestRemoveCommands_DoesNotPanicOnEmptyTree(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	removeCommands(root, "", func(path string) bool { return true })
}

func TestAssistantDeniedList(t *testing.T) {
	expectedDenied := []string{
		"hub.auth", "hub.token",
		"project.reconnect",
		"config.migrate", "config.cd-config", "config.cd-project",
		"cdw", "clean",
	}
	for _, path := range expectedDenied {
		assert.True(t, assistantDenied[path], "assistantDenied should contain %s", path)
	}

	notDenied := []string{
		"create", "list", "hub.status", "config.list", "config.set",
		"server", "project.init", "templates",
	}
	for _, path := range notDenied {
		assert.False(t, assistantDenied[path], "assistantDenied should NOT contain %s", path)
	}
}

func TestAgentAllowedList(t *testing.T) {
	expectedAllowed := []string{
		"create", "delete", "list", "start", "stop", "suspend", "look", "logs",
		"message",
		"resume", "version",
		"notifications",
		"schedule", "schedule.list", "schedule.get", "schedule.cancel", "schedule.history",
		"schedule.create", "schedule.create-recurring", "schedule.pause", "schedule.resume", "schedule.delete",
		"shared-dir", "shared-dir.list", "shared-dir.info",
		"templates", "templates.list", "templates.show", "templates.create",
		"templates.clone", "templates.delete", "templates.update-default",
		"templates.import", "templates.sync", "templates.push", "templates.pull", "templates.status",
		"template", "template.list", "template.show", "template.clone",
		"template.delete", "template.import", "template.sync",
		"template.push", "template.pull", "template.status",
		"harness-config", "harness-config.list", "harness-config.show", "harness-config.install",
		"harness-config.sync", "harness-config.push", "harness-config.pull",
		"harness-config.delete", "harness-config.reset", "harness-config.upgrade",
	}
	for _, path := range expectedAllowed {
		assert.True(t, agentAllowed[path], "agentAllowed should contain %s", path)
	}

	notAllowed := []string{
		"attach", "broadcast", "restore", "sync", "clean", "cdw", "init",
		"completion", "config", "doctor", "hub", "messages",
		"server", "broker", "grove",
		"config.set", "config.validate", "config.migrate",
		"config.list", "config.get", "config.dir", "config.schema",
		"hub.enable", "hub.disable", "hub.link", "hub.unlink",
		"hub.auth", "hub.token", "hub.groves", "hub.brokers",
		"hub.env", "hub.secret", "hub.status", "hub.notifications",
		"messages.read",
		"shared-dir.create", "shared-dir.remove",
	}
	for _, path := range notAllowed {
		assert.False(t, agentAllowed[path], "agentAllowed should NOT contain %s", path)
	}
}

func TestResolveModeEnvOverridesSettings(t *testing.T) {
	// Even if settings would return "assistant", env var wins
	t.Setenv("SCION_CLI_MODE", "agent")
	mode := resolveMode()
	require.Equal(t, ModeAgent, mode)
}

// resolveCommandPath walks a dot-separated command path (as produced by
// removeCommands, using each command's canonical Name(), never an Aliases
// entry) starting at root, and returns the command it points to, or nil if
// no such command exists.
func resolveCommandPath(root *cobra.Command, path string) *cobra.Command {
	current := root
	for _, seg := range strings.Split(path, ".") {
		var next *cobra.Command
		for _, child := range current.Commands() {
			if child.Name() == seg {
				next = child
				break
			}
		}
		if next == nil {
			return nil
		}
		current = next
	}
	return current
}

// TestAssistantDeniedKeysResolveToRealCommands guards against the denylist
// drifting out of sync with the real command tree: a key built from a stale
// or aliased name (e.g. "grove.reconnect" instead of the canonical
// "project.reconnect") silently never matches anything in removeCommands,
// so the command it names is never actually hidden. This walks every
// assistantDenied key against the real rootCmd tree (populated by this
// package's init() functions) and fails if any key does not resolve.
func TestAssistantDeniedKeysResolveToRealCommands(t *testing.T) {
	for path := range assistantDenied {
		t.Run(path, func(t *testing.T) {
			cmd := resolveCommandPath(rootCmd, path)
			assert.NotNil(t, cmd,
				"assistantDenied key %q does not resolve to any command in the real root command tree "+
					"(paths must use each command's canonical Name(), not an Aliases entry)", path)
		})
	}
}

// ptone/scion#1968: agents may browse the hub skill bank read-only. The
// allowlisted skill paths must resolve to real commands, and every mutating
// skill verb must stay out of agent mode.
func TestAgentAllowedSkillBrowse(t *testing.T) {
	for _, path := range []string{"skills", "skills.list", "skills.show", "skill", "skill.list"} {
		assert.True(t, agentAllowed[path], "agentAllowed should contain %s", path)
		assert.NotNil(t, resolveCommandPath(rootCmd, path),
			"agentAllowed key %q must resolve to a real command", path)
	}
	for _, path := range []string{
		"skills.create", "skills.publish", "skills.delete", "skills.deprecate",
		"skills.registries", "skills.registries.add", "skills.registries.update",
		"skills.registries.remove", "skills.registries.pin",
	} {
		assert.False(t, agentAllowed[path], "agentAllowed must NOT contain mutating skill verb %s", path)
	}
}
