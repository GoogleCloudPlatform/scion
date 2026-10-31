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
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
)

func TestCloudRunContainerResources(t *testing.T) {
	limits := func(cpu, mem string) api.ResourceList { return api.ResourceList{CPU: cpu, Memory: mem} }
	tests := []struct {
		name    string
		spec    *api.ResourceSpec
		want    map[string]string // nil: no ResourceRequirements
		wantErr []string          // substrings; empty: no error
	}{
		{name: "nil spec keeps platform defaults", spec: nil},
		{name: "empty spec keeps platform defaults", spec: &api.ResourceSpec{}},
		{name: "disk only keeps platform defaults", spec: &api.ResourceSpec{Disk: "20Gi"}},
		{
			name: "built-in default cpu limit",
			spec: config.BuiltinDefaultResources(),
			want: map[string]string{"cpu": "2"},
		},
		{
			name: "limits map directly",
			spec: &api.ResourceSpec{Limits: limits("4", "8Gi")},
			want: map[string]string{"cpu": "4", "memory": "8192Mi"},
		},
		{
			name: "requests used when limits unset",
			spec: &api.ResourceSpec{Requests: limits("2", "4Gi")},
			want: map[string]string{"cpu": "2", "memory": "4096Mi"},
		},
		{
			name: "limit wins over request",
			spec: &api.ResourceSpec{Requests: limits("1", "1Gi"), Limits: limits("2", "2Gi")},
			want: map[string]string{"cpu": "2", "memory": "2048Mi"},
		},
		{
			name: "millicore cpu and decimal G memory",
			spec: &api.ResourceSpec{Limits: limits("2000m", "1G")},
			want: map[string]string{"cpu": "2", "memory": "1000M"},
		},
		{
			name:    "memory finer than a megabyte",
			spec:    &api.ResourceSpec{Limits: limits("1", "1000000001")},
			wantErr: []string{"resources.limits.memory", `"1000000001"`, "whole number of Mi or M"},
		},
		{
			name: "largest band in Mi",
			spec: &api.ResourceSpec{Limits: limits("8000m", "32768Mi")},
			want: map[string]string{"cpu": "8", "memory": "32768Mi"},
		},
		{
			name: "memory only checked against default 1 vCPU",
			spec: &api.ResourceSpec{Limits: limits("", "4Gi")},
			want: map[string]string{"memory": "4096Mi"},
		},
		{
			name:    "memory only above the 1 vCPU band",
			spec:    &api.ResourceSpec{Limits: limits("", "6Gi")},
			wantErr: []string{"1 vCPU (platform default)", "512Mi to 4096Mi", "6144Mi"},
		},
		{
			name:    "cpu needing more than the default memory",
			spec:    &api.ResourceSpec{Limits: limits("4", "")},
			wantErr: []string{"4 vCPU (from resources.limits.cpu)", "2048Mi to 16384Mi", "512Mi (platform default)"},
		},
		{
			name:    "memory above the cpu band",
			spec:    &api.ResourceSpec{Limits: limits("2", "16Gi")},
			wantErr: []string{"2 vCPU", "512Mi to 8192Mi", "16384Mi"},
		},
		{
			name:    "memory below the cpu band",
			spec:    &api.ResourceSpec{Limits: limits("8", "2Gi")},
			wantErr: []string{"8 vCPU", "4096Mi to 32768Mi", "2048Mi"},
		},
		{
			name:    "fractional cpu",
			spec:    &api.ResourceSpec{Limits: limits("500m", "")},
			wantErr: []string{"resources.limits.cpu", `"500m"`, "1, 2, 4, 6 or 8 vCPU"},
		},
		{
			name:    "unsupported whole cpu",
			spec:    &api.ResourceSpec{Limits: limits("3", "")},
			wantErr: []string{"1, 2, 4, 6 or 8 vCPU"},
		},
		{
			name:    "cpu above maximum",
			spec:    &api.ResourceSpec{Limits: limits("16", "")},
			wantErr: []string{"1, 2, 4, 6 or 8 vCPU"},
		},
		{
			name:    "cpu from request named in error",
			spec:    &api.ResourceSpec{Requests: limits("3", "")},
			wantErr: []string{"resources.requests.cpu"},
		},
		{
			name:    "memory above maximum",
			spec:    &api.ResourceSpec{Limits: limits("8", "64Gi")},
			wantErr: []string{"maximum of 32768Mi"},
		},
		{
			name:    "memory below minimum",
			spec:    &api.ResourceSpec{Limits: limits("1", "256Mi")},
			wantErr: []string{"minimum of 512Mi"},
		},
		{
			name:    "unparseable cpu",
			spec:    &api.ResourceSpec{Limits: limits("lots", "")},
			wantErr: []string{"cloudrun:", "invalid resources.limits.cpu"},
		},
		{
			name:    "unparseable memory",
			spec:    &api.ResourceSpec{Limits: limits("1", "big")},
			wantErr: []string{"cloudrun:", "invalid resources.limits.memory"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cloudRunContainerResources(tc.spec)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("cloudRunContainerResources() = %v, want error containing %q", got, tc.wantErr)
				}
				for _, s := range tc.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q does not contain %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("cloudRunContainerResources(): %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("cloudRunContainerResources() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("cloudRunContainerResources() = nil, want limits %v", tc.want)
			}
			if !reflect.DeepEqual(got.GetLimits(), tc.want) {
				t.Errorf("limits = %v, want %v", got.GetLimits(), tc.want)
			}
			if got.GetCpuIdle() {
				t.Error("CpuIdle = true, want CPU allocated for the instance lifetime")
			}
		})
	}
}

// TestCloudRunContainerResources_EveryBandEdgeAccepted pins the table: each
// supported vCPU count accepts the lowest and highest memory of its band.
func TestCloudRunContainerResources_EveryBandEdgeAccepted(t *testing.T) {
	if len(cloudRunCPUMemoryBands) != 5 {
		t.Fatalf("got %d vCPU bands, want 5", len(cloudRunCPUMemoryBands))
	}
	for _, b := range cloudRunCPUMemoryBands {
		for _, mib := range []int64{b.minMiB, b.maxMiB} {
			spec := &api.ResourceSpec{Limits: api.ResourceList{
				CPU:    strconv.FormatInt(b.cpu, 10),
				Memory: strconv.FormatInt(mib, 10) + "Mi",
			}}
			if _, err := cloudRunContainerResources(spec); err != nil {
				t.Errorf("%d vCPU with %dMi: %v", b.cpu, mib, err)
			}
		}
	}
}

func TestCloudRunRun_AppliesResourcesToInstance(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()
	cfg.Resources = &api.ResourceSpec{Limits: api.ResourceList{CPU: "4", Memory: "8Gi"}, Disk: "20Gi"}

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	got := fake.createReqs[0].GetInstance().GetContainers()[0].GetResources()
	want := map[string]string{"cpu": "4", "memory": "8192Mi"}
	if !reflect.DeepEqual(got.GetLimits(), want) {
		t.Errorf("container resource limits = %v, want %v", got.GetLimits(), want)
	}
}

func TestCloudRunRun_NoResourcesKeepsPlatformDefaults(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)

	if _, err := rt.Run(context.Background(), runConfigForTest()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	if res := fake.createReqs[0].GetInstance().GetContainers()[0].GetResources(); res != nil {
		t.Errorf("container resources = %v, want nil (platform defaults)", res)
	}
}

// TestCloudRunRun_InvalidResourcesFailBeforeAnyCall checks that resources
// outside the Cloud Run limits fail the run before the NFS share is
// provisioned or a client is created.
func TestCloudRunRun_InvalidResourcesFailBeforeAnyCall(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	clientCalls, provisionCalls := 0, 0
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) {
		clientCalls++
		return fake, nil
	}
	rt.provisionNFS = func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
		provisionCalls++
		return stubCloudRunNFSProvision(ctx, cfg, agentID, uid, gid)
	}
	cfg := runConfigForTest()
	cfg.Resources = &api.ResourceSpec{Limits: api.ResourceList{CPU: "2", Memory: "16Gi"}}

	_, err := rt.Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "2 vCPU") {
		t.Fatalf("Run error = %v, want the Cloud Run vCPU/memory error", err)
	}
	if provisionCalls != 0 || clientCalls != 0 {
		t.Errorf("provision calls = %d, client calls = %d; want 0 and 0", provisionCalls, clientCalls)
	}
	if n := len(fake.getReqs) + len(fake.createReqs); n != 0 {
		t.Errorf("Instances API called %d times, want 0", n)
	}
}
