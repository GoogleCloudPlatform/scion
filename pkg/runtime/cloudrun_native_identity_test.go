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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Example accounts for the hub.cloudrun_native_identity tests. They stand
// for the runtime-config account, an assigned account and a block account.
const (
	nativeIDInstanceSA = "instance-sa@example.com"
	nativeIDAssignedSA = "assigned-sa@example.com"
	nativeIDBlockSA    = "block-sa@example.com"
	nativeIDRunID      = "run-1"
)

// nativeIdentityRuntime is newReadyTestRuntime (fake-clock wait bounds)
// with a configured runtime account and an exec connector that records
// forgotten keys.
func nativeIdentityRuntime(t *testing.T, fake *fakeInstancesClient) (*CloudRunRuntime, *forgettingExecConnector) {
	t.Helper()
	rt := newReadyTestRuntime(t, fake)
	rt.config.ServiceAccount = nativeIDInstanceSA
	conn := &forgettingExecConnector{}
	rt.exec = conn
	return rt, conn
}

// nativeIdentityRunConfig is runConfigForTest with a run ID and the given
// dispatch identity (nil: the experiment off).
func nativeIdentityRunConfig(id *api.CloudRunIdentity) RunConfig {
	cfg := runConfigForTest()
	cfg.Labels[api.LabelRunID] = nativeIDRunID
	cfg.CloudRunIdentity = id
	return cfg
}

// nativeIdentityInstance is an existing instance of the agent with the
// given run label and identity label ("" for none).
func nativeIdentityInstance(runID, identityLabel string, state *runpb.Instance) *runpb.Instance {
	inst := &runpb.Instance{}
	if state != nil {
		inst = proto.Clone(state).(*runpb.Instance)
	}
	inst.Name = "projects/test-project/locations/us-central1/instances/" + cloudRunInstanceID("agent-1")
	inst.Etag = "etag-1"
	inst.Labels = map[string]string{"agent_id": "agent-1"}
	if runID != "" {
		inst.Labels[sanitizeGCPLabelKey(api.LabelRunID)] = runID
	}
	if identityLabel != "" {
		inst.Labels[cloudRunIdentityLabel] = identityLabel
	}
	return inst
}

func TestCloudRunIdentityHash(t *testing.T) {
	got := cloudRunIdentityHash(nativeIDAssignedSA)
	if len(got) != 16 || strings.Trim(got, "0123456789abcdef") != "" {
		t.Fatalf("hash = %q, want 16 lower-case hex characters", got)
	}
	if upper := cloudRunIdentityHash(strings.ToUpper(nativeIDAssignedSA)); upper != got {
		t.Errorf("hash of the upper-cased account = %q, want %q (case-insensitive)", upper, got)
	}
	if other := cloudRunIdentityHash(nativeIDBlockSA); other == got {
		t.Errorf("different accounts share the hash %q", got)
	}
	if sanitizeGCPLabelValue(got) != got {
		t.Errorf("hash %q is not a valid label value as is", got)
	}
}

// buildCloudRunInstance: nil keeps the configured account and adds no
// label; a dispatch identity, whatever its mode, sets the account and the
// scion-identity label.
func TestBuildCloudRunInstance_CloudRunIdentity(t *testing.T) {
	rt, _ := nativeIdentityRuntime(t, &fakeInstancesClient{})
	tests := []struct {
		name      string
		id        *api.CloudRunIdentity
		wantSA    string
		wantLabel string
		// callerLabel, when set, is a dispatch label key that stores as
		// the identity label; the identity hash must win over its value.
		callerLabel string
	}{
		{name: "nil uses the configured account", id: nil, wantSA: nativeIDInstanceSA},
		{name: "assign", id: &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"},
			wantSA: nativeIDAssignedSA, wantLabel: cloudRunIdentityHash(nativeIDAssignedSA)},
		{name: "block", id: &api.CloudRunIdentity{ServiceAccount: nativeIDBlockSA, Mode: "block"},
			wantSA: nativeIDBlockSA, wantLabel: cloudRunIdentityHash(nativeIDBlockSA)},
		{name: "caller scion-identity label does not shadow the identity", id: &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"},
			wantSA: nativeIDAssignedSA, wantLabel: cloudRunIdentityHash(nativeIDAssignedSA), callerLabel: "scion-identity"},
		{name: "caller Scion-Identity label does not shadow the identity", id: &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"},
			wantSA: nativeIDAssignedSA, wantLabel: cloudRunIdentityHash(nativeIDAssignedSA), callerLabel: "Scion-Identity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := nativeIdentityRunConfig(tc.id)
			if tc.callerLabel != "" {
				cfg.Labels[tc.callerLabel] = "caller-value"
			}
			inst := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
			if inst.GetServiceAccount() != tc.wantSA {
				t.Errorf("ServiceAccount = %q, want %q", inst.GetServiceAccount(), tc.wantSA)
			}
			label, ok := inst.GetLabels()[cloudRunIdentityLabel]
			if tc.wantLabel == "" && ok {
				t.Errorf("label %s = %q, want none", cloudRunIdentityLabel, label)
			}
			if tc.wantLabel != "" && label != tc.wantLabel {
				t.Errorf("label %s = %q, want %q", cloudRunIdentityLabel, label, tc.wantLabel)
			}
			for k, v := range inst.GetLabels() {
				if strings.Contains(v, "@") || strings.Contains(v, "example") {
					t.Errorf("label %s = %q carries the account", k, v)
				}
			}
		})
	}
}

// Gate off: the instance is built exactly as without the field.
// The spec with an identity differs from the gate-off spec only in the
// service account and the scion-identity label, and the gate-off spec has
// the configured account and exactly the sanitized dispatch labels.
func TestBuildCloudRunInstance_GateOffUnchanged(t *testing.T) {
	rt, _ := nativeIdentityRuntime(t, &fakeInstancesClient{})
	rt.config.Network = "net-1"
	rt.config.Subnetwork = "subnet-1"
	paths, _ := stubCloudRunNFSProvision(context.Background(), RunConfig{}, "agent-1", 1000, 1000)
	base := nativeIdentityRunConfig(nil)
	base.Env = []string{"FOO=bar", "SCION_METADATA_MODE=block", "SCION_METADATA_SA_EMAIL="}
	base.HomeDir = "/home/scion"
	base.CommandArgs = []string{"sleep", "60"}

	off := rt.buildCloudRunInstance(base, 1000, 1000, paths)
	if off.GetServiceAccount() != nativeIDInstanceSA {
		t.Errorf("gate off: ServiceAccount = %q, want the configured account", off.GetServiceAccount())
	}
	wantLabels := map[string]string{}
	for k, v := range base.Labels {
		wantLabels[sanitizeGCPLabelKey(k)] = sanitizeGCPLabelValue(v)
	}
	if len(off.GetLabels()) != len(wantLabels) {
		t.Errorf("gate off: labels = %v, want %v", off.GetLabels(), wantLabels)
	}
	for k, v := range wantLabels {
		if off.GetLabels()[k] != v {
			t.Errorf("gate off: label %s = %q, want %q", k, off.GetLabels()[k], v)
		}
	}

	on := base
	on.Labels = map[string]string{}
	for k, v := range base.Labels {
		on.Labels[k] = v
	}
	on.CloudRunIdentity = &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}
	withID := rt.buildCloudRunInstance(on, 1000, 1000, paths)
	// Undo exactly the two documented differences; the rest must match.
	withID.ServiceAccount = nativeIDInstanceSA
	delete(withID.Labels, cloudRunIdentityLabel)
	if !proto.Equal(off, withID) {
		t.Errorf("an identity changed more than the service account and the identity label:\n off: %v\n on:  %v", off, withID)
	}
}

// A dispatch identity with no account is refused before any provisioning
// or Instances API call: never the configured account.
func TestCloudRunRun_EmptyIdentityAccountRefused(t *testing.T) {
	for _, account := range []string{"", "  "} {
		fake := &fakeInstancesClient{getErr: notFoundErr()}
		rt, _ := nativeIdentityRuntime(t, fake)
		provisioned := false
		rt.provisionNFS = func(context.Context, RunConfig, string, int, int) (*cloudRunNFSProvisionPaths, error) {
			provisioned = true
			return nil, errors.New("must not provision")
		}
		cfg := nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: account, Mode: "assign"})

		_, err := rt.Run(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "agent-1") {
			t.Fatalf("account %q: Run error = %v, want a refusal naming the agent", account, err)
		}
		if provisioned {
			t.Errorf("account %q: the NFS share was provisioned before the refusal", account)
		}
		if len(fake.getReqs)+len(fake.createReqs)+len(fake.startReqs)+len(fake.deleteReqs) != 0 {
			t.Errorf("account %q: Instances API called (get %d, create %d, start %d, delete %d), want none",
				account, len(fake.getReqs), len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
		}
	}
}

// No instance yet: the create carries the dispatch account and label.
func TestCloudRunRun_IdentityCreate(t *testing.T) {
	for _, id := range []*api.CloudRunIdentity{
		{ServiceAccount: nativeIDAssignedSA, Mode: "assign"},
		{ServiceAccount: nativeIDBlockSA, Mode: "block"},
	} {
		t.Run(id.Mode, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, _ := nativeIdentityRuntime(t, fake)
			cfg := nativeIdentityRunConfig(id)

			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fake.createReqs) != 1 || len(fake.startReqs) != 0 || len(fake.deleteReqs) != 0 {
				t.Fatalf("create %d, start %d, delete %d; want 1, 0, 0", len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
			}
			inst := fake.createReqs[0].GetInstance()
			if inst.GetServiceAccount() != id.ServiceAccount {
				t.Errorf("ServiceAccount = %q, want %q", inst.GetServiceAccount(), id.ServiceAccount)
			}
			if inst.GetLabels()[cloudRunIdentityLabel] != cloudRunIdentityHash(id.ServiceAccount) {
				t.Errorf("identity label = %q", inst.GetLabels()[cloudRunIdentityLabel])
			}
		})
	}
}

// An identity or secret change must recreate the instance, so with a
// dispatch identity this run's existing instance, running or stopped,
// with the same, a different or no identity label, and also one whose
// spec matches, is deleted run-checked (its exec key dropped), waited for
// until it is gone, and created again with the current account.
// StartInstance is never called. The start, restart and resume
// dispatches reach the runtime through the same Run, differing only in the
// RunConfig they carry (Resume for resume).
func TestCloudRunRun_IdentityReplacesSameRunInstance(t *testing.T) {
	assigned := &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}
	block := &api.CloudRunIdentity{ServiceAccount: nativeIDBlockSA, Mode: "block"}
	tests := []struct {
		name     string
		id       *api.CloudRunIdentity
		label    string
		state    *runpb.Instance
		resume   bool
		matching bool
	}{
		// The instance this start would create, so its spec matches;
		// without an identity it would be started.
		{name: "matching spec and identity", id: assigned, matching: true},
		{name: "different identity", id: assigned, label: cloudRunIdentityHash(nativeIDBlockSA)},
		{name: "same identity", id: assigned, label: cloudRunIdentityHash(nativeIDAssignedSA)},
		{name: "no identity label (created with the experiment off)", id: assigned},
		// Start and restart reach Run with the same RunConfig, so one row
		// covers both at this layer; the broker tests cover the dispatches.
		{name: "stopped instance, start or restart", id: assigned, label: cloudRunIdentityHash(nativeIDAssignedSA), state: recordedStopped()},
		{name: "stopped instance, resume", id: assigned, label: cloudRunIdentityHash(nativeIDAssignedSA), state: recordedStopped(), resume: true},
		{name: "revoked assignment, restart as block", id: block, label: cloudRunIdentityHash(nativeIDAssignedSA), state: recordedStopped()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, conn := nativeIdentityRuntime(t, fake)
			cfg := nativeIdentityRunConfig(tc.id)
			cfg.Resume = tc.resume
			existing := nativeIdentityInstance(nativeIDRunID, tc.label, tc.state)
			if tc.matching {
				existing = computedCloudRunInstance(t, rt, cfg)
			}
			// Run's read, then the delete's run-checked read; the wait for
			// the deleted instance then reads NotFound.
			fake.getSeq = []fakeGetResult{{inst: existing}, {inst: existing}}

			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fake.startReqs) != 0 {
				t.Errorf("StartInstance called %d times, want 0", len(fake.startReqs))
			}
			if len(fake.deleteReqs) != 1 {
				t.Fatalf("DeleteInstance called %d times, want 1", len(fake.deleteReqs))
			}
			if fake.deleteReqs[0].GetName() != existing.GetName() || fake.deleteReqs[0].GetEtag() != existing.GetEtag() {
				t.Errorf("delete = %v, want %s with the read's etag", fake.deleteReqs[0], existing.GetName())
			}
			if len(fake.createReqs) != 1 {
				t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
			}
			inst := fake.createReqs[0].GetInstance()
			// The matching row is about a match: the existing instance has
			// the spec of the one this Run created, and is still replaced.
			if tc.matching {
				if diff := cloudRunSpecDiff(existing, inst, map[string]bool{stagedsecrets.EnvVar: true}); len(diff) != 0 {
					t.Errorf("the matching row's instance differs from the created one in %v", diff)
				}
			}
			if inst.GetServiceAccount() != tc.id.ServiceAccount {
				t.Errorf("ServiceAccount = %q, want %q", inst.GetServiceAccount(), tc.id.ServiceAccount)
			}
			if inst.GetLabels()[cloudRunIdentityLabel] != cloudRunIdentityHash(tc.id.ServiceAccount) {
				t.Errorf("identity label = %q, want the current account's", inst.GetLabels()[cloudRunIdentityLabel])
			}
			want := "test-project/us-central1/" + cloudRunInstanceID("agent-1")
			if len(conn.forgotten) != 1 || conn.forgotten[0] != want {
				t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
			}
		})
	}
}

// A delete that finds the instance already gone still creates.
func TestCloudRunRun_IdentityReplaceInstanceGone(t *testing.T) {
	existing := nativeIdentityInstance(nativeIDRunID, "", nil)
	fake := &fakeInstancesClient{getSeq: []fakeGetResult{{inst: existing}, {err: notFoundErr()}}, getErr: notFoundErr()}
	rt, _ := nativeIdentityRuntime(t, fake)

	if _, err := rt.Run(context.Background(), nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 || len(fake.startReqs) != 0 {
		t.Errorf("create %d, start %d; want 1, 0", len(fake.createReqs), len(fake.startReqs))
	}
}

// A failed delete is reported and nothing is created or started.
func TestCloudRunRun_IdentityReplaceDeleteFails(t *testing.T) {
	existing := nativeIdentityInstance(nativeIDRunID, "", nil)
	fake := &fakeInstancesClient{getSeq: []fakeGetResult{{inst: existing}, {inst: existing}}, deleteErr: errors.New("boom")}
	rt, conn := nativeIdentityRuntime(t, fake)

	_, err := rt.Run(context.Background(), nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}))
	if err == nil {
		t.Fatal("Run succeeded, want the delete error")
	}
	if len(fake.createReqs) != 0 || len(fake.startReqs) != 0 {
		t.Errorf("create %d, start %d; want 0, 0", len(fake.createReqs), len(fake.startReqs))
	}
	// The exec key is dropped also after a failed delete.
	if len(conn.forgotten) != 1 {
		t.Errorf("Forget calls = %q, want one", conn.forgotten)
	}
}

// With a dispatch identity, this run's instance whose run label is gone
// at the delete's own read (an unlabelled instance created between the
// two reads) is not deleted: the delete is strict, and Run refuses with
// ErrRunConflict naming the agent, creating nothing. The exec key cached
// for that instance ID is dropped.
func TestCloudRunRun_IdentityReplaceUnlabelledAtDeleteRead(t *testing.T) {
	labelled := nativeIdentityInstance(nativeIDRunID, "", nil)
	unlabelled := nativeIdentityInstance("", "", nil)
	fake := &fakeInstancesClient{getSeq: []fakeGetResult{{inst: labelled}, {inst: unlabelled}}, getErr: notFoundErr()}
	rt, conn := nativeIdentityRuntime(t, fake)

	_, err := rt.Run(context.Background(), nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}))
	if !errors.Is(err, ErrRunConflict) || !strings.Contains(err.Error(), "agent-1") {
		t.Fatalf("Run error = %v, want ErrRunConflict naming the agent", err)
	}
	if len(fake.deleteReqs) != 0 || len(fake.createReqs) != 0 || len(fake.startReqs) != 0 {
		t.Errorf("delete %d, create %d, start %d; want 0, 0, 0", len(fake.deleteReqs), len(fake.createReqs), len(fake.startReqs))
	}
	want := "test-project/us-central1/" + cloudRunInstanceID("agent-1")
	if len(conn.forgotten) != 1 || conn.forgotten[0] != want {
		t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
	}
}

// With a dispatch identity, an instance that is being deleted (delete_time
// set) is never started: its exec key is dropped, Run waits until it is
// gone, then creates a new instance as the current account.
func TestCloudRunRun_IdentityInstanceBeingDeleted(t *testing.T) {
	deleting := nativeIdentityInstance(nativeIDRunID, cloudRunIdentityHash(nativeIDAssignedSA), nil)
	deleting.DeleteTime = timestamppb.New(time.Unix(1, 0))
	// Run's read finds it being deleted, the wait's first read still finds
	// it, and its second read finds it gone.
	fake := &fakeInstancesClient{getSeq: []fakeGetResult{{inst: deleting}, {inst: deleting}}, getErr: notFoundErr()}
	rt, conn := nativeIdentityRuntime(t, fake)

	if _, err := rt.Run(context.Background(), nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 0 || len(fake.deleteReqs) != 0 || len(fake.createReqs) != 1 {
		t.Errorf("start %d, delete %d, create %d; want 0, 0, 1", len(fake.startReqs), len(fake.deleteReqs), len(fake.createReqs))
	}
	// Run's read and two reads of the wait until the instance is gone; the
	// create's own ready instance needs no further read.
	if len(fake.getReqs) != 3 {
		t.Errorf("GetInstance called %d times, want 3", len(fake.getReqs))
	}
	if len(fake.createReqs) == 1 && fake.createReqs[0].GetInstance().GetServiceAccount() != nativeIDAssignedSA {
		t.Errorf("ServiceAccount = %q, want the assigned account", fake.createReqs[0].GetInstance().GetServiceAccount())
	}
	want := "test-project/us-central1/" + cloudRunInstanceID("agent-1")
	if len(conn.forgotten) != 1 || conn.forgotten[0] != want {
		t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
	}
}

// With a dispatch identity, an instance without a run label (or a start
// without a run ID) is refused with ErrRunConflict naming the agent, and
// another run's instance keeps today's ErrRunConflict. Nothing is deleted,
// created or started.
func TestCloudRunRun_IdentityRefusesUnmatchedInstance(t *testing.T) {
	tests := []struct {
		name       string
		instRun    string
		cfgRun     string
		wantText   string
		wantForget bool
	}{
		// The instance cannot be attributed to this run, so the exec key
		// cached for its name is dropped.
		{name: "legacy instance without a run label", instRun: "", cfgRun: nativeIDRunID, wantText: "agent-1", wantForget: true},
		{name: "start without a run ID", instRun: nativeIDRunID, cfgRun: "", wantText: "agent-1", wantForget: true},
		// Run refuses another run's live instance before the identity rule.
		{name: "another run's instance", instRun: "run-other", cfgRun: nativeIDRunID, wantText: "run-other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getInstance: nativeIdentityInstance(tc.instRun, "", nil)}
			rt, conn := nativeIdentityRuntime(t, fake)
			cfg := nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"})
			if tc.cfgRun == "" {
				delete(cfg.Labels, api.LabelRunID)
			}

			_, err := rt.Run(context.Background(), cfg)
			if !errors.Is(err, ErrRunConflict) {
				t.Fatalf("Run error = %v, want ErrRunConflict", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not name %q", err, tc.wantText)
			}
			if strings.Contains(err.Error(), nativeIDAssignedSA) {
				t.Errorf("error %q names the account", err)
			}
			if len(fake.deleteReqs)+len(fake.createReqs)+len(fake.startReqs) != 0 {
				t.Errorf("delete %d, create %d, start %d; want none", len(fake.deleteReqs), len(fake.createReqs), len(fake.startReqs))
			}
			want := "test-project/us-central1/" + cloudRunInstanceID("agent-1")
			if tc.wantForget && (len(conn.forgotten) != 1 || conn.forgotten[0] != want) {
				t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
			}
			if !tc.wantForget && len(conn.forgotten) != 0 {
				t.Errorf("Forget calls = %q, want none", conn.forgotten)
			}
		})
	}
}

// Gate off: this run's existing instance whose spec matches the computed
// one is started, not recreated, and nothing is deleted.
func TestCloudRunRun_GateOffReusesMatchingInstance(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt, conn := nativeIdentityRuntime(t, fake)
	cfg := nativeIdentityRunConfig(nil)
	fake.getInstance = computedCloudRunInstance(t, rt, cfg)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 1 || len(fake.deleteReqs) != 0 || len(fake.createReqs) != 0 {
		t.Errorf("start %d, delete %d, create %d; want 1, 0, 0", len(fake.startReqs), len(fake.deleteReqs), len(fake.createReqs))
	}
	if len(conn.forgotten) != 0 {
		t.Errorf("Forget calls = %q, want none", conn.forgotten)
	}
}

// A create that finds an instance already present (AlreadyExists): with a
// dispatch identity the instance was not created by this Run and is not
// adopted, so Run refuses with ErrRunConflict naming the agent, without a
// further read, a start or an exec key drop. Without an identity this
// run's matching instance is adopted.
func TestCloudRunRun_CreateAlreadyExistsAdoption(t *testing.T) {
	alreadyExists := status.Error(codes.AlreadyExists, "instance already exists")

	t.Run("identity: refused", func(t *testing.T) {
		fake := &fakeInstancesClient{getErr: notFoundErr(), createErr: alreadyExists}
		rt, conn := nativeIdentityRuntime(t, fake)

		_, err := rt.Run(context.Background(), nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}))
		if !errors.Is(err, ErrRunConflict) || !strings.Contains(err.Error(), "agent-1") {
			t.Fatalf("Run error = %v, want ErrRunConflict naming the agent", err)
		}
		if strings.Contains(err.Error(), nativeIDAssignedSA) {
			t.Errorf("error %q names the account", err)
		}
		if len(fake.createReqs) != 1 || len(fake.startReqs) != 0 || len(fake.deleteReqs) != 0 {
			t.Errorf("create %d, start %d, delete %d; want 1, 0, 0", len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
		}
		if len(fake.getReqs) != 1 {
			t.Errorf("GetInstance called %d times, want 1 (Run's read only; no adoption read)", len(fake.getReqs))
		}
		if len(conn.forgotten) != 0 {
			t.Errorf("Forget calls = %q, want none", conn.forgotten)
		}
	})

	t.Run("no identity: adopted", func(t *testing.T) {
		fake := &fakeInstancesClient{createErr: alreadyExists}
		rt, _ := nativeIdentityRuntime(t, fake)
		cfg := nativeIdentityRunConfig(nil)
		present := computedCloudRunInstance(t, rt, cfg)
		ready := fakeReadyInstance(present, present.GetName(), 0)
		// Run's read finds nothing; the adoption read finds this run's
		// matching instance, ready.
		fake.getSeq = []fakeGetResult{{err: notFoundErr()}, {inst: ready}}
		fake.getInstance = ready

		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(fake.createReqs) != 1 || len(fake.startReqs) != 0 || len(fake.deleteReqs) != 0 {
			t.Errorf("create %d, start %d, delete %d; want 1, 0, 0", len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
		}
		if len(fake.getReqs) < 2 {
			t.Errorf("GetInstance called %d times, want the adoption read", len(fake.getReqs))
		}
	})
}

// crInitFirewallCases mirror sciontool init's own parse of
// SCION_INIT_FIREWALL (initFirewallRequested; its cases are
// TestSetupInitFirewall in cmd/sciontool/commands/init_netadmin_test.go),
// with more spellings strconv.ParseBool accepts or rejects and surrounding
// space. wantOn: init reads it as on. wantErr: init fails the start.
var crInitFirewallCases = []struct {
	value   string
	wantOn  bool
	wantErr bool
}{
	// init's own cases.
	{value: ""},
	{value: "0"},
	{value: "false"},
	{value: "1", wantOn: true},
	{value: "true", wantOn: true},
	{value: "yes please", wantErr: true},
	// Trimmed, then strconv.ParseBool.
	{value: "  "},
	{value: " 0 "},
	{value: "F"},
	{value: "TRUE", wantOn: true},
	{value: "t", wantOn: true},
	{value: " true ", wantOn: true},
	{value: "\ttrue\n", wantOn: true},
	{value: "garbage", wantErr: true},
	{value: "yes", wantErr: true},
	{value: "on", wantErr: true},
}

// CloudRunInitFirewallRequested reads every value as sciontool init does,
// and CloudRunInitFirewallRefused gives the refusal for an on value, the
// invalid-value refusal for an unparsable one and nothing for an off
// value, never carrying the value.
func TestCloudRunInitFirewallRequested(t *testing.T) {
	for _, tc := range crInitFirewallCases {
		t.Run(strconv.Quote(tc.value), func(t *testing.T) {
			on, err := CloudRunInitFirewallRequested(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CloudRunInitFirewallRequested(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
			if on != tc.wantOn {
				t.Errorf("CloudRunInitFirewallRequested(%q) = %v, want %v", tc.value, on, tc.wantOn)
			}
			refusal := CloudRunInitFirewallRefused(tc.value)
			switch {
			case tc.wantErr:
				if refusal != "the "+CloudRunInitFirewallEnv+" value is invalid; "+CloudRunInitFirewallRefusal {
					t.Errorf("CloudRunInitFirewallRefused(%q) = %q, want the invalid-value refusal", tc.value, refusal)
				}
			case tc.wantOn:
				if refusal != CloudRunInitFirewallRefusal {
					t.Errorf("CloudRunInitFirewallRefused(%q) = %q, want the refusal", tc.value, refusal)
				}
			default:
				if refusal != "" {
					t.Errorf("CloudRunInitFirewallRefused(%q) = %q, want none", tc.value, refusal)
				}
			}
			if v := strings.TrimSpace(tc.value); len(v) > 3 && strings.Contains(refusal, v) {
				t.Errorf("CloudRunInitFirewallRefused(%q) carries the value", tc.value)
			}
		})
	}
}

// On a native "assign" instance, Run refuses SCION_INIT_FIREWALL in the
// final instance env (here from cfg.Env, which carries every layer the
// agent manager assembles) when init would read it as on or cannot parse
// it, before any Instances API call, so no instance is created. An off
// value is created as before, with the setting unchanged.
func TestCloudRunRun_AssignInitFirewallRefused(t *testing.T) {
	for _, tc := range crInitFirewallCases {
		t.Run(strconv.Quote(tc.value), func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, _ := nativeIdentityRuntime(t, fake)
			provisioned := crCountProvisionNFS(rt)
			cfg := nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"})
			cfg.Env = []string{CloudRunInitFirewallEnv + "=" + tc.value}

			_, err := rt.Run(context.Background(), cfg)
			if !tc.wantOn && !tc.wantErr {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if *provisioned != 1 {
					t.Errorf("provisioned %d times, want 1", *provisioned)
				}
				if got := crInstanceEnv(t, fake)[CloudRunInitFirewallEnv]; got != tc.value {
					t.Errorf("%s = %q, want %q unchanged", CloudRunInitFirewallEnv, got, tc.value)
				}
				return
			}
			if err == nil {
				t.Fatal("Run succeeded, want the init firewall refusal")
			}
			want := fmt.Sprintf("agent %q: %s", "agent-1", CloudRunInitFirewallRefused(tc.value))
			if err.Error() != want {
				t.Errorf("Run error = %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), nativeIDAssignedSA) {
				t.Errorf("Run error %q names the account", err.Error())
			}
			if *provisioned != 0 {
				t.Errorf("provisioned %d times before the refusal, want none", *provisioned)
			}
			if len(fake.getReqs)+len(fake.createReqs)+len(fake.startReqs)+len(fake.deleteReqs) != 0 {
				t.Errorf("Instances API called (get %d, create %d, start %d, delete %d), want none",
					len(fake.getReqs), len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
			}
		})
	}
}

// Negative controls: native "block" and an instance without a native
// identity run unchanged with SCION_INIT_FIREWALL on or invalid in cfg.Env,
// and the setting reaches the created instance unchanged.
func TestCloudRunRun_InitFirewallNotRefusedOffAssign(t *testing.T) {
	for _, id := range []*api.CloudRunIdentity{
		{ServiceAccount: nativeIDBlockSA, Mode: "block"},
		nil,
	} {
		for _, value := range []string{"true", "garbage"} {
			label := "identity-unset"
			if id != nil {
				label = id.Mode
			}
			t.Run(label+"/"+value, func(t *testing.T) {
				fake := &fakeInstancesClient{getErr: notFoundErr()}
				rt, _ := nativeIdentityRuntime(t, fake)
				cfg := nativeIdentityRunConfig(id)
				cfg.Env = []string{CloudRunInitFirewallEnv + "=" + value}

				if _, err := rt.Run(context.Background(), cfg); err != nil {
					t.Fatalf("Run: %v", err)
				}
				if got := crInstanceEnv(t, fake)[CloudRunInitFirewallEnv]; got != value {
					t.Errorf("%s = %q, want %q unchanged", CloudRunInitFirewallEnv, got, value)
				}
			})
		}
	}
}

// crCountProvisionNFS wraps rt's NFS provisioning stub and returns the
// number of times Run has called it.
func crCountProvisionNFS(rt *CloudRunRuntime) *int {
	calls := new(int)
	provision := rt.provisionNFS
	rt.provisionNFS = func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
		*calls++
		return provision(ctx, cfg, agentID, uid, gid)
	}
	return calls
}

// On a native "assign" start with SCION_INIT_FIREWALL on, Run refuses
// before anything is provisioned: with the real NFS provisioning
// (rollbackTestRun), the project's shared workspace provisioning
// (cloudRunProvisionShared) is never invoked, no agent directory or
// workspace is created on the share and the Instances API is not called.
// The control: with the setting off, the same start provisions as before.
func TestCloudRunRun_AssignInitFirewallRefusedBeforeProvisioning(t *testing.T) {
	for name, clone := range map[string]bool{"clone-per-agent": true, "shared-plain": false} {
		for _, value := range []string{"true", "garbage", "false"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				fake := &fakeInstancesClient{getErr: notFoundErr()}
				rt, cfg, agentDir := rollbackTestRun(t, fake, clone)
				shared := 0
				stubCloudRunNFSMount(t, func(ProvisionInput) error {
					shared++
					return nil
				})
				cfg.CloudRunIdentity = &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}
				cfg.Env = append(append([]string(nil), cfg.Env...), CloudRunInitFirewallEnv+"="+value)

				_, err := rt.Run(context.Background(), cfg)
				if value == "false" {
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					wantShared := 1
					if clone {
						wantShared = 0
					}
					if shared != wantShared {
						t.Errorf("shared workspace provisioned %d times, want %d", shared, wantShared)
					}
					if _, statErr := os.Stat(agentDir); statErr != nil {
						t.Errorf("agent directory: %v, want it provisioned", statErr)
					}
					return
				}
				if err == nil {
					t.Fatal("Run succeeded, want the init firewall refusal")
				}
				want := fmt.Sprintf("agent %q: %s", crTestAgentID, CloudRunInitFirewallRefused(value))
				if err.Error() != want {
					t.Errorf("Run error = %q, want %q", err.Error(), want)
				}
				if shared != 0 {
					t.Errorf("shared workspace provisioned %d times before the refusal, want none", shared)
				}
				for _, dir := range []string{agentDir, cfg.Workspace} {
					if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
						t.Errorf("%s exists after the refusal (stat error %v), want nothing created", dir, statErr)
					}
				}
				if len(fake.getReqs)+len(fake.createReqs)+len(fake.startReqs)+len(fake.deleteReqs) != 0 {
					t.Errorf("Instances API called (get %d, create %d, start %d, delete %d), want none",
						len(fake.getReqs), len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
				}
			})
		}
	}
}
