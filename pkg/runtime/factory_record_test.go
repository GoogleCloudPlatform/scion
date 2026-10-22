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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const br4GlobalSettings = `schema_version: "1"
active_profile: main
runtimes:
  main-rt:
    type: docker
  shadowed-rt:
    type: docker
profiles:
  main:
    runtime: main-rt
  shadow:
    runtime: shadowed-rt
`

const br4ProjectSettings = `schema_version: "1"
runtimes:
  shadowed-rt:
    type: docker
    host: tcp://project-local:2375
`

// br4Setup writes the global and project settings BR4 uses and returns the
// project's .scion directory. The working directory is moved outside any
// project, and no settings overlay is installed until a subtest does.
func br4Setup(t *testing.T) string {
	t.Helper()
	t.Setenv("PATH", "")
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(br4GlobalSettings), 0644); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(home, "proj", ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(br4ProjectSettings), 0644); err != nil {
		t.Fatal(err)
	}

	prev := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })
	return projectScionDir
}

func installBR4Overlay(t *testing.T, runtimes map[string]config.V1RuntimeConfig, profiles map[string]config.V1ProfileConfig) {
	t.Helper()
	o := config.NewSettingsOverlay()
	o.Update(runtimes, profiles, nil)
	config.SetGlobalSettingsOverlay(o)
}

// BR4: discovery and GetRuntimeWithRecord set a global EntryName, with the
// project shadowing clause applied in file mode and while an active overlay
// stores no runtimes, and not applied when the overlay supplies runtimes.
func TestGetRuntimeWithRecord_BR4(t *testing.T) {
	t.Run("global entry records its name", func(t *testing.T) {
		project := br4Setup(t)
		rt, rec := GetRuntimeWithRecord(project, "main")
		if _, ok := rt.(*DockerRuntime); !ok {
			t.Fatalf("runtime = %T, want *DockerRuntime", rt)
		}
		want := config.RuntimeBuildRecord{Present: true, EntryName: "main-rt"}
		if rec != want {
			t.Errorf("record = %+v, want %+v", rec, want)
		}
	})

	t.Run("ResolveRuntime's active profile records its entry", func(t *testing.T) {
		project := br4Setup(t)
		_, rec := GetRuntimeWithRecord(project, "")
		if rec.EntryName != "main-rt" || !rec.Present {
			t.Errorf("record = %+v, want Present with EntryName main-rt", rec)
		}
	})

	t.Run("file mode: a project-local same-name entry records empty", func(t *testing.T) {
		project := br4Setup(t)
		rt, rec := GetRuntimeWithRecord(project, "shadow")
		if dr, ok := rt.(*DockerRuntime); !ok || dr.Host != "tcp://project-local:2375" {
			t.Fatalf("runtime = %#v, want the project-local docker entry", rt)
		}
		want := config.RuntimeBuildRecord{Present: true}
		if rec != want {
			t.Errorf("record = %+v, want %+v", rec, want)
		}
	})

	t.Run("overlay supplies runtimes: an overlay entry records its name, shadowing not applied", func(t *testing.T) {
		project := br4Setup(t)
		installBR4Overlay(t,
			map[string]config.V1RuntimeConfig{"ov-rt": {Type: "docker"}, "shadowed-rt": {Type: "docker"}},
			map[string]config.V1ProfileConfig{"ov": {Runtime: "ov-rt"}, "shadow": {Runtime: "shadowed-rt"}})
		if _, rec := GetRuntimeWithRecord(project, "ov"); rec.EntryName != "ov-rt" {
			t.Errorf("overlay entry: EntryName = %q, want ov-rt", rec.EntryName)
		}
		if _, rec := GetRuntimeWithRecord(project, "shadow"); rec.EntryName != "shadowed-rt" {
			t.Errorf("overlay entry with a project-local namesake: EntryName = %q, want shadowed-rt", rec.EntryName)
		}
	})

	t.Run("overlay active with no stored runtimes: file runtimes stay, shadowing applies", func(t *testing.T) {
		project := br4Setup(t)
		installBR4Overlay(t, nil, map[string]config.V1ProfileConfig{
			"main":   {Runtime: "main-rt"},
			"shadow": {Runtime: "shadowed-rt"},
		})
		if config.GetGlobalSettingsOverlay().SuppliesRuntimes() {
			t.Fatal("overlay unexpectedly supplies runtimes")
		}
		if _, rec := GetRuntimeWithRecord(project, "main"); rec.EntryName != "main-rt" {
			t.Errorf("file entry: EntryName = %q, want main-rt", rec.EntryName)
		}
		if _, rec := GetRuntimeWithRecord(project, "shadow"); rec.EntryName != "" {
			t.Errorf("shadowed file entry: EntryName = %q, want empty", rec.EntryName)
		}
	})

	t.Run("startup default record is taken from the file view", func(t *testing.T) {
		br4Setup(t)
		// Resolved before the overlay is installed, as the agent runtime's
		// startup default is, with the global project path the server passes.
		_, startup := GetRuntimeWithRecord("global", "")
		if startup.EntryName != "main-rt" {
			t.Errorf("startup record: EntryName = %q, want main-rt (file view)", startup.EntryName)
		}
		installBR4Overlay(t,
			map[string]config.V1RuntimeConfig{"ov-rt": {Type: "docker"}},
			map[string]config.V1ProfileConfig{"main": {Runtime: "ov-rt"}})
		if _, after := GetRuntimeWithRecord("global", ""); after.EntryName != "ov-rt" {
			t.Errorf("after the overlay: EntryName = %q, want ov-rt", after.EntryName)
		}
	})

	t.Run("type fallback records no entry", func(t *testing.T) {
		project := br4Setup(t)
		rt, rec := GetRuntimeWithRecord(project, "docker-not-a-profile")
		if rt == nil {
			t.Fatal("nil runtime")
		}
		if rec.EntryName != "" || !rec.Present {
			t.Errorf("record = %+v, want Present with no EntryName", rec)
		}
	})

	t.Run("an error runtime has the zero record", func(t *testing.T) {
		project := br4Setup(t)
		t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
		installBR4Overlay(t,
			map[string]config.V1RuntimeConfig{"k8s-rt": {Type: "kubernetes"}},
			map[string]config.V1ProfileConfig{"k8s": {Runtime: "k8s-rt"}})
		rt, rec := GetRuntimeWithRecord(project, "k8s")
		if _, ok := rt.(*ErrorRuntime); !ok {
			t.Fatalf("runtime = %T, want *ErrorRuntime", rt)
		}
		if rec != (config.RuntimeBuildRecord{}) {
			t.Errorf("record = %+v, want zero", rec)
		}
	})

	t.Run("Kubernetes records the raw kubeconfig current context", func(t *testing.T) {
		project := br4Setup(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/version" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"major":"1","minor":"30","gitVersion":"v1.30.0"}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()
		kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + srv.URL + `
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: ctx-a
- context:
    cluster: c
    user: u
  name: ctx-b
current-context: ctx-a
users:
- name: u
  user:
    token: fake-token
`
		path := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(path, []byte(kubeconfig), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUBECONFIG", path)
		installBR4Overlay(t,
			map[string]config.V1RuntimeConfig{"k8s-rt": {Type: "kubernetes", Context: "ctx-b"}},
			map[string]config.V1ProfileConfig{"k8s": {Runtime: "k8s-rt"}})
		rt, rec := GetRuntimeWithRecord(project, "k8s")
		kr, ok := rt.(*KubernetesRuntime)
		if !ok {
			t.Fatalf("runtime = %T (%v), want *KubernetesRuntime", rt, rt)
		}
		if kr.Client.CurrentContext != "ctx-b" {
			t.Fatalf("client CurrentContext = %q, want ctx-b", kr.Client.CurrentContext)
		}
		want := config.RuntimeBuildRecord{Present: true, EntryName: "k8s-rt", KubeconfigContext: "ctx-a"}
		if rec != want {
			t.Errorf("record = %+v, want %+v", rec, want)
		}
	})
}
