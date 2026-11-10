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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Example accounts for the hub.cloudrun_native_identity broker tests.
const (
	crAssignedSA = "assigned-sa@example-project.iam.gserviceaccount.com"
	crListedSA   = "listed-sa@example-project.iam.gserviceaccount.com"
	crBlockSA    = "block-sa@example-project.iam.gserviceaccount.com"
	crProjectID  = "example-project"
)

// crBlockUnset is the fixed part of the refusal of block on Cloud Run when
// the "cloudrun" runtime entry sets no block account.
const crBlockUnset = "set runtimes.cloudrun.cloudrun.block_service_account on the broker"

// testCloudRunBlockGlobalSettingsYAML is testCloudRunAllowListGlobalSettingsYAML
// plus a block account, in mixed case with surrounding spaces to check that
// it is lower-cased and trimmed.
const testCloudRunBlockGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun
        cloudrun:
            assignable_service_accounts:
                - Assigned-SA@example-project.iam.gserviceaccount.com
                - listed-sa@example-project.iam.gserviceaccount.com
            block_service_account: " Block-SA@Example-Project.iam.gserviceaccount.com "
`

// testCloudRunProjectSettingsYAML selects the "cloudrun" runtime entry, the
// ForceRuntime-resolved entry newTestServerForStartContextRuntime uses.
const testCloudRunProjectSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun
`

// testCloudRunAllowListGlobalSettingsYAML lists the assigned account (in
// another case, to check the comparison) for the "cloudrun" entry in the
// broker's global settings.
const testCloudRunAllowListGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun
        cloudrun:
            assignable_service_accounts:
                - Assigned-SA@example-project.iam.gserviceaccount.com
                - listed-sa@example-project.iam.gserviceaccount.com
`

// crGateCtx is a dispatch context carrying the experiment, as start and
// restart attach it from the request.
func crGateCtx() context.Context {
	return api.ContextWithHubAgentDefaults(context.Background(),
		&api.HubAgentDefaults{Experiments: []string{experiments.CloudRunNativeIdentity}})
}

// crGateDefaults is the experiment as a create request's config carries it.
func crGateDefaults() *api.HubAgentDefaults {
	return &api.HubAgentDefaults{Experiments: []string{experiments.CloudRunNativeIdentity}}
}

// newCloudRunStartContextServer is a buildStartContext server whose runtime
// is Cloud Run, with the project settings and the given global settings.
func newCloudRunStartContextServer(t *testing.T, globalSettings string) (*Server, string) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, cloudRunRuntimeType)
	projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML)
	if globalSettings != "" {
		newTestGlobalSettings(t, globalSettings)
	}
	return srv, projectDir
}

// requireStartContextError returns err as a *startContextError.
func requireStartContextError(t *testing.T, err error) *startContextError {
	t.Helper()
	var sce *startContextError
	if !errors.As(err, &sce) {
		t.Fatalf("expected a *startContextError, got %T: %v", err, err)
	}
	return sce
}

// Gate off: every mode on Cloud Run builds today's env and no
// instance identity. So does a dispatch to another runtime with the gate on.
func TestBuildStartContext_CloudRunIdentityGateOff(t *testing.T) {
	tests := []struct {
		name     string
		runtime  string
		ctx      context.Context
		gcp      *GCPIdentityConfig
		wantMode string
		wantHost string
		wantSA   string
	}{
		{name: "unset", runtime: cloudRunRuntimeType, ctx: context.Background(), wantMode: "block", wantHost: "localhost:18380"},
		{name: "block", runtime: cloudRunRuntimeType, ctx: context.Background(), gcp: &GCPIdentityConfig{MetadataMode: "block"},
			wantMode: "block", wantHost: "localhost:18380"},
		{name: "passthrough", runtime: cloudRunRuntimeType, ctx: context.Background(), gcp: &GCPIdentityConfig{MetadataMode: "passthrough"},
			wantMode: "passthrough"},
		{name: "hub-default passthrough", runtime: cloudRunRuntimeType, ctx: context.Background(),
			gcp:      &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
			wantMode: "block", wantHost: "localhost:18380"},
		{name: "assign", runtime: cloudRunRuntimeType, ctx: context.Background(),
			gcp:      &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID},
			wantMode: "assign", wantHost: "localhost:18380", wantSA: crAssignedSA},
		{name: "assign on docker with the gate on", runtime: "docker", ctx: crGateCtx(),
			gcp:      &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID},
			wantMode: "assign", wantHost: "localhost:18380", wantSA: crAssignedSA},
		{name: "assign with only another experiment on", runtime: cloudRunRuntimeType,
			ctx:      api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{Experiments: []string{experiments.K8sNFSHome}}),
			gcp:      &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID},
			wantMode: "assign", wantHost: "localhost:18380", wantSA: crAssignedSA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, tc.runtime)
			newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
			in := startContextInputs{
				Name:        "agent-cr-gate-off",
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
			if tc.gcp != nil {
				in.Config = &CreateAgentConfig{GCPIdentity: tc.gcp}
			}
			sc, err := srv.buildStartContext(tc.ctx, in)
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if sc.Opts.CloudRunIdentity != nil {
				t.Errorf("CloudRunIdentity = %+v, want nil", sc.Opts.CloudRunIdentity)
			}
			if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != tc.wantMode {
				t.Errorf("SCION_METADATA_MODE = %q, want %q", got, tc.wantMode)
			}
			if got := sc.Opts.Env["GCE_METADATA_HOST"]; got != tc.wantHost {
				t.Errorf("GCE_METADATA_HOST = %q, want %q", got, tc.wantHost)
			}
			if got := sc.Opts.Env["SCION_METADATA_SA_EMAIL"]; got != tc.wantSA {
				t.Errorf("SCION_METADATA_SA_EMAIL = %q, want %q", got, tc.wantSA)
			}
		})
	}
}

// gateOffEnvModes are the GCP identity modes the gate-off env tests build.
//
// localOnlyDiffers marks a mode whose identity env depends on whether the
// runtime is local: a hub-default passthrough is kept on docker and turned
// into block on Cloud Run (downgradeUnverifiedHubDefaultPassthrough), with
// or without this change.
var gateOffEnvModes = []struct {
	name             string
	gcp              *GCPIdentityConfig
	localOnlyDiffers bool
}{
	{name: "unset"},
	{name: "block", gcp: &GCPIdentityConfig{MetadataMode: "block"}},
	{name: "passthrough", gcp: &GCPIdentityConfig{MetadataMode: "passthrough"}},
	{name: "hub-default passthrough", gcp: &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true}, localOnlyDiffers: true},
	{name: "assign", gcp: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID}},
}

// buildGateOffStartContext builds a create dispatch for gcp (nil: no GCP
// identity input) on srv with ctx and requires no Cloud Run identity.
func buildGateOffStartContext(t *testing.T, ctx context.Context, srv *Server, gcp *GCPIdentityConfig) *startContext {
	t.Helper()
	in := startContextInputs{
		Name:        "agent-cr-gate-off-env",
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	}
	if gcp != nil {
		c := *gcp
		in.Config = &CreateAgentConfig{GCPIdentity: &c}
	}
	sc, err := srv.buildStartContext(ctx, in)
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if sc.Opts.CloudRunIdentity != nil {
		t.Fatalf("CloudRunIdentity = %+v, want nil", sc.Opts.CloudRunIdentity)
	}
	return sc
}

// metadataEnv returns the GCP identity env of env: the SCION_METADATA_* and
// GCE_METADATA_* keys.
func metadataEnv(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "SCION_METADATA_") || strings.HasPrefix(k, "GCE_METADATA_") {
			out[k] = v
		}
	}
	return out
}

// Gate off, on the env, for every mode:
//   - Cloud Run vs docker (every mode but localOnlyDiffers): the gate-off
//     Cloud Run dispatch has exactly the GCP identity env (every
//     SCION_METADATA_* and GCE_METADATA_* key and value) of the same
//     dispatch on docker, which this change does not touch;
//   - Cloud Run, unaffected by other hub defaults: the whole Opts.Env with
//     only another experiment in the hub defaults equals the whole Opts.Env
//     with no hub defaults;
//   - docker, experiment off vs on: the whole Opts.Env is the same.
func TestBuildStartContext_CloudRunIdentityGateOffFullEnv(t *testing.T) {
	otherExperiment := api.ContextWithHubAgentDefaults(context.Background(),
		&api.HubAgentDefaults{Experiments: []string{experiments.K8sNFSHome}})
	newServer := func(t *testing.T, runtimeName string) *Server {
		t.Helper()
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContextRuntime(t, cfg, runtimeName)
		newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
		return srv
	}
	for _, mode := range gateOffEnvModes {
		if mode.localOnlyDiffers {
			continue
		}
		t.Run("cloudrun vs docker identity env/"+mode.name, func(t *testing.T) {
			// Each server changes the working directory, so each build runs
			// right after its own server is made.
			cr := buildGateOffStartContext(t, context.Background(), newServer(t, cloudRunRuntimeType), mode.gcp)
			docker := buildGateOffStartContext(t, context.Background(), newServer(t, "docker"), mode.gcp)
			crEnv, dockerEnv := metadataEnv(cr.Opts.Env), metadataEnv(docker.Opts.Env)
			if len(dockerEnv) == 0 {
				t.Fatal("the docker dispatch has no GCP identity env; the comparison would be vacuous")
			}
			if !maps.Equal(crEnv, dockerEnv) {
				t.Errorf("gate-off Cloud Run identity env differs from docker's:\n cloudrun: %v\n docker:   %v", crEnv, dockerEnv)
			}
		})
	}
	pairs := []struct {
		name     string
		runtime  string
		ctxA     context.Context
		ctxB     context.Context
		wantSame string
	}{
		{name: "cloudrun", runtime: cloudRunRuntimeType, ctxA: context.Background(), ctxB: otherExperiment,
			wantSame: "no hub defaults vs another experiment only"},
		{name: "docker", runtime: "docker", ctxA: context.Background(), ctxB: crGateCtx(),
			wantSame: "experiment off vs on"},
	}
	for _, pair := range pairs {
		for _, mode := range gateOffEnvModes {
			t.Run(pair.name+"/"+mode.name, func(t *testing.T) {
				srv := newServer(t, pair.runtime)
				a := buildGateOffStartContext(t, pair.ctxA, srv, mode.gcp)
				b := buildGateOffStartContext(t, pair.ctxB, srv, mode.gcp)
				if !maps.Equal(a.Opts.Env, b.Opts.Env) {
					t.Errorf("Opts.Env differs (%s):\n A: %v\n B: %v", pair.wantSame, a.Opts.Env, b.Opts.Env)
				}
				if len(a.Opts.Env) == 0 {
					t.Error("Opts.Env is empty; the comparison would be vacuous")
				}
			})
		}
	}
}

// assertCloudRunAssignEnv checks the assign result: the identity is the
// assigned account, and the env is passthrough with the account and project
// as informational env and no metadata emulator redirect.
func assertCloudRunAssignEnv(t *testing.T, sc *startContext, wantSA, wantProject string) {
	t.Helper()
	id := sc.Opts.CloudRunIdentity
	if id == nil || id.ServiceAccount != wantSA || id.Mode != "assign" {
		t.Fatalf("CloudRunIdentity = %+v, want {%s assign}", id, wantSA)
	}
	if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "passthrough" {
		t.Errorf("SCION_METADATA_MODE = %q, want passthrough", got)
	}
	if got := sc.Opts.Env["SCION_METADATA_SA_EMAIL"]; got != wantSA {
		t.Errorf("SCION_METADATA_SA_EMAIL = %q, want %q", got, wantSA)
	}
	if got := sc.Opts.Env["SCION_METADATA_PROJECT_ID"]; got != wantProject {
		t.Errorf("SCION_METADATA_PROJECT_ID = %q, want %q", got, wantProject)
	}
	for _, key := range []string{"GCE_METADATA_HOST", "GCE_METADATA_ROOT", "SCION_METADATA_PORT"} {
		if v, ok := sc.Opts.Env[key]; ok {
			t.Errorf("%s = %q, want unset (no emulator redirect)", key, v)
		}
	}
}

// Create path: the account comes from the create request's GCPIdentity.
// The gate is read from the create config (the hub defaults reach the
// context only after buildStartContext on create) or from the context.
func TestBuildStartContext_CloudRunAssignCreatePath(t *testing.T) {
	for _, viaCtx := range []bool{false, true} {
		srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
		ctx := context.Background()
		config := &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID},
		}
		if viaCtx {
			ctx = crGateCtx()
		} else {
			config.HubAgentDefaults = crGateDefaults()
		}
		sc, err := srv.buildStartContext(ctx, startContextInputs{
			Name:        "agent-cr-assign-create",
			ProjectPath: projectDir,
			Config:      config,
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatalf("gate via ctx %v: buildStartContext: %v", viaCtx, err)
		}
		assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
	}
}

// Start path: the account comes from the hub's resolved env with
// SCION_METADATA_MODE_SOURCE=hub.
func TestBuildStartContext_CloudRunAssignStartPath(t *testing.T) {
	srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
	sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
		Name:        "agent-cr-assign-start",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":            "assign",
			"SCION_METADATA_MODE_SOURCE":     "hub",
			"SCION_METADATA_SA_EMAIL":        crAssignedSA,
			"SCION_METADATA_PROJECT_ID":      crProjectID,
			"SCION_METADATA_NATIVE_CLOUDRUN": "1",
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
}

// A mixed-case account is lower-cased once, for the allow-list check, the
// instance identity and the informational env.
func TestBuildStartContext_CloudRunAssignMixedCaseLowerCased(t *testing.T) {
	srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-cr-assign-case",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity:      &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: "Assigned-SA@EXAMPLE-PROJECT.iam.gserviceaccount.com", ProjectID: crProjectID},
			HubAgentDefaults: crGateDefaults(),
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
}

// The refusals with the gate on, each before any provisioning. None names
// an account or the allow-list.
func TestBuildStartContext_CloudRunIdentityRefusals(t *testing.T) {
	assignStartEnv := func(source string) map[string]string {
		env := map[string]string{"SCION_METADATA_MODE": "assign", "SCION_METADATA_SA_EMAIL": crAssignedSA}
		if source != "" {
			env["SCION_METADATA_MODE_SOURCE"] = source
		}
		return env
	}
	tests := []struct {
		name        string
		global      string
		gcp         *GCPIdentityConfig
		resolvedEnv map[string]string
		wantStatus  int
		wantText    []string
	}{
		{name: "assign outside the allow-list", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: "unlisted-sa@example-project.iam.gserviceaccount.com"},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", "account not attachable on this broker"}},
		{name: "assign with no allow-list", global: testCloudRunProjectSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", "account not attachable on this broker"}},
		{name: "assign with no global settings at all",
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA},
			wantStatus: http.StatusBadRequest, wantText: []string{"account not attachable on this broker"}},
		{name: "assign with an empty account", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true},
			wantStatus: http.StatusBadRequest, wantText: []string{"requires a service account email"}},
		{name: "assign on start without SOURCE=hub is block, no block account", global: testCloudRunAllowListGlobalSettingsYAML,
			resolvedEnv: assignStartEnv(""),
			wantStatus:  http.StatusBadRequest, wantText: []string{`"block"`, crBlockUnset}},
		{name: "assign on start with another SOURCE is block, no block account", global: testCloudRunAllowListGlobalSettingsYAML,
			resolvedEnv: assignStartEnv("agent"),
			wantStatus:  http.StatusBadRequest, wantText: []string{`"block"`, crBlockUnset}},
		{name: "block, no block account", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "block"},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", `"block"`, crBlockUnset}},
		{name: "block, no global settings at all",
			gcp:        &GCPIdentityConfig{MetadataMode: "block"},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", crBlockUnset}},
		{name: "unset is block, no block account", global: testCloudRunAllowListGlobalSettingsYAML,
			wantStatus: http.StatusBadRequest, wantText: []string{`"block"`, crBlockUnset}},
		{name: "hub-default passthrough is block, no block account", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
			wantStatus: http.StatusBadRequest, wantText: []string{`"block"`, crBlockUnset}},
		{name: "explicit passthrough", global: testCloudRunBlockGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "passthrough"},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", cloudRunPassthroughRefusal}},
		{name: "project-default passthrough on start", global: testCloudRunBlockGlobalSettingsYAML,
			resolvedEnv: map[string]string{"SCION_METADATA_MODE": "passthrough", "SCION_METADATA_MODE_SOURCE": "hub"},
			wantStatus:  http.StatusBadRequest, wantText: []string{"agent-cr-refusal", cloudRunPassthroughRefusal}},
		{name: "empty mode", global: testCloudRunBlockGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", "is not valid on Cloud Run Instances"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, tc.global)
			in := startContextInputs{
				Name:        "agent-cr-refusal",
				ProjectPath: projectDir,
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
			if tc.resolvedEnv != nil {
				in.ResolvedEnv = tc.resolvedEnv
				in.Operation = opHTTPStart
			} else {
				in.Config = &CreateAgentConfig{GCPIdentity: tc.gcp}
			}
			sc, err := srv.buildStartContext(crGateCtx(), in)
			if err == nil {
				t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if sce.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", sce.Status, tc.wantStatus)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(sce.Message, want) {
					t.Errorf("message %q does not contain %q", sce.Message, want)
				}
			}
			if strings.Contains(sce.Message, "not yet supported") {
				t.Errorf("message %q is the earlier interim refusal", sce.Message)
			}
			for _, account := range []string{crAssignedSA, crListedSA, crBlockSA, "unlisted-sa@example-project.iam.gserviceaccount.com", "Assigned-SA", "Block-SA"} {
				if strings.Contains(sce.Message, account) {
					t.Errorf("message %q names the account %q", sce.Message, account)
				}
			}
		})
	}
}

// A project's own settings.yaml cannot add an account to the allow-list:
// it is ignored, with a warning, and the dispatch is refused.
func TestBuildStartContext_CloudRunProjectAllowListIgnored(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, cloudRunRuntimeType)
	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML+`        cloudrun:
            assignable_service_accounts:
                - unlisted-sa@example-project.iam.gserviceaccount.com
`)
	newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)

	_, err := srv.buildStartContext(crGateCtx(), startContextInputs{
		Name:        "agent-cr-project-list",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: "unlisted-sa@example-project.iam.gserviceaccount.com"},
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	sce := requireStartContextError(t, err)
	if !strings.Contains(sce.Message, "account not attachable on this broker") {
		t.Errorf("message = %q, want the allow-list refusal", sce.Message)
	}
	logged := buf.String()
	if !strings.Contains(logged, "assignable_service_accounts") || !strings.Contains(logged, "never consulted") {
		t.Errorf("expected a warning that the project-level list is never consulted, got log: %s", logged)
	}
	if strings.Contains(logged, "unlisted-sa@example-project.iam.gserviceaccount.com") || strings.Contains(logged, crListedSA) {
		t.Errorf("the log names an account: %s", logged)
	}
}

func TestRejectCloudRunIdentityRuntimeChange(t *testing.T) {
	id := &api.CloudRunIdentity{ServiceAccount: crAssignedSA, Mode: "assign"}
	entryA := dispatchProfileSelection{ProfileName: "local", RuntimeEntryName: "cloudrun"}
	entryB := dispatchProfileSelection{ProfileName: "local", RuntimeEntryName: "cr-team"}
	pinnedA, pinnedB, pinnedNone := "cloudrun", "cr-team", ""
	tests := []struct {
		name       string
		id         *api.CloudRunIdentity
		early      *dispatchProfileSelection
		late       dispatchProfileSelection
		pinned     *string
		runtime    string
		gate       bool
		wantReject bool
	}{
		{name: "identity, same Cloud Run entry", id: id, early: &entryA, late: entryA, runtime: cloudRunRuntimeType, gate: true},
		{name: "no identity, not Cloud Run", runtime: "docker", gate: true},
		{name: "no identity, Cloud Run with the gate off", runtime: cloudRunRuntimeType},
		{name: "identity, now docker (gap A)", id: id, early: &entryA, late: entryA, runtime: "docker", gate: true, wantReject: true},
		{name: "identity, now kubernetes (gap A)", id: id, early: &entryA, late: entryA, runtime: "kubernetes", gate: true, wantReject: true},
		{name: "no identity, now Cloud Run with the gate on (gap B)", runtime: cloudRunRuntimeType, gate: true, wantReject: true},
		{name: "identity, another Cloud Run entry", id: id, early: &entryA, late: entryB, runtime: cloudRunRuntimeType, gate: true, wantReject: true},
		{name: "identity, no recorded selection", id: id, late: entryA, runtime: cloudRunRuntimeType, gate: true, wantReject: true},
		{name: "identity, pinned runtime of the same entry", id: id, early: &entryA, late: entryA, pinned: &pinnedA, runtime: cloudRunRuntimeType, gate: true},
		{name: "identity, pinned runtime of another entry", id: id, early: &entryA, late: entryA, pinned: &pinnedB, runtime: cloudRunRuntimeType, gate: true, wantReject: true},
		{name: "identity, pinned runtime with no recorded entry", id: id, early: &entryA, late: entryA, pinned: &pinnedNone, runtime: cloudRunRuntimeType, gate: true, wantReject: true},
		{name: "no identity, gate off, pinned runtime of another entry", early: &entryA, late: entryA, pinned: &pinnedB, runtime: cloudRunRuntimeType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.gate {
				ctx = crGateCtx()
			}
			opts := api.StartOptions{Name: "agent-cr-late", CloudRunIdentity: tc.id}
			sce := rejectCloudRunIdentityRuntimeChange(ctx, opts, tc.early, tc.runtime, func() dispatchProfileSelection { return tc.late }, tc.pinned)
			if !tc.wantReject {
				if sce != nil {
					t.Fatalf("expected no rejection, got %v", sce)
				}
				return
			}
			if sce == nil {
				t.Fatal("expected a rejection, got nil")
			}
			if sce.Status != http.StatusConflict {
				t.Errorf("status = %d, want %d", sce.Status, http.StatusConflict)
			}
			if !strings.Contains(sce.Message, "agent-cr-late") || strings.Contains(sce.Message, crAssignedSA) {
				t.Errorf("message %q must name the agent and no account", sce.Message)
			}
			if tc.pinned != nil && *tc.pinned == "" && !strings.Contains(sce.Message, "recreate the agent or restart it with its saved profile") {
				t.Errorf("message %q must give the remedy for a pin with no recorded entry", sce.Message)
			}
		})
	}
}

// crLateCheckGlobalSettingsYAML lists the assigned account under the
// "cloudrun" entry that newTestServerForLateCheckOrdering's "local"
// profile selects.
const crLateCheckGlobalSettingsYAML = `schema_version: "1"
runtimes:
    cloudrun:
        cloudrun:
            assignable_service_accounts:
                - assigned-sa@example-project.iam.gserviceaccount.com
`

// postCloudRunLifecycle posts a start or restart (verb) of url-id with the
// given resolved GCP identity mode and the experiment on.
func postCloudRunLifecycle(t *testing.T, srv *Server, verb, mode string) *httptest.ResponseRecorder {
	t.Helper()
	return postCloudRunLifecycleEnv(t, srv, verb, mode, nil)
}

// postCloudRunLifecycleEnv is postCloudRunLifecycle with extra entries in
// the resolved env.
func postCloudRunLifecycleEnv(t *testing.T, srv *Server, verb, mode string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	resolvedEnv := map[string]string{"SCION_METADATA_MODE": mode, "SCION_METADATA_MODE_SOURCE": "hub"}
	if mode == "assign" {
		resolvedEnv["SCION_METADATA_SA_EMAIL"] = crAssignedSA
		resolvedEnv["SCION_METADATA_PROJECT_ID"] = crProjectID
		resolvedEnv[cloudRunNativeMarkerEnv] = "1"
	}
	maps.Copy(resolvedEnv, extra)
	body, err := json.Marshal(map[string]any{
		"resolvedEnv":      resolvedEnv,
		"hubAgentDefaults": map[string]any{"experiments": []string{experiments.CloudRunNativeIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/url-id/"+verb, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// The start and restart handlers' later runtime resolution is checked
// against the identity buildStartContext resolved: a move off Cloud Run
// after an identity was resolved (gap A), or onto Cloud Run with the gate
// on after none was (gap B), is refused with 409 before anything runs or
// stops. The matching case runs, and the identity reaches the runtime's
// RunConfig through the real agent manager.
func TestStartRestart_CloudRunIdentityLateRuntimeChange(t *testing.T) {
	tests := []struct {
		name         string
		verb         string
		first, later string
		wantStatus   int
	}{
		{name: "start, gap A", verb: "start", later: "docker", wantStatus: http.StatusConflict},
		{name: "restart, gap A", verb: "restart", later: "docker", wantStatus: http.StatusConflict},
		{name: "start, gap B", verb: "start", first: "docker", wantStatus: http.StatusConflict},
		{name: "restart, gap B", verb: "restart", first: "docker", wantStatus: http.StatusConflict},
		{name: "start, unchanged", verb: "start", wantStatus: http.StatusAccepted},
		{name: "restart, unchanged", verb: "restart", wantStatus: http.StatusAccepted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", cloudRunRuntimeType)
			var got *runtime.RunConfig
			remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
				if tc.wantStatus != http.StatusAccepted {
					t.Error("the agent must not run once the late runtime change is refused")
				}
				got = &config
				return "instance-id", nil
			}
			changeProfileRuntimeBetweenLoads(srv, "local", tc.first, tc.later)
			newTestGlobalSettings(t, crLateCheckGlobalSettingsYAML)

			w := postCloudRunLifecycle(t, srv, tc.verb, "assign")

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusConflict {
				if !strings.Contains(w.Body.String(), "Cloud Run Instances") {
					t.Errorf("expected the Cloud Run identity refusal, got %s", w.Body.String())
				}
				if strings.Contains(w.Body.String(), crAssignedSA) {
					t.Errorf("the refusal names the account: %s", w.Body.String())
				}
				if mgr.stopCalls != 0 {
					t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
				}
				return
			}
			if got == nil {
				t.Fatal("expected the Cloud Run runtime to run the agent")
			}
			if got.CloudRunIdentity == nil || got.CloudRunIdentity.ServiceAccount != crAssignedSA || got.CloudRunIdentity.Mode != "assign" {
				t.Errorf("RunConfig.CloudRunIdentity = %+v, want {%s assign}", got.CloudRunIdentity, crAssignedSA)
			}
		})
	}
}

// crLateCheckBlockGlobalSettingsYAML is crLateCheckGlobalSettingsYAML plus
// a block account.
const crLateCheckBlockGlobalSettingsYAML = `schema_version: "1"
runtimes:
    cloudrun:
        cloudrun:
            assignable_service_accounts:
                - assigned-sa@example-project.iam.gserviceaccount.com
            block_service_account: block-sa@example-project.iam.gserviceaccount.com
`

// Restart after revoke: an identity change must recreate the instance,
// never start the existing one. An assign agent whose assignment the hub
// revoked is restarted with "block": the restart runs, and the runtime is
// asked to run the agent as the block account with the emulator in block
// mode. That Run deletes the agent's existing instance and creates a new
// one without calling StartInstance
// (TestCloudRunRun_IdentityReplacesSameRunInstance, "revoked assignment,
// restart as block").
func TestRestart_CloudRunRevokedAssignmentArrivesAsBlock(t *testing.T) {
	srv, _, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", cloudRunRuntimeType)
	var got *runtime.RunConfig
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		got = &config
		return "instance-id", nil
	}
	newTestGlobalSettings(t, crLateCheckBlockGlobalSettingsYAML)

	w := postCloudRunLifecycle(t, srv, "restart", "block")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if got == nil {
		t.Fatal("expected the Cloud Run runtime to run the agent")
	}
	if got.CloudRunIdentity == nil || got.CloudRunIdentity.ServiceAccount != crBlockSA || got.CloudRunIdentity.Mode != "block" {
		t.Errorf("RunConfig.CloudRunIdentity = %+v, want {%s block}", got.CloudRunIdentity, crBlockSA)
	}
	env := map[string]string{}
	for _, e := range got.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			env[k] = v
		}
	}
	if env["SCION_METADATA_MODE"] != "block" || env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected the emulator in block mode, got SCION_METADATA_MODE=%q GCE_METADATA_HOST=%q",
			env["SCION_METADATA_MODE"], env["GCE_METADATA_HOST"])
	}
}

// A start or restart whose later resolution moves the agent's profile to
// another Cloud Run runtime entry is refused with 409 before anything runs
// or stops: the allow-list that was checked belongs to the first entry,
// although both entries list the account here. Modelled on
// TestRestartAgent_LateKubernetesAssignRuntimeEntryChangeRunsBeforeStop.
func TestStartRestart_CloudRunIdentityLateEntryChange(t *testing.T) {
	const projectSettingsYAML = `schema_version: "1"
active_profile: other
profiles:
    other:
        runtime: docker
    local:
        runtime: cloudrun
runtimes:
    docker:
        type: docker
    cloudrun:
        type: cloudrun
    cr-team:
        type: cloudrun
`
	for _, verb := range []string{"start", "restart"} {
		t.Run(verb, func(t *testing.T) {
			srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", cloudRunRuntimeType)
			remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
				t.Error("the agent must not run once the late runtime entry change is refused")
				return "", nil
			}
			dotScion := mgr.agents[0].ProjectPath
			if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(projectSettingsYAML), 0644); err != nil {
				t.Fatal(err)
			}
			// The settings file moves "local" to cr-team just before the
			// handler's later resolution (the second runtime-settings load),
			// after buildStartContext has made its selection.
			var loads atomic.Int32
			srv.loadSettings = func(projectDir string) (*config.VersionedSettings, []string, error) {
				if loads.Add(1) == 2 {
					changed := strings.Replace(projectSettingsYAML, "    local:\n        runtime: cloudrun\n", "    local:\n        runtime: cr-team\n", 1)
					if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(changed), 0644); err != nil {
						t.Error(err)
					}
				}
				return config.LoadEffectiveSettings(projectDir)
			}
			newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    cloudrun:
        cloudrun:
            assignable_service_accounts:
                - assigned-sa@example-project.iam.gserviceaccount.com
    cr-team:
        cloudrun:
            assignable_service_accounts:
                - assigned-sa@example-project.iam.gserviceaccount.com
`)

			w := postCloudRunLifecycle(t, srv, verb, "assign")

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusConflict, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "Cloud Run Instances") || !strings.Contains(body, "cr-team") {
				t.Errorf("expected the Cloud Run entry change refusal naming the new entry, got %s", body)
			}
			if strings.Contains(body, crAssignedSA) {
				t.Errorf("the refusal names the account: %s", body)
			}
			if mgr.stopCalls != 0 {
				t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
			}
		})
	}
}

// newFlatCloudRunStartServer is a flat Runtime Broker instance (key cr-a)
// whose runtime is Cloud Run.
func newFlatCloudRunStartServer(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, cloudRunRuntimeType)
	target := config.V1RuntimeTargetConfig{Type: cloudRunRuntimeType}
	srv.config.FlatInstance = &FlatInstanceConfig{
		Identity: &brokeridentity.Identity{
			InstanceKey:     "cr-a",
			RuntimeBrokerID: "flat-cr-broker",
			RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "flat-cr-target", Type: cloudRunRuntimeType},
			ExecutionScope:  brokeridentity.ExecutionScope{Type: cloudRunRuntimeType},
		},
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "cr-a", RuntimeTarget: &target},
	}
	srv.flatK8sIdentity = newFlatKubernetesIdentityPolicy(srv.config.FlatInstance)
	return srv
}

// A flat Cloud Run instance has no allow-list or block account setting, so
// with the gate on assign and block are refused there, even though the
// broker's global settings list the account under a runtime entry. No
// start context is built, and the refusal returns before the agent token
// step (the token probe). The assertions stay off the full message text,
// which a later change adding flat support will reword.
func TestBuildStartContext_CloudRunFlatInstanceRefused(t *testing.T) {
	tests := []struct {
		name string
		gcp  *GCPIdentityConfig
	}{
		{name: "assign", gcp: &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID}},
		{name: "block", gcp: &GCPIdentityConfig{MetadataMode: "block"}},
		{name: "unset is block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFlatCloudRunStartServer(t)
			logs := probeFlatTokenStep(srv)
			newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
			projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML)
			in := startContextInputs{
				Name:        "agent-cr-f1",
				ProjectPath: projectDir,
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
			if tc.gcp != nil {
				in.Config = &CreateAgentConfig{GCPIdentity: tc.gcp}
			}
			sc, err := srv.buildStartContext(crGateCtx(), withFlatTokenProbe(in))
			if sc != nil {
				t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if sce.Status < 400 || sce.Status > 499 {
				t.Errorf("status = %d, want a 4xx", sce.Status)
			}
			if !strings.Contains(sce.Message, "agent-cr-f1") || !strings.Contains(sce.Message, "flat Cloud Run") {
				t.Errorf("message %q must name the agent and the flat cause", sce.Message)
			}
			for _, other := range []string{crBlockUnset, cloudRunPassthroughRefusal, "not attachable"} {
				if strings.Contains(sce.Message, other) {
					t.Errorf("message %q is another refusal (%q), not the flat one", sce.Message, other)
				}
			}
			if strings.Contains(sce.Message, crAssignedSA) || strings.Contains(strings.ToLower(sce.Message), "assigned-sa") {
				t.Errorf("message %q names an account", sce.Message)
			}
			requireNoFlatTokenStep(t, logs)
		})
	}
}

// A malformed allow-list entry is reported at Warn by its position only,
// never its value, and does not stop a listed account from matching.
func TestBuildStartContext_CloudRunAllowListMalformedEntryWarned(t *testing.T) {
	const validSA = "valid-sa@example-project.iam.gserviceaccount.com"
	srv, projectDir := newCloudRunStartContextServer(t, `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun
        cloudrun:
            assignable_service_accounts:
                - valid-sa@example-project.iam.gserviceaccount.com
                - not-an-account-value
`)
	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
		Name:        "agent-cr-malformed",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: validSA, ProjectID: crProjectID},
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertCloudRunAssignEnv(t, sc, validSA, crProjectID)
	logged := buf.String()
	if !strings.Contains(logged, "assignable_service_accounts[1]") {
		t.Errorf("expected a warning naming entry [1], got log: %s", logged)
	}
	if strings.Contains(logged, "assignable_service_accounts[0]") {
		t.Errorf("the well-formed entry [0] was warned about: %s", logged)
	}
	if strings.Contains(logged, "not-an-account-value") || strings.Contains(logged, validSA) {
		t.Errorf("the log names an allow-list value: %s", logged)
	}
}

// Block on Cloud Run with the gate on: block, an unset mode, a hub-default
// passthrough (create and start shapes) and an assign without the hub's
// marker all run the instance as the block account, lower-cased and
// trimmed, with the emulator kept in block mode and no informational
// account env.
func TestBuildStartContext_CloudRunBlock(t *testing.T) {
	tests := []struct {
		name        string
		gcp         *GCPIdentityConfig
		resolvedEnv map[string]string
	}{
		{name: "block", gcp: &GCPIdentityConfig{MetadataMode: "block"}},
		{name: "unset"},
		{name: "hub-default passthrough on create", gcp: &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true}},
		{name: "hub-default passthrough on start", resolvedEnv: map[string]string{
			"SCION_METADATA_MODE":                  "passthrough",
			"SCION_METADATA_MODE_SOURCE":           "hub",
			"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
		}},
		{name: "block on start", resolvedEnv: map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"}},
		{name: "assign on start without SOURCE=hub", resolvedEnv: map[string]string{
			"SCION_METADATA_MODE":     "assign",
			"SCION_METADATA_SA_EMAIL": crAssignedSA,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunBlockGlobalSettingsYAML)
			in := startContextInputs{
				Name:        "agent-cr-block",
				ProjectPath: projectDir,
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
			if tc.resolvedEnv != nil {
				in.ResolvedEnv = tc.resolvedEnv
				in.Operation = opHTTPStart
			} else if tc.gcp != nil {
				in.Config = &CreateAgentConfig{GCPIdentity: tc.gcp}
			}
			sc, err := srv.buildStartContext(crGateCtx(), in)
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			id := sc.Opts.CloudRunIdentity
			if id == nil || id.ServiceAccount != crBlockSA || id.Mode != "block" {
				t.Fatalf("CloudRunIdentity = %+v, want {%s block}", id, crBlockSA)
			}
			if sc.CloudRunSelection == nil || sc.CloudRunSelection.RuntimeEntryName != cloudRunRuntimeType {
				t.Errorf("CloudRunSelection = %+v, want runtime entry %q", sc.CloudRunSelection, cloudRunRuntimeType)
			}
			env := sc.Opts.Env
			if env["SCION_METADATA_MODE"] != "block" || env["GCE_METADATA_HOST"] != "localhost:18380" ||
				env["GCE_METADATA_ROOT"] != "localhost:18380" || env["SCION_METADATA_PORT"] != "18380" {
				t.Errorf("expected the emulator in block mode, got %v", metadataEnv(env))
			}
			if v := env["SCION_METADATA_SA_EMAIL"]; v != "" && v != crAssignedSA {
				t.Errorf("SCION_METADATA_SA_EMAIL = %q, want no block account in the env", v)
			}
			for k, v := range env {
				if strings.Contains(strings.ToLower(v), "block-sa") {
					t.Errorf("env %s carries the block account", k)
				}
			}
		})
	}
}

// A project's own settings.yaml cannot supply the block account: it is
// ignored, with a warning, and block is refused with the remedy.
func TestBuildStartContext_CloudRunProjectBlockAccountIgnored(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, cloudRunRuntimeType)
	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	projectDir := newTestProjectSettings(t, testCloudRunProjectSettingsYAML+`        cloudrun:
            block_service_account: project-block-sa@example-project.iam.gserviceaccount.com
`)
	newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)

	sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
		Name:        "agent-cr-project-block",
		ProjectPath: projectDir,
		Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if sc != nil {
		t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
	}
	sce := requireStartContextError(t, err)
	if !strings.Contains(sce.Message, crBlockUnset) || strings.Contains(sce.Message, "project-block-sa") {
		t.Errorf("message = %q, want the block remedy and no account", sce.Message)
	}
	logged := buf.String()
	if !strings.Contains(logged, "block_service_account") || !strings.Contains(logged, "never consulted") {
		t.Errorf("expected a warning that the project-level block account is never consulted, got log: %s", logged)
	}
	if strings.Contains(logged, "project-block-sa") {
		t.Errorf("the log names the account: %s", logged)
	}
}

// A block account that is not a service account email is still used (the
// instance create then fails), and a warning names the setting only.
func TestBuildStartContext_CloudRunBlockAccountMalformedWarned(t *testing.T) {
	srv, projectDir := newCloudRunStartContextServer(t, `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun
        cloudrun:
            block_service_account: not-a-block-value
`)
	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
		Name:        "agent-cr-block-malformed",
		ProjectPath: projectDir,
		Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if id := sc.Opts.CloudRunIdentity; id == nil || id.ServiceAccount != "not-a-block-value" {
		t.Errorf("CloudRunIdentity = %+v", id)
	}
	logged := buf.String()
	if !strings.Contains(logged, "runtimes.cloudrun.cloudrun.block_service_account is not a service account email") {
		t.Errorf("expected a warning naming the setting, got log: %s", logged)
	}
	if strings.Contains(logged, "not-a-block-value") {
		t.Errorf("the log names the value: %s", logged)
	}
}

// A runtime entry of type cloudrun-instances has no allow-list or block
// account setting: with the gate on, assign and block there are refused
// with their own message, which names the agent and the entry type and no
// account.
func TestBuildStartContext_CloudRunInstancesEntryRefused(t *testing.T) {
	const instancesGlobalSettingsYAML = `schema_version: "1"
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
	tests := []struct {
		name string
		gcp  *GCPIdentityConfig
	}{
		{name: "assign", gcp: &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID}},
		{name: "block", gcp: &GCPIdentityConfig{MetadataMode: "block"}},
		{name: "unset is block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, instancesGlobalSettingsYAML)
			in := startContextInputs{
				Name:        "agent-cr-i1",
				ProjectPath: projectDir,
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
			if tc.gcp != nil {
				in.Config = &CreateAgentConfig{GCPIdentity: tc.gcp}
			}
			sc, err := srv.buildStartContext(crGateCtx(), in)
			if sc != nil {
				t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if sce.Status < 400 || sce.Status > 499 {
				t.Errorf("status = %d, want a 4xx", sce.Status)
			}
			if !strings.Contains(sce.Message, "agent-cr-i1") || !strings.Contains(sce.Message, "of type cloudrun-instances does not support assigned identities") {
				t.Errorf("message %q must name the agent and the entry type", sce.Message)
			}
			if strings.Contains(sce.Message, crAssignedSA) {
				t.Errorf("message %q names an account", sce.Message)
			}
		})
	}
}

func TestCloudRunInstancesEntry(t *testing.T) {
	vs := &config.VersionedSettings{Runtimes: map[string]config.V1RuntimeConfig{
		"typed":     {Type: "cloudrun-instances"},
		"blockonly": {CloudRunInstances: &config.V1CloudRunInstancesConfig{ProjectID: "p", Region: "us-central1"}},
		"cloudrun":  {Type: "cloudrun", CloudRun: &config.CloudRunConfig{}},
		"both":      {CloudRun: &config.CloudRunConfig{}, CloudRunInstances: &config.V1CloudRunInstancesConfig{}},

		// An explicit type wins over the entry's name: a cloudrun entry
		// named "cloudrun-instances" carries the identity settings as usual.
		"cloudrun-instances": {Type: "cloudrun", CloudRun: &config.CloudRunConfig{BlockServiceAccount: crBlockSA}},
	}}
	for name, want := range map[string]bool{
		"typed": true, "blockonly": true, "cloudrun": false, "both": false, "absent": false, "cloudrun-instances": false,
	} {
		if got := cloudRunInstancesEntry(vs, name); got != want {
			t.Errorf("cloudRunInstancesEntry(%q) = %v, want %v", name, got, want)
		}
	}
	// With no type field the entry's name is its type, so an entry named
	// "cloudrun-instances" is one, with or without a cloudrun block.
	for label, entry := range map[string]config.V1RuntimeConfig{
		"with a cloudrun block": {CloudRun: &config.CloudRunConfig{BlockServiceAccount: crBlockSA}},
		"with no blocks":        {},
	} {
		untyped := &config.VersionedSettings{Runtimes: map[string]config.V1RuntimeConfig{"cloudrun-instances": entry}}
		if !cloudRunInstancesEntry(untyped, "cloudrun-instances") {
			t.Errorf("untyped entry named cloudrun-instances %s: want true", label)
		}
	}
	if cloudRunInstancesEntry(nil, "typed") {
		t.Error("nil settings must report false")
	}
}

func TestHubDefaultPassthrough(t *testing.T) {
	tests := []struct {
		name string
		in   startContextInputs
		want bool
	}{
		{name: "create flag set", in: startContextInputs{Operation: opCreate, Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{RequireLocalRuntime: true}}}, want: true},
		{name: "create flag unset", in: startContextInputs{Operation: opCreate, Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{}}}},
		// A start carrying a GCPIdentity: only the env decides.
		{name: "start with the flag only in a GCPIdentity", in: startContextInputs{Operation: opHTTPStart,
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{RequireLocalRuntime: true}}}},
		{name: "start with a GCPIdentity and the flag in the resolved env", in: startContextInputs{Operation: opHTTPStart,
			Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{}},
			ResolvedEnv: map[string]string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true"}}, want: true},
		{name: "restart with the flag only in a GCPIdentity", in: startContextInputs{Operation: opHTTPRestart,
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{RequireLocalRuntime: true}}}},
		{name: "resolved env", in: startContextInputs{ResolvedEnv: map[string]string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true"}}, want: true},
		{name: "config env overrides resolved env", in: startContextInputs{
			ResolvedEnv: map[string]string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true"},
			Config:      &CreateAgentConfig{Env: []string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME=false"}},
		}},
		{name: "nothing"},
	}
	for _, tc := range tests {
		if got := hubDefaultPassthrough(tc.in); got != tc.want {
			t.Errorf("%s: hubDefaultPassthrough = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// pinnedCloudRunEntry reports the pinned runtime's recorded entry only when
// resolveExistingAgentManager would run on the pin: no profile, a pin, and
// no forced runtime.
func TestPinnedCloudRunEntry(t *testing.T) {
	recorded := pinnedRuntime{mgr: &mockManager{}, name: cloudRunRuntimeType,
		record: config.RuntimeBuildRecord{Present: true, EntryName: "cr-team"}}
	unrecorded := pinnedRuntime{mgr: &mockManager{}, name: cloudRunRuntimeType}

	srv := &Server{}
	if got := srv.pinnedCloudRunEntry(api.StartOptions{}, recorded); got == nil || *got != "cr-team" {
		t.Errorf("pin with a record: got %v, want cr-team", got)
	}
	if got := srv.pinnedCloudRunEntry(api.StartOptions{}, unrecorded); got == nil || *got != "" {
		t.Errorf("pin without a record: got %v, want an empty entry", got)
	}
	if got := srv.pinnedCloudRunEntry(api.StartOptions{Profile: "local"}, recorded); got != nil {
		t.Errorf("with a profile the pin is not used: got %q", *got)
	}
	if got := srv.pinnedCloudRunEntry(api.StartOptions{}, pinnedRuntime{}); got != nil {
		t.Errorf("no pin: got %q", *got)
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	forced := newTestServerForStartContextRuntime(t, cfg, cloudRunRuntimeType)
	if got := forced.pinnedCloudRunEntry(api.StartOptions{}, recorded); got != nil {
		t.Errorf("a forced runtime does not use the pin: got %q", *got)
	}
}

// A runtime entry with no type field takes its name as its type, so an
// entry named "cloudrun-instances" without a type is a cloudrun-instances
// entry, with or without a cloudrun block: with the gate on, assign and
// block through a profile that selects it are refused with the
// cloudrun-instances message.
func TestBuildStartContext_CloudRunUntypedInstancesEntryRefused(t *testing.T) {
	entries := map[string]string{
		"with a cloudrun block": `    cloudrun-instances:
        cloudrun:
            assignable_service_accounts:
                - assigned-sa@example-project.iam.gserviceaccount.com
            block_service_account: block-sa@example-project.iam.gserviceaccount.com
`,
		"with no blocks": `    cloudrun-instances: {}
`,
	}
	modes := map[string]*GCPIdentityConfig{
		"assign": {MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID},
		"block":  {MetadataMode: "block"},
	}
	for entryLabel, entryYAML := range entries {
		for modeLabel, gcp := range modes {
			t.Run(entryLabel+"/"+modeLabel, func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "docker", "crp", "cloudrun-instances")
				settings := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: docker
    crp:
        runtime: cloudrun-instances
runtimes:
    docker:
        type: docker
` + entryYAML
				if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settings), 0644); err != nil {
					t.Fatal(err)
				}
				newTestGlobalSettings(t, settings)
				// Every Cloud Run runtime reports the runtime type cloudrun.
				srv.resolveAuxiliaryRuntime = withoutRecord(func(projectPath, agentName, profileFlag string) runtime.Runtime {
					return &runtime.MockRuntime{NameFunc: func() string { return cloudRunRuntimeType }}
				})
				c := *gcp
				sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
					Name:        "agent-cr-i2",
					Config:      &CreateAgentConfig{Profile: "crp", GCPIdentity: &c},
					HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
					Operation:   opCreate,
				})
				if sc != nil {
					t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
				}
				sce := requireStartContextError(t, err)
				if !strings.Contains(sce.Message, "agent-cr-i2") || !strings.Contains(sce.Message, "of type cloudrun-instances does not support assigned identities") {
					t.Errorf("message %q must be the cloudrun-instances refusal naming the agent", sce.Message)
				}
				if strings.Contains(sce.Message, crAssignedSA) || strings.Contains(sce.Message, crBlockSA) {
					t.Errorf("message %q names an account", sce.Message)
				}
			})
		}
	}
}

// With the gate on, a Cloud Run assign runs only when the hub marked the
// dispatch for native Cloud Run identity: the create request's
// GCPIdentity.NativeCloudRun, or SCION_METADATA_NATIVE_CLOUDRUN=1 in a
// resolved env that also carries SCION_METADATA_MODE_SOURCE=hub. Without
// it the dispatch is refused, naming the agent and no account. Assign with
// the marker is covered by the create and start path tests above.
func TestBuildStartContext_CloudRunAssignRequiresNativeMarker(t *testing.T) {
	startEnv := func(marker string) map[string]string {
		env := map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    crAssignedSA,
		}
		if marker != "" {
			env[cloudRunNativeMarkerEnv] = marker
		}
		return env
	}
	tests := []struct {
		name        string
		op          startOperation
		config      *CreateAgentConfig
		resolvedEnv map[string]string
	}{
		{name: "create without the marker", config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID}}},
		{name: "create with the marker only in the config env", config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID},
			Env:         []string{cloudRunNativeMarkerEnv + "=1"}}},
		{name: "start without the marker", op: opHTTPStart, resolvedEnv: startEnv("")},
		{name: "start with another marker value", op: opHTTPStart, resolvedEnv: startEnv("true")},
		// A GCPIdentity carrying the create field on a start is not
		// consulted: with the hub's assign env and no env marker, the
		// start is refused for the missing marker (restart takes the same
		// branch, as does every operation but opCreate). A start with the
		// create field and no hub env is block:
		// TestBuildStartContext_CloudRunStartWithoutHubModeIsBlock.
		{name: "start with the create field set and no env marker", op: opHTTPStart, config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID, NativeCloudRun: true}},
			resolvedEnv: startEnv("")},
		// A start whose config env (with no GCPIdentity) carries the marker:
		// the config env is never read for it.
		{name: "start with the marker only in the config env", op: opHTTPStart,
			config:      &CreateAgentConfig{Env: []string{cloudRunNativeMarkerEnv + "=1"}},
			resolvedEnv: startEnv("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
			in := startContextInputs{
				Name:        "agent-cr-unmarked",
				ProjectPath: projectDir,
				Config:      tc.config,
				ResolvedEnv: tc.resolvedEnv,
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
			if tc.op != "" {
				in.Operation = tc.op
			}
			sc, err := srv.buildStartContext(crGateCtx(), in)
			if sc != nil {
				t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if sce.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", sce.Status, http.StatusBadRequest)
			}
			if !strings.Contains(sce.Message, "agent-cr-unmarked") || !strings.Contains(sce.Message, "did not mark this dispatch for native Cloud Run identity") {
				t.Errorf("message %q must name the agent and the missing marker", sce.Message)
			}
			if strings.Contains(sce.Message, crAssignedSA) {
				t.Errorf("message %q names the account", sce.Message)
			}
		})
	}
}

// On a start, the mode comes only from a resolved env carrying
// SCION_METADATA_MODE_SOURCE=hub. Without one, the start is block, never
// assign or passthrough, whatever the request's GCPIdentity, the
// hub-default passthrough flag, or an unsourced resolved env says. Each row
// runs without a block account (refused for it) and with one (the instance
// runs as the block account, with the emulator in block mode).
func TestBuildStartContext_CloudRunStartWithoutHubModeIsBlock(t *testing.T) {
	tests := []struct {
		name        string
		config      *CreateAgentConfig
		resolvedEnv map[string]string
	}{
		{
			// The create field set, with no hub env.
			name: "create field only",
			config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID, NativeCloudRun: true}},
		},
		{
			// A hub-default passthrough in every place it can appear:
			// GCPIdentity{passthrough, RequireLocalRuntime: true}, and
			// SCION_METADATA_REQUIRE_LOCAL_RUNTIME=true in the resolved env
			// and the config env. On a start, hubDefaultPassthrough reads
			// only the two env inputs; GCPIdentity is consulted only on a
			// create. No mode and no SCION_METADATA_MODE_SOURCE=hub in the
			// resolved env.
			name: "hub-default passthrough in every input",
			config: &CreateAgentConfig{
				GCPIdentity: &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
				Env:         []string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME=true"},
			},
			resolvedEnv: map[string]string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true"},
		},
		{
			// Passthrough and the hub-default flag in a resolved env without
			// SCION_METADATA_MODE_SOURCE=hub.
			name: "passthrough without the hub source",
			resolvedEnv: map[string]string{
				"SCION_METADATA_MODE":                  "passthrough",
				"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
			},
		},
		{
			// Passthrough alone in a resolved env without
			// SCION_METADATA_MODE_SOURCE=hub, and no
			// SCION_METADATA_REQUIRE_LOCAL_RUNTIME in any input, so only the
			// SOURCE=hub rule of effectiveGCPMetadataMode makes it block.
			// Without that rule the start would reach the passthrough
			// refusal (or passthrough mode) and this row would fail.
			name:        "passthrough without the hub source or the hub-default flag",
			resolvedEnv: map[string]string{"SCION_METADATA_MODE": "passthrough"},
		},
	}
	for _, tc := range tests {
		in := func(projectDir string) startContextInputs {
			return startContextInputs{
				Name:        "agent-cr-start-block",
				ProjectPath: projectDir,
				Config:      tc.config,
				ResolvedEnv: tc.resolvedEnv,
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
				Operation:   opHTTPStart,
			}
		}
		t.Run(tc.name+", no block account", func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), in(projectDir))
			if sc != nil {
				t.Fatalf("expected a refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
			}
			sce := requireStartContextError(t, err)
			if sce.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", sce.Status, http.StatusBadRequest)
			}
			if !strings.Contains(sce.Message, "agent-cr-start-block") || !strings.Contains(sce.Message, crBlockUnset) {
				t.Errorf("message %q must name the agent and the missing block account", sce.Message)
			}
			for _, other := range []string{cloudRunPassthroughRefusal, "did not mark this dispatch", crAssignedSA} {
				if strings.Contains(sce.Message, other) {
					t.Errorf("message %q is not the block refusal (has %q)", sce.Message, other)
				}
			}
		})
		t.Run(tc.name+", block account", func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunBlockGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), in(projectDir))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			id := sc.Opts.CloudRunIdentity
			if id == nil || id.ServiceAccount != crBlockSA || id.Mode != "block" {
				t.Fatalf("CloudRunIdentity = %+v, want {%s block}", id, crBlockSA)
			}
			env := sc.Opts.Env
			if env["SCION_METADATA_MODE"] != "block" || env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected the emulator in block mode, got %v", metadataEnv(env))
			}
			if v := env["SCION_METADATA_SA_EMAIL"]; v == crAssignedSA {
				t.Errorf("SCION_METADATA_SA_EMAIL = %q, want no assigned account", v)
			}
		})
	}
}

// The marker key is dropped from the request's config env, whether or not
// the experiment is on; other config env keys are applied.
func TestBuildStartContext_CloudRunNativeMarkerStrippedFromConfigEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-cr-marker-env",
		Config:      &CreateAgentConfig{Env: []string{cloudRunNativeMarkerEnv + "=1", "EXAMPLE_KEY=example-value"}},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if v, ok := sc.Opts.Env[cloudRunNativeMarkerEnv]; ok {
		t.Errorf("%s = %q reached the env from the config env", cloudRunNativeMarkerEnv, v)
	}
	if got := sc.Opts.Env["EXAMPLE_KEY"]; got != "example-value" {
		t.Errorf("EXAMPLE_KEY = %q, want example-value", got)
	}
}

// A restart of an assign agent without the hub's marker is refused before
// the agent is stopped, and the runtime is not called.
func TestRestart_CloudRunAssignWithoutNativeMarkerRefused(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", cloudRunRuntimeType)
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Error("the runtime must not be called for a refused restart")
		return "", nil
	}
	newTestGlobalSettings(t, crLateCheckGlobalSettingsYAML)
	body, err := json.Marshal(map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    crAssignedSA,
		},
		"hubAgentDefaults": map[string]any{"experiments": []string{experiments.CloudRunNativeIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/url-id/restart", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "did not mark this dispatch for native Cloud Run identity") || strings.Contains(w.Body.String(), crAssignedSA) {
		t.Errorf("expected the missing-marker refusal without the account, got %s", w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
	}
}

// On a start or restart the mode and the account come only from the hub's
// resolved env: a GCPIdentity on such a request is not consulted for either.
func TestBuildStartContext_CloudRunStartIgnoresGCPIdentity(t *testing.T) {
	hubEnv := func(withAccount bool) map[string]string {
		env := map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			cloudRunNativeMarkerEnv:      "1",
		}
		if withAccount {
			env["SCION_METADATA_SA_EMAIL"] = crAssignedSA
			env["SCION_METADATA_PROJECT_ID"] = crProjectID
		}
		return env
	}
	t.Run("account from the resolved env", func(t *testing.T) {
		srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
		sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
			Name:        "agent-cr-start-carrier",
			ProjectPath: projectDir,
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign", SAEmail: "unlisted-sa@example-project.iam.gserviceaccount.com", NativeCloudRun: true}},
			ResolvedEnv: hubEnv(true),
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
			Operation:   opHTTPStart,
		})
		if err != nil {
			t.Fatalf("buildStartContext: %v", err)
		}
		assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
	})
	t.Run("mode from the resolved env", func(t *testing.T) {
		// The GCPIdentity says block, which would be refused here (no block
		// account); the resolved env says assign.
		srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
		sc, err := srv.buildStartContext(crGateCtx(), startContextInputs{
			Name:        "agent-cr-start-carrier",
			ProjectPath: projectDir,
			Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
			ResolvedEnv: hubEnv(true),
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
			Operation:   opHTTPStart,
		})
		if err != nil {
			t.Fatalf("buildStartContext: %v", err)
		}
		assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
	})
	t.Run("no account in the resolved env", func(t *testing.T) {
		srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
		_, err := srv.buildStartContext(crGateCtx(), startContextInputs{
			Name:        "agent-cr-start-carrier",
			ProjectPath: projectDir,
			Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign", SAEmail: crAssignedSA, NativeCloudRun: true}},
			ResolvedEnv: hubEnv(false),
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
			Operation:   opHTTPStart,
		})
		sce := requireStartContextError(t, err)
		if !strings.Contains(sce.Message, "requires a service account email") {
			t.Errorf("message = %q, want the missing account refusal", sce.Message)
		}
	})
}

// crSentinelGlobalSettingsYAML cannot be loaded: a duplicated mapping key
// makes the YAML parser fail with an error that quotes the key, so the
// loader error carries crSettingsSentinel.
const crSettingsSentinel = "SENTINEL-acct-value"

const crSentinelGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: cloudrun
runtimes:
    cloudrun:
        type: cloudrun
        cloudrun:
            SENTINEL-acct-value: one
            SENTINEL-acct-value: two
`

// A settings file that cannot be loaded refuses the dispatch with a fixed
// text naming the agent and the setting key, and logs the setting key at
// Warn. The loader error, which can quote file content, appears in
// neither: the test first checks that the loader error does carry the
// sentinel.
func TestResolveCloudRunIdentity_SettingsReadErrorNotEchoed(t *testing.T) {
	tests := []struct {
		name    string
		forced  bool
		wantKey string
	}{
		{name: "profile selection", forced: false, wantKey: "profiles.local.runtime"},
		{name: "global settings", forced: true, wantKey: "runtimes.cloudrun.cloudrun"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, crSentinelGlobalSettingsYAML)
			if !tc.forced {
				srv.config.ForceRuntime = ""
			}
			var logs bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			// The loader error the refusal must not echo.
			var loadErr error
			if tc.forced {
				_, _, loadErr = config.LoadGlobalSettingsWithOverlay()
			} else {
				_, loadErr = srv.resolveDispatchProfileSelectionWithError(api.StartOptions{ProjectPath: projectDir, Profile: "local"})
			}
			if loadErr == nil || !strings.Contains(loadErr.Error(), crSettingsSentinel) {
				t.Fatalf("the fixture must give a loader error carrying %q, got %v", crSettingsSentinel, loadErr)
			}

			in := startContextInputs{
				Name:        "agent-cr-noecho",
				ProjectPath: projectDir,
				Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}},
				Operation:   opCreate,
			}
			id, sce := srv.resolveCloudRunIdentity(crGateCtx(), in, cloudRunRuntimeType, "block", "local", false)
			if sce == nil {
				t.Fatalf("expected a refusal, got %+v", id)
			}
			if sce.Status != http.StatusInternalServerError {
				t.Errorf("status = %d, want %d", sce.Status, http.StatusInternalServerError)
			}
			want := `agent "agent-cr-noecho": ` + tc.wantKey + " could not be read"
			if sce.Message != want {
				t.Errorf("message = %q, want %q", sce.Message, want)
			}
			if strings.Contains(sce.Message, crSettingsSentinel) {
				t.Errorf("message %q quotes the loader error", sce.Message)
			}
			logged := logs.String()
			if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "setting="+tc.wantKey) {
				t.Errorf("expected a Warn line naming setting %s, got %s", tc.wantKey, logged)
			}
			if strings.Contains(logged, crSettingsSentinel) || strings.Contains(logged, "error=") {
				t.Errorf("the log carries the loader error: %s", logged)
			}
		})
	}
}

// crInitFirewallParseCases mirror sciontool init's own parse of
// SCION_INIT_FIREWALL (initFirewallRequested; its cases are
// TestSetupInitFirewall in cmd/sciontool/commands/init_netadmin_test.go),
// with more spellings strconv.ParseBool accepts or rejects and surrounding
// space. wantOn: init installs the firewall. wantErr: init fails the start.
var crInitFirewallParseCases = []struct {
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

// crInitFirewallAgent is the agent name of the init firewall tests.
const crInitFirewallAgent = "agent-cr-firewall"

// crAssignCreateInputs is a native "assign" create of crInitFirewallAgent,
// with the experiment in the create config, and the given resolved env and
// config env.
func crAssignCreateInputs(projectDir string, resolvedEnv map[string]string, configEnv []string) startContextInputs {
	return startContextInputs{
		Name:        crInitFirewallAgent,
		ProjectPath: projectDir,
		ResolvedEnv: resolvedEnv,
		Config: &CreateAgentConfig{
			GCPIdentity:      &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID},
			HubAgentDefaults: crGateDefaults(),
			Env:              configEnv,
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	}
}

// crAssignStartInputs is a native "assign" start of crInitFirewallAgent:
// the hub's assign env with the marker, plus extra in the resolved env, and
// configEnv (nil: no config) as the config env. The experiment comes from
// the context (crGateCtx).
func crAssignStartInputs(projectDir string, extra map[string]string, configEnv []string) startContextInputs {
	resolvedEnv := map[string]string{
		"SCION_METADATA_MODE":        "assign",
		"SCION_METADATA_MODE_SOURCE": "hub",
		"SCION_METADATA_SA_EMAIL":    crAssignedSA,
		"SCION_METADATA_PROJECT_ID":  crProjectID,
		cloudRunNativeMarkerEnv:      "1",
	}
	maps.Copy(resolvedEnv, extra)
	in := startContextInputs{
		Name:        crInitFirewallAgent,
		ProjectPath: projectDir,
		ResolvedEnv: resolvedEnv,
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
		Operation:   opHTTPStart,
	}
	if configEnv != nil {
		in.Config = &CreateAgentConfig{Env: configEnv}
	}
	return in
}

// requireCloudRunInitFirewallRefusal checks a buildStartContext result is
// the init firewall refusal: 400, naming the agent and the remedy, with the
// invalid-value wording exactly when invalid, and no account, project or
// value of the setting.
func requireCloudRunInitFirewallRefusal(t *testing.T, sc *startContext, err error, invalid bool, value string) {
	t.Helper()
	if sc != nil {
		t.Fatalf("expected the init firewall refusal, got CloudRunIdentity %+v", sc.Opts.CloudRunIdentity)
	}
	sce := requireStartContextError(t, err)
	if sce.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", sce.Status, http.StatusBadRequest)
	}
	if !strings.Contains(sce.Message, crInitFirewallAgent) || !strings.Contains(sce.Message, cloudRunInitFirewallRefusal) ||
		!strings.Contains(sce.Message, experiments.CloudRunNativeIdentity) {
		t.Errorf("message %q must name the agent and give the init firewall remedy", sce.Message)
	}
	if got := strings.Contains(sce.Message, "value is invalid"); got != invalid {
		t.Errorf("message %q: invalid-value wording present = %v, want %v", sce.Message, got, invalid)
	}
	for _, leak := range []string{crAssignedSA, "assigned-sa", crProjectID} {
		if strings.Contains(strings.ToLower(sce.Message), leak) {
			t.Errorf("message %q names %q", sce.Message, leak)
		}
	}
	if v := strings.TrimSpace(value); len(v) > 3 && strings.Contains(sce.Message, v) {
		t.Errorf("message %q carries the setting's value", sce.Message)
	}
}

// On a native "assign" dispatch, SCION_INIT_FIREWALL is an unsupported
// combination: it is refused whenever init would read it as on, and also
// when init could not parse it, with the invalid-value wording. An off
// value starts as before and reaches the env unchanged.
func TestBuildStartContext_CloudRunAssignInitFirewallParse(t *testing.T) {
	for _, tc := range crInitFirewallParseCases {
		t.Run(strconv.Quote(tc.value), func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
			sc, err := srv.buildStartContext(context.Background(),
				crAssignCreateInputs(projectDir, nil, []string{cloudRunInitFirewallEnv + "=" + tc.value}))
			if tc.wantOn || tc.wantErr {
				requireCloudRunInitFirewallRefusal(t, sc, err, tc.wantErr, tc.value)
				return
			}
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
			if got, ok := sc.Opts.Env[cloudRunInitFirewallEnv]; !ok || got != tc.value {
				t.Errorf("%s = %q (present %v), want %q unchanged", cloudRunInitFirewallEnv, got, ok, tc.value)
			}
		})
	}
}

// The check reads the merged env, so the firewall is refused from either
// source the broker merges (the hub's resolved env, then the config env),
// on create and start, and the config env's value wins over the resolved
// env's, as it does for the env itself.
func TestBuildStartContext_CloudRunAssignInitFirewallSources(t *testing.T) {
	on := map[string]string{cloudRunInitFirewallEnv: "true"}
	off := map[string]string{cloudRunInitFirewallEnv: "false"}
	tests := []struct {
		name        string
		in          func(projectDir string) startContextInputs
		wantRefused bool
	}{
		{name: "create, resolved env only", wantRefused: true, in: func(dir string) startContextInputs {
			return crAssignCreateInputs(dir, on, nil)
		}},
		{name: "create, config env only", wantRefused: true, in: func(dir string) startContextInputs {
			return crAssignCreateInputs(dir, nil, []string{cloudRunInitFirewallEnv + "=true"})
		}},
		{name: "start, resolved env only", wantRefused: true, in: func(dir string) startContextInputs {
			return crAssignStartInputs(dir, on, nil)
		}},
		{name: "start, config env only", wantRefused: true, in: func(dir string) startContextInputs {
			return crAssignStartInputs(dir, nil, []string{cloudRunInitFirewallEnv + "=true"})
		}},
		{name: "create, resolved env on, config env off", wantRefused: false, in: func(dir string) startContextInputs {
			return crAssignCreateInputs(dir, on, []string{cloudRunInitFirewallEnv + "=false"})
		}},
		{name: "create, resolved env off, config env on", wantRefused: true, in: func(dir string) startContextInputs {
			return crAssignCreateInputs(dir, off, []string{cloudRunInitFirewallEnv + "=true"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunAllowListGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), tc.in(projectDir))
			if tc.wantRefused {
				requireCloudRunInitFirewallRefusal(t, sc, err, false, "true")
				return
			}
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			assertCloudRunAssignEnv(t, sc, crAssignedSA, crProjectID)
			if got := sc.Opts.Env[cloudRunInitFirewallEnv]; got != "false" {
				t.Errorf("%s = %q, want the config env's false", cloudRunInitFirewallEnv, got)
			}
		})
	}
}

// The start and restart handlers refuse a native "assign" dispatch with the
// init firewall before the agent is stopped or the runtime is called; the
// create handler's only start context is buildStartContext with opCreate
// (TestBuildStartContext_CloudRunAssignInitFirewallParse and ...Sources).
// An off value, and "block" with the firewall on, reach the runtime with
// the setting unchanged.
func TestStartRestart_CloudRunAssignInitFirewallRefused(t *testing.T) {
	tests := []struct {
		name       string
		verb       string
		mode       string
		value      string
		global     string
		wantStatus int
	}{
		{name: "start, assign, on", verb: "start", mode: "assign", value: "true", global: crLateCheckGlobalSettingsYAML, wantStatus: http.StatusBadRequest},
		{name: "restart, assign, on", verb: "restart", mode: "assign", value: "true", global: crLateCheckGlobalSettingsYAML, wantStatus: http.StatusBadRequest},
		{name: "start, assign, invalid", verb: "start", mode: "assign", value: "garbage", global: crLateCheckGlobalSettingsYAML, wantStatus: http.StatusBadRequest},
		{name: "restart, assign, invalid", verb: "restart", mode: "assign", value: "garbage", global: crLateCheckGlobalSettingsYAML, wantStatus: http.StatusBadRequest},
		{name: "start, assign, off", verb: "start", mode: "assign", value: "false", global: crLateCheckGlobalSettingsYAML, wantStatus: http.StatusAccepted},
		{name: "restart, assign, off", verb: "restart", mode: "assign", value: "0", global: crLateCheckGlobalSettingsYAML, wantStatus: http.StatusAccepted},
		{name: "restart, block, on", verb: "restart", mode: "block", value: "true", global: crLateCheckBlockGlobalSettingsYAML, wantStatus: http.StatusAccepted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", cloudRunRuntimeType)
			var got *runtime.RunConfig
			remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
				if tc.wantStatus != http.StatusAccepted {
					t.Error("the runtime must not be called for a refused dispatch")
				}
				got = &config
				return "instance-id", nil
			}
			newTestGlobalSettings(t, tc.global)

			w := postCloudRunLifecycleEnv(t, srv, tc.verb, tc.mode, map[string]string{cloudRunInitFirewallEnv: tc.value})

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			body := w.Body.String()
			if tc.wantStatus != http.StatusAccepted {
				if !strings.Contains(body, cloudRunInitFirewallRefusal) {
					t.Errorf("expected the init firewall refusal, got %s", body)
				}
				if strings.Contains(body, crAssignedSA) {
					t.Errorf("the refusal names the account: %s", body)
				}
				if mgr.stopCalls != 0 {
					t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
				}
				return
			}
			if got == nil {
				t.Fatal("expected the Cloud Run runtime to run the agent")
			}
			env := map[string]string{}
			for _, e := range got.Env {
				if k, v, ok := strings.Cut(e, "="); ok {
					env[k] = v
				}
			}
			if env[cloudRunInitFirewallEnv] != tc.value {
				t.Errorf("%s = %q, want %q unchanged", cloudRunInitFirewallEnv, env[cloudRunInitFirewallEnv], tc.value)
			}
			if got.CloudRunIdentity == nil || got.CloudRunIdentity.Mode != tc.mode {
				t.Errorf("RunConfig.CloudRunIdentity = %+v, want mode %s", got.CloudRunIdentity, tc.mode)
			}
		})
	}
}

// Negative control: without a native identity (the experiment off on Cloud
// Run, only another experiment on, or the experiment on for docker) the
// init firewall is not refused, and the env is that of the same dispatch
// without it, plus SCION_INIT_FIREWALL=true.
func TestBuildStartContext_CloudRunInitFirewallNativeIdentityOff(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		ctx     context.Context
	}{
		{name: "cloudrun, experiment off", runtime: cloudRunRuntimeType, ctx: context.Background()},
		{name: "cloudrun, only another experiment", runtime: cloudRunRuntimeType,
			ctx: api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{Experiments: []string{experiments.K8sNFSHome}})},
		{name: "docker, experiment on", runtime: "docker", ctx: crGateCtx()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, tc.runtime)
			newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
			build := func(configEnv []string) *startContext {
				t.Helper()
				sc, err := srv.buildStartContext(tc.ctx, startContextInputs{
					Name: crInitFirewallAgent,
					Config: &CreateAgentConfig{
						GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID},
						Env:         configEnv,
					},
					HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
					Operation:   opCreate,
				})
				if err != nil {
					t.Fatalf("buildStartContext: %v", err)
				}
				if sc.Opts.CloudRunIdentity != nil {
					t.Fatalf("CloudRunIdentity = %+v, want nil", sc.Opts.CloudRunIdentity)
				}
				return sc
			}
			without := build(nil)
			with := build([]string{cloudRunInitFirewallEnv + "=true"})
			if got := with.Opts.Env[cloudRunInitFirewallEnv]; got != "true" {
				t.Errorf("%s = %q, want true", cloudRunInitFirewallEnv, got)
			}
			if got := with.Opts.Env["SCION_METADATA_MODE"]; got != "assign" {
				t.Errorf("SCION_METADATA_MODE = %q, want assign", got)
			}
			rest := maps.Clone(with.Opts.Env)
			delete(rest, cloudRunInitFirewallEnv)
			if !maps.Equal(rest, without.Opts.Env) {
				t.Errorf("the env with the init firewall differs beyond that key:\n with:    %v\n without: %v", rest, without.Opts.Env)
			}
		})
	}
}

// Native "block" with the init firewall is not refused: the refusal applies
// to "assign" only. The setting reaches the env unchanged, on create and
// start.
func TestBuildStartContext_CloudRunBlockInitFirewallKept(t *testing.T) {
	tests := []struct {
		name string
		in   func(projectDir string) startContextInputs
	}{
		{name: "create", in: func(dir string) startContextInputs {
			return startContextInputs{
				Name:        crInitFirewallAgent,
				ProjectPath: dir,
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
					Env:         []string{cloudRunInitFirewallEnv + "=true"},
				},
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			}
		}},
		{name: "start", in: func(dir string) startContextInputs {
			return startContextInputs{
				Name:        crInitFirewallAgent,
				ProjectPath: dir,
				ResolvedEnv: map[string]string{
					"SCION_METADATA_MODE":        "block",
					"SCION_METADATA_MODE_SOURCE": "hub",
					cloudRunInitFirewallEnv:      "true",
				},
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents/x/start", nil),
				Operation:   opHTTPStart,
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, projectDir := newCloudRunStartContextServer(t, testCloudRunBlockGlobalSettingsYAML)
			sc, err := srv.buildStartContext(crGateCtx(), tc.in(projectDir))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			id := sc.Opts.CloudRunIdentity
			if id == nil || id.ServiceAccount != crBlockSA || id.Mode != "block" {
				t.Fatalf("CloudRunIdentity = %+v, want {%s block}", id, crBlockSA)
			}
			env := sc.Opts.Env
			if env["SCION_METADATA_MODE"] != "block" || env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected metadata mode block, got %v", metadataEnv(env))
			}
			if got := env[cloudRunInitFirewallEnv]; got != "true" {
				t.Errorf("%s = %q, want true unchanged", cloudRunInitFirewallEnv, got)
			}
		})
	}
}
