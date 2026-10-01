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

// Package settingscheck is a test-only harness for
// modules/hub-cloudrun/templates/settings.yaml.tftpl. It makes no product
// code change: it only renders the template with fixture values and decodes
// the result strictly against the real config.VersionedSettings struct.
//
// Why this exists: config.LoadGlobalSettings silently ignores keys it
// doesn't recognize (the koanf-based loader has no "unknown field" mode), so
// a settings file loading successfully does not prove every key in it binds
// to something the hub actually reads. A key that's misspelled, nested under
// the wrong parent, or left over from a renamed field will load with zero
// warnings and simply do nothing at runtime. Decoding the same YAML with
// yaml.v3's KnownFields(true) instead catches exactly that class of mistake,
// because it fails loudly on any key the target struct doesn't declare.
//
// Why shell out to `terraform` to render the template, instead of
// reimplementing template evaluation in Go: the template isn't plain YAML.
// It uses Terraform's own interpolation syntax (`${...}`), a conditional
// directive (`%{ if ... ~}`), and the `jsonencode(...)` function. A
// hand-rolled Go substitution would only be an approximation of Terraform's
// template language, and could drift from it silently -- passing on
// something real Terraform would reject, or vice versa. The CI job that
// runs this test already installs `terraform` to run `fmt`, `validate`,
// `tflint` and `terraform test` against the very module this template lives
// in, so shelling out adds no new dependency, and it exercises the exact
// `templatefile()` code path production rendering also goes through. If
// `terraform` isn't on PATH (e.g. a plain local `go test ./...` outside the
// CI job), the test skips with an explicit message rather than failing --
// this check is meant to run wherever terraform already runs, not
// everywhere `go test` runs.
package settingscheck

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"
)

// templatePath resolves the absolute path to the real, production
// settings.yaml.tftpl, relative to this test file's own location, so the
// test works no matter what directory `go test` is invoked from.
func templatePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate this test file's directory")
	}
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", "modules", "hub-cloudrun", "templates", "settings.yaml.tftpl")
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("resolving settings template path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("settings template not found at %s: %v", abs, err)
	}
	return abs
}

// fixtureVars is the full set of top-level names settings.yaml.tftpl's
// templatefile() call needs, with realistic-looking values. Keep this in
// sync with the template: if it gains a new top-level reference, terraform
// fails the render with "no value for required variable" naming the
// missing key, which points straight back here.
func fixtureVars() map[string]any {
	return map[string]any{
		"image_registry":       "us-central1-docker.pkg.dev/example-project/tfha-scion",
		"hub_name":             "tfha-h1",
		"public_url":           "https://tfha-h1-123456789012.us-central1.run.app",
		"hub_write_timeout":    "300s",
		"max_open_conns":       10,
		"bucket":               "example-project-tfha-h1-artifacts",
		"project_id":           "example-project",
		"admin_emails":         []string{"admin@example.com"},
		"iap_audience":         "/projects/123456789012/locations/us-central1/services/tfha-h1",
		"iap_oauth_client_id":  "123456789012-abc.apps.googleusercontent.com",
		"transport_sa_email":   "tfha-h1-transport@example-project.iam.gserviceaccount.com",
		"broker_id":            "3372fd2e-0000-4000-8000-000000000000",
		"broker_name":          "tfha-h1-broker",
		"broker_write_timeout": "120s",
		"nfs_mount_root":       "/mnt/nfs",
		"nfs_uid":              1000,
		"nfs_gid":              1000,
		"nfs_subpath_root":     "projects",
		"nfs_server":           "10.0.0.2",
		"nfs_export":           "/scion/tfha-h1",
		"pv_name":              "tfha-h1-nfs",
		"namespace":            "tfha-h1",
	}
}

// renderTemplate shells out to terraform to evaluate templatefile() over the
// real settings template with the given variables, and returns the
// rendered YAML text. It skips the calling test if terraform isn't
// available.
func renderTemplate(t *testing.T, vars map[string]any) string {
	t.Helper()

	tfBin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not found on PATH; skipping settingscheck render/decode test (this check is meant to run in CI's terraform job, where terraform is already installed for fmt/validate/tflint/test)")
	}

	dir := t.TempDir()

	const mainTF = `
terraform {
  required_version = ">= 1.9"
}

variable "tpl_path" {
  type = string
}

variable "fixture" {
  type = any
}

output "rendered" {
  value = templatefile(var.tpl_path, var.fixture)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(mainTF), 0o644); err != nil {
		t.Fatalf("writing scratch main.tf: %v", err)
	}

	varsPayload := map[string]any{
		"tpl_path": templatePath(t),
		"fixture":  vars,
	}
	varsJSON, err := json.Marshal(varsPayload)
	if err != nil {
		t.Fatalf("marshaling fixture vars: %v", err)
	}
	// terraform auto-loads *.auto.tfvars.json with no extra flag needed.
	if err := os.WriteFile(filepath.Join(dir, "fixture.auto.tfvars.json"), varsJSON, 0o644); err != nil {
		t.Fatalf("writing fixture.auto.tfvars.json: %v", err)
	}

	run := func(args ...string) []byte {
		cmd := exec.Command(tfBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("terraform %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	// No backend, no provider, no credentials: templatefile() is a pure
	// function over local files, so this never touches the network or GCP.
	run("init", "-input=false", "-backend=false")
	run("apply", "-input=false", "-auto-approve")

	outCmd := exec.Command(tfBin, "output", "-raw", "rendered")
	outCmd.Dir = dir
	outCmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	rendered, err := outCmd.Output()
	if err != nil {
		t.Fatalf("terraform output -raw rendered failed: %v", err)
	}
	return string(rendered)
}

// decodeStrict decodes yamlText into a config.VersionedSettings using
// yaml.v3's KnownFields(true), so any key the struct doesn't declare is a
// decode error rather than a silent no-op.
func decodeStrict(yamlText string) (*config.VersionedSettings, error) {
	var vs config.VersionedSettings
	dec := yaml.NewDecoder(strings.NewReader(yamlText))
	dec.KnownFields(true)
	if err := dec.Decode(&vs); err != nil {
		return nil, err
	}
	return &vs, nil
}

// TestSettingsTemplateRendersAndDecodes renders settings.yaml.tftpl with a
// realistic fixture and proves every emitted key decodes into a field
// config.VersionedSettings actually declares, then spot-checks the fields
// that gate the most load-bearing behavior (schema_version, the profile ->
// runtime resolution, the workspace_storage backend, and hub_id -- see
// settings_v1.go's V1ServerHubConfig.HubID, which is what the hub uses to
// namespace its Secret Manager reads, not the legacy hub_config.go field of
// almost the same name).
func TestSettingsTemplateRendersAndDecodes(t *testing.T) {
	vars := fixtureVars()
	rendered := renderTemplate(t, vars)

	vs, err := decodeStrict(rendered)
	if err != nil {
		t.Fatalf("settings.yaml.tftpl rendered a key that config.VersionedSettings does not declare: %v\n--- rendered ---\n%s", err, rendered)
	}

	if got, want := vs.SchemaVersion, "1"; got != want {
		t.Errorf("schema_version = %q, want %q", got, want)
	}
	if got, want := vs.ImageRegistry, vars["image_registry"].(string); got != want {
		t.Errorf("image_registry = %q, want %q", got, want)
	}
	if vs.Server == nil || vs.Server.Hub == nil {
		t.Fatal("server.hub is nil after decode")
	}
	if got, want := vs.Server.Hub.HubID, vars["hub_name"].(string); got != want {
		t.Errorf("server.hub.hub_id = %q, want %q (this is the value the hub hashes into its Secret Manager scope prefix)", got, want)
	}
	if vs.Server.WorkspaceStorage == nil {
		t.Fatal("server.workspace_storage is nil after decode")
	}
	if got, want := vs.Server.WorkspaceStorage.Backend, "nfs"; got != want {
		t.Errorf("server.workspace_storage.backend = %q, want %q", got, want)
	}
	if vs.Server.WorkspaceStorage.NFS == nil {
		t.Fatal("server.workspace_storage.nfs is nil after decode")
	}

	rt, rtType, err := vs.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime(\"\") (i.e. via active_profile): %v", err)
	}
	if got, want := rtType, "kubernetes"; got != want {
		t.Errorf("resolved runtime type = %q, want %q (without the profiles block, this silently falls back to the embedded default runtime instead)", got, want)
	}
	if got, want := rt.Namespace, vars["namespace"].(string); got != want {
		t.Errorf("resolved runtime namespace = %q, want %q", got, want)
	}
	if rt.GKE {
		t.Error("resolved runtime gke = true, want false (the agent service account has no Secret Manager role; gke:true would build a SecretProviderClass it cannot read)")
	}
}

// TestSettingsTemplateRejectsUnknownKey is the required positive control: it
// proves decodeStrict actually fails closed on an unknown key, rather than
// TestSettingsTemplateRendersAndDecodes above passing only because nothing
// exercises the failure path. Without this, a KnownFields(true) call that
// was silently no-op'd (e.g. by an intervening step that re-marshals through
// a lenient decoder) would leave every test in this package green.
func TestSettingsTemplateRejectsUnknownKey(t *testing.T) {
	rendered := renderTemplate(t, fixtureVars())

	const bogusKey = "totally_bogus_settingscheck_positive_control"
	tampered := bogusKey + ": true\n" + rendered

	_, err := decodeStrict(tampered)
	if err == nil {
		t.Fatal("decodeStrict accepted a YAML document with an unknown top-level key; KnownFields(true) is not doing its job")
	}
	if !strings.Contains(err.Error(), bogusKey) && !strings.Contains(err.Error(), "not found") {
		t.Errorf("decode failed as expected, but the error doesn't look like an unknown-field error (got: %v) -- confirm this is still the KnownFields path and not some other decode failure", err)
	}
}
