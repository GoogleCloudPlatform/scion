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
	crProjectID  = "example-project"
)

// crNotYetSupported is the fixed part of the refusal for every mode other
// than assign while the experiment is on (Phase 1).
const crNotYetSupported = "is not yet supported on Cloud Run Instances with the hub.cloudrun_native_identity experiment on"

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
var gateOffEnvModes = []struct {
	name string
	gcp  *GCPIdentityConfig
}{
	{name: "unset"},
	{name: "block", gcp: &GCPIdentityConfig{MetadataMode: "block"}},
	{name: "passthrough", gcp: &GCPIdentityConfig{MetadataMode: "passthrough"}},
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
//   - Cloud Run vs docker: the gate-off Cloud Run dispatch has exactly the
//     GCP identity env (every SCION_METADATA_* and GCE_METADATA_* key and
//     value) of the same dispatch on docker, which this change does not
//     touch, so it is today's identity env;
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
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID},
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
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    crAssignedSA,
			"SCION_METADATA_PROJECT_ID":  crProjectID,
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
			GCPIdentity:      &GCPIdentityConfig{MetadataMode: "assign", SAEmail: "Assigned-SA@EXAMPLE-PROJECT.iam.gserviceaccount.com", ProjectID: crProjectID},
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
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", SAEmail: "unlisted-sa@example-project.iam.gserviceaccount.com"},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", "account not attachable on this broker"}},
		{name: "assign with no allow-list", global: testCloudRunProjectSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", "account not attachable on this broker"}},
		{name: "assign with no global settings at all",
			gcp:        &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA},
			wantStatus: http.StatusBadRequest, wantText: []string{"account not attachable on this broker"}},
		{name: "assign with an empty account", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "assign"},
			wantStatus: http.StatusBadRequest, wantText: []string{"requires a service account email"}},
		{name: "assign on start without SOURCE=hub is block", global: testCloudRunAllowListGlobalSettingsYAML,
			resolvedEnv: assignStartEnv(""),
			wantStatus:  http.StatusBadRequest, wantText: []string{`"block"`, crNotYetSupported}},
		{name: "assign on start with another SOURCE is block", global: testCloudRunAllowListGlobalSettingsYAML,
			resolvedEnv: assignStartEnv("agent"),
			wantStatus:  http.StatusBadRequest, wantText: []string{`"block"`, crNotYetSupported}},
		{name: "block", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "block"},
			wantStatus: http.StatusBadRequest, wantText: []string{"agent-cr-refusal", `"block"`, crNotYetSupported}},
		{name: "unset is block", global: testCloudRunAllowListGlobalSettingsYAML,
			wantStatus: http.StatusBadRequest, wantText: []string{`"block"`, crNotYetSupported}},
		{name: "explicit passthrough", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "passthrough"},
			wantStatus: http.StatusBadRequest, wantText: []string{`"passthrough"`, crNotYetSupported}},
		{name: "hub-default passthrough", global: testCloudRunAllowListGlobalSettingsYAML,
			gcp:        &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
			wantStatus: http.StatusBadRequest, wantText: []string{crNotYetSupported}},
		{name: "project-default passthrough on start", global: testCloudRunAllowListGlobalSettingsYAML,
			resolvedEnv: map[string]string{"SCION_METADATA_MODE": "passthrough", "SCION_METADATA_MODE_SOURCE": "hub"},
			wantStatus:  http.StatusBadRequest, wantText: []string{`"passthrough"`, crNotYetSupported}},
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
			for _, account := range []string{crAssignedSA, crListedSA, "unlisted-sa@example-project.iam.gserviceaccount.com", "Assigned-SA"} {
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
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: "unlisted-sa@example-project.iam.gserviceaccount.com"},
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
	tests := []struct {
		name       string
		id         *api.CloudRunIdentity
		early      *dispatchProfileSelection
		late       dispatchProfileSelection
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.gate {
				ctx = crGateCtx()
			}
			opts := api.StartOptions{Name: "agent-cr-late", CloudRunIdentity: tc.id}
			sce := rejectCloudRunIdentityRuntimeChange(ctx, opts, tc.early, tc.runtime, func() dispatchProfileSelection { return tc.late })
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
	resolvedEnv := map[string]string{"SCION_METADATA_MODE": mode, "SCION_METADATA_MODE_SOURCE": "hub"}
	if mode == "assign" {
		resolvedEnv["SCION_METADATA_SA_EMAIL"] = crAssignedSA
		resolvedEnv["SCION_METADATA_PROJECT_ID"] = crProjectID
	}
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

// Restart after revoke: an identity change must recreate the instance,
// never start the old one. An assign agent whose assignment the hub
// revoked is restarted with "block". In Phase 1 block is not yet supported
// with the experiment on, so the restart is refused before the agent is
// stopped and the runtime is never called: no existing instance is started
// in place. Phase 2 turns this into a new instance under the block account.
func TestRestart_CloudRunRevokedAssignmentArrivesAsBlock(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", cloudRunRuntimeType)
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Error("the runtime must not be called for a refused restart")
		return "", nil
	}
	newTestGlobalSettings(t, crLateCheckGlobalSettingsYAML)

	w := postCloudRunLifecycle(t, srv, "restart", "block")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), crNotYetSupported) {
		t.Errorf("expected the not-yet-supported refusal, got %s", w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected the agent not to be stopped, got %d stop calls", mgr.stopCalls)
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
		{name: "assign", gcp: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: crAssignedSA, ProjectID: crProjectID}},
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
			for _, other := range []string{crNotYetSupported, "not attachable"} {
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
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: validSA, ProjectID: crProjectID},
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
