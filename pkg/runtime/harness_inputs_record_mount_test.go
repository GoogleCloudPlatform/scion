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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	corev1 "k8s.io/api/core/v1"
)

// TestHarnessInputsRecordOutsideScionMounts pins that an agent's control-plane
// records (<agent dir>/harness-inputs, config.HarnessInputsRecordDirName;
// <agent dir>/harness-secrets, config.HarnessSecretsRecordDirName; and the
// broker-side image provenance file <agent dir>/image-provenance.json,
// config.ImageProvenanceFileName) are outside
// every container mount scion computes for the agent, in every runtime mode,
// using the real mount builders. Author-configured volumes are excluded: they
// are a separately tracked capability.
//
// The Docker/Podman/Apple case checks containment of each record path in each
// bind mount directly. The Kubernetes and Cloud Run cases rely on an
// assumption that holds for today's layouts: in those modes the agent
// directory is broker-local and always lies under a .scion directory, so it
// suffices that no mount source or subPath has a .scion element. A future
// layout that mounts a parent of an agent directory without a .scion element
// needs a direct containment check here instead.
func TestHarnessInputsRecordOutsideScionMounts(t *testing.T) {
	t.Run("docker/podman/apple run args", testRecordOutsideRunArgMounts)
	t.Run("kubernetes pod", testRecordOutsidePodMounts)
	t.Run("cloud run nfs host paths", testRecordOutsideCloudRunPaths)
}

// controlPlaneRecordNames are the agent-dir entries that must never be
// visible inside the container: the harness inputs and secrets record
// directories and the image provenance file.
var controlPlaneRecordNames = []string{
	config.HarnessInputsRecordDirName,
	config.HarnessSecretsRecordDirName,
	config.ImageProvenanceFileName,
}

// pathWithin reports whether p is base or below it.
func pathWithin(p, base string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func hasScionElement(p string) bool {
	for _, el := range strings.Split(filepath.ToSlash(p), "/") {
		if el == config.DotScion {
			return true
		}
	}
	return false
}

func testRecordOutsideRunArgMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	scion := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(scion, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(scion, "11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}

	type mode struct {
		name     string
		agentDir string
		cfg      RunConfig
	}
	inProject := config.GetAgentDir(scion, "a", false)
	external := config.GetAgentDir(scion, "a", true)
	if inProject == external {
		t.Fatalf("fixture: shared-workspace agent dir should be external, got %s", external)
	}
	withPaths := func(home, repoRoot, workspace string, gc *api.GitCloneConfig) RunConfig {
		cfg := minimalRunConfig()
		cfg.BrokerMode = true
		cfg.HomeDir = home
		cfg.RepoRoot = repoRoot
		cfg.Workspace = workspace
		cfg.GitClone = gc
		return cfg
	}
	modes := []mode{
		{"worktree outside the repo (full repo root mounted)", inProject,
			withPaths(config.GetAgentHomePath(scion, "a"), root, filepath.Join(base, ".scion_worktrees", "repo", "a"), nil)},
		{"worktree inside the repo", inProject,
			withPaths(config.GetAgentHomePath(scion, "a"), root, filepath.Join(root, "worktrees", "a"), nil)},
		{"shared workspace (repo root)", external,
			withPaths(filepath.Join(external, "home"), root, root, nil)},
		{"git clone into the agent workspace", inProject,
			withPaths(filepath.Join(inProject, "home"), "", filepath.Join(inProject, "workspace"), &api.GitCloneConfig{URL: "https://example.com/r.git"})},
		{"per-agent workspace, no repo", inProject,
			withPaths(filepath.Join(inProject, "home"), "", filepath.Join(inProject, "workspace"), nil)},
	}
	// A restart dispatch without the shared-workspace flag could resolve the
	// in-project agent dir; in the shared-workspace layout that path is under
	// the /workspace mount and must be shadowed by the agents tmpfs.
	modes = append(modes, mode{"shared workspace, in-project agent dir (restart resolution)", inProject,
		withPaths(filepath.Join(external, "home"), root, root, nil)})
	for _, m := range modes {
		for _, recordName := range controlPlaneRecordNames {
			t.Run(m.name+"/"+recordName, func(t *testing.T) {
				checkRecordOutsideRunArgs(t, filepath.Join(m.agentDir, recordName), m.cfg)
			})
		}
	}
}

func checkRecordOutsideRunArgs(t *testing.T, record string, cfg RunConfig) {
	t.Helper()
	if !hasScionElement(record) {
		t.Fatalf("fixture: record %s should lie under a .scion directory", record)
	}
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	var tmpfs []string
	type bind struct{ src, dst string }
	var binds []bind
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "-v":
			parts := strings.SplitN(args[i+1], ":", 3)
			if len(parts) >= 2 {
				binds = append(binds, bind{parts[0], parts[1]})
			}
		case "--mount":
			spec := args[i+1]
			if strings.Contains(spec, "type=tmpfs") {
				for _, kv := range strings.Split(spec, ",") {
					if v, ok := strings.CutPrefix(kv, "destination="); ok {
						tmpfs = append(tmpfs, v)
					}
				}
			} else {
				t.Errorf("unexpected non-tmpfs --mount %q; extend this test", spec)
			}
		}
	}
	if len(binds) == 0 {
		t.Fatal("fixture: expected bind mounts")
	}
	for _, b := range binds {
		if !pathWithin(record, b.src) {
			continue
		}
		rel, _ := filepath.Rel(b.src, record)
		inContainer := filepath.Join(b.dst, rel)
		masked := false
		for _, m := range tmpfs {
			if pathWithin(inContainer, m) {
				masked = true
			}
		}
		if !masked {
			t.Errorf("record %s is visible in the container at %s via the mount of %s", record, inContainer, b.src)
		}
	}
}

func testRecordOutsidePodMounts(t *testing.T) {
	plain := minimalRunConfig()
	plain.HomeDir = filepath.Join(t.TempDir(), "home")
	configs := map[string]RunConfig{
		"default storage":     plain,
		"nfs shared checkout": nfsBaseConfig("shared"),
		"nfs clone-per-agent": nfsAgentDirConfig("cpa"),
		"nfs empty-per-agent": nfsEmptyAgentDirConfig("epa"),
		"nfs worktree":        nfsWorktreeConfig("wt"),
		"nfs home":            nfsHomeTestConfig(true),
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			rt := newNFSTestK8sRuntime()
			pod, err := rt.buildPod("default", cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			check := func(where string, mounts []corev1.VolumeMount) {
				for _, m := range mounts {
					if hasScionElement(m.SubPath) || hasScionElement(m.SubPathExpr) {
						t.Errorf("%s mount %s (subPath %q) is inside a .scion directory, where agent records live", where, m.MountPath, m.SubPath)
					}
				}
			}
			for _, c := range pod.Spec.InitContainers {
				check("init container "+c.Name, c.VolumeMounts)
			}
			for _, c := range pod.Spec.Containers {
				check("container "+c.Name, c.VolumeMounts)
			}
			for _, v := range pod.Spec.Volumes {
				if v.HostPath != nil && hasScionElement(v.HostPath.Path) {
					t.Errorf("hostPath volume %s at %s is inside a .scion directory", v.Name, v.HostPath.Path)
				}
				if v.NFS != nil && hasScionElement(v.NFS.Path) {
					t.Errorf("nfs volume %s at %s is inside a .scion directory", v.Name, v.NFS.Path)
				}
			}
		})
	}
}

func testRecordOutsideCloudRunPaths(t *testing.T) {
	root, err := config.ResolveSubPathRoot("")
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join("/mnt/filestore", root, "proj-123", "workspace")
	paths, err := cloudRunNFSHostPaths(workspace, "", "proj-123", "agent-1")
	if err != nil {
		t.Fatalf("cloudRunNFSHostPaths: %v", err)
	}
	for what, p := range map[string]string{
		"workspace": paths.workspaceHostPath,
		"home":      paths.homeHostPath,
		"secrets":   paths.secretsHostPath,
	} {
		if hasScionElement(p) {
			t.Errorf("cloud run %s host path %s is inside a .scion directory, where agent records live", what, p)
		}
	}
}

// In the shared-workspace layout the in-project agents root is shadowed with
// a tmpfs when <workspace>/.scion is a directory, and no mask is added when
// .scion is a project marker file.
func TestSharedWorkspaceAgentsMask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hasMask := func(args []string) bool {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--mount" && args[i+1] == "type=tmpfs,destination=/workspace/.scion/agents" {
				return true
			}
		}
		return false
	}
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := minimalRunConfig()
	cfg.BrokerMode = true
	cfg.HomeDir = filepath.Join(t.TempDir(), "home")
	cfg.RepoRoot = root
	cfg.Workspace = root
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMask(args) {
		t.Error("shared workspace with a .scion directory: expected the agents tmpfs mask")
	}

	markerRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(markerRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerRoot, ".scion"), []byte("project-id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.RepoRoot = markerRoot
	cfg.Workspace = markerRoot
	args, err = buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if hasMask(args) {
		t.Error("shared workspace with a .scion marker file: no agents mask expected")
	}
}
