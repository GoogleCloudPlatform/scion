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

// This file is an external test package: pkg/harness imports this package
// (for the embedded bundles), so the pin test cannot live in package
// harnesses.
package harnesses_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// The broker-side fail-fast check (harness.CheckStagedAuth) cannot share
// code with the container-side auth selection, which is Python
// (scion_harness.ProvisionContext.select_auth plus each harness's AuthSpec).
// These tests pin the two together in one direction: whenever the Go check
// rejects a start, the real Python selection also fails for the same staged
// auth-candidates.json. The Go check may allow starts the provisioner later
// rejects; it must never reject a start the provisioner would accept.
//
// Limit: the pins call select_auth (and opencode's _select_vertex_explicit),
// not each harness's provision(). Some provision() functions wrap
// select_auth (antigravity, copilot, grok-build, opencode); today every
// wrapper re-raises when an explicit type is set and swallows the failure
// only when no explicit type is set and no_auth is present, where the Go
// check never rejects. A wrapper that swallowed an explicit-type failure
// (for example by falling back to another credential source) would make the
// Go check reject starts the provisioner accepts, and these pins would not
// notice. When changing such a wrapper, extend the driver to follow it.

// pinDriver evaluates the real provisioner auth selection for a list of
// staged auth-candidates payloads. argv: harness dir, request JSON path.
// It prints one JSON object: {"spec": [...methods...], "results": [...]}.
//
// opencode handles an explicit vertex-ai selection outside select_auth
// (_select_vertex_explicit); the driver follows the provisioner there so the
// pin covers what actually runs.
const pinDriver = `
import importlib.util, json, os, sys, tempfile

harness_dir, request_path = sys.argv[1], sys.argv[2]
sys.path.insert(0, harness_dir)
import scion_harness

spec_obj = importlib.util.spec_from_file_location("pin_provision", os.path.join(harness_dir, "provision.py"))
mod = importlib.util.module_from_spec(spec_obj)
spec_obj.loader.exec_module(mod)

auth_specs = [v for v in vars(mod).values() if type(v).__name__ == "AuthSpec"]
if len(auth_specs) != 1:
    raise SystemExit("expected exactly one AuthSpec in provision.py, found %d" % len(auth_specs))
auth_spec = auth_specs[0]

with open(request_path) as f:
    request = json.load(f)

results = []
for case in request["cases"]:
    home = tempfile.mkdtemp()
    os.environ["HOME"] = home
    for rel in case.get("home_files") or []:
        p = scion_harness.expand_path(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w") as f:
            f.write("{}")
    bundle = tempfile.mkdtemp()
    os.makedirs(os.path.join(bundle, "inputs"))
    with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w") as f:
        json.dump(case["candidates"], f)
    manifest = {"harness_bundle_dir": bundle, "harness_config": case["harness_config"]}
    ctx = scion_harness.ProvisionContext(auth_spec.harness, manifest)
    ctx.info = lambda *a, **k: None
    try:
        if ctx.explicit_type == "vertex-ai" and hasattr(mod, "_select_vertex_explicit"):
            got = mod._select_vertex_explicit(ctx)
        else:
            got = ctx.select_auth(auth_spec)
        results.append({"ok": True, "method": got.method})
    except scion_harness.ProvisionError as exc:
        results.append({"ok": False, "error": str(exc)})

methods = [{
    "name": m.name, "kind": m.kind, "any_of": m.any_of, "all_of": m.all_of,
    "path": m.path, "secret_key": m.secret_key,
} for m in auth_spec.methods]
print(json.dumps({"spec": methods, "results": results}))
`

type pinMethod struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	AnyOf     []string `json:"any_of"`
	AllOf     []string `json:"all_of"`
	Path      string   `json:"path"`
	SecretKey string   `json:"secret_key"`
}

type pinResult struct {
	OK     bool   `json:"ok"`
	Method string `json:"method"`
	Error  string `json:"error"`
}

type pinCase struct {
	Candidates    map[string]interface{} `json:"candidates"`
	HarnessConfig map[string]interface{} `json:"harness_config"`
	// HomeFiles are created in the provisioner's HOME ("~/..." paths).
	HomeFiles []string `json:"home_files,omitempty"`

	label  string
	noAuth *config.HarnessNoAuthConfig
	// in supplies the Go side's view of the same container files: mount
	// targets, or files in the agent home (agentHomeFiles).
	mounts         []string
	fileTargets    []string
	agentHomeFiles []string
}

// shippedHarnessesWithAuth returns the harness dirs that declare auth types
// and ship a provision.py.
func shippedHarnessesWithAuth(t *testing.T) map[string]config.HarnessConfigEntry {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("*", "provision.py"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]config.HarnessConfigEntry{}
	for _, p := range dirs {
		dir := filepath.Dir(p)
		data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		entry, err := config.ParseHarnessConfigYAML(data)
		if err != nil {
			t.Fatalf("%s: parse config.yaml: %v", dir, err)
		}
		if entry.Auth == nil || len(entry.Auth.Types) == 0 {
			continue
		}
		out[dir] = entry
	}
	if len(out) < minProvisionTestFiles {
		t.Fatalf("found %d shipped harnesses with auth types, want at least %d: discovery is broken", len(out), minProvisionTestFiles)
	}
	return out
}

// minProvisionTestFiles mirrors the floor in python_unit_test.go (a
// different package, so it cannot be shared).
const minProvisionTestFiles = 9

func runPinDriver(t *testing.T, python, dir string, cases []pinCase) ([]pinMethod, []pinResult) {
	t.Helper()
	if cases == nil {
		cases = []pinCase{}
	}
	tmp := t.TempDir()
	driver := filepath.Join(tmp, "pin_driver.py")
	if err := os.WriteFile(driver, []byte(pinDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(map[string]interface{}{"cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	reqPath := filepath.Join(tmp, "request.json")
	if err := os.WriteFile(reqPath, req, 0o600); err != nil {
		t.Fatal(err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// A minimal environment: no credential can come from the container
	// environment, and HOME holds no credential file, matching the Go side's
	// empty env and empty agent home.
	cmd := exec.Command(python, "-I", driver, absDir, reqPath)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("%s: pin driver failed: %v\n%s", dir, err, ee.Stderr)
		}
		t.Fatalf("%s: pin driver failed: %v", dir, err)
	}
	var parsed struct {
		Spec    []pinMethod `json:"spec"`
		Results []pinResult `json:"results"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("%s: parse pin driver output: %v\n%s", dir, err, out)
	}
	if len(parsed.Results) != len(cases) {
		t.Fatalf("%s: got %d results for %d cases", dir, len(parsed.Results), len(cases))
	}
	return parsed.Spec, parsed.Results
}

// harnessConfigJSON round-trips entry through JSON, the way the manifest
// carries it to the provisioner.
func harnessConfigJSON(t *testing.T, entry config.HarnessConfigEntry) map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// buildPinCases enumerates explicit types against single staged
// credentials of every kind, with the harness's no_auth as shipped and
// with no_auth removed.
func buildPinCases(t *testing.T, entry config.HarnessConfigEntry, spec []pinMethod) []pinCase {
	t.Helper()
	envKeys := map[string]bool{}
	fileNames := map[string]bool{}
	filePaths := map[string]bool{}
	explicitTypes := map[string]bool{"": true}
	for name, typ := range entry.Auth.Types {
		explicitTypes[name] = true
		for _, g := range typ.RequiredEnv {
			for _, k := range g.AnyOf {
				envKeys[k] = true
			}
		}
		for _, rf := range typ.RequiredFiles {
			if rf.Name != "" {
				fileNames[rf.Name] = true
			}
			for _, k := range rf.AlternativeEnvKeys {
				envKeys[k] = true
			}
			if rf.TargetSuffix != "" {
				filePaths["~/"+strings.TrimPrefix(rf.TargetSuffix, "/")] = true
			}
		}
	}
	for _, m := range spec {
		explicitTypes[m.Name] = true
		for _, k := range append(append([]string{}, m.AnyOf...), m.AllOf...) {
			envKeys[k] = true
		}
		if m.SecretKey != "" {
			fileNames[m.SecretKey] = true
		}
		if m.Path != "" {
			filePaths[m.Path] = true
		}
	}

	type staged struct {
		label string
		mut   func(c map[string]interface{})
	}
	stagings := []staged{{label: "nothing staged", mut: func(map[string]interface{}) {}}}
	for _, k := range sortedSet(envKeys) {
		k := k
		stagings = append(stagings,
			staged{"env_vars " + k, func(c map[string]interface{}) { c["env_vars"] = []string{k} }},
			staged{"env_secret_files " + k, func(c map[string]interface{}) {
				c["env_secret_files"] = map[string]string{k: "$HOME/.scion/harness/secrets/" + k}
			}})
	}
	for _, n := range sortedSet(fileNames) {
		n := n
		stagings = append(stagings, staged{"file_secret_files " + n, func(c map[string]interface{}) {
			c["file_secret_files"] = map[string]string{n: "$HOME/.scion/harness/secrets/" + n}
		}})
	}
	for _, p := range sortedSet(filePaths) {
		p := p
		stagings = append(stagings, staged{"files " + p, func(c map[string]interface{}) {
			c["files"] = []map[string]string{{"container_path": p}}
		}})
	}

	withNoAuth := entry
	withoutNoAuth := entry
	withoutNoAuth.NoAuthConfig = nil
	variants := []struct {
		label string
		entry config.HarnessConfigEntry
	}{{"no_auth as shipped", withNoAuth}, {"no_auth removed", withoutNoAuth}}

	var cases []pinCase
	for _, v := range variants {
		hc := harnessConfigJSON(t, v.entry)
		for _, explicit := range sortedSet(explicitTypes) {
			for _, s := range stagings {
				cand := map[string]interface{}{
					"schema_version":    1,
					"explicit_type":     explicit,
					"resolved_method":   "container-script",
					"env_vars":          []string{},
					"env_secret_files":  map[string]string{},
					"file_secret_files": map[string]string{},
					"files":             []map[string]string{},
				}
				s.mut(cand)
				cases = append(cases, pinCase{
					Candidates:    cand,
					HarnessConfig: hc,
					label:         v.label + ", explicit=" + explicit + ", " + s.label,
					noAuth:        v.entry.NoAuthConfig,
				})
			}
		}
	}
	return cases
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeCandidates stages c as auth-candidates.json under a fresh agent home.
func writeCandidates(t *testing.T, c map[string]interface{}) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".scion", "harness", "inputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth-candidates.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestCheckStagedAuthPinnedToProvisioner: for every shipped harness and a
// matrix of staged candidates, a Go rejection implies a Python rejection.
func TestCheckStagedAuthPinnedToProvisioner(t *testing.T) {
	python := requirePythonPin(t)
	for dir, entry := range shippedHarnessesWithAuth(t) {
		dir, entry := dir, entry
		t.Run(dir, func(t *testing.T) {
			spec, _ := runPinDriver(t, python, dir, nil)
			cases := buildPinCases(t, entry, spec)
			_, results := runPinDriver(t, python, dir, cases)
			goRejects := 0
			for i, c := range cases {
				home := writeCandidates(t, c.Candidates)
				err := harness.CheckStagedAuth(entry.Harness, entry.Auth, c.noAuth, harness.AuthCheckInputs{AgentHome: home, HomeIsAgentHome: true})
				if err == nil {
					continue
				}
				if !errors.Is(err, harness.ErrNoAuthSatisfied) {
					t.Fatalf("%s: unexpected error kind: %v", c.label, err)
				}
				goRejects++
				if results[i].OK {
					t.Errorf("%s: Go check rejects the start but the provisioner selects %q\nGo error: %v",
						c.label, results[i].Method, err)
				}
			}
			// Non-vacuous: every harness has explicit types the check can
			// reject when nothing is staged.
			if goRejects == 0 {
				t.Errorf("Go check rejected none of %d cases; the pin is vacuous", len(cases))
			}
			t.Logf("%d cases, %d rejected by the Go check (all also rejected by the provisioner)", len(cases), goRejects)
		})
	}
}

// TestCheckStagedAuthPinnedWithContainerFile covers the input the staged
// matrix cannot: a credential file that exists in the container at a file
// method's target, with nothing staged. The provisioner accepts it; the Go
// check must too when it sees the same file as a volume mounted at the
// target, as a volume mounted at the parent directory, as a file-type
// secret at the target, or as a file in the agent home (which Docker, Podman
// and Apple container mount over the container home). HomeIsAgentHome is
// true throughout, the strictest setting.
func TestCheckStagedAuthPinnedWithContainerFile(t *testing.T) {
	python := requirePythonPin(t)
	fileMethods := 0
	for dir, entry := range shippedHarnessesWithAuth(t) {
		dir, entry := dir, entry
		t.Run(dir, func(t *testing.T) {
			spec, _ := runPinDriver(t, python, dir, nil)
			withoutNoAuth := entry
			withoutNoAuth.NoAuthConfig = nil
			hc := harnessConfigJSON(t, withoutNoAuth)
			var cases []pinCase
			for _, m := range spec {
				if m.Kind != "file" || m.Path == "" {
					continue
				}
				fileMethods++
				parent := m.Path[:strings.LastIndex(m.Path, "/")]
				for _, explicit := range []string{m.Name, ""} {
					cand := map[string]interface{}{
						"schema_version": 1, "explicit_type": explicit, "resolved_method": "container-script",
						"env_vars": []string{}, "env_secret_files": map[string]string{},
						"file_secret_files": map[string]string{}, "files": []map[string]string{},
					}
					base := pinCase{Candidates: cand, HarnessConfig: hc, HomeFiles: []string{m.Path}}
					for _, v := range []struct {
						label                 string
						mounts, files, agentF []string
					}{
						{"volume at target", []string{m.Path}, nil, nil},
						{"volume at parent dir", []string{parent}, nil, nil},
						{"file secret at target", nil, []string{m.Path}, nil},
						{"file in agent home", nil, nil, []string{m.Path}},
					} {
						c := base
						c.label = "no_auth removed, explicit=" + explicit + ", " + m.Path + " via " + v.label
						c.mounts, c.fileTargets, c.agentHomeFiles = v.mounts, v.files, v.agentF
						cases = append(cases, c)
					}
				}
			}
			if len(cases) == 0 {
				return
			}
			_, results := runPinDriver(t, python, dir, cases)
			for i, c := range cases {
				if !results[i].OK {
					t.Fatalf("%s: fixture: provisioner rejects a start with the file present: %s", c.label, results[i].Error)
				}
				home := writeCandidates(t, c.Candidates)
				for _, f := range c.agentHomeFiles {
					p := filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(f, "~/")))
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				err := harness.CheckStagedAuth(entry.Harness, entry.Auth, nil,
					harness.AuthCheckInputs{AgentHome: home, ContainerHome: "/home/scion", MountTargets: c.mounts,
						FileTargets: c.fileTargets, HomeIsAgentHome: true})
				if err != nil {
					t.Errorf("%s: provisioner selects %q but the Go check rejects: %v", c.label, results[i].Method, err)
				}
			}
		})
	}
	if fileMethods == 0 {
		t.Error("no provisioner file auth methods found; the pin is vacuous")
	}
}

// TestCheckStagedAuthCoversProvisionerSpec checks, per shipped harness, that
// every provisioner auth method has a harness-config auth type of the same
// name whose credentials include the method's: its env keys, its file
// secret name and its file path. The Go check treats any such credential as
// satisfying the type, so this keeps it at least as permissive as the
// provisioner for inputs the matrix above does not enumerate (combinations
// of credentials).
//
// The single-credential matrix generalises to combinations only because the
// Go check treats any credential a type names as satisfying it, and every
// provisioner key is a credential of the same-named harness-config type.
// This test enforces the second half. It re-encodes the credential kinds
// stagedSatisfiesType (pkg/harness/auth_failfast.go) accepts; keep the two in
// step. It cannot catch an acceptance rule dropped from stagedSatisfiesType
// (the behavioural pin above does).
func TestCheckStagedAuthCoversProvisionerSpec(t *testing.T) {
	python := requirePythonPin(t)
	for dir, entry := range shippedHarnessesWithAuth(t) {
		dir, entry := dir, entry
		t.Run(dir, func(t *testing.T) {
			spec, _ := runPinDriver(t, python, dir, nil)
			for _, m := range spec {
				typ, ok := entry.Auth.Types[m.Name]
				if !ok {
					t.Errorf("provisioner auth method %q has no harness-config auth type", m.Name)
					continue
				}
				envKeys := map[string]bool{}
				fileNames := map[string]bool{}
				targets := map[string]bool{}
				for _, g := range typ.RequiredEnv {
					for _, k := range g.AnyOf {
						envKeys[k] = true
					}
				}
				for _, rf := range typ.RequiredFiles {
					fileNames[rf.Name] = true
					envKeys[rf.Name] = true // CheckStagedAuth accepts an env secret named like the file
					for _, k := range rf.AlternativeEnvKeys {
						envKeys[k] = true
					}
					if rf.TargetSuffix != "" {
						targets["~/"+strings.TrimPrefix(rf.TargetSuffix, "/")] = true
					}
				}
				switch m.Kind {
				case "env":
					for _, k := range append(append([]string{}, m.AnyOf...), m.AllOf...) {
						if !envKeys[k] {
							t.Errorf("method %q: env key %s is not a credential of harness-config type %q", m.Name, k, m.Name)
						}
					}
				case "file":
					if m.SecretKey != "" && !fileNames[m.SecretKey] {
						t.Errorf("method %q: file secret %s is not a required_files name of type %q", m.Name, m.SecretKey, m.Name)
					}
					if !targets[m.Path] {
						t.Errorf("method %q: file path %s is not a required_files target of type %q", m.Name, m.Path, m.Name)
					}
				}
			}
		})
	}
}

// requirePythonPin matches requirePython in python_unit_test.go (another
// package): skip locally, fail under CI.
func requirePythonPin(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 not found in PATH under CI — auth pin tests were NOT run")
		}
		t.Skip("python3 not found in PATH; skipping auth pin tests")
	}
	return python
}
