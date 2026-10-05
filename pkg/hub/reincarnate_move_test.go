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
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// moveTestExportID is the export identity marker both test brokers report.
const moveTestExportID = "6f1c2a8e-0d4b-4c1e-9a57-3b2f8e1d0c99"

func moveTestStorage(server, export, subPathRoot string, healthy bool) *api.BrokerWorkspaceStorage {
	return &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS: &api.BrokerNFSWorkspaceStorage{Server: server, Export: export, SubPathRoot: subPathRoot, Healthy: healthy,
			ExportID: moveTestExportID},
	}
}

func moveTestBroker(id string) *store.RuntimeBroker {
	return &store.RuntimeBroker{
		ID:               id,
		Name:             id + "-name",
		WorkspaceStorage: moveTestStorage("10.0.0.2", "/vol1", "projects", true),
		Capabilities:     &store.BrokerCapabilities{AgentMove: true},
		DefaultProfile:   "k8s",
		Profiles: []store.BrokerProfile{
			{Name: "k8s", Type: "kubernetes", Available: true},
			{Name: "local", Type: "docker", Available: true},
		},
	}
}

// moveTestProbeCalls counts probe calls by name.
type moveTestProbeCalls map[string]int

// eligibleMoveInput is a clone-per-agent Kubernetes agent moving between two
// brokers that pass every check.
func eligibleMoveInput(calls moveTestProbeCalls) moveEligibilityInput {
	return moveEligibilityInput{
		Agent: &store.Agent{ID: "agent-1", Runtime: "kubernetes", WorkspacePlacement: api.WorkspacePlacementExport,
			AppliedConfig: &store.AgentAppliedConfig{}},
		Src:       moveTestBroker("src"),
		Dst:       moveTestBroker("dst"),
		CloneMode: true,
		Probes: moveProbes{
			Reachable:        func(b *store.RuntimeBroker) bool { calls["reachable:"+b.ID]++; return true },
			CanDispatch:      func(*store.RuntimeBroker) bool { calls["dispatch"]++; return true },
			CanUseAsProvider: func(*store.RuntimeBroker) bool { calls["provider"]++; return true },
			ServesProject:    func(*store.RuntimeBroker) bool { calls["serves"]++; return true },
			Capacity:         func(*store.RuntimeBroker) string { calls["capacity"]++; return "" },
		},
	}
}

func TestEvaluateMoveEligibility_FullPass(t *testing.T) {
	calls := moveTestProbeCalls{}
	in := eligibleMoveInput(calls)
	in.Probes.Passthrough = func(*store.RuntimeBroker) string { calls["passthrough"]++; return "" }

	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if !v.Eligible {
		t.Fatal("verdict not eligible")
	}
	if v.SourceBroker != (MoveBrokerRef{ID: "src", Name: "src-name"}) || v.TargetBroker != (MoveBrokerRef{ID: "dst", Name: "dst-name"}) {
		t.Errorf("broker refs = %+v / %+v", v.SourceBroker, v.TargetBroker)
	}
	if v.Profile != "k8s" || v.RuntimeType != "kubernetes" {
		t.Errorf("profile = %q/%q, want k8s/kubernetes", v.Profile, v.RuntimeType)
	}
	if len(v.Checks) != len(moveCheckOrder) {
		t.Fatalf("got %d checks, want %d", len(v.Checks), len(moveCheckOrder))
	}
	for i, c := range v.Checks {
		if c.Name != moveCheckOrder[i] || c.Result != MoveCheckPassed {
			t.Errorf("check %d = %+v, want %s passed", i, c, moveCheckOrder[i])
		}
		// Only workspace_on_export explains its pass: the directory itself
		// is confirmed on the target at move time.
		if c.Name == moveCheckWorkspaceOnExport {
			if !strings.Contains(c.Message, "at move time") {
				t.Errorf("workspace_on_export message %q does not say the workspace is confirmed at move time", c.Message)
			}
		} else if c.Message != "" {
			t.Errorf("check %s passed with message %q", c.Name, c.Message)
		}
	}
	for _, k := range []string{"reachable:dst", "reachable:src", "dispatch", "provider", "passthrough", "capacity"} {
		if calls[k] != 1 {
			t.Errorf("probe %s called %d times, want 1", k, calls[k])
		}
	}
	if calls["serves"] != 0 {
		t.Errorf("serves-project probe called %d times for a user move, want 0", calls["serves"])
	}
}

// With no pinned profile, the move resolves the target's own default
// profile, even when the source's default differs (A6).
func TestEvaluateMoveEligibility_ResolvesTargetDefaultProfile(t *testing.T) {
	in := eligibleMoveInput(moveTestProbeCalls{})
	in.Dst.DefaultProfile = "gke"
	in.Dst.Profiles = append(in.Dst.Profiles, store.BrokerProfile{Name: "gke", Type: "kubernetes", Available: true})
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if v.Profile != "gke" || v.RuntimeType != "kubernetes" {
		t.Errorf("profile = %q/%q, want the target default gke/kubernetes", v.Profile, v.RuntimeType)
	}
}

// An agent moving itself to a broker that serves its project passes access
// on the serves-project check alone: it needs no dispatch scope and no
// provider-link right, and those probes are not consulted (A8).
func TestEvaluateMoveEligibility_SelfMoveToServingBroker(t *testing.T) {
	calls := moveTestProbeCalls{}
	in := eligibleMoveInput(calls)
	in.SelfMove = true
	in.Probes.CanDispatch = func(*store.RuntimeBroker) bool { calls["dispatch"]++; return false }
	in.Probes.CanUseAsProvider = func(*store.RuntimeBroker) bool { calls["provider"]++; return false }
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if !v.Eligible {
		t.Fatal("verdict not eligible")
	}
	if calls["serves"] != 1 || calls["dispatch"] != 0 || calls["provider"] != 0 {
		t.Errorf("probe calls = %v, want serves once and no dispatch/provider", calls)
	}
}

func TestEvaluateMoveEligibility_SharedWorkspaceOnAnyRuntime(t *testing.T) {
	in := eligibleMoveInput(moveTestProbeCalls{})
	in.CloneMode = false
	in.Agent.Runtime = "docker"
	in.Profile = "local"
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		t.Fatalf("unexpected refusal: %+v", ref)
	}
	if v.Profile != "local" || v.RuntimeType != "docker" {
		t.Errorf("profile = %q/%q, want local/docker", v.Profile, v.RuntimeType)
	}
}

func TestEvaluateMoveEligibility_Refusals(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(in *moveEligibilityInput, calls moveTestProbeCalls)
		check      string
		status     int
		code       string
		msgContain string
		// probes that must not have been called (they belong to later checks).
		notCalled []string
	}{
		{
			name:   "workspace mode",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.WorkspaceModeError = "worktree workspaces cannot move" },
			check:  moveCheckWorkspaceMode, status: http.StatusBadRequest, code: ErrCodeValidationError,
			msgContain: "worktree workspaces cannot move",
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name:   "target descriptor absent (old broker)",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Dst.WorkspaceStorage = nil },
			check:  moveCheckWorkspaceStorage, status: http.StatusPreconditionFailed, code: ErrCodeUnsupportedCapability,
			msgContain: "broker dst-name does not advertise workspace storage",
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name:   "source descriptor absent (old broker)",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Src.WorkspaceStorage = nil },
			check:  moveCheckWorkspaceStorage, status: http.StatusPreconditionFailed, code: ErrCodeUnsupportedCapability,
			msgContain: "broker src-name does not advertise workspace storage",
		},
		{
			name: "target is local storage",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Dst.WorkspaceStorage = &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal}
			},
			check: moveCheckSameExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "mounts backend local",
		},
		{
			name: "different export",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Dst.WorkspaceStorage = moveTestStorage("10.0.0.2", "/vol2", "projects", true)
			},
			check: moveCheckSameExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "10.0.0.2:/vol2",
		},
		{
			name: "different subpath root",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Dst.WorkspaceStorage = moveTestStorage("10.0.0.2", "/vol1", "other", true)
			},
			check: moveCheckSameExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `subpath root "other"`,
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name: "target reports no export identity marker",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Dst.WorkspaceStorage.NFS.ExportID = ""
			},
			check: moveCheckSameExport, status: http.StatusPreconditionFailed, code: ErrCodeUnsupportedCapability,
			msgContain: "broker dst-name has not reported the export identity marker (projects/.scion-export-id",
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name: "source reports no export identity marker (old broker)",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Src.WorkspaceStorage.NFS.ExportID = ""
			},
			check: moveCheckSameExport, status: http.StatusPreconditionFailed, code: ErrCodeUnsupportedCapability,
			msgContain: "broker src-name has not reported the export identity marker",
		},
		{
			name: "same export settings, different export identity",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Dst.WorkspaceStorage.NFS.ExportID = "0a0a0a0a-0000-4000-8000-000000000000"
			},
			check: moveCheckSameExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "see different export identity markers (" + moveTestExportID + " and 0a0a0a0a-0000-4000-8000-000000000000)",
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name:   "gcs-synced workspace",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Agent.AppliedConfig.WorkspaceStoragePath = "gs://bucket/ws" },
			check:  moveCheckWorkspaceOnExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "GCS-synced",
		},
		{
			name:   "workspace placement unknown",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Agent.WorkspacePlacement = "" },
			check:  moveCheckWorkspaceOnExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "workspace placement is unknown (it has not started since placements were recorded); re-provision the agent once",
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name:   "workspace placement local",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Agent.WorkspacePlacement = api.WorkspacePlacementLocal },
			check:  moveCheckWorkspaceOnExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `not on the shared NFS export on broker src-name (placement "local")`,
		},
		{
			name:   "workspace placement unrecognised",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Agent.WorkspacePlacement = "pvc" },
			check:  moveCheckWorkspaceOnExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `placement "pvc" is not recognised as the shared NFS export; re-provision the agent once`,
		},
		{
			name:   "clone-per-agent on docker",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Agent.Runtime = "docker" },
			check:  moveCheckWorkspaceOnExport, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `runtime "docker"`,
		},
		{
			name:   "unknown target profile",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Profile = "gpu" },
			check:  moveCheckTargetProfile, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `has no profile "gpu"`,
		},
		{
			name:   "no target default profile",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Dst.DefaultProfile = "" },
			check:  moveCheckTargetProfile, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "has no default profile",
		},
		{
			name:   "target profile unavailable",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Dst.Profiles[0].Available = false },
			check:  moveCheckTargetProfile, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `profile "k8s" is not available`,
		},
		{
			name:   "clone-per-agent onto docker profile",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Profile = "local" },
			check:  moveCheckTargetProfile, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: `runtime type "docker"`,
		},
		{
			name:   "target mount unhealthy",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Dst.WorkspaceStorage.NFS.Healthy = false },
			check:  moveCheckTargetHealth, status: http.StatusServiceUnavailable, code: ErrCodeRuntimeBrokerUnavail,
			msgContain: "NFS export mount unhealthy",
			notCalled:  []string{"reachable:dst", "dispatch", "capacity"},
		},
		{
			name: "target unreachable",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Probes.Reachable = func(b *store.RuntimeBroker) bool { return b.ID != "dst" }
			},
			check: moveCheckTargetHealth, status: http.StatusServiceUnavailable, code: ErrCodeRuntimeBrokerUnavail,
			msgContain: "broker dst-name is unavailable",
			notCalled:  []string{"dispatch", "capacity"},
		},
		{
			name: "no dispatch permission",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Probes.CanDispatch = func(*store.RuntimeBroker) bool { return false }
			},
			check: moveCheckAccess, status: http.StatusForbidden, code: ErrCodeForbidden,
			msgContain: "permission to run agents",
			notCalled:  []string{"provider", "capacity"},
		},
		{
			name: "cannot link target as provider",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Probes.CanUseAsProvider = func(*store.RuntimeBroker) bool { return false }
			},
			check: moveCheckAccess, status: http.StatusForbidden, code: ErrCodeForbidden,
			msgContain: "not a provider for this project",
		},
		{
			name: "passthrough denied on target",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Probes.Passthrough = func(*store.RuntimeBroker) string { return "broker not trusted" }
			},
			check: moveCheckAccess, status: http.StatusForbidden, code: ErrCodeForbidden,
			msgContain: "broker not trusted",
			notCalled:  []string{"capacity"},
		},
		{
			name: "self-move with a passthrough identity",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.SelfMove = true
				in.Probes.Passthrough = func(*store.RuntimeBroker) string { calls["passthrough"]++; return "" }
			},
			check: moveCheckAccess, status: http.StatusForbidden, code: ErrCodeForbidden,
			msgContain: "this agent cannot move itself; ask a user to move you",
			notCalled:  []string{"passthrough", "serves", "dispatch", "provider", "capacity"},
		},
		{
			name: "self-move to a broker not serving the project",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.SelfMove = true
				in.Probes.ServesProject = func(*store.RuntimeBroker) bool { return false }
			},
			check: moveCheckAccess, status: http.StatusConflict, code: ErrCodeConflict,
			msgContain: "target broker dst-name does not serve this project",
			notCalled:  []string{"dispatch", "provider", "capacity"},
		},
		{
			name:   "target lacks agent move (live phase-1 case)",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Dst.Capabilities.AgentMove = false },
			check:  moveCheckCapability, status: http.StatusPreconditionFailed, code: ErrCodeUnsupportedCapability,
			msgContain: "broker dst-name does not support agent move",
			notCalled:  []string{"reachable:src", "capacity"},
		},
		{
			name:   "source lacks agent move",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) { in.Src.Capabilities = nil },
			check:  moveCheckCapability, status: http.StatusPreconditionFailed, code: ErrCodeUnsupportedCapability,
			msgContain: "broker src-name does not support agent move",
		},
		{
			name: "source offline",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Probes.Reachable = func(b *store.RuntimeBroker) bool { return b.ID != "src" }
			},
			check: moveCheckCapability, status: http.StatusPreconditionFailed, code: ErrCodeRuntimeBrokerUnavail,
			msgContain: "source broker src-name is unavailable",
			notCalled:  []string{"capacity"},
		},
		{
			name: "target at agent limit",
			mutate: func(in *moveEligibilityInput, calls moveTestProbeCalls) {
				in.Probes.Capacity = func(*store.RuntimeBroker) string { return "broker dst-name is at its agent limit (5 of 5)" }
			},
			check: moveCheckCapacity, status: http.StatusTooManyRequests, code: ErrCodeQuotaExceeded,
			msgContain: "agent limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := moveTestProbeCalls{}
			in := eligibleMoveInput(calls)
			tc.mutate(&in, calls)

			v, ref := evaluateMoveEligibility(in)
			if ref == nil {
				t.Fatalf("expected refusal at %s, got eligible verdict", tc.check)
			}
			if ref.Status != tc.status || ref.Code != tc.code {
				t.Errorf("refusal = %d %s, want %d %s", ref.Status, ref.Code, tc.status, tc.code)
			}
			if !strings.Contains(ref.Message, tc.msgContain) {
				t.Errorf("message %q does not contain %q", ref.Message, tc.msgContain)
			}
			if v.Eligible {
				t.Error("refused verdict marked eligible")
			}
			if len(v.Checks) != len(moveCheckOrder) {
				t.Fatalf("got %d checks, want all %d", len(v.Checks), len(moveCheckOrder))
			}
			failedAt := -1
			for i, c := range v.Checks {
				if c.Name != moveCheckOrder[i] {
					t.Errorf("check %d name = %s, want %s", i, c.Name, moveCheckOrder[i])
				}
				if c.Name == tc.check {
					failedAt = i
				}
			}
			if failedAt < 0 {
				t.Fatalf("check %s not in verdict", tc.check)
			}
			for i, c := range v.Checks {
				want := MoveCheckNotEvaluated
				switch {
				case i < failedAt:
					want = MoveCheckPassed
				case i == failedAt:
					want = MoveCheckFailed
				}
				if c.Result != want {
					t.Errorf("check %s = %s, want %s", c.Name, c.Result, want)
				}
			}
			if v.Checks[failedAt].Message != ref.Message {
				t.Errorf("failed check message %q != refusal message %q", v.Checks[failedAt].Message, ref.Message)
			}
			for _, k := range tc.notCalled {
				if calls[k] != 0 {
					t.Errorf("probe %s called after refusal at %s", k, tc.check)
				}
			}
		})
	}
}
