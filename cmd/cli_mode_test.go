package cmd

import (
	"os"
	"sort"
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
				os.Unsetenv("SCION_CLI_MODE")
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
	for _, name := range []string{"list", "set", "get", "validate", "migrate", "dir", "cd-config", "cd-grove", "schema"} {
		cfg.AddCommand(&cobra.Command{Use: name})
	}
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

	// grove with subcommands
	grove := &cobra.Command{Use: "grove"}
	for _, name := range []string{"init", "list", "prune", "reconnect"} {
		grove.AddCommand(&cobra.Command{Use: name})
	}
	groveSA := &cobra.Command{Use: "service-accounts"}
	groveSA.AddCommand(&cobra.Command{Use: "add"})
	groveSA.AddCommand(&cobra.Command{Use: "list"})
	grove.AddCommand(groveSA)
	root.AddCommand(grove)

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
		"grove.reconnect",
		"config.migrate", "config.cd-config", "config.cd-grove",
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
		"grove", "grove.init", "grove.list", "grove.prune", "grove.service-accounts",
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
		"help",
		"list", "logs", "look",
		"message",
		"notifications",
		"notifications.ack", "notifications.subscribe", "notifications.subscriptions",
		"notifications.unsubscribe", "notifications.update",
		"resume",
		"schedule", "schedule.cancel", "schedule.get", "schedule.history", "schedule.list",
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
		"attach", "broker", "cdw", "clean", "completion", "config", "doctor",
		"grove", "harness-config", "hub",
		"init", "messages", "restore", "server", "sync",
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

	assert.NotContains(t, remaining, "schedule.create")
	assert.NotContains(t, remaining, "schedule.create-recurring")
	assert.NotContains(t, remaining, "schedule.pause")
	assert.NotContains(t, remaining, "schedule.resume")
	assert.NotContains(t, remaining, "schedule.delete")
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
		"grove.reconnect",
		"config.migrate", "config.cd-config", "config.cd-grove",
		"cdw", "clean",
	}
	for _, path := range expectedDenied {
		assert.True(t, assistantDenied[path], "assistantDenied should contain %s", path)
	}

	notDenied := []string{
		"create", "list", "hub.status", "config.list", "config.set",
		"server", "grove.init", "templates",
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
		"shared-dir", "shared-dir.list", "shared-dir.info",
		"templates", "templates.list", "templates.show", "templates.create",
		"templates.clone", "templates.delete", "templates.update-default",
		"templates.import", "templates.sync", "templates.push", "templates.pull", "templates.status",
		"template", "template.list", "template.show", "template.clone",
		"template.delete", "template.import", "template.sync",
		"template.push", "template.pull", "template.status",
	}
	for _, path := range expectedAllowed {
		assert.True(t, agentAllowed[path], "agentAllowed should contain %s", path)
	}

	notAllowed := []string{
		"attach", "restore", "sync", "clean", "cdw", "init",
		"completion", "config", "doctor", "hub", "messages",
		"server", "broker", "grove",
		"harness-config",
		"config.set", "config.validate", "config.migrate",
		"config.list", "config.get", "config.dir", "config.schema",
		"hub.enable", "hub.disable", "hub.link", "hub.unlink",
		"hub.auth", "hub.token", "hub.groves", "hub.brokers",
		"hub.env", "hub.secret", "hub.status", "hub.notifications",
		"messages.read",
		"schedule.create", "schedule.create-recurring", "schedule.delete",
		"schedule.pause", "schedule.resume",
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
