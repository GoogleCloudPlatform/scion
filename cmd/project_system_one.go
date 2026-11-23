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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

var (
	projectSystemOneProject string
	projectSystemOneClear   bool
)

var projectSystemOneClientLoader = func() (hubclient.SystemOneService, string, error) {
	settings, client, err := loadHubClient()
	if err != nil {
		return nil, "", err
	}
	projectID, err := resolveProjectID(settings, projectSystemOneProject)
	if err != nil {
		return nil, "", err
	}
	return client.SystemOne(), projectID, nil
}

// projectSystemOneCmd manages a project's System One hub instance allowlist
// (ptone/scion#8045, D1).
var projectSystemOneCmd = &cobra.Command{
	Use:   "system-one",
	Short: "Manage project System One provider instance allowlist",
	Long: `View and manage the hub System One provider instances allowed for a project.

Every project's allowlist is empty by default on both hosted and workstation
hubs. A project owner, project admin, or Hub admin must explicitly add hub
instance names before project rules may send content to those instances.`,
}

var projectSystemOneListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"get"},
	Short:   "List allowed System One instances for a project",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		svc, projectID, err := projectSystemOneClientLoader()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(projectSystemOneCmdCtx(cmd), 30*time.Second)
		defer cancel()

		resp, err := svc.GetProjectInstances(ctx, projectID)
		if err != nil {
			return fmt.Errorf("failed to get project System One instances: %w", err)
		}
		return printProjectSystemOneInstances(cmd, resp)
	},
}

var projectSystemOneAddCmd = &cobra.Command{
	Use:   "add <instance-name>",
	Short: "Allow a hub System One instance for a project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		if name == "" {
			return newUsageError("instance name is required")
		}
		svc, projectID, err := projectSystemOneClientLoader()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(projectSystemOneCmdCtx(cmd), 30*time.Second)
		defer cancel()

		resp, err := svc.AddProjectInstance(ctx, projectID, name)
		if err != nil {
			return fmt.Errorf("failed to add System One instance %q: %w", name, err)
		}
		return printProjectSystemOneInstances(cmd, resp)
	},
}

var projectSystemOneRemoveCmd = &cobra.Command{
	Use:     "remove <instance-name>",
	Aliases: []string{"rm"},
	Short:   "Remove a hub System One instance from a project's allowlist",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		if name == "" {
			return newUsageError("instance name is required")
		}
		svc, projectID, err := projectSystemOneClientLoader()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(projectSystemOneCmdCtx(cmd), 30*time.Second)
		defer cancel()

		resp, err := svc.RemoveProjectInstance(ctx, projectID, name)
		if err != nil {
			return fmt.Errorf("failed to remove System One instance %q: %w", name, err)
		}
		return printProjectSystemOneInstances(cmd, resp)
	},
}

var projectSystemOneSetCmd = &cobra.Command{
	Use:   "set [instance-name...]",
	Short: "Replace a project's allowed System One instance list",
	Long: `Replace the project's System One instance allowlist with the given
instance names. Pass --clear with no arguments to empty the allowlist.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if projectSystemOneClear && len(args) > 0 {
			return newUsageError("cannot combine --clear with instance names")
		}
		if !projectSystemOneClear && len(args) == 0 {
			return newUsageError("provide at least one instance name, or pass --clear to empty the allowlist")
		}
		instances := make([]string, 0, len(args))
		for _, a := range args {
			trimmed := strings.TrimSpace(a)
			if trimmed == "" {
				return newUsageError("instance name must not be empty")
			}
			instances = append(instances, trimmed)
		}
		svc, projectID, err := projectSystemOneClientLoader()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(projectSystemOneCmdCtx(cmd), 30*time.Second)
		defer cancel()

		resp, err := svc.SetProjectInstances(ctx, projectID, instances)
		if err != nil {
			return fmt.Errorf("failed to set project System One instances: %w", err)
		}
		return printProjectSystemOneInstances(cmd, resp)
	},
}

func projectSystemOneCmdCtx(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func printProjectSystemOneInstances(cmd *cobra.Command, resp *hubclient.ProjectSystemOneInstancesResponse) error {
	out := cmd.OutOrStdout()
	if hubOutputJSON || outputFormat == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	if len(resp.Instances) == 0 {
		_, err := fmt.Fprintf(out, "Project %s has no allowed System One instances.\n", resp.ProjectID)
		return err
	}
	if _, err := fmt.Fprintf(out, "Allowed System One instances for project %s:\n", resp.ProjectID); err != nil {
		return err
	}
	for _, name := range resp.Instances {
		if _, err := fmt.Fprintf(out, "  - %s\n", name); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	projectCmd.AddCommand(projectSystemOneCmd)
	projectSystemOneCmd.AddCommand(projectSystemOneListCmd)
	projectSystemOneCmd.AddCommand(projectSystemOneAddCmd)
	projectSystemOneCmd.AddCommand(projectSystemOneRemoveCmd)
	projectSystemOneCmd.AddCommand(projectSystemOneSetCmd)

	projectSystemOneCmd.PersistentFlags().StringVar(&projectSystemOneProject, "project", "", "Project ID or slug")
	projectSystemOneSetCmd.Flags().BoolVar(&projectSystemOneClear, "clear", false, "Clear the project's allowed System One instances")
}
