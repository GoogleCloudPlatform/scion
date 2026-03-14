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
	"sort"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

var configGlobal bool

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage scion configuration settings",
	Long:  `View and modify settings for scion-agent. Settings are resolved from grove (.scion/settings.json) and global (~/.scion/settings.json) locations.`,
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all effective settings",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Resolve grove path
		projectDir, err := config.GetResolvedProjectDir(grovePath)
		// If we are not in a grove, we might only show global settings or defaults
		// We handle the case where grove resolution fails gracefully for global listing?
		// But LoadSettings expects grovePath. If empty, it loads Global + Defaults.

		var effective *config.Settings
		if err == nil {
			effective, err = config.LoadSettings(projectDir)
		} else {
			// Try loading just global/defaults
			effective, err = config.LoadSettings("")
		}

		if err != nil {
			return err
		}

		// Flatten struct for display
		m := config.GetSettingsMap(effective)

		if isJSONOutput() {
			return outputJSON(m)
		}

		// Sort keys
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		fmt.Println("Effective Settings:")
		for _, k := range keys {
			val := m[k]
			if val == "" {
				val = "<empty>"
			}
			fmt.Printf("  %s: %s\n", k, val)
		}

		// Also show sources?
		// For now just effective settings as per design doc example.
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		value := args[1]

		targetPath := ""
		if !configGlobal {
			projectDir, err := config.GetResolvedProjectDir(grovePath)
			if err != nil {
				return fmt.Errorf("cannot set local setting: not inside a grove or grove path invalid: %w", err)
			}
			targetPath = projectDir
		}

		if err := config.UpdateSetting(targetPath, key, value, configGlobal); err != nil {
			return err
		}

		scope := "local"
		if configGlobal {
			scope = "global"
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "config set",
				Message: fmt.Sprintf("Updated %s setting '%s' to '%s'", scope, key, value),
				Details: map[string]interface{}{
					"key":   key,
					"value": value,
					"scope": scope,
				},
			})
		}

		fmt.Printf("Updated %s setting '%s' to '%s'\n", scope, key, value)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a specific configuration value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]

		projectDir, _ := config.GetResolvedProjectDir(grovePath)
		// Even if error, we can try loading defaults/global

		// Try versioned settings first (supports all v1 keys like image_registry)
		vs, err := config.LoadVersionedSettings(projectDir)
		if err != nil {
			return err
		}

		val, err := config.GetVersionedSettingValue(vs, key)
		if err != nil {
			return err
		}

		if isJSONOutput() {
			return outputJSON(map[string]string{
				"key":   key,
				"value": val,
			})
		}

		fmt.Println(val)
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate settings files against the schema",
	Long: `Validate settings files against the JSON Schema for the declared schema version.

Checks both global (~/.scion/settings.yaml) and grove-level (.scion/settings.yaml)
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

		projectDir, err := config.GetResolvedProjectDir(grovePath)
		if err == nil && projectDir != "" && projectDir != globalDir {
			filePaths = append(filePaths, struct {
				dir   string
				label string
			}{projectDir, "grove"})
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

Migrates legacy settings.yaml files (global and grove-level) to the versioned
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

	// Include grove dir if applicable and --global was not specified
	if !configMigrateGlobal {
		projectDir, err := config.GetResolvedProjectDir(grovePath)
		if err == nil && projectDir != "" && projectDir != globalDir {
			dirs = append(dirs, dirEntry{dir: projectDir, label: "grove"})
		}
	}

	var results []*config.MigrationResult
	var labels []string
	var migrationErr error

	for _, d := range dirs {
		result, err := config.MigrateSettingsFile(d.dir, configMigrateDryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error migrating %s settings (%s): %v\n", d.label, d.dir, err)
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
			Label  string                `json:"label"`
			Result *config.MigrationResult `json:"result"`
		}
		var jsonResults []jsonResult
		for i, r := range results {
			jsonResults = append(jsonResults, jsonResult{Label: labels[i], Result: r})
		}
		return outputJSON(map[string]interface{}{
			"results":          jsonResults,
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
	Short: "Print the path to the grove config directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(grovePath)
		if err != nil {
			return err
		}
		if isJSONOutput() {
			return outputJSON(map[string]string{"path": projectDir})
		}
		fmt.Println(projectDir)
		return nil
	},
}

var configCdConfigCmd = &cobra.Command{
	Use:   "cd-config",
	Short: "Open a shell in the grove config directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(grovePath)
		if err != nil {
			return err
		}
		return execShellInDir(projectDir)
	},
}

var configCdGroveCmd = &cobra.Command{
	Use:   "cd-grove",
	Short: "Open a shell in the grove workspace directory",
	Long: `Open a shell in the grove workspace directory.

For external groves (non-git), navigates to the workspace path stored in settings.
For git groves, navigates to the project root (parent of the .scion directory).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(grovePath)
		if err != nil {
			return err
		}

		workspacePath, err := resolveGroveWorkspace(projectDir)
		if err != nil {
			return err
		}

		return execShellInDir(workspacePath)
	},
}

// resolveGroveWorkspace returns the workspace path for a grove given its config dir.
// For external groves (under ~/.scion/grove-configs/), the workspace path is read from settings.
// For git groves, the workspace is the parent directory of the .scion config dir.
func resolveGroveWorkspace(configDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	groveConfigsDir := filepath.Join(home, config.GlobalDir, "grove-configs")

	if strings.HasPrefix(configDir, groveConfigsDir) {
		// External grove — workspace path is recorded in settings
		settings, err := config.LoadSettings(configDir)
		if err != nil {
			return "", fmt.Errorf("failed to load grove settings: %w", err)
		}
		if settings.WorkspacePath == "" {
			return "", fmt.Errorf("no workspace path found in grove settings; the grove may be orphaned")
		}
		if _, err := os.Stat(settings.WorkspacePath); err != nil {
			return "", fmt.Errorf("workspace path does not exist: %s", settings.WorkspacePath)
		}
		return settings.WorkspacePath, nil
	}

	// Git grove — workspace is the project root (parent of .scion dir)
	parent := filepath.Dir(configDir)
	if _, err := os.Stat(parent); err != nil {
		return "", fmt.Errorf("grove workspace does not exist: %s", parent)
	}
	return parent, nil
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
	configCmd.AddCommand(configValidateCmd)
	configCmd.AddCommand(configMigrateCmd)
	configCmd.AddCommand(configDirCmd)
	configCmd.AddCommand(configCdConfigCmd)
	configCmd.AddCommand(configCdGroveCmd)

	configSetCmd.Flags().BoolVar(&configGlobal, "global", false, "Set configuration globally (~/.scion/settings.json)")
	configMigrateCmd.Flags().BoolVar(&configMigrateDryRun, "dry-run", false, "Preview changes without writing files")
	configMigrateCmd.Flags().BoolVar(&configMigrateGlobal, "global", false, "Migrate only the global settings file")
}
