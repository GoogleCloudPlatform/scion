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

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
	"github.com/spf13/cobra"
)

var checkUpdate bool

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of scion",
	Long:  `All software has versions. This is scion's`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			info      *update.UpdateInfo
			updateErr error
		)
		if checkUpdate {
			info, updateErr = update.CheckForUpdate(cmd.Context(), version.Version)
		}

		if isJSONOutput() {
			result := map[string]interface{}{
				"version":   version.Version,
				"commit":    version.Commit,
				"buildTime": version.BuildTime,
				"short":     version.Short(),
			}

			if checkUpdate {
				if updateErr != nil {
					result["updateError"] = updateErr.Error()
				} else {
					result["channel"] = info.Channel
					result["updateAvailable"] = info.UpdateAvailable
					result["latestVersion"] = info.LatestVersion
					result["releaseUrl"] = info.ReleaseURL
				}
			}

			return outputJSON(result)
		}
		if resolveMode() != ModeAgent {
			fmt.Println(util.GetBanner())
		}
		fmt.Println(version.Get())

		if checkUpdate {
			if updateErr != nil {
				fmt.Fprintf(os.Stderr, "\nCould not check for updates: %v\n", updateErr)
			} else if info.UpdateAvailable {
				fmt.Printf("\nUpdate available: %s\n", info.LatestVersion)
				if info.ReleaseURL != "" {
					fmt.Printf("  %s\n", info.ReleaseURL)
				}
			} else if info.Channel != "" {
				fmt.Printf("\nYou are running the latest %s version.\n", info.Channel)
			} else {
				fmt.Println("\nUpdate checking is not supported for development or unknown builds.")
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	versionCmd.Flags().BoolVar(&checkUpdate, "check", false, "Check for available updates")
}
