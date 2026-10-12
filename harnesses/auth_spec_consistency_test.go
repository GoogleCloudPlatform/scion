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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Each harness declares its auth types twice: in config.yaml (auth.types,
// read by the hub, the broker and harness.CheckStagedAuth) and in
// provision.py (the AuthSpec that select_auth runs in the container). The
// two cannot share code, so TestAuthSpecMatchesHarnessConfig locks them
// together, in both directions, for every shipped harness:
//
//	a. the same auth type names;
//	b. an env method's key groups (each all_of key is its own group, any_of
//	   is one group) equal the type's required_env groups. A type with no
//	   required_env may instead be backed by one required file whose name or
//	   alternative_env_keys are the method's keys (the env carries the
//	   credential the file would hold). Because of this exemption, rule (b)
//	   does not itself check that a provisioner using this shape accepts
//	   the file-only case (file present, env key unset); such a provisioner
//	   must handle it and test it itself. For antigravity's explicit
//	   oauth-token, ExplicitOAuthTokenTest in antigravity/provision_test.py
//	   pins that behaviour;
//	c. a file method's path is "~" + target_suffix and its secret_key is
//	   the name of the type's only required file, and the type declares no
//	   required_env;
//	d. an env method's type may declare required files only for the
//	   broker-side ADC file (skipped_when_gcp_service_account_assigned) or
//	   the file backing in (b).
//
// The fail-fast pins in auth_failfast_pin_test.go cover a different
// property (a Go rejection implies a provisioner rejection) and are kept
// as they are.

// authSpecDriver prints the AuthSpec of the provision.py in argv[1] as JSON.
// opencode handles vertex-ai outside its AuthSpec (_select_vertex_explicit
// and _has_vertex, over _VERTEX_PROJECT_VARS and _VERTEX_LOCATION_VARS);
// the driver reports that as a two-group env method so it is checked like
// the others. A module-level VALID_AUTH_TYPES is reported as well.
const authSpecDriver = `
import importlib.util, json, os, sys

harness_dir = sys.argv[1]
sys.path.insert(0, harness_dir)
import scion_harness

spec_obj = importlib.util.spec_from_file_location("spec_provision", os.path.join(harness_dir, "provision.py"))
mod = importlib.util.module_from_spec(spec_obj)
spec_obj.loader.exec_module(mod)

auth_specs = [v for v in vars(mod).values() if type(v).__name__ == "AuthSpec"]
if len(auth_specs) != 1:
    raise SystemExit("expected exactly one AuthSpec in provision.py, found %d" % len(auth_specs))

methods = []
for m in auth_specs[0].methods:
    groups = [[k] for k in m.all_of]
    if m.any_of:
        groups.append(list(m.any_of))
    methods.append({"name": m.name, "kind": m.kind, "groups": groups,
                    "path": m.path or "", "secret_key": m.secret_key})

names = [m["name"] for m in methods]
if "vertex-ai" not in names and hasattr(mod, "_VERTEX_PROJECT_VARS") and hasattr(mod, "_VERTEX_LOCATION_VARS"):
    methods.append({"name": "vertex-ai", "kind": "env",
                    "groups": [list(mod._VERTEX_PROJECT_VARS), list(mod._VERTEX_LOCATION_VARS)],
                    "path": "", "secret_key": ""})

out = {"methods": methods}
if hasattr(mod, "VALID_AUTH_TYPES"):
    out["valid_auth_types"] = list(mod.VALID_AUTH_TYPES)
print(json.dumps(out))
`

type specMethod struct {
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	Groups    [][]string `json:"groups"`
	Path      string     `json:"path"`
	SecretKey string     `json:"secret_key"`
}

type specReport struct {
	Methods        []specMethod `json:"methods"`
	ValidAuthTypes []string     `json:"valid_auth_types"`
}

func runAuthSpecDriver(t *testing.T, python, dir string) specReport {
	t.Helper()
	tmp := t.TempDir()
	driver := filepath.Join(tmp, "spec_driver.py")
	if err := os.WriteFile(driver, []byte(authSpecDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", driver, absDir)
	cmd.Env = []string{"HOME=" + tmp, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("%s: spec driver failed: %v\n%s", dir, err, ee.Stderr)
		}
		t.Fatalf("%s: spec driver failed: %v", dir, err)
	}
	var rep specReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%s: parse spec driver output: %v\n%s", dir, err, out)
	}
	return rep
}

// canonGroups renders key groups order-insensitively, e.g. "[A B] [C]".
func canonGroups(groups [][]string) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		keys := append([]string(nil), g...)
		sort.Strings(keys)
		parts = append(parts, "["+strings.Join(keys, " ")+"]")
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func yamlEnvGroups(typ api.HarnessAuthTypeMetadata) [][]string {
	var groups [][]string
	for _, g := range typ.RequiredEnv {
		groups = append(groups, g.AnyOf)
	}
	return groups
}

// fileBacksEnvMethod reports whether required file rf carries the
// credential of an env method with key groups groups: a single group whose
// keys are all rf's name or one of its alternative_env_keys.
func fileBacksEnvMethod(rf api.HarnessAuthFileRequirement, groups [][]string) bool {
	if len(groups) != 1 || len(groups[0]) == 0 {
		return false
	}
	accepted := map[string]bool{rf.Name: true}
	for _, k := range rf.AlternativeEnvKeys {
		accepted[k] = true
	}
	for _, k := range groups[0] {
		if !accepted[k] {
			return false
		}
	}
	return true
}

// checkAuthSpecConsistency returns one message per disagreement between a
// harness's config.yaml auth types and its provisioner AuthSpec.
func checkAuthSpecConsistency(auth *config.HarnessAuthMetadata, rep specReport) []string {
	var problems []string
	types := map[string]api.HarnessAuthTypeMetadata{}
	if auth != nil {
		types = auth.Types
	}

	// (a) names, both directions.
	pyNames := map[string]bool{}
	for _, m := range rep.Methods {
		pyNames[m.Name] = true
	}
	for name := range types {
		if !pyNames[name] {
			problems = append(problems, "config.yaml auth type "+name+" has no provisioner auth method")
		}
	}
	for _, m := range rep.Methods {
		if _, ok := types[m.Name]; !ok {
			problems = append(problems, "provisioner auth method "+m.Name+" has no config.yaml auth type")
		}
	}
	if rep.ValidAuthTypes != nil {
		valid := append([]string(nil), rep.ValidAuthTypes...)
		sort.Strings(valid)
		want := make([]string, 0, len(pyNames))
		for n := range pyNames {
			want = append(want, n)
		}
		sort.Strings(want)
		if strings.Join(valid, ",") != strings.Join(want, ",") {
			problems = append(problems, "VALID_AUTH_TYPES ["+strings.Join(valid, ", ")+"] differs from the auth methods ["+strings.Join(want, ", ")+"]")
		}
	}

	for _, m := range rep.Methods {
		typ, ok := types[m.Name]
		if !ok {
			continue
		}
		switch m.Kind {
		case "env":
			yg := yamlEnvGroups(typ)
			backingFile := ""
			if len(yg) == 0 {
				// (b) file-backed env method.
				for _, rf := range typ.RequiredFiles {
					if fileBacksEnvMethod(rf, m.Groups) {
						backingFile = rf.Name
						break
					}
				}
				if backingFile == "" {
					problems = append(problems, "env method "+m.Name+": keys "+canonGroups(m.Groups)+" but config.yaml declares no required_env and no required file carrying them")
				}
			} else if canonGroups(yg) != canonGroups(m.Groups) {
				// (b) key groups.
				problems = append(problems, "env method "+m.Name+": provisioner key groups "+canonGroups(m.Groups)+" differ from config.yaml required_env "+canonGroups(yg))
			}
			// (d) required files on an env type.
			for _, rf := range typ.RequiredFiles {
				if rf.SkippedWhenGCPServiceAccountAssigned || rf.Name == backingFile {
					continue
				}
				problems = append(problems, "env method "+m.Name+": config.yaml required file "+rf.Name+" is neither the ADC file nor carried by the method's keys")
			}
		case "file":
			// (c) file methods.
			if len(typ.RequiredEnv) > 0 {
				problems = append(problems, "file method "+m.Name+": config.yaml type declares required_env "+canonGroups(yamlEnvGroups(typ)))
			}
			if len(typ.RequiredFiles) != 1 {
				problems = append(problems, "file method "+m.Name+": config.yaml type must declare exactly one required file")
				continue
			}
			rf := typ.RequiredFiles[0]
			want := "~/" + strings.TrimPrefix(rf.TargetSuffix, "/")
			if rf.TargetSuffix == "" || m.Path != want {
				problems = append(problems, "file method "+m.Name+": provisioner path "+m.Path+" differs from config.yaml target "+want)
			}
			if m.SecretKey != rf.Name {
				problems = append(problems, "file method "+m.Name+": provisioner secret_key "+m.SecretKey+" differs from config.yaml file name "+rf.Name)
			}
		default:
			problems = append(problems, "auth method "+m.Name+": unknown kind "+m.Kind)
		}
	}
	sort.Strings(problems)
	return problems
}

// TestAuthSpecMatchesHarnessConfig: every shipped harness's config.yaml
// auth types and provisioner AuthSpec agree (rules a-d above).
func TestAuthSpecMatchesHarnessConfig(t *testing.T) {
	python := requirePythonPin(t)
	dirs, err := filepath.Glob(filepath.Join("*", "provision.py"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
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
		checked++
		t.Run(dir, func(t *testing.T) {
			rep := runAuthSpecDriver(t, python, dir)
			if len(rep.Methods) == 0 {
				t.Fatal("provisioner AuthSpec declares no auth methods")
			}
			for _, problem := range checkAuthSpecConsistency(entry.Auth, rep) {
				t.Error(problem)
			}
		})
	}
	if checked < minProvisionTestFiles {
		t.Fatalf("checked %d harnesses, want at least %d: discovery is broken", checked, minProvisionTestFiles)
	}
}

// TestCheckAuthSpecConsistencyRules exercises each rule on synthetic input,
// so a rule that stops firing is noticed even while every shipped harness
// agrees.
func TestCheckAuthSpecConsistencyRules(t *testing.T) {
	adc := api.HarnessAuthFileRequirement{Name: "gcloud-adc", SkippedWhenGCPServiceAccountAssigned: true}
	auth := &config.HarnessAuthMetadata{Types: map[string]api.HarnessAuthTypeMetadata{
		"api-key":   {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"K1", "K2"}}}},
		"auth-file": {RequiredFiles: []api.HarnessAuthFileRequirement{{Name: "AUTH", TargetSuffix: "/.x/auth.json"}}},
		"vertex-ai": {
			RequiredEnv:   []api.HarnessAuthEnvRequirement{{AnyOf: []string{"P"}}, {AnyOf: []string{"R1", "R2"}}},
			RequiredFiles: []api.HarnessAuthFileRequirement{adc},
		},
		"oauth-token": {RequiredFiles: []api.HarnessAuthFileRequirement{{Name: "TOK", TargetSuffix: "/.x/tok"}}},
	}}
	good := func() specReport {
		return specReport{Methods: []specMethod{
			{Name: "api-key", Kind: "env", Groups: [][]string{{"K2", "K1"}}},
			{Name: "auth-file", Kind: "file", Path: "~/.x/auth.json", SecretKey: "AUTH"},
			{Name: "vertex-ai", Kind: "env", Groups: [][]string{{"P"}, {"R2", "R1"}}},
			{Name: "oauth-token", Kind: "env", Groups: [][]string{{"TOK"}}},
		}}
	}
	if got := checkAuthSpecConsistency(auth, good()); len(got) != 0 {
		t.Fatalf("consistent input reported problems: %v", got)
	}

	cases := []struct {
		name string
		mut  func(r *specReport)
		want string
	}{
		{"python lacks a yaml type", func(r *specReport) { r.Methods = r.Methods[1:] }, "config.yaml auth type api-key has no provisioner auth method"},
		{"yaml lacks a python method", func(r *specReport) {
			r.Methods = append(r.Methods, specMethod{Name: "extra", Kind: "env", Groups: [][]string{{"E"}}})
		}, "provisioner auth method extra has no config.yaml auth type"},
		{"env key dropped", func(r *specReport) { r.Methods[2].Groups = [][]string{{"P"}, {"R1"}} }, "env method vertex-ai: provisioner key groups"},
		{"env group dropped", func(r *specReport) { r.Methods[2].Groups = [][]string{{"P"}} }, "env method vertex-ai: provisioner key groups"},
		{"env key added", func(r *specReport) { r.Methods[0].Groups = [][]string{{"K1", "K2", "K3"}} }, "env method api-key: provisioner key groups"},
		{"file path differs", func(r *specReport) { r.Methods[1].Path = "~/.y/auth.json" }, "file method auth-file: provisioner path"},
		{"file secret differs", func(r *specReport) { r.Methods[1].SecretKey = "OTHER" }, "file method auth-file: provisioner secret_key"},
		{"env method not carried by file", func(r *specReport) { r.Methods[3].Groups = [][]string{{"OTHER"}} }, "env method oauth-token: keys"},
		{"file method becomes env", func(r *specReport) {
			r.Methods[1] = specMethod{Name: "auth-file", Kind: "env", Groups: [][]string{{"AUTH"}, {"X"}}}
		}, "env method auth-file: keys"},
		{"valid types differ", func(r *specReport) { r.ValidAuthTypes = []string{"api-key"} }, "VALID_AUTH_TYPES"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := good()
			c.mut(&r)
			got := checkAuthSpecConsistency(auth, r)
			for _, p := range got {
				if strings.HasPrefix(p, c.want) {
					return
				}
			}
			t.Errorf("want a problem starting %q, got %v", c.want, got)
		})
	}

	// (d): a non-ADC required file on an env type is reported.
	auth.Types["api-key"] = api.HarnessAuthTypeMetadata{
		RequiredEnv:   auth.Types["api-key"].RequiredEnv,
		RequiredFiles: []api.HarnessAuthFileRequirement{{Name: "STRAY"}},
	}
	got := checkAuthSpecConsistency(auth, good())
	if len(got) != 1 || !strings.HasPrefix(got[0], "env method api-key: config.yaml required file STRAY") {
		t.Errorf("stray required file: got %v", got)
	}
}
