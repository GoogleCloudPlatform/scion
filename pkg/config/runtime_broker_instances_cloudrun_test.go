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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Cloud Run flat instance settings (contract amendment P5-CR, CR1 and CR2).

// cloudRunTarget returns a valid cloudrun runtime_target.
func cloudRunTarget() *V1RuntimeTargetConfig {
	return &V1RuntimeTargetConfig{
		Type:            RuntimeTargetTypeCloudRun,
		DisplayName:     "Cloud Run us-central1",
		ProjectID:       "example-project",
		Region:          "us-central1",
		ServiceAccount:  "agents@example-project.iam.gserviceaccount.com",
		Subnetwork:      "projects/example-project/regions/us-central1/subnetworks/agents",
		CredentialsFile: "/etc/scion/cr-us-central1.json",
	}
}

// validateCloudRun validates one instance whose runtime_target is t after
// mutate changed it.
func validateCloudRun(mutate func(t *V1RuntimeTargetConfig)) []ValidationError {
	t := cloudRunTarget()
	if mutate != nil {
		mutate(t)
	}
	return ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "cr-a", Name: "a", RuntimeTarget: t}})
}

// expectOneCloudRunError checks that errs holds exactly one error, at the
// runtime_target field, whose message contains msg.
func expectOneCloudRunError(t *testing.T, errs []ValidationError, field, msg string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("want exactly one error at %s, got %v", field, errs)
	}
	if want := "server.broker.instances[0].runtime_target." + field; errs[0].Path != want {
		t.Fatalf("error path = %q, want %q (%v)", errs[0].Path, want, errs)
	}
	if !strings.Contains(errs[0].Message, msg) {
		t.Fatalf("error message %q does not contain %q", errs[0].Message, msg)
	}
}

func TestRuntimeBrokerInstances_CloudRunAccepted(t *testing.T) {
	for name, mutate := range map[string]func(t *V1RuntimeTargetConfig){
		"subnetwork only": nil,
		"network only":    func(t *V1RuntimeTargetConfig) { t.Subnetwork, t.Network = "", "agents-vpc" },
		"both networks":   func(t *V1RuntimeTargetConfig) { t.Network = "agents-vpc" },
		"no display name": func(t *V1RuntimeTargetConfig) { t.DisplayName = "" },
		"compute-like id": func(t *V1RuntimeTargetConfig) {
			t.ServiceAccount = "agents-compute@example-project.iam.gserviceaccount.com"
		},
		"six-char project": func(t *V1RuntimeTargetConfig) { t.ProjectID = "abcdef" },
		"region europe":    func(t *V1RuntimeTargetConfig) { t.Region = "europe-west4" },
	} {
		t.Run(name, func(t *testing.T) {
			if errs := validateCloudRun(mutate); len(errs) != 0 {
				t.Fatalf("a valid cloudrun target was refused: %v", errs)
			}
		})
	}
}

func TestRuntimeBrokerInstances_CloudRunRequiresProjectRegionSA(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate     func(t *V1RuntimeTargetConfig)
		field, msg string
	}{
		"project missing":       {func(t *V1RuntimeTargetConfig) { t.ProjectID = "" }, "project_id", "project_id is required"},
		"project upper case":    {func(t *V1RuntimeTargetConfig) { t.ProjectID = "Example-Project" }, "project_id", "must be a GCP project ID"},
		"project too short":     {func(t *V1RuntimeTargetConfig) { t.ProjectID = "abc" }, "project_id", "must be a GCP project ID"},
		"project too long":      {func(t *V1RuntimeTargetConfig) { t.ProjectID = "a" + strings.Repeat("b", 30) }, "project_id", "must be a GCP project ID"},
		"project digit first":   {func(t *V1RuntimeTargetConfig) { t.ProjectID = "1example" }, "project_id", "must be a GCP project ID"},
		"project trailing dash": {func(t *V1RuntimeTargetConfig) { t.ProjectID = "example-" }, "project_id", "must be a GCP project ID"},
		"region missing":        {func(t *V1RuntimeTargetConfig) { t.Region = "" }, "region", "region is required"},
		"region a zone":         {func(t *V1RuntimeTargetConfig) { t.Region = "us-central1-a" }, "region", "must be a Cloud Run region"},
		"region no number":      {func(t *V1RuntimeTargetConfig) { t.Region = "us-central" }, "region", "must be a Cloud Run region"},
		"region upper case":     {func(t *V1RuntimeTargetConfig) { t.Region = "US-CENTRAL1" }, "region", "must be a Cloud Run region"},
		"sa missing":            {func(t *V1RuntimeTargetConfig) { t.ServiceAccount = "" }, "service_account", "service_account is required"},
	} {
		t.Run(name, func(t *testing.T) {
			expectOneCloudRunError(t, validateCloudRun(tc.mutate), tc.field, tc.msg)
		})
	}
	// Every required field missing at once: one path-named error each.
	errs := validateCloudRun(func(t *V1RuntimeTargetConfig) {
		*t = V1RuntimeTargetConfig{Type: RuntimeTargetTypeCloudRun}
	})
	expectValidationPaths(t, errs, "runtime_target.project_id", "runtime_target.region", "runtime_target.service_account",
		"runtime_target.network", "runtime_target.credentials_file")
	if len(errs) != 5 {
		t.Fatalf("want five errors for an empty cloudrun target, got %v", errs)
	}
}

// cloudRunUserManagedSAMessage is the refusal text of every service_account
// that is not a user-managed account (contract amendment P5-CR, CR2 as
// narrowed to user-managed accounts), exactly.
const cloudRunUserManagedSAMessage = "service_account must be a user-managed service account (name@project.iam.gserviceaccount.com)"

// TestRuntimeBrokerInstances_CloudRunServiceAccountUserManagedOnly: only a
// user-managed account (<name>@<project_id>.iam.gserviceaccount.com, lower
// case) is accepted. Every other address, Google-managed service agents
// included, is refused with the user-managed text, exactly; the project
// default accounts keep their own text (checked first).
func TestRuntimeBrokerInstances_CloudRunServiceAccountUserManagedOnly(t *testing.T) {
	saPath := "server.broker.instances[0].runtime_target.service_account"
	for name, sa := range map[string]string{
		"not an email":                    "agents",
		"user email":                      "dev@example.com",
		"lookalike domain":                "agents@gserviceaccount.com.example.com",
		"bare domain":                     "agents@gserviceaccount.com",
		"service agent":                   "service-123@serverless-robot-prod.iam.gserviceaccount.com",
		"gcp-sa agent":                    "robot-agent@gcp-sa-example1.iam.gserviceaccount.com",
		"gcp-sa service agent":            "service-123456789012@gcp-sa-run.iam.gserviceaccount.com",
		"developer non-default":           "agents-x@developer.gserviceaccount.com",
		"other project app engine":        "other-project@appspot.gserviceaccount.com",
		"cloudservices agent":             "123456789012@cloudservices.gserviceaccount.com",
		"upper-case user-managed":         "Agents@example-project.iam.gserviceaccount.com",
		"upper-case project":              "agents@Example-Project.iam.gserviceaccount.com",
		"domain-scoped project":           "agents@example.com:proj.iam.gserviceaccount.com",
		"short name":                      "abc@example-project.iam.gserviceaccount.com",
		"no iam label":                    "agents@example-project.gserviceaccount.com",
		"trailing dot":                    "agents@example-project.iam.gserviceaccount.com.",
		"surrounding space (non-default)": " agents@example-project.iam.gserviceaccount.com",
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateCloudRun(func(t *V1RuntimeTargetConfig) { t.ServiceAccount = sa })
			want := ValidationError{Path: saPath, Message: cloudRunUserManagedSAMessage}
			if len(errs) != 1 || errs[0] != want {
				t.Fatalf("got %#v, want exactly [%#v]", errs, want)
			}
		})
	}
	for _, sa := range []string{"agents@example-project.iam.gserviceaccount.com", "agents-compute@example-project.iam.gserviceaccount.com",
		"service-account-1@example-project.iam.gserviceaccount.com"} {
		if errs := validateCloudRun(func(t *V1RuntimeTargetConfig) { t.ServiceAccount = sa }); len(errs) != 0 {
			t.Fatalf("user-managed %q refused: %v", sa, errs)
		}
	}
}

func TestRuntimeBrokerInstances_CloudRunRequiresNetwork(t *testing.T) {
	expectOneCloudRunError(t, validateCloudRun(func(t *V1RuntimeTargetConfig) { t.Network, t.Subnetwork = "", "" }),
		"network", "a VPC network or subnetwork is required")
}

func TestRuntimeBrokerInstances_CloudRunCredentialsFileRequired(t *testing.T) {
	expectOneCloudRunError(t, validateCloudRun(func(t *V1RuntimeTargetConfig) { t.CredentialsFile = "" }),
		"credentials_file", "credentials_file is required for runtime target type cloudrun")
}

func TestRuntimeBrokerInstances_CloudRunCredentialsFileAbsolutePath(t *testing.T) {
	for name, p := range map[string]string{
		"relative":        "cr.json",
		"dot relative":    "./cr.json",
		"tilde":           "~/cr.json",
		"tilde inside":    "/home/~svc/cr.json",
		"env":             "$HOME/cr.json",
		"env inside":      "/etc/${KEY_DIR}/cr.json",
		"path list":       "/a/cr.json:/b/cr.json",
		"semicolon list":  "/a/cr.json;/b/cr.json",
		"root":            "/",
		"slashes":         "///",
		"double slash":    "/etc//scion/cr.json",
		"dot element":     "/etc/./cr.json",
		"dotdot element":  "/etc/scion/../cr.json",
		"trailing slash":  "/etc/scion/",
		"trailing dot":    "/etc/scion/.",
		"trailing dotdot": "/etc/scion/..",
	} {
		t.Run(name, func(t *testing.T) {
			expectOneCloudRunError(t, validateCloudRun(func(t *V1RuntimeTargetConfig) { t.CredentialsFile = p }),
				"credentials_file", "credentials_file must be one clean absolute local file path")
		})
	}
}

// cloudRunDefaultSAMessage is the refusal text of contract amendment P5-CR,
// CR2, exactly.
const cloudRunDefaultSAMessage = "the project default service accounts are not allowed"

func TestRuntimeBrokerInstances_CloudRunRefusesDefaultServiceAccounts(t *testing.T) {
	for name, sa := range map[string]string{
		"compute default":            "123456789012-compute@developer.gserviceaccount.com",
		"compute default mixed case": "123456789012-Compute@Developer.GServiceAccount.com",
		"compute default upper case": "123456789012-COMPUTE@DEVELOPER.GSERVICEACCOUNT.COM",
		"app engine default":         "example-project@appspot.gserviceaccount.com",
		"app engine mixed case":      "Example-Project@AppSpot.gserviceaccount.com",
		"app engine upper case":      "EXAMPLE-PROJECT@APPSPOT.GSERVICEACCOUNT.COM",
		"surrounding space":          " 123456789012-compute@developer.gserviceaccount.com ",
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateCloudRun(func(t *V1RuntimeTargetConfig) { t.ServiceAccount = sa })
			// The path and the message are separate fields: the path names
			// the field and the message is the contract text, exactly.
			want := ValidationError{Path: "server.broker.instances[0].runtime_target.service_account", Message: cloudRunDefaultSAMessage}
			if len(errs) != 1 || errs[0] != want {
				t.Fatalf("got %#v, want exactly [%#v]", errs, want)
			}
		})
	}
}

func TestRuntimeBrokerInstances_CloudRunRefusesDomainScopedProject(t *testing.T) {
	for _, p := range []string{"example.com:proj", "example.com:example-project"} {
		expectOneCloudRunError(t, validateCloudRun(func(t *V1RuntimeTargetConfig) { t.ProjectID = p }),
			"project_id", "domain-scoped project IDs are not supported")
	}
}

func TestRuntimeBrokerInstances_CloudRunRejectsKubernetesFields(t *testing.T) {
	evict := false
	errs := validateCloudRun(func(t *V1RuntimeTargetConfig) {
		t.Context, t.Namespace, t.Kubeconfig = "c", "ns", "/etc/kubeconfig"
		t.KubernetesBlockServiceAccount = "zero-priv"
		t.KubernetesServiceAccountMappings = map[string]string{"agent@proj.iam.gserviceaccount.com": "ksa"}
		t.SharedDirStorageClass, t.SharedDirSize = "rwx", "40Gi"
		t.GKE, t.PriorityClassName, t.SafeToEvict = true, "high", &evict
		t.NodeSelector = map[string]string{"pool": "agents"}
		t.Tolerations = []api.K8sToleration{{Key: "k", Operator: "Exists"}}
		t.RuntimeClassName = "gvisor"
	})
	fields := []string{"context", "namespace", "kubeconfig", "kubernetes_block_service_account",
		"kubernetes_service_account_mappings", "shared_dir_storage_class", "shared_dir_size", "gke",
		"priority_class_name", "safe_to_evict", "node_selector", "tolerations", "runtime_class_name"}
	if len(errs) != len(fields) {
		t.Fatalf("want one error per Kubernetes-only field (%d), got %v", len(fields), errs)
	}
	for _, f := range fields {
		found := false
		for _, e := range errs {
			if e.Path == "server.broker.instances[0].runtime_target."+f && e.Message == "field not valid for runtime target type cloudrun" {
				found = true
			}
		}
		if !found {
			t.Errorf("no cloudrun refusal for %s: %v", f, errs)
		}
	}
	// The removed storage overrides are refused by name, as for every type.
	errs = validateCloudRun(func(t *V1RuntimeTargetConfig) { t.HomeStorageBackend = "nfs" })
	expectValidationPaths(t, errs, "runtime_target.home_storage_backend")
}

func TestRuntimeBrokerInstances_DockerAndKubernetesRejectCloudRunFields(t *testing.T) {
	fields := []string{"project_id", "region", "service_account", "network", "subnetwork", "credentials_file"}
	for _, typ := range []string{RuntimeTargetTypeDocker, RuntimeTargetTypeKubernetes} {
		t.Run(typ, func(t *testing.T) {
			target := cloudRunTarget()
			target.Type, target.DisplayName, target.Network = typ, "", "agents-vpc"
			errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n", RuntimeTarget: target}})
			if len(errs) != len(fields) {
				t.Fatalf("want one error per Cloud Run field (%d), got %v", len(fields), errs)
			}
			for _, f := range fields {
				found := false
				for _, e := range errs {
					if e.Path == "server.broker.instances[0].runtime_target."+f && e.Message == "field not valid for runtime target type "+typ {
						found = true
					}
				}
				if !found {
					t.Errorf("no %s refusal for %s: %v", typ, f, errs)
				}
			}
			// Each field alone is refused too.
			for _, f := range fields {
				one := &V1RuntimeTargetConfig{Type: typ}
				reflect.ValueOf(one).Elem().FieldByName(cloudRunGoField[f]).SetString("x")
				errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n", RuntimeTarget: one}})
				if len(errs) != 1 || errs[0].Path != "server.broker.instances[0].runtime_target."+f {
					t.Errorf("%s alone on %s: got %v", f, typ, errs)
				}
			}
		})
	}
}

// cloudRunGoField maps each Cloud Run settings key to its Go field.
var cloudRunGoField = map[string]string{
	"project_id": "ProjectID", "region": "Region", "service_account": "ServiceAccount",
	"network": "Network", "subnetwork": "Subnetwork", "credentials_file": "CredentialsFile",
}

func TestRuntimeBrokerInstances_UnsupportedTypeMessage(t *testing.T) {
	// Only the canonical lower-case "cloudrun" is a Cloud Run target: any
	// other spelling is an unsupported type, refused here, so no other
	// layer sees it.
	for _, typ := range []string{"podman", "cloudrun-instances", "cloudrun-sandbox", "CloudRun", "CLOUDRUN", "Cloudrun", " cloudrun", "cloudrun ", "cloud-run"} {
		errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n", RuntimeTarget: &V1RuntimeTargetConfig{Type: typ}}})
		want := `unsupported runtime target type "` + typ + `" (supported: docker, kubernetes, cloudrun)`
		if len(errs) != 1 || errs[0].Message != want || errs[0].Path != "server.broker.instances[0].runtime_target.type" {
			t.Fatalf("type %q: got %v, want one error %q", typ, errs, want)
		}
	}
}

const fullCloudRunInstanceYAML = `schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: cr-us-central1
        name: cloudrun-us-central1
        runtime_target:
          type: cloudrun
          display_name: Cloud Run us-central1
          project_id: example-project
          region: us-central1
          service_account: agents@example-project.iam.gserviceaccount.com
          network: agents-vpc
          subnetwork: projects/example-project/regions/us-central1/subnetworks/agents
          credentials_file: /etc/scion/cr-us-central1.json
`

// TestRuntimeBrokerInstances_CloudRunRoundTrip: a full cloudrun entry
// survives the strict loader, the settings loader (save, load, save byte
// stable), both config-family conversions and the startup comparison
// mapping, and the server-config form carries the camelCase keys.
func TestRuntimeBrokerInstances_CloudRunRoundTrip(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), fullCloudRunInstanceYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	want := cloudRunTarget()
	want.Network = "agents-vpc"
	if len(strict) != 1 || !reflect.DeepEqual(strict[0].RuntimeTarget, want) {
		t.Fatalf("strict: got %+v, want %+v", strict, want)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	got := gc.RuntimeBroker.Instances
	if len(got) != 1 || got[0].RuntimeTarget == nil {
		t.Fatalf("GlobalConfig instances: %+v", got)
	}
	rt := got[0].RuntimeTarget
	if rt.ProjectID != want.ProjectID || rt.Region != want.Region || rt.ServiceAccount != want.ServiceAccount ||
		rt.Network != want.Network || rt.Subnetwork != want.Subnetwork || rt.CredentialsFile != want.CredentialsFile {
		t.Fatalf("server-config form lost a Cloud Run field: %+v", rt)
	}
	if !reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(strict), gc.RuntimeBroker.Instances) {
		t.Fatal("startup comparison mapping must equal the GlobalConfig conversion")
	}
	if v1 := ConvertGlobalToV1ServerConfig(gc); !reflect.DeepEqual(v1.Broker.Instances, strict) {
		t.Fatalf("back-conversion differs: %+v", v1.Broker.Instances)
	}
	if back := globalInstancesToV1(v1InstancesToGlobal(strict)); !reflect.DeepEqual(back, strict) {
		t.Fatalf("round trip = %+v", back[0].RuntimeTarget)
	}
	for _, f := range []string{"ProjectID", "Region", "ServiceAccount", "Network", "Subnetwork", "CredentialsFile"} {
		a, okA := reflect.TypeOf(V1RuntimeTargetConfig{}).FieldByName(f)
		b, okB := reflect.TypeOf(RuntimeTargetConfig{}).FieldByName(f)
		if !okA || !okB || a.Type != b.Type {
			t.Errorf("field %s differs between the settings and server-config forms", f)
		}
	}
	for f, key := range map[string]string{"ProjectID": "projectId", "ServiceAccount": "serviceAccount", "CredentialsFile": "credentialsFile",
		"Region": "region", "Network": "network", "Subnetwork": "subnetwork"} {
		sf, _ := reflect.TypeOf(RuntimeTargetConfig{}).FieldByName(f)
		if name, _, _ := strings.Cut(sf.Tag.Get("json"), ","); name != key {
			t.Errorf("server-config key of %s = %q, want %q", f, name, key)
		}
	}

	dir := t.TempDir()
	writeFlatTestFile(t, filepath.Join(dir, "settings.yaml"), fullCloudRunInstanceYAML)
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
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
	if string(b1) != string(b2) || !strings.Contains(string(b1), "credentials_file: /etc/scion/cr-us-central1.json") {
		t.Fatalf("round trip not byte-stable or lost a field:\n%s\n---\n%s", b1, b2)
	}
}

// TestRuntimeBrokerInstances_CloudRunStrictLoaderRefusesNonStrings: the strict loader
// refuses a non-string or null Cloud Run field, naming its path.
func TestRuntimeBrokerInstances_CloudRunStrictLoaderRefusesNonStrings(t *testing.T) {
	p := "server.broker.instances[0].runtime_target."
	for line, want := range map[string]string{
		"project_id: 123":          p + "project_id must be a string",
		"region: null":             p + "region must be a string",
		"service_account: [a]":     p + "service_account must be a string",
		"network: true":            p + "network must be a string",
		"subnetwork: null":         p + "subnetwork must be a string",
		"credentials_file: 7":      p + "credentials_file must be a string",
		"credentials_files: /a/b":  p + "credentials_files: unknown key",
		"impersonate_account: x@y": p + "impersonate_account: unknown key",
	} {
		t.Run(line, func(t *testing.T) {
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), withRuntimeTargetLine(t, fullCloudRunInstanceYAML, line))
			_, err := LoadRuntimeBrokerInstances("")
			if err == nil || err.Error() != want {
				t.Fatalf("got %v, want exactly %q", err, want)
			}
		})
	}
}

// withRuntimeTargetLine returns doc with the runtime_target line "<key>: <value>"
// set: an existing line for the key is replaced, otherwise the line is added
// after the type line. The result has exactly one line for the key, so the
// YAML parser never refuses a duplicate key before the strict loader runs.
func withRuntimeTargetLine(t *testing.T, doc, line string) string {
	t.Helper()
	const indent = "          "
	key, _, _ := strings.Cut(line, ":")
	var out []string
	replaced := false
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, indent+key+":") {
			out = append(out, indent+line)
			replaced = true
			continue
		}
		out = append(out, l)
	}
	got := strings.Join(out, "\n")
	if !replaced {
		got = strings.Replace(got, indent+"type: cloudrun\n", indent+"type: cloudrun\n"+indent+line+"\n", 1)
	}
	if n := strings.Count(got, "\n"+indent+key+":"); n != 1 {
		t.Fatalf("fixture has %d lines for runtime_target.%s, want 1", n, key)
	}
	return got
}

// TestSettingsSchema_CloudRunParity: scion config validate (the JSON schema
// plus its post-schema passes) and the strict loader with
// ValidateRuntimeBrokerInstances accept and refuse the same Cloud Run cases.
func TestSettingsSchema_CloudRunParity(t *testing.T) {
	full := "type: cloudrun, project_id: example-project, region: us-central1, service_account: agents@example-project.iam.gserviceaccount.com, " +
		"subnetwork: agents-subnet, credentials_file: /etc/scion/cr.json"
	entry := func(target string) string {
		return instancesYAML("- {key: a, name: x, runtime_target: {" + target + "}}")
	}
	with := func(from, repl string) string { return entry(strings.Replace(full, from, repl, 1)) }
	cases := map[string]struct {
		yaml  string
		valid bool
	}{
		"full":                 {entry(full), true},
		"network only":         {with("subnetwork: agents-subnet", "network: agents-vpc"), true},
		"both networks":        {entry(full + ", network: agents-vpc"), true},
		"display name":         {entry(full + ", display_name: CR"), true},
		"full multi-line":      {fullCloudRunInstanceYAML, true},
		"no project":           {with("project_id: example-project, ", ""), false},
		"empty project":        {with("project_id: example-project", "project_id: \"\""), false},
		"bad project":          {with("project_id: example-project", "project_id: Example"), false},
		"short project":        {with("project_id: example-project", "project_id: abc"), false},
		"domain project":       {with("project_id: example-project", "project_id: \"example.com:proj\""), false},
		"no region":            {with("region: us-central1, ", ""), false},
		"empty region":         {with("region: us-central1", "region: \"\""), false},
		"zone region":          {with("region: us-central1", "region: us-central1-a"), false},
		"no sa":                {with("service_account: agents@example-project.iam.gserviceaccount.com, ", ""), false},
		"empty sa":             {with("service_account: agents@example-project.iam.gserviceaccount.com", "service_account: \"\""), false},
		"user sa":              {with("agents@example-project.iam.gserviceaccount.com", "dev@example.com"), false},
		"upper sa":             {with("agents@example-project.iam.gserviceaccount.com", "Agents@example-project.iam.gserviceaccount.com"), false},
		"compute default":      {with("agents@example-project.iam.gserviceaccount.com", "123-compute@developer.gserviceaccount.com"), false},
		"compute default case": {with("agents@example-project.iam.gserviceaccount.com", "123-Compute@developer.gserviceaccount.com"), false},
		"appspot default":      {with("agents@example-project.iam.gserviceaccount.com", "example-project@appspot.gserviceaccount.com"), false},
		"appspot default case": {with("agents@example-project.iam.gserviceaccount.com", "Example-Project@appspot.gserviceaccount.com"), false},
		"compute-like sa":      {with("agents@example-project.iam.gserviceaccount.com", "agents-compute@example-project.iam.gserviceaccount.com"), true},
		"bare-domain sa":       {with("agents@example-project.iam.gserviceaccount.com", "agents@gserviceaccount.com"), false},
		"service agent sa":     {with("agents@example-project.iam.gserviceaccount.com", "service-123@serverless-robot-prod.iam.gserviceaccount.com"), false},
		"gcp-sa agent sa":      {with("agents@example-project.iam.gserviceaccount.com", "robot-agent@gcp-sa-example1.iam.gserviceaccount.com"), false},
		"developer sa":         {with("agents@example-project.iam.gserviceaccount.com", "agents-x@developer.gserviceaccount.com"), false},
		"svc-account-1 sa":     {with("agents@example-project.iam.gserviceaccount.com", "service-account-1@example-project.iam.gserviceaccount.com"), true},
		"gcp-sa svc agent sa":  {with("agents@example-project.iam.gserviceaccount.com", "service-123456789012@gcp-sa-run.iam.gserviceaccount.com"), false},
		"other appspot sa":     {with("agents@example-project.iam.gserviceaccount.com", "other-project@appspot.gserviceaccount.com"), false},
		"upper project sa":     {with("agents@example-project.iam.gserviceaccount.com", "agents@Example-Project.iam.gserviceaccount.com"), false},
		"dotted-name sa":       {with("agents@example-project.iam.gserviceaccount.com", "a.gent@example-project.iam.gserviceaccount.com"), false},
		"double-slash creds":   {with("/etc/scion/cr.json", "/etc//cr.json"), false},
		"dotdot creds":         {with("/etc/scion/cr.json", "/etc/scion/../cr.json"), false},
		"dot creds":            {with("/etc/scion/cr.json", "/etc/./cr.json"), false},
		"trailing-slash creds": {with("/etc/scion/cr.json", "/etc/scion/"), false},
		"trailing-dot creds":   {with("/etc/scion/cr.json", "/etc/scion/."), false},
		"dotted-file creds":    {with("/etc/scion/cr.json", "/etc/scion/.cr.json"), true},
		"no network":           {with(", subnetwork: agents-subnet", ""), false},
		"empty networks":       {with("subnetwork: agents-subnet", "subnetwork: \"\", network: \"\""), false},
		"no credentials":       {with(", credentials_file: /etc/scion/cr.json", ""), false},
		"empty credentials":    {with("credentials_file: /etc/scion/cr.json", "credentials_file: \"\""), false},
		"relative credentials": {with("/etc/scion/cr.json", "cr.json"), false},
		"tilde credentials":    {with("/etc/scion/cr.json", "~/cr.json"), false},
		"env credentials":      {with("/etc/scion/cr.json", "/etc/$DIR/cr.json"), false},
		"list credentials":     {with("/etc/scion/cr.json", "\"/a.json:/b.json\""), false},
		"root credentials":     {with("/etc/scion/cr.json", "/"), false},
		"cloudrun context":     {entry(full + ", context: c"), false},
		"cloudrun namespace":   {entry(full + ", namespace: ns"), false},
		"cloudrun kubeconfig":  {entry(full + ", kubeconfig: /etc/kc"), false},
		"cloudrun gke":         {entry(full + ", gke: true"), false},
		"cloudrun gke false":   {entry(full + ", gke: false"), true},
		"cloudrun evict":       {entry(full + ", safe_to_evict: false"), false},
		"cloudrun selector":    {entry(full + ", node_selector: {a: b}"), false},
		"cloudrun tolerations": {entry(full + ", tolerations: [{key: k, operator: Exists}]"), false},
		"cloudrun rt class":    {entry(full + ", runtime_class_name: gvisor"), false},
		"cloudrun priority":    {entry(full + ", priority_class_name: high"), false},
		"cloudrun block sa":    {entry(full + ", kubernetes_block_service_account: zero-priv"), false},
		"cloudrun sd class":    {entry(full + ", shared_dir_storage_class: rwx"), false},
		"cloudrun home":        {entry(full + ", home_storage_backend: nfs"), false},
		"cloudrun empty ctx":   {entry(full + ", context: \"\""), true},
		"cloudrun num region":  {with("region: us-central1", "region: 5"), false},
		"cloudrun null sa":     {with("service_account: agents@example-project.iam.gserviceaccount.com", "service_account: null"), false},
		"docker project":       {entry("type: docker, project_id: example-project"), false},
		"docker region":        {entry("type: docker, region: us-central1"), false},
		"docker sa":            {entry("type: docker, service_account: agents@example-project.iam.gserviceaccount.com"), false},
		"docker network":       {entry("type: docker, network: agents-vpc"), false},
		"docker subnetwork":    {entry("type: docker, subnetwork: agents-subnet"), false},
		"docker credentials":   {entry("type: docker, credentials_file: /etc/scion/cr.json"), false},
		"docker empty project": {entry("type: docker, project_id: \"\""), true},
		"k8s project":          {entry("type: kubernetes, project_id: example-project"), false},
		"k8s region":           {entry("type: kubernetes, region: us-central1"), false},
		"k8s sa":               {entry("type: kubernetes, service_account: agents@example-project.iam.gserviceaccount.com"), false},
		"k8s network":          {entry("type: kubernetes, network: agents-vpc"), false},
		"k8s subnetwork":       {entry("type: kubernetes, subnetwork: agents-subnet"), false},
		"k8s credentials":      {entry("type: kubernetes, credentials_file: /etc/scion/cr.json"), false},
		"k8s empty network":    {entry("type: kubernetes, network: \"\""), true},
		"unsupported type":     {entry("type: cloudrun-instances"), false},
		"mixed-case type":      {entry(strings.Replace(full, "type: cloudrun", "type: CloudRun", 1)), false},
		"upper-case type":      {entry(strings.Replace(full, "type: cloudrun", "type: CLOUDRUN", 1)), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			schemaErrs, err := ValidateSettings([]byte(c.yaml), "1")
			if err != nil {
				t.Fatal(err)
			}
			if (len(schemaErrs) == 0) != c.valid {
				t.Fatalf("schema: valid=%v, want %v, errors=%v", len(schemaErrs) == 0, c.valid, schemaErrs)
			}
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			writeFlatTestFile(t, filepath.Join(dir, GlobalDir, "settings.yaml"), c.yaml)
			_, loadErr := LoadRuntimeBrokerInstances("")
			if (loadErr == nil) != c.valid {
				t.Fatalf("validator: valid=%v, want %v, err=%v", loadErr == nil, c.valid, loadErr)
			}
		})
	}
}
