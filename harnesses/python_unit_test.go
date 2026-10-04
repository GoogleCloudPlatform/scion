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

func TestScionHarnessPythonUnit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found in PATH; skipping Python unit tests")
	}

	cmd := exec.Command(python, "-m", "unittest", "scion_harness_test", "-v")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 -m unittest scion_harness_test failed:\n%s", out)
	}
	t.Logf("Python unit tests output:\n%s", out)
}

// TestTelemetryProvisionPythonUnit runs the cross-harness telemetry
// provisioning tests in harnesses/telemetry_provision_test.py.
func TestTelemetryProvisionPythonUnit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found in PATH; skipping Python unit tests")
	}

	cmd := exec.Command(python, "-m", "unittest", "telemetry_provision_test", "-v")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 -m unittest telemetry_provision_test failed:\n%s", out)
	}
	t.Logf("Python unit tests output:\n%s", out)
}

// TestHarnessProvisionPythonUnit runs each harness's provision_test.py.
// The harness directories are not Python packages (most names contain a
// dash), so "unittest discover" from harnesses/ never reaches them; each
// test module is run from inside its own directory instead.
func TestHarnessProvisionPythonUnit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found in PATH; skipping Python unit tests")
	}

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
			cmd := exec.Command(python, "-m", "unittest", "provision_test", "-v")
			cmd.Dir = dir
			// Keep Python from writing __pycache__ into the source tree.
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("python3 -m unittest provision_test in %s failed:\n%s", dir, out)
			}
			t.Logf("Python unit tests output:\n%s", out)
		})
	}
}
