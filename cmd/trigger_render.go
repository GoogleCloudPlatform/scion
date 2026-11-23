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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var triggerRenderEventRef string

var triggerRenderCmd = &cobra.Command{
	Use:   "render <id|name>",
	Short: "Preview rendered templates for a trigger against a stored event",
	Long: `Compile and render every template slot of a trigger against a stored trigger event without firing or creating any resources.

Untrusted event variables used in each slot are highlighted, and terminal
control and bidirectional override characters are escaped in human output.
Use --json to output the raw render response DTO.`,
	Args: cobra.ExactArgs(1),
	RunE: runTriggerRender,
}

func runTriggerRender(cmd *cobra.Command, args []string) error {
	eventRef := strings.TrimSpace(triggerRenderEventRef)
	if eventRef == "" {
		return newUsageError("--event is required")
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullTriggerID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}
	fullEventID, err := resolveTriggerEventID(ctx, hubCtx, projectID, eventRef)
	if err != nil {
		return err
	}

	resp, err := hubCtx.Client.Triggers(projectID).Render(ctx, fullTriggerID, fullEventID)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to render trigger: %w", formatTriggerAPIError(err)))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(resp)
	}

	printTriggerRenderResult(resp)
	return nil
}

func printTriggerRenderResult(resp *hubclient.TriggerRenderResponse) {
	if resp == nil {
		fmt.Println("No render output.")
		return
	}

	printedAny := false
	printSlot := func(name string, slot *hubclient.TriggerRenderSlotResult, keepLines bool) {
		if slot == nil {
			return
		}
		if printedAny {
			fmt.Println()
		}
		printedAny = true
		fmt.Printf("Slot: %s\n", triggerTerminalText(name, false))
		if len(slot.Used) > 0 {
			parts := make([]string, len(slot.Used))
			for i, u := range slot.Used {
				p := triggerTerminalText(u.Path, false)
				if u.Class == "untrusted" {
					parts[i] = p + " [UNTRUSTED]"
				} else {
					parts[i] = p + " [" + triggerTerminalText(u.Class, false) + "]"
				}
			}
			fmt.Printf("  Used: %s\n", strings.Join(parts, ", "))
		}
		rendered := triggerTerminalText(slot.Rendered, keepLines)
		if keepLines && strings.Contains(rendered, "\n") {
			fmt.Println("  Rendered:")
			for _, line := range strings.Split(rendered, "\n") {
				fmt.Printf("    %s\n", line)
			}
		} else {
			fmt.Printf("  Rendered: %s\n", rendered)
		}
	}

	printSlot("task", resp.Slots.Task, true)
	printSlot("name", resp.Slots.Name, false)
	printSlot("branch", resp.Slots.Branch, false)
	if len(resp.Slots.Labels) > 0 {
		keys := make([]string, 0, len(resp.Slots.Labels))
		for k := range resp.Slots.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := resp.Slots.Labels[k]
			printSlot("label."+k, &v, false)
		}
	}
	printSlot("reuseMessage", resp.Slots.ReuseMessage, true)
	printSlot("message", resp.Slots.Message, true)
	printSlot("body", resp.Slots.Body, true)

	if len(resp.Errors) > 0 {
		if printedAny {
			fmt.Println()
		}
		printedAny = true
		fmt.Println("Errors:")
		for _, e := range resp.Errors {
			slot := triggerTerminalText(e.Slot, false)
			kind := triggerTerminalText(e.Kind, false)
			path := triggerTerminalText(e.Path, false)
			msg := triggerTerminalText(e.Message, false)
			line := fmt.Sprintf("  [%s] %s", slot, kind)
			if path != "" {
				line += fmt.Sprintf(" (path: %s)", path)
			}
			if msg != "" {
				line += ": " + msg
			}
			fmt.Println(line)
		}
	}

	if len(resp.Notes) > 0 {
		if printedAny {
			fmt.Println()
		}
		printedAny = true
		fmt.Println("Notes:")
		for _, n := range resp.Notes {
			fmt.Printf("  - %s\n", triggerTerminalText(n, false))
		}
	}

	if !printedAny {
		fmt.Println("No template slots rendered.")
	}
}

func init() {
	triggerCmd.AddCommand(triggerRenderCmd)
	triggerRenderCmd.Flags().StringVar(&triggerRenderEventRef, "event", "", "Stored trigger event ID (or unique prefix) to render against (required)")
}
