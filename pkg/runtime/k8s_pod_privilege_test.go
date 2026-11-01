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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// forbiddenPodCapabilities may not be added to any container of an agent
// pod.
var forbiddenPodCapabilities = map[corev1.Capability]bool{
	"SYS_ADMIN": true, "CAP_SYS_ADMIN": true,
	"MKNOD": true, "CAP_MKNOD": true,
	"ALL": true,
}

// TestBuildPod_AgentPodPrivileges: on every workspace and home backend
// (including a share mounted inline, without pv_name), the
// agent container runs as non-root without privilege escalation, adds no
// capabilities, is not privileged and gets no host devices; no init
// container adds SYS_ADMIN or MKNOD or is privileged; and the pod has no
// host-path volume.
func TestBuildPod_AgentPodPrivileges(t *testing.T) {
	nfsHome := nfsHomeTestConfig(true)
	nfsHome.Name = "agent-nfs-home"
	claimCfg, _ := nfsClaimGoldenConfig(t)
	inlineWorkspace := inlineTestConfig(claimCfg)
	cases := map[string]struct {
		cfg RunConfig
		// wantInit are init containers the pod must have.
		wantInit []string
		// nfsRuntime builds the pod with the NFS test runtime, as the
		// inline-share tests do.
		nfsRuntime bool
		// wantInlineNFS: the pod must mount the workspace share inline (an
		// NFS volume), so the case covers that pod shape.
		wantInlineNFS bool
	}{
		"local workspace": {cfg: RunConfig{
			Name: "agent-local", Image: "test:latest", UnixUsername: "scion",
		}},
		"nfs workspace": {cfg: RunConfig{
			Name: "agent-nfs", Image: "test:latest", UnixUsername: "scion",
			WorkspaceBackendName: "nfs",
			NFSPVClaimName:       "scion-workspaces",
			NFSSubPath:           "projects/proj-123/workspace",
		}, wantInit: []string{k8sWorkspaceProvisionContainer}},
		"nfs home": {cfg: nfsHome, wantInit: []string{k8sHomeLeafContainer, k8sHomePrepareContainer}},
		// No pv_name: the pod mounts the share inline by server path.
		"nfs workspace inline": {cfg: inlineWorkspace, wantInit: []string{k8sWorkspaceProvisionContainer}, nfsRuntime: true, wantInlineNFS: true},
	}
	for name, tc := range cases {
		cfg := tc.cfg
		t.Run(name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			if tc.nfsRuntime {
				rt = newNFSTestK8sRuntime()
			}
			pod, err := rt.buildPod("default", cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}

			psc := pod.Spec.SecurityContext
			if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
				t.Errorf("pod RunAsNonRoot not true: %+v", psc)
			}
			if psc != nil && (psc.RunAsUser == nil || *psc.RunAsUser == 0) {
				t.Errorf("pod RunAsUser = %v, want a non-root uid", psc.RunAsUser)
			}
			if pod.Spec.HostPID || pod.Spec.HostIPC || pod.Spec.HostNetwork {
				t.Error("pod shares a host namespace")
			}
			inlineNFS := false
			for _, v := range pod.Spec.Volumes {
				if v.HostPath != nil {
					t.Errorf("pod volume %q is a host path (%s)", v.Name, v.HostPath.Path)
				}
				if v.Name == "workspace" && v.NFS != nil {
					inlineNFS = true
				}
			}
			if tc.wantInlineNFS && !inlineNFS {
				t.Errorf("pod has no inline NFS workspace volume: %+v", pod.Spec.Volumes)
			}

			var agent *corev1.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == agentContainerName {
					agent = &pod.Spec.Containers[i]
				}
			}
			if agent == nil {
				t.Fatalf("no %q container", agentContainerName)
			}
			sc := agent.SecurityContext
			if sc == nil {
				t.Fatal("agent container has no SecurityContext")
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Error("agent container: AllowPrivilegeEscalation not false")
			}
			if sc.Privileged != nil && *sc.Privileged {
				t.Error("agent container is privileged")
			}
			if sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot {
				t.Error("agent container overrides RunAsNonRoot to false")
			}
			if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
				t.Error("agent container runs as uid 0")
			}
			if sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 {
				t.Errorf("agent container adds capabilities: %+v", sc.Capabilities)
			}
			if sc.Capabilities == nil || !dropsAll(sc.Capabilities) {
				t.Errorf("agent container does not drop ALL: %+v", sc.Capabilities)
			}
			if len(agent.VolumeDevices) != 0 {
				t.Errorf("agent container has volume devices: %+v", agent.VolumeDevices)
			}
			if sc.ProcMount != nil && *sc.ProcMount == corev1.UnmaskedProcMount {
				t.Error("agent container has an unmasked /proc")
			}

			present := map[string]bool{}
			for _, ic := range pod.Spec.InitContainers {
				present[ic.Name] = true
				isc := ic.SecurityContext
				if isc == nil {
					t.Errorf("init container %q has no SecurityContext (it would keep the runtime's default capabilities)", ic.Name)
					continue
				}
				if isc.Capabilities == nil || !dropsAll(isc.Capabilities) {
					t.Errorf("init container %q does not drop ALL: %+v", ic.Name, isc.Capabilities)
				}
				if isc.Privileged != nil && *isc.Privileged {
					t.Errorf("init container %q is privileged", ic.Name)
				}
				if isc.AllowPrivilegeEscalation == nil || *isc.AllowPrivilegeEscalation {
					t.Errorf("init container %q: AllowPrivilegeEscalation not false", ic.Name)
				}
				if isc.Capabilities != nil {
					for _, c := range isc.Capabilities.Add {
						if forbiddenPodCapabilities[c] {
							t.Errorf("init container %q adds %s", ic.Name, c)
						}
					}
				}
				if len(ic.VolumeDevices) != 0 {
					t.Errorf("init container %q has volume devices", ic.Name)
				}
			}
			for _, want := range tc.wantInit {
				if !present[want] {
					t.Errorf("init container %q missing (have %v)", want, present)
				}
			}
		})
	}
}

// dropsAll reports whether caps drops ALL.
func dropsAll(caps *corev1.Capabilities) bool {
	for _, c := range caps.Drop {
		if c == "ALL" {
			return true
		}
	}
	return false
}
