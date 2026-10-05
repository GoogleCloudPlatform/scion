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

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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

In Hub mode, keys are delivered through the Hub's dedicated keys operation.
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

		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return sendKeysViaHub(hubCtx, agentName, keystrokes)
		}

		return sendKeysLocal(agentName, keystrokes)
	},
}

// ---------------------------------------------------------------------------
// Outcome classification shared by the Hub and local paths, and by JSON and
// human-readable output. #2184 requires that an unknown outcome never says
// "delivered" and never invites a blind retry, and that it stay visibly
// distinct from a definite rejection; #2198 requires JSON mode to produce a
// useful, machine-readable result on failure too, not just on success.
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

// keysResult is what every keys attempt — Hub or local, success or failure —
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

// reportKeysResult is the single exit point for sendKeysViaHub and
// sendKeysLocalWithManager: exactly one JSON result on success or failure
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

// classifyLocalKeysError turns an agent.Manager.SendKeys/SendKeysLocal error
// into a keysResult, matching the return-class table both methods document
// (.design/agent-keys-contract.md §4.3's manager-level taxonomy).
func classifyLocalKeysError(err error) keysResult {
	switch {
	case errors.Is(err, agentkeys.ErrTargetNotFound):
		return keysResult{Outcome: keysOutcomeRejected, Code: string(agentkeys.OutcomeNotFound), Message: "agent not found"}
	case errors.Is(err, agentkeys.ErrAgentNotRunning):
		return keysResult{Outcome: keysOutcomeRejected, Code: string(agentkeys.OutcomeAgentNotRunning), Message: "agent is not running"}
	case errors.Is(err, agentkeys.ErrTerminalNotReady):
		return keysResult{Outcome: keysOutcomeRejected, Code: string(agentkeys.OutcomeTerminalNotReady), Message: "agent's terminal session is not ready"}
	case errors.Is(err, agent.ErrKeysUnsupported):
		return keysResult{Outcome: keysOutcomeRejected, Code: string(agentkeys.OutcomeKeysUnsupported), Message: "this runtime backend does not support keys delivery"}
	case errors.Is(err, agent.ErrKeysNotStarted):
		return keysResult{Outcome: keysOutcomeRejected, Code: string(agentkeys.OutcomeKeysUnavailable), Message: "keys dispatch did not start: " + err.Error()}
	}
	if ve, ok := agentkeys.AsValidationError(err); ok {
		return keysResult{Outcome: keysOutcomeRejected, Code: string(ve.Outcome), Message: ve.Error()}
	}
	// Any other failure — including one that itself wraps a context error —
	// cannot be proven to have happened before delivery began (see
	// agent.Manager.SendKeys's doc comment on ErrKeysNotStarted). Ambiguous,
	// never reported as a plain rejection.
	//
	// This fixed message, not err.Error(), is
	// what JSON mode prints (reportKeysResult's ActionResult.Message is
	// exactly this field) and is the real, independent protection for that
	// output path. It is NOT what protects the human-readable error path:
	// reportKeysResult still wraps the caller's original err via %w into
	// the returned error, and cmd/root.go's Execute prints that. The actual
	// protection there is upstream, in SendKeys/SendKeysLocal's own
	// sanitization (sendKeysDeliveryErrorClass, pkg/agent/manager.go) --
	// err itself never carries backend text by the time it reaches this
	// function. This fixed Message does not depend on that holding; it is
	// a second, independent guarantee for the field it actually controls.
	return keysResult{Outcome: keysOutcomeUnknown, Message: "keys dispatch outcome is unknown; do not retry automatically"}
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

// localKeysScope records which identity dimension resolveLocalKeysTarget
// resolved against — exactly one of HubProjectID/ProjectPath is set,
// mirroring pkg/agent's own keysScope — so sendKeysLocalWithManager calls
// the matching manager entry point (SendKeys vs. SendKeysLocal) without
// re-deriving which one applies.
type localKeysScope struct {
	hubProjectID string
	projectPath  string
}

// sendKeysLocal delivers keystrokes to a local (non-Hub-transport) agent,
// resolving a fresh agent.Manager from the ambient runtime/profile and
// delegating to sendKeysLocalWithManager.
func sendKeysLocal(agentName, keystrokes string) error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)
	defer mgr.Close()

	return sendKeysLocalWithManager(context.Background(), mgr, agentName, keystrokes)
}

// sendKeysLocalWithManager is sendKeysLocal's testable core: it accepts an
// already-constructed agent.Manager so tests can inject a mock runtime and
// observe the exact delivery call (projectID/path, slug, agent_id, and the
// runtime payload), which sendKeysLocal's own runtime.GetRuntime construction
// does not allow.
//
// It calls agent.Manager.SendKeys when the selected project has a
// Hub-linked project ID, or the additive agent.Manager.SendKeysLocal
// (.design/agent-keys-contract.md's local-scope correction, ptone/scion#2468
// finding 3) when it does not — never falling back to an unscoped lookup
// either way, and never retrying one entry point's failure through the
// other.
func sendKeysLocalWithManager(ctx context.Context, mgr agent.Manager, agentName, keystrokes string) error {
	if err, ok := rejectInvalidKeys(agentName, keystrokes); !ok {
		return err
	}

	target, scope, err := resolveLocalKeysTarget(ctx, mgr, agentName)
	if err != nil {
		return reportKeysResult(agentName, keysResult{Outcome: keysOutcomeRejected, Code: string(agentkeys.OutcomeNotFound), Message: err.Error()}, err)
	}

	statusf("Sending keys to agent '%s'...\n", agentName)

	expectedAgentID := target.Labels["agent_id"]
	slug := target.Labels["scion.name"]
	if slug == "" {
		slug = api.Slugify(agentName)
	}

	var sendErr error
	if scope.hubProjectID != "" {
		sendErr = mgr.SendKeys(ctx, scope.hubProjectID, slug, expectedAgentID, keystrokes)
	} else {
		sendErr = mgr.SendKeysLocal(ctx, scope.projectPath, slug, expectedAgentID, keystrokes)
	}
	if sendErr != nil {
		return reportKeysResult(agentName, classifyLocalKeysError(sendErr), sendErr)
	}

	return reportKeysResult(agentName, keysResult{Outcome: keysOutcomeDispatched, AgentID: expectedAgentID}, nil)
}

// resolveLocalKeysTarget finds the single agent `agentName` (by slug) refers
// to, scoped to the local project selected by projectPath (the --project
// flag, or the current working directory) — ambiguity-safe, mirroring
// `scion stop`/`scion delete`'s selectAgentTarget, but additionally
// returning which identity dimension (Hub-linked project ID, or local
// resolved project-config directory path) the call was scoped by, since
// agent.Manager's two keys entry points require exactly one.
//
// When this project has a Hub-linked project ID (VersionedSettings.Hub.
// ProjectID — independent of the Hub endpoint being reachable right now;
// see pkg/agent/run.go's own label population, which this mirrors),
// resolution is scoped by that ID, matching every container's
// "scion.project_id" label. Otherwise — a purely local project, never
// linked to a Hub project, whose containers carry no such label at all — it
// is scoped by the resolved project-config directory path instead
// (config.GetResolvedProjectDir, the same identity recorded on
// "scion.project_path"), per .design/agent-keys-contract.md's Authorization
// section ("Projects without a Hub ID use the existing local project
// identity/filter; ambiguous resolution fails"). Either way, an empty scope
// is never passed through to a project-blind lookup.
func resolveLocalKeysTarget(ctx context.Context, mgr agent.Manager, agentName string) (api.AgentInfo, localKeysScope, error) {
	slug := api.Slugify(agentName)

	resolvedProjectDir, _ := config.GetResolvedProjectDir(projectPath)
	var hubProjectID, projectName string
	if resolvedProjectDir != "" {
		projectName = config.GetProjectName(resolvedProjectDir)
		if settings, _, err := config.LoadEffectiveSettings(resolvedProjectDir); err == nil && settings != nil && settings.Hub != nil {
			hubProjectID = settings.Hub.ProjectID
		}
	}

	var filter map[string]string
	var scope localKeysScope
	switch {
	case hubProjectID != "":
		filter = map[string]string{"scion.name": slug, projectkeys.LabelProjectID: hubProjectID}
		scope = localKeysScope{hubProjectID: hubProjectID}
	case resolvedProjectDir != "":
		filter = map[string]string{"scion.name": slug, projectkeys.LabelProjectPath: resolvedProjectDir}
		scope = localKeysScope{projectPath: resolvedProjectDir}
	default:
		// No Hub link AND no resolvable project directory at all (e.g. run
		// outside any scion project with no --project given): there is no
		// stable identity to scope this call to, so this is a hard refusal
		// rather than an unscoped lookup.
		return api.AgentInfo{}, localKeysScope{}, fmt.Errorf(
			"could not resolve a project for agent '%s'; run inside a scion project or pass --project", agentName)
	}

	agents, err := mgr.List(ctx, filter)
	if err != nil {
		return api.AgentInfo{}, localKeysScope{}, err
	}

	candidates := agent.DedupeByContainerID(agents)
	switch len(candidates) {
	case 0:
		if projectName != "" {
			return api.AgentInfo{}, localKeysScope{}, fmt.Errorf("agent '%s' not found in project %q", agentName, projectName)
		}
		return api.AgentInfo{}, localKeysScope{}, fmt.Errorf("agent '%s' not found", agentName)
	case 1:
		return candidates[0], scope, nil
	default:
		return api.AgentInfo{}, localKeysScope{}, fmt.Errorf(
			"agent '%s' is ambiguous: %d containers match in project %q", agentName, len(candidates), projectName)
	}
}

func init() {
	rootCmd.AddCommand(keysCmd)
}
