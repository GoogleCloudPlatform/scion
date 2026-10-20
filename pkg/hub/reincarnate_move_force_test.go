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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// forceMoveInput is eligibleMoveInput for a clone-per-agent agent with a git
// remote, both brokers able to take part in a forced move, and Force set.
func forceMoveInput(calls moveTestProbeCalls) moveEligibilityInput {
	in := eligibleMoveInput(calls)
	in.Force = true
	in.WorkspaceMode = string(store.SharingModeClonePerAgent)
	in.GitRemote = "https://example.com/repo.git"
	in.Branch = "scion/agent-1"
	in.DefaultBranch = "main"
	in.Agent.Generation = 3
	for _, b := range []*store.RuntimeBroker{in.Src, in.Dst} {
		b.Capabilities = &store.BrokerCapabilities{AgentMove: true, AgentMoveRecreate: true}
	}
	return in
}

// Ways to make the export checks fail at each waivable check.
func breakWorkspaceStorage(in *moveEligibilityInput) { in.Dst.WorkspaceStorage = nil }
func breakSameExport(in *moveEligibilityInput) {
	in.Dst.WorkspaceStorage = moveTestStorage("10.9.9.9", "/other", "projects", true)
}
func breakWorkspaceOnExport(in *moveEligibilityInput) {
	in.Agent.WorkspacePlacement = api.WorkspacePlacementLocal
}

func checkResults(v MoveVerdict) map[string]string {
	out := map[string]string{}
	for _, c := range v.Checks {
		out[c.Name] = c.Result
	}
	return out
}

func consequenceKeys(v MoveVerdict) map[string]string {
	out := map[string]string{}
	for _, c := range v.Consequences {
		out[c.Item] = c.Kind
	}
	return out
}

// assertCheckOrder checks the verdict lists every check in order.
func assertCheckOrder(t *testing.T, v MoveVerdict) {
	t.Helper()
	if len(v.Checks) != len(moveCheckOrder) {
		t.Fatalf("got %d checks, want %d", len(v.Checks), len(moveCheckOrder))
	}
	for i, c := range v.Checks {
		if c.Name != moveCheckOrder[i] {
			t.Errorf("check %d = %s, want %s", i, c.Name, moveCheckOrder[i])
		}
	}
}

// workspace_recreatable sits right after workspace_on_export.
func TestMoveCheckOrder_WorkspaceRecreatableAfterWorkspaceOnExport(t *testing.T) {
	for i, name := range moveCheckOrder {
		if name == moveCheckWorkspaceOnExport {
			if i+1 >= len(moveCheckOrder) || moveCheckOrder[i+1] != moveCheckWorkspaceRecreatable {
				t.Fatalf("check after workspace_on_export = %v, want workspace_recreatable", moveCheckOrder[i+1:])
			}
			return
		}
	}
	t.Fatal("workspace_on_export not in moveCheckOrder")
}

// F2: --force on a move the export checks allow is an export move, with the
// same checks as without --force.
func TestEvaluateMoveEligibility_ForceNeverDowngradesExportMove(t *testing.T) {
	for _, force := range []bool{false, true} {
		calls := moveTestProbeCalls{}
		in := forceMoveInput(calls)
		in.Force = force
		v, ref := evaluateMoveEligibility(in)
		if ref != nil {
			t.Fatalf("force=%v: unexpected refusal %+v", force, ref)
		}
		if v.TransferMode != MoveTransferModeExport {
			t.Errorf("force=%v: transfer mode = %q, want export", force, v.TransferMode)
		}
		assertCheckOrder(t, v)
		for _, c := range v.Checks {
			if c.Result != MoveCheckPassed {
				t.Errorf("force=%v: check %s = %s, want passed", force, c.Name, c.Result)
			}
			if c.Name == moveCheckWorkspaceRecreatable && c.Message != "" {
				t.Errorf("force=%v: workspace_recreatable passed with message %q in an export move", force, c.Message)
			}
		}
		keys := consequenceKeys(v)
		for item, kind := range map[string]string{
			"identity": "kept", "relationships": "kept", "config": "kept", "workspace": "kept", "local-work": "kept",
			"handoff": "carried", "home": "recreated", "harness-session": "recreated", "credentials": "recreated",
			"process-state": "lost",
		} {
			if keys[item] != kind {
				t.Errorf("force=%v: consequence %s = %q, want %s", force, item, keys[item], kind)
			}
		}
		if calls["capacity"] != 1 || calls["reachable:dst"] != 1 {
			t.Errorf("force=%v: probes called %v, want each once", force, calls)
		}
	}
}

// Under --force, each waivable export check failure switches to a recreated
// workspace: the three export checks are waived, workspace_recreatable
// passes, and every later check runs once.
func TestEvaluateMoveEligibility_ForceRecreatesWhenExportCannotCarry(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*moveEligibilityInput)
	}{
		{"workspace storage not reported", breakWorkspaceStorage},
		{"different export", breakSameExport},
		{"different export identity", func(in *moveEligibilityInput) {
			in.Dst.WorkspaceStorage.NFS.ExportID = "0a0a0a0a-0000-4000-8000-000000000000"
		}},
		{"placement local", breakWorkspaceOnExport},
		{"placement unknown", func(in *moveEligibilityInput) { in.Agent.WorkspacePlacement = "" }},
		{"GCS-synced workspace", func(in *moveEligibilityInput) { in.Agent.AppliedConfig.WorkspaceStoragePath = "gs://b/p" }},
		{"clone-per-agent on docker", func(in *moveEligibilityInput) { in.Agent.Runtime = "docker" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := moveTestProbeCalls{}
			in := forceMoveInput(calls)
			tc.mutate(&in)
			v, ref := evaluateMoveEligibility(in)
			if ref != nil {
				t.Fatalf("unexpected refusal: %+v", ref)
			}
			if !v.Eligible || v.TransferMode != MoveTransferModeRecreate {
				t.Fatalf("eligible=%v mode=%q, want eligible recreate", v.Eligible, v.TransferMode)
			}
			assertCheckOrder(t, v)
			for _, c := range v.Checks {
				want := MoveCheckPassed
				if moveCheckWaivableByForce(c.Name) {
					want = MoveCheckWaived
					if c.Message != moveForceWaivedMessage {
						t.Errorf("waived check %s message = %q", c.Name, c.Message)
					}
				}
				if c.Result != want {
					t.Errorf("check %s = %s, want %s", c.Name, c.Result, want)
				}
				if c.Name == moveCheckWorkspaceRecreatable && !strings.Contains(c.Message, "fresh clone of example.com/repo") {
					t.Errorf("workspace_recreatable message = %q", c.Message)
				}
			}
			keys := consequenceKeys(v)
			for item, kind := range map[string]string{
				"identity": "kept", "relationships": "kept", "config": "kept",
				"handoff": "carried", "workspace": "recreated", "home": "recreated",
				"harness-session": "recreated", "credentials": "recreated",
				"local-work": "lost", "process-state": "lost",
			} {
				if keys[item] != kind {
					t.Errorf("consequence %s = %q, want %s", item, keys[item], kind)
				}
			}
			for _, k := range []string{"reachable:dst", "reachable:src", "dispatch", "provider", "capacity"} {
				if calls[k] != 1 {
					t.Errorf("probe %s called %d times, want 1", k, calls[k])
				}
			}
		})
	}
}

// The Kubernetes-only constraint for clone-per-agent profiles keeps a
// workspace on the export; a recreated workspace may run under any profile.
func TestEvaluateMoveEligibility_ForceDropsKubernetesProfileConstraint(t *testing.T) {
	in := forceMoveInput(moveTestProbeCalls{})
	breakWorkspaceOnExport(&in)
	in.Profile = "local"
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if v.TransferMode != MoveTransferModeRecreate || v.RuntimeType != "docker" {
		t.Errorf("mode=%q runtime=%q, want recreate on docker", v.TransferMode, v.RuntimeType)
	}
}

// In recreate mode the target's NFS mount health matters only when the
// target places the agent directory on the export.
func TestEvaluateMoveEligibility_ForceTargetExportHealth(t *testing.T) {
	t.Run("docker profile ignores an unhealthy mount", func(t *testing.T) {
		in := forceMoveInput(moveTestProbeCalls{})
		breakWorkspaceOnExport(&in)
		in.Profile = "local"
		in.Dst.WorkspaceStorage.NFS.Healthy = false
		if _, ref := evaluateMoveEligibility(in); ref != nil {
			t.Fatalf("unexpected refusal: %+v", ref)
		}
	})
	t.Run("kubernetes profile on nfs needs a healthy mount", func(t *testing.T) {
		in := forceMoveInput(moveTestProbeCalls{})
		breakWorkspaceOnExport(&in)
		in.Dst.WorkspaceStorage.NFS.Healthy = false
		v, ref := evaluateMoveEligibility(in)
		if ref == nil || ref.Status != http.StatusServiceUnavailable || !strings.Contains(ref.Message, "NFS export mount unhealthy") {
			t.Fatalf("refusal = %+v, want 503 unhealthy mount", ref)
		}
		if checkResults(v)[moveCheckTargetHealth] != MoveCheckFailed {
			t.Errorf("target_health = %s", checkResults(v)[moveCheckTargetHealth])
		}
	})
	t.Run("no target descriptor", func(t *testing.T) {
		in := forceMoveInput(moveTestProbeCalls{})
		breakWorkspaceStorage(&in)
		if _, ref := evaluateMoveEligibility(in); ref != nil {
			t.Fatalf("unexpected refusal: %+v", ref)
		}
	})
}

// An empty-per-agent workspace is recreated as a fresh empty directory.
func TestEvaluateMoveEligibility_ForceEmptyPerAgent(t *testing.T) {
	in := forceMoveInput(moveTestProbeCalls{})
	in.EmptyPerAgent = true
	in.WorkspaceMode = string(store.SharingModeEmptyPerAgent)
	in.GitRemote = ""
	breakWorkspaceOnExport(&in)
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if v.TransferMode != MoveTransferModeRecreate {
		t.Fatalf("mode = %q", v.TransferMode)
	}
	for _, c := range v.Checks {
		if c.Name == moveCheckWorkspaceRecreatable && c.Message != "fresh empty workspace on the target" {
			t.Errorf("workspace_recreatable message = %q", c.Message)
		}
	}
	for _, c := range v.Consequences {
		if c.Item == "workspace" && !strings.Contains(c.Text, "fresh empty directory") {
			t.Errorf("workspace consequence = %q", c.Text)
		}
	}
}

// Workspaces a forced move cannot recreate are refused at
// workspace_recreatable, after the waived checks.
func TestEvaluateMoveEligibility_ForceWorkspaceNotRecreatable(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*moveEligibilityInput)
		msgContain string
	}{
		{"shared workspace", func(in *moveEligibilityInput) {
			in.CloneMode = false
			in.WorkspaceMode = string(store.SharingModeSharedPlain)
		}, "this agent's workspace is shared-plain"},
		{"clone-per-agent without a git remote", func(in *moveEligibilityInput) { in.GitRemote = "" }, "no git remote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := moveTestProbeCalls{}
			in := forceMoveInput(calls)
			breakSameExport(&in)
			tc.mutate(&in)
			v, ref := evaluateMoveEligibility(in)
			if ref == nil {
				t.Fatal("expected a refusal")
			}
			if ref.Status != http.StatusConflict || ref.Code != ErrCodeConflict || !strings.Contains(ref.Message, tc.msgContain) {
				t.Errorf("refusal = %+v, want 409 conflict containing %q", ref, tc.msgContain)
			}
			if ref.ForceWouldApply {
				t.Error("forceWouldApply set on a forced refusal")
			}
			if v.Eligible || v.TransferMode != "" || v.Consequences != nil {
				t.Errorf("refused verdict eligible=%v mode=%q consequences=%v", v.Eligible, v.TransferMode, v.Consequences)
			}
			got := checkResults(v)
			for name, want := range map[string]string{
				moveCheckRuntimeTarget: MoveCheckPassed, moveCheckWorkspaceMode: MoveCheckPassed,
				moveCheckWorkspaceStorage: MoveCheckWaived, moveCheckSameExport: MoveCheckWaived,
				moveCheckWorkspaceOnExport: MoveCheckWaived, moveCheckWorkspaceRecreatable: MoveCheckFailed,
				moveCheckTargetProfile: MoveCheckNotEvaluated, moveCheckCapacity: MoveCheckNotEvaluated,
			} {
				if got[name] != want {
					t.Errorf("check %s = %s, want %s", name, got[name], want)
				}
			}
			if len(calls) != 0 {
				t.Errorf("probes called after refusal: %v", calls)
			}
		})
	}
}

// --force relaxes only the workspace checks: every other refusal stands,
// with the same status and code as without --force.
func TestEvaluateMoveEligibility_ForceKeepsOtherChecks(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*moveEligibilityInput)
		check      string
		status     int
		code       string
		msgContain string
	}{
		{"pinned agent", func(in *moveEligibilityInput) {
			in.Agent.PinnedRuntimeTargetID = "rt-1"
		}, moveCheckRuntimeTarget, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported, "not movable"},
		{"workspace mode", func(in *moveEligibilityInput) {
			in.WorkspaceModeError = "reincarnate does not yet support worktree-per-agent workspaces"
		}, moveCheckWorkspaceMode, http.StatusBadRequest, ErrCodeValidationError, "worktree-per-agent"},
		{"missing profile", func(in *moveEligibilityInput) { in.Profile = "nope" },
			moveCheckTargetProfile, http.StatusConflict, ErrCodeConflict, `has no profile "nope"`},
		{"profile unavailable", func(in *moveEligibilityInput) { in.Dst.Profiles[0].Available = false },
			moveCheckTargetProfile, http.StatusConflict, ErrCodeConflict, "not available"},
		{"target unreachable", func(in *moveEligibilityInput) {
			in.Probes.Reachable = func(b *store.RuntimeBroker) bool { return b.ID != "dst" }
		}, moveCheckTargetHealth, http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail, "is unavailable"},
		{"cannot dispatch", func(in *moveEligibilityInput) {
			in.Probes.CanDispatch = func(*store.RuntimeBroker) bool { return false }
		}, moveCheckAccess, http.StatusForbidden, ErrCodeForbidden, "permission to run agents"},
		{"self-move to non-provider", func(in *moveEligibilityInput) {
			in.SelfMove = true
			in.Probes.ServesProject = func(*store.RuntimeBroker) bool { return false }
		}, moveCheckAccess, http.StatusConflict, ErrCodeConflict, "does not serve this project"},
		{"target without agent move", func(in *moveEligibilityInput) { in.Dst.Capabilities.AgentMove = false },
			moveCheckCapability, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability, "does not support agent move"},
		{"target without forced move", func(in *moveEligibilityInput) { in.Dst.Capabilities.AgentMoveRecreate = false },
			moveCheckCapability, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability, "broker dst-name does not support forced moves; upgrade it"},
		{"target at agent limit", func(in *moveEligibilityInput) {
			in.Probes.Capacity = func(*store.RuntimeBroker) string { return "broker dst-name is at its agent limit (5 of 5)" }
		}, moveCheckCapacity, http.StatusTooManyRequests, ErrCodeQuotaExceeded, "agent limit"},
	}
	for _, tc := range cases {
		for _, breaker := range []struct {
			name string
			fn   func(*moveEligibilityInput)
		}{{"same_export fail", breakSameExport}, {"workspace_on_export fail", breakWorkspaceOnExport}} {
			t.Run(tc.name+"/"+breaker.name, func(t *testing.T) {
				in := forceMoveInput(moveTestProbeCalls{})
				breaker.fn(&in)
				tc.mutate(&in)
				v, ref := evaluateMoveEligibility(in)
				if ref == nil {
					t.Fatalf("expected refusal at %s", tc.check)
				}
				if ref.Status != tc.status || ref.Code != tc.code || !strings.Contains(ref.Message, tc.msgContain) {
					t.Errorf("refusal = %+v, want %d %s containing %q", ref, tc.status, tc.code, tc.msgContain)
				}
				if checkResults(v)[tc.check] != MoveCheckFailed {
					t.Errorf("check %s = %s, want failed", tc.check, checkResults(v)[tc.check])
				}
				if v.TransferMode != "" {
					t.Errorf("refused verdict has transfer mode %q", v.TransferMode)
				}
			})
		}
	}
}

// Without --force a waivable failure is refused exactly as before, with
// workspace_recreatable not evaluated, and forceWouldApply says whether a
// forced move would support this agent's workspace.
func TestEvaluateMoveEligibility_WithoutForceUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*moveEligibilityInput)
		check     string
		wouldWork bool
	}{
		{"same_export clone-per-agent", breakSameExport, moveCheckSameExport, true},
		{"workspace_on_export clone-per-agent", breakWorkspaceOnExport, moveCheckWorkspaceOnExport, true},
		{"workspace storage not reported", breakWorkspaceStorage, moveCheckWorkspaceStorage, false},
		{"same_export shared workspace", func(in *moveEligibilityInput) {
			breakSameExport(in)
			in.CloneMode = false
			in.WorkspaceMode = string(store.SharingModeSharedPlain)
		}, moveCheckSameExport, false},
		{"same_export clone-per-agent without remote", func(in *moveEligibilityInput) {
			breakSameExport(in)
			in.GitRemote = ""
		}, moveCheckSameExport, false},
		{"same_export, target without forced moves", func(in *moveEligibilityInput) {
			breakSameExport(in)
			in.Dst.Capabilities.AgentMoveRecreate = false
		}, moveCheckSameExport, false},
		{"workspace_on_export, target without forced moves", func(in *moveEligibilityInput) {
			breakWorkspaceOnExport(in)
			in.Dst.Capabilities.AgentMoveRecreate = false
		}, moveCheckWorkspaceOnExport, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forced := forceMoveInput(moveTestProbeCalls{})
			forced.Force = false
			tc.mutate(&forced)
			v, ref := evaluateMoveEligibility(forced)
			if ref == nil {
				t.Fatal("expected a refusal")
			}
			if ref.ForceWouldApply != tc.wouldWork {
				t.Errorf("forceWouldApply = %v, want %v", ref.ForceWouldApply, tc.wouldWork)
			}
			got := checkResults(v)
			if got[tc.check] != MoveCheckFailed || got[moveCheckWorkspaceRecreatable] != MoveCheckNotEvaluated {
				t.Errorf("checks = %v, want %s failed and workspace_recreatable not_evaluated", got, tc.check)
			}
			if v.TransferMode != "" || v.Consequences != nil {
				t.Errorf("refused verdict mode=%q consequences=%v", v.TransferMode, v.Consequences)
			}
			// The message is the export check's own, unchanged.
			plain := eligibleMoveInput(moveTestProbeCalls{})
			tc.mutate(&plain)
			_, plainRef := evaluateMoveEligibility(plain)
			if plainRef == nil || plainRef.Message != ref.Message || plainRef.Status != ref.Status || plainRef.Code != ref.Code {
				t.Errorf("refusal %+v differs from the unforced input's %+v", ref, plainRef)
			}
		})
	}
	t.Run("same_export message unchanged", func(t *testing.T) {
		in := eligibleMoveInput(moveTestProbeCalls{})
		breakSameExport(&in)
		_, ref := evaluateMoveEligibility(in)
		if ref == nil || ref.Message != sameExportMessage(in.Src, in.Dst) {
			t.Errorf("refusal = %+v, want the sameExportMessage text", ref)
		}
	})
}

// AgentMoveRecreate is ingested from registration names and from the
// heartbeat's stored capabilities, like AgentMove.
func TestAgentMoveRecreateCapabilityIngest(t *testing.T) {
	for _, name := range []string{"agentMoveRecreate", "agentmoverecreate", "agent_move_recreate"} {
		if !capabilitiesFromStrings([]string{name}).AgentMoveRecreate {
			t.Errorf("capabilitiesFromStrings(%q).AgentMoveRecreate = false", name)
		}
	}
	if capabilitiesFromStrings([]string{"agentMove"}).AgentMoveRecreate {
		t.Error("agentMove alone must not imply AgentMoveRecreate")
	}
	var hb brokerHeartbeatRequest
	if err := json.Unmarshal([]byte(`{"capabilities":{"agentMove":true,"agentMoveRecreate":true}}`), &hb); err != nil {
		t.Fatal(err)
	}
	if hb.Capabilities == nil || !hb.Capabilities.AgentMoveRecreate {
		t.Errorf("heartbeat capabilities = %+v, want AgentMoveRecreate", hb.Capabilities)
	}
}

// A2.6: a Kubernetes, export-placed clone-per-agent agent moving to a
// broker whose profile is docker. The export checks refuse it at
// target_profile only because of placement: --force recreates it, and
// without --force the refusal is unchanged with forceWouldApply set.
func TestEvaluateMoveEligibility_PlacementCausedTargetProfile(t *testing.T) {
	plain := eligibleMoveInput(moveTestProbeCalls{})
	plain.Profile = "local"
	_, plainRef := evaluateMoveEligibility(plain)
	if plainRef == nil {
		t.Fatal("expected the export checks to refuse a docker profile")
	}

	in := forceMoveInput(moveTestProbeCalls{})
	in.Profile = "local"
	in.Force = false
	v, ref := evaluateMoveEligibility(in)
	if ref == nil {
		t.Fatal("expected a refusal without --force")
	}
	if ref.Status != plainRef.Status || ref.Code != plainRef.Code || ref.Message != plainRef.Message {
		t.Errorf("refusal %+v differs from today's %+v", ref, plainRef)
	}
	if checkResults(v)[moveCheckTargetProfile] != MoveCheckFailed {
		t.Errorf("target_profile = %s, want failed", checkResults(v)[moveCheckTargetProfile])
	}
	if !ref.ForceWouldApply {
		t.Error("forceWouldApply not set for a placement-caused target_profile refusal")
	}
	// A5 (N1): the same refusal onto a target without forced moves does not
	// suggest --force.
	noCap := forceMoveInput(moveTestProbeCalls{})
	noCap.Profile = "local"
	noCap.Force = false
	noCap.Dst.Capabilities.AgentMoveRecreate = false
	if _, ncRef := evaluateMoveEligibility(noCap); ncRef == nil || ncRef.ForceWouldApply {
		t.Errorf("target without AgentMoveRecreate: refusal = %+v, want a refusal without forceWouldApply", ncRef)
	}

	calls := moveTestProbeCalls{}
	in = forceMoveInput(calls)
	in.Profile = "local"
	v, ref = evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("--force: unexpected refusal %+v", ref)
	}
	if v.TransferMode != MoveTransferModeRecreate || v.RuntimeType != "docker" {
		t.Errorf("--force: mode=%q runtime=%q, want recreate on docker", v.TransferMode, v.RuntimeType)
	}
	for _, k := range []string{"reachable:dst", "reachable:src", "dispatch", "provider", "capacity"} {
		if calls[k] != 1 {
			t.Errorf("probe %s called %d times, want 1", k, calls[k])
		}
	}
}

// A2.6: an nfs target with an unhealthy mount and a docker profile. The
// export checks refuse it at target_profile (the Kubernetes constraint,
// placement-caused), and the recreate checks do not need the mount on a
// docker profile, so --force recreates.
func TestEvaluateMoveEligibility_ForceUnhealthyMountDockerProfile(t *testing.T) {
	in := forceMoveInput(moveTestProbeCalls{})
	in.Profile = "local"
	in.Dst.WorkspaceStorage.NFS.Healthy = false
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if v.TransferMode != MoveTransferModeRecreate {
		t.Errorf("mode = %q, want recreate", v.TransferMode)
	}
}

// A2.6: an nfs target with an unhealthy mount and a Kubernetes profile for
// a clone-per-agent workspace: the recreated agent directory would be on
// the export too, so the move is refused 503 with --force as without it,
// and forceWouldApply is not set.
func TestEvaluateMoveEligibility_UnhealthyMountKubernetesProfileStaysRefused(t *testing.T) {
	for _, force := range []bool{false, true} {
		in := forceMoveInput(moveTestProbeCalls{})
		in.Force = force
		in.Dst.WorkspaceStorage.NFS.Healthy = false
		v, ref := evaluateMoveEligibility(in)
		if ref == nil || ref.Status != http.StatusServiceUnavailable || !strings.Contains(ref.Message, "NFS export mount unhealthy") {
			t.Fatalf("force=%v: refusal = %+v, want 503 unhealthy mount", force, ref)
		}
		if checkResults(v)[moveCheckTargetHealth] != MoveCheckFailed {
			t.Errorf("force=%v: target_health = %s", force, checkResults(v)[moveCheckTargetHealth])
		}
		if ref.ForceWouldApply {
			t.Errorf("force=%v: forceWouldApply set although --force fails the same way", force)
		}
	}
}

// A3 (R1): a target_health failure of the export checks is never waived. A
// shared-workspace agent on the same export, with an unhealthy target
// mount, gets the same 503 at target_health with and without --force, and
// forceWouldApply is not set.
func TestEvaluateMoveEligibility_UnhealthyMountSharedWorkspaceNotWaived(t *testing.T) {
	plain := eligibleMoveInput(moveTestProbeCalls{})
	plain.CloneMode = false
	plain.Dst.WorkspaceStorage.NFS.Healthy = false
	_, plainRef := evaluateMoveEligibility(plain)
	if plainRef == nil || plainRef.Status != http.StatusServiceUnavailable {
		t.Fatalf("without the force fields: refusal = %+v, want 503", plainRef)
	}
	for _, force := range []bool{false, true} {
		in := forceMoveInput(moveTestProbeCalls{})
		in.Force = force
		in.CloneMode = false
		in.WorkspaceMode = string(store.SharingModeSharedPlain)
		in.Dst.WorkspaceStorage.NFS.Healthy = false
		v, ref := evaluateMoveEligibility(in)
		if ref == nil {
			t.Fatalf("force=%v: expected a refusal", force)
		}
		if ref.Status != plainRef.Status || ref.Code != plainRef.Code || ref.Message != plainRef.Message {
			t.Errorf("force=%v: refusal %+v, want today's %+v", force, ref, plainRef)
		}
		got := checkResults(v)
		if got[moveCheckTargetHealth] != MoveCheckFailed || got[moveCheckWorkspaceRecreatable] != MoveCheckPassed {
			t.Errorf("force=%v: checks = %v, want the export verdict failed at target_health", force, got)
		}
		if ref.ForceWouldApply {
			t.Errorf("force=%v: forceWouldApply set", force)
		}
		if v.TransferMode != "" {
			t.Errorf("force=%v: transfer mode %q on a refusal", force, v.TransferMode)
		}
	}
}

// A14: a forced move onto a target with no capabilities at all is refused
// at the capability check (412), without a panic.
func TestEvaluateMoveEligibility_ForceTargetWithoutCapabilities(t *testing.T) {
	in := forceMoveInput(moveTestProbeCalls{})
	breakSameExport(&in)
	in.Dst.Capabilities = nil
	v, ref := evaluateMoveEligibility(in)
	if ref == nil || ref.Status != http.StatusPreconditionFailed || ref.Code != ErrCodeUnsupportedCapability {
		t.Fatalf("refusal = %+v, want 412 unsupported capability", ref)
	}
	if checkResults(v)[moveCheckCapability] != MoveCheckFailed {
		t.Errorf("capability = %s, want failed", checkResults(v)[moveCheckCapability])
	}
}

// A14: the git remote is normalized for display, so a remote carrying
// userinfo shows none, in the workspace_recreatable message and in the
// workspace consequence.
func TestEvaluateMoveEligibility_ForceGitRemoteNormalizedForDisplay(t *testing.T) {
	for _, remote := range []string{
		"https://user:pass@example.com/org/repo.git",
		"https://x-access-token:tok123@github.com/org/repo",
	} {
		in := forceMoveInput(moveTestProbeCalls{})
		breakSameExport(&in)
		in.GitRemote = remote
		v, ref := evaluateMoveEligibility(in)
		if ref != nil {
			t.Fatalf("%s: unexpected refusal %+v", remote, ref)
		}
		var shown []string
		for _, c := range v.Checks {
			if c.Name == moveCheckWorkspaceRecreatable {
				shown = append(shown, c.Message)
			}
		}
		for _, c := range v.Consequences {
			if c.Item == "workspace" {
				shown = append(shown, c.Text)
			}
		}
		if len(shown) != 2 {
			t.Fatalf("%s: got %d texts, want the check message and the consequence", remote, len(shown))
		}
		for _, text := range shown {
			if strings.Contains(text, "@") || strings.Contains(text, "pass") || strings.Contains(text, "tok123") {
				t.Errorf("%s: shown text %q carries userinfo", remote, text)
			}
			if !strings.Contains(text, "/org/repo") {
				t.Errorf("%s: shown text %q does not name the repository", remote, text)
			}
		}
	}
}

// A14: a target without an NFS descriptor reads as an unhealthy export
// mount, so a move that needs the mount is refused (503), never a panic.
// The export pass cannot reach target_health with no descriptor
// (same_export requires one), so the decision is pinned on its helper.
func TestTargetExportMountHealthy_MissingDescriptorIsUnhealthy(t *testing.T) {
	healthy := moveTestBroker("dst")
	noNFS := moveTestBroker("dst")
	noNFS.WorkspaceStorage = &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendNFS}
	noStorage := moveTestBroker("dst")
	noStorage.WorkspaceStorage = nil
	for name, tc := range map[string]struct {
		b    *store.RuntimeBroker
		want bool
	}{"healthy": {healthy, true}, "nil NFS": {noNFS, false}, "nil descriptor": {noStorage, false}} {
		if got := targetExportMountHealthy(tc.b); got != tc.want {
			t.Errorf("%s: targetExportMountHealthy = %v, want %v", name, got, tc.want)
		}
	}
}
