package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
			name:     "removed assistant mode from env falls back to human",
			envValue: "assistant",
			expected: ModeHuman,
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

// resetRemovedModeWarning lets a test observe the once-per-process warning
// for the removed assistant mode.
func resetRemovedModeWarning(t *testing.T) {
	t.Helper()
	removedModeWarnOnce = &sync.Once{}
	t.Cleanup(func() { removedModeWarnOnce = &sync.Once{} })
}

// The assistant CLI mode was removed. An old SCION_CLI_MODE=assistant must
// not restrict any command and must print exactly one warning, however many
// times the mode is resolved.
func TestRemovedAssistantMode_EnvIgnoredWithOneWarning(t *testing.T) {
	resetRemovedModeWarning(t)
	t.Setenv("SCION_CLI_MODE", "assistant")

	root := buildTestTree()
	before := collectCommandNames(root)
	stderr := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			assert.Equal(t, ModeHuman, resolveMode())
		}
		applyModeRestrictions(root, resolveMode())
	})

	assert.Equal(t, before, collectCommandNames(root),
		"the removed assistant mode must not remove any commands")
	for _, path := range []string{"hub.auth", "hub.token", "config.migrate", "cdw", "clean", "completion"} {
		assert.Contains(t, collectCommandNames(root), path)
	}
	assert.Equal(t, 1, strings.Count(stderr, "has been removed"), "stderr: %q", stderr)
	assert.Contains(t, stderr, `SCION_CLI_MODE="assistant"`)
}

// The same applies to a cli.mode: assistant value left in a settings file.
func TestRemovedAssistantMode_SettingIgnoredWithOneWarning(t *testing.T) {
	resetRemovedModeWarning(t)
	t.Setenv("SCION_CLI_MODE", "")
	_ = os.Unsetenv("SCION_CLI_MODE")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\ncli:\n  mode: assistant\n"), 0644))

	root := buildTestTree()
	before := collectCommandNames(root)
	stderr := captureStderr(t, func() {
		assert.Equal(t, ModeHuman, resolveMode())
		applyModeRestrictions(root, resolveMode())
	})

	assert.Equal(t, before, collectCommandNames(root))
	assert.Equal(t, 1, strings.Count(stderr, "has been removed"), "stderr: %q", stderr)
	assert.Contains(t, stderr, `cli.mode="assistant"`)
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
	cfg.AddCommand(&cobra.Command{Use: "cd-project"})
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
	hubToken.AddCommand(&cobra.Command{Use: "scopes"})
	hub.AddCommand(hubToken)

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
	hubSecret.AddCommand(&cobra.Command{Use: "migrate"})
	hubSecret.AddCommand(&cobra.Command{Use: "migrate-names"})
	hub.AddCommand(hubSecret)

	hubNotif := &cobra.Command{Use: "notifications"}
	hub.AddCommand(hubNotif)

	root.AddCommand(hub)

	// project with subcommands; canonical name is "project", "group" is a
	// legacy alias that must resolve to the same command.
	project := &cobra.Command{Use: "project", Aliases: []string{"group"}}
	for _, name := range []string{"init", "list", "prune", "reconnect"} {
		project.AddCommand(&cobra.Command{Use: name})
	}
	projectSA := &cobra.Command{Use: "service-accounts"}
	for _, name := range []string{"add", "list", "show", "mint", "verify", "remove"} {
		projectSA.AddCommand(&cobra.Command{Use: name})
	}
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
	applyModeRestrictions(root, resolveMode())
	after := collectCommandNames(root)
	assert.Equal(t, before, after, "human mode should not remove any commands")
}

func TestApplyModeRestrictions_Agent(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root, resolveMode())
	remaining := collectCommandNames(root)

	// These commands should be present in agent mode
	expected := []string{
		"create", "delete",
		"harness-config", "harness-config.install", "harness-config.list",
		"help",
		// hub and hub status stay for target-hub profiles (ptone/scion#4240); the
		// command refuses at run time without a target profile.
		"hub", "hub.status",
		"list", "logs", "look",
		"message",
		"notifications",
		"notifications.ack", "notifications.subscribe", "notifications.subscriptions",
		"notifications.unsubscribe", "notifications.update",
		// "project" itself stays allowed (mirrors the real agentAllowed map,
		// which permits bare "project" so "project.skills" routes through
		// it). Of its subcommands in this fake tree only the read-only
		// project service-accounts show is agent-allowed.
		"project",
		// Only the read-only show subcommand of project service-accounts.
		"project.service-accounts", "project.service-accounts.show",
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
		"hub.enable", "hub.disable", "hub.link", "hub.unlink", "hub.auth", "hub.token", "hub.brokers",
		"init", "messages", "restore", "server", "sync",
		// "project" itself remains (see expected list above), and of
		// project service-accounts only show is agent-allowed.
		"project.init", "project.list", "project.prune", "project.reconnect",
		"project.service-accounts.add", "project.service-accounts.list", "project.service-accounts.mint",
		"project.service-accounts.verify", "project.service-accounts.remove",
	}
	for _, cmd := range absent {
		assert.NotContains(t, remaining, cmd, "agent mode should remove %s", cmd)
	}
}

func TestApplyModeRestrictions_AgentConfigRemoved(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root, resolveMode())
	remaining := collectCommandNames(root)

	assert.NotContains(t, remaining, "config")
	assert.NotContains(t, remaining, "config.list")
	assert.NotContains(t, remaining, "config.get")
	assert.NotContains(t, remaining, "config.set")
}

func TestApplyModeRestrictions_AgentScheduleSubcommands(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root, resolveMode())
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
	for _, mode := range []string{"human", "agent"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", mode)
			root := buildTestTree()
			applyModeRestrictions(root, resolveMode())
			remaining := collectCommandNames(root)
			assert.Contains(t, remaining, "help")
		})
	}
}

func TestApplyModeRestrictions_CompletionRemovedInAgentMode(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root, resolveMode())
	remaining := collectCommandNames(root)
	assert.NotContains(t, remaining, "completion")

	t.Setenv("SCION_CLI_MODE", "human")
	root = buildTestTree()
	applyModeRestrictions(root, resolveMode())
	remaining = collectCommandNames(root)
	assert.Contains(t, remaining, "completion")
}

func TestApplyModeRestrictions_TemplateAlias(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root, resolveMode())
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

func TestAgentAllowedList(t *testing.T) {
	expectedAllowed := []string{
		"create", "delete", "list", "start", "stop", "suspend", "look", "logs",
		"message", "keys",
		"resume", "version",
		// Target-hub profiles and test identities (ptone/scion#4240): allowed, but
		// they refuse at run time without a target profile.
		"hub", "hub.status", "hub.profile", "hub.profile.add", "hub.profile.list",
		"hub.profile.show", "hub.profile.remove", "hub.test-identity",
		"hub.test-identity.issue", "hub.test-identity.token", "hub.test-identity.list",
		"hub.test-identity.delete", "hub.test-identity.teardown",
		"hub.test-identity.web-code", "hub.test-identity.issue-admin",
		"hub.test-identity.cleanup", "hub.test-identity.provenance",
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
		"trigger", "trigger.create", "trigger.list", "trigger.delete", "trigger.pause", "trigger.resume",
		"trigger.webhook", "trigger.webhook.create", "trigger.webhook.list", "trigger.webhook.delete",
	}
	for _, path := range expectedAllowed {
		assert.True(t, agentAllowed[path], "agentAllowed should contain %s", path)
	}

	notAllowed := []string{
		"attach", "broadcast", "restore", "sync", "clean", "cdw", "init",
		"completion", "config", "doctor", "messages",
		"server", "broker",
		"config.set", "config.unset", "config.validate", "config.migrate",
		"config.list", "config.get", "config.dir", "config.schema",
		"hub.enable", "hub.disable", "hub.link", "hub.unlink",
		"hub.auth", "hub.token", "hub.token.scopes", "hub.brokers",
		"hub.env", "hub.secret", "hub.notifications", "hub.projects",
		"hub.test-identity.delegate", "hub.users", "hub.users.invite", "hub.allow-list",
		"messages.read",
		"shared-dir.create", "shared-dir.remove",
	}
	for _, path := range notAllowed {
		assert.False(t, agentAllowed[path], "agentAllowed should NOT contain %s", path)
	}
}

// TestHubSecretMigrateNamesCmd_DeniedInAgentMode pins the CLI mode for the
// ptone/scion#2152 rename/migration command: human mode only. Agent mode
// denies it because hub.secret is absent from agentAllowed (only the
// target-hub profile commands of the hub subtree are allowed; an allow-list).
func TestHubSecretMigrateNamesCmd_DeniedInAgentMode(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := buildTestTree()
	applyModeRestrictions(root, resolveMode())
	remaining := collectCommandNames(root)
	assert.NotContains(t, remaining, "hub.secret.migrate-names",
		"hub secret migrate-names must be denied in agent mode")

	// Human mode (the default) keeps it.
	t.Setenv("SCION_CLI_MODE", "human")
	root = buildTestTree()
	applyModeRestrictions(root, resolveMode())
	remaining = collectCommandNames(root)
	assert.Contains(t, remaining, "hub.secret.migrate-names")
}

// TestHubSecretMigrateNamesCmd_DeniedAgainstRealTree runs the same assertion
// against the real command tree (rootCmd), so it also fails if the new
// command's registration path ever changes shape (e.g. moved to a different
// parent).
func TestHubSecretMigrateNamesCmd_DeniedAgainstRealTree(t *testing.T) {
	real := resolveCommandPath(rootCmd, "hub.secret.migrate-names")
	require.NotNil(t, real, "hub secret migrate-names must exist in the real command tree")

	root := &cobra.Command{Use: "scion"}
	hubReal := resolveCommandPath(rootCmd, "hub")
	require.NotNil(t, hubReal)
	root.AddCommand(cloneCommandShape(hubReal))

	t.Setenv("SCION_CLI_MODE", "agent")
	applyModeRestrictions(root, resolveMode())
	assert.Nil(t, resolveCommandPath(root, "hub.secret.migrate-names"),
		"hub secret migrate-names must be denied in agent mode")
}

func TestResolveModeEnvOverridesSettings(t *testing.T) {
	// Even if settings would return another mode, env var wins
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

// ptone/scion#1968, ptone/scion#8734: agents may browse the hub skill bank
// and publish project-scoped skills. The allowlisted skill paths must resolve
// to real commands, and every other mutating skill verb must stay out of agent
// mode.
func TestAgentAllowedSkillBrowse(t *testing.T) {
	for _, path := range []string{"skills", "skills.list", "skills.show", "skills.publish", "skill", "skill.list"} {
		assert.True(t, agentAllowed[path], "agentAllowed should contain %s", path)
		assert.NotNil(t, resolveCommandPath(rootCmd, path),
			"agentAllowed key %q must resolve to a real command", path)
	}
	for _, path := range []string{
		"skills.create", "skills.delete", "skills.deprecate",
		"skills.registries", "skills.registries.add", "skills.registries.update",
		"skills.registries.remove", "skills.registries.pin",
	} {
		assert.False(t, agentAllowed[path], "agentAllowed must NOT contain mutating skill verb %s", path)
	}
}

// ptone/scion#2257: a large-DM offload stub names `scion conversation
// get-message <ref> <id> --body` as its fetch command, so the recipient
// agent must be able to run it in agent mode.
func TestAgentAllowedConversationGetMessage(t *testing.T) {
	assert.True(t, agentAllowed["conversation.get-message"], "agentAllowed should contain conversation.get-message")
	assert.NotNil(t, resolveCommandPath(rootCmd, "conversation.get-message"),
		"agentAllowed key conversation.get-message must resolve to a real command")
}

// cloneCommandShape copies the names of cmd and its subcommands into a fresh
// tree, so mode filtering can run against the real command layout without
// mutating the shared rootCmd.
func cloneCommandShape(cmd *cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: cmd.Name(), Run: func(*cobra.Command, []string) {}}
	for _, child := range cmd.Commands() {
		c.AddCommand(cloneCommandShape(child))
	}
	return c
}

// ptone/scion#1968, ptone/scion#8734: runs agent-mode filtering over a copy of
// the real skills and skill subtrees and checks that exactly the allowed browse
// and publish verbs survive, so a mistyped allowlist key or a new mutating verb
// fails here.
func TestApplyModeRestrictions_AgentRealSkillTree(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	for _, name := range []string{"skills", "skill"} {
		real := resolveCommandPath(rootCmd, name)
		require.NotNil(t, real, "real command %q must exist", name)
		root.AddCommand(cloneCommandShape(real))
	}
	before := collectCommandNames(root)
	for _, mutating := range []string{"skills.create", "skills.delete", "skills.deprecate"} {
		require.Contains(t, before, mutating, "real tree should contain %s before filtering", mutating)
	}

	applyModeRestrictions(root, resolveMode())

	assert.Equal(t, []string{"skill", "skill.list", "skills", "skills.list", "skills.publish", "skills.show"},
		collectCommandNames(root),
		"agent mode must keep exactly the allowed skill browse and publish verbs")
}

// TestHubTokenScopesCommand_ModeRestricted pins the mode-availability
// decision for "scion hub token scopes" (ptone/scion#2122): although it is
// read-only, token management stays session-only, so it inherits "human
// only" from its parent hub.token rather than getting its own allowlist
// entry -- consistent with every other hub.token subcommand.
func TestHubTokenScopesCommand_ModeRestricted(t *testing.T) {
	require.NotNil(t, resolveCommandPath(rootCmd, "hub.token.scopes"),
		"hub.token.scopes must exist in the real command tree in human mode")

	t.Run("agent mode removes it: not in agentAllowed", func(t *testing.T) {
		assert.False(t, agentAllowed["hub.token.scopes"],
			"agentAllowed must NOT contain hub.token.scopes: it stays human-only like the rest of hub.token")
		root := &cobra.Command{Use: "scion"}
		real := resolveCommandPath(rootCmd, "hub")
		require.NotNil(t, real)
		root.AddCommand(cloneCommandShape(real))
		t.Setenv("SCION_CLI_MODE", "agent")
		applyModeRestrictions(root, resolveMode())
		assert.Nil(t, resolveCommandPath(root, "hub.token.scopes"))
	})
}

// TestAgentModeAllowsKeys covers the A.6 gap: pins, against the real command
// tree (not just the agentAllowed map TestAgentAllowedList already checks),
// that "keys" survives applyModeRestrictions in agent mode -- an agent
// running inside its own container must be able to send terminal
// keystrokes to another agent it created.
func TestAgentModeAllowsKeys(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	real := resolveCommandPath(rootCmd, "keys")
	require.NotNil(t, real, "real command %q must exist", "keys")
	root.AddCommand(cloneCommandShape(real))

	applyModeRestrictions(root, resolveMode())

	assert.Equal(t, []string{"keys"}, collectCommandNames(root),
		"agent mode must keep the keys command")
}

// TestSSHCommands_HumanOnly pins that ssh and ssh-config are absent in agent
// mode: they are not in agentAllowed and are removed from the real tree.
func TestSSHCommands_HumanOnly(t *testing.T) {
	for _, name := range []string{"ssh", "ssh-config", "vscode"} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, agentAllowed[name], "agentAllowed must not contain %s", name)
			real := resolveCommandPath(rootCmd, name)
			require.NotNil(t, real, "%s must exist in the real command tree", name)

			root := &cobra.Command{Use: "scion"}
			root.AddCommand(cloneCommandShape(real))
			t.Setenv("SCION_CLI_MODE", "agent")
			applyModeRestrictions(root, resolveMode())
			assert.Nil(t, resolveCommandPath(root, name), "%s must be removed in agent mode", name)

			root = &cobra.Command{Use: "scion"}
			root.AddCommand(cloneCommandShape(real))
			t.Setenv("SCION_CLI_MODE", "human")
			applyModeRestrictions(root, resolveMode())
			assert.NotNil(t, resolveCommandPath(root, name), "%s stays in human mode", name)
		})
	}
}

// The tunnel commands are human-only: they are not in agentAllowed, and
// agent-mode filtering over the real tunnel subtree removes all of it.
func TestTunnelCommandsAbsentInAgentMode(t *testing.T) {
	real := resolveCommandPath(rootCmd, "tunnel")
	require.NotNil(t, real, "tunnel must exist in the real command tree in human mode")
	for _, path := range []string{"tunnel", "tunnel.status"} {
		assert.False(t, agentAllowed[path], "agentAllowed must not contain %s", path)
	}
	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	root.AddCommand(cloneCommandShape(real))
	applyModeRestrictions(root, resolveMode())
	assert.Empty(t, collectCommandNames(root), "agent mode must remove every tunnel command")
}

// TestConfigUnset_HumanOnly: config unset is available in human mode only,
// like config set; agent mode removes it with the rest of config.
func TestConfigUnset_HumanOnly(t *testing.T) {
	assert.False(t, agentAllowed["config.unset"], "config unset must not be allowed in agent mode")
	assert.False(t, agentAllowed["config"], "config must not be allowed in agent mode")

	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	cfg := &cobra.Command{Use: "config"}
	cfg.AddCommand(&cobra.Command{Use: "unset", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(cfg)
	applyModeRestrictions(root, resolveMode())
	remaining := collectCommandNames(root)
	assert.NotContains(t, remaining, "config.unset")
	assert.NotContains(t, remaining, "config")

	found := false
	for _, c := range configCmd.Commands() {
		if c.Name() == "unset" {
			found = true
		}
	}
	assert.True(t, found, "config unset is registered (human mode)")
}

// TestServerOpenCmd_HumanModeOnly: 'scion server open' mints a web sign-in
// link with the server's dev token, so it exists only in human mode. It is
// on no allow-list, and agent mode removes it from the real command tree.
func TestServerOpenCmd_HumanModeOnly(t *testing.T) {
	assert.False(t, agentAllowed["server.open"], "server open must not be allowed in agent mode")
	assert.False(t, agentAllowed["server"], "the server command must not be allowed in agent mode")

	real := resolveCommandPath(rootCmd, "server.open")
	require.NotNil(t, real, "server open must exist in the real command tree")

	root := &cobra.Command{Use: "scion"}
	serverReal := resolveCommandPath(rootCmd, "server")
	require.NotNil(t, serverReal)
	root.AddCommand(cloneCommandShape(serverReal))
	t.Setenv("SCION_CLI_MODE", "agent")
	applyModeRestrictions(root, resolveMode())
	assert.Nil(t, resolveCommandPath(root, "server.open"), "server open must be removed in agent mode")

	human := &cobra.Command{Use: "scion"}
	human.AddCommand(cloneCommandShape(serverReal))
	t.Setenv("SCION_CLI_MODE", "human")
	applyModeRestrictions(human, resolveMode())
	assert.NotNil(t, resolveCommandPath(human, "server.open"), "human mode keeps server open")
}

// ptone/scion#8047 (decision m1-agent-cli v1): in agent mode, ONLY read-only
// inspection (message-policy, message-policy.list, message-policy.get) is
// available; create, delete, test, and decisions are removed.
func TestMessagePolicyCommands_AgentModeReadOnly(t *testing.T) {
	real := resolveCommandPath(rootCmd, "message-policy")
	require.NotNil(t, real, "message-policy must exist in the real command tree")

	for _, allowed := range []string{"message-policy", "message-policy.list", "message-policy.get"} {
		assert.True(t, agentAllowed[allowed], "agentAllowed must contain %s", allowed)
		assert.NotNil(t, resolveCommandPath(rootCmd, allowed), "%s must resolve to a real command", allowed)
	}
	for _, blocked := range []string{
		"message-policy.create",
		"message-policy.delete",
		"message-policy.test",
		"message-policy.decisions",
	} {
		assert.False(t, agentAllowed[blocked], "agentAllowed must NOT contain %s", blocked)
		assert.NotNil(t, resolveCommandPath(rootCmd, blocked), "%s must resolve to a real command in human mode", blocked)
	}

	root := &cobra.Command{Use: "scion"}
	root.AddCommand(cloneCommandShape(real))
	t.Setenv("SCION_CLI_MODE", "agent")
	applyModeRestrictions(root, resolveMode())

	assert.Equal(t,
		[]string{"message-policy", "message-policy.get", "message-policy.list"},
		collectCommandNames(root),
		"agent mode must keep ONLY message-policy, list, and get",
	)
}

// cloneCommandForModeExec copies cmd's structure, aliases, hidden flag, Args
// validator, and Runnable state into a fresh tree so applyModeRestrictions and
// Execute() can run against the real CLI layout without mutating rootCmd or
// invoking real network/filesystem handlers.
func cloneCommandForModeExec(src *cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:     src.Use,
		Aliases: append([]string(nil), src.Aliases...),
		Long:    src.Long,
		Hidden:  src.Hidden,
		Args:    src.Args,
	}
	if src.Runnable() {
		c.Run = func(*cobra.Command, []string) {}
	}
	for _, child := range src.Commands() {
		c.AddCommand(cloneCommandForModeExec(child))
	}
	return c
}

// TestAgentModeGatedSubcommandsFail covers ptone/scion#8365 over the real
// command tree: every subcommand removed by the agent-mode gate (whether its
// surviving parent is Runnable or a pure group like "project service-accounts")
// must fail with an unknown-command error instead of printing parent help and
// exiting 0. Bare group invocations and "<group> help" still print help.
func TestAgentModeGatedSubcommandsFail(t *testing.T) {
	type gatedCase struct {
		path       string
		parentPath string
		leaf       string
		args       []string
		wantParent string
	}

	var cases []gatedCase
	var walk func(parent *cobra.Command, parentPath string)
	walk = func(parent *cobra.Command, parentPath string) {
		for _, child := range parent.Commands() {
			name := child.Name()
			if name == "help" {
				continue
			}
			path := name
			if parentPath != "" {
				path = parentPath + "." + name
			}
			if !agentAllowed[path] {
				// Top-level removed command, or a removed child of an allowed parent.
				// Skip parents that intentionally accept positional arguments (e.g.
				// "scion message <recipient> <text>"), where an unknown first word
				// is a positional argument rather than a subcommand.
				if parentPath == "" || !acceptsPositionalArgs(parent) {
					args := strings.Split(path, ".")
					wantParent := "scion"
					if parentPath != "" {
						wantParent = "scion " + strings.ReplaceAll(parentPath, ".", " ")
					}
					cases = append(cases, gatedCase{
						path:       path,
						parentPath: parentPath,
						leaf:       name,
						args:       args,
						wantParent: wantParent,
					})
				}
				// Do not descend into a removed parent; the parent itself fails.
				continue
			}
			walk(child, path)
		}
	}
	walk(rootCmd, "")
	require.NotEmpty(t, cases, "real command tree must contain agent-gated commands")

	// Sanity-check that representative nested non-Runnable groups are in the table.
	var paths []string
	for _, tc := range cases {
		paths = append(paths, tc.path)
	}
	for _, mustInclude := range []string{
		"project.service-accounts.list",
		"project.service-accounts.add",
		"project.service-accounts.mint",
		"project.service-accounts.verify",
		"project.service-accounts.remove",
		"project.init",
		"secret.set",
		"shared-dir.create",
		"skills.create",
		"conversation.group.delete",
		"hub.test-identity.delegate",
		"artifact.share",
		"message-policy.create",
	} {
		require.Contains(t, paths, mustInclude, "table must cover %s", mustInclude)
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			root := cloneCommandForModeExec(rootCmd)
			root.SilenceErrors = true
			root.SilenceUsage = true
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)

			applyModeRestrictions(root, ModeAgent)

			root.SetArgs(tc.args)
			err := root.Execute()
			require.Error(t, err, "gated command %q must fail, not fall through to help", tc.path)
			assert.Contains(t, err.Error(), `unknown command "`+tc.leaf+`" for "`+tc.wantParent+`"`)
			assert.NotContains(t, err.Error(), "mode")
			assert.NotContains(t, out.String(), "Usage:", "gated command %q must not print parent help", tc.path)

			// For nested gated subcommands, "<parent> <gated> --help" must also fail.
			if tc.parentPath != "" {
				helpArgs := append(append([]string(nil), tc.args...), "--help")
				helpErr := unknownSubcommandHelpError(root, helpArgs)
				require.Error(t, helpErr, "gated command help %q must fail", helpArgs)
				assert.Contains(t, helpErr.Error(), `unknown command "`+tc.leaf+`" for "`+tc.wantParent+`"`)
			}
		})
	}

	// Surviving groups must still print help and exit 0 on bare invocation
	// or "<group> help", and reject arbitrary unknown subcommands.
	for _, groupPath := range []string{
		"project",
		"project.service-accounts",
		"secret",
		"shared-dir",
		"skills",
		"conversation.group",
		"hub.test-identity",
		"schedule",
		"templates",
		"decisions",
		"trigger",
	} {
		t.Run("group_"+groupPath, func(t *testing.T) {
			args := strings.Split(groupPath, ".")
			wantParent := "scion " + strings.ReplaceAll(groupPath, ".", " ")

			for _, sub := range []struct {
				name string
				argv []string
			}{
				{"bare", args},
				{"help", append(append([]string(nil), args...), "help")},
			} {
				root := cloneCommandForModeExec(rootCmd)
				root.SilenceErrors = true
				root.SilenceUsage = true
				var out bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&out)
				applyModeRestrictions(root, ModeAgent)

				root.SetArgs(sub.argv)
				require.NoError(t, root.Execute(), "%s %q should print help and succeed", sub.name, sub.argv)
				assert.Contains(t, out.String(), "Usage:", "%s %q should print usage", sub.name, sub.argv)
			}

			root := cloneCommandForModeExec(rootCmd)
			root.SilenceErrors = true
			root.SilenceUsage = true
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			applyModeRestrictions(root, ModeAgent)

			root.SetArgs(append(append([]string(nil), args...), "nosuch"))
			err := root.Execute()
			require.Error(t, err, "unknown subcommand under %q must fail", groupPath)
			assert.Contains(t, err.Error(), `unknown command "nosuch" for "`+wantParent+`"`)
			assert.NotContains(t, out.String(), "Usage:")
		})
	}
}

// TestAgentModeGatedSubcommandCLI runs the real CLI entrypoint (Execute) in a
// child process configured for agent mode via settings.yaml, confirming that
// gated subcommands under non-Runnable parents exit non-zero with an error on
// stderr instead of printing parent help and exiting 0.
func TestAgentModeGatedSubcommandCLI(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\ncli:\n  mode: agent\n"),
		0644,
	))

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "project_service_accounts_list",
			args:    []string{"project", "service-accounts", "list"},
			wantErr: `unknown command "list" for "scion project service-accounts"`,
		},
		{
			name:    "project_service_accounts_list_help",
			args:    []string{"project", "service-accounts", "list", "--help"},
			wantErr: `unknown command "list" for "scion project service-accounts"`,
		},
		{
			name:    "project_list",
			args:    []string{"project", "list"},
			wantErr: `unknown command "list" for "scion project"`,
		},
		{
			name:    "secret_set",
			args:    []string{"secret", "set"},
			wantErr: `unknown command "set" for "scion secret"`,
		},
		{
			name:    "completion",
			args:    []string{"completion"},
			wantErr: `unknown command "completion" for "scion"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runSSHCLI(t, dir, home, nil, tc.args...)
			assert.NotEqual(t, 0, res.code, "%q exited 0; stdout=%q stderr=%q", tc.args, res.stdout, res.stderr)
			assert.Contains(t, res.stderr, tc.wantErr, "%q", tc.args)
			assert.NotContains(t, res.stdout, "Usage:", "%q must not print parent help on stdout", tc.args)
		})
	}

	// Bare parent still prints help and exits 0, listing only allowed subcommands.
	res := runSSHCLI(t, dir, home, nil, "project", "service-accounts")
	assert.Equal(t, 0, res.code, "bare group failed: stderr=%q", res.stderr)
	assert.Contains(t, res.stdout, "Usage:")
	assert.Contains(t, res.stdout, "show")
	assert.NotContains(t, res.stdout, "scion project service-accounts list")
}
