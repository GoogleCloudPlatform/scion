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

package msgaction

import "time"

// Command request kind constants and kind-registered event/conflict codes (ptone/scion#9278).
const (
	KindCommandRequest           = "command_request"
	CommandRequestResolvedStatus = "approved"
	CommandRequestTTL            = 24 * time.Hour

	// Surfaces accepted on approve, dismiss, and markdown_block create-and-resolve.
	CommandRequestSurfaceNative         = "native"
	CommandRequestSurfaceStandalonePage = "standalone_page"
	CommandRequestSurfaceCLI            = "cli"
	CommandRequestSurfaceMarkdownBlock  = "markdown_block"

	// Kind-registered audit event types (C3).
	EventCmdRunWaking      = "run_waking"
	EventCmdRunDispatched  = "run_dispatched"
	EventCmdRunSucceeded   = "run_succeeded"
	EventCmdRunFailed      = "run_failed"
	EventCmdRunUnknown     = "run_unknown"
	EventCmdRunNotStarted  = "run_not_started"
	EventCmdOutputReturned = "output_returned"
	EventCmdOutputViewed   = "output_viewed"

	// Kind-registered 4xx conflict/error codes (C3).
	ConflictCmdHashMismatch     = "command_hash_mismatch"
	ConflictCmdNotApprover      = "not_approver"
	ConflictCmdExecUnconfigured = "exec_container_unconfigured"
	ConflictCmdExecUnavailable  = "exec_container_unavailable"
	ConflictCmdExecStopped      = "exec_container_stopped"
	ConflictCmdMarkdownBlock    = "markdown_block_not_found"
	ConflictCmdKeyPending       = "command_request_key_pending"
	ConflictCmdRequestLimit     = "command_request_limit"
)

// CommandRequestKindSpec is the KindSpec for human-approved command requests.
// Approval uses no claim state (UsesClaim=false, ClaimRecovery=ClaimRecoveryNone):
// it is a single compare-and-set transition pending -> approved inside the
// approval transaction so a stale claim can never be recovered and re-armed.
var CommandRequestKindSpec = KindSpec{
	Name:                    KindCommandRequest,
	ResolvedStatus:          CommandRequestResolvedStatus,
	ResolvedConflictCode:    ConflictAlreadyResolved,
	UsesClaim:               false,
	ClaimRecovery:           ClaimRecoveryNone,
	Expires:                 CommandRequestTTL,
	DismissMinAge:           0,
	RepostTerminalOK:        true,
	ReplaceResolvedOnUpdate: false,
	EventTypes: []string{
		EventCmdRunWaking,
		EventCmdRunDispatched,
		EventCmdRunSucceeded,
		EventCmdRunFailed,
		EventCmdRunUnknown,
		EventCmdRunNotStarted,
		EventCmdOutputReturned,
		EventCmdOutputViewed,
	},
	ConflictCodes: []string{
		ConflictCmdHashMismatch,
		ConflictCmdNotApprover,
		ConflictCmdExecUnconfigured,
		ConflictCmdExecUnavailable,
		ConflictCmdExecStopped,
		ConflictCmdMarkdownBlock,
		ConflictCmdKeyPending,
		ConflictCmdRequestLimit,
	},
	DetailKeys: []string{
		"command_sha256",
		"exec_argv_sha256",
		"exec_agent_id",
		"exec_project_id",
		"exec_launch_id",
		"run_id",
		"wake",
		"outcome",
		"exit_code",
		"duration_ms",
		"output_bytes",
		"output_status",
		"redaction_count",
		"workdir_kind",
		"share_output",
		"not_started_reason",
		"approver_user_id",
	},
}

func init() {
	RegisterKind(CommandRequestKindSpec)
}
