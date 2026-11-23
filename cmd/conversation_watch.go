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
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const (
	watchExitTimeout       = 124
	watchExitCursorExpired = 3
	watchExitInterrupted   = 130

	watchMaxServerWait  = 55 * time.Second
	watchInitialBackoff = 1 * time.Second
	watchMaxBackoff     = 30 * time.Second
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

// watchCursorHeartbeat is the NDJSON line written in stream --json mode when
// a long-poll times out with no matching events so consumers can checkpoint.
type watchCursorHeartbeat struct {
	Type       string `json:"type"`
	NextCursor string `json:"nextCursor"`
}

var conversationWatchCmd = &cobra.Command{
	Use:   "watch <conversation-ref>",
	Short: "Wait for new messages in a conversation",
	Long: `Wait for new messages in a conversation via server-side long-polling.

By default, watch runs in stream mode: it stays attached and prints each new
message as it arrives (NDJSON under --json, with cursor heartbeat lines on
empty polls; plain message blocks with next-cursor on stderr at exit).
Reaching --timeout or receiving SIGINT/SIGTERM ends stream mode with exit 0.

In --once mode, watch blocks until at least one matching message arrives (or
--timeout elapses), prints the result, and exits.

Exit codes:
  0    At least one message delivered (--once), or stream ended normally
  124  --once timed out with no matching messages (JSON output still written)
  3    Cursor expired (410 Gone); restart with --after-message or no cursor
  130  Interrupted (SIGINT/SIGTERM) in --once mode
  1    Error (authentication, authorization, invalid flags/cursor, network)

Examples:
  scion conversation watch conv:<uuid>
  scion conversation watch @my-agent --json
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
	conversationWatchCmd.Flags().BoolVar(&convWatchJSON, "json", false, "Output as compact JSON (single object in --once mode, NDJSON in stream mode)")
}

type watchParams struct {
	ConversationID string
	Once           bool
	Cursor         string
	AfterMessage   string
	FromStart      bool
	Filter         string
	Limit          int
	Timeout        time.Duration
	JSON           bool
	Stdout         io.Writer
	Stderr         io.Writer

	// Optional test hooks; nil uses production defaults.
	Sleep   func(ctx context.Context, d time.Duration) error
	Backoff func(attempt int, retryAfter time.Duration) time.Duration
}

func runConversationWatch(cmd *cobra.Command, args []string) error {
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
			if convWatchOnce {
				return &watchExitError{code: watchExitInterrupted, err: errors.New("interrupted")}
			}
			return nil
		}
		return err
	}

	return executeConversationWatch(sigCtx, client.Conversations(), watchParams{
		ConversationID: conversationID,
		Once:           convWatchOnce,
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

func executeConversationWatch(ctx context.Context, svc hubclient.ConversationService, params watchParams) error {
	sleepFn := params.Sleep
	if sleepFn == nil {
		sleepFn = sleepWithContext
	}
	backoffFn := params.Backoff
	if backoffFn == nil {
		backoffFn = watchBackoffDuration
	}

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

	var (
		truncated         bool
		polled            bool
		consecutiveErrors int
		lastCursor        = params.Cursor
	)

	emitStreamCursor := func() {
		if !params.Once && !params.JSON && lastCursor != "" && params.Stderr != nil {
			_, _ = fmt.Fprintf(params.Stderr, "next-cursor: %s\n", lastCursor)
		}
	}

	handleInterrupt := func() error {
		if params.Once {
			return &watchExitError{code: watchExitInterrupted, err: errors.New("interrupted")}
		}
		emitStreamCursor()
		return nil
	}

	for {
		if ctx.Err() != nil {
			return handleInterrupt()
		}

		wait := watchMaxServerWait
		opts.Immediate = false
		if hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				if polled {
					if params.Once {
						return writeWatchOnceTimeout(params, lastCursor, truncated)
					}
					emitStreamCursor()
					return nil
				}
				// Always perform at least one immediate (wait=0) poll so the
				// caller receives any already-committed messages or a valid
				// nextCursor even when --timeout is tiny (e.g. 1ns).
				wait = 0
				opts.Immediate = true
			} else if remaining < wait {
				wait = remaining
			}
		}
		opts.Wait = wait

		res, err := svc.Watch(ctx, params.ConversationID, opts)
		if err != nil {
			if ctx.Err() != nil {
				return handleInterrupt()
			}
			if errors.Is(err, hubclient.ErrCursorExpired) {
				return &watchExitError{code: watchExitCursorExpired, err: err}
			}
			if errors.Is(err, hubclient.ErrInvalidCursor) {
				return err
			}
			if isWatchUnsupportedError(err) {
				return fmt.Errorf("this Hub does not support conversation watch (needs a newer Hub or the hub.conversation_watch experiment)")
			}

			retryable, retryAfter := classifyWatchError(err)
			if !retryable {
				emitStreamCursor()
				return err
			}

			consecutiveErrors++
			delay := backoffFn(consecutiveErrors, retryAfter)
			if hasDeadline {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					if params.Once {
						return err
					}
					emitStreamCursor()
					return nil
				}
				if delay > remaining {
					delay = remaining
				}
			}
			if !params.JSON && params.Stderr != nil {
				_, _ = fmt.Fprintf(params.Stderr, "watch error (retrying in %s): %v\n", delay.Round(time.Millisecond), err)
			}
			if sleepErr := sleepFn(ctx, delay); sleepErr != nil {
				return handleInterrupt()
			}
			if hasDeadline && time.Until(deadline) <= 0 {
				if params.Once {
					return err
				}
				emitStreamCursor()
				return nil
			}
			continue
		}

		polled = true
		consecutiveErrors = 0
		truncated = truncated || res.Truncated

		// Advance cursor for subsequent polls from the last good position.
		if res.NextCursor != "" {
			lastCursor = res.NextCursor
			opts.Cursor = res.NextCursor
			opts.AfterMessage = ""
			opts.Start = ""
		}

		if params.Once {
			if len(res.Events) > 0 {
				res.Truncated = truncated
				if params.JSON {
					return writeNDJSONTo(params.Stdout, res)
				}
				return writeWatchOncePlain(params.Stdout, params.Stderr, res)
			}
			if hasDeadline && time.Until(deadline) <= 0 {
				return writeWatchOnceTimeout(params, lastCursor, truncated)
			}
		} else {
			if len(res.Events) > 0 {
				for _, evt := range res.Events {
					if params.JSON {
						if wErr := writeNDJSONTo(params.Stdout, evt); wErr != nil {
							return wErr
						}
					} else {
						if wErr := writeWatchEventPlain(params.Stdout, evt); wErr != nil {
							return wErr
						}
					}
				}
			} else if params.JSON && res.TimedOut && res.NextCursor != "" {
				if wErr := writeNDJSONTo(params.Stdout, watchCursorHeartbeat{
					Type:       "cursor",
					NextCursor: res.NextCursor,
				}); wErr != nil {
					return wErr
				}
			}

			if hasDeadline && time.Until(deadline) <= 0 {
				emitStreamCursor()
				return nil
			}
		}

		if res.RetryAfter > 0 {
			delay := res.RetryAfter
			if hasDeadline {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					if params.Once {
						return writeWatchOnceTimeout(params, lastCursor, truncated)
					}
					emitStreamCursor()
					return nil
				}
				if delay > remaining {
					delay = remaining
				}
			}
			if sleepErr := sleepFn(ctx, delay); sleepErr != nil {
				return handleInterrupt()
			}
		}
	}
}

// isWatchUnsupportedError reports whether err is a 404 from an older Hub that
// does not implement GET /api/v1/conversations/{id}/watch or a Hub with the
// hub.conversation_watch experiment disabled. Genuine "Conversation not found"
// 404s return false so the caller sees the real missing-conversation error.
func isWatchUnsupportedError(err error) bool {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "route") ||
		strings.Contains(msg, "conversation action") ||
		strings.Contains(msg, "page not found")
}

// classifyWatchError distinguishes transient errors (network failures, 5xx,
// 429) from terminal errors (400, 401, 403, 404, 410, etc.).
func classifyWatchError(err error) (retryable bool, retryAfter time.Duration) {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsRateLimited() || apiErr.IsServerError() {
			var ra time.Duration
			if apiErr.RetryAfterSeconds > 0 {
				ra = time.Duration(apiErr.RetryAfterSeconds) * time.Second
			}
			return true, ra
		}
		return false, 0
	}
	return true, 0
}

// watchBackoffDuration computes exponential jittered backoff (1s base, capped
// at 30s) honoring any server Retry-After hint up to the cap.
func watchBackoffDuration(attempt int, retryAfter time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	base := watchInitialBackoff << shift
	if base > watchMaxBackoff {
		base = watchMaxBackoff
	}
	// Equal jitter in [base/2, base] spreads concurrent retries while keeping
	// a non-zero wait and staying within watchMaxBackoff.
	half := base / 2
	delay := half
	if half > 0 {
		delay += time.Duration(rand.Int64N(int64(half) + 1))
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > watchMaxBackoff {
		delay = watchMaxBackoff
	}
	return delay
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeWatchOnceTimeout(params watchParams, nextCursor string, truncated bool) error {
	if params.JSON {
		out := &hubclient.WatchResult{
			Events:     []hubclient.WatchEvent{},
			NextCursor: nextCursor,
			Truncated:  truncated,
			TimedOut:   true,
		}
		if wErr := writeNDJSONTo(params.Stdout, out); wErr != nil {
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

func writeWatchEventPlain(stdout io.Writer, evt hubclient.WatchEvent) error {
	ts := clitime.Format(evt.OccurredAt, clitime.Full)
	sender, recipient, body := "", "", ""
	if evt.Data != nil {
		sender = evt.Data.Sender
		recipient = evt.Data.Recipient
		body = evt.Data.Msg
	}
	if _, err := fmt.Fprintf(stdout, "[%s] %s -> %s\n%s\n", ts, sender, recipient, body); err != nil {
		return err
	}
	if flusher, ok := stdout.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}
	return nil
}

func writeWatchOncePlain(stdout, stderr io.Writer, res *hubclient.WatchResult) error {
	for _, evt := range res.Events {
		if err := writeWatchEventPlain(stdout, evt); err != nil {
			return err
		}
	}
	if res.NextCursor != "" && stderr != nil {
		_, _ = fmt.Fprintf(stderr, "next-cursor: %s\n", res.NextCursor)
	}
	return nil
}
