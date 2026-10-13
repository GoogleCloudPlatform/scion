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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// fakeKubeAPIServer answers the version discovery request Verify makes.
func fakeKubeAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeKubeconfig(t *testing.T, server, contextName string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
users:
- name: u
  user:
    token: test-token
contexts:
- name: %s
  context:
    cluster: c
    user: u
current-context: %s
`, server, contextName, contextName)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewKubernetesRuntimeFromConfig_RealConstructor(t *testing.T) {
	api := fakeKubeAPIServer(t)
	path := writeKubeconfig(t, api.URL, "ctx-a")

	rt, err := NewKubernetesRuntimeFromConfig(path, config.V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a", Namespace: "agents"})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if rt.DefaultNamespace != "agents" {
		t.Errorf("namespace = %q, want agents", rt.DefaultNamespace)
	}
	if rt.Client == nil || rt.Client.CurrentContext != "ctx-a" {
		t.Errorf("client context = %+v, want ctx-a", rt.Client)
	}

	// Safe to call more than once per process.
	if _, err := NewKubernetesRuntimeFromConfig(path, config.V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a"}); err != nil {
		t.Fatalf("second construction: %v", err)
	}

	// An explicit file that cannot be loaded is an error, never a fallback.
	if _, err := NewKubernetesRuntimeFromConfig(filepath.Join(t.TempDir(), "missing"), config.V1RuntimeConfig{Type: "kubernetes"}); err == nil {
		t.Fatal("a missing explicit kubeconfig must fail")
	}
	// An unknown context in the explicit file is an error.
	if _, err := NewKubernetesRuntimeFromConfig(path, config.V1RuntimeConfig{Type: "kubernetes", Context: "nope"}); err == nil {
		t.Fatal("an unknown context must fail")
	}
}

// TestGetRuntime_KubernetesClientErrorIsErrorRuntime: GetRuntime's
// kubernetes case still wraps a client error in an ErrorRuntime with the
// client's error text.
func TestGetRuntime_KubernetesClientErrorIsErrorRuntime(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.json"),
		[]byte(`{"active_profile": "remote", "runtimes": {"kubernetes": {}}, "profiles": {"remote": {"runtime": "kubernetes"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", filepath.Join(tmpHome, "no-such-kubeconfig"))

	r := GetRuntime("", "")
	er, ok := r.(*ErrorRuntime)
	if !ok {
		t.Fatalf("got %T, want *ErrorRuntime", r)
	}
	if !strings.Contains(er.Err.Error(), "failed to load kubeconfig") {
		t.Errorf("error = %v, want the client's load error", er.Err)
	}
}
