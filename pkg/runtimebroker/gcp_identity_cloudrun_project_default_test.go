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
	"net/http/httptest"
	"strings"
	"testing"
)

// A project-default passthrough the hub granted under its confinement
// carries the RequireLocalRuntime flag, the same as a hub-default grant. On
// Cloud Run Instances (hub.cloudrun_native_identity on) the broker runs such
// a grant as block, and refuses a passthrough without the flag. On a
// "cloudrun-instances" runtime entry the flagged grant becomes block and is
// refused as block there, never as passthrough.

const pdcrInstancesGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun-instances
        cloudrun_instances:
            project_id: example-project
            region: us-central1
`

// pdcrStartEnv is a start or restart's resolved env for a project-default
// passthrough from the hub, with the local-runtime flag when flag is set.
func pdcrStartEnv(flag bool) map[string]string {
	env := map[string]string{"SCION_METADATA_MODE": "passthrough", "SCION_METADATA_MODE_SOURCE": "hub"}
	if flag {
		env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"] = "true"
	}
	return env
}

func pdcrInputs(t *testing.T, name, projectDir string, gcp *GCPIdentityConfig, resolvedEnv map[string]string) startContextInputs {
	t.Helper()
	in := startContextInputs{
		Name:        name,
		ProjectPath: projectDir,
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	}
	if resolvedEnv != nil {
		in.ResolvedEnv = resolvedEnv
		in.Operation = opHTTPStart
	} else {
		in.Config = &CreateAgentConfig{GCPIdentity: gcp}
	}
	return in
}

// "cloudrun" entry: the confined grant runs as the block account; an
// unconfined project-default passthrough is refused.
func TestBuildStartContext_CloudRunProjectDefaultPassthroughConfinedIsBlock(t *testing.T) {
	confined := &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true}
	for _, tc := range []struct {
		name        string
		gcp         *GCPIdentityConfig
		resolvedEnv map[string]string
	}{
		{name: "confined grant on create", gcp: confined},
		{name: "confined grant on start", resolvedEnv: pdcrStartEnv(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunBlockGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), pdcrInputs(t, "agent-pdcr-block", projectDir, tc.gcp, tc.resolvedEnv))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			id := sc.Opts.CloudRunIdentity
			if id == nil || id.Mode != "block" || id.ServiceAccount != crBlockSA {
				t.Fatalf("CloudRunIdentity = %+v, want {%s block}", id, crBlockSA)
			}
			if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "block" {
				t.Errorf("SCION_METADATA_MODE = %q, want block", got)
			}
		})
	}
}

func TestBuildStartContext_CloudRunProjectDefaultPassthroughUnconfinedRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gcp         *GCPIdentityConfig
		resolvedEnv map[string]string
	}{
		{name: "unconfined on create", gcp: &GCPIdentityConfig{MetadataMode: "passthrough"}},
		{name: "unconfined on start", resolvedEnv: pdcrStartEnv(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunBlockGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), pdcrInputs(t, "agent-pdcr-pt", projectDir, tc.gcp, tc.resolvedEnv))
			if err == nil {
				t.Fatalf("expected the passthrough refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if !strings.Contains(sce.Message, "agent-pdcr-pt") || !strings.Contains(sce.Message, cloudRunPassthroughRefusal) {
				t.Errorf("message %q must be the passthrough refusal naming the agent", sce.Message)
			}
		})
	}
}

// "cloudrun-instances" entry: the confined grant becomes block and is
// refused as block on that entry type, never as passthrough; an unconfined
// passthrough gets the passthrough refusal. On start the flag is read from
// the resolved env.
func TestBuildStartContext_CloudRunInstancesProjectDefaultPassthrough(t *testing.T) {
	const instancesText = "of type cloudrun-instances does not support assigned identities"
	for _, tc := range []struct {
		name        string
		gcp         *GCPIdentityConfig
		resolvedEnv map[string]string
		wantText    string
		notText     string
	}{
		{name: "confined grant is block, refused on the entry type",
			gcp:      &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
			wantText: instancesText,
			notText:  cloudRunPassthroughRefusal},
		{name: "unconfined passthrough is refused as passthrough",
			gcp:      &GCPIdentityConfig{MetadataMode: "passthrough"},
			wantText: cloudRunPassthroughRefusal},
		{name: "confined grant on start is block, refused on the entry type",
			resolvedEnv: pdcrStartEnv(true),
			wantText:    instancesText,
			notText:     cloudRunPassthroughRefusal},
		{name: "unconfined passthrough on start is refused as passthrough",
			resolvedEnv: pdcrStartEnv(false),
			wantText:    cloudRunPassthroughRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, pdcrInstancesGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), pdcrInputs(t, "agent-pdcr-i1", projectDir, tc.gcp, tc.resolvedEnv))
			if sc != nil {
				t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if !strings.Contains(sce.Message, "agent-pdcr-i1") || !strings.Contains(sce.Message, tc.wantText) {
				t.Errorf("message %q must name the agent and contain %q", sce.Message, tc.wantText)
			}
			if tc.notText != "" && strings.Contains(sce.Message, tc.notText) {
				t.Errorf("message %q must not be %q: the confined grant is decided as block", sce.Message, tc.notText)
			}
		})
	}
}

// Experiment off on the "cloudrun" entry: the resolver steps aside, and the
// broker's own re-check turns the confined grant into block
// (downgradeUnverifiedHubDefaultPassthrough), on create and on start. An
// unconfined passthrough is not touched by that re-check (the controls).
func TestBuildStartContext_CloudRunProjectDefaultPassthroughExperimentOffDowngradesToBlock(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gcp         *GCPIdentityConfig
		resolvedEnv map[string]string
		wantMode    string
		wantHost    string
	}{
		{name: "confined grant on create", gcp: &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
			wantMode: "block", wantHost: "localhost:18380"},
		{name: "confined grant on start", resolvedEnv: pdcrStartEnv(true),
			wantMode: "block", wantHost: "localhost:18380"},
		{name: "unconfined on create (control)", gcp: &GCPIdentityConfig{MetadataMode: "passthrough"},
			wantMode: "passthrough"},
		{name: "unconfined on start (control)", resolvedEnv: pdcrStartEnv(false),
			wantMode: "passthrough"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
			sc, err := srv.buildStartContext(context.Background(), pdcrInputs(t, "agent-pdcr-off", projectDir, tc.gcp, tc.resolvedEnv))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if sc.Opts.CloudRunIdentity != nil {
				t.Errorf("CloudRunIdentity = %+v, want nil with the experiment off", sc.Opts.CloudRunIdentity)
			}
			if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != tc.wantMode {
				t.Errorf("SCION_METADATA_MODE = %q, want %q", got, tc.wantMode)
			}
			if tc.wantHost != "" {
				if got := sc.Opts.Env["GCE_METADATA_HOST"]; got != tc.wantHost {
					t.Errorf("GCE_METADATA_HOST = %q, want %q", got, tc.wantHost)
				}
			}
		})
	}
}
