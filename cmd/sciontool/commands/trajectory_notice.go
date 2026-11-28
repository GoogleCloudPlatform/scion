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

package commands

import (
	"context"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// trajectoryNoticeFetchTimeout strictly bounds the best-effort Hub GET used
// to display the first-line tmux capture notice so startup never blocks on a
// slow or unreachable Hub.
const trajectoryNoticeFetchTimeout = 2 * time.Second

// tmuxAgentWindowShPrefix is the canonical prefix built by
// pkg/runtime.buildTmuxStartCmd when wrapping the harness in the "agent" window.
const tmuxAgentWindowShPrefix = "tmux new-session -d -s scion -n agent sh -c '"

type trajectoryPolicyFetcher interface {
	IsConfigured() bool
	GetTrajectoryCapturePolicy(ctx context.Context) (*telemetrycontract.TrajectoryCapturePolicyStatus, error)
}

// fetchTrajectoryCaptureNotice performs a time-bounded best-effort fetch of
// the agent's effective Track 2 capture policy and returns the canonical notice
// string when capture is effective ("full" or "metadata_only"). Returns "" on
// any error, timeout, unconfigured client, experiment-off response, or "off"
// policy.
func fetchTrajectoryCaptureNotice(ctx context.Context, client trajectoryPolicyFetcher, timeout time.Duration) string {
	if client == nil || !client.IsConfigured() {
		return ""
	}
	if timeout <= 0 {
		timeout = trajectoryNoticeFetchTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	status, err := client.GetTrajectoryCapturePolicy(fetchCtx)
	if err != nil {
		log.Debug("Trajectory capture notice fetch skipped: %v", err)
		return ""
	}
	if status == nil || !telemetrycontract.IsTrajectoryCaptureEffective(status.Policy) {
		return ""
	}
	return telemetrycontract.TrajectoryCaptureNotice(*status)
}

// escapeForDoubleQuotedInSingleQuotes escapes notice so it can be placed inside
// double quotes within an outer single-quoted sh -c '...' argument.
func escapeForDoubleQuotedInSingleQuotes(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
		`'`, ``,
		"\n", " ",
		"\r", "",
	)
	return r.Replace(s)
}

// canInjectTmuxNotice reports whether childArgs has the literal inline tmux
// agent-window command shape that injectTmuxTrajectoryNotice can safely rewrite:
//
//	sh -c "tmux new-session -d -s scion -n agent sh -c '<harness>...' ..."
//
// Runtime coverage (best-effort surface; CLI, web agent detail, and web
// terminal are the required surfaces):
//   - Receives the tmux first-line banner: Docker and Podman runtimes without
//     FUSE mounts (pkg/runtime/common.go buildTmuxStartCmd).
//   - Skipped (returns false, no Hub GET is made): Cloud Run, Cloud Run sandbox,
//     and Substrate (which invoke `/bin/sh -c ...`), Kubernetes (which runs
//     `sh -c "$SCION_START_CMD"`), and Docker/Podman with FUSE mounts (which
//     also pass the command via `$SCION_START_CMD`).
func canInjectTmuxNotice(childArgs []string) bool {
	if len(childArgs) < 3 {
		return false
	}
	if childArgs[0] != "sh" || childArgs[1] != "-c" {
		return false
	}
	return strings.Contains(childArgs[2], tmuxAgentWindowShPrefix)
}

// maybeInjectTmuxTrajectoryNotice checks canInjectTmuxNotice(childArgs) before
// contacting the Hub so runtimes whose command shape cannot accept the banner
// never incur Hub fetch latency. When injectable and capture is effective, it
// returns childArgs with the notice printf prepended inside the tmux agent
// window; otherwise it returns childArgs unchanged.
func maybeInjectTmuxTrajectoryNotice(ctx context.Context, client trajectoryPolicyFetcher, childArgs []string, timeout time.Duration) []string {
	if !canInjectTmuxNotice(childArgs) {
		return childArgs
	}
	notice := fetchTrajectoryCaptureNotice(ctx, client, timeout)
	if notice == "" {
		return childArgs
	}
	return injectTmuxTrajectoryNotice(childArgs, notice)
}

// injectTmuxTrajectoryNotice prepends a best-effort printf of notice as the
// first line printed in the tmux "agent" window before the harness starts and
// accepts input. Leaves childArgs untouched if notice is empty or
// canInjectTmuxNotice(childArgs) is false.
func injectTmuxTrajectoryNotice(childArgs []string, notice string) []string {
	if notice == "" || !canInjectTmuxNotice(childArgs) {
		return childArgs
	}
	idx := strings.Index(childArgs[2], tmuxAgentWindowShPrefix)
	insertAt := idx + len(tmuxAgentWindowShPrefix)
	escaped := escapeForDoubleQuotedInSingleQuotes(notice)
	prefix := `printf "%s\n" "Notice: ` + escaped + `"; `

	out := make([]string, len(childArgs))
	copy(out, childArgs)
	out[2] = childArgs[2][:insertAt] + prefix + childArgs[2][insertAt:]
	return out
}
