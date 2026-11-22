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

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/spf13/cobra"
)

var triggerGetCmd = &cobra.Command{
	Use:   "get <id|name>",
	Short: "Get an event trigger",
	Long:  `Display detailed configuration and numbered match rules for an event trigger.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerGet,
}

func effectiveTriggerMatchRules(match hubclient.TriggerMatchSpec) []hubclient.TriggerMatchRuleSpec {
	if len(match.Rules) > 0 {
		out := make([]hubclient.TriggerMatchRuleSpec, len(match.Rules))
		for i, r := range match.Rules {
			d := strings.TrimSpace(r.Dialect)
			if d == "" {
				d = ruleengine.DialectCEL
			}
			out[i] = hubclient.TriggerMatchRuleSpec{
				Dialect: d,
				Expr:    r.Expr,
			}
		}
		return out
	}
	if match.Expr != "" {
		d := strings.TrimSpace(match.Dialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		return []hubclient.TriggerMatchRuleSpec{{
			Dialect: d,
			Expr:    match.Expr,
		}}
	}
	return nil
}

func runTriggerGet(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerIDOrName(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	tr, err := hubCtx.Client.Triggers(projectID).Get(ctx, fullID)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to get trigger: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(tr)
	}

	printTriggerDetail(tr)
	return nil
}

func printTriggerDetail(tr *hubclient.Trigger) {
	agentLabel := tr.Action.AgentName
	if agentLabel == "" {
		agentLabel = tr.Action.AgentID
	}

	fmt.Printf("Trigger: %s\n", triggerTerminalText(tr.ID, false))
	fmt.Printf("  Name:        %s\n", triggerTerminalText(tr.Name, false))
	if tr.Description != "" {
		fmt.Printf("  Description: %s\n", triggerTerminalText(tr.Description, false))
	}
	fmt.Printf("  Project:     %s\n", triggerTerminalText(tr.ProjectID, false))
	fmt.Printf("  Status:      %s\n", triggerTerminalText(triggerStatusLabel(tr.Status, tr.StatusReason), false))
	fmt.Printf("  Revision:    %d\n", tr.AuthorizationRevision)
	fmt.Printf("  Source:      %s\n", triggerTerminalText(formatTriggerSource(tr.Source, 0), false))
	if presetLabel := formatTriggerMatchFormLabel(tr.Match.Form); presetLabel != "" {
		fmt.Printf("  Preset:      %s\n", triggerTerminalText(presetLabel, false))
	}
	fmt.Printf("  Types:       %s\n", triggerTerminalText(strings.Join(tr.Match.Types, ", "), false))

	rules := effectiveTriggerMatchRules(tr.Match)
	if tr.Match.RulesBroken {
		fmt.Printf("  Rules:       broken (stored match rules cannot be parsed)\n")
	} else if len(rules) == 0 {
		fmt.Printf("  Rules:       none (types only)\n")
	} else {
		fmt.Printf("  Rules:       %d\n", len(rules))
		for i, r := range rules {
			fmt.Printf("    %d/%d [%s] %q\n", i+1, len(rules), triggerTerminalText(r.Dialect, false), r.Expr)
		}
	}

	if tr.Subject.Expr != "" {
		subjDialect := strings.TrimSpace(tr.Subject.Dialect)
		if subjDialect == "" {
			subjDialect = ruleengine.DialectCEL
		}
		line := fmt.Sprintf("[%s] %q (mode: %s", triggerTerminalText(subjDialect, false), tr.Subject.Expr, triggerTerminalText(tr.Subject.Mode, false))
		if tr.Subject.CoalesceWindow != "" {
			line += ", coalesce: " + triggerTerminalText(tr.Subject.CoalesceWindow, false)
		}
		line += ")"
		fmt.Printf("  Subject:     %s\n", line)
	} else if tr.Subject.Mode != "" || tr.Subject.CoalesceWindow != "" {
		line := fmt.Sprintf("default (mode: %s", triggerTerminalText(tr.Subject.Mode, false))
		if tr.Subject.CoalesceWindow != "" {
			line += ", coalesce: " + triggerTerminalText(tr.Subject.CoalesceWindow, false)
		}
		line += ")"
		fmt.Printf("  Subject:     %s\n", line)
	}

	if tr.Action.Type == "dispatch_agent" {
		fmt.Printf("  Action:      dispatch_agent\n")
		fmt.Printf("  Template:    %s\n", triggerTerminalText(tr.Action.Template, false))
		if tr.Action.AgentName != "" {
			fmt.Printf("  Agent Name:  %s\n", triggerTerminalText(tr.Action.AgentName, false))
		}
		if tr.Action.NameConflict != "" {
			fmt.Printf("  On Conflict: %s\n", triggerTerminalText(tr.Action.NameConflict, false))
		}
		if tr.Action.Branch != "" {
			fmt.Printf("  Branch:      %s\n", triggerTerminalText(tr.Action.Branch, false))
		}
		if len(tr.Action.Labels) > 0 {
			keys := make([]string, 0, len(tr.Action.Labels))
			for k := range tr.Action.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, len(keys))
			for i, k := range keys {
				pairs[i] = triggerTerminalText(k, false) + "=" + triggerTerminalText(tr.Action.Labels[k], false)
			}
			fmt.Printf("  Labels:      %s\n", strings.Join(pairs, ", "))
		}
		if tr.Action.Ephemeral {
			fmt.Printf("  Ephemeral:   true\n")
		}
		if tr.Action.GithubToken != "" {
			fmt.Printf("  GH Token:    %s\n", triggerTerminalText(tr.Action.GithubToken, false))
		}
		if tr.Action.AgentRole != "" {
			fmt.Printf("  Agent Role:  %s\n", triggerTerminalText(tr.Action.AgentRole, false))
		}
		if tr.Action.ReuseMessage != "" {
			fmt.Printf("  Reuse Msg:   %s\n", triggerTerminalText(tr.Action.ReuseMessage, true))
		}
		if len(tr.Action.UntrustedVars) > 0 {
			safeVars := make([]string, len(tr.Action.UntrustedVars))
			for i, v := range tr.Action.UntrustedVars {
				safeVars[i] = triggerTerminalText(v, false)
			}
			fmt.Printf("  Allowed:     %s\n", strings.Join(safeVars, ", "))
		}
		if tr.Action.Task != "" {
			fmt.Printf("  Task:        %s\n", triggerTerminalText(tr.Action.Task, true))
		}
	} else {
		fmt.Printf("  Agent:       %s\n", triggerTerminalText(agentLabel, false))
		fmt.Printf("  Message:     %s\n", triggerTerminalText(tr.Action.Message, true))
	}
	fmt.Printf("  Fires:       %d (errors: %d)\n", tr.FireCount, tr.ErrorCount)
	if tr.LastFireAt != nil {
		lastFire := clitime.Ago(*tr.LastFireAt)
		if tr.LastFireStatus != "" {
			lastFire += " (" + triggerTerminalText(tr.LastFireStatus, false) + ")"
		}
		fmt.Printf("  Last Fire:   %s\n", lastFire)
	}
	if tr.LastFireError != "" {
		fmt.Printf("  Last Error:  %s\n", triggerTerminalText(tr.LastFireError, false))
	}
}
