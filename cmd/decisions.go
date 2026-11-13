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
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const errMsgChoicesExperimentRequired = "decisions need the web.chat_choices experiment"

var decisionsClientFactory = func() (hubclient.Client, error) {
	_, client, err := requireHubClient()
	return client, err
}

// decisionsCmd is the top-level command group for choice decision self-service.
var decisionsCmd = newDecisionsCmd()

func newDecisionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decisions",
		Short: "Inspect and record answers for choice decisions",
		Long: `Inspect choice decisions you have asked and record interpreted answers from user replies.

Commands:
  scion decisions list [--pending | --decided | --all]   List your decisions
  scion decisions show <key>                             Show all versions and answers for a decision
  scion decisions record ... --from <message-id>         Record interpreted answer(s) from a user reply`,
	}
	cmd.AddCommand(
		newDecisionsListCmd(),
		newDecisionsShowCmd(),
		newDecisionsRecordCmd(),
	)
	return cmd
}

func init() {
	rootCmd.AddCommand(decisionsCmd)
}

func decisionsWantJSON(cmd *cobra.Command) bool {
	if isJSONOutput() {
		return true
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

func decisionProvenanceDisplay(a *hubclient.DecisionAnswerView) string {
	if a == nil {
		return "-"
	}
	switch a.Provenance {
	case "direct":
		switch a.Outcome {
		case "confirm":
			return "confirmed"
		case "override":
			return "overridden"
		default:
			return "direct"
		}
	case "agent_interpreted":
		return "interpreted"
	default:
		if a.Provenance != "" {
			return safeText(a.Provenance)
		}
		return "-"
	}
}

func decisionAnswererDisplay(a *hubclient.DecisionAnswerView) string {
	if a == nil {
		return "-"
	}
	if a.AnsweredByName != "" {
		return safeText(a.AnsweredByName)
	}
	if a.AnsweredByEmail != "" {
		return safeText(a.AnsweredByEmail)
	}
	if a.AnsweredByUserID != "" {
		return safeText(a.AnsweredByUserID)
	}
	return "-"
}

func collapseSingleLine(s string) string {
	return strings.Join(strings.Fields(safeText(s)), " ")
}

func formatDecisionAPIError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.Code == "experiment_disabled" || (apiErr.StatusCode == http.StatusNotFound && apiErr.Code != "decision_not_found") {
		return errors.New(errMsgChoicesExperimentRequired)
	}
	if rawList, ok := apiErr.Details["errors"].([]interface{}); ok && len(rawList) > 0 {
		var lines []string
		for _, raw := range rawList {
			entry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if msg, ok := entry["message"].(string); ok && strings.TrimSpace(msg) != "" {
				lines = append(lines, safeText(msg))
			}
		}
		if len(lines) > 0 {
			return errors.New(strings.Join(lines, "\n"))
		}
	}
	if strings.TrimSpace(apiErr.Message) != "" {
		return errors.New(safeText(apiErr.Message))
	}
	return err
}
