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
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestLaunchHooks_ObserverNeverTakesCleanup(t *testing.T) {
	var observed []string
	obs := func(h api.ResourceHandle) { observed = append(observed, "observed:"+h.UID) }
	h := launchHooks{observedFn: obs}
	if h.active() {
		t.Fatal("an observer alone must not make the runtime leave cleanup to the caller")
	}
	h.created(api.ResourceHandle{UID: "u1"})

	var created []string
	both := launchHooks{createdFn: func(h api.ResourceHandle) { created = append(created, h.UID) }, observedFn: obs}
	if !both.active() {
		t.Fatal("OnResourceCreated still selects caller cleanup")
	}
	both.created(api.ResourceHandle{UID: "u2"})
	if len(created) != 1 || len(observed) != 2 || observed[1] != "observed:u2" {
		t.Fatalf("created=%v observed=%v", created, observed)
	}
}

// TestK8sRun_ObserverSeesPartialCreatesAndSyncCleanupStays: on the
// synchronous path (no OnResourceCreated) an observer sees every resource
// created before a later failure, and the runtime still removes them itself.
func TestK8sRun_ObserverSeesPartialCreatesAndSyncCleanupStays(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	var fx uidFixture
	fx.install(t, clientset)
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("admission denied")
	})
	var mu sync.Mutex
	var observed []api.ResourceHandle
	config := hookTestConfig("obs-agent")
	config.ResolvedSecrets = envSecrets(1)
	config.ResolvedAuth = authFiles(t)
	config.ObserveResourceCreated = func(h api.ResourceHandle) {
		mu.Lock()
		observed = append(observed, h)
		mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := rt.Run(ctx, config); err == nil || !strings.Contains(err.Error(), "failed to create pod") {
		t.Fatalf("Run error = %v, want a pod create failure", err)
	}
	mu.Lock()
	n := len(observed)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("observed = %d handles, want both secrets created before the failure", n)
	}
	for _, h := range observed {
		if h.UID == "" || h.Kind != api.ResourceKindSecret {
			t.Fatalf("handle = %+v", h)
		}
	}
	secrets, _ := clientset.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	if len(secrets.Items) != 0 {
		t.Fatalf("secrets after the failed sync start = %d, want 0 (the runtime still cleans up)", len(secrets.Items))
	}
}
