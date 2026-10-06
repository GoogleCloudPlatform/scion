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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestNFSHomeSubPaths(t *testing.T) {
	agentDir, home, err := NFSHomeSubPaths("projects", testHomeProjectID, "my-agent", testHomeAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/" + testHomeProjectID + "/agents/my-agent"; agentDir != want {
		t.Errorf("agentDir = %q, want %q", agentDir, want)
	}
	if want := agentDir + "/home-" + testHomeAgentID; home != want {
		t.Errorf("home = %q, want %q", home, want)
	}

	for _, tc := range []struct {
		name                         string
		root, project, slug, agentID string
		want                         string
	}{
		{"empty root", "", testHomeProjectID, "a", testHomeAgentID, "subpath root"},
		{"absolute root", "/projects", testHomeProjectID, "a", testHomeAgentID, "subpath root"},
		{"escaping root", "../x", testHomeProjectID, "a", testHomeAgentID, "subpath root"},
		{"unclean root", "a/../b", testHomeProjectID, "a", testHomeAgentID, "subpath root"},
		{"empty project", "projects", "", "a", testHomeAgentID, "project ID"},
		{"traversing project", "projects", "../other", "a", testHomeAgentID, "project ID"},
		{"project with slash", "projects", "a/b", "a", testHomeAgentID, "project ID"},
		{"empty slug", "projects", testHomeProjectID, "", testHomeAgentID, "agent slug"},
		{"slug with slash", "projects", testHomeProjectID, "a/b", testHomeAgentID, "agent slug"},
		{"slug not normalised", "projects", testHomeProjectID, "My Agent", testHomeAgentID, "agent slug"},
		{"traversing slug", "projects", testHomeProjectID, "..", testHomeAgentID, "agent slug"},
		{"empty agent ID", "projects", testHomeProjectID, "a", "", "agent ID"},
		{"agent ID is a name", "projects", testHomeProjectID, "a", "a", "agent ID"},
		{"upper-case agent ID", "projects", testHomeProjectID, "a", strings.ToUpper(testHomeAgentID), "agent ID"},
		{"traversing agent ID", "projects", testHomeProjectID, "a", "../9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10", "agent ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NFSHomeSubPaths(tc.root, tc.project, tc.slug, tc.agentID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one naming the %s", err, tc.want)
			}
		})
	}
}

// The token the init containers require is the one sciontool advertises
// for this contract.
func TestHomeFeatureToken_MatchesSciontool(t *testing.T) {
	if homeFeatureToken != homeprep.FeatureToken {
		t.Errorf("runtime requires %q, sciontool advertises %q", homeFeatureToken, homeprep.FeatureToken)
	}
}

func homeInitContainers(pod *corev1.Pod) []string {
	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

func containerEnv(c corev1.Container) map[string]string {
	out := map[string]string{}
	for _, e := range c.Env {
		out[e.Name] = e.Value
	}
	return out
}

func TestBuildPod_NFSHomeInitContainersAndMounts(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	agentDir := "projects/" + testHomeProjectID + "/agents/a"
	home := agentDir + "/home-" + testHomeAgentID

	if got := homeInitContainers(pod); !reflect.DeepEqual(got, []string{k8sHomeLeafContainer, k8sHomePrepareContainer}) {
		t.Fatalf("init containers = %v", got)
	}
	leaf, prep := pod.Spec.InitContainers[0], pod.Spec.InitContainers[1]

	// home-leaf: root with ownership capabilities only, mounting only the
	// agent directory.
	if sc := leaf.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 ||
		!reflect.DeepEqual(sc.Capabilities.Add, []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"}) ||
		!reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || *sc.AllowPrivilegeEscalation {
		t.Errorf("home-leaf security context = %+v", leaf.SecurityContext)
	}
	if !reflect.DeepEqual(leaf.VolumeMounts, []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: k8sHomeAgentDirMount, SubPath: agentDir}}) {
		t.Errorf("home-leaf mounts = %+v", leaf.VolumeMounts)
	}
	if e := containerEnv(leaf); e[homeAgentIDEnvVar] != testHomeAgentID || e[homeGIDEnvVar] != "1000" || len(e) != 2 {
		t.Errorf("home-leaf env = %v", e)
	}

	// home-prepare: the agent uid, no capabilities, the home and the
	// memory directory only.
	if sc := prep.SecurityContext; sc == nil || *sc.RunAsUser != containerUID || !*sc.RunAsNonRoot ||
		len(sc.Capabilities.Add) != 0 || *sc.AllowPrivilegeEscalation {
		t.Errorf("home-prepare security context = %+v", prep.SecurityContext)
	}
	wantPrepMounts := []corev1.VolumeMount{
		{Name: k8sHomeVolume, MountPath: k8sHomePrepareMount, SubPath: home},
		{Name: k8sMemVolume, MountPath: k8sMemDir},
	}
	if !reflect.DeepEqual(prep.VolumeMounts, wantPrepMounts) {
		t.Errorf("home-prepare mounts = %+v", prep.VolumeMounts)
	}
	pe := containerEnv(prep)
	if pe[homeAgentIDEnvVar] != testHomeAgentID || pe[homeStartIDEnvVar] != testHomeStartID ||
		pe[homeSkeletonSrcEnvVar] != "/home/scion" || pe[homeSkeletonMaxEnvVar] != "1024" {
		t.Errorf("home-prepare env = %v", pe)
	}
	var links []k8sHomeLink
	if err := json.Unmarshal([]byte(pe[homeLinksEnvVar]), &links); err != nil || !reflect.DeepEqual(links, rt.k8sHomeLinks(cfg)) {
		t.Errorf("home-prepare links = %s (%v)", pe[homeLinksEnvVar], err)
	}
	for _, c := range pod.Spec.InitContainers {
		if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
			t.Errorf("%s termination message policy = %q", c.Name, c.TerminationMessagePolicy)
		}
		if c.Image != cfg.Image {
			t.Errorf("%s image = %q", c.Name, c.Image)
		}
	}

	// The home volume, the agent container's home mount, the grace period.
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == k8sHomeVolume {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "home-pvc" {
		t.Errorf("home volume = %+v", vol)
	}
	assertOnlyHomeMount(t, pod, "/home/scion")
	if g := pod.Spec.TerminationGracePeriodSeconds; g == nil || *g != 30 {
		t.Errorf("grace = %v, want 30", g)
	}
	if p := pod.Spec.SecurityContext.FSGroupChangePolicy; p == nil || *p != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %v", p)
	}
}

func TestBuildPod_NFSHomeBrokerLeafAndSharedClaim(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	cfg.HomeStorage.Leaf = "broker"
	cfg.WorkspaceBackendName = "nfs"
	cfg.NFSPVClaimName = "home-pvc"
	cfg.NFSSubPath = "projects/" + testHomeProjectID + "/workspace"
	cfg.HomeStorage.StopGraceSeconds = 45
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	names := homeInitContainers(pod)
	if len(names) < 2 || names[0] != k8sHomePrepareContainer {
		t.Errorf("broker leaf mode: init containers = %v, want home-prepare first and no home-leaf", names)
	}
	for _, n := range names {
		if n == k8sHomeLeafContainer {
			t.Error("broker leaf mode must not add the home-leaf container")
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == k8sHomeVolume {
			t.Error("the home shares the workspace volume when the claims match")
		}
	}
	found := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == "/home/scion" && vm.Name == "workspace" {
			found = true
		}
	}
	if !found {
		t.Error("home not mounted from the workspace volume")
	}
	if g := pod.Spec.TerminationGracePeriodSeconds; g == nil || *g != 45 {
		t.Errorf("grace = %v, want 45", g)
	}

	cfg.HomeStorage.Leaf = "node"
	if _, err := rt.buildPod("default", cfg); err == nil || !strings.Contains(err.Error(), "leaf mode") {
		t.Errorf("unknown leaf mode: err = %v", err)
	}
	cfg.HomeStorage.Leaf = "pod"
	cfg.HomeStorage.AgentID = "bad"
	if _, err := rt.buildPod("default", cfg); err == nil {
		t.Error("an invalid agent ID must fail the pod build")
	}
	cfg = nfsHomeTestConfig(true)
	cfg.HomeStorage = nil
	if _, err := rt.buildPod("default", cfg); err == nil {
		t.Error("an NFS-home pod without a home description must fail")
	}
}

// The home settings reach the init containers only: the agent container's
// env does not depend on them and carries none of the home init variables.
func TestBuildPod_NFSHomeMainEnvUnchangedByHomeSettings(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	build := func(mutate func(*HomeStorageRealization)) []corev1.EnvVar {
		cfg := nfsHomeTestConfig(true)
		cfg.Name = "a"
		mutate(cfg.HomeStorage)
		pod, err := rt.buildPod("default", cfg)
		if err != nil {
			t.Fatal(err)
		}
		return pod.Spec.Containers[0].Env
	}
	base := build(func(*HomeStorageRealization) {})
	for _, mutate := range []func(*HomeStorageRealization){
		func(h *HomeStorageRealization) { h.Leaf = "broker" },
		func(h *HomeStorageRealization) { h.AgentID = "11111111-2222-4333-8444-555555555555" },
		func(h *HomeStorageRealization) {
			h.PVClaimName = "other"
			h.SkeletonMaxBytes = 5
			h.StopGraceSeconds = 60
		},
	} {
		got := build(mutate)
		a, _ := json.Marshal(base)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Errorf("agent container env changed with the home settings:\n%s\n%s", a, b)
		}
	}
	for _, e := range base {
		switch e.Name {
		case homeAgentIDEnvVar, homeStartIDEnvVar, homeGIDEnvVar, homeSkeletonSrcEnvVar, homeSkeletonMaxEnvVar, homeLinksEnvVar:
			t.Errorf("agent container env has %s", e.Name)
		}
	}
}

func TestCheckHomeMounts(t *testing.T) {
	home := "/home/scion"
	hs := &HomeStorageRealization{SubPathRoot: "p", ProjectID: "x", AgentSlug: "a", AgentID: testHomeAgentID}
	homeMount := corev1.VolumeMount{Name: k8sHomeVolume, MountPath: home, SubPath: "p/x/agents/a/home-" + testHomeAgentID}
	provision := corev1.VolumeMount{Name: k8sHomeVolume, MountPath: "/workspace", SubPath: "p/x/agents/a"}
	rules, err := newHomeMountRules(home, homeMount, hs, &provision)
	if err != nil {
		t.Fatal(err)
	}
	pod := func(agent []corev1.VolumeMount, init ...corev1.Container) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "agent", VolumeMounts: agent}},
			InitContainers: init,
		}}
	}
	leaf := corev1.Container{Name: k8sHomeLeafContainer, VolumeMounts: []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: k8sHomeAgentDirMount, SubPath: "p/x/agents/a"}}}
	ok := pod([]corev1.VolumeMount{homeMount, {Name: "workspace", MountPath: "/workspace"}, {Name: "scion-mem", MountPath: "/run/scion/mem"}},
		leaf,
		corev1.Container{Name: k8sHomePrepareContainer, VolumeMounts: []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: k8sHomePrepareMount, SubPath: homeMount.SubPath}}},
		corev1.Container{Name: "workspace-provision", VolumeMounts: []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: "/workspace", SubPath: "p/x/agents/a"}}})
	if err := checkHomeMounts(ok, rules); err != nil {
		t.Errorf("valid pod rejected: %v", err)
	}
	withSidecar := pod([]corev1.VolumeMount{homeMount})
	withSidecar.Spec.Containers = append(withSidecar.Spec.Containers, corev1.Container{Name: "sidecar", VolumeMounts: []corev1.VolumeMount{homeMount}})
	homeVol := func(sub string) corev1.VolumeMount {
		return corev1.VolumeMount{Name: k8sHomeVolume, MountPath: "/mnt", SubPath: sub}
	}
	for name, p := range map[string]*corev1.Pod{
		"another volume at the home":      pod([]corev1.VolumeMount{homeMount, {Name: "gcs-vol-0", MountPath: "/home/scion"}}),
		"another volume in the home":      pod([]corev1.VolumeMount{homeMount, {Name: "gcs-vol-0", MountPath: "/home/scion/cache"}}),
		"unclean path in the home":        pod([]corev1.VolumeMount{homeMount, {Name: "v", MountPath: "/home/scion/./x/.."}}),
		"home volume with other path":     pod([]corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: home, SubPath: "other"}}),
		"home mounted twice":              pod([]corev1.VolumeMount{homeMount, homeMount}),
		"home missing":                    pod([]corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}}),
		"init container in the home":      pod([]corev1.VolumeMount{homeMount}, corev1.Container{Name: "x", VolumeMounts: []corev1.VolumeMount{{Name: "w", MountPath: "/home/scion/.ssh"}}}),
		"init container at the home":      pod([]corev1.VolumeMount{homeMount}, corev1.Container{Name: "x", VolumeMounts: []corev1.VolumeMount{homeMount}}),
		"home mount on a sidecar":         withSidecar,
		"home volume at export root":      pod([]corev1.VolumeMount{homeMount, homeVol("")}),
		"home volume at slash":            pod([]corev1.VolumeMount{homeMount, homeVol("/")}),
		"home volume at subpath root":     pod([]corev1.VolumeMount{homeMount, homeVol("p")}),
		"home volume at project":          pod([]corev1.VolumeMount{homeMount, homeVol("p/x")}),
		"home volume at agents":           pod([]corev1.VolumeMount{homeMount, homeVol("p/x/agents/")}),
		"agent dir on the agent":          pod([]corev1.VolumeMount{homeMount, homeVol("p/x/agents/a")}),
		"agent dir on another init":       pod([]corev1.VolumeMount{homeMount}, corev1.Container{Name: k8sHomePrepareContainer, VolumeMounts: []corev1.VolumeMount{homeVol("p/x/agents/a")}}),
		"export root on an init":          pod([]corev1.VolumeMount{homeMount}, corev1.Container{Name: k8sHomeLeafContainer, VolumeMounts: []corev1.VolumeMount{homeVol("")}}),
		"provision with another path":     pod([]corev1.VolumeMount{homeMount}, corev1.Container{Name: k8sWorkspaceProvisionContainer, VolumeMounts: []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: "/other", SubPath: "p/x/agents/a"}}}),
		"another agent's dir":             pod([]corev1.VolumeMount{homeMount, homeVol("p/x/agents/b")}),
		"another agent's home":            pod([]corev1.VolumeMount{homeMount, homeVol("p/x/agents/b/home-" + testHomeAgentID)}),
		"another agent's dir on leaf":     pod([]corev1.VolumeMount{homeMount}, corev1.Container{Name: k8sHomeLeafContainer, VolumeMounts: []corev1.VolumeMount{homeVol("p/x/agents/b")}}),
		"sibling slug extending this one": pod([]corev1.VolumeMount{homeMount, homeVol("p/x/agents/ab")}),
		"sibling slug's home":             pod([]corev1.VolumeMount{homeMount, homeVol("p/x/agents/ab/home-" + testHomeAgentID)}),
		"another project":                 pod([]corev1.VolumeMount{homeMount, homeVol("p/y")}),
		"another project's agent":         pod([]corev1.VolumeMount{homeMount, homeVol("p/y/agents/a")}),
	} {
		if err := checkHomeMounts(p, rules); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuildPod_NFSHomeRejectsVolumeAtHome(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := withTestHomeStorage(RunConfig{Name: "a", Image: "i", UnixUsername: "scion",
		Volumes: []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/home/scion"}}})
	if _, err := rt.buildPod("default", cfg); err == nil || !strings.Contains(err.Error(), "gcs-vol-0") {
		t.Errorf("a volume at the home path must be rejected, naming it: %v", err)
	}
}

// The init command runs the sciontool command only when the image's
// sciontool advertises the token, and otherwise fails with exit 3 and a
// message naming the missing support.
func TestHomeInitCommand_FeatureGuard(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tc := range []struct {
		name     string
		features string
		wantCode int
		wantOut  string
	}{
		{"supported", "other\nhome-v1\n", 0, "ran home prepare --home /scion-home"},
		{"older sciontool", "other\n", 3, "image sciontool lacks NFS-home support (needs home-v1)"},
		{"prefix only", "home-v10\n", 3, "lacks NFS-home support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then printf '%s'; exit 0; fi\necho ran \"$@\"\n", strings.ReplaceAll(tc.features, "\n", "\\n"))
			if err := os.WriteFile(filepath.Join(bin, "sciontool"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := homeInitCommand("sciontool", "home", "prepare", "--home", "/scion-home")
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			out, err := c.CombinedOutput()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.wantCode || !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("exit %d, output %q; want exit %d and %q", code, out, tc.wantCode, tc.wantOut)
			}
		})
	}
}

func TestParseK8sHomeMode(t *testing.T) {
	if m, err := parseK8sHomeMode(`{"mode":"seed","start_id":"s1"}`, "s1"); err != nil || m != "seed" {
		t.Errorf("seed: %q %v", m, err)
	}
	if m, err := parseK8sHomeMode(`{"mode":"seed-over","start_id":"s1"}`, "s1"); err != nil || m != "seed-over" {
		t.Errorf("seed-over: %q %v", m, err)
	}
	for _, bad := range []string{``, `{`, `{"mode":"seed","start_id":"other"}`, `{"mode":"refresh","start_id":"s1"}`, `{"start_id":"s1"}`} {
		if _, err := parseK8sHomeMode(bad, "s1"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHomeKeptOnExport(t *testing.T) {
	if !homeKeptOnExport(map[string]string{k8sHomeStorageAnnotation: "nfs"}) {
		t.Error("NFS-home pod not recognised")
	}
	if homeKeptOnExport(map[string]string{"scion.homedir": "/x"}) || homeKeptOnExport(nil) {
		t.Error("plain pod treated as NFS-home")
	}
}

func boundPod(statuses func(p *corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		Spec: corev1.PodSpec{
			NodeName:            "node-1",
			InitContainers:      []corev1.Container{{Name: "home-prepare"}},
			Containers:          []corev1.Container{{Name: "agent"}},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}},
		},
	}
	if statuses != nil {
		statuses(p)
	}
	return p
}

var (
	stTerminated = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	stRunning    = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
)

func TestPodTerminationConfirmed(t *testing.T) {
	all := func(p *corev1.Pod) {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "home-prepare", State: stTerminated}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: stTerminated}}
		p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", State: stTerminated}}
	}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"never bound", &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}}, true},
		{"empty status", boundPod(nil), false},
		{"all terminated", boundPod(all), true},
		{"main running", boundPod(func(p *corev1.Pod) { all(p); p.Status.ContainerStatuses[0].State = stRunning }), false},
		{"ephemeral running", boundPod(func(p *corev1.Pod) { all(p); p.Status.EphemeralContainerStatuses[0].State = stRunning }), false},
		{"ephemeral status missing", boundPod(func(p *corev1.Pod) { all(p); p.Status.EphemeralContainerStatuses = nil }), false},
		{"init status missing", boundPod(func(p *corev1.Pod) { all(p); p.Status.InitContainerStatuses = nil }), false},
	} {
		if got := podTerminationConfirmed(tc.pod); got != tc.want {
			t.Errorf("%s: confirmed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPodNodeLost(t *testing.T) {
	cond := func(c ...corev1.PodCondition) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: c}}
	}
	if !podNodeLost(cond(corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue})) {
		t.Error("DisruptionTarget true not detected")
	}
	if !podNodeLost(cond(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "NodeNotReady"})) {
		t.Error("NodeNotReady not detected")
	}
	if podNodeLost(cond(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"})) ||
		podNodeLost(cond(corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionFalse})) || podNodeLost(cond()) {
		t.Error("node loss reported without a node-loss condition")
	}
}

// fakeTerminationClock is a fake clock whose sleep advances time and runs
// onTick, so a test can change the pod at a given fake time.
type fakeTerminationClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps int
	onTick func(now time.Time)
}

func (c *fakeTerminationClock) clock() execReadyClock {
	return execReadyClock{
		now: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now },
		sleep: func(ctx context.Context, d time.Duration) error {
			c.mu.Lock()
			c.now = c.now.Add(d)
			c.sleeps++
			now := c.now
			c.mu.Unlock()
			if c.onTick != nil {
				c.onTick(now)
			}
			return ctx.Err()
		},
	}
}

// keepPodsOnDelete makes the fake clientset accept pod deletes without
// removing the pod (as a delete with a grace period does) and records the
// options.
func keepPodsOnDelete(cs *k8sfake.Clientset) *[]metav1.DeleteOptions {
	var mu sync.Mutex
	var seen []metav1.DeleteOptions
	cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, a.(k8stesting.DeleteActionImpl).DeleteOptions)
		return true, nil, nil
	})
	return &seen
}

func runningNFSHomePod(name string) *corev1.Pod {
	grace := int64(30)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "uid-1",
			Annotations: map[string]string{k8sHomeStorageAnnotation: HomeStorageNFS}},
		Spec: corev1.PodSpec{NodeName: "node-1", TerminationGracePeriodSeconds: &grace,
			Containers: []corev1.Container{{Name: "agent"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: stRunning}}},
	}
}

// A graceful delete leaves the pod's container running for a while; the
// start waits until it is confirmed stopped, well inside the hub's 90 s
// window even when the old pod uses its whole grace period.
func TestCleanupStalePod_WaitsForConfirmedTermination(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	fc.onTick = func(now time.Time) {
		if now.Sub(start) >= 30*time.Second {
			p, _ := cs.CoreV1().Pods("default").Get(context.Background(), "a", metav1.GetOptions{})
			if p != nil && p.Status.ContainerStatuses[0].State.Terminated == nil {
				p.Status.ContainerStatuses[0].State = stTerminated
				_, _ = cs.CoreV1().Pods("default").UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
			}
		}
	}
	rt.execReadyClock = fc.clock()

	if err := rt.cleanupStalePod(context.Background(), "default", "a", true, &HomeStorageRealization{TerminationWaitSeconds: 15}); err != nil {
		t.Fatalf("cleanupStalePod: %v", err)
	}
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds != nil {
		t.Errorf("delete options = %+v, want one delete with the pod's own grace", *deletes)
	}
	if waited := fc.now.Sub(start); waited < 30*time.Second || waited > 90*time.Second {
		t.Errorf("waited %s, want the old pod's shutdown time and less than 90s", waited)
	}
}

func TestCleanupStalePod_RefusesWhenUnconfirmed(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	rt.execReadyClock = fc.clock()
	err := rt.cleanupStalePod(context.Background(), "default", "a", true, &HomeStorageRealization{TerminationWaitSeconds: 15})
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Fatalf("err = %v, want previous_pod_unconfirmed", err)
	}
	if waited := fc.now.Sub(start); waited < 45*time.Second || waited > 47*time.Second {
		t.Errorf("refused after %s, want the 45s bound (grace 30 + wait 15)", waited)
	}
	if !strings.Contains(err.Error(), "previous_pod_unconfirmed") {
		t.Errorf("error text = %q", err)
	}
}

func TestCleanupStalePod_NodeLostRefusedAtOnce(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	p := runningNFSHomePod("a")
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}}
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	keepPodsOnDelete(cs)
	fc := &fakeTerminationClock{now: time.Unix(1000, 0)}
	rt.execReadyClock = fc.clock()
	err := rt.cleanupStalePod(context.Background(), "default", "a", true, nil)
	if !errors.Is(err, errPreviousPodUnconfirmed) || fc.sleeps != 0 {
		t.Errorf("err = %v after %d sleeps, want an immediate refusal", err, fc.sleeps)
	}
}

func TestCleanupStalePod_PlainPodUnchanged(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	p := runningNFSHomePod("a")
	p.Annotations = nil
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)
	fc := &fakeTerminationClock{now: time.Unix(1000, 0)}
	rt.execReadyClock = fc.clock()
	if err := rt.cleanupStalePod(context.Background(), "default", "a", false, nil); err != nil {
		t.Fatal(err)
	}
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds == nil || *(*deletes)[0].GracePeriodSeconds != 0 || fc.sleeps != 0 {
		t.Errorf("plain pod: deletes %+v, sleeps %d; want one immediate delete and no wait", *deletes, fc.sleeps)
	}
	if err := rt.cleanupStalePod(context.Background(), "default", "missing", false, nil); err != nil {
		t.Errorf("missing pod: %v", err)
	}
}

// Delete and Stop never force-delete an NFS-home pod and return without
// waiting; other pods are still deleted immediately.
func TestDelete_GracefulForNFSHomePods(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nfs       bool
		wantGrace bool
	}{{"nfs home", true, true}, {"plain", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			rt, cs, _ := newTestK8sRuntime()
			p := runningNFSHomePod("a")
			if !tc.nfs {
				p.Annotations = nil
			}
			if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			deletes := keepPodsOnDelete(cs)
			done := make(chan error, 1)
			go func() { done <- rt.Stop(context.Background(), "default/a") }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Stop waited for the pod to terminate")
			}
			if len(*deletes) != 1 {
				t.Fatalf("deletes = %+v", *deletes)
			}
			graceful := (*deletes)[0].GracePeriodSeconds == nil
			if graceful != tc.wantGrace {
				t.Errorf("graceful = %v, want %v", graceful, tc.wantGrace)
			}
		})
	}
}

func TestCleanupStartResources_GracefulForNFSHomePods(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	p := runningNFSHomePod("a")
	p.Labels = map[string]string{labelStartID: "s1"}
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)
	rt.cleanupStartResources(context.Background(), "default", "a", "s1", true)
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds != nil || (*deletes)[0].Preconditions == nil {
		t.Errorf("deletes = %+v, want one graceful delete with a UID precondition", *deletes)
	}
}

// A second start of the same NFS-home agent while one holds the lock is
// refused with a retryable error before anything is deleted or created.
func TestRun_NFSHomeStartLock(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	cfg.Locker = &alwaysLoseLocker{}
	deletes := keepPodsOnDelete(cs)
	_, err := rt.Run(context.Background(), cfg)
	if !errors.Is(err, errAgentStartInProgress) {
		t.Fatalf("err = %v, want agent_start_in_progress", err)
	}
	if len(*deletes) != 0 {
		t.Error("a refused start deleted a pod")
	}
	if pods, _ := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Error("a refused start created a pod")
	}

	l := newTestLocker()
	release, err := acquireHomeStartLock(context.Background(), RunConfig{Name: "a", Locker: l, HomeStorage: cfg.HomeStorage})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireHomeStartLock(context.Background(), RunConfig{Name: "a", Locker: l, HomeStorage: cfg.HomeStorage}); !errors.Is(err, errAgentStartInProgress) {
		t.Errorf("second acquire: %v", err)
	}
	release()
	again, err := acquireHomeStartLock(context.Background(), RunConfig{Name: "a", Locker: l, HomeStorage: cfg.HomeStorage})
	if err != nil {
		t.Errorf("after release: %v", err)
	} else {
		again()
	}
}

// The start of an NFS-home pod reads the mode home-prepare chose; a seed is
// marked seeded after the transfer and before the startup gate, a seed-over
// is not, and a missing or foreign mode file stops the start before the
// transfer.
func TestRun_NFSHomeModeAndMarkSeeded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     func(startID string) string
		wantMark bool
		wantErr  string
	}{
		{"seed", func(s string) string { return `{"mode":"seed","start_id":"` + s + `"}` }, true, ""},
		{"seed-over", func(s string) string { return `{"mode":"seed-over","start_id":"` + s + `"}` }, false, ""},
		{"other start", func(string) string { return `{"mode":"seed","start_id":"old"}` }, false, "not this start"},
		{"no mode file", func(string) string { return "" }, false, "home-mode.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			rt.execProbe = func(context.Context, string, string) error { return nil }
			var mu sync.Mutex
			var steps []string
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				joined := strings.Join(cmd, " ")
				mu.Lock()
				steps = append(steps, joined)
				mu.Unlock()
				if joined == "cat "+k8sHomeModeFile {
					pod, err := clientset.CoreV1().Pods("default").Get(context.Background(), "nfs-agent", metav1.GetOptions{})
					if err != nil {
						return "", err
					}
					return tc.mode(pod.Labels[labelStartID]), nil
				}
				return "", nil
			}
			rt.homeSync = func(context.Context, string, string, string, string, []string) error {
				mu.Lock()
				steps = append(steps, "home-sync")
				mu.Unlock()
				return nil
			}
			adc := filepath.Join(t.TempDir(), "adc.json")
			if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := nfsHomeTestConfig(true)
			config.Name = "nfs-agent"
			config.HomeStorage.AgentSlug = "nfs-agent"
			config.Labels = nil
			config.Harness = &MockHarness{}
			config.ResolvedAuth.Files[0].SourcePath = adc
			config.HomeDir = t.TempDir()

			runErr := startFakeK8sPod(t, rt, clientset, config)
			mu.Lock()
			defer mu.Unlock()
			idx := func(prefix string) int {
				for i, s := range steps {
					if strings.HasPrefix(s, prefix) {
						return i
					}
				}
				return -1
			}
			if tc.wantErr != "" {
				if runErr == nil || !strings.Contains(runErr.Error(), tc.wantErr) {
					t.Fatalf("Run error = %v, want %q", runErr, tc.wantErr)
				}
				if idx("home-sync") >= 0 {
					t.Error("home transferred despite a bad mode file")
				}
				return
			}
			if runErr != nil {
				t.Fatalf("Run: %v (steps %q)", runErr, steps)
			}
			guard := idx("sh -c for p in")
			mark, sync, gate := idx("sciontool home mark-seeded"), idx("home-sync"), idx("touch /tmp/.scion-home-ready")
			if guard < 0 || guard > sync {
				t.Errorf("hooks guard must run before the transfer: guard %d, sync %d", guard, sync)
			}
			if tc.wantMark != (mark >= 0) {
				t.Fatalf("mark-seeded run = %v, want %v (steps %q)", mark >= 0, tc.wantMark, steps)
			}
			if tc.wantMark && (sync >= mark || mark >= gate) {
				t.Errorf("order: sync %d, mark %d, gate %d", sync, mark, gate)
			}
			if tc.wantMark && !strings.Contains(steps[mark], "--agent-id "+testHomeAgentID) {
				t.Errorf("mark-seeded command = %q", steps[mark])
			}
			for _, s := range steps {
				if strings.HasPrefix(s, "chown -R") {
					t.Errorf("recursive chown over the NFS home: %q", s)
				}
			}
		})
	}
}

// The seed-over transfer is refused when .scion or .scion/hooks is a
// symbolic link, so hook scripts are never written through a link.
func TestK8sHomeHooksGuardCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(home string) (string, error) {
		cmd := k8sHomeHooksGuardCommand(home)
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		return string(out), err
	}
	home := t.TempDir()
	if out, err := run(home); err != nil {
		t.Errorf("empty home refused: %v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(home, ".scion", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(home); err != nil {
		t.Errorf("plain .scion/hooks refused: %v %s", err, out)
	}
	for _, linked := range []string{".scion/hooks", ".scion"} {
		h := t.TempDir()
		target := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(h, linked)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(h, linked)); err != nil {
			t.Fatal(err)
		}
		out, err := run(h)
		if err == nil || !strings.Contains(out, "is a symbolic link") {
			t.Errorf("%s linked: err %v, output %q", linked, err, out)
		}
	}
}

// NFS-home starts: the harness bundle's secrets and outputs are left out
// of the home transfer, and the secrets are staged in memory only, with a
// separate transfer into SCION_HARNESS_SECRETS_DIR before the startup gate.
func TestRun_NFSHomeHarnessSecretsStagedInMemory(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	rt.execProbe = func(context.Context, string, string) error { return nil }
	type transfer struct {
		src, dest string
		excludes  []string
	}
	var mu sync.Mutex
	var steps []string
	var transfers []transfer
	rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
		joined := strings.Join(cmd, " ")
		mu.Lock()
		steps = append(steps, joined)
		mu.Unlock()
		if joined == "cat "+k8sHomeModeFile {
			pod, err := clientset.CoreV1().Pods("default").Get(context.Background(), "nfs-agent", metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			return `{"mode":"seed-over","start_id":"` + pod.Labels[labelStartID] + `"}`, nil
		}
		return "", nil
	}
	rt.homeSync = func(_ context.Context, _, _, src, dest string, ex []string) error {
		mu.Lock()
		transfers = append(transfers, transfer{src, dest, ex})
		steps = append(steps, "transfer "+dest)
		mu.Unlock()
		return nil
	}
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := nfsHomeTestConfig(true)
	config.Name = "nfs-agent"
	config.HomeStorage.AgentSlug = "nfs-agent"
	config.Labels = nil
	config.Harness = &MockHarness{}
	config.ResolvedAuth.Files[0].SourcePath = adc
	config.HomeDir = t.TempDir()
	secrets := filepath.Join(config.HomeDir, ".scion", "harness", "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "API_KEY"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := startFakeK8sPod(t, rt, clientset, config); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(transfers) != 2 {
		t.Fatalf("transfers = %+v, want the home and the in-memory secrets", transfers)
	}
	home, mem := transfers[0], transfers[1]
	if home.src != config.HomeDir || home.dest != "/home/scion" {
		t.Errorf("home transfer = %+v", home)
	}
	for _, ex := range []string{".scion/harness/secrets", ".scion/harness/outputs"} {
		found := false
		for _, e := range home.excludes {
			if e == ex {
				found = true
			}
		}
		if !found {
			t.Errorf("home transfer does not exclude %s: %v", ex, home.excludes)
		}
	}
	if mem.src != secrets || mem.dest != k8sHarnessSecretsDir {
		t.Errorf("secrets transfer = %+v, want %s to %s", mem, secrets, k8sHarnessSecretsDir)
	}
	idx := func(prefix string) int {
		for i, s := range steps {
			if strings.HasPrefix(s, prefix) {
				return i
			}
		}
		return -1
	}
	mk, tr, gate := -1, idx("transfer "+k8sHarnessSecretsDir), idx("touch /tmp/.scion-home-ready")
	for i, s := range steps {
		if s == "sh -c mkdir -p /run/scion/mem/harness-secrets && chmod 0700 /run/scion/mem/harness-secrets" {
			mk = i
		}
	}
	if mk < 0 || mk > tr || tr > gate {
		t.Errorf("order: mkdir %d, secrets transfer %d, gate %d", mk, tr, gate)
	}
}

// The home transfer of an NFS-home pod never includes the harness bundle's
// secrets or outputs, whatever the links are.
func TestNFSHomeSyncExcludes(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	for _, cfg := range []RunConfig{nfsHomeTestConfig(true), withTestHomeStorage(RunConfig{Name: "a", Image: "i", UnixUsername: "scion"})} {
		ex := rt.nfsHomeSyncExcludes(cfg)
		want := map[string]bool{".scion/harness/secrets": false, ".scion/harness/outputs": false}
		for _, e := range ex {
			if _, ok := want[e]; ok {
				want[e] = true
			}
		}
		for k, v := range want {
			if !v {
				t.Errorf("excludes %v lack %s", ex, k)
			}
		}
	}
}

// An NFS-home start never force-deletes a previous pod it could not read.
func TestCleanupStalePod_UnreadablePodRefused(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	err := rt.cleanupStalePod(context.Background(), "default", "a", true, &HomeStorageRealization{})
	if !errors.Is(err, errPreviousPodUnconfirmed) || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("err = %v, want previous_pod_unconfirmed wrapping the read error", err)
	}
	if len(*deletes) != 0 {
		t.Errorf("deletes = %+v, want none", *deletes)
	}
}

// For an NFS-home start the previous pod's secrets are removed only after
// the pod is confirmed stopped.
func TestRun_NFSHomeSecretsCleanedAfterPreviousPod(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	keepPodsOnDelete(cs)
	fc := &fakeTerminationClock{now: time.Unix(1000, 0)}
	rt.execReadyClock = fc.clock()
	_, err := rt.Run(context.Background(), cfg)
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Fatalf("err = %v, want previous_pod_unconfirmed", err)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" && (a.GetVerb() == "delete" || a.GetVerb() == "delete-collection" || a.GetVerb() == "list") {
			t.Errorf("secrets touched (%s) before the previous pod was confirmed stopped", a.GetVerb())
		}
	}
}

// countingLocker records acquires and releases.
type countingLocker struct {
	mu                 sync.Mutex
	acquires, releases int
	acquire            bool
}

func (l *countingLocker) TryAdvisoryLock(context.Context, store.AdvisoryLockKey) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func (l *countingLocker) TryAdvisoryLockObject(context.Context, store.AdvisoryLockKey, int32) (bool, func() error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.acquire {
		return false, func() error { return nil }, nil
	}
	l.acquires++
	return true, func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releases++
		return nil
	}, nil
}

// The per-agent start lock, when a locker is supplied, is released exactly
// once: on a refusal after it was taken, on a pod build failure, and after
// the pod is created.
func TestRun_NFSHomeStartLockReleasedOnce(t *testing.T) {
	t.Run("previous pod unconfirmed", func(t *testing.T) {
		rt, cs, _ := newTestK8sRuntime()
		cfg := nfsHomeTestConfig(true)
		cfg.Name = "a"
		l := &countingLocker{acquire: true}
		cfg.Locker = l
		if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		keepPodsOnDelete(cs)
		rt.execReadyClock = (&fakeTerminationClock{now: time.Unix(1000, 0)}).clock()
		if _, err := rt.Run(context.Background(), cfg); !errors.Is(err, errPreviousPodUnconfirmed) {
			t.Fatalf("err = %v", err)
		}
		if l.acquires != 1 || l.releases != 1 {
			t.Errorf("acquires %d, releases %d; want 1 and 1", l.acquires, l.releases)
		}
	})
	t.Run("pod build failure", func(t *testing.T) {
		rt, _, _ := newTestK8sRuntime()
		cfg := nfsHomeTestConfig(true)
		cfg.Name = "a"
		cfg.HomeStorage.Leaf = "unknown"
		l := &countingLocker{acquire: true}
		cfg.Locker = l
		if _, err := rt.Run(context.Background(), cfg); err == nil {
			t.Fatal("expected a pod build failure")
		}
		if l.acquires != 1 || l.releases != 1 {
			t.Errorf("acquires %d, releases %d; want 1 and 1", l.acquires, l.releases)
		}
	})
	t.Run("pod created", func(t *testing.T) {
		rt, clientset, _ := newTestK8sRuntime()
		rt.execProbe = func(context.Context, string, string) error { return nil }
		l := &countingLocker{acquire: true}
		rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
			if strings.Join(cmd, " ") == "cat "+k8sHomeModeFile {
				l.mu.Lock()
				released := l.releases
				l.mu.Unlock()
				if released != 1 {
					t.Errorf("lock not released once the pod exists (releases %d)", released)
				}
				pod, err := clientset.CoreV1().Pods("default").Get(context.Background(), "nfs-agent", metav1.GetOptions{})
				if err != nil {
					return "", err
				}
				return `{"mode":"seed-over","start_id":"` + pod.Labels[labelStartID] + `"}`, nil
			}
			return "", nil
		}
		rt.homeSync = func(context.Context, string, string, string, string, []string) error { return nil }
		adc := filepath.Join(t.TempDir(), "adc.json")
		if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := nfsHomeTestConfig(true)
		cfg.Name = "nfs-agent"
		cfg.HomeStorage.AgentSlug = "nfs-agent"
		cfg.Labels = nil
		cfg.Harness = &MockHarness{}
		cfg.ResolvedAuth.Files[0].SourcePath = adc
		cfg.HomeDir = t.TempDir()
		cfg.Locker = l
		if err := startFakeK8sPod(t, rt, clientset, cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if l.acquires != 1 || l.releases != 1 {
			t.Errorf("acquires %d, releases %d; want 1 and 1", l.acquires, l.releases)
		}
	})
}

// Sync of an NFS-home agent copies the workspace only, in both directions,
// and says that the home is not synced.
func TestSync_NFSHomePodSkipsHome(t *testing.T) {
	for _, dir := range []SyncDirection{SyncTo, SyncFrom} {
		t.Run(string(dir), func(t *testing.T) {
			rt, cs, _ := newTestK8sRuntime()
			pod := newFakeAgentPodWithHomeDirAnnotation("test-agent", "/some/agent/home")
			pod.Annotations[k8sHomeStorageAnnotation] = HomeStorageNFS
			if _, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			var copied []string
			rt.syncTransfer = func(_ context.Context, _, _, src, dest string, _ bool) error {
				copied = append(copied, src+" -> "+dest)
				return nil
			}
			out := captureStdout(t, func() {
				if err := rt.Sync(context.Background(), "test-agent", dir); err != nil {
					t.Fatalf("Sync: %v", err)
				}
			})
			if len(copied) != 1 || !strings.Contains(copied[0], "/workspace") {
				t.Errorf("copies = %v, want the workspace only", copied)
			}
			if !strings.Contains(out, "Agent home is kept on persistent storage; not syncing it.") {
				t.Errorf("no notice in %q", out)
			}

			// A plain pod still syncs its home.
			rt2, cs2, _ := newTestK8sRuntime()
			plain := newFakeAgentPodWithHomeDirAnnotation("test-agent", "/some/agent/home")
			if _, err := cs2.CoreV1().Pods("default").Create(context.Background(), plain, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			copied = nil
			rt2.syncTransfer = rt.syncTransfer
			captureStdout(t, func() {
				if err := rt2.Sync(context.Background(), "test-agent", dir); err != nil {
					t.Fatalf("Sync: %v", err)
				}
			})
			if len(copied) != 2 {
				t.Errorf("plain pod copies = %v, want workspace and home", copied)
			}
		})
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	defer func() { os.Stdout = old }()
	f()
	_ = w.Close()
	return <-done
}

// With a shared mode (no clone-per-agent provisioning mount), the
// provisioning container may not mount the agent directory; a two-component
// subpath root forbids each of its ancestors.
func TestCheckHomeMounts_SharedModeAndNestedRoot(t *testing.T) {
	home := "/home/scion"
	hs := &HomeStorageRealization{SubPathRoot: "a/b", ProjectID: "x", AgentSlug: "s", AgentID: testHomeAgentID}
	homeMount := corev1.VolumeMount{Name: k8sHomeVolume, MountPath: home, SubPath: "a/b/x/agents/s/home-" + testHomeAgentID}
	rules, err := newHomeMountRules(home, homeMount, hs, nil)
	if err != nil {
		t.Fatal(err)
	}
	pod := func(extra ...corev1.Container) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "agent", VolumeMounts: []corev1.VolumeMount{homeMount}}},
			InitContainers: extra,
		}}
	}
	if err := checkHomeMounts(pod(), rules); err != nil {
		t.Fatalf("valid pod rejected: %v", err)
	}
	provision := corev1.Container{Name: k8sWorkspaceProvisionContainer, VolumeMounts: []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: "/workspace", SubPath: "a/b/x/agents/s"}}}
	if err := checkHomeMounts(pod(provision), rules); err == nil {
		t.Error("shared mode: the provisioning container mounting the agent directory was accepted")
	}
	for _, sub := range []string{"a", "a/b", "a/b/x", "a/b/x/agents", "a/b/x/agents/other", "a/b/x/agents/sx", "a/b/y", "a/b/y/agents/s"} {
		c := corev1.Container{Name: k8sHomeLeafContainer, VolumeMounts: []corev1.VolumeMount{{Name: k8sHomeVolume, MountPath: "/mnt", SubPath: sub}}}
		if err := checkHomeMounts(pod(c), rules); err == nil {
			t.Errorf("subPath %q accepted", sub)
		}
	}
}

// A clone-per-agent workspace on the same claim as the home builds: the
// provisioning container's own mount of the agent directory is accepted.
func TestBuildPod_NFSHomeClonePerAgentSharedClaim(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	cfg.WorkspaceBackendName = "nfs"
	cfg.NFSPVClaimName = "home-pvc"
	cfg.NFSSubPath = "projects/" + testHomeProjectID + "/workspace"
	cfg.NFSAgentDirName = "a"
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	found := false
	for _, c := range pod.Spec.InitContainers {
		if c.Name == k8sWorkspaceProvisionContainer {
			for _, vm := range c.VolumeMounts {
				if vm.SubPath == "projects/"+testHomeProjectID+"/agents/a" && vm.MountPath == "/workspace" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("provisioning container does not mount the agent directory: %+v", pod.Spec.InitContainers)
	}
}

// For an NFS-home start the previous pod's Secrets are deleted after the
// pod is confirmed stopped and before the new pod is created.
func TestRun_NFSHomeSecretDeleteBetweenConfirmAndCreate(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ResolvedAuth.Files[0].SourcePath = adc
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	gone := false
	fc.onTick = func(now time.Time) {
		if !gone && now.Sub(start) >= 20*time.Second {
			gone = true
			_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "default", "a")
		}
	}
	rt.execReadyClock = fc.clock()
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("stop here")
	})
	if _, err := rt.Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("Run: %v", err)
	}
	lastPodGet, secretDelete, podCreate := -1, -1, -1
	for i, a := range cs.Actions() {
		switch {
		case a.GetResource().Resource == "pods" && a.GetVerb() == "get":
			lastPodGet = i
		case a.GetResource().Resource == "secrets" && a.GetVerb() == "delete" && secretDelete < 0:
			secretDelete = i
		case a.GetResource().Resource == "pods" && a.GetVerb() == "create":
			podCreate = i
		}
	}
	if secretDelete < 0 || lastPodGet >= secretDelete || secretDelete >= podCreate {
		t.Errorf("order: last pod read %d, secret delete %d, pod create %d", lastPodGet, secretDelete, podCreate)
	}
}

// podHandle is the launch resource handle of pod p.
func podHandle(p *corev1.Pod) api.ResourceHandle {
	return api.ResourceHandle{Kind: api.ResourceKindPod, Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)}
}

// wantUIDPrecondition fails unless opts carry a UID precondition for uid.
func wantUIDPrecondition(t *testing.T, opts metav1.DeleteOptions, uid string) {
	t.Helper()
	if opts.Preconditions == nil || opts.Preconditions.UID == nil || string(*opts.Preconditions.UID) != uid {
		t.Errorf("delete preconditions = %+v, want UID %q", opts.Preconditions, uid)
	}
}

// Cleanup of a cancelled launch deletes an NFS-home pod with its grace
// period and a UID precondition, never with grace 0, so the pod is still
// there while it shuts down and the agent's next start waits for it.
func TestK8sDeleteResource_NFSHomePodGraceful(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	p := runningNFSHomePod("a")
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)

	// A handle with a stale UID (the name now belongs to another pod)
	// leaves the pod alone: any delete issued must carry the stale UID as
	// its precondition, which the API server rejects, and never grace 0.
	stale := podHandle(p)
	stale.UID = "uid-stale"
	if err := rt.DeleteResource(context.Background(), stale); err != nil {
		t.Fatalf("stale DeleteResource: %v", err)
	}
	for i, d := range *deletes {
		wantUIDPrecondition(t, d, "uid-stale")
		if d.GracePeriodSeconds != nil && *d.GracePeriodSeconds == 0 {
			t.Errorf("stale delete %d used grace 0: %+v", i, d)
		}
	}
	if _, err := cs.CoreV1().Pods("default").Get(context.Background(), "a", metav1.GetOptions{}); err != nil {
		t.Fatalf("pod removed by a stale handle: %v", err)
	}
	*deletes = nil

	if err := rt.DeleteResource(context.Background(), podHandle(p)); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if len(*deletes) != 1 {
		t.Fatalf("deletes = %+v, want exactly one", *deletes)
	}
	if g := (*deletes)[0].GracePeriodSeconds; g != nil {
		t.Errorf("grace period = %d, want the pod's own (unset)", *g)
	}
	wantUIDPrecondition(t, (*deletes)[0], "uid-1")

	// The next start finds the pod still shutting down and waits until its
	// container is confirmed stopped.
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	fc.onTick = func(now time.Time) {
		if now.Sub(start) >= 20*time.Second {
			p, _ := cs.CoreV1().Pods("default").Get(context.Background(), "a", metav1.GetOptions{})
			if p != nil && p.Status.ContainerStatuses[0].State.Terminated == nil {
				p.Status.ContainerStatuses[0].State = stTerminated
				_, _ = cs.CoreV1().Pods("default").UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
			}
		}
	}
	rt.execReadyClock = fc.clock()
	if err := rt.cleanupStalePod(context.Background(), "default", "a", true, &HomeStorageRealization{TerminationWaitSeconds: 15}); err != nil {
		t.Fatalf("cleanupStalePod: %v", err)
	}
	if waited := fc.now.Sub(start); waited < 20*time.Second {
		t.Errorf("next start waited %s, want it to wait for the pod to stop", waited)
	}
	for i, d := range *deletes {
		if d.GracePeriodSeconds != nil && *d.GracePeriodSeconds == 0 {
			t.Errorf("delete %d used grace 0: %+v", i, d)
		}
	}
}

// A pod without an NFS home is still deleted immediately, with the UID
// precondition.
func TestK8sDeleteResource_PlainPodImmediate(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	p := runningNFSHomePod("a")
	p.Annotations = nil
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var seen []metav1.DeleteOptions
	cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		seen = append(seen, a.(k8stesting.DeleteActionImpl).DeleteOptions)
		return false, nil, nil
	})
	if err := rt.DeleteResource(context.Background(), podHandle(p)); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if len(seen) != 1 || seen[0].GracePeriodSeconds == nil || *seen[0].GracePeriodSeconds != 0 {
		t.Fatalf("deletes = %+v, want one delete with grace 0", seen)
	}
	wantUIDPrecondition(t, seen[0], "uid-1")
	if _, err := cs.CoreV1().Pods("default").Get(context.Background(), "a", metav1.GetOptions{}); err == nil {
		t.Error("pod still present")
	}
}

// A pod that cannot be read is deleted gracefully, with the UID
// precondition, never with grace 0.
func TestK8sDeleteResource_PodReadErrorGraceful(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	p := runningNFSHomePod("a")
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	if err := rt.DeleteResource(context.Background(), podHandle(p)); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds != nil {
		t.Fatalf("deletes = %+v, want one graceful delete", *deletes)
	}
	wantUIDPrecondition(t, (*deletes)[0], "uid-1")
}

// An NFS-home start refuses on an unreadable previous pod even when no home
// storage realization is set; a start without an NFS home still deletes
// the pod immediately by name.
func TestCleanupStalePod_UnreadablePodRefusedForNFSStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nfsHome bool
	}{{"nfs home start", true}, {"plain start", false}} {
		t.Run(tc.name, func(t *testing.T) {
			rt, cs, _ := newTestK8sRuntime()
			if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			deletes := keepPodsOnDelete(cs)
			cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, errors.New("connection reset")
			})
			err := rt.cleanupStalePod(context.Background(), "default", "a", tc.nfsHome, nil)
			if tc.nfsHome {
				if !errors.Is(err, errPreviousPodUnconfirmed) {
					t.Errorf("err = %v, want previous_pod_unconfirmed", err)
				}
				if len(*deletes) != 0 {
					t.Errorf("deletes = %+v, want none", *deletes)
				}
				return
			}
			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds == nil || *(*deletes)[0].GracePeriodSeconds != 0 {
				t.Errorf("deletes = %+v, want one delete with grace 0", *deletes)
			}
		})
	}
}

// Run passes the start's home storage backend to the previous-pod cleanup:
// an NFS-home start without a home storage realization refuses on an
// unreadable previous pod instead of force-deleting it by name.
func TestRun_NFSHomeStartUnreadablePodRefusedWithoutRealization(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := nfsHomeTestConfig(true)
	cfg.Name = "a"
	cfg.HomeStorage = nil
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), runningNFSHomePod("a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes := keepPodsOnDelete(cs)
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	_, err := rt.Run(context.Background(), cfg)
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Fatalf("err = %v, want previous_pod_unconfirmed", err)
	}
	if len(*deletes) != 0 {
		t.Errorf("deletes = %+v, want none", *deletes)
	}
}
