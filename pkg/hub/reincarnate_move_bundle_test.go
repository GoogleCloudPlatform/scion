//go:build !no_sqlite

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
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// bundleMoveInput returns a moveEligibilityInput where export fails on a
// waivable check (different NFS exports / local workspace) and all bundle
// preconditions pass.
func bundleMoveInput(calls moveTestProbeCalls) moveEligibilityInput {
	in := forceMoveInput(calls)
	in.Force = false
	breakSameExport(&in)
	in.BundleExperimentEnabled = true
	in.BundleStorageError = ""
	for _, b := range []*store.RuntimeBroker{in.Src, in.Dst} {
		b.Capabilities = &store.BrokerCapabilities{
			AgentMove:         true,
			AgentMoveRecreate: true,
			AgentMoveBundle:   true,
		}
	}
	in.Probes.BundleSource = func(src *store.RuntimeBroker, agent *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
		calls["bundle_source:"+src.ID]++
		return &api.MoveBundlePrecheckResponse{
			Durable: true,
			Workspace: api.MoveBundleRootEstimate{
				Bytes:   12 << 20,
				Entries: 420,
			},
			Home: api.MoveBundleRootEstimate{
				Bytes:   4 << 20,
				Entries: 80,
			},
		}, nil
	}
	return in
}

func TestMoveBundleStorageCheck(t *testing.T) {
	if got := moveBundleStorageCheck(nil); got != "hub object storage is not configured" {
		t.Fatalf("nil storage = %q", got)
	}
	localStor := &mockStorage{provider: storage.ProviderLocal}
	if got := moveBundleStorageCheck(localStor); got != `hub object storage provider "local" cannot issue signed URLs a remote broker can use` {
		t.Fatalf("local storage = %q", got)
	}
	gcsStor := &mockStorage{provider: storage.ProviderGCS}
	if got := moveBundleStorageCheck(gcsStor); got != "" {
		t.Fatalf("gcs storage = %q, want empty", got)
	}
}

// T8: Export always wins over bundle, even when bundle is eligible and even
// with --force.
func TestEvaluateMoveEligibility_BundleLadder_ExportAlwaysWins(t *testing.T) {
	for _, force := range []bool{false, true} {
		calls := moveTestProbeCalls{}
		in := bundleMoveInput(calls)
		in.Force = force
		// Restore matching NFS exports so export succeeds.
		in.Dst.WorkspaceStorage = in.Src.WorkspaceStorage

		v, ref := evaluateMoveEligibility(in)
		if ref != nil {
			t.Fatalf("force=%v: unexpected refusal %+v", force, ref)
		}
		if v.TransferMode != MoveTransferModeExport {
			t.Fatalf("force=%v: mode = %q, want export", force, v.TransferMode)
		}
		if calls["bundle_source:"+in.Src.ID] != 0 {
			t.Errorf("force=%v: bundle_source probe should not run when export succeeds", force)
		}
	}
}

// T8: Every waivable export failure falls through to bundle, with or without
// --force (D4: --force with bundle eligible -> bundle).
func TestEvaluateMoveEligibility_BundleLadder_WaivableFailuresSelectBundle(t *testing.T) {
	breakers := []struct {
		name  string
		apply func(*moveEligibilityInput)
	}{
		{"workspace_storage_reported", breakWorkspaceStorage},
		{"same_export", breakSameExport},
		{"workspace_on_export", breakWorkspaceOnExport},
		{"placementCaused target_profile", func(in *moveEligibilityInput) {
			in.Dst.WorkspaceStorage = in.Src.WorkspaceStorage
			in.Dst.Profiles = []store.BrokerProfile{{Name: "default", Type: "docker", Available: true}}
			in.Profile = "default"
		}},
	}
	for _, br := range breakers {
		for _, force := range []bool{false, true} {
			calls := moveTestProbeCalls{}
			in := bundleMoveInput(calls)
			in.Force = force
			// Reset to matching export, then apply the specific breaker.
			in.Dst.WorkspaceStorage = in.Src.WorkspaceStorage
			br.apply(&in)

			v, ref := evaluateMoveEligibility(in)
			if ref != nil {
				t.Fatalf("%s (force=%v): unexpected refusal %+v", br.name, force, ref)
			}
			if !v.Eligible || v.TransferMode != MoveTransferModeBundle {
				t.Fatalf("%s (force=%v): eligible=%v mode=%q, want true/bundle", br.name, force, v.Eligible, v.TransferMode)
			}
			if v.BundleEstimate == nil || v.BundleEstimate.WorkspaceBytes != 12<<20 || v.BundleEstimate.HomeBytes != 4<<20 {
				t.Fatalf("%s (force=%v): unexpected estimate %+v", br.name, force, v.BundleEstimate)
			}
			if v.BundleUnavailable != "" {
				t.Errorf("%s (force=%v): BundleUnavailable = %q, want empty", br.name, force, v.BundleUnavailable)
			}
			res := checkResults(v)
			for _, waived := range []string{moveCheckWorkspaceStorage, moveCheckSameExport, moveCheckWorkspaceOnExport} {
				if res[waived] != MoveCheckWaived {
					t.Errorf("%s (force=%v): %s = %s, want waived", br.name, force, waived, res[waived])
				}
			}
			if res[moveCheckWorkspaceBundleable] != MoveCheckPassed {
				t.Errorf("%s (force=%v): workspace_bundleable = %s, want passed", br.name, force, res[moveCheckWorkspaceBundleable])
			}
			keys := consequenceKeys(v)
			wantKinds := map[string]string{
				"identity":        MoveConsequenceKept,
				"relationships":   MoveConsequenceKept,
				"config":          MoveConsequenceKept,
				"workspace":       MoveConsequenceCarried,
				"home":            MoveConsequenceCarried,
				"bundle-size":     MoveConsequenceCarried,
				"handoff":         MoveConsequenceCarried,
				"harness-session": MoveConsequenceRecreated,
				"credentials":     MoveConsequenceRecreated,
				"process-state":   MoveConsequenceLost,
			}
			for item, wantKind := range wantKinds {
				if keys[item] != wantKind {
					t.Errorf("%s (force=%v): consequence %q = %q, want %q", br.name, force, item, keys[item], wantKind)
				}
			}
			if _, hasLostWork := keys["local-work"]; hasLostWork {
				t.Errorf("%s (force=%v): bundle mode must not list local-work as lost", br.name, force)
			}
		}
	}
}

// T8: empty-per-agent and clone-per-agent without git remote are both bundleable.
func TestEvaluateMoveEligibility_Bundle_WorkspaceModes(t *testing.T) {
	t.Run("empty-per-agent", func(t *testing.T) {
		calls := moveTestProbeCalls{}
		in := bundleMoveInput(calls)
		in.EmptyPerAgent = true
		in.GitRemote = ""
		in.WorkspaceMode = string(store.SharingModeEmptyPerAgent)
		v, ref := evaluateMoveEligibility(in)
		if ref != nil || v.TransferMode != MoveTransferModeBundle {
			t.Fatalf("got mode=%q ref=%+v, want bundle", v.TransferMode, ref)
		}
	})
	t.Run("clone-per-agent without git remote", func(t *testing.T) {
		calls := moveTestProbeCalls{}
		in := bundleMoveInput(calls)
		in.GitRemote = ""
		v, ref := evaluateMoveEligibility(in)
		if ref != nil || v.TransferMode != MoveTransferModeBundle {
			t.Fatalf("got mode=%q ref=%+v, want bundle", v.TransferMode, ref)
		}
	})
	t.Run("shared workspace not bundleable", func(t *testing.T) {
		calls := moveTestProbeCalls{}
		in := bundleMoveInput(calls)
		in.CloneMode = false
		in.WorkspaceMode = string(store.SharingModeSharedPlain)
		v, ref := evaluateMoveEligibility(in)
		if ref == nil {
			t.Fatal("expected refusal for shared-plain workspace")
		}
		if !strings.Contains(v.BundleUnavailable, "clone-per-agent and empty-per-agent") {
			t.Errorf("BundleUnavailable = %q, want workspace_bundleable reason", v.BundleUnavailable)
		}
		if calls["bundle_source:"+in.Src.ID] != 0 {
			t.Errorf("bundle_source should not be called when workspace_bundleable fails")
		}
	})
}

// T8: Target health in bundle mode only checks Reachable(dst), not NFS export mount.
func TestEvaluateMoveEligibility_Bundle_TargetHealthIgnoresUnhealthyNFS(t *testing.T) {
	calls := moveTestProbeCalls{}
	in := bundleMoveInput(calls)
	in.Dst.WorkspaceStorage = moveTestStorage("10.9.9.9", "/other", "projects", false)
	v, ref := evaluateMoveEligibility(in)
	if ref != nil || v.TransferMode != MoveTransferModeBundle {
		t.Fatalf("got mode=%q ref=%+v, want bundle even with unhealthy target NFS mount", v.TransferMode, ref)
	}
}

// T8: Bundle failure matrix -> without --force: export refusal + ForceWouldApply + BundleUnavailable;
// with --force: falls through to recreate.
func TestEvaluateMoveEligibility_BundleUnavailableMatrix(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func(*moveEligibilityInput)
		wantSubstr     string
		expectProbeRun bool
		recreateOK     bool
	}{
		{
			name:           "experiment off",
			mutate:         func(in *moveEligibilityInput) { in.BundleExperimentEnabled = false },
			wantSubstr:     experiments.AgentMoveBundle,
			expectProbeRun: false,
			recreateOK:     true,
		},
		{
			name:           "storage nil",
			mutate:         func(in *moveEligibilityInput) { in.BundleStorageError = moveBundleStorageCheck(nil) },
			wantSubstr:     "hub object storage is not configured",
			expectProbeRun: false,
			recreateOK:     true,
		},
		{
			name: "storage local",
			mutate: func(in *moveEligibilityInput) {
				in.BundleStorageError = moveBundleStorageCheck(&mockStorage{provider: storage.ProviderLocal})
			},
			wantSubstr:     `hub object storage provider "local" cannot issue signed URLs`,
			expectProbeRun: false,
			recreateOK:     true,
		},
		{
			name: "src lacks AgentMoveBundle",
			mutate: func(in *moveEligibilityInput) {
				in.Src.Capabilities.AgentMoveBundle = false
			},
			wantSubstr:     "does not support bundle move",
			expectProbeRun: false,
			recreateOK:     true,
		},
		{
			name: "dst lacks AgentMoveBundle",
			mutate: func(in *moveEligibilityInput) {
				in.Dst.Capabilities.AgentMoveBundle = false
			},
			wantSubstr:     "does not support bundle move",
			expectProbeRun: false,
			recreateOK:     true,
		},
		{
			name: "precheck non-durable",
			mutate: func(in *moveEligibilityInput) {
				in.Probes.BundleSource = func(src *store.RuntimeBroker, _ *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
					return &api.MoveBundlePrecheckResponse{Durable: false, Reason: "not yet supported"}, nil
				}
			},
			wantSubstr:     "not yet supported",
			expectProbeRun: true,
			recreateOK:     true,
		},
		{
			name: "precheck over workspace.bytes",
			mutate: func(in *moveEligibilityInput) {
				in.Probes.BundleSource = func(src *store.RuntimeBroker, _ *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
					return &api.MoveBundlePrecheckResponse{
						Durable:   true,
						Over:      "workspace.bytes",
						Workspace: api.MoveBundleRootEstimate{Bytes: 5 << 30, Over: "bytes"},
					}, nil
				}
			},
			wantSubstr:     "bundle exceeds limit: workspace bytes",
			expectProbeRun: true,
			recreateOK:     true,
		},
		{
			name: "precheck error",
			mutate: func(in *moveEligibilityInput) {
				in.Probes.BundleSource = func(src *store.RuntimeBroker, _ *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
					return nil, errors.New(`Post "http://10.0.0.1:9800/x": dial tcp`)
				}
			},
			wantSubstr:     "source bundle precheck failed",
			expectProbeRun: true,
			recreateOK:     true,
		},
		{
			name: "secret targets error",
			mutate: func(in *moveEligibilityInput) {
				in.Probes.BundleSource = func(src *store.RuntimeBroker, _ *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
					return nil, errMoveBundleSecretTargetsFailed
				}
			},
			wantSubstr:     "could not resolve the agent's secret targets",
			expectProbeRun: true,
			recreateOK:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/no_force", func(t *testing.T) {
			calls := moveTestProbeCalls{}
			in := bundleMoveInput(calls)
			in.Force = false
			tc.mutate(&in)

			v, ref := evaluateMoveEligibility(in)
			if ref == nil {
				t.Fatalf("expected refusal, got eligible mode=%q", v.TransferMode)
			}
			if ref.Status != http.StatusConflict {
				t.Errorf("status = %d, want 409 (export same_export refusal)", ref.Status)
			}
			if !ref.ForceWouldApply {
				t.Errorf("ForceWouldApply = false, want true")
			}
			if !strings.Contains(v.BundleUnavailable, tc.wantSubstr) {
				t.Errorf("BundleUnavailable = %q, want substring %q", v.BundleUnavailable, tc.wantSubstr)
			}
			if strings.Contains(v.BundleUnavailable, "10.0.0.1") || strings.Contains(v.BundleUnavailable, "http://") {
				t.Errorf("BundleUnavailable leaked internal endpoint URL: %q", v.BundleUnavailable)
			}
			if !tc.expectProbeRun && calls["bundle_source:"+in.Src.ID] != 0 {
				t.Errorf("bundle_source probe ran unexpectedly")
			}
		})

		t.Run(tc.name+"/with_force_falls_back_to_recreate", func(t *testing.T) {
			calls := moveTestProbeCalls{}
			in := bundleMoveInput(calls)
			in.Force = true
			tc.mutate(&in)

			v, ref := evaluateMoveEligibility(in)
			if tc.recreateOK {
				if ref != nil || v.TransferMode != MoveTransferModeRecreate {
					t.Fatalf("got mode=%q ref=%+v, want recreate", v.TransferMode, ref)
				}
				// O4: forced recreate carries BundleUnavailable explaining why bundle did not apply.
				if !strings.Contains(v.BundleUnavailable, tc.wantSubstr) {
					t.Errorf("forced recreate BundleUnavailable = %q, want substring %q", v.BundleUnavailable, tc.wantSubstr)
				}
				if strings.Contains(v.BundleUnavailable, "10.0.0.1") || strings.Contains(v.BundleUnavailable, "http://") {
					t.Errorf("forced recreate BundleUnavailable leaked internal endpoint URL: %q", v.BundleUnavailable)
				}
			}
		})
	}
}

// T8 (O5b): bundle_experiment runs before workspace_bundleable so an off-experiment
// hub reports the experiment requirement even for shared-workspace agents.
func TestEvaluateMoveEligibility_BundleExperimentBeforeWorkspaceBundleable(t *testing.T) {
	calls := moveTestProbeCalls{}
	in := bundleMoveInput(calls)
	in.BundleExperimentEnabled = false
	in.CloneMode = false
	in.WorkspaceMode = string(store.SharingModeSharedPlain)

	v, ref := evaluateMoveEligibility(in)
	if ref == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(v.BundleUnavailable, experiments.AgentMoveBundle) {
		t.Errorf("BundleUnavailable = %q, want experiment reason before workspace_bundleable", v.BundleUnavailable)
	}
}

// T8: Non-waivable export refusal (e.g. flat broker) never tries bundle and
// leaves BundleUnavailable empty.
func TestEvaluateMoveEligibility_NonWaivableNeverTriesBundle(t *testing.T) {
	calls := moveTestProbeCalls{}
	in := bundleMoveInput(calls)
	in.WorkspaceModeError = "linked projects cannot move"
	v, ref := evaluateMoveEligibility(in)
	if ref == nil {
		t.Fatal("expected refusal")
	}
	if v.BundleUnavailable != "" {
		t.Errorf("BundleUnavailable = %q, want empty on non-waivable failure", v.BundleUnavailable)
	}
	if calls["bundle_source:"+in.Src.ID] != 0 {
		t.Errorf("bundle_source should not run on non-waivable failure")
	}
}
