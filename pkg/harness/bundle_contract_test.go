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
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	harnessFS "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestBundleContract runs each harness's provision.py against a set of
// fixture cases, asserting exit codes and output files. This freezes
// current behavior so later WPs can prove they changed nothing
// unintentionally.
//
// Each fixture case lives under pkg/harness/testdata/bundle_contract/<harness>/<case>/
// and contains:
//   - input.json: {"auth_candidates": {...}, "harness_config": {...}, "mcp_servers": {...}}
//   - want.json:  {"exit_code": N, "resolved_auth": {...}, "env": {...}, "env_absent": [...]}
//
// Optional files in the fixture dir:
//   - secrets/<NAME>: staged as secrets/<NAME> in the bundle
//   - instructions.md: staged as inputs/instructions.md
//   - system-prompt.md: staged as inputs/system-prompt.md
//   - bin/<NAME>: staged as an executable and prepended onto PATH — for
//     stubbing external CLIs a provisioner shells out to (e.g. antigravity's
//     `agy --version` gate), so the version-gated path is reachable without
//     the real binary installed.
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
	// EnvAbsent lists keys that must not appear in env.json.
	EnvAbsent []string `json:"env_absent"`
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

	// Stage optional fixture-local executables (e.g. a fake `agy` CLI so
	// antigravity's version-gated vertex-ai path is reachable without a real
	// AGY install) and prepend their directory to PATH.
	extraPathDir := ""
	if binEntries, err := os.ReadDir(filepath.Join(caseDir, "bin")); err == nil {
		extraPathDir = filepath.Join(tmpHome, "fixture-bin")
		if err := os.MkdirAll(extraPathDir, 0755); err != nil {
			t.Fatalf("mkdir fixture-bin: %v", err)
		}
		for _, be := range binEntries {
			data, err := os.ReadFile(filepath.Join(caseDir, "bin", be.Name()))
			if err != nil {
				t.Fatalf("read bin/%s: %v", be.Name(), err)
			}
			if err := os.WriteFile(filepath.Join(extraPathDir, be.Name()), data, 0755); err != nil {
				t.Fatalf("write bin/%s: %v", be.Name(), err)
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

	// Run provision.py with an allowlisted env, never the test process's
	// own: a credential there (a git token in an agent container, ambient
	// GCP settings) could otherwise change auth selection or reach the
	// outputs. Only PATH, LANG, LC_ALL and TMPDIR pass through, when set;
	// HOME is the per-case temp home.
	cmd := exec.Command(python, filepath.Join(bundleDir, "provision.py"), "--manifest", manifestPath)
	cmd.Env = []string{"HOME=" + tmpHome, "PYTHONDONTWRITEBYTECODE=1"}
	for _, k := range []string{"LANG", "LC_ALL", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	path := os.Getenv("PATH")
	if extraPathDir != "" {
		path = extraPathDir + string(os.PathListSeparator) + path
	}
	cmd.Env = append(cmd.Env, "PATH="+path)
	// The allowlist and the harness's auth metadata guard each other: no env
	// name the harness declares for auth, and no ambient GCP setting, may
	// reach provision.py.
	for _, k := range append(harnessAuthEnvNames(t, hname), gcpAmbientEnvNames...) {
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, k+"=") {
				t.Fatalf("provision.py env carries %s; the bundle contract runs without harness credentials", k)
			}
		}
	}
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
		assertJSONSubset(t, "resolved-auth.json", want.ResolvedAuth, gotAuth, false)
	}

	// Check env.json if expected.
	if want.Env != nil {
		envPath := filepath.Join(bundleDir, "outputs", "env.json")
		gotEnv := readJSONFile(t, envPath)
		assertJSONSubset(t, "env.json", want.Env, gotEnv, true)
	}

	// Check keys that env.json must leave out.
	if len(want.EnvAbsent) > 0 {
		gotEnv := readJSONFile(t, filepath.Join(bundleDir, "outputs", "env.json"))
		for _, k := range want.EnvAbsent {
			if v, ok := gotEnv[k]; ok {
				t.Errorf("env.json: key %q present (%s), want absent", k, valueShape(v))
			}
		}
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
		t.Fatalf("parse %s: %v (%d bytes; content not printed)", path, err, len(data))
	}
	return result
}

// assertJSONSubset checks that every key in want exists in got with the same value.
// This allows the actual output to contain additional keys (forward-compatible).
// assertJSONSubset checks that got holds every key of want with an equal
// value. With redact set (env.json, whose values can be credentials) a
// mismatch reports the key and the value lengths only, never a value.
func assertJSONSubset(t *testing.T, label string, want, got map[string]interface{}, redact bool) {
	t.Helper()
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			if redact {
				t.Errorf("%s: missing key %q (want %s)", label, k, valueShape(wv))
			} else {
				t.Errorf("%s: missing key %q (want %v)", label, k, wv)
			}
			continue
		}
		wJSON, _ := json.Marshal(wv)
		gJSON, _ := json.Marshal(gv)
		if string(wJSON) != string(gJSON) {
			if redact {
				t.Errorf("%s: key %q differs: want %s, got %s", label, k, valueShape(wv), valueShape(gv))
			} else {
				t.Errorf("%s: key %q:\n  want: %s\n  got:  %s", label, k, wJSON, gJSON)
			}
		}
	}
}

// gcpAmbientEnvNames are the ambient GCP settings provision.py's env_fallback
// would read; the bundle contract never passes them.
var gcpAmbientEnvNames = []string{
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_REGION",
	"GOOGLE_CLOUD_LOCATION",
	"CLOUD_ML_REGION",
	"GOOGLE_APPLICATION_CREDENTIALS",
}

// valueShape describes a value without revealing it: its length only.
func valueShape(v interface{}) string {
	return fmt.Sprintf("value of length %d, not printed", len(fmt.Sprint(v)))
}

// harnessAuthEnvNames returns every env name the harness's embedded
// config.yaml auth metadata declares: required_env names, the alternative env
// keys of required files, and autodetect env names.
func harnessAuthEnvNames(t *testing.T, hname string) []string {
	t.Helper()
	data, err := fs.ReadFile(harnessFS.FS, hname+"/config.yaml")
	if err != nil {
		t.Fatalf("read %s/config.yaml: %v", hname, err)
	}
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		t.Fatalf("parse %s/config.yaml: %v", hname, err)
	}
	if entry.Auth == nil {
		return nil
	}
	var names []string
	for _, typ := range entry.Auth.Types {
		for _, req := range typ.RequiredEnv {
			names = append(names, req.AnyOf...)
		}
		for _, f := range typ.RequiredFiles {
			names = append(names, f.AlternativeEnvKeys...)
		}
	}
	for k := range entry.Auth.Autodetect.Env {
		names = append(names, k)
	}
	return names
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
