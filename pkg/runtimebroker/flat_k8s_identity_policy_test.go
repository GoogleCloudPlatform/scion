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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A flat Kubernetes instance's GCP identity policy is its own
// (ptone/scion#3274 amendment): the block ServiceAccount and the assign
// mapping come from its runtime_target snapshot, never from profiles,
// runtime entries, the Hub overlay or project settings.

const flatTestGSA = "agent@proj.iam.gserviceaccount.com"

// globalPolicyThatMustNotApply is operator global settings with a block
// ServiceAccount and an assign mapping on the "kubernetes" runtime entry,
// which a legacy server would use. A flat instance never reads them.
const globalPolicyThatMustNotApply = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_block_service_account: global-block
        kubernetes_service_account_mappings:
            agent@proj.iam.gserviceaccount.com: global-ksa
`

// newFlatKubernetesStartServer is a flat Kubernetes instance (key k8s-a,
// namespace agents) whose runtime_target carries target.
func newFlatKubernetesStartServer(t *testing.T, target config.V1RuntimeTargetConfig) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	target.Type = "kubernetes"
	if target.Namespace == "" {
		target.Namespace = "agents"
	}
	srv.config.FlatInstance = &FlatInstanceConfig{
		Identity: &brokeridentity.Identity{
			InstanceKey:     "k8s-a",
			RuntimeBrokerID: "flat-k8s-broker",
			RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "flat-k8s-target", Type: "kubernetes"},
			ExecutionScope: brokeridentity.ExecutionScope{Type: "kubernetes",
				Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-1", Namespace: target.Namespace}},
		},
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "k8s-a", Name: "k8s-a", RuntimeTarget: &target},
	}
	srv.flatK8sIdentity = newFlatKubernetesIdentityPolicy(srv.config.FlatInstance)
	return srv
}

func flatStartInputs(name string, in startContextInputs) startContextInputs {
	in.Name = name
	in.HTTPRequest = httptest.NewRequest("POST", "/api/v1/agents", nil)
	return in
}

func wantFlatSelection() dispatchProfileSelection {
	return dispatchProfileSelection{InstanceKey: "k8s-a", RuntimeTargetID: "flat-k8s-target"}
}

func startContextStatus(t *testing.T, err error) int {
	t.Helper()
	var sce *startContextError
	if !errors.As(err, &sce) {
		t.Fatalf("want a start context refusal, got %v", err)
	}
	return sce.Status
}

// TestFlatKubernetesIdentity_BlockWithInstanceServiceAccount: every block
// source (request, project and Hub defaults through resolved env, and the
// saved identity on start) runs as the instance's block ServiceAccount,
// keeps SCION_METADATA_MODE=block and adds no assign identity env; the
// selection is the instance's own. Global settings never apply.
func TestFlatKubernetesIdentity_BlockWithInstanceServiceAccount(t *testing.T) {
	cases := map[string]startContextInputs{
		"request":                {Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}}, Operation: opCreate},
		"project_default_create": {Config: &CreateAgentConfig{}, ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"}, Operation: opCreate},
		"hub_default_start":      {ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"}, Operation: opHTTPStart},
		"saved_start":            {Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}}, Operation: opHTTPStart},
		"saved_restart":          {Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}}, Operation: opHTTPRestart},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{KubernetesBlockServiceAccount: "instance-block"})
			newTestGlobalSettings(t, globalPolicyThatMustNotApply)
			sc, err := srv.buildStartContext(context.Background(), flatStartInputs("flat-block-"+strings.ReplaceAll(name, "_", "-"), in))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			assertKubernetesBlockContext(t, sc, "instance-block")
			if sc.BlockSelection == nil || *sc.BlockSelection != wantFlatSelection() {
				t.Errorf("BlockSelection = %+v, want %+v", sc.BlockSelection, wantFlatSelection())
			}
		})
	}
}

// TestFlatKubernetesIdentity_BlockOmittedUsesNamespaceDefault: with no
// instance block ServiceAccount the pod runs as the namespace's default
// ServiceAccount (token not mounted and the node selector come from the
// block identity), even though global settings name one.
func TestFlatKubernetesIdentity_BlockOmittedUsesNamespaceDefault(t *testing.T) {
	srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{})
	newTestGlobalSettings(t, globalPolicyThatMustNotApply)
	sc, err := srv.buildStartContext(context.Background(), flatStartInputs("flat-block-default", startContextInputs{
		Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}}, Operation: opCreate}))
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertKubernetesBlockContext(t, sc, "")
}

// TestFlatKubernetesIdentity_BlockInvalidOrConflictingRefused: an invalid
// configured block ServiceAccount (one that bypassed the strict loader) and
// an explicit conflicting ServiceAccount are refused with 400.
func TestFlatKubernetesIdentity_BlockInvalidOrConflictingRefused(t *testing.T) {
	t.Run("invalid configured", func(t *testing.T) {
		srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{KubernetesBlockServiceAccount: "Bad_SA"})
		_, err := srv.buildStartContext(context.Background(), flatStartInputs("flat-block-bad", startContextInputs{
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}}, Operation: opCreate}))
		if got := startContextStatus(t, err); got != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", got)
		}
	})
	t.Run("explicit conflicting", func(t *testing.T) {
		srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{KubernetesBlockServiceAccount: "instance-block"})
		_, err := srv.buildStartContext(context.Background(), flatStartInputs("flat-block-conflict", startContextInputs{
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
				Kubernetes: &api.KubernetesConfig{ServiceAccountName: "other-ksa"}}, Operation: opCreate}))
		if got := startContextStatus(t, err); got != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", got)
		}
	})
}

func assignInputs(name string, extra func(*CreateAgentConfig)) startContextInputs {
	c := &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: flatTestGSA, ProjectID: "proj"}}
	if extra != nil {
		extra(c)
	}
	return flatStartInputs(name, startContextInputs{Config: c, Operation: opCreate})
}

// TestFlatKubernetesIdentity_AssignUsesInstanceMapping: assign runs as the
// KSA the instance maps the GSA to, never the global mapping; the
// selection is the instance's own.
func TestFlatKubernetesIdentity_AssignUsesInstanceMapping(t *testing.T) {
	srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{
		KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "instance-ksa"}})
	newTestGlobalSettings(t, globalPolicyThatMustNotApply)
	sc, err := srv.buildStartContext(context.Background(), assignInputs("flat-assign", nil))
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "instance-ksa" {
		t.Errorf("assign ServiceAccount = %q, want instance-ksa", got)
	}
	if sc.AssignSelection == nil || *sc.AssignSelection != wantFlatSelection() {
		t.Errorf("AssignSelection = %+v, want %+v", sc.AssignSelection, wantFlatSelection())
	}
	if sc.Opts.KubernetesBlockIdentity != nil {
		t.Errorf("no block identity for assign, got %+v", sc.Opts.KubernetesBlockIdentity)
	}
}

// TestFlatKubernetesIdentity_AssignRefusals: an unmapped GSA (although the
// global settings map it), an explicit ServiceAccount other than the
// mapped one, and an explicit namespace other than the instance's are
// refused with 400.
func TestFlatKubernetesIdentity_AssignRefusals(t *testing.T) {
	t.Run("unmapped", func(t *testing.T) {
		srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{})
		newTestGlobalSettings(t, globalPolicyThatMustNotApply)
		_, err := srv.buildStartContext(context.Background(), assignInputs("flat-assign-unmapped", nil))
		var sce *startContextError
		if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest || sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("want 400 %s, got %v", ErrCodeIdentityNotMapped, err)
		}
	})
	mapped := config.V1RuntimeTargetConfig{KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "instance-ksa"}}
	t.Run("explicit ServiceAccount", func(t *testing.T) {
		srv := newFlatKubernetesStartServer(t, mapped)
		_, err := srv.buildStartContext(context.Background(), assignInputs("flat-assign-ksa", func(c *CreateAgentConfig) {
			c.Kubernetes = &api.KubernetesConfig{ServiceAccountName: "other-ksa"}
		}))
		var sce *startContextError
		if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest || sce.Code != ErrCodeIdentityKSAMismatch {
			t.Fatalf("want 400 %s, got %v", ErrCodeIdentityKSAMismatch, err)
		}
	})
	t.Run("explicit namespace", func(t *testing.T) {
		srv := newFlatKubernetesStartServer(t, mapped)
		_, err := srv.buildStartContext(context.Background(), assignInputs("flat-assign-ns", func(c *CreateAgentConfig) {
			c.Kubernetes = &api.KubernetesConfig{Namespace: "elsewhere"}
		}))
		if got := startContextStatus(t, err); got != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", got)
		}
	})
}

// TestFlatKubernetesIdentity_InstancesKeepTheirOwnPolicy: two flat
// instances with different policies resolve independently, and the
// snapshot does not follow later changes to the instance config.
func TestFlatKubernetesIdentity_InstancesKeepTheirOwnPolicy(t *testing.T) {
	a := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{KubernetesBlockServiceAccount: "block-a",
		KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "ksa-a"}})
	b := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{KubernetesBlockServiceAccount: "block-b",
		KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "ksa-b"}})
	for srv, want := range map[*Server][2]string{a: {"block-a", "ksa-a"}, b: {"block-b", "ksa-b"}} {
		if got, _ := srv.flatK8sIdentity.blockAccount(); got != want[0] {
			t.Errorf("block account %q, want %q", got, want[0])
		}
		if got, _ := srv.flatK8sIdentity.mappedServiceAccount(flatTestGSA); got != want[1] {
			t.Errorf("mapped KSA %q, want %q", got, want[1])
		}
	}
	a.config.FlatInstance.Instance.RuntimeTarget.KubernetesServiceAccountMappings[flatTestGSA] = "changed"
	if got, _ := a.flatK8sIdentity.mappedServiceAccount(flatTestGSA); got != "ksa-a" {
		t.Errorf("the snapshot followed a later config change: %q", got)
	}
}

// TestFlatKubernetesIdentity_LateSelectionIsTheInstance: the late
// resolution on start/restart returns the same instance selection, so the
// consistency checks compare equal, and it is never a settings key.
//
// Unit evidence only: on a running flat instance the runtime is fixed and
// the selection is the instance itself, so the start/restart 409s
// (rejectKubernetesAssignRuntimeChange, rejectKubernetesBlockRuntimeChange,
// rejectRuntimeClassificationMismatch) cannot be provoked live; this test
// and the classification check below exercise them in process on the
// production code.
func TestFlatKubernetesIdentity_LateSelectionIsTheInstance(t *testing.T) {
	srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{})
	newTestGlobalSettings(t, globalPolicyThatMustNotApply)
	got := srv.resolveDispatchProfileSelection(api.StartOptions{Profile: "local"})
	if got != wantFlatSelection() || got.ProfileName != "" || got.RuntimeEntryName != "" {
		t.Fatalf("late selection = %+v, want %+v", got, wantFlatSelection())
	}
	if sce := rejectRuntimeClassificationMismatch("kubernetes", "docker"); sce == nil {
		t.Error("a runtime classification mismatch must be refused")
	}
}
