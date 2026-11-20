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

	if isJSONOutput() {
		return outputJSON(tr)
	}

	printTriggerDetail(tr)
	return nil
}

func printTriggerDetail(tr *hubclient.Trigger) {
	agentLabel := tr.Action.AgentName
	if agentLabel == "" {
		agentLabel = tr.Action.AgentID
	}

	fmt.Printf("Trigger: %s\n", tr.ID)
	fmt.Printf("  Name:        %s\n", tr.Name)
	if tr.Description != "" {
		fmt.Printf("  Description: %s\n", tr.Description)
	}
	fmt.Printf("  Project:     %s\n", tr.ProjectID)
	fmt.Printf("  Status:      %s\n", triggerStatusLabel(tr.Status, tr.StatusReason))
	fmt.Printf("  Revision:    %d\n", tr.AuthorizationRevision)
	fmt.Printf("  Source:      %s:%s\n", tr.Source.Kind, tr.Source.ID)
	fmt.Printf("  Types:       %s\n", strings.Join(tr.Match.Types, ", "))

	rules := effectiveTriggerMatchRules(tr.Match)
	if tr.Match.RulesBroken {
		fmt.Printf("  Rules:       broken (stored match rules cannot be parsed)\n")
	} else if len(rules) == 0 {
		fmt.Printf("  Rules:       none (types only)\n")
	} else {
		fmt.Printf("  Rules:       %d\n", len(rules))
		for i, r := range rules {
			fmt.Printf("    %d/%d [%s] %q\n", i+1, len(rules), r.Dialect, r.Expr)
		}
	}

	if tr.Subject.Expr != "" {
		subjDialect := strings.TrimSpace(tr.Subject.Dialect)
		if subjDialect == "" {
			subjDialect = ruleengine.DialectCEL
		}
		fmt.Printf("  Subject:     [%s] %q (mode: %s)\n", subjDialect, tr.Subject.Expr, tr.Subject.Mode)
	} else if tr.Subject.Mode != "" {
		fmt.Printf("  Subject:     default (mode: %s)\n", tr.Subject.Mode)
	}

	fmt.Printf("  Agent:       %s\n", agentLabel)
	fmt.Printf("  Message:     %s\n", tr.Action.Message)
	fmt.Printf("  Fires:       %d (errors: %d)\n", tr.FireCount, tr.ErrorCount)
	if tr.LastFireAt != nil {
		lastFire := clitime.Ago(*tr.LastFireAt)
		if tr.LastFireStatus != "" {
			lastFire += " (" + tr.LastFireStatus + ")"
		}
		fmt.Printf("  Last Fire:   %s\n", lastFire)
	}
	if tr.LastFireError != "" {
		fmt.Printf("  Last Error:  %s\n", tr.LastFireError)
	}
}
