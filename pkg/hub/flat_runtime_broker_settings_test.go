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
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// P1.1 Hub-side tests of the flat Runtime Broker contract that run for real
// (.design/flat-runtime-brokers-contract.md section 15, groups A and D).

func TestFlatRuntimeBrokersExperiment_DefaultOffInHub(t *testing.T) {
	srv := &Server{experiments: experiments.Default()}
	if srv.experimentEnabled(experiments.FlatRuntimeBrokers) {
		t.Fatal("hub.flat_runtime_brokers must be off without an override")
	}
}

func TestFlatRuntimeBrokerErrorCodes_FrozenValues(t *testing.T) {
	want := map[string]string{
		ErrCodeRuntimeTargetMismatch:            "runtime_target_mismatch",
		ErrCodeRuntimeProfileUnsupported:        "runtime_profile_unsupported",
		ErrCodeRuntimeTargetRequired:            "runtime_target_required",
		ErrCodeRuntimeTargetChanged:             "runtime_target_changed",
		ErrCodeRuntimeBrokerNotFlat:             "runtime_broker_not_flat",
		ErrCodeRuntimeBrokerNameConflict:        "runtime_broker_name_conflict",
		ErrCodeRuntimeTargetMoveUnsupported:     "runtime_target_move_unsupported",
		ErrCodeRuntimeTargetPinStale:            "runtime_target_pin_stale",
		ErrCodeRuntimeBrokerNotLinked:           "runtime_broker_not_linked",
		ErrCodeRuntimeBrokerLinkPathUnsupported: "runtime_broker_link_path_unsupported",
		ErrCodeExperimentDisabled:               "experiment_disabled",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("code %q != frozen %q", got, w)
		}
	}
	if ErrCodeRuntimeTargetMismatch != api.ErrCodeRuntimeTargetMismatch {
		t.Fatal("shared wire code must alias pkg/api")
	}
	r := &RuntimeTargetRefusal{Code: ErrCodeRuntimeTargetPinStale, Status: http.StatusConflict, Message: "stale"}
	if r.Error() != "runtime_target_pin_stale: stale" {
		t.Fatalf("Error() = %q", r.Error())
	}
}

const flatInstanceSettingsYAML = `schema_version: "1"
server:
  broker:
    enabled: true
    broker_id: b-123
    broker_token: tok-secret
    instances:
      - key: local-docker
        name: example-docker
        runtime_target:
          type: docker
`

func flatSettingsHome(t *testing.T, content string) string {
	t.Helper()
	path := tempSettingsHome(t)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func putFileModeServerConfig(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr
}

func storedInstances(t *testing.T) []config.V1RuntimeBrokerInstanceConfig {
	t.Helper()
	inst, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatalf("stored instances must stay valid: %v", err)
	}
	return inst
}

var wantFlatInstance = []config.V1RuntimeBrokerInstanceConfig{{
	Key: "local-docker", Name: "example-docker", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"},
}}

func TestServerConfigPut_FileModePreservesRuntimeBrokerInstances(t *testing.T) {
	t.Run("absent key keeps the stored entry", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		// The admin editor sends server.broker built from form fields only.
		rr := putFileModeServerConfig(t, `{"server":{"broker":{"enabled":true,"port":9811}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
			t.Fatalf("instances dropped or changed: %+v", got)
		}
	})
	t.Run("explicit list is validated and applied", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"server":{"broker":{"enabled":true,"instances":[{"key":"other","name":"other-docker","runtime_target":{"type":"docker"}}]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		got := storedInstances(t)
		if len(got) != 1 || got[0].Key != "other" {
			t.Fatalf("explicit list not applied: %+v", got)
		}
	})
	t.Run("explicit empty list removes", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"server":{"broker":{"enabled":true,"instances":[]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		if got := storedInstances(t); len(got) != 0 {
			t.Fatalf("explicit [] must remove: %+v", got)
		}
	})
	t.Run("null server.broker keeps the stored entry", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"server":{"broker":null}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
			t.Fatalf("a null server.broker must keep instances: %+v", got)
		}
	})
	t.Run("null server keeps the stored entry", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"server":null}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
			t.Fatalf("a null server must keep instances: %+v", got)
		}
	})
	t.Run("explicit null instances removes", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"server":{"broker":{"enabled":true,"instances":null}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		if got := storedInstances(t); len(got) != 0 {
			t.Fatalf("an explicit null must remove: %+v", got)
		}
	})
	t.Run("unrelated non-broker key keeps the stored entry", func(t *testing.T) {
		path := flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"default_template":"other-template"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
			t.Fatalf("an unrelated edit changed instances: %+v", got)
		}
		if after := readFileString(t, path); !strings.Contains(after, "other-template") {
			t.Fatalf("the unrelated key was not written:\n%s", after)
		}
	})
	t.Run("invalid explicit list is rejected before writing", func(t *testing.T) {
		path := flatSettingsHome(t, flatInstanceSettingsYAML)
		before, _ := os.ReadFile(path)
		for _, body := range []string{
			`{"server":{"broker":{"instances":[{"key":"a","name":"x","runtime_target":{"type":"kubernetes"}}]}}}`,
			`{"server":{"broker":{"instances":[{"key":"a","name":"x","profile":"p","runtime_target":{"type":"docker"}}]}}}`,
		} {
			rr := putFileModeServerConfig(t, body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400 for %s, got %d %s", body, rr.Code, rr.Body.String())
			}
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("a rejected PUT must not touch settings.yaml")
		}
	})
}

// TestServerConfigPut_FileModeInstancesAreKnownKeys: server.broker.instances
// is decoded by the instances guard before the unknown-key rejection runs.
// A valid entry using every instance field is applied (200, not 422), and
// an unknown key outside instances in the same body is still rejected with
// 422 before anything is written.
func TestServerConfigPut_FileModeInstancesAreKnownKeys(t *testing.T) {
	const fullInstances = `"instances":[{"key":"other","name":"other-docker","runtime_target":{"type":"docker","display_name":"Other Docker"}}]`
	t.Run("full-field instances entry is applied", func(t *testing.T) {
		flatSettingsHome(t, flatInstanceSettingsYAML)
		rr := putFileModeServerConfig(t, `{"server":{"broker":{"enabled":true,`+fullInstances+`}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: want 200, got %d %s", rr.Code, rr.Body.String())
		}
		got := storedInstances(t)
		if len(got) != 1 || got[0].Key != "other" || got[0].RuntimeTarget == nil || got[0].RuntimeTarget.DisplayName != "Other Docker" {
			t.Fatalf("full-field entry not applied: %+v", got)
		}
	})
	t.Run("unknown non-instances key in the same body is still 422", func(t *testing.T) {
		path := flatSettingsHome(t, flatInstanceSettingsYAML)
		before, _ := os.ReadFile(path)
		rr := putFileModeServerConfig(t, `{"not_a_setting":"x","server":{"broker":{"enabled":true,`+fullInstances+`}}}`)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT: want 422, got %d %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "not_a_setting") || strings.Contains(rr.Body.String(), "instances") {
			t.Fatalf("422 must name only the unknown key: %s", rr.Body.String())
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Fatal("a rejected PUT must not touch settings.yaml")
		}
		if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
			t.Fatalf("instances changed by a rejected PUT: %+v", got)
		}
	})
}

func TestServerConfigPut_WorkstationDBExplicitInstancesValidatedAndApplied(t *testing.T) {
	path := flatSettingsHome(t, flatInstanceSettingsYAML)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	before, _ := os.ReadFile(path)
	rr := putServerConfig(t, srv, `{"server":{"broker":{"instances":[{"key":"a","name":"x","runtime_target":{"type":"podman"}}]}}}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid list: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("an invalid list must be rejected before any write")
	}
	rr = putServerConfig(t, srv, `{"server":{"broker":{"instances":[{"key":"other","name":"other-docker","runtime_target":{"type":"docker"}}]}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid list: %d %s", rr.Code, rr.Body.String())
	}
	got := storedInstances(t)
	if len(got) != 1 || got[0].Key != "other" || got[0].Name != "other-docker" {
		t.Fatalf("explicit list not written: %+v", got)
	}
}

func TestServerConfigPut_WorkstationDBNullBrokerKeepsInstances(t *testing.T) {
	for _, body := range []string{`{"server":{"broker":null}}`, `{"server":null}`} {
		t.Run(body, func(t *testing.T) {
			path := flatSettingsHome(t, flatInstanceSettingsYAML)
			srv, _, _ := newSQLiteHubInMode(t, true, nil)
			rr := putServerConfig(t, srv, body)
			if rr.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
			}
			if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
				t.Fatalf("a null must keep instances: %+v\n%s", got, readFileString(t, path))
			}
			m := readYAMLMap(t, path)
			if yamlAt(m, "server", "broker", "broker_id") != "b-123" {
				t.Fatal("hub-owned broker_id must still be kept")
			}
		})
	}
}

func TestServerConfigPut_WorkstationDBUnrelatedKeysUnchanged(t *testing.T) {
	path := flatSettingsHome(t, flatInstanceSettingsYAML)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	rr := putServerConfig(t, srv, `{"server":{"broker":{"port":9812}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	if got := storedInstances(t); !reflect.DeepEqual(got, wantFlatInstance) {
		t.Fatalf("instances changed by an unrelated edit: %+v", got)
	}
	after := readFileString(t, path)
	if !strings.Contains(after, "port: 9812") || !strings.Contains(after, "broker_token: tok-secret") {
		t.Fatalf("unexpected settings after an unrelated edit:\n%s", after)
	}
}
