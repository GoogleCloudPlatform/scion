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
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/agentstatus"
)

// Decision prompt flags of scion message (design choices v4, 4.3).
var (
	msgDecisions       string
	msgDecisionsFile   string
	msgDecisionsUpdate bool
	// legacy choices fallback: remove in ptone/scion#8249
	msgLegacyChoices     string
	msgLegacyChoicesFile string
	// msgDecisionsPayload is the decisions JSON loaded from --decisions or
	// --decisions-file (or their deprecated --choices* aliases) for this
	// invocation; nil for an ordinary message.
	msgDecisionsPayload json.RawMessage
)

// decisionsStdin is where --decisions-file - reads; tests replace it.
var decisionsStdin io.Reader = os.Stdin

// runDecisionsAskUser is the ask_user side effect after a decision prompt is
// sent from an agent: the same status update as
// `sciontool status ask_user "<summary>"`. Tests replace it.
var runDecisionsAskUser = agentstatus.RunAskUser

// decisionsSummaryMax caps the first question in the ask_user summary.
const decisionsSummaryMax = 120

// decisionsRequested reports whether this invocation sends decisions.
func decisionsRequested() bool {
	return msgDecisions != "" || msgDecisionsFile != "" || msgLegacyChoices != "" || msgLegacyChoicesFile != ""
}

// resetDecisionFlags clears the decision flag state (tests reuse the command).
func resetDecisionFlags() {
	msgDecisions, msgDecisionsFile, msgLegacyChoices, msgLegacyChoicesFile, msgDecisionsUpdate, msgDecisionsPayload = "", "", "", "", false, nil
}

// validateDecisionFlags checks the decision flags against each other and the
// recipient, before anything is read or sent. agentTarget and group are
// the recipient kinds a decision prompt may not go to.
func validateDecisionFlags(agentTarget, group bool) error {
	if msgDecisions != "" && msgLegacyChoices != "" {
		return newUsageError("--decisions and --choices are mutually exclusive; use --decisions")
	}
	if msgDecisionsFile != "" && msgLegacyChoicesFile != "" {
		return newUsageError("--decisions-file and --choices-file are mutually exclusive; use --decisions-file")
	}
	hasInline := msgDecisions != "" || msgLegacyChoices != ""
	hasFile := msgDecisionsFile != "" || msgLegacyChoicesFile != ""
	if hasInline && hasFile {
		return newUsageError("--decisions and --decisions-file are mutually exclusive")
	}
	if !decisionsRequested() {
		if msgDecisionsUpdate {
			return newUsageError("--update is valid only with --decisions or --decisions-file")
		}
		return nil
	}
	if (msgDecisionsFile == "-" || msgLegacyChoicesFile == "-") && msgBodyFile == "-" {
		return newUsageError("--decisions-file - and --body-file - both read stdin; use a file for one of them")
	}
	if agentTarget || group {
		return errors.New("decisions can be sent only to a user or a conversation (user:<email>, @<email>, conv:<id>, #<thread>), not to an agent or a group")
	}
	if msgIn != "" || msgAt != "" {
		return newUsageError("--decisions cannot be combined with --in or --at")
	}
	// The hub refuses these too (attachments) or ignores them for a decision
	// prompt; refuse them here, before anything is read or sent.
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"--attach", len(msgAttach) > 0},
		{"--artifact", len(msgArtifacts) > 0},
		{"--wake", msgWake},
		{"--cc", len(msgCC) > 0},
		{"--channel", msgChannel != ""},
		{"--thread-id", msgThreadID != ""},
	} {
		if f.set {
			return newUsageError("--decisions cannot be combined with %s", f.name)
		}
	}
	if !decisionsSenderIsAgent() {
		return errors.New("decisions can be sent only by an agent; it works inside an agent container (agent mode, with SCION_AGENT_NAME set)")
	}
	return nil
}

// decisionsSenderIsAgent is the one test for "an agent sends this decision
// prompt", used both to refuse a send (validateDecisionFlags) and to run the
// ask_user side effect after one (afterDecisionsSent). It needs both: agent
// mode, which the side effect is for, and SCION_AGENT_NAME, the agent
// identity the prompt is sent as (the hub records it as the asker). Agent
// mode alone would let a send through that has no sender identity; the
// identity alone would mark a non-agent CLI waiting for input.
func decisionsSenderIsAgent() bool {
	return resolveMode() == ModeAgent && os.Getenv("SCION_AGENT_NAME") != ""
}

// loadDecisionsPayload reads the decisions JSON and checks only that it is a
// JSON array; every rule is the hub's to enforce.
func loadDecisionsPayload() (json.RawMessage, error) {
	inline := msgDecisions
	if inline == "" {
		inline = msgLegacyChoices
	}
	file := msgDecisionsFile
	if file == "" {
		file = msgLegacyChoicesFile
	}
	var raw []byte
	switch {
	case inline != "":
		raw = []byte(inline)
	case file == "-":
		b, err := io.ReadAll(decisionsStdin)
		if err != nil {
			return nil, fmt.Errorf("reading --decisions-file from stdin: %w", err)
		}
		raw = b
	default:
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading --decisions-file: %w", err)
		}
		raw = b
	}
	raw = bytes.TrimSpace(raw)
	var probe []json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("--decisions must be a JSON array of decisions: %v", err)
	}
	if len(probe) == 0 {
		return nil, errors.New("--decisions must hold at least one decision")
	}
	return json.RawMessage(raw), nil
}

var (
	mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// mdHeadingRe is a heading or quote marker at the start of a line.
	mdHeadingRe = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6}|>+)[ \t]+`)
	// mdEmphasisRe is a run of emphasis or code markers at a word
	// boundary: after the start, a space or an opening bracket, or before
	// the end, a space or punctuation. A marker inside a word (ship_now)
	// is text.
	mdEmphasisRe = regexp.MustCompile("(^|[\\s(\\[])[*_`~]+|[*_`~]+($|[\\s).,:;!?\\]])")
)

// decisionsSummary is the ask_user message after a decision prompt is sent:
// "Decision needed: <first question as plain text, 120 chars>" plus
// " (+N more)" for further decisions.
func decisionsSummary(payload json.RawMessage) string {
	var decisions []struct {
		Question string `json:"question"`
	}
	_ = json.Unmarshal(payload, &decisions)
	first := ""
	if len(decisions) > 0 {
		first = decisions[0].Question
	}
	first = plainSummaryText(first)
	if utf8.RuneCountInString(first) > decisionsSummaryMax {
		first = string([]rune(first)[:decisionsSummaryMax-1]) + "…"
	}
	summary := "Decision needed: " + first
	if len(decisions) > 1 {
		summary += fmt.Sprintf(" (+%d more)", len(decisions)-1)
	}
	return summary
}

// plainSummaryText is markdown text as one plain line: links become their
// text, and heading, quote, emphasis and code markers go where they mark
// (line starts and word boundaries), so "ship_now" and "PR #12" stay.
func plainSummaryText(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdHeadingRe.ReplaceAllString(s, "")
	// Twice: a marker run can end where the next one starts ("**`x`**").
	for i := 0; i < 2; i++ {
		s = mdEmphasisRe.ReplaceAllString(s, "$1$2")
	}
	return strings.Join(strings.Fields(s), " ")
}

// decisionSendLine is one decision in the text output (design 4.2):
// "D234 v1 (new)", "D1 v2 (new, replaces v1 which was pending)" or
// "D1 v2 (new, replaces v1 which was decided by Pat: yes)".
func decisionSendLine(d hubclient.DecisionSendResult) string {
	if d.Replaced == nil {
		return fmt.Sprintf("%s v%d (new)", d.Key, d.Version)
	}
	state := d.Replaced.Status
	switch {
	case d.Replaced.Answer != "" && d.Replaced.AnsweredBy != "":
		state += " by " + d.Replaced.AnsweredBy + ": " + d.Replaced.Answer
	case d.Replaced.Answer != "":
		state += ": " + d.Replaced.Answer
	}
	return fmt.Sprintf("%s v%d (new, replaces v%d which was %s)", d.Key, d.Version, d.Replaced.Version, state)
}

// printSentDecisions prints one line per decision of a sent prompt.
func printSentDecisions(result *hubclient.OutboundMessageResult) {
	if result == nil {
		return
	}
	for _, d := range result.Decisions {
		fmt.Println(decisionSendLine(d))
	}
}

// applyDecisionsToOutbound puts the loaded decisions on an outbound request.
// The hub sets the message type of a decision prompt.
func applyDecisionsToOutbound(out *hubclient.OutboundMessageRequest) {
	if msgDecisionsPayload == nil {
		return
	}
	out.Decisions = msgDecisionsPayload
	out.DecisionsUpdate = msgDecisionsUpdate
	out.Type = ""
}

// afterDecisionsSent runs the ask_user side effect after the hub accepted a
// decision prompt, in agent mode only (design 4.9). The status update logs
// its own failures and never fails the send; a refused send (for example
// 400 experiment_disabled) never reaches here.
func afterDecisionsSent() {
	if msgDecisionsPayload == nil || !decisionsSenderIsAgent() {
		return
	}
	runDecisionsAskUser(decisionsSummary(msgDecisionsPayload))
}
