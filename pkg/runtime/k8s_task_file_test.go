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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRun_HomeSyncCarriesTaskFile checks that a task file at .scion/task.md
// in the staging home is copied into the pod's home by the home sync, for
// plain and NFS homes, readable by the pod user, before the startup gate
// lets the harness start. The test writes the file itself, as Start does
// before Run (the writer and its tests are in pkg/agent, which this
// package cannot import), so it checks the sync, not the writer, and
// passes without the task file change too.
func TestRun_HomeSyncCarriesTaskFile(t *testing.T) {
	requireTools(t, "tar", "sh")
	// startFakeK8sPod looks for the pod in "default"; do not let the
	// namespace of a pod the test runs in leak in.
	t.Setenv("SCION_K8S_NAMESPACE", "default")
	brief := strings.Repeat("brief line\n", 64*1024/11+1)

	for _, nfs := range []bool{false, true} {
		name := "plain"
		if nfs {
			name = "nfs"
		}
		t.Run(name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			rt.execProbe = func(context.Context, string, string) error { return nil }
			podHome := t.TempDir()
			var mu sync.Mutex
			var steps []string
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				joined := strings.Join(cmd, " ")
				mu.Lock()
				steps = append(steps, joined)
				mu.Unlock()
				if joined == "cat "+k8sHomeModeFile {
					pod, err := clientset.CoreV1().Pods("default").Get(context.Background(), "task-agent", metav1.GetOptions{})
					if err != nil {
						return "", err
					}
					return `{"mode":"seed-over","start_id":"` + pod.Labels[labelStartID] + `"}`, nil
				}
				return "", nil
			}
			var syncErr error
			rt.homeSync = func(_ context.Context, _, _, src, dest string, ex []string) error {
				mu.Lock()
				defer mu.Unlock()
				if dest != "/home/scion" {
					return nil
				}
				steps = append(steps, "home-sync")
				// Archive and extract exactly as syncToPod does, into a
				// stand-in for the pod's home.
				cmd := exec.Command("tar", syncArchiveCreateArgs(src, ex...)...)
				cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
				archive, err := cmd.Output()
				if err != nil {
					syncErr = err
					return err
				}
				if stderr, err := extractHomeArchive(t, archive, podHome); err != nil {
					syncErr = err
					t.Errorf("extract: %v %s", err, stderr)
				}
				return nil
			}
			adc := filepath.Join(t.TempDir(), "adc.json")
			if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := nfsHomeTestConfig(nfs)
			config.Name = "task-agent"
			if config.HomeStorage != nil {
				config.HomeStorage.AgentSlug = "task-agent"
			}
			config.Harness = &MockHarness{}
			config.ResolvedAuth.Files[0].SourcePath = adc
			config.HomeDir = t.TempDir()
			taskPath := filepath.Join(config.HomeDir, ".scion", "task.md")
			if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(taskPath, []byte(brief), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := startFakeK8sPod(t, rt, clientset, config); err != nil {
				t.Fatalf("Run: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if syncErr != nil {
				t.Fatalf("home sync: %v", syncErr)
			}
			got, err := os.ReadFile(filepath.Join(podHome, ".scion", "task.md"))
			if err != nil {
				t.Fatalf("task file not in the pod home: %v", err)
			}
			if string(got) != brief {
				t.Errorf("task file in the pod home has %d bytes, want %d", len(got), len(brief))
			}
			st, err := os.Stat(filepath.Join(podHome, ".scion", "task.md"))
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm()&0o400 == 0 {
				t.Errorf("task file mode %v is not readable by the pod user", st.Mode().Perm())
			}
			syncIdx, gateIdx := -1, -1
			for i, s := range steps {
				if s == "home-sync" && syncIdx < 0 {
					syncIdx = i
				}
				if strings.Contains(s, ".scion-home-ready") && gateIdx < 0 {
					gateIdx = i
				}
			}
			if syncIdx < 0 || gateIdx < 0 || syncIdx > gateIdx {
				t.Errorf("home sync at step %d, startup gate at step %d: want the sync first; steps %q", syncIdx, gateIdx, steps)
			}
		})
	}
}
