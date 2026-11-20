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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	triggerEventListSource  string
	triggerEventListWebhook string
	triggerEventListStatus  string
	triggerEventListSince   string
	triggerEventListLimit   int
	triggerEventListCursor  string

	triggerEventPayloadOnly bool
)

var triggerEventCmd = &cobra.Command{
	Use:   "event",
	Short: "Inspect received trigger events",
	Long:  `List and inspect received trigger events for the current project.`,
}

var triggerEventListCmd = &cobra.Command{
	Use:   "list",
	Short: "List received trigger events",
	Long:  `List received trigger events for the current project, ordered newest first. When called in agent mode, only events visible to the calling agent are shown.`,
	RunE:  runTriggerEventList,
}

var triggerEventGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show details for a received trigger event",
	Long:  `Show metadata, extracted attributes, extracted lists, headers, and the projected or raw JSON payload for a received trigger event.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerEventGet,
}

const (
	defaultTriggerEventListLimit   = 50
	triggerEventPrefixScanMaxPages = 10
)

// resolveTriggerEventID resolves a full event UUID or unique ID prefix to a
// full UUID, stopping pagination as soon as a second match is observed or
// triggerEventPrefixScanMaxPages pages have been scanned.
func resolveTriggerEventID(ctx context.Context, hubCtx *HubContext, projectID, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", newUsageError("trigger event ID is required")
	}
	if uuid.Validate(ref) == nil {
		return ref, nil
	}

	svc := hubCtx.Client.TriggerEvents(projectID)
	opts := hubclient.ListTriggerEventsOptions{
		Page: apiclient.PageOptions{Limit: triggerCLIMaxPageLimit},
	}
	var (
		prefixMatches []string
		lastScope     string
		seenCursors   = map[string]bool{}
		scannedEvents int
		truncated     bool
	)
	for pagesScanned := 0; ; pagesScanned++ {
		if pagesScanned >= triggerEventPrefixScanMaxPages {
			truncated = true
			break
		}
		page, err := svc.List(ctx, &opts)
		if err != nil {
			return "", wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list trigger events for resolution: %w", err))
		}
		lastScope = page.Scope
		scannedEvents += len(page.Events)
		for _, ev := range page.Events {
			if strings.HasPrefix(ev.ID, ref) {
				prefixMatches = append(prefixMatches, ev.ID)
				if len(prefixMatches) >= 2 {
					return "", fmt.Errorf("ambiguous trigger event ID prefix %q matches multiple events (%s, %s); pass the full event UUID",
						ref, prefixMatches[0], prefixMatches[1])
				}
			}
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			break
		}
		seenCursors[page.NextCursor] = true
		opts.Page.Cursor = page.NextCursor
	}

	if len(prefixMatches) == 0 {
		if truncated {
			return "", fmt.Errorf("trigger event ID prefix %q not found in the newest %d events; pass the full event UUID", ref, scannedEvents)
		}
		if isCallerScopedTriggerView(hubCtx, lastScope) {
			return "", fmt.Errorf("no trigger event visible to this agent found matching %q", ref)
		}
		return "", fmt.Errorf("no trigger event found matching %q", ref)
	}
	if truncated {
		return "", fmt.Errorf("trigger event ID prefix %q matched %s in the newest %d events, but older events were not scanned; pass the full event UUID", ref, prefixMatches[0], scannedEvents)
	}
	return prefixMatches[0], nil
}

func runTriggerEventList(cmd *cobra.Command, args []string) error {
	if triggerEventListLimit < 0 {
		return newUsageError("--limit must be non-negative")
	}
	limit := triggerEventListLimit
	if limit == 0 {
		limit = defaultTriggerEventListLimit
	}

	var since *time.Time
	if sinceRaw := strings.TrimSpace(triggerEventListSince); sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, sinceRaw)
		if err != nil {
			return newUsageError("invalid --since timestamp %q (expected RFC3339)", sinceRaw)
		}
		utc := parsed.UTC()
		since = &utc
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	opts := &hubclient.ListTriggerEventsOptions{
		Source: strings.TrimSpace(triggerEventListSource),
		Status: strings.TrimSpace(triggerEventListStatus),
		Since:  since,
		Page: apiclient.PageOptions{
			Limit:  limit,
			Cursor: strings.TrimSpace(triggerEventListCursor),
		},
	}
	if webhookRef := strings.TrimSpace(triggerEventListWebhook); webhookRef != "" {
		resolvedID, rErr := resolveWebhookEndpointIDOrName(ctx, hubCtx, projectID, webhookRef)
		if rErr != nil {
			return rErr
		}
		opts.SourceID = resolvedID
		if opts.Source == "" {
			opts.Source = "webhook"
		}
	}

	resp, err := hubCtx.Client.TriggerEvents(projectID).List(ctx, opts)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to list trigger events: %w", err))
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(resp)
	}

	callerScoped := isCallerScopedTriggerView(hubCtx, resp.Scope)
	if len(resp.Events) == 0 {
		if callerScoped {
			fmt.Println("No trigger events visible to this agent (other project events are hidden).")
		} else {
			fmt.Println("No trigger events found.")
		}
		return nil
	}

	if callerScoped {
		fmt.Println("Note: showing only trigger events visible to this agent (other project events are hidden).")
		fmt.Println()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSOURCE\tTYPE\tSUBJECT\tSTATUS\tMATCHES\tFIRES\tRECEIVED")
	for _, ev := range resp.Events {
		shortID := ev.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		subj := ev.Subject
		if subj == "" {
			subj = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			triggerTerminalText(shortID, false),
			triggerTerminalText(formatTriggerSource(ev.Source, 8), false),
			triggerTerminalText(ev.Type, false),
			triggerTerminalText(subj, false),
			triggerTerminalText(ev.Status, false),
			ev.MatchCount,
			ev.FireCount,
			clitime.Ago(ev.ReceivedAt),
		)
	}
	_ = w.Flush()
	if resp.NextCursor != "" {
		safeCursor := triggerTerminalText(resp.NextCursor, false)
		fmt.Printf("\nNext cursor: %s (use 'scion trigger event list --cursor %s' for the next page)\n", safeCursor, safeCursor)
	}
	return nil
}

func runTriggerEventGet(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	fullID, err := resolveTriggerEventID(ctx, hubCtx, projectID, args[0])
	if err != nil {
		return err
	}

	detail, err := hubCtx.Client.TriggerEvents(projectID).Get(ctx, fullID)
	if err != nil {
		return wrapTriggerHubError(hubCtx, fmt.Errorf("failed to get trigger event: %w", err))
	}

	if triggerEventPayloadOnly {
		fmt.Println(formatTriggerEventPayload(detail.Payload, ""))
		return nil
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(detail)
	}

	subj := detail.Subject
	if subj == "" {
		subj = "-"
	}
	processed := "-"
	if detail.ProcessedAt != nil {
		processed = clitime.Format(*detail.ProcessedAt, clitime.Full)
	}

	fmt.Printf("Event:        %s\n", triggerTerminalText(detail.ID, false))
	fmt.Printf("Type:         %s\n", triggerTerminalText(detail.Type, false))
	fmt.Printf("Source:       %s\n", triggerTerminalText(formatTriggerSource(detail.Source, 0), false))
	fmt.Printf("Subject:      %s\n", triggerTerminalText(subj, false))
	fmt.Printf("Dedup Key:    %s\n", triggerTerminalText(detail.DedupKey, false))
	fmt.Printf("Status:       %s\n", triggerTerminalText(detail.Status, false))
	if detail.Error != "" {
		fmt.Printf("Error:        %s\n", triggerTerminalText(detail.Error, false))
	}
	fmt.Printf("Occurred:     %s\n", clitime.Format(detail.OccurredAt, clitime.Full))
	fmt.Printf("Received:     %s\n", clitime.Format(detail.ReceivedAt, clitime.Full))
	fmt.Printf("Processed:    %s\n", processed)
	fmt.Printf("Matches:      %d\n", detail.MatchCount)
	fmt.Printf("Fires:        %d\n", detail.FireCount)
	fmt.Printf("Payload Size: %d bytes\n", detail.PayloadBytes)

	if len(detail.Attributes) == 0 {
		fmt.Println("Attributes:   (none)")
	} else {
		fmt.Println("Attributes:")
		keys := make([]string, 0, len(detail.Attributes))
		for k := range detail.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s: %s\n", triggerTerminalText(k, false), triggerTerminalText(detail.Attributes[k], false))
		}
	}

	if len(detail.Lists) == 0 {
		fmt.Println("Lists:        (none)")
	} else {
		fmt.Println("Lists:")
		keys := make([]string, 0, len(detail.Lists))
		for k := range detail.Lists {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s: %s\n", triggerTerminalText(k, false), formatTriggerQuotedList(detail.Lists[k]))
		}
	}

	if len(detail.Headers) > 0 {
		fmt.Println("Headers:")
		keys := make([]string, 0, len(detail.Headers))
		for k := range detail.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s: %s\n", triggerTerminalText(k, false), triggerTerminalText(detail.Headers[k], false))
		}
	}

	fmt.Println("Payload:")
	fmt.Println(formatTriggerEventPayload(detail.Payload, "  "))
	return nil
}

func formatTriggerEventPayload(raw json.RawMessage, prefix string) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return prefix + "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, prefix, "  "); err != nil {
		safe := triggerTerminalText(string(trimmed), true)
		if prefix != "" {
			safe = strings.ReplaceAll(safe, "\n", "\n"+prefix)
		}
		return prefix + safe
	}
	return prefix + escapeTriggerJSONBytes(buf.Bytes())
}

func init() {
	triggerEventCmd.AddCommand(triggerEventListCmd)
	triggerEventCmd.AddCommand(triggerEventGetCmd)

	triggerEventListCmd.Flags().StringVar(&triggerEventListSource, "source", "", "Filter by source kind (webhook or github)")
	triggerEventListCmd.Flags().StringVar(&triggerEventListWebhook, "webhook", "", "Filter by webhook endpoint ID or name")
	triggerEventListCmd.Flags().StringVar(&triggerEventListStatus, "status", "", "Filter by event status (pending, processed, or failed)")
	triggerEventListCmd.Flags().StringVar(&triggerEventListSince, "since", "", "Filter events received at or after an RFC3339 timestamp")
	triggerEventListCmd.Flags().IntVar(&triggerEventListLimit, "limit", defaultTriggerEventListLimit, "Maximum number of events to return")
	triggerEventListCmd.Flags().StringVar(&triggerEventListCursor, "cursor", "", "Pagination cursor from a previous event list response")

	triggerEventGetCmd.Flags().BoolVar(&triggerEventPayloadOnly, "payload", false, "Print only the JSON payload")
}
