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

import (
	"testing"
	"time"
)

func TestCommandRequestKindSpecRegistered(t *testing.T) {
	spec, ok := LookupKind(KindCommandRequest)
	if !ok {
		t.Fatalf("LookupKind(%q) = false, want true", KindCommandRequest)
	}
	if spec.ResolvedStatus != CommandRequestResolvedStatus {
		t.Errorf("ResolvedStatus = %q, want %q", spec.ResolvedStatus, CommandRequestResolvedStatus)
	}
	if spec.UsesClaim {
		t.Errorf("UsesClaim = true, want false")
	}
	if spec.ClaimRecovery != ClaimRecoveryNone {
		t.Errorf("ClaimRecovery = %q, want %q", spec.ClaimRecovery, ClaimRecoveryNone)
	}
	if spec.ShouldRecoverClaim(time.Now().Add(-time.Hour), time.Now()) {
		t.Errorf("ShouldRecoverClaim = true, want false")
	}
	if spec.Expires != 24*time.Hour {
		t.Errorf("Expires = %v, want 24h", spec.Expires)
	}
	if !spec.RepostTerminalOK {
		t.Errorf("RepostTerminalOK = false, want true")
	}
	if spec.ReplaceResolvedOnUpdate {
		t.Errorf("ReplaceResolvedOnUpdate = true, want false")
	}

	if cls, ok := spec.Classify("approved"); !ok || cls != ClassResolved {
		t.Errorf("Classify(approved) = (%q, %v), want (%q, true)", cls, ok, ClassResolved)
	}
	if _, ok := spec.Classify("claimed"); ok {
		t.Errorf("Classify(claimed) = true, want false when UsesClaim=false")
	}
	if code := ConflictCode(spec, "approved"); code != ConflictAlreadyResolved {
		t.Errorf("ConflictCode(approved) = %q, want %q", code, ConflictAlreadyResolved)
	}

	for _, ev := range []string{
		EventRequested, EventRefused, CommandRequestResolvedStatus,
		EventCmdRunWaking, EventCmdRunDispatched, EventCmdRunSucceeded,
		EventCmdRunFailed, EventCmdRunUnknown, EventCmdRunNotStarted,
		EventCmdOutputReturned, EventCmdOutputViewed,
	} {
		if !spec.AcceptsEventType(ev) {
			t.Errorf("AcceptsEventType(%q) = false, want true", ev)
		}
	}
	for _, code := range []string{
		ConflictCmdHashMismatch, ConflictCmdNotApprover,
		ConflictCmdExecUnconfigured, ConflictCmdExecUnavailable,
		ConflictCmdExecStopped, ConflictCmdMarkdownBlock, ConflictCmdKeyPending,
	} {
		if !spec.AcceptsConflictCode(code) {
			t.Errorf("AcceptsConflictCode(%q) = false, want true", code)
		}
	}
	for _, dk := range []string{
		"command_sha256", "exec_argv_sha256", "exec_agent_id", "exec_project_id",
		"exec_launch_id", "run_id", "wake", "outcome", "exit_code", "duration_ms",
		"output_bytes", "output_status", "workdir_kind", "share_output",
		"not_started_reason", "approver_user_id",
	} {
		if !spec.AcceptsDetailKey(dk) {
			t.Errorf("AcceptsDetailKey(%q) = false, want true", dk)
		}
	}
	if spec.AcceptsDetailKey("script") || spec.AcceptsDetailKey("output") {
		t.Errorf("script/output must never be allow-listed audit detail keys")
	}

	// PlanRepost on an approved head: auto next version without replacing approved status.
	plan, err := PlanRepost(spec, &Head{RowID: "r1", Key: "deploy", Version: 1, Status: "approved"}, false)
	if err != nil {
		t.Fatalf("PlanRepost(approved, update=false) err = %v", err)
	}
	if plan.NextVersion != 2 || plan.ReplacesRowID != "r1" || plan.MarkReplaced {
		t.Errorf("PlanRepost(approved) = %+v, want NextVersion=2 ReplacesRowID=r1 MarkReplaced=false", plan)
	}
}
