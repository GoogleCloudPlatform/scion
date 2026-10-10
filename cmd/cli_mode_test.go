package cmd

import (
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
	removedModeWarnOnce = sync.Once{}
	t.Cleanup(func() { removedModeWarnOnce = sync.Once{} })
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
		"server", "broker",
		"config.set", "config.validate", "config.migrate",
		"config.list", "config.get", "config.dir", "config.schema",
		"hub.enable", "hub.disable", "hub.link", "hub.unlink",
		"hub.auth", "hub.token", "hub.token.scopes", "hub.brokers",
		"hub.env", "hub.secret", "hub.status", "hub.notifications",
		"messages.read",
		"shared-dir.create", "shared-dir.remove",
	}
	for _, path := range notAllowed {
		assert.False(t, agentAllowed[path], "agentAllowed should NOT contain %s", path)
	}
}

// TestHubSecretMigrateNamesCmd_DeniedInAgentMode pins the CLI mode for the
// ptone/scion#2152 rename/migration command: human mode only. Agent mode
// denies it because the entire "hub" subtree is absent from agentAllowed
// (an allow-list).
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

// ptone/scion#1968: runs agent-mode filtering over a copy of the real skills
// and skill subtrees and checks that exactly the read-only browse verbs
// survive, so a mistyped allowlist key or a new mutating verb fails here.
func TestApplyModeRestrictions_AgentRealSkillTree(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	for _, name := range []string{"skills", "skill"} {
		real := resolveCommandPath(rootCmd, name)
		require.NotNil(t, real, "real command %q must exist", name)
		root.AddCommand(cloneCommandShape(real))
	}
	before := collectCommandNames(root)
	for _, mutating := range []string{"skills.create", "skills.publish", "skills.delete", "skills.deprecate"} {
		require.Contains(t, before, mutating, "real tree should contain %s before filtering", mutating)
	}

	applyModeRestrictions(root, resolveMode())

	assert.Equal(t, []string{"skill", "skill.list", "skills", "skills.list", "skills.show"},
		collectCommandNames(root),
		"agent mode must keep exactly the read-only skill browse verbs")
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
