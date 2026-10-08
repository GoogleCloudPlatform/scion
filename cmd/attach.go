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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
  scion attach does not reconnect. If the connection to the Hub or the runtime
  broker drops, or the agent's session ends, the command exits with a message
  explaining what happened and what to run next (for example scion resume
  <agent> for a stopped agent, or scion attach <agent> to try again).`,
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

// attachUnsupportedErr returns a fixed, explicit error for an agent that has
// no exec/attach/TTY primitive to dial, or nil when attach should proceed to
// dial. It covers managed agents (a `managed:`-prefixed runtime) and any
// agent whose runtime broker advertises its profile — or, failing that, the
// broker as a whole — as EXPLICITLY not supporting attach (see
// attachSupportedByBroker). A runtime whose broker would otherwise reject
// the PTY stream only after the WebSocket upgrade has already happened is
// rejected here instead when the broker's own record is readable, so that
// rejection never reaches the CLI process. Every attach entry point (both
// the direct `scion attach` path and the `scion start -a` / `scion resume
// -a` paths) must call this before dialing.
func attachUnsupportedErr(ctx context.Context, hubCtx *HubContext, agentRuntime, runtimeBrokerID, profile string) error {
	if strings.HasPrefix(agentRuntime, "managed:") {
		return fmt.Errorf("attach is not supported for managed agents — use scion message and scion look")
	}
	supported, unreadable := attachSupportedByBroker(ctx, hubCtx, runtimeBrokerID, profile)
	if unreadable {
		// The broker record could not be read at all (point-GET and the LIST
		// fallback both failed, e.g. a 403/404 the CLI's own principal
		// doesn't have read access to). That is not "the broker said no", it
		// is "this client doesn't know" — a hub-member principal without
		// broker-record read access must not be refused attach client-side
		// for a runtime that does support it. Proceed to dial: the broker's
		// own gate on the dial path (the 4501 close or the 501
		// runtime_attach_unsupported pre-upgrade response) stays the
		// authoritative check and still refuses a genuinely unsupported
		// runtime, with this same fixed message, before any PTY data flows.
		return nil
	}
	if !supported {
		if agentRuntime == "" {
			return fmt.Errorf("attach is not supported for this agent's runtime")
		}
		return fmt.Errorf("attach is not supported for agents on the %s runtime", agentRuntime)
	}
	return nil
}

// attachSupportedByBroker reports whether attach is supported for an agent
// on runtimeBrokerID's profile, read from the Hub's own record of that
// broker: runtimebroker.BrokerProfile.Attach / BrokerCapabilities.Attach,
// mirrored into store.RuntimeBroker at registration and served back by
// hubclient.RuntimeBrokers().Get — the same broker/provider read other CLI
// commands already use (e.g. printAutoResolvedBroker), not a new endpoint.
// When the point-GET can't be read (a 403, a 404, or anything else), it
// falls back to the same broker's entry in the LIST response — a hub-member
// principal can be denied the point-GET yet still see the broker on LIST,
// since LIST applies its own, already-authorized, read-scope boundary
// rather than widening anything here; the GET-vs-LIST authorization
// difference is a separate hub concern.
//
// The named profile's own Attach wins when the broker gave one (from
// whichever read produced the record). When it didn't — an older broker, or
// one whose registration producer has no live runtime instance to ask for a
// non-default profile (see buildBrokerProfiles) — this falls through to the
// broker-wide Capabilities.Attach, and THAT answer is final: true (or the
// whole Capabilities record being absent, an older broker's shape) means
// supported, but an explicit broker-wide false means not, even though the
// specific profile itself said nothing. A profile carrying no signal is not
// itself information; the broker saying "my default runtime doesn't
// support attach" is. This does mean a non-default-type profile on a
// broker whose default runtime opts out of attach is refused before
// dialing even though that specific profile's own runtime might support
// it fine — an accepted, documented cost of not being able to ask a
// specific profile's own runtime without a live instance for it (see
// pkg/runtimebroker's resolver and its own equivalent unknown-profile
// cases): a silent profile on a broker whose broker-wide Capabilities.Attach
// is false refuses.
//
// No broker ID on the agent record defaults to supported: there is nothing
// to read in that case, unlike a broker that answered but had nothing to
// say about that particular profile, and the server-side gate stays the
// authoritative check regardless. A broker ID that neither the point-GET
// nor the LIST fallback could resolve to a record also comes back
// unreadable, for the same reason a 403/404 on either read does: the caller
// (attachUnsupportedErr) treats "could not find out" as unknown and lets
// the dial proceed, rather than refusing client-side on a signal that is
// about this principal's read access, not about the runtime's own attach
// support.
func attachSupportedByBroker(ctx context.Context, hubCtx *HubContext, runtimeBrokerID, profile string) (supported bool, unreadable bool) {
	if hubCtx == nil || hubCtx.Client == nil || runtimeBrokerID == "" {
		return true, false
	}
	broker, err := hubCtx.Client.RuntimeBrokers().Get(ctx, runtimeBrokerID)
	if err != nil || broker == nil {
		// The point-GET error itself never reaches the user-facing message
		// (attachUnsupportedErr's fixed wording carries no raw server text);
		// log it at debug level only, for diagnosability.
		slog.Debug("attach gate: runtime broker point-GET unreadable, falling back to LIST", "broker_id", runtimeBrokerID, "error", err)
		broker, err = findRuntimeBrokerByIDViaList(ctx, hubCtx, runtimeBrokerID)
		if err != nil || broker == nil {
			slog.Debug("attach gate: runtime broker unreadable via LIST fallback too", "broker_id", runtimeBrokerID, "error", err)
			return false, true
		}
	}
	return attachSupportedFromBrokerRecord(broker, profile), false
}

// attachSupportedFromBrokerRecord applies the profile-then-broker-wide
// attach ruling documented on attachSupportedByBroker to a broker record
// that was successfully read, regardless of whether it came from the
// point-GET or the LIST fallback.
func attachSupportedFromBrokerRecord(broker *hubclient.RuntimeBroker, profile string) bool {
	if profile != "" {
		for _, p := range broker.Profiles {
			if p.Name != profile {
				continue
			}
			if p.Attach != nil {
				return *p.Attach
			}
			break // found the profile, but it said nothing; fall through below
		}
	}
	if broker.Capabilities != nil {
		return broker.Capabilities.Attach
	}
	return true
}

// findRuntimeBrokerListMaxPages bounds how many pages
// findRuntimeBrokerByIDViaList will follow before giving up, so a
// misbehaving Hub response (a cursor that never ends, or cycles back on
// itself) can't turn one CLI attach call into an unbounded loop.
const findRuntimeBrokerListMaxPages = 50

// findRuntimeBrokerByIDViaList looks up runtimeBrokerID by paging through
// RuntimeBrokers().List — the fallback attachSupportedByBroker uses when the
// point-GET can't be read — scoped to hubCtx.ProjectID when known. It
// returns (nil, nil) when the list pages are exhausted without a match, the
// page cap is hit, or a cursor repeats; the caller (attachSupportedByBroker)
// treats all three the same as unreadable, which attachUnsupportedErr in
// turn treats as unknown and lets the dial proceed, rather than refusing
// client-side on a signal that is about this principal's read access, not
// about the runtime's own attach support.
func findRuntimeBrokerByIDViaList(ctx context.Context, hubCtx *HubContext, runtimeBrokerID string) (*hubclient.RuntimeBroker, error) {
	opts := &hubclient.ListBrokersOptions{ProjectID: hubCtx.ProjectID}
	seenCursors := map[string]bool{}
	for page := 0; page < findRuntimeBrokerListMaxPages; page++ {
		resp, err := hubCtx.Client.RuntimeBrokers().List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range resp.Brokers {
			if resp.Brokers[i].ID == runtimeBrokerID {
				return &resp.Brokers[i], nil
			}
		}
		if !resp.Page.HasMore() {
			return nil, nil
		}
		if seenCursors[resp.Page.NextCursor] {
			return nil, nil
		}
		seenCursors[resp.Page.NextCursor] = true
		opts.Page.Cursor = resp.Page.NextCursor
	}
	return nil, nil
}

// agentProfileName returns the settings profile an agent was created with,
// or "" when the agent record carries no applied config (an older broker,
// or an agent created before AppliedConfig was tracked) — the "no profile"
// case attachSupportedByBroker falls back to the broker-wide capability for.
func agentProfileName(agent *hubclient.Agent) string {
	if agent == nil || agent.AppliedConfig == nil {
		return ""
	}
	return agent.AppliedConfig.Profile
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

	// Check attach support before the phase: an agent whose runtime can never
	// be attached must not be told to resume first (resuming has side effects
	// and would still end in "attach is not supported").
	if err := attachUnsupportedErr(ctx, hubCtx, agent.Runtime, agent.RuntimeBrokerID, agentProfileName(agent)); err != nil {
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
		Name:     agentName,
		ID:       agentID,
		Runtime:  agent.Runtime,
		BrokerID: agent.RuntimeBrokerID,
		Profile:  agentProfileName(agent),
		// attachUnsupportedErr already ran above, before the phase check.
		GateChecked: true,
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
	// Runtime, BrokerID and Profile feed attachUnsupportedErr. Empty values
	// are meaningful there (they default to "supported").
	Runtime  string
	BrokerID string
	Profile  string
	// GateChecked means the caller already ran attachUnsupportedErr for this
	// agent, so attachHubSession skips a second broker lookup.
	GateChecked bool
}

// attachHubSession is the shared Hub attach flow used by scion attach and by
// scion start -a / resume -a, so that both get the same capability gate, auth
// resolution, error wrapping and hints. The agent must already be running.
func attachHubSession(ctx context.Context, hubCtx *HubContext, target hubAttachTarget) error {
	// Some runtimes have no exec/attach/TTY primitive to dial: managed agents
	// never did, and a runtime that opts out of attach entirely (see
	// attachUnsupportedErr) would otherwise have its broker reject the PTY
	// stream only after the WebSocket upgrade has already happened. Reject
	// here instead, using broker metadata the Hub already serves, so the user
	// gets a fixed, explicit, non-zero-exit error before any WebSocket dial
	// is attempted.
	if !target.GateChecked {
		if err := attachUnsupportedErr(ctx, hubCtx, target.Runtime, target.BrokerID, target.Profile); err != nil {
			return err
		}
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
		return attachErrorWithUATHint(describeAttachClose(err, target.Name), token)
	}
	return nil
}

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
		Summary: "the runtime broker did not start the session in time",
	},
}

// Fallback hints by disposition, used when a row has no hint of its own.
const (
	ptyCloseRetryHint    = "This may be temporary. scion attach does not reconnect automatically; try again with: scion attach {agent}"
	ptyCloseTerminalHint = "Check the agent with: scion list"
)

// describeAttachClose turns a *wsclient.PTYCloseError into an actionable
// message for agentName. Any other error is returned unchanged.
func describeAttachClose(err error, agentName string) error {
	var closeErr *wsclient.PTYCloseError
	if !errors.As(err, &closeErr) {
		return err
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
	code := fmt.Sprintf("close code %d", closeErr.Code)
	if closeErr.Reason != "" {
		code += ": " + closeErr.Reason
	}
	return &attachCloseError{
		msg: fmt.Sprintf("attach to agent '%s' ended: %s (%s)\n\n%s",
			agentName, msg.Summary, code, strings.ReplaceAll(hint, "{agent}", agentName)),
		err: closeErr,
	}
}

// attachCloseError is the user-facing form of a *wsclient.PTYCloseError. It
// unwraps to the original so callers can still inspect the close code.
type attachCloseError struct {
	msg string
	err *wsclient.PTYCloseError
}

func (e *attachCloseError) Error() string { return e.msg }
func (e *attachCloseError) Unwrap() error { return e.err }

// attachErrorWithUATHint appends a hint to a failed Hub WebSocket handshake
// when the presented credential is a user access token (ptone/scion#2122):
// the token may simply lack agent:attach for this agent, which use-time
// authorization re-checks on every handshake independently of what was
// eligible to select at mint time. Does not change the underlying error —
// this only augments the message shown once, here. (The CLI does not
// reconnect; a failed handshake or a dropped session ends the command.)
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
