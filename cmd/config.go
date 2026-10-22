// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	configGlobal      bool
	configUnsetGlobal bool
	configGetSource   bool
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage scion configuration settings",
	Long:  `View and modify settings for scion-agent. Settings are resolved from project (.scion/settings.yaml) and global (~/.scion/settings.yaml) locations.`,
	// Args/Run make this command Runnable so cobra validates subcommand
	// names: an unrecognized subcommand (e.g. a removed alias) returns an
	// "unknown command" error instead of silently falling through to this
	// command's own help with exit status 0.
	//
	// A bare "config" (no subcommand), and "config help ..." (cobra only
	// registers a real "help" subcommand on the root command, so under
	// "config" it would otherwise hit the same unknown-command path)
	// return pflag.ErrHelp instead of going through cobra.NoArgs. Cobra's
	// ExecuteC handles ErrHelp specially: it prints help and returns a nil
	// error *before* any PersistentPreRunE hook runs. That distinction
	// matters here because root's PersistentPreRunE requires an active
	// scion project for "config" (it's not in the exempt command list), so
	// letting a bare "config" fall through to a normal nil return would
	// turn "scion config" run outside a project into a "not in a scion
	// project" error instead of printing help.
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 || args[0] == "help" {
			return pflag.ErrHelp
		}
		return cobra.NoArgs(cmd, args)
	},
	// Must be Runnable (any Run/RunE) for the Args validator above to be
	// evaluated at all; the actual printing happens via the ErrHelp path.
	Run: func(cmd *cobra.Command, args []string) {},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all effective settings and where each comes from",
	Long: `List every setting "config set" accepts, plus the hub transport settings
and each hub connection's endpoint, with its effective value and the layer
it comes from: the embedded defaults, the global settings file, the
project's settings file, an environment variable, or the project-id file.

A setting with no value shows <unset>. The value of a credential-like
setting (for example hub.brokerToken) is masked. "config get <key>" reads
the same settings and gives the same value and source.

With --format json the output is an object keyed by setting:
{"<key>": {"value": "...", "source": "..."}, ...}.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		values, err := config.ResolveSettingValues(configReadProjectDir())
		if err != nil {
			return err
		}

		if isJSONOutput() {
			out := make(map[string]map[string]string, len(values))
			for _, v := range values {
				out[v.Key] = map[string]string{"value": v.Value, "source": v.Source}
			}
			return outputJSON(out)
		}

		w := cmd.OutOrStdout()
		_, _ = fmt.Fprintln(w, "Effective Settings:")
		for _, v := range values {
			_, _ = fmt.Fprintf(w, "  %s\n", formatSettingValue(v))
		}
		return nil
	},
}

// configReadProjectDir is the project whose settings config list and config
// get read: the resolved project, else "" (the current directory's project
// or, outside one, the global settings and defaults).
func configReadProjectDir() string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	return projectDir
}

// cliModeEnvUnsetNote is what config unset prints, instead of the
// still-set warning, when SCION_CLI_MODE sets cli.mode: the documented way
// back from agent mode (cliModeAgentNote) sets it for that command only, so
// the note says what cli.mode is without it. ok is false for any other key
// or variable, or when that value cannot be resolved.
func cliModeEnvUnsetNote(key, envName string) (note string, ok bool) {
	if key != "cli.mode" || envName != "SCION_CLI_MODE" {
		return "", false
	}
	without, err := config.ResolveSettingValueIgnoringEnv(configReadProjectDir(), key, envName)
	if err != nil {
		return "", false
	}
	value := "<unset>"
	if without.Value != "" {
		value = without.Value
		if without.Source != "" {
			value += " (" + without.Source + ")"
		}
	}
	return fmt.Sprintf("Note: SCION_CLI_MODE sets cli.mode for this command; without it, cli.mode is now %s.", value), true
}

// cliModeAgentNote is printed when config set writes cli.mode agent: agent
// mode removes the config commands, so config unset cannot undo it.
const cliModeAgentNote = `Note: agent mode removes the "config" commands, so "scion config unset cli.mode" is not available afterwards.
To return to human mode, run: SCION_CLI_MODE=human scion config unset cli.mode (add --global for the global setting), or remove "mode" from the "cli" section of the settings file.`

// formatValueSource is a setting's value with its source, as config list
// and config get --source print it: "<unset>" without a value, and
// "(unknown source)" for a value whose layer was not recorded (a key spelt
// in another case, for example).
func formatValueSource(v config.SettingValue) string {
	if v.Value == "" {
		return "<unset>"
	}
	source := v.Source
	if source == "" {
		source = "unknown source"
	}
	return fmt.Sprintf("%s  (%s)", v.Value, source)
}

// formatSettingValue is one config list line: "key: value  (source)", or
// "key: <unset>".
func formatSettingValue(v config.SettingValue) string {
	return v.Key + ": " + formatValueSource(v)
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Long: `Set a configuration value in the project's own settings, or in the global
settings (~/.scion/settings.yaml) with --global.

A key the settings file would accept but never read is refused and the file is
left unchanged: hub.token, hub.apiKey, hub.lastSyncedAt, bucket.* and
hub_connections.* at either layer, and server.auth.* at the project layer (read
only from the global settings). The value of a credential-like key (for example
hub.brokerToken) is masked in the output.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		value := args[1]

		targetPath := ""
		if !configGlobal {
			projectDir, err := config.GetResolvedProjectDir(projectPath)
			if err != nil {
				return fmt.Errorf("cannot set local setting: not inside a project or project path invalid: %w", err)
			}
			targetPath = projectDir
		}

		// Rejects keys that would be accepted and then never read at this
		// layer, before any settings file is read or written. The root
		// --global (or -g global) resolves the project to the global
		// directory itself, which is the global layer. The error never
		// contains the value.
		layerGlobal := configGlobal || config.IsGlobalProjectDir(targetPath)
		if err := config.ValidateConfigSetKey(key, layerGlobal); err != nil {
			return err
		}
		if err := config.UpdateSetting(targetPath, key, value, configGlobal); err != nil {
			return err
		}
		if key == "cli.mode" && value == "agent" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), cliModeAgentNote)
		}

		scope := "local"
		if configGlobal {
			scope = "global"
		}

		shown := configSetDisplayValue(key, value)
		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "config set",
				Message: fmt.Sprintf("Updated %s setting '%s' to '%s'", scope, key, shown),
				Details: map[string]interface{}{
					"key":   key,
					"value": shown,
					"scope": scope,
				},
			})
		}

		fmt.Printf("Updated %s setting '%s' to '%s'\n", scope, key, shown)
		return nil
	},
}

// configSetDisplayValue is the value "config set" prints after a write: the
// value itself, or a mask for a credential key (config.IsSecretSettingKey).
// An empty value (which clears the key) is shown as is.
func configSetDisplayValue(key, value string) string {
	if value != "" && config.IsSecretSettingKey(key) {
		return "********"
	}
	return value
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a specific configuration value",
	Long: `Get a specific configuration value.

Supported keys include the top-level settings (active_profile, default_template,
default_harness_config, workspace_path, image_registry, project_id, cli.autohelp,
cli.mode, hub.enabled, hub.linked, hub.endpoint, hub.local_only, hub.transport.mode,
hub.transport.audience, hub_connections.<name>.endpoint, hub.brokerId, hub.brokerToken
(masked), hub.brokerNickname),
plus dotted paths into a named entry of the profiles or runtimes maps:
profiles.<name>.<field> and runtimes.<name>.<field>, where <field> is one of that
entry's scalar (string, bool, or integer) settings, named by its settings.yaml key
(e.g. profiles.local.runtime, runtimes.kubernetes.namespace). Within a
profiles/runtimes entry, structured fields (maps, lists, nested objects — e.g. env,
volumes, secrets, harness_overrides) and credential-like field names are not
supported and return an error rather than a partial, reformatted, or unmasked value.

harness_configs.<name>.<field> is not supported: harness configs are normally
resolved from on-disk harness-config directories, not from settings.yaml alone.
Use "scion harness-config" to inspect them.

With --source the value is followed by the layer it comes from, the same
source "config list" shows. With --format json the output is
{"key": "...", "value": "...", "source": "..."}. The value of a
credential-like setting is masked.

Examples:
  scion config get active_profile
  scion config get --source hub.endpoint
  scion config get image_registry
  scion config get profiles.local.runtime
  scion config get runtimes.kubernetes.namespace`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]

		v, err := config.ResolveSettingValue(configReadProjectDir(), key)
		if err != nil {
			return err
		}

		if isJSONOutput() {
			return outputJSON(map[string]string{
				"key":    key,
				"value":  v.Value,
				"source": v.Source,
			})
		}

		w := cmd.OutOrStdout()
		if configGetSource {
			_, _ = fmt.Fprintln(w, formatValueSource(v))
			return nil
		}
		_, _ = fmt.Fprintln(w, v.Value)
		return nil
	},
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Remove a setting so it takes its value from the layer below",
	Long: `Remove a setting from the project's own settings, or from the global
settings (~/.scion/settings.yaml) with --global. The setting then takes its
value from the layers below (the global settings or the embedded defaults),
which is not the same as setting a boolean to false.

It accepts the keys "config set" accepts, with the same write rules. A key
that is not in the settings file changes nothing. An environment variable
that sets the key still applies after it is removed from the file.

Examples:
  scion config unset cli.autohelp
  scion config unset --global hub.endpoint`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]

		targetPath := ""
		if !configUnsetGlobal {
			projectDir, err := config.GetResolvedProjectDir(projectPath)
			if err != nil {
				return fmt.Errorf("cannot unset local setting: not inside a project or project path invalid: %w", err)
			}
			targetPath = projectDir
		}

		res, err := config.UnsetSetting(targetPath, key, configUnsetGlobal)
		if err != nil {
			return err
		}
		errOut := cmd.ErrOrStderr()
		for _, w := range res.MigrationWarnings {
			_, _ = fmt.Fprintf(errOut, "  Migration warning: %s\n", w)
		}

		scope := "local"
		if configUnsetGlobal || config.IsGlobalProjectDir(targetPath) {
			scope = "global"
		}

		// The value the setting has now, and where it comes from.
		now, nowErr := config.ResolveSettingValue(configReadProjectDir(), key)
		if nowErr == nil && strings.HasPrefix(now.Source, "env ") {
			envName := strings.TrimPrefix(now.Source, "env ")
			if note, ok := cliModeEnvUnsetNote(key, envName); ok {
				// The documented way back from agent mode sets
				// SCION_CLI_MODE for this one command (ptone/scion#5483).
				_, _ = fmt.Fprintln(errOut, note)
			} else {
				_, _ = fmt.Fprintf(errOut, "Warning: %s is still set by %s\n", key, envName)
			}
		}
		if nowErr == nil && strings.HasPrefix(now.Source, "project-id file ") {
			_, _ = fmt.Fprintf(errOut, "Note: %s still comes from %s\n", key, strings.TrimPrefix(now.Source, "project-id file "))
		}

		// A file of the edited layer that still sets the key: a spelling
		// unset does not edit (another case, a merge key, a legacy key), or
		// the in-repo file of a project whose settings live in its external
		// settings directory. Only a source the loader recorded
		// unambiguously is named (see config.ResolveSettingValue); a value
		// of unknown source is not attributed to any file.
		stillSetIn := ""
		if nowErr == nil {
			if file, ok := strings.CutPrefix(now.Source, configLayerSourcePrefix(scope)); ok {
				stillSetIn = file
			}
		}

		var msg string
		switch {
		case res.Removed && stillSetIn == "":
			msg = fmt.Sprintf("Removed %s setting '%s'", scope, key)
		case res.Removed:
			msg = fmt.Sprintf("Removed %s setting '%s' from %s, but it is still set in %s; edit that file by hand", scope, key, res.Path, stillSetIn)
		case stillSetIn != "" && stillSetIn != res.Path:
			msg = fmt.Sprintf("%s setting '%s' was not removed: it is still set in %s, which config unset does not edit; edit that file by hand", capitalize(scope), key, stillSetIn)
		case stillSetIn != "":
			msg = fmt.Sprintf("%s setting '%s' was not removed: it is still set in %s in a form config unset does not edit; edit that file by hand", capitalize(scope), key, stillSetIn)
		default:
			msg = fmt.Sprintf("%s setting '%s' is not set; nothing changed", capitalize(scope), key)
		}

		if isJSONOutput() {
			details := map[string]interface{}{
				"key":     key,
				"scope":   scope,
				"removed": res.Removed,
			}
			if stillSetIn != "" {
				details["still_set_in"] = stillSetIn
			}
			if nowErr == nil {
				details["value"] = now.Value
				details["source"] = now.Source
			}
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "config unset",
				Message: msg,
				Details: details,
			})
		}

		w := cmd.OutOrStdout()
		_, _ = fmt.Fprintln(w, msg)
		if nowErr == nil {
			_, _ = fmt.Fprintf(w, "Now: %s\n", formatSettingValue(now))
		}
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate settings files against the schema",
	Long: `Validate settings files against the JSON Schema for the declared schema version.

Checks both global (~/.scion/settings.yaml) and project-level (.scion/settings.yaml)
settings files. Reports whether each file uses the versioned or legacy format,
and lists any schema validation errors found.

Legacy settings files (without schema_version) are identified but not validated
against the schema — they use the pre-versioned format.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		type fileResult struct {
			Path     string   `json:"path"`
			Format   string   `json:"format"`
			Version  string   `json:"version,omitempty"`
			Valid    bool     `json:"valid"`
			Errors   []string `json:"errors,omitempty"`
			Warnings []string `json:"warnings,omitempty"`
		}

		var results []fileResult
		hasErrors := false

		// Collect settings file paths to validate.
		var filePaths []struct {
			dir   string
			label string
		}

		globalDir, _ := config.GetGlobalDir()
		if globalDir != "" {
			filePaths = append(filePaths, struct {
				dir   string
				label string
			}{globalDir, "global"})
		}

		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err == nil && projectDir != "" && projectDir != globalDir {
			filePaths = append(filePaths, struct {
				dir   string
				label string
			}{projectDir, "project"})
		}

		for _, fp := range filePaths {
			settingsPath := config.GetSettingsPath(fp.dir)
			if settingsPath == "" {
				continue
			}

			data, err := os.ReadFile(settingsPath)
			if err != nil {
				results = append(results, fileResult{
					Path:   settingsPath,
					Format: "unknown",
					Errors: []string{fmt.Sprintf("failed to read file: %v", err)},
				})
				hasErrors = true
				continue
			}

			version, isLegacy := config.DetectSettingsFormat(data)

			r := fileResult{
				Path:  settingsPath,
				Valid: true,
			}

			switch {
			case version != "":
				r.Format = "versioned"
				r.Version = version

				validationErrors, err := config.ValidateSettings(data, version)
				r.Warnings = append(r.Warnings, config.SettingsWarnings(data, version)...)
				if err != nil {
					r.Valid = false
					r.Errors = []string{err.Error()}
					hasErrors = true
				} else if len(validationErrors) > 0 {
					r.Valid = false
					hasErrors = true
					for _, ve := range validationErrors {
						r.Errors = append(r.Errors, ve.Error())
					}
				}

			case isLegacy:
				r.Format = "legacy"
				r.Warnings = append(r.Warnings, "Legacy settings format detected. Run 'scion config migrate' to update.")

			default:
				r.Format = "minimal"
				r.Warnings = append(r.Warnings, "No schema_version found. File may be empty or use an unrecognized format.")
			}

			results = append(results, r)
		}

		if len(results) == 0 {
			if isJSONOutput() {
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "config validate",
					Message: "No settings files found.",
				})
			}
			fmt.Println("No settings files found.")
			return nil
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"files": results,
				"valid": !hasErrors,
			})
		}

		for _, r := range results {
			fmt.Printf("%s (%s", r.Path, r.Format)
			if r.Version != "" {
				fmt.Printf(" v%s", r.Version)
			}
			fmt.Println(")")

			if r.Valid && len(r.Warnings) == 0 {
				fmt.Println("  Valid")
			}

			for _, w := range r.Warnings {
				fmt.Printf("  WARNING: %s\n", w)
			}

			for _, e := range r.Errors {
				fmt.Printf("  ERROR: %s\n", e)
			}

			fmt.Println()
		}

		if hasErrors {
			return fmt.Errorf("validation failed")
		}
		return nil
	},
}

var (
	configMigrateDryRun bool
	configMigrateGlobal bool
)

var configMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate configuration to the versioned format",
	Long: `Migrate configuration files to the versioned settings format.

Migrates legacy settings.yaml files (global and project-level) to the versioned
format with schema_version. If a server.yaml exists alongside the settings file,
it is automatically merged under the 'server' key.

Use --dry-run to preview changes without writing files.
Use --global to migrate only the global settings file.

Examples:
  # Preview all settings migration
  scion config migrate --dry-run

  # Migrate all legacy settings files
  scion config migrate

  # Migrate only global settings
  scion config migrate --global`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSettingsMigration()
	},
}

// runSettingsMigration handles general settings migration (legacy → versioned).
func runSettingsMigration() error {
	type dirEntry struct {
		dir   string
		label string
	}

	var dirs []dirEntry

	// Always include global dir
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}
	dirs = append(dirs, dirEntry{dir: globalDir, label: "global"})

	// Include project dir if applicable and --global was not specified
	if !configMigrateGlobal {
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err == nil && projectDir != "" && projectDir != globalDir {
			dirs = append(dirs, dirEntry{dir: projectDir, label: "project"})
		}
	}

	var results []*config.MigrationResult
	var labels []string
	var migrationErr error

	for _, d := range dirs {
		result, err := config.MigrateSettingsFile(d.dir, configMigrateDryRun)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error migrating %s settings (%s): %v\n", d.label, d.dir, err)
			migrationErr = err
			continue
		}
		results = append(results, result)
		labels = append(labels, d.label)
	}

	// Check if everything was skipped
	allSkipped := true
	for _, r := range results {
		if !r.Skipped {
			allSkipped = false
			break
		}
	}

	if isJSONOutput() {
		type jsonResult struct {
			Label  string                  `json:"label"`
			Result *config.MigrationResult `json:"result"`
		}
		var jsonResults []jsonResult
		for i, r := range results {
			jsonResults = append(jsonResults, jsonResult{Label: labels[i], Result: r})
		}
		return outputJSON(map[string]interface{}{
			"results":            jsonResults,
			"nothing_to_migrate": allSkipped && migrationErr == nil,
		})
	}

	if allSkipped && migrationErr == nil {
		fmt.Println("Nothing to migrate. All settings files are already versioned or absent.")
		return nil
	}

	for i, r := range results {
		label := labels[i]
		if r.Skipped {
			fmt.Printf("%s (%s): skipped — %s\n", label, r.Path, r.SkipReason)
			continue
		}

		if configMigrateDryRun {
			fmt.Printf("%s (%s): would migrate from %s format\n", label, r.Path, r.Format)
		} else {
			fmt.Printf("%s: migrated %s\n", label, r.Path)
			if r.BackupPath != "" {
				fmt.Printf("  Backup: %s\n", r.BackupPath)
			}
		}

		if r.ServerMigrated {
			if configMigrateDryRun {
				fmt.Println("  Would merge server.yaml into settings.yaml under 'server' key")
			} else {
				fmt.Println("  Merged server.yaml into settings.yaml under 'server' key")
				if r.ServerBackupPath != "" {
					fmt.Printf("  Server backup: %s\n", r.ServerBackupPath)
				}
			}
		}

		if r.StateMigrated {
			if configMigrateDryRun {
				fmt.Println("  Would migrate hub.lastSyncedAt to state.yaml")
			} else {
				fmt.Println("  Migrated hub.lastSyncedAt to state.yaml")
			}
		}

		if len(r.Warnings) > 0 {
			fmt.Println("  Deprecation warnings:")
			for _, w := range r.Warnings {
				fmt.Printf("    - %s\n", w)
			}
		}
	}

	return migrationErr
}

var configDirCmd = &cobra.Command{
	Use:   "dir",
	Short: "Print the path to the project config directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return err
		}
		configDir := config.GetProjectConfigDir(projectDir)
		if isJSONOutput() {
			return outputJSON(map[string]string{"path": configDir})
		}
		fmt.Println(configDir)
		return nil
	},
}

var configCdConfigCmd = &cobra.Command{
	Use:   "cd-config",
	Short: "Open a shell in the project config directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return err
		}
		configDir := config.GetProjectConfigDir(projectDir)
		return execShellInDir(configDir)
	},
}

var configCdProjectCmd = &cobra.Command{
	Use:   "cd-project",
	Short: "Open a shell in the project workspace directory",
	Long: `Open a shell in the project workspace directory.

For external projects (non-git), navigates to the workspace path stored in settings.
For git projects, navigates to the project root (parent of the .scion directory).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return err
		}

		workspacePath, err := resolveProjectWorkspace(projectDir)
		if err != nil {
			return err
		}

		return execShellInDir(workspacePath)
	},
}

// resolveProjectWorkspace returns the workspace path for a project given its config dir.
// For external projects (under ~/.scion/project-configs/), the workspace path is read from settings.
// For git projects, the workspace is the parent directory of the .scion config dir.
func resolveProjectWorkspace(configDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	projectConfigsDir := filepath.Join(home, config.GlobalDir, "project-configs")

	if strings.HasPrefix(configDir, projectConfigsDir) {
		// External project — workspace path is recorded in settings
		settings, err := config.LoadSettings(configDir)
		if err != nil {
			return "", fmt.Errorf("failed to load project settings: %w", err)
		}
		if settings.WorkspacePath == "" {
			return "", fmt.Errorf("no workspace path found in project settings; the project may be orphaned")
		}
		if _, err := os.Stat(settings.WorkspacePath); err != nil {
			return "", fmt.Errorf("workspace path does not exist: %s", settings.WorkspacePath)
		}
		return settings.WorkspacePath, nil
	}

	// Git project — workspace is the project root (parent of .scion dir)
	parent := filepath.Dir(configDir)
	if _, err := os.Stat(parent); err != nil {
		return "", fmt.Errorf("project workspace does not exist: %s", parent)
	}
	return parent, nil
}

// configLayerSourcePrefix is the setting-source prefix of the settings
// layer config unset edits: "global " or "project ".
func configLayerSourcePrefix(scope string) string {
	if scope == "global" {
		return "global "
	}
	return "project "
}

// capitalize upper-cases the first letter of s.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// execShellInDir changes to the given directory and replaces the current process
// with a new interactive shell session.
func execShellInDir(dir string) error {
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("error changing directory to '%s': %v", dir, err)
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}

	shellPath, err := exec.LookPath(shell)
	if err != nil {
		if _, statErr := os.Stat("/bin/bash"); statErr == nil {
			shellPath = "/bin/bash"
		} else if _, statErr := os.Stat("/bin/sh"); statErr == nil {
			shellPath = "/bin/sh"
		} else {
			return fmt.Errorf("error finding shell '%s': %v", shell, err)
		}
	}

	return syscall.Exec(shellPath, []string{shell}, os.Environ())
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configUnsetCmd)
	configCmd.AddCommand(configValidateCmd)
	configCmd.AddCommand(configMigrateCmd)
	configCmd.AddCommand(configDirCmd)
	configCmd.AddCommand(configCdConfigCmd)
	configCmd.AddCommand(configCdProjectCmd)

	configSetCmd.Flags().BoolVar(&configGlobal, "global", false, "Set configuration globally (~/.scion/settings.yaml)")
	configUnsetCmd.Flags().BoolVar(&configUnsetGlobal, "global", false, "Unset the setting in the global settings (~/.scion/settings.yaml)")
	configGetCmd.Flags().BoolVar(&configGetSource, "source", false, "Also print where the value comes from")
	configMigrateCmd.Flags().BoolVar(&configMigrateDryRun, "dry-run", false, "Preview changes without writing files")
	configMigrateCmd.Flags().BoolVar(&configMigrateGlobal, "global", false, "Migrate only the global settings file")
}
