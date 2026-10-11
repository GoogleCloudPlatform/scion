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

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func getTestHarnesses() []api.Harness {
	return []api.Harness{
		&harness.Generic{},
	}
}

// mockRuntimeForTest overrides runtime detection so InitMachine/InitProject
// succeed without a real container runtime installed.
func mockRuntimeForTest(t *testing.T) {
	t.Helper()
	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	t.Cleanup(restore)
}

// hermeticProject is a machine and project initialized by newHermeticProject.
// home is the temporary HOME, root the project root and scion <root>/.scion.
type hermeticProject struct {
	home, root, scion string
}

// newHermeticProject initializes a machine and an in-repo project under
// t.TempDir() so the result does not depend on where the checkout lives or on
// the ambient environment:
//   - HOME points at the temp dir, so the global ~/.scion is test-local;
//   - every SCION_* variable and GIT_DIR/GIT_WORK_TREE are unset, so values
//     exported by an enclosing agent container do not reach config loading;
//   - git-repo detection in InitProject is pinned to true, so the project is
//     always the in-repo layout (a .scion directory). Detection otherwise
//     shells out to git from the cwd, and its answer depends on the
//     checkout's location, ownership and the user's git config;
//   - GIT_CEILING_DIRECTORIES stops git discovery at the temp dir and the
//     project root is the working directory, so no git command reaches the
//     checkout's own repository.
//
// It uses t.Setenv and t.Chdir, so callers must not use t.Parallel.
func newHermeticProject(t *testing.T) *hermeticProject {
	t.Helper()
	mockRuntimeForTest(t)
	isolateTestEnv(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	t.Cleanup(config.OverrideIsGitRepo(func() bool { return true }))
	root := filepath.Join(home, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine: %v", err)
	}
	scion := filepath.Join(root, config.DotScion)
	if err := config.InitProject(scion, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if info, err := os.Stat(scion); err != nil || !info.IsDir() {
		t.Fatalf("fixture: expected %s to be an in-repo project directory (err=%v)", scion, err)
	}
	return &hermeticProject{home: home, root: root, scion: scion}
}

// isolateTestEnv unsets every SCION_* variable and the git variables that
// redirect repository discovery, restoring them when the test ends.
func isolateTestEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SCION_") {
			unsetEnvForTest(t, k)
		}
	}
	unsetEnvForTest(t, "GIT_DIR")
	unsetEnvForTest(t, "GIT_WORK_TREE")
}

// unsetEnvForTest unsets k for the rest of the test; t.Setenv records the
// original value so it is restored on cleanup.
func unsetEnvForTest(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "")
	if err := os.Unsetenv(k); err != nil {
		t.Fatal(err)
	}
}
