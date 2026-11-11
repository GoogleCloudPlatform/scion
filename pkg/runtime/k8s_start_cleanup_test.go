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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"
)

// These tests cover a start that does not complete because its context ends
// (the broker cancels an in-flight start when the agent is deleted or
// stopped): Run must remove the pod, Secrets and SecretProviderClass it
// created, including ones that only appear after the delete, and must leave
// alone the objects of a newer agent created with the same name.

const startCleanupAgent = "proj1--agent"

// The fake clientset ignores context cancellation. ctxClientset wraps it so
// that pod and Secret calls fail on a done context, as real API calls do;
// this is what makes a cleanup that reused the start's own (cancelled)
// context fail these tests.
type ctxClientset struct{ *k8sfake.Clientset }

func (c ctxClientset) CoreV1() typedcorev1.CoreV1Interface {
	return ctxCoreV1{c.Clientset.CoreV1()}
}

type ctxCoreV1 struct{ typedcorev1.CoreV1Interface }

func (c ctxCoreV1) Pods(ns string) typedcorev1.PodInterface {
	return ctxPods{c.CoreV1Interface.Pods(ns)}
}

func (c ctxCoreV1) Secrets(ns string) typedcorev1.SecretInterface {
	return ctxSecrets{c.CoreV1Interface.Secrets(ns)}
}

type ctxPods struct{ typedcorev1.PodInterface }

func (p ctxPods) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.Pod, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.PodInterface.Get(ctx, name, o)
}

func (p ctxPods) Delete(ctx context.Context, name string, o metav1.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.PodInterface.Delete(ctx, name, o)
}

type ctxSecrets struct{ typedcorev1.SecretInterface }

func (s ctxSecrets) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.Secret, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.SecretInterface.Get(ctx, name, o)
}

func (s ctxSecrets) Delete(ctx context.Context, name string, o metav1.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.SecretInterface.Delete(ctx, name, o)
}

func (p ctxPods) List(ctx context.Context, o metav1.ListOptions) (*corev1.PodList, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.PodInterface.List(ctx, o)
}

func (s ctxSecrets) List(ctx context.Context, o metav1.ListOptions) (*corev1.SecretList, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.SecretInterface.List(ctx, o)
}

var _ kubernetes.Interface = ctxClientset{}

// newStartCleanupRuntime returns a GKE-mode runtime whose typed client
// honours context cancellation (see ctxClientset).
func newStartCleanupRuntime(t *testing.T) (*KubernetesRuntime, *k8sfake.Clientset, *dynfake.FakeDynamicClient) {
	t.Helper()
	rt, clientset, dyn := newGKECleanupTestRuntime(t)
	rt.Client = k8s.NewTestClient(dyn, ctxClientset{clientset})
	return rt, clientset, dyn
}

// deleteRecorder gives every created object a UID (the fake API server does
// not) and records the deletes issued after the first pod create, which are
// the ones made by the start's cleanup rather than by Run's pre-clean. The
// fake tracker ignores delete preconditions, so the recorded options are
// what tests check instead.
type deleteRecorder struct {
	mu      sync.Mutex
	armed   bool
	deletes []recordedDelete
}

type recordedDelete struct {
	resource, name string
	preconditions  *metav1.Preconditions
}

// expectedUID is the UID deleteRecorder assigns to an object it sees created.
func expectedUID(resource, name string) types.UID {
	return types.UID("uid-" + resource + "-" + name)
}

// recordDeletes installs a deleteRecorder on both fake clients. Install it
// after any test-specific create reactors so it runs first.
func recordDeletes(clientset *k8sfake.Clientset, dyn *dynfake.FakeDynamicClient) *deleteRecorder {
	rec := &deleteRecorder{}
	assignUID := func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		obj, err := meta.Accessor(action.(k8stesting.CreateAction).GetObject())
		if err == nil && obj.GetUID() == "" {
			obj.SetUID(expectedUID(action.GetResource().Resource, obj.GetName()))
		}
		if action.GetResource().Resource == "pods" {
			rec.mu.Lock()
			rec.armed = true
			rec.mu.Unlock()
		}
		return false, nil, nil
	}
	record := func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		rec.mu.Lock()
		if rec.armed {
			rec.deletes = append(rec.deletes, recordedDelete{
				resource:      del.GetResource().Resource,
				name:          del.GetName(),
				preconditions: del.GetDeleteOptions().Preconditions,
			})
		}
		rec.mu.Unlock()
		return false, nil, nil
	}
	clientset.PrependReactor("create", "*", assignUID)
	clientset.PrependReactor("delete", "*", record)
	dyn.PrependReactor("create", "*", assignUID)
	dyn.PrependReactor("delete", "*", record)
	return rec
}

// assertUIDPreconditions checks that the cleanup issued a delete for the pod,
// both Secrets and the SecretProviderClass, each carrying a UID
// precondition equal to the UID of the object it listed.
func (r *deleteRecorder) assertUIDPreconditions(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	want := map[string]bool{
		"pods/" + startCleanupAgent:                              false,
		"secrets/scion-agent-" + startCleanupAgent:               false,
		"secrets/scion-auth-" + startCleanupAgent:                false,
		"secretproviderclasses/scion-agent-" + startCleanupAgent: false,
	}
	for _, d := range r.deletes {
		key := d.resource + "/" + d.name
		if d.preconditions == nil || d.preconditions.UID == nil {
			t.Errorf("delete of %s has no UID precondition", key)
			continue
		}
		if got, exp := *d.preconditions.UID, expectedUID(d.resource, d.name); got != exp {
			t.Errorf("delete of %s has UID precondition %q, want %q", key, got, exp)
		}
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("cleanup issued no delete for %s", key)
		}
	}
}

// startCleanupConfig returns a RunConfig that makes Run create the agent
// Secret, the auth Secret, the SecretProviderClass (GKE mode, Ref secret)
// and the pod.
func startCleanupConfig() RunConfig {
	return RunConfig{
		Name:         startCleanupAgent,
		Image:        "test:latest",
		UnixUsername: "scion",
		ProjectID:    "proj1",
		Labels:       productionAgentLabels("agent", "proj1"),
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user", Ref: "projects/p/secrets/api-key"},
		},
		ResolvedAuth: &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: "", ContainerPath: "~/.config/x"}}},
	}
}

// notifyOnPodCreate returns a channel that receives the submitted pod once
// Run's pod Create call reaches the fake API server.
func notifyOnPodCreate(clientset *k8sfake.Clientset) <-chan *corev1.Pod {
	ch := make(chan *corev1.Pod, 1)
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		p := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		select {
		case ch <- p.DeepCopy():
		default:
		}
		return false, nil, nil
	})
	return ch
}

// runAsync starts Run in a goroutine and returns a channel with its error.
func runAsync(ctx context.Context, rt *KubernetesRuntime, config RunConfig) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := rt.Run(ctx, config)
		done <- err
	}()
	return done
}

func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func waitPodCreate(t *testing.T, ch <-chan *corev1.Pod) *corev1.Pod {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not submit a pod")
		return nil
	}
}

type objectChecker struct {
	rt      *KubernetesRuntime
	cs      *k8sfake.Clientset
	ns      string
	agent   string
	checkFn func(kind, name string, exists bool)
}

func (c objectChecker) each() {
	ctx := context.Background()
	_, err := c.cs.CoreV1().Pods(c.ns).Get(ctx, c.agent, metav1.GetOptions{})
	c.checkFn("Pod", c.agent, !k8serrors.IsNotFound(err))
	for _, name := range []string{"scion-agent-" + c.agent, "scion-auth-" + c.agent} {
		_, err := c.cs.CoreV1().Secrets(c.ns).Get(ctx, name, metav1.GetOptions{})
		c.checkFn("Secret", name, !k8serrors.IsNotFound(err))
	}
	spcName := "scion-agent-" + c.agent
	_, err = c.rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(c.ns).Get(ctx, spcName, metav1.GetOptions{})
	c.checkFn("SecretProviderClass", spcName, !k8serrors.IsNotFound(err))
}

func assertAllGone(t *testing.T, rt *KubernetesRuntime, cs *k8sfake.Clientset) {
	t.Helper()
	objectChecker{rt: rt, cs: cs, ns: "default", agent: startCleanupAgent, checkFn: func(kind, name string, exists bool) {
		if exists {
			t.Errorf("%s %s should have been removed after the start was cancelled", kind, name)
		}
	}}.each()
}

// TestRun_CancelledWhilePending_RemovesPodAndSecrets: the agent is deleted
// while its pod is still Pending (the broker cancels the in-flight start).
// Run must remove the pod, both Secrets and the SecretProviderClass.
func TestRun_CancelledWhilePending_RemovesPodAndSecrets(t *testing.T) {
	rt, clientset, dyn := newStartCleanupRuntime(t)
	created := notifyOnPodCreate(clientset)
	deletes := recordDeletes(clientset, dyn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, rt, startCleanupConfig())

	waitPodCreate(t, created)
	// Every object exists while the pod is Pending.
	objectChecker{rt: rt, cs: clientset, ns: "default", agent: startCleanupAgent, checkFn: func(kind, name string, exists bool) {
		if !exists {
			t.Fatalf("precondition: %s %s should exist while the pod is Pending", kind, name)
		}
	}}.each()

	cancel() // delete of the agent cancels the start
	if err := waitRun(t, done); err == nil {
		t.Fatal("expected Run to fail after cancellation")
	}
	assertAllGone(t, rt, clientset)
	deletes.assertUIDPreconditions(t)
}

// TestRun_CancelledDuringPodCreate_RemovesPodThatAppeared: the delete lands
// while the pod Create request is in flight. The API server stores the pod,
// but the client sees the cancellation as an error, so the pod appears after
// the delete looked for it. Run must still find (by its start ID) and remove
// that pod and the Secrets.
func TestRun_CancelledDuringPodCreate_RemovesPodThatAppeared(t *testing.T) {
	rt, clientset, dyn := newStartCleanupRuntime(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		p := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if err := clientset.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
			return true, nil, err
		}
		cancel()
		return true, nil, context.Canceled
	})
	deletes := recordDeletes(clientset, dyn)

	err := waitRun(t, runAsync(ctx, rt, startCleanupConfig()))
	if err == nil {
		t.Fatal("expected Run to fail")
	}
	assertAllGone(t, rt, clientset)
	deletes.assertUIDPreconditions(t)
}

// TestRun_CancelledStart_LeavesNewerSameNamedAgent: while an old start is
// still waiting for its pod, the agent is deleted and a newer agent is
// created with the same name (same deterministic pod and Secret names, a
// different start). When the old start is then cancelled, its cleanup must
// not remove any of the newer agent's objects.
func TestRun_CancelledStart_LeavesNewerSameNamedAgent(t *testing.T) {
	rt, clientset, _ := newStartCleanupRuntime(t)
	created := notifyOnPodCreate(clientset)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, rt, startCleanupConfig())
	waitPodCreate(t, created)

	// The delete removes the old agent's objects by name...
	if err := rt.Delete(context.Background(), RunRef{ID: startCleanupAgent}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// ...and a newer agent with the same name creates its own.
	newer := productionAgentLabels("agent", "proj1")
	newer[labelStartID] = "newer-start"
	bg := context.Background()
	if _, err := clientset.CoreV1().Pods("default").Create(bg, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: startCleanupAgent, Namespace: "default", Labels: newer, UID: "newer-pod-uid",
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed newer pod: %v", err)
	}
	for _, name := range []string{"scion-agent-" + startCleanupAgent, "scion-auth-" + startCleanupAgent} {
		if _, err := clientset.CoreV1().Secrets("default").Create(bg, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: newer, UID: types.UID("newer-" + name),
		}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed newer secret: %v", err)
		}
	}
	if _, err := rt.createSecretProviderClass(bg, "default", startCleanupAgent, startCleanupConfig().ResolvedSecrets, newer); err != nil {
		t.Fatalf("seed newer SPC: %v", err)
	}

	cancel() // the old start ends
	if err := waitRun(t, done); err == nil {
		t.Fatal("expected Run to fail after cancellation")
	}

	objectChecker{rt: rt, cs: clientset, ns: "default", agent: startCleanupAgent, checkFn: func(kind, name string, exists bool) {
		if !exists {
			t.Errorf("%s %s of the newer same-named agent was removed by the old start's cleanup", kind, name)
		}
	}}.each()
}

// TestRun_FailureBeforePod_RemovesOnlyThisStartsSecrets: a start that fails
// before creating its pod removes the Secrets it created, matched by start
// ID rather than by name alone. Here the auth Secret create fails because a
// concurrent start of the same agent name has just created that Secret; it
// carries the same agent labels and a different start ID, and must survive.
func TestRun_FailureBeforePod_RemovesOnlyThisStartsSecrets(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	authName := "scion-auth-" + startCleanupAgent
	other := productionAgentLabels("agent", "proj1")
	other[labelStartID] = "other-start"
	clientset.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		s := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		if s.Name != authName {
			return false, nil, nil
		}
		if err := clientset.Tracker().Create(corev1.SchemeGroupVersion.WithResource("secrets"), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: authName, Namespace: s.Namespace, Labels: other, UID: "other-auth-uid"},
		}, s.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, fmt.Errorf("simulated quota rejection")
	})
	config := startCleanupConfig()
	config.ResolvedSecrets[0].Ref = ""

	if _, err := rt.Run(context.Background(), config); err == nil {
		t.Fatal("expected Run to fail")
	}
	bg := context.Background()
	if _, err := clientset.CoreV1().Secrets("default").Get(bg, "scion-agent-"+startCleanupAgent, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("agent Secret created before the failure should be removed, got err=%v", err)
	}
	if s, err := clientset.CoreV1().Secrets("default").Get(bg, authName, metav1.GetOptions{}); err != nil {
		t.Errorf("Secret of the other start should survive, got err=%v", err)
	} else if s.Labels[labelStartID] != "other-start" {
		t.Errorf("Secret %s was replaced: start ID %q", authName, s.Labels[labelStartID])
	}
}
