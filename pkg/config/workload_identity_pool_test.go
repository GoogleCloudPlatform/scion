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
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workload_identity_pool (ptone/scion#3329 phase 5) on runtime entries and
// flat runtime targets.

func TestValidateWorkloadIdentityPool(t *testing.T) {
	for _, pool := range []string{"", "my-project.svc.id.goog", "abcdef.svc.id.goog", "a12345678901234567890123456789.svc.id.goog"} {
		if err := ValidateWorkloadIdentityPool(pool); err != nil {
			t.Errorf("%q: unexpected error %v", pool, err)
		}
	}
	for _, pool := range []string{
		"my-project",                                  // no suffix
		"my-project.svc.id.goog.",                     // trailing dot
		"My-Project.svc.id.goog",                      // uppercase
		"1project.svc.id.goog",                        // starts with a digit
		"proj.svc.id.goog",                            // too short
		"project-.svc.id.goog",                        // ends with a hyphen
		"example.com:proj.svc.id.goog",                // domain-scoped
		" my-project.svc.id.goog",                     // whitespace
		"my-project.iam.gserviceaccount",              // wrong suffix
		"a123456789012345678901234567890.svc.id.goog", // 31 characters
	} {
		err := ValidateWorkloadIdentityPool(pool)
		if err == nil {
			t.Errorf("%q: expected an error", pool)
			continue
		}
		if !strings.Contains(err.Error(), "workload_identity_pool") {
			t.Errorf("%q: error %q does not name the setting", pool, err)
		}
	}
}

// The JSON schema pattern accepts exactly what the validator accepts.
func TestWorkloadIdentityPoolPattern_MatchesValidator(t *testing.T) {
	re := regexp.MustCompile(WorkloadIdentityPoolPattern)
	for _, pool := range []string{"", "my-project.svc.id.goog", "My-Project.svc.id.goog", "proj.svc.id.goog",
		"project-.svc.id.goog", "my-project", "example.com:proj.svc.id.goog", "my-projectXsvcXidXgoog"} {
		if got, want := re.MatchString(pool), ValidateWorkloadIdentityPool(pool) == nil; got != want {
			t.Errorf("%q: pattern match %v, validator ok %v", pool, got, want)
		}
	}
}

// The runtime entry key loads from global settings and passes the schema;
// a malformed value fails the schema.
func TestWorkloadIdentityPool_RuntimeEntryLoads(t *testing.T) {
	const yaml = `schema_version: "1"
runtimes:
    gke:
        type: kubernetes
        namespace: agents
        workload_identity_pool: cluster-project.svc.id.goog
`
	if errs, err := ValidateSettings([]byte(yaml), "1"); err != nil || len(errs) != 0 {
		t.Fatalf("schema: %v %v", errs, err)
	}
	bad := strings.Replace(yaml, "cluster-project.svc.id.goog", "cluster-project", 1)
	if errs, err := ValidateSettings([]byte(bad), "1"); err == nil && len(errs) == 0 {
		t.Fatal("schema accepted a malformed workload_identity_pool")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), yaml)
	vs, _, err := LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if got := vs.Runtimes["gke"].WorkloadIdentityPool; got != "cluster-project.svc.id.goog" {
		t.Errorf("WorkloadIdentityPool = %q", got)
	}
}

// A v1 file without schema_version that uses the key is loaded as v1, so
// the key is not dropped by the legacy loader.
func TestWorkloadIdentityPool_IsV1Indicator(t *testing.T) {
	raw := map[string]interface{}{"runtimes": map[string]interface{}{
		"gke": map[string]interface{}{"workload_identity_pool": "cluster-project.svc.id.goog"},
	}}
	if !hasV1RuntimeIndicators(raw) {
		t.Error("workload_identity_pool is not a v1 runtime indicator")
	}
}

func TestProjectSettingsHasWorkloadIdentityPool(t *testing.T) {
	const withPool = `schema_version: "1"
runtimes:
    gke:
        type: kubernetes
        workload_identity_pool: cluster-project.svc.id.goog
`
	setup := func(t *testing.T, projectYAML string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		// The global settings set it too: it must not be mistaken for one
		// the project set itself.
		writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), withPool)
		projectDir := t.TempDir()
		writeCloudRunTestSettings(t, filepath.Join(projectDir, ".scion"), projectYAML)
		return projectDir
	}
	if !ProjectSettingsHasWorkloadIdentityPool(setup(t, withPool), "gke") {
		t.Error("expected true: the project's own settings.yaml sets the pool")
	}
	if ProjectSettingsHasWorkloadIdentityPool(setup(t, withPool), "other") {
		t.Error("expected false for another runtime entry")
	}
	if ProjectSettingsHasWorkloadIdentityPool(setup(t, `schema_version: "1"
runtimes:
    gke:
        type: kubernetes
        namespace: agents
`), "gke") {
		t.Error("expected false: the project does not set the pool (the global one must not count)")
	}
	if ProjectSettingsHasWorkloadIdentityPool(setup(t, withPool), "") {
		t.Error("expected false for an empty runtime entry name")
	}
}

const flatWorkloadIdentityPoolYAML = `schema_version: "1"
server:
  broker:
    instances:
      - key: gke-1
        name: gke-1
        runtime_target:
          type: kubernetes
          namespace: agents
          workload_identity_pool: cluster-project.svc.id.goog
`

// The flat runtime_target key loads strictly, mirrors into the server
// config and back unchanged, and passes the schema.
func TestWorkloadIdentityPool_FlatRuntimeTargetRoundTrip(t *testing.T) {
	if errs, err := ValidateSettings([]byte(flatWorkloadIdentityPoolYAML), "1"); err != nil || len(errs) != 0 {
		t.Fatalf("schema: %v %v", errs, err)
	}
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), flatWorkloadIdentityPoolYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(strict) != 1 || strict[0].RuntimeTarget == nil || strict[0].RuntimeTarget.WorkloadIdentityPool != "cluster-project.svc.id.goog" {
		t.Fatalf("strict load: got %+v", strict)
	}
	g := v1InstancesToGlobal(strict)
	if g[0].RuntimeTarget.WorkloadIdentityPool != "cluster-project.svc.id.goog" {
		t.Fatalf("server config form: got %+v", g[0].RuntimeTarget)
	}
	back := globalInstancesToV1(g)
	if back[0].RuntimeTarget.WorkloadIdentityPool != "cluster-project.svc.id.goog" {
		t.Fatalf("back to settings form: got %+v", back[0].RuntimeTarget)
	}
	// Never published: the presentation has no place for it.
	if p := PresentInstance(strict[0]); p.RuntimeTarget == nil {
		t.Fatal("no presented runtime target")
	}
}

// The strict loader refuses a non-string value.
func TestWorkloadIdentityPool_FlatStrictLoaderRefusesNonString(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"),
		strings.Replace(flatWorkloadIdentityPoolYAML, "cluster-project.svc.id.goog", "5", 1))
	_, err := LoadRuntimeBrokerInstances("")
	if err == nil || !strings.Contains(err.Error(), "workload_identity_pool must be a string") {
		t.Fatalf("got %v, want a must-be-a-string error", err)
	}
}

// Validation refuses a malformed pool on a kubernetes target and the key
// on a non-Kubernetes target.
func TestValidateRuntimeBrokerInstances_WorkloadIdentityPool(t *testing.T) {
	inst := func(typ, pool string) []V1RuntimeBrokerInstanceConfig {
		return []V1RuntimeBrokerInstanceConfig{{Key: "k", Name: "k", RuntimeTarget: &V1RuntimeTargetConfig{Type: typ, WorkloadIdentityPool: pool}}}
	}
	if errs := ValidateRuntimeBrokerInstances(inst(RuntimeTargetTypeKubernetes, "cluster-project.svc.id.goog")); len(errs) != 0 {
		t.Errorf("valid pool: %v", errs)
	}
	errs := ValidateRuntimeBrokerInstances(inst(RuntimeTargetTypeKubernetes, "cluster-project"))
	if len(errs) != 1 || errs[0].Path != "server.broker.instances[0].runtime_target.workload_identity_pool" {
		t.Errorf("malformed pool: %v", errs)
	}
	if errs := ValidateRuntimeBrokerInstances(inst(RuntimeTargetTypeDocker, "cluster-project.svc.id.goog")); len(errs) > 0 {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Path, "workload_identity_pool") || strings.Contains(e.Message, "workload_identity_pool") {
				found = true
			}
		}
		if !found {
			t.Errorf("docker target with a pool: %v", errs)
		}
	} else {
		t.Error("docker target with a pool: expected an error")
	}
}

func TestCreateServiceAccounts_RuntimeEntryLoads(t *testing.T) {
	const yaml = `schema_version: "1"
runtimes:
    gke:
        type: kubernetes
        namespace: agents
        create_service_accounts: true
`
	if errs, err := ValidateSettings([]byte(yaml), "1"); err != nil || len(errs) != 0 {
		t.Fatalf("schema: %v %v", errs, err)
	}
	bad := strings.Replace(yaml, "true", `"yes"`, 1)
	if errs, err := ValidateSettings([]byte(bad), "1"); err == nil && len(errs) == 0 {
		t.Fatal("schema accepted a non-boolean create_service_accounts")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), yaml)
	vs, _, err := LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if !vs.Runtimes["gke"].CreateServiceAccounts {
		t.Error("CreateServiceAccounts = false, want true")
	}
	if vs.Runtimes["other"].CreateServiceAccounts {
		t.Error("default CreateServiceAccounts = true, want false")
	}
}

func TestCreateServiceAccounts_IsV1Indicator(t *testing.T) {
	raw := map[string]interface{}{"runtimes": map[string]interface{}{
		"gke": map[string]interface{}{"create_service_accounts": true},
	}}
	if !hasV1RuntimeIndicators(raw) {
		t.Error("create_service_accounts is not a v1 runtime indicator")
	}
}

func TestProjectSettingsHasCreateServiceAccounts(t *testing.T) {
	const withCreate = `schema_version: "1"
runtimes:
    gke:
        type: kubernetes
        create_service_accounts: true
`
	setup := func(t *testing.T, projectYAML string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		// The global settings set it too: it must not be mistaken for one
		// the project set itself.
		writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), withCreate)
		projectDir := t.TempDir()
		writeCloudRunTestSettings(t, filepath.Join(projectDir, ".scion"), projectYAML)
		return projectDir
	}
	if !ProjectSettingsHasCreateServiceAccounts(setup(t, withCreate), "gke") {
		t.Error("expected true: the project's own settings.yaml sets create_service_accounts: true")
	}
	if !ProjectSettingsHasCreateServiceAccounts(setup(t, strings.Replace(withCreate, "true", "false", 1)), "gke") {
		t.Error("expected true: an explicit false in the project's own settings.yaml also warns")
	}
	if ProjectSettingsHasCreateServiceAccounts(setup(t, withCreate), "other") {
		t.Error("expected false for another runtime entry")
	}
	if ProjectSettingsHasCreateServiceAccounts(setup(t, `schema_version: "1"
runtimes:
    gke:
        type: kubernetes
        namespace: agents
`), "gke") {
		t.Error("expected false: the project does not set create_service_accounts (the global one must not count)")
	}
	if ProjectSettingsHasCreateServiceAccounts(setup(t, withCreate), "") {
		t.Error("expected false for an empty runtime entry name")
	}
}

const flatCreateServiceAccountsYAML = `schema_version: "1"
server:
  broker:
    instances:
      - key: gke-1
        name: gke-1
        runtime_target:
          type: kubernetes
          namespace: agents
          create_service_accounts: true
`

// The flat runtime_target create_service_accounts key loads strictly, mirrors
// into the server config and back unchanged, passes the schema, and is
// excluded from the FC4-a presentation.
func TestCreateServiceAccounts_FlatRuntimeTargetRoundTrip(t *testing.T) {
	if errs, err := ValidateSettings([]byte(flatCreateServiceAccountsYAML), "1"); err != nil || len(errs) != 0 {
		t.Fatalf("schema: %v %v", errs, err)
	}
	bad := strings.Replace(flatCreateServiceAccountsYAML, "create_service_accounts: true", `create_service_accounts: "yes"`, 1)
	if errs, err := ValidateSettings([]byte(bad), "1"); err == nil && len(errs) == 0 {
		t.Fatal("schema accepted a non-boolean runtime_target.create_service_accounts")
	}
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), flatCreateServiceAccountsYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(strict) != 1 || strict[0].RuntimeTarget == nil || !strict[0].RuntimeTarget.CreateServiceAccounts {
		t.Fatalf("strict load: got %+v", strict)
	}
	g := v1InstancesToGlobal(strict)
	if !g[0].RuntimeTarget.CreateServiceAccounts {
		t.Fatalf("server config form: got %+v", g[0].RuntimeTarget)
	}
	back := globalInstancesToV1(g)
	if !back[0].RuntimeTarget.CreateServiceAccounts {
		t.Fatalf("back to settings form: got %+v", back[0].RuntimeTarget)
	}
	if p := PresentInstance(strict[0]); p.RuntimeTarget == nil {
		t.Fatal("no presented runtime target")
	}
	if policy := NewFlatInstancePolicy(strict[0]); !policy.Target.CreateServiceAccounts {
		t.Fatalf("flat instance policy: got %+v", policy.Target)
	}
}

// The strict loader refuses a non-boolean value for create_service_accounts.
func TestCreateServiceAccounts_FlatStrictLoaderRefusesNonBool(t *testing.T) {
	for _, rawVal := range []string{`"yes"`, "1", "null"} {
		global := flatTestHome(t)
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"),
			strings.Replace(flatCreateServiceAccountsYAML, "create_service_accounts: true", "create_service_accounts: "+rawVal, 1))
		_, err := LoadRuntimeBrokerInstances("")
		if err == nil || !strings.Contains(err.Error(), "create_service_accounts must be true or false") {
			t.Fatalf("%s: got %v, want a must-be-true-or-false error", rawVal, err)
		}
	}
}

// Validation and schema refuse create_service_accounts: true on non-Kubernetes
// runtime_target types (docker and cloudrun).
func TestValidateRuntimeBrokerInstances_CreateServiceAccounts(t *testing.T) {
	k8sInst := []V1RuntimeBrokerInstanceConfig{{
		Key: "k", Name: "k",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: RuntimeTargetTypeKubernetes, CreateServiceAccounts: true},
	}}
	if errs := ValidateRuntimeBrokerInstances(k8sInst); len(errs) != 0 {
		t.Errorf("kubernetes target with create_service_accounts: %v", errs)
	}

	dockerInst := []V1RuntimeBrokerInstanceConfig{{
		Key: "d", Name: "d",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: RuntimeTargetTypeDocker, CreateServiceAccounts: true},
	}}
	errs := ValidateRuntimeBrokerInstances(dockerInst)
	if len(errs) != 1 || errs[0].Path != "server.broker.instances[0].runtime_target.create_service_accounts" {
		t.Errorf("docker target with create_service_accounts: got %v", errs)
	}
	const dockerSchemaYAML = `schema_version: "1"
server:
  broker:
    instances:
      - key: d
        name: d
        runtime_target:
          type: docker
          create_service_accounts: true
`
	if schemaErrs, err := ValidateSettings([]byte(dockerSchemaYAML), "1"); err == nil && len(schemaErrs) == 0 {
		t.Error("schema accepted create_service_accounts: true on a docker runtime_target")
	}
}

// Legacy-to-flat profile conversion carries create_service_accounts onto a
// kubernetes runtime_target and reports field_not_valid_for_target_type when
// set on a non-Kubernetes runtime entry.
func TestConvertProfilesToInstances_CreateServiceAccounts(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"gke":   {Type: "kubernetes", Namespace: "agents", WorkloadIdentityPool: "cluster-project.svc.id.goog", CreateServiceAccounts: true},
			"local": {Type: "docker", CreateServiceAccounts: true},
		},
		Profiles: map[string]V1ProfileConfig{
			"k8s": {Runtime: "gke"},
			"dev": {Runtime: "local"},
		},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	var foundK8s, foundDocker bool
	for _, inst := range got.Instances {
		switch inst.RuntimeTarget.Type {
		case RuntimeTargetTypeKubernetes:
			foundK8s = true
			if !inst.RuntimeTarget.CreateServiceAccounts {
				t.Errorf("converted kubernetes runtime_target lost create_service_accounts: %+v", inst.RuntimeTarget)
			}
		case RuntimeTargetTypeDocker:
			foundDocker = true
			if inst.RuntimeTarget.CreateServiceAccounts {
				t.Errorf("converted docker runtime_target kept create_service_accounts: %+v", inst.RuntimeTarget)
			}
		}
	}
	if !foundK8s || !foundDocker {
		t.Fatalf("missing converted instances: %+v", got.Instances)
	}
	var reportedNotValid bool
	for _, f := range got.Findings {
		if f.Path == "runtimes.local.create_service_accounts" && f.Code == ConversionFieldNotValidForTargetType {
			reportedNotValid = true
		}
	}
	if !reportedNotValid {
		t.Errorf("expected field_not_valid_for_target_type on runtimes.local.create_service_accounts, got %+v", got.Findings)
	}
}
