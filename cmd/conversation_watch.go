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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const (
	watchExitTimeout       = 124
	watchExitCursorExpired = 3
	watchExitInterrupted   = 130

	watchMaxServerWait = 55 * time.Second
)

var (
	convWatchOnce         bool
	convWatchCursor       string
	convWatchAfterMessage string
	convWatchFromStart    bool
	convWatchFilter       string
	convWatchLimit        int
	convWatchTimeout      time.Duration
	convWatchJSON         bool
)

// watchExitError carries a specific non-zero exit code for conversation watch
// (124 on --once timeout, 3 on 410 cursor expired, 130 on interrupt) and
// optionally suppresses the stderr error banner when JSON was already written.
type watchExitError struct {
	code     int
	err      error
	reported bool
}

func (e *watchExitError) Error() string { return e.err.Error() }
func (e *watchExitError) ExitCode() int { return e.code }
func (e *watchExitError) Unwrap() []error {
	if e.reported {
		return []error{e.err, &jsonReportedError{msg: e.err.Error()}}
	}
	return []error{e.err}
}

var conversationWatchCmd = &cobra.Command{
	Use:   "watch <conversation-ref>",
	Short: "Wait for new messages in a conversation",
	Long: `Wait for new messages in a conversation via server-side long-polling.

In --once mode, watch blocks until at least one matching message arrives (or
--timeout elapses), prints the result, and exits.

Exit codes:
  0    At least one message delivered
  124  --once timed out with no matching messages (JSON output still written)
  3    Cursor expired (410 Gone); restart with --after-message or no cursor
  130  Interrupted (SIGINT/SIGTERM) in --once mode
  1    Error (authentication, authorization, invalid flags/cursor, network)

Examples:
  scion conversation watch conv:<uuid> --once --after-message <msg-id> --json
  scion conversation watch @my-agent --once --cursor <cursor> --timeout 2m --json
  scion conversation watch #ops --once --filter addressed --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationWatch,
}

func init() {
	conversationCmd.AddCommand(conversationWatchCmd)

	conversationWatchCmd.Flags().BoolVar(&convWatchOnce, "once", false, "Return as soon as at least one matching message arrives, or on --timeout")
	conversationWatchCmd.Flags().StringVar(&convWatchCursor, "cursor", "", "Resume strictly after an opaque cursor from a previous watch result")
	conversationWatchCmd.Flags().StringVar(&convWatchAfterMessage, "after-message", "", "Resume after a specific message ID in this conversation")
	conversationWatchCmd.Flags().BoolVar(&convWatchFromStart, "from-start", false, "With no cursor or --after-message, replay retained history from the start")
	conversationWatchCmd.Flags().StringVar(&convWatchFilter, "filter", "others", "Message filter: others, all, or addressed")
	conversationWatchCmd.Flags().IntVar(&convWatchLimit, "limit", 50, "Maximum number of messages to return (1-200)")
	conversationWatchCmd.Flags().DurationVar(&convWatchTimeout, "timeout", 0, "Total time budget (0 means wait indefinitely)")
	conversationWatchCmd.Flags().BoolVar(&convWatchJSON, "json", false, "Output as compact JSON")
}

type watchOnceParams struct {
	ConversationID string
	Cursor         string
	AfterMessage   string
	FromStart      bool
	Filter         string
	Limit          int
	Timeout        time.Duration
	JSON           bool
	Stdout         io.Writer
	Stderr         io.Writer
}

func runConversationWatch(cmd *cobra.Command, args []string) error {
	if !convWatchOnce {
		return newUsageError("stream mode without --once is not yet supported; pass --once")
	}
	switch convWatchFilter {
	case "others", "all", "addressed":
	default:
		return newUsageError("invalid --filter %q: must be others, all, or addressed", convWatchFilter)
	}
	if convWatchLimit < 1 || convWatchLimit > 200 {
		return newUsageError("invalid --limit %d: must be between 1 and 200", convWatchLimit)
	}
	if convWatchTimeout < 0 {
		return newUsageError("invalid --timeout %s: must be non-negative", convWatchTimeout)
	}

	jsonOut := convWatchJSON || isJSONOutput()
	if jsonOut {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	// Well-formed invocation: suppress Cobra usage block on runtime errors.
	cmd.SilenceUsage = true

	sigCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resolveCtx, resolveCancel := context.WithTimeout(sigCtx, 30*time.Second)
	conversationID, err := resolveConversationRef(resolveCtx, client, args[0])
	resolveCancel()
	if err != nil {
		if errors.Is(sigCtx.Err(), context.Canceled) {
			return &watchExitError{code: watchExitInterrupted, err: errors.New("interrupted")}
		}
		return err
	}

	return executeConversationWatchOnce(sigCtx, client.Conversations(), watchOnceParams{
		ConversationID: conversationID,
		Cursor:         convWatchCursor,
		AfterMessage:   convWatchAfterMessage,
		FromStart:      convWatchFromStart,
		Filter:         convWatchFilter,
		Limit:          convWatchLimit,
		Timeout:        convWatchTimeout,
		JSON:           jsonOut,
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
	})
}

func executeConversationWatchOnce(ctx context.Context, svc hubclient.ConversationService, params watchOnceParams) error {
	var (
		hasDeadline bool
		deadline    time.Time
	)
	if params.Timeout > 0 {
		hasDeadline = true
		deadline = time.Now().Add(params.Timeout)
	}

	opts := hubclient.WatchOptions{
		Cursor:       params.Cursor,
		AfterMessage: params.AfterMessage,
		Filter:       params.Filter,
		Limit:        params.Limit,
	}
	if opts.Cursor == "" && opts.AfterMessage == "" && params.FromStart {
		opts.Start = "beginning"
	}

	var truncated bool

	for {
		if ctx.Err() != nil {
			return &watchExitError{code: watchExitInterrupted, err: errors.New("interrupted")}
		}

		wait := watchMaxServerWait
		if hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return writeWatchOnceTimeout(params, opts.Cursor, truncated)
			}
			if remaining < wait {
				wait = remaining
			}
		}
		opts.Wait = wait

		res, err := svc.Watch(ctx, params.ConversationID, opts)
		if err != nil {
			if ctx.Err() != nil {
				return &watchExitError{code: watchExitInterrupted, err: errors.New("interrupted")}
			}
			if errors.Is(err, hubclient.ErrCursorExpired) {
				return &watchExitError{code: watchExitCursorExpired, err: err}
			}
			var apiErr *apiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound &&
				strings.Contains(strings.ToLower(apiErr.Message), "route") {
				return fmt.Errorf("this Hub does not support conversation watch (needs a newer Hub or the hub.conversation_watch experiment)")
			}
			return err
		}

		truncated = truncated || res.Truncated

		// Advance cursor for any follow-up poll inside the same --once budget.
		if res.NextCursor != "" {
			opts.Cursor = res.NextCursor
			opts.AfterMessage = ""
			opts.Start = ""
		}

		if len(res.Events) > 0 {
			res.Truncated = truncated
			if params.JSON {
				return writeCompactJSON(params.Stdout, res)
			}
			return writeWatchOncePlain(params.Stdout, params.Stderr, res)
		}

		// Empty result: check whether the total --timeout budget has expired.
		if hasDeadline && time.Until(deadline) <= 0 {
			return writeWatchOnceTimeout(params, res.NextCursor, truncated)
		}
	}
}

func writeWatchOnceTimeout(params watchOnceParams, nextCursor string, truncated bool) error {
	if params.JSON {
		out := &hubclient.WatchResult{
			Events:     []hubclient.WatchEvent{},
			NextCursor: nextCursor,
			Truncated:  truncated,
			TimedOut:   true,
		}
		if wErr := writeCompactJSON(params.Stdout, out); wErr != nil {
			return wErr
		}
		return &watchExitError{
			code:     watchExitTimeout,
			err:      errors.New("watch timed out"),
			reported: true,
		}
	}
	if nextCursor != "" && params.Stderr != nil {
		_, _ = fmt.Fprintf(params.Stderr, "next-cursor: %s\n", nextCursor)
	}
	return &watchExitError{
		code: watchExitTimeout,
		err:  errors.New("watch timed out"),
	}
}

func writeCompactJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func writeWatchOncePlain(stdout, stderr io.Writer, res *hubclient.WatchResult) error {
	for _, evt := range res.Events {
		ts := evt.OccurredAt.UTC().Format("2006-01-02 15:04:05Z")
		sender, recipient, body := "", "", ""
		if evt.Data != nil {
			sender = evt.Data.Sender
			recipient = evt.Data.Recipient
			body = evt.Data.Msg
		}
		if _, err := fmt.Fprintf(stdout, "[%s] %s -> %s\n%s\n", ts, sender, recipient, body); err != nil {
			return err
		}
	}
	if res.NextCursor != "" && stderr != nil {
		_, _ = fmt.Fprintf(stderr, "next-cursor: %s\n", res.NextCursor)
	}
	return nil
}
