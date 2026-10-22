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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/spf13/cobra"
)

// keysCmd represents the keys command
var keysCmd = &cobra.Command{
	Use:   "keys <agent-name> <keystrokes>",
	Short: "Send raw keystrokes to an agent's terminal",
	Long: `Sends literal bytes to an agent's terminal via tmux send-keys,
with no trailing Enter. Supports control keys like arrows, Escape, etc.

This is useful for interacting with interactive TUI applications running
inside an agent's terminal session.

Keys are delivered through the Hub's dedicated keys operation.
This replaces the removed 'scion message --raw' flag. When run by an agent,
this only works within the agent's own project; cross-project targets are
refused. A human operator using --project can still target other projects.

Each invocation delivers exactly one argument to tmux: each example below is
a separate call, not a sequence. A whole argument that matches a recognized
tmux key name (like "Enter" or "Escape") is sent as that key; any other text
is typed literally, character by character, including spaces — and nothing
ever adds a trailing Enter for you.

Examples:
  scion keys my-agent "Escape"
  scion keys my-agent "C-c"
  scion keys my-agent Up
  scion keys my-agent Enter`,
	Args: cobra.ExactArgs(2),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return getAgentNames(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Resolve the agent exactly as `scion message` does: strip the
		// optional "agent:" prefix and slugify.
		agentName := api.Slugify(strings.TrimPrefix(args[0], "agent:"))
		keystrokes := strings.Join(args[1:], " ")

		// keys must not work as a cross-project command. This CLI check is
		// UX only; the authoritative refusal is hub-side (ExecuteAgentKeys).
		if crossProjectTarget := detectCrossProjectTarget(cmd); crossProjectTarget != "" {
			return fmt.Errorf("scion keys does not support cross-project targets; message the agent from within its own project")
		}

		hubCtx, err := requireHub(projectPath)
		if err != nil {
			return err
		}

		return sendKeysViaHub(hubCtx, agentName, keystrokes)
	},
}

// ---------------------------------------------------------------------------
// Outcome classification shared by JSON and human-readable output. #2184
// requires that an unknown outcome never says "delivered" and never invites
// a blind retry, and that it stay visibly distinct from a definite
// rejection; #2198 requires JSON mode to produce a useful, machine-readable
// result on failure too, not just on success.
// ---------------------------------------------------------------------------

// keysOutcomeStatus is the three-way classification every keys attempt
// resolves to.
type keysOutcomeStatus string

const (
	// keysOutcomeDispatched means the broker acknowledged terminal
	// injection — not that the harness consumed it (contract §2.4).
	keysOutcomeDispatched keysOutcomeStatus = "dispatched"
	// keysOutcomeRejected means the attempt definitely did not inject
	// anything, for the stated reason (not found, not running, denied,
	// invalid, unsupported, ...). Safe to correct and retry.
	keysOutcomeRejected keysOutcomeStatus = "rejected"
	// keysOutcomeUnknown means the attempt may or may not have reached the
	// terminal. Never safe to blindly retry, for the opposite reason from
	// "rejected": a retry here could be a second, unwanted injection.
	keysOutcomeUnknown keysOutcomeStatus = "unknown"
)

// keysResult is what every keys attempt — success or failure —
// reports, in both JSON and human-readable form.
type keysResult struct {
	Outcome     keysOutcomeStatus
	Code        string // machine outcome code, when known (e.g. "agent_not_running")
	OperationID string // present only when the Hub minted and returned one
	AgentID     string
	Message     string // human-readable detail; never normative (contract §2.4a)
	// RetryAfterSeconds is the Hub's Retry-After value (seconds), when the
	// response carried one (429 keys_rate_limited; also present on some 503
	// keys_unavailable responses). 0 means none was present — this is
	// informational only: the CLI never auto-retries regardless (contract
	// C§2.4a: "describes admission, not permission to replay").
	RetryAfterSeconds int
}

// reportKeysResult is the single exit point for sendKeysViaHub: exactly one
// JSON result on success or failure
// (contract: "JSON mode emits exactly one parseable result"), or a
// human-readable success line only — a failure's text comes solely from the
// returned error, which cmd/root.go already prints to stderr, so this never
// prints a failure line itself (would otherwise duplicate that banner).
//
// cause, when non-nil, is wrapped (%w) so errors.As can still recover it
// (e.g. the original *apiclient.APIError, with Code/Details intact).
func reportKeysResult(agentName string, res keysResult, cause error) error {
	if isJSONOutput() {
		details := map[string]interface{}{"outcome": string(res.Outcome)}
		if res.Code != "" {
			details["code"] = res.Code
		}
		if res.OperationID != "" {
			details["operation_id"] = res.OperationID
		}
		if res.AgentID != "" {
			details["agent_id"] = res.AgentID
		}
		if res.RetryAfterSeconds > 0 {
			details["retry_after_seconds"] = res.RetryAfterSeconds
		}
		status := "success"
		if res.Outcome != keysOutcomeDispatched {
			status = "error"
		}
		if jsonErr := outputJSON(ActionResult{
			Status:  status,
			Command: "keys",
			Agent:   agentName,
			Message: res.Message,
			Details: details,
		}); jsonErr != nil {
			return jsonErr
		}
	} else if res.Outcome == keysOutcomeDispatched {
		// Failure text is deliberately not printed here: the error this
		// function returns below carries it, and cmd/root.go's Execute
		// already prints every returned error to stderr — printing it again
		// here would duplicate that line.
		fmt.Printf("Keys dispatched to agent '%s' (terminal injection acknowledged; this does not confirm the agent processed it).\n", agentName)
	}

	if res.Outcome == keysOutcomeDispatched {
		return nil
	}

	// An unknown outcome carries its own hint in the returned error (the
	// only place failure text appears, in both JSON and text mode): never
	// suggest delivery, and never invite a blind resend, since a retry here
	// could be a second, unwanted injection.
	suffix := ""
	if res.Outcome == keysOutcomeUnknown {
		suffix = " — the terminal may or may not have received them; check before resending"
	}
	if res.RetryAfterSeconds > 0 {
		// Informational only: this never triggers an automatic retry here
		// or anywhere upstream (contract C§2.4a: Retry-After "describes
		// admission, not permission to replay"). Phrased with "do not
		// retry", the one allowed form, never a bare "retry" that could
		// read as an invitation.
		suffix += fmt.Sprintf(" (Retry-After: %ds — do not retry automatically)", res.RetryAfterSeconds)
	}
	if cause != nil {
		if res.Message == "" || res.Message == cause.Error() {
			return fmt.Errorf("keys %s for agent '%s'%s: %w", res.Outcome, agentName, suffix, cause)
		}
		return fmt.Errorf("keys %s for agent '%s': %s%s: %w", res.Outcome, agentName, res.Message, suffix, cause)
	}
	return fmt.Errorf("keys %s for agent '%s': %s%s", res.Outcome, agentName, res.Message, suffix)
}

// classifyHubKeysError turns a hubclient.AgentService.SendKeys error into a
// keysResult. err is always non-nil here (the caller only invokes this on
// SendKeys's error path).
func classifyHubKeysError(err error) keysResult {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		// No structured response was ever received (connection
		// refused/reset, DNS failure, context deadline, ...): the caller
		// cannot know whether the broker executed the keys. Genuinely
		// unknown, never reclassified as a rejection.
		return keysResult{
			Outcome: keysOutcomeUnknown,
			Message: "no response was received: " + err.Error(),
		}
	}

	opID, _ := apiErr.Details["operation_id"].(string)

	// A 404/405 carrying no operation_id cannot be keys' own not_found: the
	// contract mints one for every outcome from validation onward (§2.5's
	// OperationID policy). This shape also covers AK-21e's project-not-found
	// 404 on a current Hub (also no operation_id), so the wording below
	// names both rather than asserting the Hub is necessarily old.
	if (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) && opID == "" {
		return keysResult{
			Outcome: keysOutcomeRejected,
			Code:    "hub_unsupported",
			Message: "this Hub does not support the keys operation, or the project was not found",
		}
	}

	// Contract §2.5: a 4xx, or a 503 whose Code is keys_unavailable, is the
	// only shape that proves no delivery was attempted. Everything else —
	// including a bare 5xx with no parseable body at all (a load balancer's
	// HTML 502/504, a proxy timeout, a 500 that never reached the keys
	// handler) — leaves open that the Hub already dispatched, so it must
	// classify as unknown, never a "safe to retry" rejection.
	isDefiniteRejection := apiErr.StatusCode >= 400 && apiErr.StatusCode < 500
	if apiErr.StatusCode == http.StatusServiceUnavailable && apiErr.Code == string(agentkeys.OutcomeKeysUnavailable) {
		isDefiniteRejection = true
	}
	// The Hub's dispatch-error classifier never returns an empty code, but a
	// proxy or load balancer between the CLI and the Hub can answer on its
	// behalf (e.g. a bodyless 502). apiclient.ParseErrorResponse then fills
	// in a generic status-derived code such as internal_error. Never surface
	// an empty or generic code for an ambiguous outcome: a non-definite
	// status whose code is not a keys outcome and that carries no
	// operation_id (so it never came from the keys handler) is reported as
	// keys_outcome_unknown.
	code := apiErr.Code
	message := apiErr.Message
	if message == "" {
		message = fmt.Sprintf("HTTP %d %s with no error body", apiErr.StatusCode, http.StatusText(apiErr.StatusCode))
	}
	if !isDefiniteRejection {
		_, isKeysOutcome := agentkeys.HTTPStatus(agentkeys.Outcome(code))
		if code == "" || (!isKeysOutcome && opID == "") {
			message = notFromKeysHandlerMessage(apiErr)
			code = string(agentkeys.OutcomeKeysOutcomeUnknown)
		}
		return keysResult{Outcome: keysOutcomeUnknown, Code: code, OperationID: opID, Message: message, RetryAfterSeconds: apiErr.RetryAfterSeconds}
	}
	if code == "" {
		code = fmt.Sprintf("http_%d", apiErr.StatusCode)
	}
	return keysResult{Outcome: keysOutcomeRejected, Code: code, OperationID: opID, Message: message, RetryAfterSeconds: apiErr.RetryAfterSeconds}
}

// notFromKeysHandlerMessage describes an ambiguous error response that did
// not come from the Hub's keys handler (no keys outcome code, no
// operation_id): typically a proxy or gateway answering for the Hub. The
// response's own message is kept only when it says more than the status
// text.
func notFromKeysHandlerMessage(apiErr *apiclient.APIError) string {
	statusText := http.StatusText(apiErr.StatusCode)
	msg := fmt.Sprintf("HTTP %d %s with no keys outcome from the Hub (possibly a proxy or gateway response)", apiErr.StatusCode, statusText)
	if detail := strings.TrimSpace(apiErr.Message); detail != "" && detail != statusText {
		msg += ": " + detail
	}
	return msg
}

// rejectInvalidKeys validates keys via agentkeys.ValidateKeys and, if
// invalid, reports the rejection through reportKeysResult before any
// network/List call. ok is false when keys was invalid, in which case the
// caller must return err immediately.
func rejectInvalidKeys(agentName, keys string) (err error, ok bool) {
	verr := agentkeys.ValidateKeys(keys)
	if verr == nil {
		return nil, true
	}
	code := string(agentkeys.OutcomeInvalidRequest)
	if ve, isVE := agentkeys.AsValidationError(verr); isVE {
		code = string(ve.Outcome)
	}
	reportErr := reportKeysResult(agentName, keysResult{Outcome: keysOutcomeRejected, Code: code, Message: verr.Error()}, verr)
	return reportErr, false
}

// sendKeysViaHub delivers keystrokes to a hub-managed agent through the
// dedicated agent-keys operation (.design/agent-keys-contract.md): it POSTs
// {"keys": ...} to the project-scoped /keys route via
// AgentService.SendKeys and never goes through /message.
func sendKeysViaHub(hubCtx *HubContext, agentName, keys string) error {
	if err, ok := rejectInvalidKeys(agentName, keys); !ok {
		return err
	}

	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	// Contract C§3 "ambiguous resolution fails rather
	// than guessing" (ptone/scion#2200): unlike GetProjectID's default behavior (shared by
	// every other command, left unchanged), the keys path fails closed
	// when several projects match the git remote, instead of silently
	// picking the first one.
	projectID, err := getProjectIDForKeys(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	statusf("Sending keys to agent '%s'...\n", agentName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := agentSvc.SendKeys(ctx, agentName, keys)
	if err != nil {
		// Never fall back to the legacy raw-message path on any failure,
		// including "this Hub predates /keys" — a new client talking to an
		// old Hub must fail clearly, not silently retry through /message
		// (.design/agent-keys-contract.md, "Raw compatibility and removal").
		return reportKeysResult(agentName, classifyHubKeysError(err), err)
	}

	return reportKeysResult(agentName, keysResult{
		Outcome:     keysOutcomeDispatched,
		OperationID: resp.OperationID,
		AgentID:     resp.AgentID,
	}, nil)
}

func init() {
	rootCmd.AddCommand(keysCmd)
}
