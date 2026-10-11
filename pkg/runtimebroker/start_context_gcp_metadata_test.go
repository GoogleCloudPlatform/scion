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

func TestBuildStartContext_GCPMetadataDefaultBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// No GCPIdentity config — should default to block mode
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-no-gcp",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block' by default, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_PORT"] != "18380" {
		t.Errorf("expected SCION_METADATA_PORT='18380', got %q", sc.Opts.Env["SCION_METADATA_PORT"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
	// No SA env vars should be set in block mode
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "" {
		t.Errorf("expected empty SCION_METADATA_SA_EMAIL in block mode, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
}

func TestBuildStartContext_GCPMetadataPassthrough(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Explicit passthrough — should NOT set metadata env vars
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-passthrough",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "passthrough",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// SCION_METADATA_MODE is still recorded for passthrough (unlike the
	// redirect vars below) — it is the only channel through which
	// downstream auth-type auto-detection (e.g. antigravity's vertex-ai
	// selection, ptone/scion#1873) can tell a GCP SA is reachable via
	// passthrough on a freshly created agent.
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "" {
		t.Errorf("expected no GCE_METADATA_ROOT for passthrough, got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

func TestBuildStartContext_GCPMetadataExplicitBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-block",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "block",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

// TestBuildStartContext_GCPMetadataNoIdentityInputOnKubernetesDefaultsToPassthrough
// covers the no-input fallback case: when the caller supplies no GCP identity
// information at all (no Config.GCPIdentity, no resolvedEnv
// SCION_METADATA_MODE), buildStartContext's own secure default is
// runtime-aware. On Kubernetes it resolves to "passthrough", not "block" —
// block on Kubernetes must be chosen explicitly (ptone/scion#2328 phase 1),
// and the Hub's own resolution ladder deliberately leaves the mode unset for
// exactly this "nothing configured" case (as opposed to an explicit project
// or hub default of "block", which the Hub still threads through explicitly
// — see TestBuildStartContext_KubernetesBlockConfiguredServiceAccount) so
// this runtime-appropriate default can apply.
func TestBuildStartContext_GCPMetadataNoIdentityInputOnKubernetesDefaultsToPassthrough(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-no-identity-input",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected no GCP identity input on Kubernetes to be accepted, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected the Kubernetes-specific default 'passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for the passthrough default, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataNoIdentityInputOnDockerDefaultsToBlock is
// the docker-side half of the same guard: the runtime-aware default must not
// change anything for every runtime except Kubernetes. Docker keeps exactly
// its pre-existing "block" default when the caller supplies no GCP identity
// information at all.
func TestBuildStartContext_GCPMetadataNoIdentityInputOnDockerDefaultsToBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-docker-no-identity-input",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected no GCP identity input on docker to be accepted, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected the unchanged default 'block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380' for the block default, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataPassthroughUnchangedOnKubernetes guards the
// Kubernetes block handling against over-reach: "passthrough" must behave
// exactly as on any other runtime.
//
// This test used to also cover "assign" (as
// TestBuildStartContext_GCPMetadataAssignAndPassthroughUnchangedOnKubernetes),
// asserting the placeholder behavior of emulator env unchanged on Kubernetes
// before this change. ptone/scion#2328 gives Kubernetes "assign" its own,
// different behavior (Workload Identity instead of the emulator), so that
// case moved to TestBuildStartContext_KubernetesAssign* below.
func TestBuildStartContext_GCPMetadataPassthroughUnchangedOnKubernetes(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-passthrough",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "passthrough"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected passthrough to be accepted on Kubernetes, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataAssignUnchangedOnDocker covers the other
// required-unchanged case: "assign" on any non-Kubernetes runtime (Docker
// and, by extension, Apple/Podman) still uses the sciontool metadata emulator
// exactly as before — only the Kubernetes runtime's "assign" behavior changes
// here.
func TestBuildStartContext_GCPMetadataAssignUnchangedOnDocker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-docker-assign",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "sa@proj.iam.gserviceaccount.com",
				ProjectID:    "proj",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected assign to be accepted on docker, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "assign" {
		t.Errorf("expected SCION_METADATA_MODE='assign', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "sa@proj.iam.gserviceaccount.com" {
		t.Errorf("expected SCION_METADATA_SA_EMAIL to be set, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "proj" {
		t.Errorf("expected SCION_METADATA_PROJECT_ID to be set, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
}

// TestBuildStartContext_GCPMetadataBlockUnchangedOnDocker guards the
// inversion from the other direction: "block" must remain valid on Docker
// (and, by extension, any non-Kubernetes runtime).
func TestBuildStartContext_GCPMetadataBlockUnchangedOnDocker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-docker-block",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "block",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected block mode to be accepted on docker, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataUnknownModeRejected covers the fail-open case
// the allow-list exists to close. Before the inversion an unrecognised mode was
// treated as a no-op: no SCION_METADATA_MODE, no GCE_METADATA_HOST redirect, and
// therefore a container talking to the real GCE metadata server. The failure was
// silent, which is what made it dangerous — a typo in a mode string downgraded
// the agent to unprotected rather than refusing to start it.
//
// The empty-string case matters most. It is not a typo an operator makes; it is
// what a non-nil GCPIdentity with an unset MetadataMode produces, and it used to
// be indistinguishable from passthrough.
func TestBuildStartContext_GCPMetadataUnknownModeRejected(t *testing.T) {
	modes := map[string]string{
		"typo":            "blocked",
		"unknown value":   "sandbox",
		"empty":           "",
		"wrong case":      "Block",
		"leading space":   " block",
		"prefix of known": "bl",
	}

	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-bad-mode",
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: mode,
					},
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err == nil {
				t.Fatalf("expected an error for metadata mode %q, got nil (env: %v)", mode, sc.Opts.Env)
			}
			// The start must fail rather than proceed with a partial or absent
			// redirect, so there is no context to inspect at all.
			if sc != nil {
				t.Errorf("expected nil startContext alongside the error, got %+v", sc)
			}
			if !strings.Contains(err.Error(), "Invalid GCP metadata mode") {
				t.Errorf("expected an invalid-mode error, got %q", err.Error())
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataUnknownModeFromResolvedEnv checks the second
// input path. The mode can also arrive as hub-injected resolvedEnv on the start
// path, where there is no CreateAgentConfig at all, so the allow-list has to
// cover it too — a broker that validated only the create path would still accept
// an unknown mode from a newer or misbehaving hub.
func TestBuildStartContext_GCPMetadataUnknownModeFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-bad-env-mode",
		ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "blocked"},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for hub-injected mode 'blocked', got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "Invalid GCP metadata mode") {
		t.Errorf("expected an invalid-mode error, got %q", err.Error())
	}
}

// TestBuildStartContext_GCPMetadataModeFromResolvedEnvStillAccepted guards the
// inversion against over-reach: the start path must keep working for the modes
// the hub legitimately injects. SCION_METADATA_MODE_SOURCE=hub is included
// because "assign" is an elevated mode: without the marker the broker treats
// it as untrusted and downgrades it (see
// TestBuildStartContext_GCPMetadataElevatedModeWithoutSourceMarkerDowngraded).
func TestBuildStartContext_GCPMetadataModeFromResolvedEnvStillAccepted(t *testing.T) {
	for _, mode := range []string{"assign", "block"} {
		t.Run(mode, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-env-mode",
				ResolvedEnv: map[string]string{
					"SCION_METADATA_MODE":        mode,
					"SCION_METADATA_MODE_SOURCE": "hub",
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("expected mode %q to be accepted, got %v", mode, err)
			}
			if sc.Opts.Env["SCION_METADATA_MODE"] != mode {
				t.Errorf("expected SCION_METADATA_MODE=%q, got %q", mode, sc.Opts.Env["SCION_METADATA_MODE"])
			}
			if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected the metadata redirect to be set, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataFromResolvedEnv verifies that the start
// path (no Config.GCPIdentity) picks up GCP identity from resolvedEnv
// injected by the hub.  This is the code path hit by "Create & Edit" where
// the agent is provisioned first and then started with updated config.
func TestBuildStartContext_GCPMetadataFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate hub injecting GCP identity via resolvedEnv (start path).
	// SCION_METADATA_MODE_SOURCE marks "assign" as the hub's own
	// authoritative write; without it the broker would downgrade it.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resolved-assign",
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "sa@proj.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID":  "my-project",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "assign" {
		t.Errorf("expected SCION_METADATA_MODE='assign', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "sa@proj.iam.gserviceaccount.com" {
		t.Errorf("expected SA email from resolvedEnv, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "my-project" {
		t.Errorf("expected project ID from resolvedEnv, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

func TestBuildStartContext_GCPMetadataPassthroughFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate hub injecting passthrough mode via resolvedEnv. The source
	// marker is required here too: passthrough is an elevated mode.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resolved-passthrough",
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "passthrough",
			"SCION_METADATA_MODE_SOURCE": "hub",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Passthrough should NOT set metadata server env vars
	if sc.Opts.Env["SCION_METADATA_PORT"] != "" {
		t.Errorf("expected no SCION_METADATA_PORT for passthrough, got %q", sc.Opts.Env["SCION_METADATA_PORT"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataElevatedModeWithoutSourceMarkerDowngraded
// is the version-skew case SCION_METADATA_MODE_SOURCE exists to close: a hub
// old enough to predate the marker sends SCION_METADATA_MODE without it, on
// the same fallback path a stored env var or secret can also reach (see the
// reserved-target checks elsewhere in this change). Absent the marker, an
// elevated mode must be downgraded to the secure default rather than
// trusted.
func TestBuildStartContext_GCPMetadataElevatedModeWithoutSourceMarkerDowngraded(t *testing.T) {
	for _, mode := range []string{"assign", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-unmarked-env-mode",
				ResolvedEnv: map[string]string{"SCION_METADATA_MODE": mode},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("expected downgrade, not rejection, for mode %q, got error: %v", mode, err)
			}
			if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
				t.Errorf("expected unmarked mode %q to be downgraded to block, got %q", mode, sc.Opts.Env["SCION_METADATA_MODE"])
			}
			if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected the metadata redirect to be set after downgrade, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataBothVarsAlwaysMatch guards the invariant
// that GCE_METADATA_HOST and GCE_METADATA_ROOT are always set to the same
// value. A mismatch means gcloud (ROOT) and language SDKs (HOST) would talk
// to different servers. Only tests non-cloudrun-sandbox runtimes since
// cloudrun-sandbox may fail in CI (no link-local).
func TestBuildStartContext_GCPMetadataBothVarsAlwaysMatch(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerWithRuntime(t, cfg, "mock")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-both-vars",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	host := sc.Opts.Env["GCE_METADATA_HOST"]
	root := sc.Opts.Env["GCE_METADATA_ROOT"]
	if host != root {
		t.Errorf("GCE_METADATA_HOST=%q GCE_METADATA_ROOT=%q — must match", host, root)
	}
}

// gcpIdentityDispatchProfileCases is the shared table for
// TestBuildStartContext_GCPIdentityUsesDispatchProfile_{Create,Start,Restart}:
// a broker can register more than one profile (e.g. both a "docker" and a
// "kubernetes" profile), and a dispatch's profile selects which one it runs
// on (ptone/scion#2328). The GCP identity check must follow the runtime the
// profile THIS dispatch names resolves to, not the broker's default runtime
// — covering both directions catches a fix that only handles one.
var gcpIdentityDispatchProfileCases = []struct {
	name                string
	defaultRuntime      string
	profileRuntime      string
	wantDefaultMode     string // resolved mode when no GCP identity is configured
	wantKubernetesBlock bool   // explicit block gets the Kubernetes block identity
}{
	{
		name:                "docker-default broker, kubernetes profile",
		defaultRuntime:      "docker",
		profileRuntime:      "kubernetes",
		wantDefaultMode:     "passthrough",
		wantKubernetesBlock: true,
	},
	{
		name:                "kubernetes-default broker, docker profile",
		defaultRuntime:      "kubernetes",
		profileRuntime:      "docker",
		wantDefaultMode:     "block",
		wantKubernetesBlock: false,
	},
}

const gcpIdentityDispatchOtherProfile = "other-profile"

// assertGCPIdentityDefaultMode calls buildStartContext with makeInputs (which
// must configure no GCP identity at all) and checks the resolved
// SCION_METADATA_MODE matches wantMode.
func assertGCPIdentityDefaultMode(t *testing.T, srv *Server, wantMode string, makeInputs func() startContextInputs) {
	t.Helper()
	sc, err := srv.buildStartContext(context.Background(), makeInputs())
	if err != nil {
		t.Fatalf("expected no GCP identity input to be accepted, got %v", err)
	}
	if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != wantMode {
		t.Errorf("expected SCION_METADATA_MODE=%q, got %q", wantMode, got)
	}
}

// assertGCPIdentityExplicitBlock calls buildStartContext with makeInputs
// (which must configure an explicit "block") and checks it is accepted as
// "block", with the Kubernetes block identity exactly when
// wantKubernetesBlock (the dispatch's profile resolves to Kubernetes,
// ptone/scion#4034).
func assertGCPIdentityExplicitBlock(t *testing.T, srv *Server, wantKubernetesBlock bool, makeInputs func() startContextInputs) {
	t.Helper()
	sc, err := srv.buildStartContext(context.Background(), makeInputs())
	if err != nil {
		t.Fatalf("expected explicit block to be accepted, got %v", err)
	}
	if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", got)
	}
	if got := sc.Opts.KubernetesBlockIdentity != nil; got != wantKubernetesBlock {
		t.Errorf("KubernetesBlockIdentity set = %v, want %v", got, wantKubernetesBlock)
	}
}

// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Create covers the
// create path, where the profile comes from the request's Config.Profile.
func TestBuildStartContext_GCPIdentityUsesDispatchProfile_Create(t *testing.T) {
	for _, tt := range gcpIdentityDispatchProfileCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("no identity configured", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, _ := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				r := httptest.NewRequest("POST", "/api/v1/agents", nil)
				assertGCPIdentityDefaultMode(t, srv, tt.wantDefaultMode, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-profile-default",
						Config:      &CreateAgentConfig{Profile: gcpIdentityDispatchOtherProfile},
						HTTPRequest: r,
						Operation:   opCreate,
					}
				})
			})

			t.Run("explicit block", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, _ := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				r := httptest.NewRequest("POST", "/api/v1/agents", nil)
				assertGCPIdentityExplicitBlock(t, srv, tt.wantKubernetesBlock, func() startContextInputs {
					return startContextInputs{
						Name: "agent-profile-block",
						Config: &CreateAgentConfig{
							Profile:     gcpIdentityDispatchOtherProfile,
							GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
						},
						HTTPRequest: r,
						Operation:   opCreate,
					}
				})
			})
		})
	}
}

// TestBuildStartContext_GCPIdentityCreateIgnoresSavedProfile pins that a
// create dispatch resolves its runtime and GCP identity classification from
// Config.Profile (falling back to the project's active profile when empty,
// inside resolveManagerForOpts itself) — never from the agent's own saved
// profile, even when one exists on disk for this agent name (e.g. a
// re-create after a crash, or a preserved directory from a prior run).
// opts.Profile (what ProvisionAgent, image resolution, and the saved profile
// written back by provision.go all use) is always in.Config.Profile on
// create, with no saved-profile fallback; the manager and GCP classification
// resolved here must agree with that, or a single create could run on one
// runtime while everything else about it is configured for another. Only
// start/restart fall back to the saved profile — see
// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Start/_Restart below.
func TestBuildStartContext_GCPIdentityCreateIgnoresSavedProfile(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	// defaultRuntime (the project's active profile) is docker; the
	// non-default profile resolves to kubernetes.
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "docker", gcpIdentityDispatchOtherProfile, "kubernetes")
	// This agent's own saved profile is the kubernetes one — different from
	// both the active profile and the (empty) Config.Profile the create
	// request below sends.
	writeSavedAgentProfile(t, dotScion, "agent-create-saved-profile-differs", gcpIdentityDispatchOtherProfile)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	assertGCPIdentityDefaultMode(t, srv, "block", func() startContextInputs {
		return startContextInputs{
			Name: "agent-create-saved-profile-differs",
			// No Config.Profile: must resolve against the active profile
			// (docker, "block"), not the saved one (kubernetes, would be
			// "passthrough").
			Config:      &CreateAgentConfig{},
			HTTPRequest: r,
			Operation:   opCreate,
		}
	})
}

// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Start covers the HTTP
// start path, where there is no Config.Profile — the profile comes from the
// agent's own saved profile (agent.GetSavedProfile), the same source
// handlers.go's startAgent already uses for manager resolution.
func TestBuildStartContext_GCPIdentityUsesDispatchProfile_Start(t *testing.T) {
	for _, tt := range gcpIdentityDispatchProfileCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("no identity configured", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-default", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-default/start", nil)
				assertGCPIdentityDefaultMode(t, srv, tt.wantDefaultMode, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-default",
						HTTPRequest: r,
						Operation:   opHTTPStart,
					}
				})
			})

			t.Run("explicit block via resolvedEnv", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-block", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-block/start", nil)
				assertGCPIdentityExplicitBlock(t, srv, tt.wantKubernetesBlock, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-block",
						ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"},
						HTTPRequest: r,
						Operation:   opHTTPStart,
					}
				})
			})
		})
	}
}

// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Restart is the
// restart-path twin of the start-path test above.
func TestBuildStartContext_GCPIdentityUsesDispatchProfile_Restart(t *testing.T) {
	for _, tt := range gcpIdentityDispatchProfileCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("no identity configured", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-restart-default", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-restart-default/restart", nil)
				assertGCPIdentityDefaultMode(t, srv, tt.wantDefaultMode, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-restart-default",
						HTTPRequest: r,
						Operation:   opHTTPRestart,
					}
				})
			})

			t.Run("explicit block via resolvedEnv", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-restart-block", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-restart-block/restart", nil)
				assertGCPIdentityExplicitBlock(t, srv, tt.wantKubernetesBlock, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-restart-block",
						ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"},
						HTTPRequest: r,
						Operation:   opHTTPRestart,
					}
				})
			})
		})
	}
}

// TestBuildStartContext_GCPIdentityForceRuntimeOverridesProfile pins that
// ForceRuntime takes priority over a dispatch's profile, exactly as
// resolveManagerForOpts's own ForceRuntime branch (handlers.go) does: a
// profile naming a different runtime than an operator's forced runtime must
// not be able to work around it.
func TestBuildStartContext_GCPIdentityForceRuntimeOverridesProfile(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	const otherProfile = "other-profile"
	// Settings declare a "docker" default and a "kubernetes" other profile,
	// but ForceRuntime below pins the broker to "docker" regardless.
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "docker", otherProfile, "kubernetes")
	srv.config.ForceRuntime = "docker"

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-force-runtime",
		Config: &CreateAgentConfig{
			Profile:     otherProfile, // would resolve to kubernetes via settings alone
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected block to be accepted since ForceRuntime pins docker regardless of the kubernetes profile, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
}
