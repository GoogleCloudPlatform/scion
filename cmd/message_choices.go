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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/agentstatus"
)

// Choice prompt flags of scion message (design choices v4, 4.3).
var (
	msgChoices       string
	msgChoicesFile   string
	msgChoicesUpdate bool
	// msgChoicesPayload is the decisions JSON loaded from --choices or
	// --choices-file for this invocation; nil for an ordinary message.
	msgChoicesPayload json.RawMessage
)

// choicesStdin is where --choices-file - reads; tests replace it.
var choicesStdin io.Reader = os.Stdin

// runChoicesAskUser is the ask_user side effect after a choice prompt is
// sent from an agent: the same status update as
// `sciontool status ask_user "<summary>"`. Tests replace it.
var runChoicesAskUser = agentstatus.RunAskUser

// choicesSummaryMax caps the first question in the ask_user summary.
const choicesSummaryMax = 120

// choicesRequested reports whether this invocation sends choices.
func choicesRequested() bool {
	return msgChoices != "" || msgChoicesFile != ""
}

// resetChoiceFlags clears the choice flag state (tests reuse the command).
func resetChoiceFlags() {
	msgChoices, msgChoicesFile, msgChoicesUpdate, msgChoicesPayload = "", "", false, nil
}

// validateChoiceFlags checks the choice flags against each other and the
// recipient, before anything is read or sent. agentTarget and group are
// the recipient kinds a choice prompt may not go to.
func validateChoiceFlags(agentTarget, group bool) error {
	if msgChoices != "" && msgChoicesFile != "" {
		return newUsageError("--choices and --choices-file are mutually exclusive")
	}
	if !choicesRequested() {
		if msgChoicesUpdate {
			return newUsageError("--update is valid only with --choices or --choices-file")
		}
		return nil
	}
	if msgChoicesFile == "-" && msgBodyFile == "-" {
		return newUsageError("--choices-file - and --body-file - both read stdin; use a file for one of them")
	}
	if agentTarget || group {
		return newUsageError("choices can be sent only to a user or a conversation (user:<email>, @<email>, conv:<id>, #<thread>), not to an agent or a group")
	}
	if msgIn != "" || msgAt != "" {
		return newUsageError("--choices cannot be combined with --in or --at")
	}
	if len(msgAttach) > 0 {
		return newUsageError("--choices cannot be combined with --attach")
	}
	if os.Getenv("SCION_AGENT_NAME") == "" {
		return fmt.Errorf("choices can be sent only by an agent; it works inside an agent container where SCION_AGENT_NAME is set")
	}
	return nil
}

// loadChoicesPayload reads the decisions JSON and checks only that it is a
// JSON array; every rule is the hub's to enforce.
func loadChoicesPayload() (json.RawMessage, error) {
	var raw []byte
	switch {
	case msgChoices != "":
		raw = []byte(msgChoices)
	case msgChoicesFile == "-":
		b, err := io.ReadAll(choicesStdin)
		if err != nil {
			return nil, fmt.Errorf("reading --choices-file from stdin: %w", err)
		}
		raw = b
	default:
		b, err := os.ReadFile(msgChoicesFile)
		if err != nil {
			return nil, fmt.Errorf("reading --choices-file: %w", err)
		}
		raw = b
	}
	raw = bytes.TrimSpace(raw)
	var probe []json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, newUsageError("choices must be a JSON array of decisions: %v", err)
	}
	if len(probe) == 0 {
		return nil, newUsageError("choices must hold at least one decision")
	}
	return json.RawMessage(raw), nil
}

var (
	mdLinkRe  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdCharsRe = regexp.MustCompile("[*_`~#>]+")
)

// choicesSummary is the ask_user message after a choice prompt is sent:
// "Decision needed: <first question as plain text, 120 chars>" plus
// " (+N more)" for further decisions.
func choicesSummary(payload json.RawMessage) string {
	var decisions []struct {
		Question string `json:"question"`
	}
	_ = json.Unmarshal(payload, &decisions)
	first := ""
	if len(decisions) > 0 {
		first = decisions[0].Question
	}
	first = mdLinkRe.ReplaceAllString(first, "$1")
	first = mdCharsRe.ReplaceAllString(first, "")
	first = strings.Join(strings.Fields(first), " ")
	if utf8.RuneCountInString(first) > choicesSummaryMax {
		first = string([]rune(first)[:choicesSummaryMax-1]) + "…"
	}
	summary := "Decision needed: " + first
	if len(decisions) > 1 {
		summary += fmt.Sprintf(" (+%d more)", len(decisions)-1)
	}
	return summary
}

// choiceDecisionLine is one decision in the text output:
// "D234 v1 (new)", "D1 v2 (replaced v1, pending)" or
// "D1 v2 (replaced v1, decided: yes)".
func choiceDecisionLine(d hubclient.ChoiceDecisionResult) string {
	if d.Replaced == nil {
		return fmt.Sprintf("%s v%d (new)", d.Key, d.Version)
	}
	state := d.Replaced.Status
	if d.Replaced.Answer != "" {
		state += ": " + d.Replaced.Answer
	}
	return fmt.Sprintf("%s v%d (replaced v%d, %s)", d.Key, d.Version, d.Replaced.Version, state)
}

// printChoiceDecisions prints one line per decision of a sent prompt.
func printChoiceDecisions(result *hubclient.OutboundMessageResult) {
	if result == nil {
		return
	}
	for _, d := range result.Decisions {
		fmt.Println(choiceDecisionLine(d))
	}
}

// applyChoicesToOutbound puts the loaded decisions on an outbound request.
// The hub sets the message type of a choice prompt.
func applyChoicesToOutbound(out *hubclient.OutboundMessageRequest) {
	if msgChoicesPayload == nil {
		return
	}
	out.Choices = msgChoicesPayload
	out.ChoicesUpdate = msgChoicesUpdate
	out.Type = ""
}

// afterChoicesSent runs the ask_user side effect after the hub accepted a
// choice prompt, in agent mode only (design 4.9). The status update logs
// its own failures and never fails the send; a refused send (for example
// 400 experiment_disabled) never reaches here.
func afterChoicesSent() {
	if msgChoicesPayload == nil || resolveMode() != ModeAgent {
		return
	}
	runChoicesAskUser(choicesSummary(msgChoicesPayload))
}
