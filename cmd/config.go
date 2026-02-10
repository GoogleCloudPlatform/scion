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
	"sort"

	"github.com/ptone/scion-agent/pkg/config"
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

		settings, err := config.LoadSettings(projectDir)
		if err != nil {
			return err
		}

		val, err := config.GetSettingValue(settings, key)
		if err != nil {
			return err
		}

		fmt.Println(val)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)

	configSetCmd.Flags().BoolVar(&configGlobal, "global", false, "Set configuration globally (~/.scion/settings.json)")
	// configListCmd.Flags().BoolVar(&configGlobal, "global", false, "List global configuration only") // Not strictly required by design but useful
}
