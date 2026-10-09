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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Ephemeral workspace warnings (ptone/scion#3819).
//
// A Kubernetes agent whose workspace is not on the shared NFS export runs
// with an EmptyDir at /workspace: every new pod starts with an empty
// workspace, which sciontool init re-clones. Unpushed commits and changed
// files of the previous pod are gone after a stop, suspend or restart. The
// hub does not prevent that; it checks the workspace just before the pod is
// removed, warns in the stop response, records what it found on the agent,
// and repeats the result when the agent is next started.

// workspaceAtStopAnnotation is the agent annotation holding the result of
// the last pre-stop workspace check (an ephemeralWorkspaceRecord as JSON).
const workspaceAtStopAnnotation = "scion.dev/workspace-at-stop"

const (
	// workspaceCheckExecTimeoutSeconds is the timeout sent with the exec
	// to the broker.
	workspaceCheckExecTimeoutSeconds = 5
	// workspaceCheckMaxOutput caps how much of the exec output is parsed.
	workspaceCheckMaxOutput = 4096
	// workspaceCheckMarker starts the one line the check script prints.
	workspaceCheckMarker = "scion-workspace-check"
)

// workspaceCheckTimeout bounds the whole check on the hub side, the exec
// and the write of its result together, so a stop never waits longer than
// this for it. A variable so tests can shorten it.
var workspaceCheckTimeout = 6 * time.Second

// workspaceRecordReserve is the share of workspaceCheckTimeout kept for
// writing the result after the exec (1s of the default 6s).
func workspaceRecordReserve() time.Duration {
	return workspaceCheckTimeout / 6
}

// workspaceClearTimeout bounds the write that clears the record outside the
// stop's check (after a start, or a stop that failed).
const workspaceClearTimeout = 5 * time.Second

// workspaceCheckScript prints "scion-workspace-check commits=N files=M":
// N is the number of commits on HEAD not on its upstream (or, without an
// upstream, not on any remote-tracking ref), M the number of changed and
// untracked paths. It exits non-zero when /workspace is not a git checkout.
const workspaceCheckScript = `cd /workspace 2>/dev/null || exit 3
g() { git -c safe.directory='*' "$@"; }
g rev-parse --git-dir >/dev/null 2>&1 || exit 3
c=$(g rev-list --count '@{upstream}..HEAD' 2>/dev/null) || c=$(g rev-list --count HEAD --not --remotes 2>/dev/null) || c=0
f=$(g status --porcelain 2>/dev/null | wc -l)
echo "` + workspaceCheckMarker + ` commits=$c files=$f"`

// workspaceCheckCommand is the exec command for the pre-stop check.
func workspaceCheckCommand() []string {
	return []string{"sh", "-c", workspaceCheckScript}
}

// ephemeralWorkspaceRecord is the stored result of a pre-stop check.
// Unchecked is set when the check could not run or its output could not be
// read.
type ephemeralWorkspaceRecord struct {
	Commits   int  `json:"commits,omitempty"`
	Files     int  `json:"files,omitempty"`
	Unchecked bool `json:"unchecked,omitempty"`
}

// hasWork reports whether the record names unpushed commits or changed
// files.
func (r ephemeralWorkspaceRecord) hasWork() bool {
	return !r.Unchecked && (r.Commits > 0 || r.Files > 0)
}

// hasEphemeralWorkspace reports whether agent's workspace is an EmptyDir
// that a new pod re-clones: a Kubernetes agent whose broker reported a
// local (non-export) workspace placement on its last start. Any other
// runtime, an export placement, or an unknown placement is not.
func hasEphemeralWorkspace(agent *store.Agent) bool {
	return agent != nil && isKubernetesRuntimeType(agent.Runtime) &&
		agent.WorkspacePlacement == api.WorkspacePlacementLocal
}

// parseWorkspaceCheckOutput reads the check script's output. ok is false
// when the marker line is missing or malformed.
func parseWorkspaceCheckOutput(output string) (commits, files int, ok bool) {
	if len(output) > workspaceCheckMaxOutput {
		output = output[:workspaceCheckMaxOutput]
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != workspaceCheckMarker {
			continue
		}
		c, cok := parseCountField(fields[1], "commits=")
		f, fok := parseCountField(fields[2], "files=")
		if cok && fok {
			return c, f, true
		}
	}
	return 0, 0, false
}

func parseCountField(field, prefix string) (int, bool) {
	v, found := strings.CutPrefix(field, prefix)
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// plural formats n with noun, adding "s" unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// describeWork names the unpushed commits and changed files of r, leaving
// out a zero count.
func describeWork(r ephemeralWorkspaceRecord) string {
	var parts []string
	if r.Commits > 0 {
		parts = append(parts, plural(r.Commits, "unpushed commit"))
	}
	if r.Files > 0 {
		parts = append(parts, plural(r.Files, "changed file"))
	}
	return strings.Join(parts, " and ")
}

// ephemeralWorkspaceStopWarning is the stop and suspend warning for a
// check that found work; "" when it found none or did not run.
func ephemeralWorkspaceStopWarning(r ephemeralWorkspaceRecord) string {
	if !r.hasWork() {
		return ""
	}
	them := "them"
	if r.Commits+r.Files == 1 {
		them = "it"
	}
	return fmt.Sprintf("Workspace is ephemeral and will be re-cloned on next start; %s will be lost. Push first to keep %s.", describeWork(r), them)
}

// ephemeralWorkspaceRecloneNotice is the start warning when no usable
// record of the previous run's workspace exists.
const ephemeralWorkspaceRecloneNotice = "Workspace is ephemeral and is re-cloned on start; local changes from the previous run are not kept."

// ephemeralWorkspaceStartWarning is the start warning for the recorded
// result rec (nil when there is no record): what was lost, the generic
// notice when nothing is known, or "" when the previous run's workspace was
// clean.
func ephemeralWorkspaceStartWarning(rec *ephemeralWorkspaceRecord) string {
	if rec == nil || rec.Unchecked {
		return ephemeralWorkspaceRecloneNotice
	}
	if !rec.hasWork() {
		return ""
	}
	were := "were"
	if rec.Commits+rec.Files == 1 {
		were = "was"
	}
	return fmt.Sprintf("Workspace is ephemeral and was re-cloned; %s from the previous run %s lost.", describeWork(*rec), were)
}

// recordedWorkspaceAtStop returns the agent's recorded pre-stop result, or
// nil when there is none or it cannot be read.
func recordedWorkspaceAtStop(agent *store.Agent) *ephemeralWorkspaceRecord {
	raw := agent.Annotations[workspaceAtStopAnnotation]
	if raw == "" {
		return nil
	}
	var rec ephemeralWorkspaceRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil
	}
	return &rec
}

// checkEphemeralWorkspaceBeforeStop runs the pre-stop workspace check for an
// agent with an ephemeral workspace that is running, records the result on
// the agent, and, when warn is set and the check found work, adds the stop
// warning to ctx's dispatch warnings. It never fails and never waits longer
// than workspaceCheckTimeout: an exec that fails, times out or prints
// nothing usable is recorded as unchecked and gives no warning. Agents
// without an ephemeral workspace, or not running, are left alone (no exec,
// no record).
func (s *Server) checkEphemeralWorkspaceBeforeStop(ctx context.Context, dispatcher AgentDispatcher, agent *store.Agent, warn bool) {
	if dispatcher == nil || agent.RuntimeBrokerID == "" || !hasEphemeralWorkspace(agent) ||
		agent.Phase != string(state.PhaseRunning) {
		return
	}
	// One deadline covers the exec and the record write: the exec gets the
	// budget less a reserve for the write, which gets the rest.
	deadline := time.Now().Add(workspaceCheckTimeout)
	cctx, cancel := context.WithDeadline(ctx, deadline.Add(-workspaceRecordReserve()))
	output, exitCode, err := dispatcher.DispatchAgentExec(cctx, agent, workspaceCheckCommand(), workspaceCheckExecTimeoutSeconds)
	cancel()

	rec := ephemeralWorkspaceRecord{Unchecked: true}
	if err == nil && exitCode == 0 {
		if commits, files, ok := parseWorkspaceCheckOutput(output); ok {
			rec = ephemeralWorkspaceRecord{Commits: commits, Files: files}
		}
	}
	if rec.Unchecked {
		s.agentLifecycleLog.Info("Pre-stop workspace check did not complete; continuing without it",
			"agent_id", agent.ID, "agent", agent.Name, "exit_code", exitCode, "error", err)
	}
	s.setWorkspaceAtStop(ctx, agent, &rec, deadline)
	if warn {
		addDispatchWarnings(ctx, ephemeralWorkspaceStopWarning(rec))
	}
}

// addEphemeralWorkspaceStartWarning adds the start warning for an agent
// whose previous run had an ephemeral workspace, and clears the record so
// a later start without a recorded stop gets the generic notice. hadEphemeral
// is hasEphemeralWorkspace taken before the start's dispatch, which may
// record a new placement.
func (s *Server) addEphemeralWorkspaceStartWarning(ctx context.Context, agent *store.Agent, hadEphemeral bool) {
	if !hadEphemeral {
		return
	}
	addDispatchWarnings(ctx, ephemeralWorkspaceStartWarning(recordedWorkspaceAtStop(agent)))
	s.clearWorkspaceAtStop(ctx, agent)
}

// clearWorkspaceAtStop removes the workspace-at-stop record, for example
// after a stop whose dispatch failed: the agent may still be running, and
// what the check found is no longer the state of a stopped workspace. A
// later start without a record gives the generic notice.
func (s *Server) clearWorkspaceAtStop(ctx context.Context, agent *store.Agent) {
	s.setWorkspaceAtStop(ctx, agent, nil, time.Now().Add(workspaceClearTimeout))
}

// setWorkspaceAtStop writes (or, for nil, removes) the workspace-at-stop
// annotation with the narrow SetAgentAnnotation write, bounded by deadline,
// and mirrors it on the in-memory agent so a later whole-row write of that
// copy keeps it. A failed write is logged only.
func (s *Server) setWorkspaceAtStop(ctx context.Context, agent *store.Agent, rec *ephemeralWorkspaceRecord, deadline time.Time) {
	value := ""
	if rec != nil {
		b, err := json.Marshal(rec)
		if err != nil {
			return
		}
		value = string(b)
	}
	if value == "" && agent.Annotations[workspaceAtStopAnnotation] == "" {
		return
	}
	wctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	if err := s.store.SetAgentAnnotation(wctx, agent.ID, workspaceAtStopAnnotation, value); err != nil {
		slog.Warn("Recording the pre-stop workspace check failed", "agent_id", agent.ID, "error", err)
		return
	}
	if value == "" {
		delete(agent.Annotations, workspaceAtStopAnnotation)
		return
	}
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations[workspaceAtStopAnnotation] = value
}
