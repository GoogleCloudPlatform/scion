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
	"path"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// logAgentEnteredErrorPhase is the lifecycle log record written when a
// status write moves an agent into phase error (ptone/scion#7927).
const logAgentEnteredErrorPhase = "agent_entered_error_phase"

// Sources of the write that moved an agent into phase error.
const (
	errorPhaseSourceHeartbeat   = "broker_heartbeat"
	errorPhaseSourceAgentStatus = "agent_status"
)

// failure_stage values agentErrorStage derives from the agent's message.
const (
	failureStagePreStartHook     = "pre_start_hook"
	failureStageHarnessProvision = "harness_provision"
	failureStageGitClone         = "git_clone"
	failureStageOther            = "other"
)

// Message prefixes sciontool init writes with phase error
// (cmd/sciontool/commands/init.go), and the harness provisioner's hook
// file name (pkg/sciontool/hooks.HarnessProvisionHookFilename).
const (
	preStartHookFailedPrefix = "pre-start hook failed:"
	gitCloneFailedPrefix     = "git clone failed:"
	harnessProvisionHookName = "20-harness-provision"
)

var (
	// The failed hook script in a pre-start message: "script <path>: ".
	// The path holds a '/', which skips the "script hooks: " that wraps it.
	errorMessageHookRe = regexp.MustCompile(`\bscript ([^\s:]*/[^\s:]+): `)
	// The script's exit status in a pre-start message.
	errorMessageExitStatusRe = regexp.MustCompile(`\bexit status (-?\d+)`)
)

// agentErrorStage returns the failure stage, the failed hook script's base
// name and its exit status that message, an agent's phase error message,
// names. hook and exitStatus are "" when the message does not name them.
func agentErrorStage(message string) (stage, hook, exitStatus string) {
	switch {
	case strings.HasPrefix(message, preStartHookFailedPrefix):
		stage = failureStagePreStartHook
		if m := errorMessageHookRe.FindStringSubmatch(message); m != nil {
			hook = path.Base(m[1])
			if hook == harnessProvisionHookName {
				stage = failureStageHarnessProvision
			}
		}
		if m := errorMessageExitStatusRe.FindStringSubmatch(message); m != nil {
			exitStatus = m[1]
		}
	case strings.HasPrefix(message, gitCloneFailedPrefix):
		stage = failureStageGitClone
	default:
		stage = failureStageOther
	}
	return stage, hook, exitStatus
}

// logAgentErrorPhaseEntry writes one lifecycle warning when after, the agent
// re-read after a status write, is in phase error and before, the agent
// read before it, was not. A write that confirms an error phase already
// stored (every later heartbeat of the exited container) logs nothing.
//
// Without it a pre-start hook or harness provisioner failure reached the
// hub only as the agent's stored phase and message (ptone/scion#7927). The
// message is logged only through logging.SanitizeText, which masks
// credential-shaped values and bounds it; sciontool has already scrubbed
// the provisioner's staged secret values out of it and never puts a hook's
// raw output in it (scrubSecrets, annotateHarnessProvisionError).
func (s *Server) logAgentErrorPhaseEntry(before, after *store.Agent, source string) {
	if s.agentLifecycleLog == nil || before == nil || after == nil {
		return
	}
	if after.Phase != string(state.PhaseError) || before.Phase == string(state.PhaseError) {
		return
	}
	stage, hook, exitStatus := agentErrorStage(after.Message)
	attrs := []any{
		"agent_id", after.ID,
		"agent", after.Slug,
		"project_id", after.ProjectID,
		"broker_id", after.RuntimeBrokerID,
		"run_id", after.RunID,
		"source", source,
		"previous_phase", before.Phase,
		"failure_stage", stage,
	}
	if hook != "" {
		attrs = append(attrs, "hook", hook)
	}
	if exitStatus != "" {
		attrs = append(attrs, "hook_exit_status", exitStatus)
	}
	if after.ExitCode != nil {
		attrs = append(attrs, "exit_code", *after.ExitCode)
	}
	if after.ExitReason != "" {
		attrs = append(attrs, "exit_reason", after.ExitReason)
	}
	if after.ContainerStatus != "" {
		attrs = append(attrs, "container_status", after.ContainerStatus)
	}
	attrs = append(attrs, "message", logging.SanitizeText(after.Message))
	s.agentLifecycleLog.Warn(logAgentEnteredErrorPhase, attrs...)
}
