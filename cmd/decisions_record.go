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
	"io"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func newDecisionsRecordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record (<key> <choice-id> | <key>=<choice-id>... | --other <key>=<text>...) --from <message-id>",
		Short: "Record interpreted Decision answer(s) from a user free-text reply",
		Long: `Record one or more interpreted answers from a user's free-text reply message.

Supports a single positional pair (<key> <choice-id>), 1 to 10 <key>=<choice-id>
pairs, or repeatable --other <key>=<text> flags for decisions with Other enabled
(combinable with <key>=<choice-id> pairs in one atomic batch; do not mix the
two-argument positional form with <key>=<choice-id> or --other).

Examples:
  scion decisions record D234 1 --from <message-id>
  scion decisions record --from <message-id> D234=1 17=a
  scion decisions record --from <message-id> --other region="europe-west4 canary"
  scion decisions record --from <message-id> proceed=1 --other region="europe-west4 canary"`,
		RunE: runDecisionsRecord,
	}
	cmd.Flags().String("from", "", "Message ID of the user reply being interpreted (required)")
	cmd.Flags().StringArray("other", nil, "Record a free-form Other answer as <key>=<text> (repeatable)")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func parseDecisionsRecordArgs(args []string, otherFlags []string) ([]hubclient.InterpretedAnswerInput, error) {
	if len(args) == 0 && len(otherFlags) == 0 {
		return nil, newUsageError("provide '<key> <choice-id>', one or more '<key>=<choice-id>' pairs, or '--other <key>=<text>'")
	}

	var pairs []hubclient.InterpretedAnswerInput
	if len(args) > 0 {
		withEq := 0
		for _, a := range args {
			if strings.Contains(a, "=") {
				withEq++
			}
		}

		if withEq > 0 && withEq < len(args) {
			return nil, newUsageError("cannot mix positional '<key> <choice-id>' arguments with '<key>=<choice-id>' pairs")
		}

		if withEq == 0 {
			if len(otherFlags) > 0 {
				return nil, newUsageError("cannot mix positional '<key> <choice-id>' arguments with --other; use '<key>=<choice-id>' pairs instead")
			}
			if len(args) != 2 {
				return nil, newUsageError("positional form requires '<key> <choice-id>'; for multiple answers pass '<key>=<choice-id> ...'")
			}
			key := strings.TrimSpace(args[0])
			choiceID := strings.TrimSpace(args[1])
			if key == "" || choiceID == "" {
				return nil, newUsageError("both <key> and <choice-id> must be non-empty")
			}
			if choiceID == messages.ChoiceIDOther {
				return nil, newUsageError("use --other %s=\"<text>\" to record a free-form Other answer", key)
			}
			pairs = append(pairs, hubclient.InterpretedAnswerInput{
				Key:      key,
				ChoiceID: choiceID,
			})
		} else {
			for _, arg := range args {
				rawKey, rawChoice, _ := strings.Cut(arg, "=")
				key := strings.TrimSpace(rawKey)
				choiceID := strings.TrimSpace(rawChoice)
				if key == "" || choiceID == "" {
					return nil, newUsageError("invalid pair %q: expected <key>=<choice-id>", arg)
				}
				if choiceID == messages.ChoiceIDOther {
					return nil, newUsageError("use --other %s=\"<text>\" to record a free-form Other answer", key)
				}
				pairs = append(pairs, hubclient.InterpretedAnswerInput{
					Key:      key,
					ChoiceID: choiceID,
				})
			}
		}
	}

	for _, raw := range otherFlags {
		rawKey, rawText, hasEq := strings.Cut(raw, "=")
		key := strings.TrimSpace(rawKey)
		if !hasEq || key == "" {
			return nil, newUsageError("invalid --other %q: expected <key>=<text>", raw)
		}
		text, verr := messages.NormalizeOtherText(messages.ChoiceIDOther, rawText)
		if verr != nil {
			return nil, newUsageError("--other %s: %s", key, verr.Message)
		}
		pairs = append(pairs, hubclient.InterpretedAnswerInput{
			Key:       key,
			ChoiceID:  messages.ChoiceIDOther,
			OtherText: text,
		})
	}

	if len(pairs) > messages.MaxDecisionsPerMessage {
		return nil, newUsageError("at most %d answers may be recorded at once", messages.MaxDecisionsPerMessage)
	}

	seen := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		norm := messages.DecisionKeyNorm(p.Key)
		if seen[norm] {
			return nil, newUsageError("duplicate decision key %q", p.Key)
		}
		seen[norm] = true
	}

	return pairs, nil
}

func runDecisionsRecord(cmd *cobra.Command, args []string) error {
	fromRaw, _ := cmd.Flags().GetString("from")
	fromMsgID := strings.TrimSpace(fromRaw)
	if fromMsgID == "" {
		return newUsageError("--from <message-id> is required")
	}

	otherFlags, _ := cmd.Flags().GetStringArray("other")
	pairs, err := parseDecisionsRecordArgs(args, otherFlags)
	if err != nil {
		return err
	}

	client, err := decisionsClientFactory()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.Decisions().RecordInterpreted(ctx, &hubclient.RecordInterpretedRequest{
		SourceMessageID: fromMsgID,
		IdempotencyKey:  uuid.NewString(),
		Answers:         pairs,
	})
	if err != nil {
		return formatDecisionAPIError(err)
	}

	if decisionsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}

	writeDecisionsRecordResult(cmd.OutOrStdout(), fromMsgID, resp)
	return nil
}

func writeDecisionsRecordResult(out io.Writer, sourceMessageID string, resp *hubclient.RecordInterpretedResponse) {
	if len(resp.Decisions) == 1 {
		_, _ = fmt.Fprintf(out, "Recorded 1 interpreted answer from %s (response %s):\n",
			safeText(sourceMessageID), safeText(resp.ResponseMessageID))
	} else {
		_, _ = fmt.Fprintf(out, "Recorded %d interpreted answers from %s (batch %s, response %s):\n",
			len(resp.Decisions), safeText(sourceMessageID), safeText(resp.BatchID), safeText(resp.ResponseMessageID))
	}
	for _, d := range resp.Decisions {
		label := ""
		choiceID := ""
		if d.Decision != nil {
			label = collapseSingleLine(d.Decision.Label)
			if d.Decision.ChoiceID == messages.ChoiceIDOther && d.Decision.OtherText != "" {
				label = fmt.Sprintf("%s \"%s\"", label, safeText(d.Decision.OtherText))
			}
			choiceID = safeText(d.Decision.ChoiceID)
		}
		_, _ = fmt.Fprintf(out, "  %s v%d -> %s (%s)\n", safeText(d.Key), d.Version, label, choiceID)
	}
}
