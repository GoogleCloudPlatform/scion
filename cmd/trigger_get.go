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

func printTriggerActionDetail(act hubclient.TriggerActionSpec, prefix string) {
	switch act.Type {
	case "dispatch_agent":
		fmt.Printf("%sTemplate:    %s\n", prefix, triggerTerminalText(act.Template, false))
		if act.AgentName != "" {
			fmt.Printf("%sAgent Name:  %s\n", prefix, triggerTerminalText(act.AgentName, false))
		}
		if act.NameConflict != "" {
			fmt.Printf("%sOn Conflict: %s\n", prefix, triggerTerminalText(act.NameConflict, false))
		}
		if act.Branch != "" {
			fmt.Printf("%sBranch:      %s\n", prefix, triggerTerminalText(act.Branch, false))
		}
		if len(act.Labels) > 0 {
			keys := make([]string, 0, len(act.Labels))
			for k := range act.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, len(keys))
			for i, k := range keys {
				pairs[i] = triggerTerminalText(k, false) + "=" + triggerTerminalText(act.Labels[k], false)
			}
			fmt.Printf("%sLabels:      %s\n", prefix, strings.Join(pairs, ", "))
		}
		if act.Ephemeral {
			fmt.Printf("%sEphemeral:   true\n", prefix)
		}
		if act.GithubToken != "" {
			fmt.Printf("%sGH Token:    %s\n", prefix, triggerTerminalText(act.GithubToken, false))
		}
		if act.AgentRole != "" {
			fmt.Printf("%sAgent Role:  %s\n", prefix, triggerTerminalText(act.AgentRole, false))
		}
		if act.ReuseMessage != "" {
			fmt.Printf("%sReuse Msg:   %s\n", prefix, triggerTerminalText(act.ReuseMessage, true))
		}
		if act.AckComment != nil && act.AckComment.Body != "" {
			fmt.Printf("%sAck Comment: %s\n", prefix, triggerTerminalText(act.AckComment.Body, true))
		}
		if len(act.UntrustedVars) > 0 {
			safeVars := make([]string, len(act.UntrustedVars))
			for i, v := range act.UntrustedVars {
				safeVars[i] = triggerTerminalText(v, false)
			}
			fmt.Printf("%sAllowed:     %s\n", prefix, strings.Join(safeVars, ", "))
		}
		if act.Task != "" {
			fmt.Printf("%sTask:        %s\n", prefix, triggerTerminalText(act.Task, true))
		}
	case "exec":
		agentLabel := act.AgentName
		if agentLabel == "" {
			agentLabel = act.AgentID
		}
		fmt.Printf("%sAgent:       %s\n", prefix, triggerTerminalText(agentLabel, false))
		if act.CommandRedacted {
			fmt.Printf("%sCommand:     <redacted> (argc: %d, sha256: %s)\n", prefix, act.Argc, triggerTerminalText(act.CommandSHA256, false))
		} else if len(act.Command) > 0 {
			fmt.Printf("%sCommand:     %s\n", prefix, triggerTerminalText(strings.Join(act.Command, " "), false))
		}
		if act.TimeoutSeconds > 0 {
			fmt.Printf("%sTimeout:     %ds\n", prefix, act.TimeoutSeconds)
		}
	case "github.comment":
		if act.Target != "" {
			fmt.Printf("%sTarget:      %s\n", prefix, triggerTerminalText(act.Target, false))
		}
		if act.ReplyInThread {
			fmt.Printf("%sIn Thread:   true\n", prefix)
		}
		if len(act.UntrustedVars) > 0 {
			safeVars := make([]string, len(act.UntrustedVars))
			for i, v := range act.UntrustedVars {
				safeVars[i] = triggerTerminalText(v, false)
			}
			fmt.Printf("%sAllowed:     %s\n", prefix, strings.Join(safeVars, ", "))
		}
		if act.Body != "" {
			fmt.Printf("%sComment:     %s\n", prefix, triggerTerminalText(act.Body, true))
		}
	default:
		agentLabel := act.AgentName
		if agentLabel == "" {
			agentLabel = act.AgentID
		}
		fmt.Printf("%sAgent:       %s\n", prefix, triggerTerminalText(agentLabel, false))
		fmt.Printf("%sMessage:     %s\n", prefix, triggerTerminalText(act.Message, true))
		if act.Offline != "" {
			fmt.Printf("%sOffline:     %s\n", prefix, triggerTerminalText(act.Offline, false))
		}
		if act.QueueTTLSeconds > 0 {
			fmt.Printf("%sQueue TTL:   %ds\n", prefix, act.QueueTTLSeconds)
		}
		if len(act.UntrustedVars) > 0 {
			safeVars := make([]string, len(act.UntrustedVars))
			for i, v := range act.UntrustedVars {
				safeVars[i] = triggerTerminalText(v, false)
			}
			fmt.Printf("%sAllowed:     %s\n", prefix, strings.Join(safeVars, ", "))
		}
	}
}

func printTriggerDetail(tr *hubclient.Trigger) {
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

	actions := effectiveTriggerActions(tr)
	if tr.ActionsBroken {
		fmt.Printf("  Actions:     broken (stored actions cannot be parsed)\n")
	} else if len(actions) > 0 {
		fmt.Printf("  Actions:     %d\n", len(actions))
		for i, act := range actions {
			fmt.Printf("    %d/%d [%s]\n", i+1, len(actions), triggerTerminalText(act.Type, false))
			printTriggerActionDetail(act, "      ")
		}
	}
	if tr.Notify != nil {
		fmt.Printf("  Notify:      %s\n", formatTriggerNotifySummary(tr.Notify))
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

func formatTriggerNotifySummary(n *hubclient.TriggerNotifySpec) string {
	if n == nil {
		return ""
	}
	var parts []string
	userNotice := true
	if n.UserNotice != nil {
		userNotice = *n.UserNotice
	}
	if userNotice {
		parts = append(parts, "user=on")
	} else {
		parts = append(parts, "user=off")
	}
	agentLabel := n.AgentName
	if agentLabel == "" {
		agentLabel = n.AgentID
	}
	if agentLabel != "" {
		parts = append(parts, "agent="+triggerTerminalText(agentLabel, false))
	}
	if len(n.OnOutcomes) > 0 {
		safe := make([]string, len(n.OnOutcomes))
		for i, o := range n.OnOutcomes {
			safe[i] = triggerTerminalText(o, false)
		}
		parts = append(parts, "on="+strings.Join(safe, ","))
	}
	return strings.Join(parts, " ")
}
