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
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var adminSystemOneInstanceDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a Hub System One provider instance",
	Long: `Delete a named System One provider instance on the Hub and remove its
managed internal secret.`,
	Args: cobra.ExactArgs(1),
	RunE: runAdminSystemOneInstanceDelete,
}

func init() {
	adminSystemOneInstanceCmd.AddCommand(adminSystemOneInstanceDeleteCmd)
}

func runAdminSystemOneInstanceDelete(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("instance name is required")
	}

	svc, ctx, cancel, err := systemOneAdminService(cmd, adminSystemOneDefaultTimeout)
	if err != nil {
		return err
	}
	defer cancel()

	if err := svc.DeleteInstance(ctx, name); err != nil {
		return err
	}

	if isJSONOutput() {
		return writeJSONTo(cmd.OutOrStdout(), map[string]any{
			"deleted": true,
			"name":    name,
		})
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted System One instance %s\n", name)
	return nil
}
