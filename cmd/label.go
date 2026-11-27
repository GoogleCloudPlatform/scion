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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var labelRemoveKeys []string

var labelCmd = &cobra.Command{
	Use:   "label <agent> [key=value ...]",
	Short: "Add, update, or remove labels on an existing agent",
	Long: `Add, update, or remove labels on an existing agent using merge semantics.

Unmentioned labels are preserved. Concurrent edits from multiple callers are
merged server-side without lost updates.

Examples:
  scion label my-agent env=staging team=core
  scion label my-agent --remove team
  scion label my-agent env=prod --remove tmp --project my-project`,
	Args: cobra.MinimumNArgs(1),
	RunE: runLabel,
}

func init() {
	rootCmd.AddCommand(labelCmd)
	labelCmd.Flags().StringArrayVarP(&labelRemoveKeys, "remove", "r", nil, "Label key to remove (repeatable)")
}

func runLabel(cmd *cobra.Command, args []string) error {
	agentName := api.Slugify(args[0])
	if agentName == "" {
		return asUsageError(fmt.Errorf("agent name must not be empty"))
	}

	req, err := buildLabelUpdateRequest(args[1:], labelRemoveKeys)
	if err != nil {
		return asUsageError(err)
	}

	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	updated, err := hubCtx.Client.ProjectAgents(projectID).UpdateLabels(ctx, agentName, req)
	if err != nil {
		return wrapHubError(err)
	}

	if isJSONOutput() {
		return outputJSON(updated)
	}

	name := updated.Name
	if name == "" {
		name = agentName
	}
	if len(updated.Labels) == 0 {
		fmt.Printf("Labels for agent %q: (none)\n", name)
		return nil
	}
	fmt.Printf("Labels for agent %q: %s\n", name, formatLabelsSorted(updated.Labels))
	return nil
}

func buildLabelUpdateRequest(setArgs, removeKeys []string) (*hubclient.UpdateAgentLabelsRequest, error) {
	if len(setArgs) == 0 && len(removeKeys) == 0 {
		return nil, fmt.Errorf("specify at least one key=value label to set or --remove <key>")
	}

	setMap, err := parseLabels(setArgs)
	if err != nil {
		return nil, err
	}

	seenRemove := make(map[string]bool, len(removeKeys))
	dedupRemove := make([]string, 0, len(removeKeys))
	for _, k := range removeKeys {
		if k == "" {
			return nil, fmt.Errorf("label key to remove must not be empty")
		}
		if _, overlap := setMap[k]; overlap {
			return nil, fmt.Errorf("label key %q cannot be both set and removed in the same command", k)
		}
		if !seenRemove[k] {
			seenRemove[k] = true
			dedupRemove = append(dedupRemove, k)
		}
	}

	return &hubclient.UpdateAgentLabelsRequest{
		Set:    setMap,
		Remove: dedupRemove,
	}, nil
}

// formatLabelsSorted renders labels as sorted comma-separated key=value pairs.
func formatLabelsSorted(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}
