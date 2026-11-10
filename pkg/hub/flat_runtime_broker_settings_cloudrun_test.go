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

//go:build !no_sqlite

package hub

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// cloudRunPutTarget is a valid cloudrun runtime_target in JSON, with the
// field named by override replaced by its value ("" removes the field).
func cloudRunPutTarget(override map[string]string) string {
	fields := []struct{ k, v string }{
		{"type", "cloudrun"},
		{"project_id", "example-project"},
		{"region", "us-central1"},
		{"service_account", "agents@example-project.iam.gserviceaccount.com"},
		{"subnetwork", "agents-subnet"},
		{"credentials_file", "/etc/scion/cr.json"},
	}
	var parts []string
	for _, f := range fields {
		v, ok := override[f.k]
		if !ok {
			v = f.v
		}
		if v == "" {
			continue
		}
		parts = append(parts, `"`+f.k+`":"`+v+`"`)
	}
	return `{` + strings.Join(parts, ",") + `}`
}

func cloudRunPutBody(target string) string {
	return `{"server":{"broker":{"instances":[{"key":"cr-a","name":"cloudrun-a","runtime_target":` + target + `}]}}}`
}

// TestAdminServerConfigPut_CloudRunValidation: the Hub admin server-config
// PUT applies the Cloud Run form rules of ValidateRuntimeBrokerInstances: a
// cloudrun entry without credentials_file, with a credentials_file that is
// not one absolute path, or with a project default service account is
// refused with 422 naming its path, and settings.yaml is not touched; a
// valid entry is applied.
func TestAdminServerConfigPut_CloudRunValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		override map[string]string
		field    string
	}{
		"no credentials_file":       {map[string]string{"credentials_file": ""}, "runtime_target.credentials_file"},
		"relative credentials_file": {map[string]string{"credentials_file": "cr.json"}, "runtime_target.credentials_file"},
		"tilde credentials_file":    {map[string]string{"credentials_file": "~/cr.json"}, "runtime_target.credentials_file"},
		"compute default sa":        {map[string]string{"service_account": "123456789012-compute@developer.gserviceaccount.com"}, "runtime_target.service_account"},
		"compute default mixed":     {map[string]string{"service_account": "123456789012-Compute@developer.gserviceaccount.com"}, "runtime_target.service_account"},
		"app engine default sa":     {map[string]string{"service_account": "example-project@appspot.gserviceaccount.com"}, "runtime_target.service_account"},
		"domain-scoped project":     {map[string]string{"project_id": "example.com:proj"}, "runtime_target.project_id"},
		"no network":                {map[string]string{"subnetwork": ""}, "runtime_target.network"},
	} {
		t.Run(name, func(t *testing.T) {
			path := flatSettingsHome(t, flatInstanceSettingsYAML)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rr := putFlatFileModeServerConfig(t, cloudRunPutBody(cloudRunPutTarget(tc.override)))
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d %s", rr.Code, rr.Body.String())
			}
			found := false
			for _, e := range settingsErrorEntries(t, rr) {
				if !strings.HasPrefix(e.Field, "server.broker.instances") {
					t.Errorf("field %q is not under server.broker.instances", e.Field)
				}
				if strings.HasSuffix(e.Field, tc.field) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no error at %s: %s", tc.field, rr.Body.String())
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("a refused PUT must not touch settings.yaml")
			}
		})
	}

	t.Run("valid entry is applied", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFlatFileModeServerConfig(t, cloudRunPutBody(cloudRunPutTarget(nil)))
		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d %s", rr.Code, rr.Body.String())
		}
		got := storedInstances(t)
		if len(got) != 1 || got[0].Key != "cr-a" || got[0].RuntimeTarget == nil ||
			got[0].RuntimeTarget.CredentialsFile != "/etc/scion/cr.json" || got[0].RuntimeTarget.Region != "us-central1" {
			t.Fatalf("stored instances: %+v", got)
		}
	})
}
