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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// attachCmd represents the attach command
var attachCmd = &cobra.Command{
	Use:   "attach <agent>",
	Short: "Attach to an agent's interactive session",
	Long: `Attach your terminal to the interactive tmux session of a running agent.

scion attach needs an interactive terminal on stdin and stdout. Without one
(for example when it runs from a script or a coding harness, or its output is
piped) it fails with an error and a non-zero exit, rather than returning
without attaching.

Detaching:
  Press Ctrl-b, then d (the tmux detach key). The agent keeps running and you
  can attach again later. The docker/podman default Ctrl-p Ctrl-q is not used,
  so Ctrl-p reaches the agent. Podman's detach keys are off. On docker they
  move to Ctrl-\ then Ctrl-^: a lone Ctrl-\ is delayed until the next key,
  and the full sequence ends the attach (the agent keeps running).

Local vs Hub mode:
  With a local project, scion attach runs the container runtime's exec on this
  machine. When the project is linked to a Hub, the session goes through the
  Hub to the runtime broker running the agent, over a WebSocket. Anyone with
  attach permission on the agent can attach. Several people can be attached at
  once and see the same session.

Disconnects:
  When the Hub closes the session with code 4503 (for example a planned relay
  restart), 4504 (a transient failure) or 1011 (an internal error), scion
  attach reconnects by itself, once per close, after a short random delay,
  and the screen redraws (press Ctrl-C during the delay to stop). It stops
  after 3 reconnects in a row whose sessions each ended within a minute. If
  a reconnect fails, or the session ends for any other reason, the command
  exits with a message explaining what happened and what to run next (for
  example scion resume <agent> for a stopped agent, or scion attach <agent>
  to try again).`,
	Example: `  # Attach to a running agent; detach again with Ctrl-b d
  scion attach my-agent

  # Start or resume an agent and attach in one step
  scion start my-agent "task" --attach
  scion resume my-agent --attach`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Args are already validated at this point, so any error returned
		// below is a runtime failure (e.g. an abnormal session close), not a
		// usage error — don't print the usage block for it.
		cmd.SilenceUsage = true

		agentName := api.Slugify(args[0])

		if err := requireAttachTerminal(); err != nil {
			return err
		}

		// Check if Hub is enabled
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return attachViaHub(hubCtx, agentName)
		}

		// Try to resolve project info for better error messages
		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		projectName := config.GetProjectName(projectDir)
		targetProjectPath := projectPath

		// Verify agent exists
		found := false
		if projectDir != "" {
			agentDir := filepath.Join(projectDir, "agents", agentName)
			if _, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err == nil {
				found = true
			}
		}

		if !found {
			// If user didn't specify a project, try global fallback
			if projectPath == "" {
				globalDir, _ := config.GetGlobalDir()
				if globalDir != "" && globalDir != projectDir {
					globalAgentDir := filepath.Join(globalDir, "agents", agentName)
					if _, err := os.Stat(filepath.Join(globalAgentDir, "scion-agent.json")); err == nil {
						found = true
						targetProjectPath = globalDir
						projectName = "global"
						fmt.Printf("Agent '%s' not found in local project, using global agent.\n", agentName)
					}
				}
			}
		}

		if !found {
			return fmt.Errorf("agent '%s' not found in project '%s'", agentName, projectName)
		}

		rt := runtime.GetRuntime(targetProjectPath, profile)

		// Use project-scoped lookup to find the exact container,
		// preventing cross-project collision when agents share a name.
		filter := map[string]string{"scion.name": agentName, "scion.project": projectName}
		agents, listErr := rt.List(context.Background(), filter)
		attachID := agentName
		if listErr == nil && len(agents) > 0 {
			attachID = agents[0].ContainerID
		}

		fmt.Printf("Attaching to agent '%s' (project: %s)...\n", agentName, projectName)
		err = rt.Attach(context.Background(), attachID)
		if err != nil {
			return localAttachError(err, attachID, agentName, projectName)
		}
		return nil
	},
}

// localAttachNotFoundFormats are the "not found" errors the local runtimes
// return from Attach (pkg/runtime docker.go, podman.go, apple_container.go
// and k8s_runtime.go), with %s standing for the ID passed to Attach.
// TestLocalAttachError_RealRuntimeErrors drives the real docker, podman and
// Apple container Attach against an empty container list and checks that
// their errors match, so a runtime rewording fails that test. The k8s pod
// wording is checked only by hand-written strings (its Attach needs a
// cluster client).
var localAttachNotFoundFormats = []string{
	"agent '%s' not found",
	"agent '%s' container not found, it may have exited and been removed",
	"agent '%s' pod not found, it may have been deleted",
}

// localAttachError rewrites a runtime "not found" error from a local attach
// into a message that names the agent and project instead of the container
// ID. Other errors are returned unchanged.
func localAttachError(err error, attachID, agentName, projectName string) error {
	if err == nil {
		return nil
	}
	for _, format := range localAttachNotFoundFormats {
		if err.Error() == fmt.Sprintf(format, attachID) {
			return fmt.Errorf("agent '%s' not found in project '%s': its container may have exited and been removed\n\nCheck with: scion list", agentName, projectName)
		}
	}
	return err
}

// attachTerminalCheck reports whether stdin and stdout are both interactive
// terminals: stdin so keystrokes can be read in raw mode, stdout so the
// remote screen can render. It is a variable so tests can override it.
var attachTerminalCheck = func() bool {
	return stdioAreTerminals(util.IsTerminal(), term.IsTerminal(int(os.Stdout.Fd())))
}

// stdioAreTerminals is the attach terminal rule: both stdin and stdout must
// be terminals.
func stdioAreTerminals(stdinTTY, stdoutTTY bool) bool {
	return stdinTTY && stdoutTTY
}

// errAttachNeedsTerminal is returned when attach is requested without an
// interactive terminal. Without this check the attach would read EOF from
// stdin at once and exit 0 having done nothing, which scripts and coding
// harnesses read as success.
var errAttachNeedsTerminal = errors.New("attach requires an interactive terminal (stdin and stdout must both be a TTY)\n\nRun it from a terminal, or use scion look <agent> to view the session and scion message <agent> to send input")

// requireAttachTerminal fails fast when there is no interactive terminal to
// attach. Every attach entry point (scion attach, start -a, resume -a) calls
// it before doing any work.
func requireAttachTerminal() error {
	if !attachTerminalCheck() {
		return errAttachNeedsTerminal
	}
	return nil
}

func init() {
	rootCmd.AddCommand(attachCmd)
}

// resolveAttachTransportFn is the function used to resolve transport auth for
// the attach WebSocket path. It can be overridden in tests to inject a mock.
var resolveAttachTransportFn func() (transportauth.TokenSource, transportauth.HeaderMode, error) = resolveAttachTransport

// resolveAttachOptions resolves transport auth and builds the AttachOption
// slice for a Hub WebSocket attach. It returns the transport source alongside
// the options so callers can use transportSrc in the token-gate check (an
// application token is only required when transportSrc == nil).
func resolveAttachOptions() ([]wsclient.AttachOption, transportauth.TokenSource, error) {
	transportSrc, transportMode, err := resolveAttachTransportFn()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve transport auth: %w", err)
	}
	var opts []wsclient.AttachOption
	if transportSrc != nil {
		opts = append(opts, wsclient.WithTransport(transportSrc, transportMode))
	}
	return opts, transportSrc, nil
}

// managedAttachErr refuses attach for managed agents (a `managed:`-prefixed
// runtime), which have no terminal to attach. Whether any other agent can
// be attached is the Hub's decision: attachHubSession asks it with the
// preflight (wsclient.AttachToAgent) before dialing.
func managedAttachErr(agentRuntime string) error {
	if strings.HasPrefix(agentRuntime, "managed:") {
		return fmt.Errorf("attach is not supported for managed agents — use scion message and scion look")
	}
	return nil
}

// attachViaHub attaches to an agent via Hub WebSocket connection.
func attachViaHub(hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)

	// Get the project ID for this project
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Get agent details from Hub to verify it exists and is running
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent, err := hubCtx.Client.ProjectAgents(projectID).Get(ctx, agentName)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to get agent '%s': %w", agentName, err))
	}

	// A managed agent can never be attached: say so before the phase, so it
	// is not told to resume first. Every other agent is checked by the Hub
	// preflight once it is running.
	if err := managedAttachErr(agent.Runtime); err != nil {
		return err
	}

	// Check agent lifecycle status - the agent must be running to attach.
	agentPhase, _ := hubAgentPhaseActivity(agent.Phase, agent.Activity, agent.Status)
	if agentPhase != string(state.PhaseRunning) {
		if launchActive(agent) {
			step := ""
			if agent.Launch.Step != "" {
				step = ", step: " + agent.Launch.Step
			}
			return fmt.Errorf("agent '%s' is still launching (phase: %s%s)\n\nWait for it and attach with: scion start %s --attach",
				agentName, agentPhase, step, agentName)
		}
		// Build a helpful error message with available status info
		statusInfo := agentPhase
		if agent.Status != "" && agent.Status != agentPhase {
			statusInfo += fmt.Sprintf(", status: %s", agent.Status)
		}
		if agent.ContainerStatus != "" {
			statusInfo += fmt.Sprintf(", container: %s", agent.ContainerStatus)
		}
		return fmt.Errorf("agent '%s' is not running (phase: %s)\n\n%s",
			agentName, statusInfo, notRunningAttachHint(agentPhase, agentName))
	}

	agentID := agent.ID
	if agentID == "" {
		agentID = agentName // Fall back to name if ID not set
	}
	return attachHubSession(ctx, hubCtx, hubAttachTarget{
		Name:    agentName,
		ID:      agentID,
		Runtime: agent.Runtime,
	})
}

// notRunningAttachHint returns the command that gets a non-running agent to
// a state where it can be attached. A stopped or suspended agent comes back
// with scion resume (scion start would create a new agent run instead).
func notRunningAttachHint(phase, agentName string) string {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseSuspended:
		return fmt.Sprintf("Resume it and attach with: scion resume %s --attach", agentName)
	case state.PhaseStopping:
		return fmt.Sprintf("Wait for it to stop, then resume it with: scion resume %s --attach", agentName)
	case state.PhaseError:
		return fmt.Sprintf("Check what went wrong with: scion logs %s\nThen try: scion resume %s --attach", agentName, agentName)
	default:
		return fmt.Sprintf("Start the agent first with: scion start %s --attach", agentName)
	}
}

// hubAttachTarget identifies the agent a Hub attach dials.
type hubAttachTarget struct {
	// Name is the agent slug, used in messages and hints.
	Name string
	// ID is the agent ID used in the /pty URL.
	ID string
	// Runtime feeds managedAttachErr.
	Runtime string
}

// attachHubSession is the shared Hub attach flow used by scion attach and by
// scion start -a / resume -a, so that both get the same preflight, auth
// resolution, error wrapping and hints. The agent must already be running.
//
// Whether the agent can be attached is decided by the Hub, not from broker
// metadata on the client: wsclient.AttachToAgent asks the Hub's preflight
// first, which picks the broker path or, when the broker's runtime has no
// attach, the agent path. A preflight refusal is described by
// describeAttachPreflight and is not retried.
func attachHubSession(ctx context.Context, hubCtx *HubContext, target hubAttachTarget) error {
	if err := managedAttachErr(target.Runtime); err != nil {
		return err
	}

	// Resolve transport auth for IAP/Cloud Run traversal FIRST — in IAP mode
	// there is no application-level token by design (auth happens via
	// Proxy-Authorization at the transport layer), so we must determine
	// whether transport auth is available before deciding if an app token is
	// required.
	attachOpts, transportSrc, err := resolveAttachOptions()
	if err != nil {
		return err
	}

	// Get access token for WebSocket authentication.
	// In IAP/proxy-auth mode there is no application-level token by design —
	// only require one when no transport source is configured.
	token := getHubAccessToken(hubCtx.Endpoint)
	if token == "" && transportSrc == nil {
		return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
	}

	statusf("Attaching to agent '%s' via Hub...\n", target.Name)

	if err := attachToAgentFn(context.Background(), hubCtx.Endpoint, token, target.ID, attachOpts...); err != nil {
		return attachErrorWithUATHint(describeAttachPreflight(describeAttachClose(err, target.Name), target.Name), token)
	}
	return nil
}

// describeAttachPreflight turns a *wsclient.PTYPreflightError into an
// actionable message for agentName: what the Hub said and what to do next.
// 401, 403 and 404 have their own summaries and reuse the hints of the
// matching close codes (4401, 4403, 4404); 422 means the agent has no runtime broker; 503 is the Hub's
// reason (final when there is no path to the terminal, otherwise
// presented as temporary). The CLI retries none of them. The text keeps
// the "status N" detail, which attachErrorWithUATHint looks for. Any
// other error, or another status, is returned unchanged.
func describeAttachPreflight(err error, agentName string) error {
	var pe *wsclient.PTYPreflightError
	if !errors.As(err, &pe) {
		return err
	}
	// A refusal at a reconnect is described with the close that led to it,
	// by describeAttachClose.
	var reconnectErr *wsclient.PTYReconnectError
	if errors.As(err, &reconnectErr) {
		return err
	}
	// The summaries are the preflight's own (this is often the first
	// attach, and the Hub answered, not the broker); the hints are the
	// close-code ones.
	var msg ptyCloseMessage
	switch pe.Status {
	case http.StatusUnauthorized:
		msg = ptyCloseMessage{Summary: "your Hub credentials are not valid",
			Hint: ptyCloseMessages[wsprotocol.ClosePTYAuthRequired].Hint}
	case http.StatusForbidden:
		msg = ptyCloseMessage{Summary: "you do not have permission to attach to this agent",
			Hint: ptyCloseMessages[wsprotocol.ClosePTYForbidden].Hint}
	case http.StatusNotFound:
		msg = ptyCloseMessage{Summary: "the Hub cannot find the agent",
			Hint: ptyCloseMessages[wsprotocol.ClosePTYAgentNotFound].Hint}
	case http.StatusUnprocessableEntity:
		msg = ptyCloseMessage{
			Summary: "the agent has no runtime broker",
			Hint:    "Check the agent with: scion list",
		}
	case http.StatusServiceUnavailable:
		if pe.NoPath() {
			msg = ptyCloseMessage{
				Summary: wsclient.AttachUnsupportedMessage + ", and the agent has no session that serves a terminal",
				Hint:    ptyCloseTerminalHint,
			}
		} else {
			msg = ptyCloseMessage{Summary: pe.Message, Hint: ptyCloseRetryHint}
			if msg.Summary == "" {
				msg.Summary = "the Hub cannot attach to this agent right now"
			}
		}
	default:
		return err
	}
	return &attachPreflightError{
		msg: fmt.Sprintf("cannot attach to agent '%s': %s (%s)\n\n%s",
			agentName, msg.Summary, pe.Detail(), strings.ReplaceAll(msg.Hint, "{agent}", agentName)),
		err: err,
	}
}

// attachPreflightError is the user-facing form of a preflight refusal. It
// unwraps to the original so callers can still inspect it.
type attachPreflightError struct {
	msg string
	err error
}

func (e *attachPreflightError) Error() string { return e.msg }
func (e *attachPreflightError) Unwrap() error { return e.err }

// attachToAgentFn dials and runs the Hub PTY session. It is a variable so
// tests can substitute a fake session.
var attachToAgentFn = wsclient.AttachToAgent

// ptyCloseMessage is what the CLI tells the user for one PTY close code.
// Summary says what happened. Hint, if set, says what to do next; every
// "{agent}" in it is replaced with the agent name.
type ptyCloseMessage struct {
	Summary string
	Hint    string
}

// ptyCloseInputTooLarge (1009, "message too big") is the code the runtime
// broker closes an attach stream with when client input outruns the agent's
// terminal past its per-stream input buffer (pkg/runtimebroker
// StreamInputLimit). ClassifyPTYClose treats it as terminal.
const ptyCloseInputTooLarge = 1009

// ptyCloseMessages maps PTY close codes (see pkg/wsprotocol pty_close.go) to
// user-facing messages. Codes not listed fall back to a message chosen by
// wsprotocol.ClassifyPTYClose, so a new code needs at most one row here. The
// retry/terminal disposition always comes from ClassifyPTYClose, never from
// this table.
var ptyCloseMessages = map[int]ptyCloseMessage{
	wsprotocol.ClosePTYGoingAway: {
		Summary: "the Hub is shutting down",
	},
	wsprotocol.ClosePTYAbnormal: {
		Summary: "the connection to the Hub dropped without a close message (network loss, or the Hub restarted)",
	},
	wsprotocol.ClosePTYInternalError: {
		Summary: "the Hub or the runtime broker hit an internal error",
	},
	wsprotocol.ClosePTYServiceRestart: {
		Summary: "the Hub is restarting",
	},
	wsprotocol.ClosePTYTryAgainLater: {
		Summary: "the Hub is overloaded",
	},
	ptyCloseInputTooLarge: {
		Summary: "the input was too large for the session (pasted faster than the agent could read it)",
		Hint:    "Paste in smaller chunks, then reattach with: scion attach {agent}",
	},
	wsprotocol.ClosePTYAuthRequired: {
		Summary: "your Hub credentials are no longer valid",
		Hint:    "Log in again with: scion hub auth login",
	},
	wsprotocol.ClosePTYForbidden: {
		Summary: "you no longer have permission to attach to this agent",
		Hint:    "Ask a project owner for attach access to agent '{agent}'",
	},
	wsprotocol.ClosePTYAgentNotFound: {
		Summary: "the runtime broker cannot find the agent or its container",
		Hint:    "Check the agent with: scion list\nIf it is stopped, resume it with: scion resume {agent}",
	},
	wsprotocol.ClosePTYSessionGone: {
		Summary: "the agent's terminal session has ended (the agent exited, or its container stopped or was removed)",
		Hint:    "Check the agent with: scion list\nResume it with: scion resume {agent} --attach",
	},
	wsprotocol.ClosePTYUpstreamUnavailable: {
		Summary: "the Hub lost its connection to the agent's runtime broker, or the agent's session is not ready yet",
	},
	wsprotocol.ClosePTYUpstreamTimeout: {
		Summary: "the runtime broker did not start the session in time, or the Hub hit a transient failure",
	},
	wsprotocol.ClosePTYProtocolError: {
		Summary: "the server rejected the session as a protocol error",
		Hint:    "Check that your scion CLI is up to date, then try again with: scion attach {agent}",
	},
	wsprotocol.ClosePTYCancelled: {
		Summary: "the session was cancelled before it started",
	},
	wsprotocol.ClosePTYSuperseded: {
		Summary: "the server reports that this connection was superseded by a newer one",
	},
}

// Fallback hints by disposition, used when a row has no hint of its own.
const (
	ptyCloseRetryHint    = "This may be temporary; try again with: scion attach {agent}"
	ptyCloseTerminalHint = "Check the agent with: scion list"
)

// describeAttachClose turns a *wsclient.PTYCloseError into an actionable
// message for agentName. Any other error is returned unchanged.
func describeAttachClose(err error, agentName string) error {
	var closeErr *wsclient.PTYCloseError
	if !errors.As(err, &closeErr) {
		return err
	}
	// wsclient makes one automatic reconnect attempt for some close codes
	// (wsprotocol.PTYReconnectTiming). When that attempt failed, describe
	// how it ended: by the reconnect limit, by the new session's own close
	// code if it has one (its hint is the one that applies now), otherwise by
	// the reconnect error.
	note := ""
	hintOverride := ""
	var reconnectErr *wsclient.PTYReconnectError
	if errors.As(err, &reconnectErr) && reconnectErr.Err != nil {
		var second *wsclient.PTYCloseError
		var refusal *wsclient.PTYPreflightError
		if errors.As(reconnectErr.Err, &refusal) {
			// The Hub refused the reconnect at its preflight: give its reason,
			// and the next step for that refusal.
			reason := refusal.Message
			if refusal.NoPath() {
				reason = wsclient.AttachUnsupportedMessage + ", and the agent has no session that serves a terminal"
				hintOverride = ptyCloseTerminalHint
			}
			note = fmt.Sprintf("\nThe Hub refused the automatic reconnect: %s (%s).", reason, refusal.Detail())
		} else if errors.Is(reconnectErr.Err, wsclient.ErrPTYReconnectLimit) {
			note = "\nscion attach " + wsclient.ErrPTYReconnectLimit.Error() + "."
		} else if errors.As(reconnectErr.Err, &second) {
			note = "\nThis close came on the automatic reconnect after " + ptyCloseCodeText(closeErr) + "."
			closeErr = second
		} else {
			note = "\nThe automatic reconnect also failed: " + reconnectErr.Err.Error()
		}
	}
	disposition := wsprotocol.ClassifyPTYClose(closeErr.Code)
	msg, known := ptyCloseMessages[closeErr.Code]
	if !known {
		msg.Summary = "the server ended the session"
	}
	hint := msg.Hint
	if hint == "" {
		if disposition == wsprotocol.DispositionRetry {
			hint = ptyCloseRetryHint
		} else {
			hint = ptyCloseTerminalHint
		}
	}
	if hintOverride != "" {
		hint = hintOverride
	}
	return &attachCloseError{
		msg: fmt.Sprintf("attach to agent '%s' ended: %s (%s)%s\n\n%s",
			agentName, msg.Summary, ptyCloseCodeText(closeErr), note, strings.ReplaceAll(hint, "{agent}", agentName)),
		err: err,
	}
}

// ptyCloseCodeText formats a close as "close code N" or "close code N: reason".
func ptyCloseCodeText(ce *wsclient.PTYCloseError) string {
	code := fmt.Sprintf("close code %d", ce.Code)
	if ce.Reason != "" {
		code += ": " + ce.Reason
	}
	return code
}

// attachCloseError is the user-facing form of a *wsclient.PTYCloseError (or
// a *wsclient.PTYReconnectError wrapping one). It unwraps to the original so
// callers can still inspect the close code.
type attachCloseError struct {
	msg string
	err error
}

func (e *attachCloseError) Error() string { return e.msg }
func (e *attachCloseError) Unwrap() error { return e.err }

// attachErrorWithUATHint appends a hint to a failed Hub WebSocket handshake
// when the presented credential is a user access token (ptone/scion#2122):
// the token may simply lack agent:attach for this agent, which use-time
// authorization re-checks on every handshake independently of what was
// eligible to select at mint time. Does not change the underlying error —
// this only augments the message shown once, here. (The CLI reconnects
// automatically only after a 4503, 4504 or 1011 close; a failed handshake
// ends the command.)
func attachErrorWithUATHint(err error, token string) error {
	if err == nil || !strings.HasPrefix(token, store.UATPrefix) {
		return err
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("status %d", http.StatusForbidden)) {
		return err
	}
	return fmt.Errorf("%w\nhint: this access token may lack agent:attach for this agent (run `scion hub token scopes --project <project>` to check)", err)
}

// resolveAttachTransport resolves transport auth for the attach WebSocket path.
// It checks env vars first, then settings.yaml. Returns (nil, _, nil) when
// transport auth is not configured.
func resolveAttachTransport() (transportauth.TokenSource, transportauth.HeaderMode, error) {
	// Load settings for transport config.
	resolvedPath, _, _ := config.ResolveProjectPath(projectPath)
	settings, _ := config.LoadSettings(resolvedPath)

	var ts *transportauth.TransportSettings
	if settings != nil && settings.Hub != nil && settings.Hub.Transport != nil {
		ts = &transportauth.TransportSettings{
			Mode:     settings.Hub.Transport.Mode,
			Audience: settings.Hub.Transport.Audience,
		}
	}

	return transportauth.FromSettings(ts, adcsource.New)
}
