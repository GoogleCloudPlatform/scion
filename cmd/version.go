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

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/spf13/cobra"
)

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of scion",
	Long:  `All software has versions. This is scion's`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isJSONOutput() {
			return outputJSON(map[string]string{
				"version":   version.Version,
				"commit":    version.Commit,
				"buildTime": version.BuildTime,
				"short":     version.Short(),
			})
		}
		if resolveMode() != ModeAgent {
			fmt.Println(util.GetBanner())
		}
		fmt.Println(version.Get())
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
