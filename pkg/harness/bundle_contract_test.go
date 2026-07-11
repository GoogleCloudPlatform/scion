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

package harness

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	harnessFS "github.com/GoogleCloudPlatform/scion/harnesses"
)

// TestBundleContract runs each harness's provision.py against a set of
// fixture cases, asserting exit codes and output files. This freezes
// current behavior so later WPs can prove they changed nothing
// unintentionally.
//
// Each fixture case lives under pkg/harness/testdata/bundle_contract/<harness>/<case>/
// and contains:
//   - input.json: {"auth_candidates": {...}, "harness_config": {...}, "mcp_servers": {...}}
//   - want.json:  {"exit_code": N, "resolved_auth": {...}, "env": {...}}
//
// Optional files in the fixture dir:
//   - secrets/<NAME>: staged as secrets/<NAME> in the bundle
//   - instructions.md: staged as inputs/instructions.md
//   - system-prompt.md: staged as inputs/system-prompt.md
func TestBundleContract(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found in PATH; skipping bundle contract tests")
	}

	// Discover all harnesses that have provision.py.
	harnessNames := discoverHarnesses(t)
	if len(harnessNames) == 0 {
		t.Fatal("no harnesses with provision.py found")
	}

	for _, hname := range harnessNames {
		fixtureDir := filepath.Join("testdata", "bundle_contract", hname)
		entries, err := os.ReadDir(fixtureDir)
		if err != nil {
			t.Logf("no fixtures for harness %s (skipping): %v", hname, err)
			continue
		}

		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			caseName := e.Name()
			t.Run(hname+"/"+caseName, func(t *testing.T) {
				runBundleContractCase(t, python, hname, filepath.Join(fixtureDir, caseName))
			})
		}
	}
}

func discoverHarnesses(t *testing.T) []string {
	t.Helper()
	var names []string
	entries, err := fs.ReadDir(harnessFS.FS, ".")
	if err != nil {
		t.Fatalf("read harnesses FS: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.ReadFile(harnessFS.FS, e.Name()+"/provision.py"); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

type fixtureInput struct {
	AuthCandidates map[string]interface{} `json:"auth_candidates"`
	HarnessConfig  map[string]interface{} `json:"harness_config"`
	MCPServers     map[string]interface{} `json:"mcp_servers"`
}

type fixtureWant struct {
	ExitCode     int                    `json:"exit_code"`
	ResolvedAuth map[string]interface{} `json:"resolved_auth"`
	Env          map[string]interface{} `json:"env"`
}

func runBundleContractCase(t *testing.T, python, hname, caseDir string) {
	t.Helper()

	// Read fixture input and expected output.
	inputData, err := os.ReadFile(filepath.Join(caseDir, "input.json"))
	if err != nil {
		t.Fatalf("read input.json: %v", err)
	}
	wantData, err := os.ReadFile(filepath.Join(caseDir, "want.json"))
	if err != nil {
		t.Fatalf("read want.json: %v", err)
	}

	var input fixtureInput
	if err := json.Unmarshal(inputData, &input); err != nil {
		t.Fatalf("parse input.json: %v", err)
	}
	var want fixtureWant
	if err := json.Unmarshal(wantData, &want); err != nil {
		t.Fatalf("parse want.json: %v", err)
	}

	// Create a temp HOME.
	tmpHome := t.TempDir()
	bundleDir := filepath.Join(tmpHome, ".scion", "harness")

	for _, sub := range []string{"", "inputs", "outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(bundleDir, sub), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	// Stage the harness bundle (provision.py, config.yaml, scion_harness.py)
	// from the embedded FS.
	for _, fname := range []string{"provision.py", "config.yaml", "scion_harness.py"} {
		data, err := fs.ReadFile(harnessFS.FS, hname+"/"+fname)
		if err != nil {
			if fname == "scion_harness.py" {
				data = harnessFS.CanonicalHarnessLib
			} else if fname == "config.yaml" {
				continue // optional
			} else {
				t.Fatalf("read %s/%s from embed: %v", hname, fname, err)
			}
		}
		dst := filepath.Join(bundleDir, fname)
		if err := os.WriteFile(dst, data, 0755); err != nil {
			t.Fatalf("write %s: %v", fname, err)
		}
	}

	// Stage auth-candidates.json.
	if input.AuthCandidates != nil {
		data, _ := json.MarshalIndent(input.AuthCandidates, "", "  ")
		if err := os.WriteFile(filepath.Join(bundleDir, "inputs", "auth-candidates.json"), data, 0644); err != nil {
			t.Fatalf("write auth-candidates.json: %v", err)
		}
	}

	// Stage mcp-servers.json.
	if input.MCPServers != nil {
		data, _ := json.MarshalIndent(input.MCPServers, "", "  ")
		if err := os.WriteFile(filepath.Join(bundleDir, "inputs", "mcp-servers.json"), data, 0644); err != nil {
			t.Fatalf("write mcp-servers.json: %v", err)
		}
	}

	// Stage fixture secrets.
	secretsDir := filepath.Join(caseDir, "secrets")
	if entries, err := os.ReadDir(secretsDir); err == nil {
		for _, se := range entries {
			data, err := os.ReadFile(filepath.Join(secretsDir, se.Name()))
			if err != nil {
				t.Fatalf("read secret %s: %v", se.Name(), err)
			}
			dst := filepath.Join(bundleDir, "secrets", se.Name())
			if err := os.WriteFile(dst, data, 0600); err != nil {
				t.Fatalf("write secret %s: %v", se.Name(), err)
			}
		}
	}

	// Stage optional text inputs.
	for _, fname := range []string{"instructions.md", "system-prompt.md"} {
		data, err := os.ReadFile(filepath.Join(caseDir, fname))
		if err == nil {
			if err := os.WriteFile(filepath.Join(bundleDir, "inputs", fname), data, 0644); err != nil {
				t.Fatalf("write %s: %v", fname, err)
			}
		}
	}

	// Build the manifest.
	manifest := map[string]interface{}{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         tmpHome,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundleDir,
		"harness_config":     input.HarnessConfig,
		"inputs":             map[string]interface{}{},
		"outputs": map[string]interface{}{
			"env":           filepath.Join(bundleDir, "outputs", "env.json"),
			"resolved_auth": filepath.Join(bundleDir, "outputs", "resolved-auth.json"),
		},
		"platform": map[string]interface{}{"goos": "linux", "goarch": "amd64"},
	}

	// Set input paths in manifest for files that exist.
	manifestInputs := manifest["inputs"].(map[string]interface{})
	for fname, key := range map[string]string{
		"auth-candidates.json": "auth_candidates",
		"mcp-servers.json":     "mcp_servers",
		"instructions.md":      "instructions",
		"system-prompt.md":     "system_prompt",
	} {
		p := filepath.Join(bundleDir, "inputs", fname)
		if _, err := os.Stat(p); err == nil {
			manifestInputs[key] = p
		}
	}

	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	manifestPath := filepath.Join(bundleDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		t.Fatalf("write manifest.json: %v", err)
	}

	// Run provision.py.
	cmd := exec.Command(python, filepath.Join(bundleDir, "provision.py"), "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+tmpHome, "PYTHONDONTWRITEBYTECODE=1")
	output, err := cmd.CombinedOutput()

	gotExitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			gotExitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("exec provision.py: %v\noutput: %s", err, output)
		}
	}

	if gotExitCode != want.ExitCode {
		t.Errorf("exit code: got %d, want %d\noutput:\n%s", gotExitCode, want.ExitCode, output)
	}

	// Check resolved-auth.json if expected.
	if want.ResolvedAuth != nil {
		authPath := filepath.Join(bundleDir, "outputs", "resolved-auth.json")
		gotAuth := readJSONFile(t, authPath)
		assertJSONSubset(t, "resolved-auth.json", want.ResolvedAuth, gotAuth)
	}

	// Check env.json if expected.
	if want.Env != nil {
		envPath := filepath.Join(bundleDir, "outputs", "env.json")
		gotEnv := readJSONFile(t, envPath)
		assertJSONSubset(t, "env.json", want.Env, gotEnv)
	}
}

func readJSONFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse %s: %v\ncontent: %s", path, err, data)
	}
	return result
}

// assertJSONSubset checks that every key in want exists in got with the same value.
// This allows the actual output to contain additional keys (forward-compatible).
func assertJSONSubset(t *testing.T, label string, want, got map[string]interface{}) {
	t.Helper()
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("%s: missing key %q (want %v)", label, k, wv)
			continue
		}
		wJSON, _ := json.Marshal(wv)
		gJSON, _ := json.Marshal(gv)
		if string(wJSON) != string(gJSON) {
			t.Errorf("%s: key %q:\n  want: %s\n  got:  %s", label, k, wJSON, gJSON)
		}
	}
}

// TestBundleContractCoverage verifies every harness has at least 4 fixture cases.
func TestBundleContractCoverage(t *testing.T) {
	harnessNames := discoverHarnesses(t)
	for _, hname := range harnessNames {
		fixtureDir := filepath.Join("testdata", "bundle_contract", hname)
		entries, err := os.ReadDir(fixtureDir)
		if err != nil {
			t.Errorf("harness %s: no fixture directory at %s", hname, fixtureDir)
			continue
		}
		count := 0
		for _, e := range entries {
			if e.IsDir() {
				count++
			}
		}
		if count < 4 {
			t.Errorf("harness %s: only %d fixture cases (need ≥4)", hname, count)
		} else {
			t.Logf("harness %s: %d fixture cases", hname, count)
		}
	}
}

// TestBundleContractNoDeletedHelpers fails if any provision.py contains
// helper definitions or patterns that should live in scion_harness.py.
// This prevents re-introduction of duplicated code after consolidation.
func TestBundleContractNoDeletedHelpers(t *testing.T) {
	forbidden := []string{
		"def _expand(",
		"def _load_json(",
		"def _write_json(",
		"def _read_secret(",
		"def _present_env_keys(",
		"def _present_file_paths(",
		"def _env_secret_files(",
		"def _file_secret_files(",
		"_read_mcp_servers_inline",
	}

	harnessNames := discoverHarnesses(t)
	for _, hname := range harnessNames {
		data, err := fs.ReadFile(harnessFS.FS, hname+"/provision.py")
		if err != nil {
			continue
		}
		content := string(data)
		for _, pat := range forbidden {
			if strings.Contains(content, pat) {
				t.Errorf("harness %s/provision.py contains %q — this helper should be in scion_harness.py", hname, pat)
			}
		}
		if strings.Contains(content, "except ImportError") && strings.Contains(content, "scion_harness") {
			t.Errorf("harness %s/provision.py uses except ImportError around scion_harness — import must be mandatory", hname)
		}
	}
}
