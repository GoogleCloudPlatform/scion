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
		// The lenient server loader silently falls back to the --config
		// settings; the startup comparison must therefore detect a
		// difference (the P1.2 refusal trigger).
		gc, err := LoadGlobalConfig(cfgDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(gc.RuntimeBroker.Instances) != 1 || gc.RuntimeBroker.Instances[0].Key != "from-config" {
			t.Fatalf("expected the lenient loader to fall back to --config, got %+v", gc.RuntimeBroker.Instances)
		}
		if reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(got), gc.RuntimeBroker.Instances) {
			t.Fatal("strict and lenient results must differ in the silent-fallback case")
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
		t.Fatalf("duplicate key must be reported: %v", errs)
	}
}

// TestRuntimeBrokerInstances_MultipleEntriesAccepted: P2.1 removes the
// one-instance limit; distinct valid entries are accepted (duplicate keys
// are still rejected, see DuplicateKeyRejected).
func TestRuntimeBrokerInstances_MultipleEntriesAccepted(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{dockerInstance("a", "x"), dockerInstance("b", "y"), dockerInstance("c", "z")})
	if len(errs) != 0 {
		t.Fatalf("distinct valid entries must be accepted: %v", errs)
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

// TestRuntimeBrokerInstances_KubernetesAccepted: P2.1-K implements the
// kubernetes target (contract amendment K1/K2): context, namespace and an
// absolute kubeconfig path are optional; a kubeconfig that is not one
// absolute local path is rejected.
func TestRuntimeBrokerInstances_KubernetesAccepted(t *testing.T) {
	for name, target := range map[string]*V1RuntimeTargetConfig{
		"bare":             {Type: "kubernetes"},
		"context":          {Type: "kubernetes", Context: "c"},
		"namespace":        {Type: "kubernetes", Namespace: "ns"},
		"all":              {Type: "kubernetes", Context: "c", Namespace: "ns", Kubeconfig: "/etc/scion/cluster-a.kubeconfig"},
		"empty kubeconfig": {Type: "kubernetes", Kubeconfig: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n", RuntimeTarget: target}}); len(errs) != 0 {
				t.Fatalf("got %v", errs)
			}
		})
	}
	for name, kc := range map[string]string{
		"relative":  "cluster.kubeconfig",
		"tilde":     "~/.kube/config",
		"env":       "$HOME/.kube/config",
		"path list": "/a/config:/b/config",
		"root":      "/",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n",
				RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", Kubeconfig: kc}}})
			expectValidationPaths(t, errs, "runtime_target.kubeconfig")
		})
	}
}

// TestRuntimeBrokerInstances_KubeconfigRoundTrip: both conversions, the
// strict loader and the startup mapping preserve kubeconfig.
func TestRuntimeBrokerInstances_KubeconfigRoundTrip(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), instancesYAML(
		"- {key: k8s-a, name: a, runtime_target: {type: kubernetes, context: c, namespace: ns, kubeconfig: /etc/scion/a.kubeconfig}}"))
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	want := []V1RuntimeBrokerInstanceConfig{{Key: "k8s-a", Name: "a",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", Context: "c", Namespace: "ns", Kubeconfig: "/etc/scion/a.kubeconfig"}}}
	if !reflect.DeepEqual(strict, want) {
		t.Fatalf("strict: got %+v", strict)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if got := gc.RuntimeBroker.Instances; len(got) != 1 || got[0].RuntimeTarget == nil || got[0].RuntimeTarget.Kubeconfig != "/etc/scion/a.kubeconfig" {
		t.Fatalf("GlobalConfig instances: %+v", got)
	}
	if !reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(strict), gc.RuntimeBroker.Instances) {
		t.Fatal("startup comparison mapping must equal the GlobalConfig conversion")
	}
	if v1 := ConvertGlobalToV1ServerConfig(gc); !reflect.DeepEqual(v1.Broker.Instances, strict) {
		t.Fatalf("back-conversion differs: %+v", v1.Broker.Instances)
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
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker", Context: "c", Namespace: "ns", Kubeconfig: "/etc/kubeconfig"}}})
	expectValidationPaths(t, errs, "runtime_target.context", "runtime_target.kubeconfig", "runtime_target.namespace")
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
		"two entries":       {instancesYAML("- {key: a, name: x, runtime_target: {type: docker}}\n      - {key: b, name: y, runtime_target: {type: docker}}"), true},
		"duplicate key":     {instancesYAML("- {key: a, name: x, runtime_target: {type: docker}}\n      - {key: a, name: y, runtime_target: {type: docker}}"), false},
		"bad key":           {instancesYAML("- {key: Bad, name: x, runtime_target: {type: docker}}"), false},
		"missing name":      {instancesYAML("- {key: a, runtime_target: {type: docker}}"), false},
		"missing target":    {instancesYAML("- {key: a, name: x}"), false},
		"kubernetes":        {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, context: c, namespace: n}}"), true},
		"k8s kubeconfig":    {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubeconfig: /etc/scion/a.kubeconfig}}"), true},
		"k8s rel config":    {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubeconfig: a.kubeconfig}}"), false},
		"k8s tilde config":  {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubeconfig: ~/.kube/config}}"), false},
		"k8s config list":   {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubeconfig: \"/a:/b\"}}"), false},
		"docker kubeconfig": {instancesYAML("- {key: a, name: x, runtime_target: {type: docker, kubeconfig: /etc/kc}}"), false},
		"unsupported type":  {instancesYAML("- {key: a, name: x, runtime_target: {type: podman}}"), false},
		"docker with ns":    {instancesYAML("- {key: a, name: x, runtime_target: {type: docker, namespace: n}}"), false},
		"unknown entry key": {instancesYAML("- {key: a, name: x, profile: p, runtime_target: {type: docker}}"), false},
		"instances null":    {"schema_version: \"1\"\nserver:\n  broker:\n    instances: null\n", true},
		"docker empty ctx":  {instancesYAML("- {key: a, name: x, runtime_target: {type: docker, context: \"\"}}"), true},
		"numeric key":       {instancesYAML("- {key: 123, name: x, runtime_target: {type: docker}}"), false},
		"numeric name":      {instancesYAML("- {key: a, name: 7, runtime_target: {type: docker}}"), false},
		"broker not a map":  {"schema_version: \"1\"\nserver:\n  broker: 5\n", false},
		"k8s block sa":      {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubernetes_block_service_account: zero-priv}}"), true},
		"k8s bad block sa":  {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubernetes_block_service_account: Bad_SA}}"), false},
		"k8s mappings":      {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubernetes_service_account_mappings: {agent@proj.iam.gserviceaccount.com: ksa-agent}}}"), true},
		"k8s bad gsa":       {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubernetes_service_account_mappings: {Agent@example.com: ksa-agent}}}"), false},
		"k8s bad ksa":       {instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, kubernetes_service_account_mappings: {agent@proj.iam.gserviceaccount.com: Bad_KSA}}}"), false},
		"docker block sa":   {instancesYAML("- {key: a, name: x, runtime_target: {type: docker, kubernetes_block_service_account: zero-priv}}"), false},
		"docker mappings":   {instancesYAML("- {key: a, name: x, runtime_target: {type: docker, kubernetes_service_account_mappings: {agent@proj.iam.gserviceaccount.com: ksa-agent}}}"), false},
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
	if want := "server.broker.instances requires the Hub in the same process in this release; remote flat Runtime Broker hosting arrives in P2 (ptone/scion#3271). Remove server.broker.instances to run this host as a legacy Runtime Broker"; err.Error() != want {
		t.Fatalf("frozen message: got %q", err.Error())
	}
	if he.InstanceKey != "local-docker" {
		t.Fatalf("instance key = %q", he.InstanceKey)
	}
	// --simulate-remote-broker: the Hub runs in process but the embedded
	// registration (colocatedBrokerRegisters) does not, so hubInProcess is false.
	if err := CheckRuntimeBrokerInstanceHosting(inst, false); err == nil {
		t.Fatal("simulated remote must be refused")
	}
	if err := CheckRuntimeBrokerInstanceHosting(nil, false); err != nil {
		t.Fatalf("legacy remote hosting must be unchanged: %v", err)
	}
	if err := CheckRuntimeBrokerInstanceHosting([]V1RuntimeBrokerInstanceConfig{}, false); err != nil {
		t.Fatalf("an empty (non-nil) list is legacy hosting: %v", err)
	}
	if err := CheckRuntimeBrokerInstanceHosting(inst, true); err != nil {
		t.Fatalf("co-located flat hosting must be allowed: %v", err)
	}
}

// TestRuntimeBrokerInstances_KubernetesIdentityPolicyRoundTrips: the
// instance's Kubernetes identity policy survives both config-family
// conversions, and the mapping is copied rather than shared.
func TestRuntimeBrokerInstances_KubernetesIdentityPolicyRoundTrips(t *testing.T) {
	in := []V1RuntimeBrokerInstanceConfig{{Key: "k8s-a", Name: "a", RuntimeTarget: &V1RuntimeTargetConfig{
		Type: RuntimeTargetTypeKubernetes, Namespace: "agents",
		KubernetesBlockServiceAccount:    "zero-priv",
		KubernetesServiceAccountMappings: map[string]string{"agent@proj.iam.gserviceaccount.com": "ksa-agent"},
	}}}
	global := v1InstancesToGlobal(in)
	if got := global[0].RuntimeTarget; got.KubernetesBlockServiceAccount != "zero-priv" ||
		got.KubernetesServiceAccountMappings["agent@proj.iam.gserviceaccount.com"] != "ksa-agent" {
		t.Fatalf("server-config form lost the policy: %+v", got)
	}
	back := globalInstancesToV1(global)
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip = %+v, want %+v", back[0].RuntimeTarget, in[0].RuntimeTarget)
	}
	global[0].RuntimeTarget.KubernetesServiceAccountMappings["other@proj.iam.gserviceaccount.com"] = "x"
	if len(in[0].RuntimeTarget.KubernetesServiceAccountMappings) != 1 || len(back[0].RuntimeTarget.KubernetesServiceAccountMappings) != 1 {
		t.Fatal("the mapping is shared between the converted copies")
	}
}
