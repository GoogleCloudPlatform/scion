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
	"testing"
)

// lookPython3 returns the python3 binary, skipping the test if it is absent.
func lookPython3(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found in PATH; skipping Python unit tests")
	}
	return python
}

// runPythonUnittest runs "python3 -m unittest <module> -v" from dir. The
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
	t.Logf("Python unit tests output:\n%s", out)
}

func TestScionHarnessPythonUnit(t *testing.T) {
	runPythonUnittest(t, lookPython3(t), ".", "scion_harness_test")
}

// TestTelemetryProvisionPythonUnit runs the cross-harness telemetry
// provisioning tests in harnesses/telemetry_provision_test.py.
func TestTelemetryProvisionPythonUnit(t *testing.T) {
	runPythonUnittest(t, lookPython3(t), ".", "telemetry_provision_test")
}

// TestHarnessProvisionPythonUnit runs each harness's provision_test.py.
// "unittest discover" from harnesses/ never reaches them because the
// harness directories are not packages.
func TestHarnessProvisionPythonUnit(t *testing.T) {
	python := lookPython3(t)

	matches, err := filepath.Glob(filepath.Join("*", "provision_test.py"))
	if err != nil {
		t.Fatalf("glob provision tests: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no harnesses/*/provision_test.py files found")
	}
	for _, match := range matches {
		dir := filepath.Dir(match)
		t.Run(dir, func(t *testing.T) {
			runPythonUnittest(t, python, dir, "provision_test")
		})
	}
}
