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
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/spf13/cobra"
)

// attachCmd represents the attach command
var attachCmd = &cobra.Command{
	Use:   "attach <agent>",
	Short: "Attach to an agent's interactive session",
	Long: `Attach to the interactive session of a running agent.
If the agent was started with tmux support, this will attach to the tmux session.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Args are already validated at this point, so any error returned
		// below is a runtime failure (e.g. an abnormal session close), not a
		// usage error — don't print the usage block for it.
		cmd.SilenceUsage = true

		agentName := api.Slugify(args[0])

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
			// If the error is "not found", we can augment it with project info
			if err.Error() == fmt.Sprintf("agent '%s' not found", attachID) ||
				err.Error() == fmt.Sprintf("agent '%s' container not found. It may have exited and been removed.", attachID) {
				return fmt.Errorf("agent '%s' not found in project '%s'", agentName, projectName)
			}
			return err
		}
		return nil
	},
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

	// Some runtimes have no exec/attach/TTY primitive to dial: managed agents
	// never did, and a runtime that opts out of attach entirely (see
	// attachUnsupportedErr) would otherwise have its broker reject the PTY
	// stream only after the WebSocket upgrade has already happened, so that
	// rejection never reaches this CLI process. Reject here instead, using
	// the broker metadata already reachable from the agent GET above, so the
	// user gets a fixed, explicit, non-zero-exit error before any WebSocket
	// dial is attempted.
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
		statusInfo := agent.Status
		if statusInfo == "" {
			statusInfo = "unknown"
		}
		if agent.ContainerStatus != "" {
			statusInfo += fmt.Sprintf(", container: %s", agent.ContainerStatus)
		}
		return fmt.Errorf("agent '%s' is not running (phase: %s)\n\nStart the agent first with: scion start %s",
			agentName, statusInfo, agentName)
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

	fmt.Printf("Attaching to agent '%s' via Hub...\n", agentName)

	// Use agent UUID for the PTY endpoint.
	agentID := agent.ID
	if agentID == "" {
		agentID = agentName // Fall back to name if ID not set
	}

	if err := wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID, attachOpts...); err != nil {
		return attachErrorWithUATHint(err, token)
	}
	return nil
}

// attachErrorWithUATHint appends a hint to a failed Hub WebSocket handshake
// when the presented credential is a user access token (ptone/scion#2122):
// the token may simply lack agent:attach for this agent, which use-time
// authorization re-checks on every handshake independently of what was
// eligible to select at mint time. Does not change the underlying error or
// the reconnect logic — this only augments the message shown once, here.
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
