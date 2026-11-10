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
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
)

const (
	flatTestProjectID     = "test-project"
	flatTestProjectNumber = "123456789012"
	flatTestRegion        = "us-central1"
)

// staticTokenProvider is an auth.TokenProvider that is never asked for a
// token in these tests.
type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (*auth.Token, error) {
	return nil, errors.New("no token in this test")
}

func flatTestCredentials() *auth.Credentials {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticTokenProvider{}})
}

// newFlatCloudRunRuntimeWithFakes is a flat CloudRunRuntime with fake
// Instances and Resource Manager clients and conn as its exec connector. It
// lives in this test file only: production code has no exported path that
// builds a flat runtime without its own credentials.
func newFlatCloudRunRuntimeWithFakes(cfg config.CloudRunConfig, newClient func(ctx context.Context) (cloudrun.InstancesAPI, error),
	getProject func(ctx context.Context, name string) (*resourcemanagerpb.Project, error), conn cloudrun.ExecConnector) (*CloudRunRuntime, error) {
	rt, err := newFlatCloudRunRuntime(cfg, nil)
	if err != nil {
		return nil, err
	}
	rt.newClient, rt.getProject, rt.exec = newClient, getProject, conn
	return rt, nil
}

// newFlatTestRuntime is a flat CloudRunRuntime over fake, with conn as its
// exec connector.
func newFlatTestRuntime(t *testing.T, fake *fakeInstancesClient, conn cloudrun.ExecConnector) *CloudRunRuntime {
	t.Helper()
	rt, err := newFlatCloudRunRuntimeWithFakes(config.CloudRunConfig{ProjectID: flatTestProjectID, Location: flatTestRegion},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil }, nil, conn)
	if err != nil {
		t.Fatalf("newFlatCloudRunRuntimeWithFakes: %v", err)
	}
	return rt
}

// TestFlatCloudRunRuntime_NoMetadataFallback: the flat constructor refuses
// an empty project or region, and the built runtime never resolves its
// configuration from the metadata server (it is resolved at construction).
func TestFlatCloudRunRuntime_NoMetadataFallback(t *testing.T) {
	for _, cfg := range []config.CloudRunConfig{
		{ProjectID: flatTestProjectID},
		{Location: flatTestRegion},
		{},
	} {
		if _, err := NewFlatCloudRunRuntime(cfg, flatTestCredentials()); err == nil {
			t.Errorf("NewFlatCloudRunRuntime(%+v) = nil error, want a refusal", cfg)
		}
	}
	if _, err := NewFlatCloudRunRuntime(config.CloudRunConfig{ProjectID: flatTestProjectID, Location: flatTestRegion}, nil); err == nil {
		t.Error("NewFlatCloudRunRuntime without credentials = nil error, want a refusal")
	}
	rt, err := NewFlatCloudRunRuntime(config.CloudRunConfig{ProjectID: flatTestProjectID, Location: flatTestRegion}, flatTestCredentials())
	if err != nil {
		t.Fatalf("NewFlatCloudRunRuntime: %v", err)
	}
	if !rt.resolved {
		t.Fatal("a flat runtime must be resolved at construction, so resolveConfig never reads metadata")
	}
	// resolveConfig returns at once: a metadata read would fail (or hang)
	// off GCE, and would change the configured values.
	if err := rt.resolveConfig(context.Background()); err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if rt.config.ProjectID != flatTestProjectID || rt.config.Location != flatTestRegion {
		t.Fatalf("config = %s/%s, want the configured values", rt.config.ProjectID, rt.config.Location)
	}
}

// TestFlatCloudRunRuntime_UsesShareNFSOnly: a flat runtime takes no NFS
// server or export of its own, so the NFS target is always the host's share.
func TestFlatCloudRunRuntime_UsesShareNFSOnly(t *testing.T) {
	for _, cfg := range []config.CloudRunConfig{
		{ProjectID: flatTestProjectID, Location: flatTestRegion, NFSServer: "10.0.0.9"},
		{ProjectID: flatTestProjectID, Location: flatTestRegion, NFSExport: "/other"},
	} {
		if _, err := NewFlatCloudRunRuntime(cfg, flatTestCredentials()); err == nil {
			t.Errorf("NewFlatCloudRunRuntime(%+v) = nil error, want a refusal", cfg)
		}
	}
	server, export, err := resolveCloudRunNFSTarget("", "", "10.0.0.2", "/share")
	if err != nil || server != "10.0.0.2" || export != "/share" {
		t.Fatalf("resolveCloudRunNFSTarget with no runtime override = %q %q %v, want the share", server, export, err)
	}
}

// TestFlatCloudRunCredentials_PerInstanceIsolation (runtime half): two flat
// runtimes built from two credentials carry exactly their own credential in
// their client options.
func TestFlatCloudRunCredentials_PerInstanceIsolation(t *testing.T) {
	c1, c2 := flatTestCredentials(), flatTestCredentials()
	cfg := config.CloudRunConfig{ProjectID: flatTestProjectID, Location: flatTestRegion}
	rt1, err := NewFlatCloudRunRuntime(cfg, c1)
	if err != nil {
		t.Fatal(err)
	}
	rt2, err := NewFlatCloudRunRuntime(cfg, c2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt1.clientOpts, []option.ClientOption{option.WithAuthCredentials(c1)}) {
		t.Error("the first runtime's clients do not use exactly its own credentials")
	}
	if !reflect.DeepEqual(rt2.clientOpts, []option.ClientOption{option.WithAuthCredentials(c2)}) {
		t.Error("the second runtime's clients do not use exactly its own credentials")
	}
	if _, ok := rt1.exec.(*cloudrun.IAPExecConnector); !ok || rt1.exec == rt2.exec {
		t.Error("each flat runtime needs its own credentialed exec connector")
	}
}

// TestFlatCloudRunRuntime_BindProjectNumber: after BindProjectNumber the
// Cloud Run Admin parent and resource names use the probed number, while
// the configured project ID keeps its value; a second, different number is
// refused and the same one is accepted.
func TestFlatCloudRunRuntime_BindProjectNumber(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newFlatTestRuntime(t, fake, &forgettingExecConnector{})
	if got, want := rt.parent(), "projects/"+flatTestProjectID+"/locations/"+flatTestRegion; got != want {
		t.Fatalf("parent before binding = %q, want %q", got, want)
	}
	if err := rt.BindProjectNumber(""); err == nil {
		t.Fatal("binding an empty number must fail")
	}
	if err := rt.BindProjectNumber(flatTestProjectNumber); err != nil {
		t.Fatalf("BindProjectNumber: %v", err)
	}
	if err := rt.BindProjectNumber(flatTestProjectNumber); err != nil {
		t.Fatalf("binding the same number again: %v", err)
	}
	if err := rt.BindProjectNumber("999999999999"); !errors.Is(err, ErrProjectNumberBound) {
		t.Fatalf("binding another number = %v, want ErrProjectNumberBound", err)
	}
	if got, want := rt.parent(), "projects/"+flatTestProjectNumber+"/locations/"+flatTestRegion; got != want {
		t.Fatalf("parent after binding = %q, want %q", got, want)
	}
	if got, want := rt.instanceResourceName("agent-1"), "projects/"+flatTestProjectNumber+"/locations/"+flatTestRegion+"/instances/agent-1"; got != want {
		t.Fatalf("instance name = %q, want %q", got, want)
	}
	if rt.ProjectID() != flatTestProjectID || rt.config.ProjectID != flatTestProjectID {
		t.Fatalf("configured project ID changed to %q; the number lives only where parent() reads it", rt.config.ProjectID)
	}
}

// TestFlatCloudRunRuntime_ExecKeyForgottenUnderProjectID: after
// BindProjectNumber, Stop, Delete and the launch cleanup's DeleteResource
// each drop the exec connector's cached key under the configured project
// ID, the key Exec stored it under, never the bound number.
func TestFlatCloudRunRuntime_ExecKeyForgottenUnderProjectID(t *testing.T) {
	const (
		id    = "agent-foo-0123456789"
		runID = "0f1e2d3c-4b5a-4968-8776-655443322110"
	)
	want := flatTestProjectID + "/" + flatTestRegion + "/" + id
	runInstance := func() *runpb.Instance {
		return &runpb.Instance{Name: "projects/" + flatTestProjectNumber + "/locations/" + flatTestRegion + "/instances/" + id,
			Labels: map[string]string{sanitizeGCPLabelKey(api.LabelRunID): runID}}
	}
	tests := []struct {
		name     string
		fake     *fakeInstancesClient
		call     func(*CloudRunRuntime) error
		reqNames func(*fakeInstancesClient) []string
	}{
		{"Stop", &fakeInstancesClient{}, func(rt *CloudRunRuntime) error { return rt.Stop(context.Background(), RunRef{ID: id}) },
			func(f *fakeInstancesClient) []string {
				var n []string
				for _, r := range f.stopReqs {
					n = append(n, r.GetName())
				}
				return n
			}},
		{"Delete", &fakeInstancesClient{}, func(rt *CloudRunRuntime) error { return rt.Delete(context.Background(), RunRef{ID: id}) },
			func(f *fakeInstancesClient) []string {
				var n []string
				for _, r := range f.deleteReqs {
					n = append(n, r.GetName())
				}
				return n
			}},
		{"DeleteResource", &fakeInstancesClient{getSeq: []fakeGetResult{{inst: runInstance()}}, getErr: notFoundErr()},
			func(rt *CloudRunRuntime) error {
				return rt.DeleteResource(context.Background(), api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance,
					Namespace: "projects/" + flatTestProjectNumber + "/locations/" + flatTestRegion, Name: id, UID: runID})
			},
			func(f *fakeInstancesClient) []string {
				var n []string
				for _, r := range f.deleteReqs {
					n = append(n, r.GetName())
				}
				return n
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := &forgettingExecConnector{}
			rt := newFlatTestRuntime(t, tc.fake, conn)
			if err := rt.BindProjectNumber(flatTestProjectNumber); err != nil {
				t.Fatal(err)
			}
			if err := tc.call(rt); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(conn.forgotten) != 1 || conn.forgotten[0] != want {
				t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
			}
			wantName := "projects/" + flatTestProjectNumber + "/locations/" + flatTestRegion + "/instances/" + id
			if names := tc.reqNames(tc.fake); len(names) != 1 || names[0] != wantName {
				t.Errorf("Cloud Run Admin request names = %q, want [%q] (the bound number)", names, wantName)
			}
		})
	}
}

// TestFlatCloudRunRun_ReplacedInstanceForgetsExecKeyUnderProjectID: with a
// bound project number, each Run path that removes or waits out the
// agent's instance before an instance of the same name is created (the
// recreate after a spec difference, the wait for an instance being
// deleted, and the delete after a create that ended) drops the exec key
// under the configured project ID, also when the delete fails, while its
// Cloud Run Admin delete requests use the bound number.
func TestFlatCloudRunRun_ReplacedInstanceForgetsExecKeyUnderProjectID(t *testing.T) {
	want := flatTestProjectID + "/" + flatTestRegion + "/" + cloudRunInstanceID("agent-1")
	wantName := "projects/" + flatTestProjectNumber + "/locations/" + flatTestRegion + "/instances/" + cloudRunInstanceID("agent-1")
	unavailable := status.Error(codes.Unavailable, "delete unavailable")
	specDiff := func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
		existing := computedCloudRunInstance(t, rt, readyRunCfg())
		existing.Containers[0].Args = []string{"/bin/sh", "-c", "a launch command of an earlier start"}
		// Run's read, the delete's run-checked read, then gone.
		fake.getSeq = []fakeGetResult{{inst: existing}, {inst: existing}, {err: notFoundErr()}}
		fake.getErr = notFoundErr()
	}
	beingDeleted := func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
		existing := computedCloudRunInstance(t, rt, readyRunCfg())
		existing.DeleteTime = timestamppb.New(time.Unix(1, 0))
		fake.getSeq = []fakeGetResult{{inst: existing}}
		fake.getErr = notFoundErr()
	}
	endedAfterCreate := func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
		fake.getSeq = []fakeGetResult{{err: notFoundErr()}}
		fake.getInstance = readyShape(recordedExitedZero, readyRunID)
		fake.createOpInstance = readyShape(recordedCreating, readyRunID)
	}
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient)
		deleteErr error
		deletes   int
	}{
		{name: "spec diff recreate", setup: specDiff, deletes: 1},
		{name: "spec diff recreate, delete fails", setup: specDiff, deleteErr: unavailable, deletes: 1},
		{name: "being deleted, waited for", setup: beingDeleted, deletes: 0},
		{name: "ended after create", setup: endedAfterCreate, deletes: 1},
		{name: "ended after create, delete fails", setup: endedAfterCreate, deleteErr: unavailable, deletes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{}
			rt := newReadyTestRuntime(t, fake)
			if err := rt.BindProjectNumber(flatTestProjectNumber); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, rt, fake)
			fake.deleteErr = tc.deleteErr
			conn := &forgettingExecConnector{}
			rt.exec = conn

			_, _ = rt.Run(context.Background(), readyRunCfg())

			if len(fake.deleteReqs) != tc.deletes {
				t.Errorf("deletes = %d, want %d", len(fake.deleteReqs), tc.deletes)
			}
			for _, r := range fake.deleteReqs {
				if r.GetName() != wantName {
					t.Errorf("delete request name = %q, want %q (the bound number)", r.GetName(), wantName)
				}
			}
			if len(conn.forgotten) != 1 || conn.forgotten[0] != want {
				t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
			}
		})
	}
}

// TestFlatCloudRunRuntime_ProbeReads: the probe reads go to the configured
// project through the runtime's seams, as is.
func TestFlatCloudRunRuntime_ProbeReads(t *testing.T) {
	var asked string
	fake := &fakeInstancesClient{}
	rt, err := newFlatCloudRunRuntimeWithFakes(config.CloudRunConfig{ProjectID: flatTestProjectID, Location: flatTestRegion},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil },
		func(_ context.Context, name string) (*resourcemanagerpb.Project, error) {
			asked = name
			return &resourcemanagerpb.Project{Name: "projects/" + flatTestProjectNumber, State: resourcemanagerpb.Project_ACTIVE}, nil
		}, &forgettingExecConnector{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := rt.ProbeProject(context.Background())
	if err != nil || p.GetName() != "projects/"+flatTestProjectNumber || asked != "projects/"+flatTestProjectID {
		t.Fatalf("ProbeProject = %v, %v (asked %q)", p, err, asked)
	}
	if err := rt.ProbeListInstances(context.Background()); err != nil {
		t.Fatalf("ProbeListInstances on an empty region: %v", err)
	}
	if len(fake.listReqs) != 1 || fake.listReqs[0].GetPageSize() != 1 ||
		fake.listReqs[0].GetParent() != "projects/"+flatTestProjectID+"/locations/"+flatTestRegion {
		t.Fatalf("list requests = %v, want one page-size-1 list of the configured region", fake.listReqs)
	}
	fake.listErr = errors.New("transport down")
	if err := rt.ProbeListInstances(context.Background()); err == nil {
		t.Fatal("a list error must be returned")
	}
}

// TestFlatCloudRunCreate_HubProjectIDFormIsLabelInvariant:
// the Hub dispatches an agent's project ID as its store row ID
// (RemoteCreateAgentRequest.ProjectID = agent.ProjectID), which the Hub
// mints with api.NewUUID; the broker passes it on as SCION_PROJECT_ID, the
// scion.project_id label value. That form survives the Cloud Run label
// rewrite intact, so checkOwnerLabel accepts hub-dispatched flat creates,
// while the hosted <uuid>__<slug> form is refused before provisioning
// (TestFlatCloudRunCreate_LongProjectIDRefused).
func TestFlatCloudRunCreate_HubProjectIDFormIsLabelInvariant(t *testing.T) {
	for i := 0; i < 64; i++ {
		id := api.NewUUID()
		if got := sanitizeGCPLabelValue(id); got != id {
			t.Fatalf("hub project ID %q is rewritten to %q by the Cloud Run label rules", id, got)
		}
	}
	if long := api.NewUUID() + "__" + strings.Repeat("a", api.MaxSlugLength); sanitizeGCPLabelValue(long) == long {
		t.Fatal("control: the hosted long form must be changed by the label rules, so checkOwnerLabel refuses it")
	}
}
