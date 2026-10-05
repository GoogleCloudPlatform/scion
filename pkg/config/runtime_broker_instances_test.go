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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

const oneDockerInstanceYAML = `schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: local-docker
        name: example-docker
        runtime_target:
          type: docker
          display_name: Local Docker
`

// flatTestHome points HOME at a temp dir and returns the global .scion dir.
func flatTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	global := filepath.Join(home, GlobalDir)
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	return global
}

func writeFlatTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dockerInstance(key, name string) V1RuntimeBrokerInstanceConfig {
	return V1RuntimeBrokerInstanceConfig{Key: key, Name: name, RuntimeTarget: &V1RuntimeTargetConfig{Type: RuntimeTargetTypeDocker}}
}

func expectValidationPaths(t *testing.T, errs []ValidationError, wantPathSubstr ...string) {
	t.Helper()
	for _, w := range wantPathSubstr {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Path, w) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a validation error at %q, got %v", w, errs)
		}
	}
}

func TestRuntimeBrokerInstances_OneDockerEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFlatTestFile(t, filepath.Join(dir, "settings.yaml"), oneDockerInstanceYAML)
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.Server == nil || vs.Server.Broker == nil || len(vs.Server.Broker.Instances) != 1 {
		t.Fatalf("instance not loaded: %+v", vs.Server)
	}
	got := vs.Server.Broker.Instances[0]
	want := V1RuntimeBrokerInstanceConfig{Key: "local-docker", Name: "example-docker",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker", DisplayName: "Local Docker"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// save -> load -> save is byte-stable.
	out1 := t.TempDir()
	if err := SaveVersionedSettings(out1, vs); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(filepath.Join(out1, "settings.yaml"))
	vs2, err := LoadSingleFileVersioned(out1)
	if err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := SaveVersionedSettings(out2, vs2); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(filepath.Join(out2, "settings.yaml"))
	if string(b1) != string(b2) {
		t.Fatalf("round trip not byte-stable:\n%s\n---\n%s", b1, b2)
	}
	if !strings.Contains(string(b1), "instances:") || !strings.Contains(string(b1), "runtime_target:") {
		t.Fatalf("saved settings lost the instance:\n%s", b1)
	}
}

func TestRuntimeBrokerInstances_GlobalConfigRoundTrip(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), oneDockerInstanceYAML)
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	want := []RuntimeBrokerInstanceConfig{{Key: "local-docker", Name: "example-docker",
		RuntimeTarget: &RuntimeTargetConfig{Type: "docker", DisplayName: "Local Docker"}}}
	if !reflect.DeepEqual(gc.RuntimeBroker.Instances, want) {
		t.Fatalf("GlobalConfig instances: got %+v, want %+v", gc.RuntimeBroker.Instances, want)
	}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v1.Broker.Instances, strict) {
		t.Fatalf("back-conversion differs from settings: %+v vs %+v", v1.Broker.Instances, strict)
	}
	if !reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(strict), gc.RuntimeBroker.Instances) {
		t.Fatal("startup comparison mapping must equal the GlobalConfig conversion")
	}
}

func TestRuntimeBrokerInstances_StrictLoaderRejectsTypeErrorAndUnknownKey(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key": `schema_version: "1"
server:
  broker:
    instances:
      - key: local-docker
        name: n
        profile: local
        runtime_target:
          type: docker
`,
		"type error": `schema_version: "1"
server:
  broker:
    instances:
      - key: local-docker
        name: n
        runtime_target: docker
`,
	} {
		t.Run(name, func(t *testing.T) {
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), body)
			if _, err := LoadRuntimeBrokerInstances(""); err == nil {
				t.Fatal("strict loader must reject this")
			}
		})
	}
}

func TestRuntimeBrokerInstances_StrictLoaderFileResolution(t *testing.T) {
	t.Run("global server that fails to unmarshal is still the source", func(t *testing.T) {
		global := flatTestHome(t)
		// server.broker.port has a type error: the lenient server loader
		// drops the whole section, but the strict loader still reads it
		// and never falls through to --config.
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), `server:
  broker:
    port: not-a-number
    instances:
      - key: from-global
        name: g
        runtime_target:
          type: docker
`)
		cfgDir := t.TempDir()
		writeFlatTestFile(t, filepath.Join(cfgDir, "settings.yaml"), strings.ReplaceAll(oneDockerInstanceYAML, "local-docker", "from-config"))
		got, err := LoadRuntimeBrokerInstances(cfgDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Key != "from-global" {
			t.Fatalf("expected the global entry, got %+v", got)
		}
	})
	t.Run("unparseable global settings is an error", func(t *testing.T) {
		global := flatTestHome(t)
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), "server: [unclosed\n")
		if _, err := LoadRuntimeBrokerInstances(t.TempDir()); err == nil {
			t.Fatal("an unparseable global settings.yaml must be an error")
		}
	})
	t.Run("config dir used only without a global server key", func(t *testing.T) {
		global := flatTestHome(t)
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), "schema_version: \"1\"\n")
		cfgDir := t.TempDir()
		writeFlatTestFile(t, filepath.Join(cfgDir, "settings.yaml"), oneDockerInstanceYAML)
		got, err := LoadRuntimeBrokerInstances(cfgDir)
		if err != nil || len(got) != 1 || got[0].Key != "local-docker" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestRuntimeBrokerInstances_LegacyServerYAMLRejected(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "server.yaml"), `runtimeBroker:
  enabled: true
  instances:
    - key: local-docker
      name: n
      runtimeTarget:
        type: docker
`)
	_, err := LoadGlobalConfig("")
	if !errors.Is(err, ErrRuntimeBrokerInstancesInServerYAML) {
		t.Fatalf("want ErrRuntimeBrokerInstancesInServerYAML, got %v", err)
	}
}

func TestRuntimeBrokerInstances_ProjectSettingsIgnoredByServer(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n")
	// A project-level settings file is never consulted for instances.
	proj := t.TempDir()
	writeFlatTestFile(t, filepath.Join(proj, DotScion, "settings.yaml"), oneDockerInstanceYAML)
	t.Chdir(proj)
	got, err := LoadRuntimeBrokerInstances("")
	if err != nil || len(got) != 0 {
		t.Fatalf("project settings must be ignored, got %+v, %v", got, err)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.RuntimeBroker.Instances) != 0 {
		t.Fatalf("server config must not pick up project instances: %+v", gc.RuntimeBroker.Instances)
	}
}

func TestRuntimeBrokerInstances_EmptyListIsLegacy(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    instances: []\n")
	got, err := LoadRuntimeBrokerInstances("")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil || len(gc.RuntimeBroker.Instances) != 0 {
		t.Fatalf("empty list must be legacy: %+v, %v", gc, err)
	}
	if err := CheckRuntimeBrokerInstanceHosting(got, false); err != nil {
		t.Fatalf("empty list must be allowed anywhere: %v", err)
	}
}

func TestRuntimeBrokerInstances_DuplicateKeyRejected(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{dockerInstance("a", "x"), dockerInstance("a", "y")})
	found := false
	for _, e := range errs {
		if e.Path == "server.broker.instances[1].key" && strings.Contains(e.Message, "duplicate instance key") {
			found = true
		}
	}
	if !found {
		t.Fatalf("duplicate key must be reported even with the count rule: %v", errs)
	}
}

func TestRuntimeBrokerInstances_MultipleEntriesRejected(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{dockerInstance("a", "x"), dockerInstance("b", "y")})
	expectValidationPaths(t, errs, "server.broker.instances")
	if !strings.Contains(errs[0].Message, "only 1 Runtime Broker instance") {
		t.Fatalf("got %v", errs)
	}
}

func TestRuntimeBrokerInstances_InvalidKeyRejected(t *testing.T) {
	for _, k := range []string{"", "Bad", "-x", "x-", "a_b", strings.Repeat("a", 64)} {
		errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{dockerInstance(k, "n")})
		expectValidationPaths(t, errs, "[0].key")
	}
	if errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{dockerInstance("local-docker-1", "n")}); len(errs) != 0 {
		t.Fatalf("valid key rejected: %v", errs)
	}
}

func TestRuntimeBrokerInstances_NameRequired(t *testing.T) {
	expectValidationPaths(t, ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{dockerInstance("a", "")}), "[0].name")
}

func TestRuntimeBrokerInstances_TargetTypeRequired(t *testing.T) {
	expectValidationPaths(t, ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n"}}), "[0].runtime_target.type")
	expectValidationPaths(t, ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n", RuntimeTarget: &V1RuntimeTargetConfig{}}}), "[0].runtime_target.type")
}

func TestRuntimeBrokerInstances_KubernetesNotImplemented(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", Context: "c", Namespace: "ns"}}})
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "not implemented yet") {
		t.Fatalf("got %v", errs)
	}
}

func TestRuntimeBrokerInstances_UnsupportedTypeRejected(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n", RuntimeTarget: &V1RuntimeTargetConfig{Type: "podman"}}})
	if len(errs) != 1 || !strings.Contains(errs[0].Message, `unsupported runtime target type "podman"`) {
		t.Fatalf("got %v", errs)
	}
}

func TestRuntimeBrokerInstances_DockerRejectsKubernetesFields(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker", Context: "c", Namespace: "ns"}}})
	expectValidationPaths(t, errs, "runtime_target.context", "runtime_target.namespace")
}

// TestRuntimeBrokerInstances_SchemaMatchesValidator: scion config validate
// (the JSON schema) and the validator reject and accept the same cases.
func TestRuntimeBrokerInstances_SchemaMatchesValidator(t *testing.T) {
	cases := map[string]struct {
		yaml  string
		valid bool
	}{
		"one docker":        {oneDockerInstanceYAML, true},
		"empty list":        {"schema_version: \"1\"\nserver:\n  broker:\n    instances: []\n", true},
		"two entries":       {instancesYAML("- {key: a, name: x, runtime_target: {type: docker}}\n      - {key: b, name: y, runtime_target: {type: docker}}"), false},
		"duplicate key":     {instancesYAML("- {key: a, name: x, runtime_target: {type: docker}}\n      - {key: a, name: y, runtime_target: {type: docker}}"), false},
		"bad key":           {instancesYAML("- {key: Bad, name: x, runtime_target: {type: docker}}"), false},
		"missing name":      {instancesYAML("- {key: a, runtime_target: {type: docker}}"), false},
		"missing target":    {instancesYAML("- {key: a, name: x}"), false},
		"kubernetes":        {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, context: c, namespace: n}}"), false},
		"unsupported type":  {instancesYAML("- {key: a, name: x, runtime_target: {type: podman}}"), false},
		"docker with ns":    {instancesYAML("- {key: a, name: x, runtime_target: {type: docker, namespace: n}}"), false},
		"unknown entry key": {instancesYAML("- {key: a, name: x, profile: p, runtime_target: {type: docker}}"), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			schemaErrs, err := ValidateSettings([]byte(c.yaml), "1")
			if err != nil {
				t.Fatal(err)
			}
			if (len(schemaErrs) == 0) != c.valid {
				t.Fatalf("schema: valid=%v, errors=%v", len(schemaErrs) == 0, schemaErrs)
			}
			dir := t.TempDir()
			writeFlatTestFile(t, filepath.Join(dir, "settings.yaml"), c.yaml)
			t.Setenv("HOME", dir)
			if err := os.MkdirAll(filepath.Join(dir, GlobalDir), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFlatTestFile(t, filepath.Join(dir, GlobalDir, "settings.yaml"), c.yaml)
			_, loadErr := LoadRuntimeBrokerInstances("")
			if (loadErr == nil) != c.valid {
				t.Fatalf("validator: valid=%v, err=%v", loadErr == nil, loadErr)
			}
		})
	}
}

func instancesYAML(entries string) string {
	return "schema_version: \"1\"\nserver:\n  broker:\n    instances:\n      " + entries + "\n"
}

func TestRuntimeBrokerInstances_OverlayDoesNotTouchInstances(t *testing.T) {
	inst := []V1RuntimeBrokerInstanceConfig{dockerInstance("local-docker", "example-docker")}
	vs := &VersionedSettings{SchemaVersion: "1", Server: &V1ServerConfig{Broker: &V1BrokerConfig{Instances: inst}}}
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{"docker": {Type: "docker"}}, map[string]V1ProfileConfig{"local": {Runtime: "docker"}}, map[string]HarnessConfigEntry{}, "reg.example")
	o.Apply(vs)
	if !reflect.DeepEqual(vs.Server.Broker.Instances, inst) {
		t.Fatalf("overlay changed instances: %+v", vs.Server.Broker.Instances)
	}
}

func TestRuntimeBrokerInstances_LegacyConfigUnchanged(t *testing.T) {
	global := flatTestHome(t)
	legacy := "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    port: 9810\n    broker_id: legacy-id\n"
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), legacy)
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if gc.RuntimeBroker.Instances != nil || gc.RuntimeBroker.BrokerID != "legacy-id" || gc.RuntimeBroker.Port != 9810 {
		t.Fatalf("legacy config changed: %+v", gc.RuntimeBroker)
	}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	if v1.Broker.Instances != nil {
		t.Fatal("legacy config must not gain instances")
	}
	if errs, err := ValidateSettings([]byte(legacy), "1"); err != nil || len(errs) != 0 {
		t.Fatalf("legacy config must validate: %v %v", errs, err)
	}
}

func TestRuntimeBrokerInstanceHosting_RemoteRefused(t *testing.T) {
	global := flatTestHome(t)
	// Saved instance-scoped and legacy credentials must not change the answer.
	writeFlatTestFile(t, filepath.Join(global, "runtime-brokers", "local-docker", "hub-credentials", "hub.json"), `{"brokerId":"b","secretKey":"s"}`)
	writeFlatTestFile(t, filepath.Join(global, "broker-credentials.json"), `{"brokerId":"legacy","secretKey":"s"}`)
	writeFlatTestFile(t, filepath.Join(global, "hub-credentials", "hub.json"), `{"brokerId":"legacy","secretKey":"s"}`)
	inst := []V1RuntimeBrokerInstanceConfig{dockerInstance("local-docker", "example-docker")}

	err := CheckRuntimeBrokerInstanceHosting(inst, false)
	var he *RuntimeBrokerHostingError
	if !errors.As(err, &he) || he.Code() != api.ErrCodeFlatRuntimeBrokerRemoteUnsupported {
		t.Fatalf("remote flat hosting must be refused with %s, got %v", api.ErrCodeFlatRuntimeBrokerRemoteUnsupported, err)
	}
	if !strings.Contains(err.Error(), "remote flat Runtime Broker hosting arrives in P2") || !strings.Contains(err.Error(), "local-docker") {
		t.Fatalf("message must give guidance: %v", err)
	}
	// --simulate-remote-broker: the Hub runs in process but the embedded
	// registration (colocatedBrokerRegisters) does not, so hubInProcess is false.
	if err := CheckRuntimeBrokerInstanceHosting(inst, false); err == nil {
		t.Fatal("simulated remote must be refused")
	}
	if err := CheckRuntimeBrokerInstanceHosting(nil, false); err != nil {
		t.Fatalf("legacy remote hosting must be unchanged: %v", err)
	}
	if err := CheckRuntimeBrokerInstanceHosting(inst, true); err != nil {
		t.Fatalf("co-located flat hosting must be allowed: %v", err)
	}
}
