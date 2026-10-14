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

package config

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// conversionFixtures are the conversion pairs under
// testdata/profile_conversion (.design/flat-runtime-brokers/contract-appendix-p3.md
// FC6). Each directory holds input.yaml (legacy settings), an optional
// options.yaml (profiles in scope and operator choices),
// want_instances.yaml (the proposed instances, in settings form) and
// want_findings.json (the profile map and the findings). The list is
// explicit, so a removed fixture fails the test. home_storage_no_shared_dirs
// is a behavioural fixture, not a conversion pair.
var conversionFixtures = []string{
	"docker_rejects_k8s_fields",
	"every_profile_field",
	"every_runtime_field",
	"explicit_false_zero",
	"flat_40gi",
	"list_all_namespaces_true",
	"no_effect_fields",
	"operator_choice_resolves",
	"operator_choice_split",
	"per_dir_shadowed_by_profile_single",
	"per_key_map_conflict",
	"two_entries_one_scope",
	"two_profiles_same_target_agree",
	"two_profiles_same_target_infra_conflict",
	"two_profiles_same_target_workload_conflict",
	"unknown_keys",
	"unsupported_types",
}

type conversionFixture struct {
	name      string
	raw       []byte
	in        ProfileConversionInput
	instances []V1RuntimeBrokerInstanceConfig
	want      wantConversionFindings
}

type wantConversionFindings struct {
	ProfileMap map[string]string `json:"profile_map"`
	Findings   []struct {
		Code            string   `json:"code"`
		Severity        string   `json:"severity"`
		Path            string   `json:"path"`
		Profiles        []string `json:"profiles"`
		MessageContains []string `json:"message_contains"`
	} `json:"findings"`
}

func conversionFixtureDir(name string) string {
	return filepath.Join("testdata", "profile_conversion", name)
}

func loadConversionFixture(t *testing.T, name string) conversionFixture {
	t.Helper()
	dir := conversionFixtureDir(name)
	raw, err := os.ReadFile(filepath.Join(dir, "input.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	vs := &VersionedSettings{}
	if err := yamlv3.Unmarshal(raw, vs); err != nil {
		t.Fatalf("%s: input.yaml: %v", name, err)
	}
	f := conversionFixture{name: name, raw: raw, in: ProfileConversionInput{Settings: vs, RawSettings: raw}}

	if b, err := os.ReadFile(filepath.Join(dir, "options.yaml")); err == nil {
		var opts struct {
			Profiles []string `yaml:"profiles"`
			Choices  []struct {
				TargetGroup   string   `yaml:"target_group"`
				Field         string   `yaml:"field"`
				ChosenProfile string   `yaml:"chosen_profile"`
				Split         []string `yaml:"split"`
			} `yaml:"choices"`
		}
		if err := yamlv3.Unmarshal(b, &opts); err != nil {
			t.Fatalf("%s: options.yaml: %v", name, err)
		}
		f.in.Profiles = opts.Profiles
		for _, c := range opts.Choices {
			f.in.Choices = append(f.in.Choices, ConversionChoice{TargetGroup: c.TargetGroup, Field: c.Field, ChosenProfile: c.ChosenProfile, Split: c.Split})
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "want_instances.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var wantInst struct {
		Server struct {
			Broker struct {
				Instances []V1RuntimeBrokerInstanceConfig `yaml:"instances"`
			} `yaml:"broker"`
		} `yaml:"server"`
	}
	if err := yamlv3.Unmarshal(b, &wantInst); err != nil {
		t.Fatalf("%s: want_instances.yaml: %v", name, err)
	}
	f.instances = wantInst.Server.Broker.Instances

	b, err = os.ReadFile(filepath.Join(dir, "want_findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f.want); err != nil {
		t.Fatalf("%s: want_findings.json: %v", name, err)
	}
	return f
}

// Every fixture converts to exactly its golden instances, profile map and
// findings, and the same input always gives the same result.
func TestConvert_Fixtures(t *testing.T) {
	for _, name := range conversionFixtures {
		t.Run(name, func(t *testing.T) {
			f := loadConversionFixture(t, name)
			got := ConvertProfilesToInstances(f.in)

			if len(got.Instances) != 0 || len(f.instances) != 0 {
				if !reflect.DeepEqual(got.Instances, f.instances) {
					gy, _ := yamlv3.Marshal(got.Instances)
					t.Errorf("instances differ from want_instances.yaml; got:\n%s", gy)
				}
			}
			if len(got.ProfileMap) != 0 || len(f.want.ProfileMap) != 0 {
				if !reflect.DeepEqual(got.ProfileMap, f.want.ProfileMap) {
					t.Errorf("profile map: got %v, want %v", got.ProfileMap, f.want.ProfileMap)
				}
			}
			if len(got.Findings) != len(f.want.Findings) {
				gj, _ := json.MarshalIndent(got.Findings, "", "  ")
				t.Fatalf("got %d findings, want %d:\n%s", len(got.Findings), len(f.want.Findings), gj)
			}
			for i, w := range f.want.Findings {
				g := got.Findings[i]
				if g.Code != w.Code || g.Severity != w.Severity || g.Path != w.Path || !reflect.DeepEqual(g.Profiles, w.Profiles) {
					t.Errorf("finding %d: got %s %s %s %v, want %s %s %s %v (%s)", i,
						g.Code, g.Severity, g.Path, g.Profiles, w.Code, w.Severity, w.Path, w.Profiles, g.Message)
				}
				for _, s := range w.MessageContains {
					if !strings.Contains(g.Message, s) {
						t.Errorf("finding %d (%s): message %q does not contain %q", i, g.Path, g.Message, s)
					}
				}
			}

			again := ConvertProfilesToInstances(f.in)
			if !reflect.DeepEqual(got, again) {
				t.Error("two conversions of the same input differ")
			}
		})
	}
}

// Round trip (b): every proposed instance, marshalled into the settings
// file and loaded by the strict instance loader, comes back deep-equal.
func TestConvert_RoundTripThroughStrictLoader(t *testing.T) {
	for _, name := range conversionFixtures {
		t.Run(name, func(t *testing.T) {
			f := loadConversionFixture(t, name)
			got := ConvertProfilesToInstances(f.in)
			if len(got.Instances) == 0 {
				return
			}
			var doc map[string]interface{}
			if err := yamlv3.Unmarshal(f.raw, &doc); err != nil {
				t.Fatal(err)
			}
			b, err := yamlv3.Marshal(got.Instances)
			if err != nil {
				t.Fatal(err)
			}
			var instances interface{}
			if err := yamlv3.Unmarshal(b, &instances); err != nil {
				t.Fatal(err)
			}
			server, _ := doc["server"].(map[string]interface{})
			if server == nil {
				server = map[string]interface{}{}
				doc["server"] = server
			}
			server["broker"] = map[string]interface{}{"instances": instances}
			out, err := yamlv3.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), string(out))
			loaded, err := LoadRuntimeBrokerInstances("")
			if err != nil {
				t.Fatalf("strict load of the converted instances: %v\n%s", err, out)
			}
			if !reflect.DeepEqual(loaded, got.Instances) {
				t.Fatalf("strict load differs from the conversion:\n%s", out)
			}
		})
	}
}

// Behavioural equivalence (c): for every converted profile with no
// blocking finding (and no operator choice that changed one of its
// values), the legacy view resolving that profile and the flat view of its
// instance give the same value from every tier accessor. Source labels
// differ by design and are not compared.
func TestConvert_FlatInstanceBehavesLikeItsProfile(t *testing.T) {
	compared := 0
	for _, name := range conversionFixtures {
		t.Run(name, func(t *testing.T) {
			f := loadConversionFixture(t, name)
			got := ConvertProfilesToInstances(f.in)
			chosen := map[string]bool{}
			for _, fd := range got.Findings {
				if fd.Code == ConversionProfileConflictResolved {
					for _, p := range fd.Profiles {
						chosen[p] = true
					}
				}
			}
			byKey := map[string]V1RuntimeBrokerInstanceConfig{}
			for _, inst := range got.Instances {
				byKey[inst.Key] = inst
			}
			var profiles []string
			for p := range got.ProfileMap {
				profiles = append(profiles, p)
			}
			sort.Strings(profiles)
			for _, p := range profiles {
				if chosen[p] {
					continue
				}
				inst, ok := byKey[got.ProfileMap[p]]
				if !ok {
					t.Fatalf("profile %s maps to %s, which is not an instance", p, got.ProfileMap[p])
				}
				assertFlatEquivalent(t, f.in.Settings, p, inst)
				compared++
			}
		})
	}
	if compared < 15 {
		t.Fatalf("only %d profiles were compared; the equivalence check is too narrow", compared)
	}
}

func assertFlatEquivalent(t *testing.T, vs *VersionedSettings, p string, inst V1RuntimeBrokerInstanceConfig) {
	t.Helper()
	flat := vs.ForFlatInstance(NewFlatInstancePolicy(inst))
	prof := vs.Profiles[p]
	rt := vs.Runtimes[prof.Runtime]
	check := func(what string, legacy, flatV interface{}) {
		t.Helper()
		if !reflect.DeepEqual(legacy, flatV) {
			t.Errorf("profile %s, %s: legacy %#v, flat %#v", p, what, legacy, flatV)
		}
	}

	// Dirs and harness configs any of the tiers names, plus one none does.
	dirs := map[string]bool{"unnamed-dir": true}
	for _, m := range []map[string]string{prof.SharedDirStorageBackends, rt.SharedDirStorageBackends, inst.RuntimeTarget.SharedDirStorageBackends} {
		for d := range m {
			dirs[d] = true
		}
	}
	hcs := map[string]bool{"unnamed-hc": true}
	for hc := range vs.HarnessConfigs {
		hcs[hc] = true
	}
	for hc := range prof.HarnessOverrides {
		hcs[hc] = true
	}
	if inst.AgentDefaults != nil {
		for hc := range inst.AgentDefaults.HarnessOverrides {
			hcs[hc] = true
		}
	}

	// Docker and Kubernetes.
	for d := range dirs {
		lb, _, lp := vs.ResolveSharedDirStorageBackend(p, d)
		fb, _, fp := flat.ResolveSharedDirStorageBackend("", d)
		check("shared dir backend of "+d, []interface{}{lb, lp}, []interface{}{fb, fp})
	}
	lcfg, _ := vs.ResolveSharedDirStorage(p)
	fcfg, _ := flat.ResolveSharedDirStorage("")
	check("shared dir storage", lcfg, fcfg)
	lcd, _ := vs.ResolveCloneDepthWithSource(p)
	fcd, _ := flat.ResolveCloneDepthWithSource("")
	check("clone depth", lcd, fcd)
	check("image registry", vs.ResolveImageRegistry(p), flat.ResolveImageRegistry(""))
	for hc := range hcs {
		lhc, lerr := vs.ResolveHarnessConfig(p, hc)
		fhc, ferr := flat.ResolveHarnessConfig("", hc)
		check("harness config "+hc, []interface{}{lhc, lerr}, []interface{}{fhc, ferr})
		check("override image "+hc, vs.ProfileHarnessOverrideImage(p, hc), flat.ProfileHarnessOverrideImage("", hc))
		check("override pull policy "+hc, vs.ProfileHarnessOverrideImagePullPolicy(p, hc), flat.ProfileHarnessOverrideImagePullPolicy("", hc))
		lr, lhr, _ := vs.TierResources(p, hc)
		fr, fhr, _ := flat.TierResources("", hc)
		check("tier resources "+hc, []interface{}{lr, lhr}, []interface{}{fr, fhr})
		lo, lok, _ := vs.TierHarnessOverride(p, hc)
		fo, fok, _ := flat.TierHarnessOverride("", hc)
		check("tier harness override "+hc, []interface{}{lo, lok}, []interface{}{fo, fok})
	}
	ls, _ := vs.TierSecrets(p)
	fs, _ := flat.TierSecrets("")
	check("tier secrets", ls, fs)
	lk, _ := vs.TierAllHarnessOverrideEnvKeys(p)
	fk, _ := flat.TierAllHarnessOverrideEnvKeys("")
	check("tier empty env keys", lk, fk)
	ld, _ := vs.TierDefaultHarnessConfig(p)
	fd, _ := flat.TierDefaultHarnessConfig("")
	check("tier default harness config", ld, fd)

	if inst.RuntimeTarget.Type != RuntimeTargetTypeKubernetes {
		return
	}
	// Kubernetes only: a docker runtime ignores these settings.
	lc, lsz := vs.ResolveSharedDirDefaults(p)
	fc, fsz := flat.ResolveSharedDirDefaults("")
	check("shared dir defaults", []string{lc, lsz}, []string{fc, fsz})
	check("safe_to_evict", vs.ResolveSafeToEvict(p), flat.ResolveSafeToEvict(""))
	lh, fh := vs.ResolveHomeStorage(p), flat.ResolveHomeStorage("")
	check("home storage", []string{lh.Backend, lh.Leaf}, []string{fh.Backend, fh.Leaf})
	lrt, _, err := vs.ResolveRuntime(p)
	if err != nil {
		t.Fatalf("profile %s: %v", p, err)
	}
	frt := inst.RuntimeTarget.KubernetesRuntimeConfig()
	check("runtime construction", []interface{}{lrt.Context, lrt.Namespace, lrt.GKE, lrt.PriorityClassName},
		[]interface{}{frt.Context, frt.Namespace, frt.GKE, frt.PriorityClassName})
	gsas := map[string]bool{"unmapped@proj.iam.gserviceaccount.com": true}
	for _, m := range []map[string]string{prof.KubernetesServiceAccountMappings, rt.KubernetesServiceAccountMappings} {
		for g := range m {
			gsas[strings.ToLower(g)] = true
		}
	}
	for g := range gsas {
		lksa, lok := vs.ResolveKubernetesServiceAccountMappingForSelection(p, prof.Runtime, g)
		// The flat instance's lookup (pkg/runtimebroker flatKubernetesIdentityPolicy).
		fksa, fok := inst.RuntimeTarget.KubernetesServiceAccountMappings[g]
		fok = fok && fksa != ""
		if !fok {
			fksa = ""
		}
		check("service account mapping of "+g, []interface{}{lksa, lok}, []interface{}{fksa, fok})
	}
	lb, lbok := vs.ResolveKubernetesBlockServiceAccountForSelection(p, prof.Runtime)
	fb := inst.RuntimeTarget.KubernetesBlockServiceAccount
	check("block service account", []interface{}{lb, lbok}, []interface{}{fb, fb != ""})
}

// B4: a profile's single backend shadows its runtime entry's per-dir map.
// Dir x is nfs for the legacy profile and on the converted instance; an
// independent per-key merge would put it on local.
func TestConvert_PerDirShadowedByProfileSingle(t *testing.T) {
	f := loadConversionFixture(t, "per_dir_shadowed_by_profile_single")
	got := ConvertProfilesToInstances(f.in)
	if len(got.Instances) != 1 {
		t.Fatalf("want one instance, got %+v", got.Instances)
	}
	legacy, _, _ := f.in.Settings.ResolveSharedDirStorageBackend("batch", "x")
	flat, _, _ := f.in.Settings.ForFlatInstance(NewFlatInstancePolicy(got.Instances[0])).ResolveSharedDirStorageBackend("", "x")
	if legacy != "nfs" || flat != "nfs" {
		t.Fatalf("dir x: legacy %q, flat %q, want nfs for both", legacy, flat)
	}
	if m := got.Instances[0].RuntimeTarget.SharedDirStorageBackends; len(m) != 0 {
		t.Fatalf("the instance must not carry the shadowed per-dir map, got %v", m)
	}
}

// An unknown key under an instance's agent_defaults is a strict-load error
// naming its path (the instance side of the unknown_keys fixture).
func TestConvert_UnknownInstanceKeyFailsStrictLoad(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(conversionFixtureDir("unknown_keys"), "instance_unknown_key.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), string(b))
	_, err = LoadRuntimeBrokerInstances("")
	want := "server.broker.instances[0].agent_defaults.templat: unknown key"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an error containing %q", err, want)
	}
}

// A docker instance that sets Kubernetes-only fields is refused at strict
// load, naming each field (the instance side of docker_rejects_k8s_fields).
func TestConvert_DockerInstanceRefusesKubernetesFields(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(conversionFixtureDir("docker_rejects_k8s_fields"), "instance_with_k8s_fields.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), string(b))
	_, err = LoadRuntimeBrokerInstances("")
	if err == nil {
		t.Fatal("want a strict-load error")
	}
	for _, field := range []string{"gke", "priority_class_name", "shared_dir_storage_class", "shared_dir_size",
		"safe_to_evict", "home_storage_backend", "home_storage_leaf"} {
		path := "server.broker.instances[0].runtime_target." + field
		if !strings.Contains(err.Error(), path) {
			t.Errorf("the error does not name %s: %v", path, err)
		}
	}
	if !strings.Contains(err.Error(), "field not valid for runtime target type docker") {
		t.Errorf("got %v", err)
	}
}

// Secret declarations are copied as declarations; no secret value is read,
// resolved or emitted, even when the environment holds one, and a
// conflicting harness override env shows only its keys. The converter
// depends on no secret, Hub or environment package (import guard).
func TestConvert_NeverMaterializesSecretValues(t *testing.T) {
	t.Setenv("API_TOKEN", "VALUE-FROM-ENVIRONMENT")
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "cluster",
				Secrets: []api.RequiredSecret{{Key: "API_TOKEN", Type: "environment", Description: "token"},
					{Key: "CREDS", Type: "file", Target: "/etc/creds.json", AlternativeEnvKeys: []string{"GOOGLE_APPLICATION_CREDENTIALS"}}},
				HarnessOverrides: map[string]V1HarnessOverride{"claude": {Env: map[string]string{"TOKEN": "INLINE-VALUE-A"}}}},
			"b": {Runtime: "cluster",
				Secrets: []api.RequiredSecret{{Key: "CREDS", Type: "file", Target: "/etc/creds.json", AlternativeEnvKeys: []string{"GOOGLE_APPLICATION_CREDENTIALS"}},
					{Key: "API_TOKEN", Type: "environment", Description: "token"}},
				HarnessOverrides: map[string]V1HarnessOverride{"claude": {Env: map[string]string{"TOKEN": "INLINE-VALUE-B"}}}},
		},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"VALUE-FROM-ENVIRONMENT", "INLINE-VALUE-A", "INLINE-VALUE-B"} {
		if strings.Contains(string(out), v) {
			t.Errorf("the conversion result contains %s: %s", v, out)
		}
	}
	if len(got.Instances) != 1 || got.Instances[0].AgentDefaults == nil {
		t.Fatalf("got %+v", got.Instances)
	}
	// The two profiles declare the same set in another order: they agree,
	// and the declarations are copied as written by the first profile.
	if want := vs.Profiles["a"].Secrets; !reflect.DeepEqual(got.Instances[0].AgentDefaults.Secrets, want) {
		t.Fatalf("secrets: got %+v, want %+v", got.Instances[0].AgentDefaults.Secrets, want)
	}
	if &got.Instances[0].AgentDefaults.Secrets[0] == &vs.Profiles["a"].Secrets[0] {
		t.Fatal("the declarations share memory with the settings")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "profile_conversion.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"crypto/sha256": true, "encoding/hex": true, "encoding/json": true, "fmt": true, "reflect": true,
		"sort": true, "strings": true, "gopkg.in/yaml.v3": true, "github.com/GoogleCloudPlatform/scion/pkg/api": true,
	}
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if !allowed[path] {
			t.Errorf("profile_conversion.go imports %s; the converter must not depend on secret resolution or the environment", path)
		}
	}
}

// Invalid operator choices are blocking findings that keep the named
// group's profiles out of the profile map; nothing is chosen in their
// place. In each case the group's own conflicts are resolved by valid
// choices (choices[0] and choices[1]), so the invalid choice is the only
// thing that can block the profiles.
func TestConvert_InvalidChoicesBlock(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "cluster", ImageRegistry: "reg/a", SharedDirStorageBackend: "nfs"},
			"b": {Runtime: "cluster", ImageRegistry: "reg/b", SharedDirStorageBackend: "local"},
		},
	}
	valid := []ConversionChoice{
		{TargetGroup: "k8s-agents", Field: "image_registry", ChosenProfile: "a"},
		{TargetGroup: "k8s-agents", Field: "shared_dir_storage_backend", ChosenProfile: "b"},
	}
	// Without an invalid choice the group converts cleanly.
	clean := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Choices: valid})
	if !reflect.DeepEqual(clean.ProfileMap, map[string]string{"a": "k8s-agents", "b": "k8s-agents"}) {
		t.Fatalf("valid choices only: profile map %v, findings %+v", clean.ProfileMap, clean.Findings)
	}
	for name, tc := range map[string]struct {
		invalid ConversionChoice
		blocks  bool // an unknown group names no profiles to block
	}{
		"unknown group":          {ConversionChoice{TargetGroup: "nope", Field: "clone_depth", ChosenProfile: "a"}, false},
		"profile not in group":   {ConversionChoice{TargetGroup: "k8s-agents", Field: "clone_depth", ChosenProfile: "zz"}, true},
		"field without conflict": {ConversionChoice{TargetGroup: "k8s-agents", Field: "clone_depth", ChosenProfile: "a"}, true},
		"duplicate field choice": {ConversionChoice{TargetGroup: "k8s-agents", Field: "image_registry", ChosenProfile: "b"}, true},
		"split and field":        {ConversionChoice{TargetGroup: "k8s-agents", Field: "clone_depth", ChosenProfile: "a", Split: []string{"a"}}, true},
		"split unknown profile":  {ConversionChoice{TargetGroup: "k8s-agents", Split: []string{"zz"}}, true},
		"empty template":         {ConversionChoice{TargetGroup: "k8s-agents", Field: "clone_depth", ChosenProfile: ConversionChoiceTemplatePrefix}, true},
	} {
		t.Run(name, func(t *testing.T) {
			choices := append(append([]ConversionChoice(nil), valid...), tc.invalid)
			got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Choices: choices})
			var f *ConversionFinding
			for i := range got.Findings {
				if got.Findings[i].Path == "choices[2]" {
					f = &got.Findings[i]
				}
			}
			if f == nil || f.Code != ConversionUnknownSettingsKey || f.Severity != ConversionSeverityBlocking {
				t.Fatalf("no blocking unknown_settings_key finding at choices[2]: %+v", got.Findings)
			}
			if tc.blocks && len(got.ProfileMap) != 0 {
				t.Fatalf("profile map %v, want none", got.ProfileMap)
			}
			if !tc.blocks && len(got.ProfileMap) != 2 {
				t.Fatalf("profile map %v, want both profiles", got.ProfileMap)
			}
			if d := got.Instances[0].AgentDefaults; d == nil || d.ImageRegistry != "reg/a" || d.CloneDepth != "" {
				t.Fatalf("agent_defaults %+v, want only the chosen image registry", d)
			}
		})
	}
}

// A template choice is refused for an infrastructure field, which no
// template can set: the conflict stays reported and the field stays unset,
// rather than silently falling through to the global setting.
func TestConvert_TemplateChoiceRefusedForInfraField(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "cluster", SharedDirStorageBackend: "nfs", HomeStorageBackend: "nfs", SharedDirSize: "10Gi"},
			"b": {Runtime: "cluster", SharedDirStorageBackend: "local", HomeStorageBackend: "local", SharedDirSize: "20Gi"},
		},
	}
	for i, field := range []string{"shared_dir_storage_backend", "home_storage_backend", "shared_dir_size"} {
		choice := ConversionChoice{TargetGroup: "k8s-agents", Field: field, ChosenProfile: ConversionChoiceTemplatePrefix + "batch"}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Choices: []ConversionChoice{choice}})
		refused, conflict := false, false
		for _, f := range got.Findings {
			if f.Path == "choices[0]" && f.Code == ConversionUnknownSettingsKey && f.Severity == ConversionSeverityBlocking &&
				strings.Contains(f.Message, "no template can set it") {
				refused = true
			}
			if f.Path == "profiles.*."+field {
				if f.Code != ConversionProfileConflict {
					t.Errorf("case %d: %s is %s, want an unresolved profile_conflict", i, field, f.Code)
				}
				conflict = true
			}
		}
		if !refused || !conflict {
			t.Fatalf("%s: refused=%v conflict=%v: %+v", field, refused, conflict, got.Findings)
		}
		if len(got.ProfileMap) != 0 {
			t.Fatalf("%s: profile map %v, want none", field, got.ProfileMap)
		}
	}
}

// Distinct target scopes, and a Split group, never share an instance key,
// and the profile map sends each profile to the instance of its own scope.
func TestConvert_InstanceKeysUniqueAcrossScopes(t *testing.T) {
	for name, tc := range map[string]struct {
		runtimes map[string]V1RuntimeConfig
		profiles map[string]V1ProfileConfig
		choices  []ConversionChoice
	}{
		"context and namespace vs namespace": {
			runtimes: map[string]V1RuntimeConfig{
				"a": {Type: "kubernetes", Context: "prod", Namespace: "east"},
				"b": {Type: "kubernetes", Namespace: "prod-east"}},
			profiles: map[string]V1ProfileConfig{"pa": {Runtime: "a"}, "pb": {Runtime: "b"}},
		},
		"context vs namespace": {
			runtimes: map[string]V1RuntimeConfig{
				"a": {Type: "kubernetes", Context: "x"},
				"b": {Type: "kubernetes", Namespace: "x"}},
			profiles: map[string]V1ProfileConfig{"pa": {Runtime: "a"}, "pb": {Runtime: "b"}},
		},
		"split key vs another scope": {
			runtimes: map[string]V1RuntimeConfig{
				"a": {Type: "kubernetes"},
				"b": {Type: "kubernetes", Namespace: "agents"}},
			profiles: map[string]V1ProfileConfig{"agents": {Runtime: "a"}, "rest": {Runtime: "a"}, "pb": {Runtime: "b"}},
			choices:  []ConversionChoice{{TargetGroup: "k8s", Split: []string{"agents"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			vs := &VersionedSettings{Runtimes: tc.runtimes, Profiles: tc.profiles}
			in := ProfileConversionInput{Settings: vs, Choices: tc.choices}
			got := ConvertProfilesToInstances(in)
			if len(got.Findings) != 0 {
				t.Fatalf("findings %+v", got.Findings)
			}
			byKey := map[string]V1RuntimeBrokerInstanceConfig{}
			for _, inst := range got.Instances {
				if _, dup := byKey[inst.Key]; dup {
					t.Fatalf("duplicate instance key %q in %+v", inst.Key, got.Instances)
				}
				if !runtimeBrokerInstanceKeyRe.MatchString(inst.Key) {
					t.Errorf("%q is not a valid instance key", inst.Key)
				}
				byKey[inst.Key] = inst
			}
			if len(got.ProfileMap) != len(tc.profiles) {
				t.Fatalf("profile map %v", got.ProfileMap)
			}
			keys := map[string]string{}
			for p, key := range got.ProfileMap {
				inst, ok := byKey[key]
				if !ok {
					t.Fatalf("profile %s maps to %q, which is not an instance", p, key)
				}
				rt := tc.runtimes[tc.profiles[p].Runtime]
				if inst.RuntimeTarget.Context != rt.Context || inst.RuntimeTarget.Namespace != rt.Namespace {
					t.Errorf("profile %s maps to %s with scope %q/%q, want %q/%q", p, key,
						inst.RuntimeTarget.Context, inst.RuntimeTarget.Namespace, rt.Context, rt.Namespace)
				}
				keys[p] = key
			}
			if tc.choices != nil && keys["agents"] == keys["rest"] {
				t.Errorf("the split profile shares its key with the rest of its group: %v", keys)
			}
			if again := ConvertProfilesToInstances(in); !reflect.DeepEqual(got, again) {
				t.Error("the keys are not deterministic")
			}
		})
	}
}

// A harness override that only one profile has is reported once per
// conflicting sub-field, not again for its presence; an override that
// differs only in presence is reported once, for its presence.
func TestConvert_HarnessOverridePresenceReportedOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		override V1HarnessOverride
		want     string
	}{
		"with a sub-field": {V1HarnessOverride{Image: "claude:2"}, "profiles.*.harness_overrides.claude.image"},
		"empty override":   {V1HarnessOverride{}, "profiles.*.harness_overrides.claude"},
	} {
		t.Run(name, func(t *testing.T) {
			vs := &VersionedSettings{
				Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
				Profiles: map[string]V1ProfileConfig{
					"a": {Runtime: "cluster", HarnessOverrides: map[string]V1HarnessOverride{"claude": tc.override}},
					"b": {Runtime: "cluster"},
				},
			}
			got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
			if len(got.Findings) != 1 || got.Findings[0].Path != tc.want || got.Findings[0].Code != ConversionProfileConflict {
				t.Fatalf("findings %+v, want one profile_conflict at %s", got.Findings, tc.want)
			}
			if d := got.Instances[0].AgentDefaults; d != nil {
				t.Fatalf("agent_defaults %+v, want none", d)
			}
		})
	}
}

// Profiles in scope: only the named profiles convert, a named profile the
// settings lack is a blocking finding, and the input order does not matter.
func TestConvert_ProfilesInScope(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"local": {Type: "docker"}, "pod": {Type: "podman"}},
		Profiles: map[string]V1ProfileConfig{"a": {Runtime: "local"}, "b": {Runtime: "local"}, "p": {Runtime: "pod"}},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"missing", "b", "a", "b"}})
	if !reflect.DeepEqual(got.ProfileMap, map[string]string{"a": "docker", "b": "docker"}) {
		t.Fatalf("profile map %v", got.ProfileMap)
	}
	if len(got.Findings) != 1 || got.Findings[0].Path != "profiles.missing" || got.Findings[0].Severity != ConversionSeverityBlocking {
		t.Fatalf("findings %+v", got.Findings)
	}
	again := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"a", "missing", "b"}})
	if !reflect.DeepEqual(got, again) {
		t.Fatal("the result depends on the order of the profiles in scope")
	}
}

// Instance keys are a function of the target scope only, are always valid
// instance keys, and differ for scopes that make the same key-safe text.
func TestConversionKey_ValidAndDistinct(t *testing.T) {
	cases := []struct{ seed, prefix, ctx, ns string }{
		{"a", "k8s", "", "agents"},
		{"b", "k8s", "gke_proj_us-west1_cluster", "agents"},
		{"c", "k8s", "gke-proj-us-west1-cluster", "agents"},
		{"d", "k8s", strings.Repeat("very-long-context-", 6), "agents"},
		{"e", "k8s", "Kind.A", "Team_1"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		key := conversionKey(c.seed, c.prefix, c.ctx, c.ns)
		if !runtimeBrokerInstanceKeyRe.MatchString(key) {
			t.Errorf("%q is not a valid instance key", key)
		}
		if seen[key] {
			t.Errorf("key %q is not distinct", key)
		}
		seen[key] = true
		if again := conversionKey(c.seed, c.prefix, c.ctx, c.ns); again != key {
			t.Errorf("key %q is not deterministic (%q)", key, again)
		}
	}
	if got := conversionKey("a", "k8s", "", "agents"); got != "k8s-agents" {
		t.Errorf("a key-safe scope keeps its readable key, got %q", got)
	}
}
