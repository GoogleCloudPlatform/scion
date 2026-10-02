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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

// These tests cover the async-launch runtime hooks in the Kubernetes runtime
// (design t1-async-create-v11.md §3.8.3, §3.8.4, §6 "P1b-2"): a checkpoint
// immediately before every resource-creating call, OnResourceCreated with
// the created object's UID after every true create, and DeleteResource's
// UID-precondition delete.

// uidFixture makes a fake clientset (and optionally a fake dynamic client)
// behave like an API server for object identity: every create is assigned a
// fresh UID, and a delete with a UID precondition fails with a Conflict when
// the stored object's UID differs. The client-go fake tracker does neither
// on its own.
type uidFixture struct {
	next atomic.Int64
}

func (f *uidFixture) newUID() types.UID {
	return types.UID(fmt.Sprintf("uid-%d", f.next.Add(1)))
}

// fakeWithReactors is the subset of the fake clientset and the fake dynamic
// client the fixture needs.
type fakeWithReactors interface {
	PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	Tracker() k8stesting.ObjectTracker
}

func (f *uidFixture) install(t *testing.T, c fakeWithReactors) {
	t.Helper()
	c.PrependReactor("create", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject()
		// Like the API server, every create gets a fresh UID, even if the
		// submitted object already carries one (the delete-and-recreate
		// retry resubmits the same object).
		if m, err := meta.Accessor(obj); err == nil {
			m.SetUID(f.newUID())
		}
		return false, nil, nil // let the default reactor store it
	})
	c.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		pre := del.GetDeleteOptions().Preconditions
		if pre == nil || pre.UID == nil {
			return false, nil, nil
		}
		existing, err := c.Tracker().Get(del.GetResource(), del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil // NotFound etc.: the default reactor answers
		}
		m, err := meta.Accessor(existing)
		if err != nil {
			return true, nil, err
		}
		if m.GetUID() != *pre.UID {
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Group: del.GetResource().Group, Resource: del.GetResource().Resource},
				del.GetName(), fmt.Errorf("precondition failed: UID in precondition: %s, UID in object meta: %s", *pre.UID, m.GetUID()))
		}
		return false, nil, nil
	})
}

// hookRecorder records an async launch's runtime hooks, plus every create
// the fake API sees, in one ordered event log, so a test can assert that a
// checkpoint comes immediately before each create.
type hookRecorder struct {
	mu      sync.Mutex
	events  []string
	handles []api.ResourceHandle
	// failAt, when > 0, makes the failAt-th checkpoint call return failErr.
	failAt  int
	failErr error
	calls   int
}

func (h *hookRecorder) checkpoint(ctx context.Context, step string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.events = append(h.events, "checkpoint:"+step)
	if h.failAt > 0 && h.calls == h.failAt {
		return h.failErr
	}
	return nil
}

func (h *hookRecorder) created(rh api.ResourceHandle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "handle:"+rh.Kind+"/"+rh.Name)
	h.handles = append(h.handles, rh)
}

func (h *hookRecorder) recordCreates(c fakeWithReactors) {
	c.PrependReactor("create", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject()
		name := ""
		if m, err := meta.Accessor(obj); err == nil {
			name = m.GetName()
		}
		h.mu.Lock()
		h.events = append(h.events, "create:"+action.GetResource().Resource+"/"+name)
		h.mu.Unlock()
		return false, nil, nil
	})
}

func (h *hookRecorder) snapshot() ([]string, []api.ResourceHandle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...), append([]api.ResourceHandle(nil), h.handles...)
}

func (h *hookRecorder) apply(config *RunConfig) {
	config.Checkpoint = h.checkpoint
	config.OnResourceCreated = h.created
}

// runWithHooks runs config until Run returns. The pod create cancels ctx so
// waitForPodReady returns at once instead of polling a fake API server that
// never reports the pod ready.
func runWithHooks(t *testing.T, rt *KubernetesRuntime, clientset fakeWithReactors, config RunConfig) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		cancel()
		return false, nil, nil
	})
	_, err := rt.Run(ctx, config)
	return err
}

func envSecrets(n int) []api.ResolvedSecret {
	out := make([]api.ResolvedSecret, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("KEY_%02d", i)
		out = append(out, api.ResolvedSecret{Name: name, Type: "environment", Target: name, Value: fmt.Sprintf("v%d", i), Source: "user"})
	}
	return out
}

func authFiles(t *testing.T) *api.ResolvedAuth {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(p, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: p, ContainerPath: "/home/scion/.creds.json"}}}
}

func hookTestConfig(name string) RunConfig {
	return RunConfig{
		Name:         name,
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels:       map[string]string{"scion.name": name},
	}
}

func assertEvents(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events:\n got  %q\n want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: got %q, want %q\n all: %q", i, got[i], want[i], got)
		}
	}
}

func TestK8sRun_LaunchHooks_CheckpointBeforeEachCreateAndHandlesCarryUIDs(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	var fx uidFixture
	rec := &hookRecorder{}
	rec.recordCreates(clientset)
	fx.install(t, clientset)

	config := hookTestConfig("hooks-agent")
	config.ResolvedSecrets = envSecrets(2)
	config.ResolvedAuth = authFiles(t)
	rec.apply(&config)

	_ = runWithHooks(t, rt, clientset, config)

	events, handles := rec.snapshot()
	assertEvents(t, events, []string{
		"checkpoint:secrets", "create:secrets/scion-agent-hooks-agent", "handle:secret/scion-agent-hooks-agent",
		"checkpoint:secrets", "create:secrets/scion-auth-hooks-agent", "handle:secret/scion-auth-hooks-agent",
		"checkpoint:pod_create", "create:pods/hooks-agent", "handle:pod/hooks-agent",
	})

	// Every handle names the stored object and carries its UID.
	for _, h := range handles {
		var obj metav1.Object
		var err error
		switch h.Kind {
		case api.ResourceKindSecret:
			obj, err = clientset.CoreV1().Secrets(h.Namespace).Get(context.Background(), h.Name, metav1.GetOptions{})
		case api.ResourceKindPod:
			obj, err = clientset.CoreV1().Pods(h.Namespace).Get(context.Background(), h.Name, metav1.GetOptions{})
		default:
			t.Fatalf("unexpected handle kind %q", h.Kind)
		}
		if err != nil {
			t.Fatalf("get %s %s/%s: %v", h.Kind, h.Namespace, h.Name, err)
		}
		if h.UID == "" || string(obj.GetUID()) != h.UID {
			t.Errorf("handle %+v: UID %q does not match stored object UID %q", h, h.UID, obj.GetUID())
		}
		if h.Namespace != "default" {
			t.Errorf("handle %+v: namespace %q, want default", h, h.Namespace)
		}
	}
}

func TestK8sRun_LaunchHooks_GKESecretProviderClass(t *testing.T) {
	rt, clientset, dynClient := newGKECleanupTestRuntime(t)
	var fx uidFixture
	rec := &hookRecorder{}
	rec.recordCreates(clientset)
	rec.recordCreates(dynClient)
	fx.install(t, clientset)
	fx.install(t, dynClient)

	config := hookTestConfig("gke-agent")
	config.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk", Source: "user", Ref: "projects/p/secrets/api-key"},
	}
	rec.apply(&config)

	_ = runWithHooks(t, rt, clientset, config)

	events, handles := rec.snapshot()
	assertEvents(t, events, []string{
		"checkpoint:secrets", "create:secretproviderclasses/scion-agent-gke-agent", "handle:secretproviderclass/scion-agent-gke-agent",
		"checkpoint:secrets", "create:secrets/scion-agent-gke-agent", "handle:secret/scion-agent-gke-agent",
		"checkpoint:pod_create", "create:pods/gke-agent", "handle:pod/gke-agent",
	})
	spc, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(context.Background(), "scion-agent-gke-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if handles[0].UID == "" || handles[0].UID != string(spc.GetUID()) {
		t.Errorf("SPC handle UID %q, stored UID %q", handles[0].UID, spc.GetUID())
	}
}

func TestK8sRun_CheckpointErrorStopsTheCreate(t *testing.T) {
	errEnded := errors.New("launch ended at the hub")
	cases := []struct {
		name        string
		failAt      int
		wantSecrets []string // secrets that must exist after Run
		wantHandles int
	}{
		{name: "first secret", failAt: 1, wantSecrets: nil, wantHandles: 0},
		{name: "auth secret", failAt: 2, wantSecrets: []string{"scion-agent-stop-agent"}, wantHandles: 1},
		// The secrets this launch created are left for its UID-precondition
		// cleanup, not deleted by name.
		{name: "pod", failAt: 3, wantSecrets: []string{"scion-agent-stop-agent", "scion-auth-stop-agent"}, wantHandles: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			var fx uidFixture
			fx.install(t, clientset)
			rec := &hookRecorder{failAt: tc.failAt, failErr: errEnded}

			config := hookTestConfig("stop-agent")
			config.ResolvedSecrets = envSecrets(1)
			config.ResolvedAuth = authFiles(t)
			rec.apply(&config)

			// runWithHooks bounds Run if a pod were (wrongly) created.
			err := runWithHooks(t, rt, clientset, config)
			if !errors.Is(err, errEnded) {
				t.Fatalf("Run error = %v, want it to wrap the checkpoint error", err)
			}
			pods, _ := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
			if len(pods.Items) != 0 {
				t.Fatalf("a pod was created after a failed checkpoint: %d pods", len(pods.Items))
			}
			secrets, _ := clientset.CoreV1().Secrets("default").List(context.Background(), metav1.ListOptions{})
			got := map[string]bool{}
			for _, s := range secrets.Items {
				got[s.Name] = true
			}
			if len(got) != len(tc.wantSecrets) {
				t.Fatalf("secrets after Run: %v, want %v", got, tc.wantSecrets)
			}
			for _, name := range tc.wantSecrets {
				if !got[name] {
					t.Fatalf("secret %s missing; have %v", name, got)
				}
			}
			if _, handles := rec.snapshot(); len(handles) != tc.wantHandles {
				t.Fatalf("handles = %+v, want %d", handles, tc.wantHandles)
			}
		})
	}
}

// TestK8sRun_NoHooks_SyncPathUnchanged: with no hooks (the synchronous
// path), Run creates the same objects as before and nothing is reported.
func TestK8sRun_NoHooks_SyncPathUnchanged(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	config := hookTestConfig("sync-agent")
	config.ResolvedSecrets = envSecrets(2)
	config.ResolvedAuth = authFiles(t)

	pod := runUntilPodSubmitted(t, rt, clientset, config)
	if pod.Name != "sync-agent" {
		t.Fatalf("pod name %q", pod.Name)
	}
	for _, name := range []string{"scion-agent-sync-agent", "scion-auth-sync-agent"} {
		if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("secret %s: %v", name, err)
		}
	}
}

// --- DeleteResource: UID precondition ---

func TestK8sDeleteResource_UIDPrecondition(t *testing.T) {
	for _, keys := range []int{1, 20} {
		t.Run(fmt.Sprintf("%d keys", keys), func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			var fx uidFixture
			fx.install(t, clientset)
			ctx := context.Background()

			// The old launch creates the per-agent Secret.
			oldRec := &hookRecorder{}
			if _, err := rt.createAgentSecretWithHooks(ctx, "default", "a1", envSecrets(keys), nil, launchHooks{createdFn: oldRec.created}); err != nil {
				t.Fatal(err)
			}
			// A newer launch hits AlreadyExists and deletes and recreates
			// it, which yields a new UID.
			newRec := &hookRecorder{}
			if _, err := rt.createAgentSecretWithHooks(ctx, "default", "a1", envSecrets(keys), nil, launchHooks{createdFn: newRec.created}); err != nil {
				t.Fatal(err)
			}
			_, oldHandles := oldRec.snapshot()
			_, newHandles := newRec.snapshot()
			if len(oldHandles) != 1 || len(newHandles) != 1 {
				t.Fatalf("want exactly one handle per create regardless of key count, got old=%+v new=%+v", oldHandles, newHandles)
			}
			if oldHandles[0].UID == newHandles[0].UID {
				t.Fatalf("recreate kept the UID %q", oldHandles[0].UID)
			}

			// The stale launch's cleanup leaves the newer Secret in place.
			if err := rt.DeleteResource(ctx, oldHandles[0]); err != nil {
				t.Fatalf("stale DeleteResource: %v", err)
			}
			s, err := clientset.CoreV1().Secrets("default").Get(ctx, "scion-agent-a1", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("the newer launch's secret was deleted by a stale cleanup: %v", err)
			}
			if string(s.UID) != newHandles[0].UID || len(s.Data) != keys {
				t.Fatalf("secret UID %q (want %q), %d keys (want %d)", s.UID, newHandles[0].UID, len(s.Data), keys)
			}

			// The owning launch's cleanup removes it.
			if err := rt.DeleteResource(ctx, newHandles[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := clientset.CoreV1().Secrets("default").Get(ctx, "scion-agent-a1", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
				t.Fatalf("secret still present after the owner's cleanup: %v", err)
			}
			// Deleting it again is not an error.
			if err := rt.DeleteResource(ctx, newHandles[0]); err != nil {
				t.Fatalf("delete of a missing object: %v", err)
			}
		})
	}
}

func TestK8sDeleteResource_PodAndSecretProviderClass(t *testing.T) {
	rt, clientset, dynClient := newGKECleanupTestRuntime(t)
	var fx uidFixture
	fx.install(t, clientset)
	fx.install(t, dynClient)
	ctx := context.Background()

	rec := &hookRecorder{}
	secrets := []api.ResolvedSecret{{Name: "K", Type: "environment", Target: "K", Value: "v", Ref: "projects/p/secrets/k"}}
	if _, err := rt.createSecretProviderClassWithHooks(ctx, "default", "a2", secrets, nil, launchHooks{createdFn: rec.created}); err != nil {
		t.Fatal(err)
	}
	pod, err := clientset.CoreV1().Pods("default").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a2", Namespace: "default"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, handles := rec.snapshot()
	spcHandle := handles[0]
	podHandle := api.ResourceHandle{Kind: api.ResourceKindPod, Namespace: "default", Name: "a2", UID: string(pod.UID)}

	// Stale UIDs leave both objects alone.
	for _, h := range []api.ResourceHandle{spcHandle, podHandle} {
		stale := h
		stale.UID = "uid-stale"
		if err := rt.DeleteResource(ctx, stale); err != nil {
			t.Fatalf("stale %s: %v", h.Kind, err)
		}
	}
	if _, err := clientset.CoreV1().Pods("default").Get(ctx, "a2", metav1.GetOptions{}); err != nil {
		t.Fatalf("pod removed by a stale handle: %v", err)
	}
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(ctx, "scion-agent-a2", metav1.GetOptions{}); err != nil {
		t.Fatalf("SPC removed by a stale handle: %v", err)
	}

	// Matching UIDs delete them.
	for _, h := range []api.ResourceHandle{spcHandle, podHandle} {
		if err := rt.DeleteResource(ctx, h); err != nil {
			t.Fatalf("%s: %v", h.Kind, err)
		}
	}
	if _, err := clientset.CoreV1().Pods("default").Get(ctx, "a2", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("pod still present: %v", err)
	}
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(ctx, "scion-agent-a2", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("SPC still present: %v", err)
	}
}

func TestK8sDeleteResource_RefusesUnconditionalAndUnknownKinds(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()
	if _, err := clientset.CoreV1().Secrets("default").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := rt.DeleteResource(ctx, api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "default", Name: "s"}); err == nil {
		t.Fatal("expected an error for a handle with no UID")
	}
	if _, err := clientset.CoreV1().Secrets("default").Get(ctx, "s", metav1.GetOptions{}); err != nil {
		t.Fatalf("a handle with no UID deleted the object: %v", err)
	}
	if err := rt.DeleteResource(ctx, api.ResourceHandle{Kind: "persistentvolumeclaim", Namespace: "default", Name: "s", UID: "u"}); err == nil {
		t.Fatal("expected an error for an unsupported kind")
	}
}
