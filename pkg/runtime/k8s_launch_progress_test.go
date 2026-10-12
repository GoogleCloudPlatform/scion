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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// progressRecorder records RunConfig.Progress calls as "step: message".
type progressRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (p *progressRecorder) progress(step, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, step+": "+message)
}

func (p *progressRecorder) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *progressRecorder) hooks() launchHooks {
	return launchHooks{progressFn: p.progress}
}

// scriptPodGets makes each Get of a pod return the next status in seq,
// repeating the last one once seq is used up.
func scriptPodGets(clientset *fake.Clientset, seq ...corev1.PodStatus) {
	var mu sync.Mutex
	i := 0
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		st := seq[i]
		if i < len(seq)-1 {
			i++
		}
		name := action.(k8stesting.GetAction).GetName()
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status:     *st.DeepCopy(),
		}, nil
	})
}

func fastPodReadyRuntime(gke bool) (*KubernetesRuntime, *fake.Clientset) {
	rt, clientset, _ := newTestK8sRuntime()
	rt.GKEMode = gke
	rt.podReadyPollInterval = 5 * time.Millisecond
	rt.podReadyTimeout = 5 * time.Second
	return rt, clientset
}

func condScheduled(status corev1.ConditionStatus, reason string) corev1.PodCondition {
	return corev1.PodCondition{Type: corev1.PodScheduled, Status: status, Reason: reason}
}

func mainWaiting(reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: agentContainerName, State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}}
}

func mainRunning() corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: agentContainerName, State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{}}}
}

func initState(state corev1.ContainerState) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "workspace-provision", State: state}
}

var (
	statusUnscheduled = corev1.PodStatus{Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{condScheduled(corev1.ConditionFalse, corev1.PodReasonUnschedulable)}}
	statusInitRunning = corev1.PodStatus{Phase: corev1.PodPending,
		Conditions:            []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")},
		InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})},
		ContainerStatuses:     []corev1.ContainerStatus{mainWaiting("PodInitializing", "")}}
	statusContainerCreating = corev1.PodStatus{Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")},
		InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}})},
		ContainerStatuses: []corev1.ContainerStatus{mainWaiting("ContainerCreating", "")}}
	statusRunning = corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions:        []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")},
		ContainerStatuses: []corev1.ContainerStatus{mainRunning()}}
)

// TestWaitForPodReady_ProgressStepSequence checks that waitForPodReady
// reports one progress step per pod-status change, in order, with each
// step's fixed message, and never repeats a step across polls.
func TestWaitForPodReady_ProgressStepSequence(t *testing.T) {
	rt, clientset := fastPodReadyRuntime(false)
	// The pod is non-GKE Pending without an Unschedulable condition first
	// (still scheduling), and each status repeats so a missing change check
	// shows up as a duplicate step.
	pending := corev1.PodStatus{Phase: corev1.PodPending}
	scriptPodGets(clientset, pending, pending, statusInitRunning, statusInitRunning,
		statusContainerCreating, statusContainerCreating, statusRunning)
	rec := &progressRecorder{}

	if err := rt.waitForPodReady(context.Background(), "default", "p", rec.hooks()); err != nil {
		t.Fatalf("waitForPodReady: %v", err)
	}
	assertEvents(t, rec.snapshot(), []string{
		"scheduling: waiting to be scheduled",
		"init_containers: running init containers",
		"container_create: preparing the container (image pull, volume mounts)",
		"container_start: starting the container",
	})
}

// TestPodLaunchStep covers the pod-status-only step mapping, including the
// cases pod status makes ambiguous (design wave 3 E-T3 and E-AC7).
func TestPodLaunchStep(t *testing.T) {
	csiOnly := statusContainerCreating.DeepCopy()
	csiOnly.InitContainerStatuses = nil
	// A slow CSI SecretProviderClass mount: no image ID yet, reason
	// ContainerCreating, exactly as for an image pull.
	csiOnly.ContainerStatuses[0].Image = "registry.example/agent:1"

	creatingUnscheduled := corev1.PodStatus{Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{mainWaiting("ContainerCreating", "")}}

	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
		gke    bool
		want   string
	}{
		{"no conditions yet", corev1.PodStatus{Phase: corev1.PodPending}, false, api.LaunchStepScheduling},
		{"unschedulable not GKE", statusUnscheduled, false, api.LaunchStepScheduling},
		{"unschedulable GKE", statusUnscheduled, true, api.LaunchStepAwaitingCapacity},
		{"not scheduled other reason GKE", corev1.PodStatus{Conditions: []corev1.PodCondition{
			condScheduled(corev1.ConditionFalse, "SchedulingGated")}}, true, api.LaunchStepScheduling},
		{"ContainerCreating before scheduling (E-T3)", creatingUnscheduled, false, api.LaunchStepScheduling},
		{"init container running", statusInitRunning, false, api.LaunchStepInitContainers},
		{"init container waiting", corev1.PodStatus{
			Conditions:            []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")},
			InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}})},
		}, false, api.LaunchStepInitContainers},
		{"container creating", statusContainerCreating, false, api.LaunchStepContainerCreate},
		{"CSI-mount-only delay (E-AC7)", *csiOnly, true, api.LaunchStepContainerCreate},
		{"main container running", statusRunning, false, api.LaunchStepContainerStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podLaunchStep(&corev1.Pod{Status: tc.status}, tc.gke)
			if got != tc.want {
				t.Errorf("podLaunchStep = %q, want %q", got, tc.want)
			}
			if got == api.LaunchStepImagePull {
				t.Errorf("pod status alone must never give %q", api.LaunchStepImagePull)
			}
		})
	}
}

// TestWaitForPodReady_TerminalFailuresAreLaunchFailures checks that each
// terminal pod status ends waitForPodReady with a *LaunchFailure carrying
// its code, step and Kubernetes reason, while the error text stays exactly
// what it was before the error was typed. Reason never carries the image
// reference, pod name or the Kubernetes message.
func TestWaitForPodReady_TerminalFailuresAreLaunchFailures(t *testing.T) {
	const img = `Back-off pulling image "registry.example/secret-team/agent:7"`
	scheduled := []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")}
	initWaiting := func(reason string) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, Conditions: scheduled,
			InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: img}})}}
	}
	mainWait := func(reason string) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, Conditions: scheduled,
			ContainerStatuses: []corev1.ContainerStatus{mainWaiting(reason, img)}}
	}
	for _, tc := range []struct {
		name                     string
		status                   corev1.PodStatus
		code, step, reason, text string
	}{
		{"ImagePullBackOff", mainWait("ImagePullBackOff"),
			api.LaunchCodeImagePullFailed, api.LaunchStepContainerCreate, "ImagePullBackOff",
			`image pull failed for pod "p": ` + img + ` — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)`},
		{"ErrImagePull", mainWait("ErrImagePull"),
			api.LaunchCodeImagePullFailed, api.LaunchStepContainerCreate, "ErrImagePull",
			`image pull failed for pod "p": ` + img + ` — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)`},
		{"InvalidImageName", mainWait("InvalidImageName"),
			api.LaunchCodeInvalidImage, api.LaunchStepContainerCreate, "InvalidImageName",
			`invalid image name for pod "p": ` + img},
		{"CreateContainerConfigError", mainWait("CreateContainerConfigError"),
			api.LaunchCodeContainerConfig, api.LaunchStepContainerCreate, "CreateContainerConfigError",
			`container configuration error for pod "p": ` + img + ` — check secret references and volume mounts`},
		{"CrashLoopBackOff", mainWait("CrashLoopBackOff"),
			api.LaunchCodeCrashLoop, api.LaunchStepContainerStart, "CrashLoopBackOff",
			`container is crash-looping in pod "p": ` + img + ` — check container logs with 'scion logs'`},
		{"init CrashLoopBackOff", initWaiting("CrashLoopBackOff"),
			api.LaunchCodeInitContainerFailed, api.LaunchStepInitContainers, "CrashLoopBackOff",
			`init container "workspace-provision" is crash-looping in pod "p": ` + img + ` — check container logs with 'scion logs'`},
		{"init ImagePullBackOff", initWaiting("ImagePullBackOff"),
			api.LaunchCodeInitContainerFailed, api.LaunchStepInitContainers, "ImagePullBackOff",
			`image pull failed for init container "workspace-provision" in pod "p": ` + img + ` — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)`},
		{"init exit non-zero", corev1.PodStatus{Phase: corev1.PodPending, Conditions: scheduled,
			InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Reason: "Error", Message: "chown failed"}})}},
			api.LaunchCodeInitContainerFailed, api.LaunchStepInitContainers, "Error",
			`init container "workspace-provision" failed in pod "p": exit code 3 (Error): chown failed`},
		{"unschedulable condition", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: node(s) didn't match selector"}}},
			api.LaunchCodeUnschedulable, api.LaunchStepScheduling, "Unschedulable",
			`pod "p" cannot be scheduled: 0/3 nodes are available: node(s) didn't match selector — check node selectors, tolerations, and resource availability`},
		{"unschedulable container reason", corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{mainWaiting("Unschedulable", "no nodes")}},
			api.LaunchCodeUnschedulable, api.LaunchStepScheduling, "Unschedulable",
			`pod "p" cannot be scheduled: no nodes — check node selectors, tolerations, and resource availability`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset := fastPodReadyRuntime(false)
			scriptPodGets(clientset, tc.status)
			err := rt.waitForPodReady(context.Background(), "default", "p", launchHooks{})
			if err == nil {
				t.Fatal("waitForPodReady succeeded, want a failure")
			}
			if err.Error() != tc.text {
				t.Errorf("error text changed:\n got  %s\n want %s", err.Error(), tc.text)
			}
			var lf *LaunchFailure
			if !errors.As(err, &lf) {
				t.Fatalf("error %T is not a *LaunchFailure: %v", err, err)
			}
			if lf.Code != tc.code || lf.Step != tc.step || lf.Reason != tc.reason {
				t.Errorf("LaunchFailure{Code %q, Step %q, Reason %q}, want {%q, %q, %q}",
					lf.Code, lf.Step, lf.Reason, tc.code, tc.step, tc.reason)
			}
			for _, leak := range []string{"secret-team", "registry.example", `"p"`, "chown failed", "nodes"} {
				if strings.Contains(lf.Reason, leak) {
					t.Errorf("Reason %q carries runtime detail %q", lf.Reason, leak)
				}
			}
		})
	}
}

// TestWaitForPodReady_GKEUnschedulableWaitsThenTimesOut checks the GKE
// capacity wait (OQ-E2): an Unschedulable pod on GKE keeps waiting as step
// awaiting_capacity, and when waitForPodReady's own budget runs out it
// returns launch_timeout in that step with the old text. When the caller's
// ctx ends first, the error stays untyped: that timeout is the caller's.
func TestWaitForPodReady_GKEUnschedulableWaitsThenTimesOut(t *testing.T) {
	t.Run("own budget", func(t *testing.T) {
		rt, clientset := fastPodReadyRuntime(true)
		rt.podReadyTimeout = 100 * time.Millisecond
		scriptPodGets(clientset, statusUnscheduled)
		rec := &progressRecorder{}

		err := rt.waitForPodReady(context.Background(), "default", "p", rec.hooks())
		assertEvents(t, rec.snapshot(), []string{"awaiting_capacity: waiting for node capacity"})
		if err == nil {
			t.Fatal("waitForPodReady succeeded, want a timeout")
		}
		if want := "timed out waiting for pod to be ready: context deadline exceeded"; err.Error() != want {
			t.Errorf("error text %q, want %q", err.Error(), want)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
		}
		var lf *LaunchFailure
		if !errors.As(err, &lf) {
			t.Fatalf("error %T is not a *LaunchFailure: %v", err, err)
		}
		if lf.Code != api.LaunchCodeLaunchTimeout || lf.Step != api.LaunchStepAwaitingCapacity || lf.Reason != "" {
			t.Errorf("LaunchFailure{Code %q, Step %q, Reason %q}, want {launch_timeout, awaiting_capacity, \"\"}", lf.Code, lf.Step, lf.Reason)
		}
	})
	t.Run("caller ctx ends first", func(t *testing.T) {
		rt, clientset := fastPodReadyRuntime(true)
		scriptPodGets(clientset, statusUnscheduled)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := rt.waitForPodReady(ctx, "default", "p", launchHooks{})
		if err == nil {
			t.Fatal("waitForPodReady succeeded, want a timeout")
		}
		var lf *LaunchFailure
		if errors.As(err, &lf) {
			t.Errorf("caller's timeout typed as %+v; want the plain error", lf)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
		}
	})
}

// TestLaunchFailure_TextAndUnwrap checks that a LaunchFailure reads as the
// error it wraps and unwraps to it.
func TestLaunchFailure_TextAndUnwrap(t *testing.T) {
	inner := errors.New("inner text")
	err := newLaunchFailure(api.LaunchCodeCrashLoop, api.LaunchStepContainerStart, "CrashLoopBackOff", inner)
	if err.Error() != "inner text" {
		t.Errorf("Error() = %q, want the wrapped text", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Error("errors.Is does not reach the wrapped error")
	}
	if newLaunchFailure(api.LaunchCodeCrashLoop, "", "", nil) != nil {
		t.Error("newLaunchFailure(nil) is not nil")
	}
}

// TestK8sRun_ProgressAfterPodReady checks the steps Run reports once the pod
// is ready: container_start (from the last readiness poll, covering the
// exec-tunnel wait), then home_sync, workspace_sync and startup_gate at
// their phases. The workspace case stops at the workspace sync, which the
// fake clientset cannot perform, after reporting the step.
func TestK8sRun_ProgressAfterPodReady(t *testing.T) {
	requireTools(t, "tar", "sh")
	t.Setenv("SCION_K8S_NAMESPACE", "default")
	for _, tc := range []struct {
		name      string
		workspace bool
		want      []string
	}{
		{"home only", false, []string{
			"container_start: starting the container",
			"home_sync: syncing the agent home",
			"startup_gate: starting the agent",
		}},
		{"with workspace", true, []string{
			"container_start: starting the container",
			"home_sync: syncing the agent home",
			"workspace_sync: syncing the workspace",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			rt.podReadyPollInterval = 5 * time.Millisecond
			rt.execProbe = func(context.Context, string, string) error { return nil }
			rt.podExec = func(context.Context, string, string, []string) (string, error) { return "", nil }
			rt.homeSync = func(context.Context, string, string, string, string, []string) error { return nil }
			adc := filepath.Join(t.TempDir(), "adc.json")
			if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := nfsHomeTestConfig(false)
			config.Name = "progress-agent"
			config.Harness = &MockHarness{}
			config.ResolvedAuth.Files[0].SourcePath = adc
			config.HomeDir = t.TempDir()
			if tc.workspace {
				config.Workspace = t.TempDir()
			}
			rec := &progressRecorder{}
			config.Progress = rec.progress

			err := startFakeK8sPod(t, rt, clientset, config)
			if tc.workspace {
				if err == nil || !strings.Contains(err.Error(), "failed to sync workspace") {
					t.Fatalf("Run: %v, want the fake workspace sync failure", err)
				}
			} else if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertEvents(t, rec.snapshot(), tc.want)
		})
	}
}
