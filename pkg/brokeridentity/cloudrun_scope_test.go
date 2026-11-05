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

package brokeridentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Cloud Run identity (contract amendment P5-CR, CR3a and CR3).

func cloudRunScope(number, region, projectID string) ExecutionScope {
	return ExecutionScope{Type: TargetTypeCloudRun, CloudRun: &CloudRunScope{ProjectNumber: number, Region: region, ProjectID: projectID}}
}

func cloudRunInstanceDir(t *testing.T) string {
	t.Helper()
	return InstanceDir(t.TempDir(), "cr-us-central1")
}

// TestLoadOrCreate_CloudRunMintAndReload: a cloudrun instance mints two
// random IDs bound to its project number and region, records the file
// shape of CR3a (no service account, network or credentials), and keeps
// both IDs across a restart.
func TestLoadOrCreate_CloudRunMintAndReload(t *testing.T) {
	dir := cloudRunInstanceDir(t)
	scope := cloudRunScope("123456789012", "us-central1", "example-project")
	first, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, scope, nil)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if first.RuntimeTarget.Type != TargetTypeCloudRun || first.RuntimeBrokerID == "" || first.RuntimeTarget.ID == "" ||
		first.RuntimeBrokerID == first.RuntimeTarget.ID {
		t.Fatalf("unexpected identity: %+v", first)
	}
	if first.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", first.SchemaVersion)
	}
	data, err := os.ReadFile(filepath.Join(dir, IdentityFileName))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ExecutionScope map[string]json.RawMessage `json:"executionScope"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var cr map[string]string
	if err := json.Unmarshal(raw.ExecutionScope["cloudrun"], &cr); err != nil {
		t.Fatalf("executionScope.cloudrun: %v (%s)", err, data)
	}
	want := map[string]string{"projectNumber": "123456789012", "region": "us-central1", "projectId": "example-project"}
	if len(cr) != len(want) {
		t.Fatalf("executionScope.cloudrun = %v, want exactly %v", cr, want)
	}
	for k, v := range want {
		if cr[k] != v {
			t.Fatalf("executionScope.cloudrun.%s = %q, want %q", k, cr[k], v)
		}
	}
	for _, absent := range []string{"serviceAccount", "service_account", "network", "subnetwork", "credentials"} {
		if strings.Contains(string(data), absent) {
			t.Fatalf("identity file records %q: %s", absent, data)
		}
	}

	second, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, scope, nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if second.RuntimeBrokerID != first.RuntimeBrokerID || second.RuntimeTarget.ID != first.RuntimeTarget.ID {
		t.Fatalf("IDs changed across restart: %+v vs %+v", first, second)
	}

	// A project ID is informational: an empty one mints too.
	if _, err := LoadOrCreate(InstanceDir(t.TempDir(), "cr-b"), "cr-b", TargetTypeCloudRun, cloudRunScope("42", "europe-west4", ""), nil); err != nil {
		t.Fatalf("mint without a project ID: %v", err)
	}
}

// TestLoadOrCreate_CloudRunScopeChangedRefuses: another project number or
// another region is a different scope, refused without re-pointing; a
// different target type is refused too.
func TestLoadOrCreate_CloudRunScopeChangedRefuses(t *testing.T) {
	dir := cloudRunInstanceDir(t)
	if _, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-project"), nil); err != nil {
		t.Fatal(err)
	}
	for name, observed := range map[string]ExecutionScope{
		"other project number": cloudRunScope("999999999999", "us-central1", "example-project"),
		"other region":         cloudRunScope("123456789012", "europe-west4", "example-project"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, observed, nil)
			if !errors.Is(err, ErrExecutionScopeChanged) {
				t.Fatalf("got %v, want ErrExecutionScopeChanged", err)
			}
		})
	}
	if _, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("", "us-central1", "example-project"), nil); !errors.Is(err, ErrExecutionScopeUnidentified) {
		t.Fatalf("an observed scope without a number: got %v, want ErrExecutionScopeUnidentified", err)
	}
	if _, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeDocker, dockerScope("D1", ""), nil); !errors.Is(err, ErrRuntimeTargetTypeChanged) {
		t.Fatalf("type change: got %v, want ErrRuntimeTargetTypeChanged", err)
	}
	if err := VerifyScope(cloudRunScope("1", "us-central1", ""), dockerScope("D1", "")); !errors.Is(err, ErrExecutionScopeChanged) {
		t.Fatalf("observed type change: got %v, want ErrExecutionScopeChanged", err)
	}
}

// TestLoadOrCreate_CloudRunProjectIDAliasAccepted: the same project number
// and region under another project ID is the same target, and the stored
// project ID keeps its first-boot value.
func TestLoadOrCreate_CloudRunProjectIDAliasAccepted(t *testing.T) {
	dir := cloudRunInstanceDir(t)
	first, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-project"), nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-alias"), nil)
	if err != nil {
		t.Fatalf("same number and region with another project ID must be accepted: %v", err)
	}
	if again.RuntimeTarget.ID != first.RuntimeTarget.ID || again.RuntimeBrokerID != first.RuntimeBrokerID {
		t.Fatalf("IDs changed: %+v vs %+v", first, again)
	}
	if got := again.ExecutionScope.CloudRun.ProjectID; got != "example-project" {
		t.Fatalf("stored project ID = %q, want the first-boot value", got)
	}
}

// TestScopeIdentity_CloudRunEmptyNumberUnidentified: the scope identity is
// cloudrun:<number>/<region>; a missing number, region or record is
// unidentified, and so is a scope whose type names no record.
func TestScopeIdentity_CloudRunEmptyNumberUnidentified(t *testing.T) {
	got, err := ScopeIdentity(cloudRunScope("123456789012", "us-central1", "example-project"))
	if err != nil || got != "cloudrun:123456789012/us-central1" {
		t.Fatalf("ScopeIdentity = %q, %v", got, err)
	}
	if id, _ := ScopeIdentity(cloudRunScope("123456789012", "us-central1", "another-id")); id != got {
		t.Fatalf("the project ID changed the identity: %q vs %q", id, got)
	}
	for name, s := range map[string]ExecutionScope{
		"empty number":   cloudRunScope("", "us-central1", "example-project"),
		"blank number":   cloudRunScope("  ", "us-central1", "example-project"),
		"empty region":   cloudRunScope("123456789012", "", "example-project"),
		"no record":      {Type: TargetTypeCloudRun},
		"docker record":  {Type: TargetTypeCloudRun, Docker: &DockerScope{DaemonID: "D1"}},
		"cloudrun typed": {Type: TargetTypeDocker, CloudRun: &CloudRunScope{ProjectNumber: "1", Region: "us-central1"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ScopeIdentity(s); !errors.Is(err, ErrExecutionScopeUnidentified) {
				t.Fatalf("got %v, want ErrExecutionScopeUnidentified", err)
			}
			if _, err := LoadOrCreate(InstanceDir(t.TempDir(), "cr-x"), "cr-x", TargetTypeCloudRun, s, nil); !errors.Is(err, ErrExecutionScopeUnidentified) {
				t.Fatalf("mint: got %v, want ErrExecutionScopeUnidentified", err)
			}
		})
	}
}

// TestIdentity_CloudRunFileWithoutNumberIsCorrupt: a recorded cloudrun scope
// without its identity fields is reported corrupt and never re-minted.
func TestIdentity_CloudRunFileWithoutNumberIsCorrupt(t *testing.T) {
	dir := cloudRunInstanceDir(t)
	id, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-project"), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, IdentityFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), `"123456789012"`, `""`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-project"), nil)
	if !errors.Is(err, ErrIdentityCorrupt) {
		t.Fatalf("got %v, want ErrIdentityCorrupt", err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), id.RuntimeBrokerID) {
		t.Fatal("the identity file was replaced")
	}
}

// TestLoadOrCreate_CloudRunProjectIDAliasLogged: loading an identity whose
// project number and region match under another project ID logs one info
// line naming the instance key and neither project ID nor the project number;
// the same project ID, and VerifyScope on its own, log nothing.
func TestLoadOrCreate_CloudRunProjectIDAliasLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := cloudRunInstanceDir(t)
	if _, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-project"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-project"), nil); err != nil {
		t.Fatal(err)
	}
	if err := VerifyScope(cloudRunScope("123456789012", "us-central1", "example-project"), cloudRunScope("123456789012", "us-central1", "example-alias")); err != nil {
		t.Fatalf("alias refused: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("logged without an alias load: %s", buf.String())
	}
	if _, err := LoadOrCreate(dir, "cr-us-central1", TargetTypeCloudRun, cloudRunScope("123456789012", "us-central1", "example-alias"), nil); err != nil {
		t.Fatalf("alias refused: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "project ID alias accepted") != 1 || !strings.Contains(out, "level=INFO") || !strings.Contains(out, "instance=cr-us-central1") {
		t.Fatalf("want one info line naming the instance, got %q", out)
	}
	for _, id := range []string{"example-project", "example-alias", "123456789012"} {
		if strings.Contains(out, id) {
			t.Fatalf("the log line names project ID or number %q: %q", id, out)
		}
	}
}
