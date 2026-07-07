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
	"embed"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestBuildCommonRunArgs_LocalBackend_HostUID verifies that backend=local
// (or empty) advertises the broker's own UID/GID — today's behavior, unchanged.
func TestBuildCommonRunArgs_LocalBackend_HostUID(t *testing.T) {
	cfg := minimalRunConfig()
	// WorkspaceBackendName defaults to "" (local)

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	wantUID := fmt.Sprintf("SCION_HOST_UID=%d", os.Getuid())
	wantGID := fmt.Sprintf("SCION_HOST_GID=%d", os.Getgid())

	assertEnvInArgs(t, args, wantUID, "local backend should advertise host UID")
	assertEnvInArgs(t, args, wantGID, "local backend should advertise host GID")
}

// TestBuildCommonRunArgs_NFSBackend_StableUID verifies that backend=nfs
// advertises the configured NFS UID/GID instead of the host UID.
func TestBuildCommonRunArgs_NFSBackend_StableUID(t *testing.T) {
	cfg := minimalRunConfig()
	cfg.WorkspaceBackendName = "nfs"
	cfg.NFSUID = 1000
	cfg.NFSGID = 1000

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	assertEnvInArgs(t, args, "SCION_HOST_UID=1000", "NFS backend should advertise stable UID 1000")
	assertEnvInArgs(t, args, "SCION_HOST_GID=1000", "NFS backend should advertise stable GID 1000")
}

// TestBuildCommonRunArgs_NFSBackend_CustomUID verifies that NFS UID/GID
// can be set to non-default values via config.
func TestBuildCommonRunArgs_NFSBackend_CustomUID(t *testing.T) {
	cfg := minimalRunConfig()
	cfg.WorkspaceBackendName = "nfs"
	cfg.NFSUID = 2000
	cfg.NFSGID = 2000

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	assertEnvInArgs(t, args, "SCION_HOST_UID=2000", "NFS backend should use custom UID")
	assertEnvInArgs(t, args, "SCION_HOST_GID=2000", "NFS backend should use custom GID")
}

// TestBuildCommonRunArgs_NFSBackend_DefaultUID verifies that zero NFS UID/GID
// defaults to 1000:1000 (design §9.1 convergence with K8s pod UID/GID).
func TestBuildCommonRunArgs_NFSBackend_DefaultUID(t *testing.T) {
	cfg := minimalRunConfig()
	cfg.WorkspaceBackendName = "nfs"
	cfg.NFSUID = 0 // should default to 1000
	cfg.NFSGID = 0 // should default to 1000

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	assertEnvInArgs(t, args, "SCION_HOST_UID=1000", "zero NFS UID should default to 1000")
	assertEnvInArgs(t, args, "SCION_HOST_GID=1000", "zero NFS GID should default to 1000")
}

// TestBuildCommonRunArgs_NFSBackend_ExposesBackendEnv verifies that the
// SCION_WORKSPACE_BACKEND env var is set when backend is "nfs", so sciontool
// init can skip the per-start recursive chown.
func TestBuildCommonRunArgs_NFSBackend_ExposesBackendEnv(t *testing.T) {
	cfg := minimalRunConfig()
	cfg.WorkspaceBackendName = "nfs"
	cfg.NFSUID = 1000
	cfg.NFSGID = 1000

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	assertEnvInArgs(t, args, "SCION_WORKSPACE_BACKEND=nfs",
		"NFS backend should expose SCION_WORKSPACE_BACKEND for sciontool init")
}

// TestBuildCommonRunArgs_LocalBackend_NoBackendEnv verifies that
// SCION_WORKSPACE_BACKEND is not set when the backend is local (empty),
// preserving backward compatibility.
func TestBuildCommonRunArgs_LocalBackend_NoBackendEnv(t *testing.T) {
	cfg := minimalRunConfig()
	// WorkspaceBackendName defaults to ""

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	for i, arg := range args {
		if i > 0 && args[i-1] == "-e" && strings.HasPrefix(arg, "SCION_WORKSPACE_BACKEND=") {
			t.Error("local backend should not set SCION_WORKSPACE_BACKEND env var")
		}
	}
}

// TestPodmanRootless_NFSBackend_Rejected verifies that Podman rootless + NFS
// is rejected with a clear error (design §9.1: keep-id subuid ranges yield
// no stable on-wire UID).
func TestPodmanRootless_NFSBackend_Rejected(t *testing.T) {
	r := &PodmanRuntime{
		Command:  "podman",
		Rootless: true,
	}

	config := minimalRunConfig()
	config.WorkspaceBackendName = "nfs"

	_, err := r.Run(t.Context(), config)
	if err == nil {
		t.Fatal("expected error for Podman rootless + NFS, got nil")
	}
	if !strings.Contains(err.Error(), "rootless") || !strings.Contains(err.Error(), "NFS") {
		t.Errorf("error should mention rootless and NFS, got: %v", err)
	}
}

// TestPodmanRootless_LocalBackend_Allowed verifies that Podman rootless
// with the local backend still works (no regression).
func TestPodmanRootless_LocalBackend_Allowed(t *testing.T) {
	r := &PodmanRuntime{
		Command:  "podman",
		Rootless: true,
	}

	config := minimalRunConfig()
	config.WorkspaceBackendName = "local"

	// This will fail because podman isn't installed, but it should NOT fail
	// with the rootless+NFS rejection error.
	_, err := r.Run(t.Context(), config)
	if err != nil && strings.Contains(err.Error(), "rootless") && strings.Contains(err.Error(), "NFS") {
		t.Errorf("local backend should not trigger rootless+NFS rejection, got: %v", err)
	}
	// Other errors (podman not installed, etc.) are expected — we only check
	// that the NFS-specific guard doesn't fire.
}

// --- helpers ---

// minimalRunConfig returns a RunConfig with the minimum fields needed
// to call buildCommonRunArgs without error. It uses a stub harness.
func minimalRunConfig() RunConfig {
	return RunConfig{
		Name:         "test-agent",
		Image:        "test-image:latest",
		UnixUsername: "scion",
		Harness:      &nfsTestHarness{},
		Workspace:    "/tmp/test-workspace",
	}
}

// nfsTestHarness satisfies the api.Harness interface for N1-5 tests.
type nfsTestHarness struct{}

func (h *nfsTestHarness) Name() string { return "test" }
func (h *nfsTestHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{Harness: "test"}
}
func (h *nfsTestHarness) GetCommand(task string, resume bool, args []string) []string {
	return []string{"echo", "test"}
}
func (h *nfsTestHarness) GetEnv(name, homeDir, unixUsername string) map[string]string {
	return map[string]string{}
}
func (h *nfsTestHarness) GetTelemetryEnv() map[string]string    { return nil }
func (h *nfsTestHarness) DefaultConfigDir() string              { return ".test" }
func (h *nfsTestHarness) SkillsDir() string                     { return ".test/skills" }
func (h *nfsTestHarness) HasSystemPrompt(agentHome string) bool { return false }
func (h *nfsTestHarness) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}
func (h *nfsTestHarness) GetInterruptKey() string                { return "C-c" }
func (h *nfsTestHarness) GetInterruptSequence() []string         { return nil }
func (h *nfsTestHarness) GetHarnessEmbedsFS() (embed.FS, string) { return embed.FS{}, "" }
func (h *nfsTestHarness) InjectAgentInstructions(agentHome string, content []byte) error {
	return nil
}
func (h *nfsTestHarness) InjectSystemPrompt(agentHome string, content []byte) error {
	return nil
}
func (h *nfsTestHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	return &api.ResolvedAuth{Method: "test"}, nil
}

// assertEnvInArgs checks that the -e flag with the given env value appears in args.
func assertEnvInArgs(t *testing.T, args []string, wantEnv, msg string) {
	t.Helper()
	for i, arg := range args {
		if i > 0 && args[i-1] == "-e" && arg == wantEnv {
			return
		}
	}
	t.Errorf("%s: env %q not found in args", msg, wantEnv)
}
