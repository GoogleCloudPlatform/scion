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

package harnesses

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// minProvisionTestFiles is a floor on how many harnesses/*/provision_test.py
// files discovery must find. Without it, a broken glob (renamed file, moved
// directory, wrong cwd) would run zero suites and pass silently. Raise it
// when a harness gains a provision_test.py; do not lower it to make a
// failure go away.
const minProvisionTestFiles = 9

// requirePython returns the python3 path. A missing interpreter skips
// locally but fails under CI (CI set), where a skip would read as a pass
// for suites that never ran (same rule as hack/lib/require-tool.sh,
// ptone/scion#1114).
func requirePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 not found in PATH under CI — Python harness tests were NOT run")
		}
		t.Skip("python3 not found in PATH; skipping Python unit tests")
	}
	return python
}

// runPythonUnittest runs `python3 -m unittest <module> -v` in dir. The
// harness directories are not Python packages (most names contain a dash),
// so each module runs from its own directory. PYTHONDONTWRITEBYTECODE keeps
// the run from leaving __pycache__ in the source tree.
func runPythonUnittest(t *testing.T, python, dir, module string) {
	t.Helper()
	cmd := exec.Command(python, "-m", "unittest", module, "-v")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 -m unittest %s (in %s) failed:\n%s", module, dir, out)
	}
	t.Logf("python3 -m unittest %s (in %s) output:\n%s", module, dir, out)
}

func TestScionHarnessPythonUnit(t *testing.T) {
	python := requirePython(t)
	runPythonUnittest(t, python, ".", "scion_harness_test")
}

// TestHarnessTelemetryProvisionPython runs harnesses/telemetry_provision_test.py,
// the cross-harness telemetry provisioning suite (ptone/scion#2677).
func TestHarnessTelemetryProvisionPython(t *testing.T) {
	python := requirePython(t)
	runPythonUnittest(t, python, ".", "telemetry_provision_test")
}

// TestHarnessProvisionPython runs every per-harness provision_test.py
// (harnesses/<name>/provision_test.py), one subtest per harness
// (ptone/scion#2677). Previously only scion_harness_test ran in CI.
func TestHarnessProvisionPython(t *testing.T) {
	python := requirePython(t)

	files, err := filepath.Glob(filepath.Join("*", "provision_test.py"))
	if err != nil {
		t.Fatalf("glob */provision_test.py: %v", err)
	}
	if len(files) < minProvisionTestFiles {
		t.Fatalf("found %d harnesses/*/provision_test.py files, want at least %d: discovery is broken (found: %v)",
			len(files), minProvisionTestFiles, files)
	}
	sort.Strings(files)
	for _, f := range files {
		dir := filepath.Dir(f)
		t.Run(dir, func(t *testing.T) {
			runPythonUnittest(t, python, dir, "provision_test")
		})
	}
}
