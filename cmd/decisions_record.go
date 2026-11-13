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
		Use:   "record (<key> <choice-id> | <key>=<choice-id>...) --from <message-id>",
		Short: "Record interpreted decision answer(s) from a user free-text reply",
		Long: `Record one or more interpreted answers from a user's free-text reply message.

Supports either a single positional pair or 1 to 10 key=choice-id pairs in one
atomic batch (do not mix the two forms in one command).

Examples:
  scion decisions record D234 1 --from <message-id>
  scion decisions record --from <message-id> D234=1 17=a`,
		RunE: runDecisionsRecord,
	}
	cmd.Flags().String("from", "", "Message ID of the user reply being interpreted (required)")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func parseDecisionsRecordArgs(args []string) ([]hubclient.InterpretedAnswerInput, error) {
	if len(args) == 0 {
		return nil, newUsageError("provide either '<key> <choice-id>' or one or more '<key>=<choice-id>' pairs")
	}

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
		if len(args) != 2 {
			return nil, newUsageError("positional form requires '<key> <choice-id>'; for multiple answers pass '<key>=<choice-id> ...'")
		}
		key := strings.TrimSpace(args[0])
		choiceID := strings.TrimSpace(args[1])
		if key == "" || choiceID == "" {
			return nil, newUsageError("both <key> and <choice-id> must be non-empty")
		}
		return []hubclient.InterpretedAnswerInput{
			{Key: key, ChoiceID: choiceID},
		}, nil
	}

	if len(args) > messages.MaxDecisionsPerMessage {
		return nil, newUsageError("at most %d <key>=<choice-id> pairs may be recorded at once", messages.MaxDecisionsPerMessage)
	}

	pairs := make([]hubclient.InterpretedAnswerInput, 0, len(args))
	for _, arg := range args {
		rawKey, rawChoice, _ := strings.Cut(arg, "=")
		key := strings.TrimSpace(rawKey)
		choiceID := strings.TrimSpace(rawChoice)
		if key == "" || choiceID == "" {
			return nil, newUsageError("invalid pair %q: expected <key>=<choice-id>", arg)
		}
		pairs = append(pairs, hubclient.InterpretedAnswerInput{
			Key:      key,
			ChoiceID: choiceID,
		})
	}
	return pairs, nil
}

func runDecisionsRecord(cmd *cobra.Command, args []string) error {
	fromRaw, _ := cmd.Flags().GetString("from")
	fromMsgID := strings.TrimSpace(fromRaw)
	if fromMsgID == "" {
		return newUsageError("--from <message-id> is required")
	}

	pairs, err := parseDecisionsRecordArgs(args)
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
			choiceID = safeText(d.Decision.ChoiceID)
		}
		_, _ = fmt.Fprintf(out, "  %s v%d -> %s (%s)\n", safeText(d.Key), d.Version, label, choiceID)
	}
}
