//go:build !no_sqlite && (!hubshard || hubshard_2)

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

package hub

// A workstation server-config PUT checks the file-only blocks the hub
// validates at startup (server.hub.conduit, server.hub.port_proxy) with the
// same Validate methods, on the block settings.yaml would hold after the
// write, and refuses an invalid one with 422 before anything is written.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// startupBlockSettingsYAML is a minimal v1 settings file with stored
// conduit and port_proxy values.
const startupBlockSettingsYAML = `schema_version: "1"
server:
  hub:
    port: 9810
    conduit:
      lifetime_cap: 2h
    port_proxy:
      response_header_timeout: 45s
`

func startupBlockHome(t *testing.T, content string) string {
	t.Helper()
	path := tempSettingsHome(t)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type startupBlockErrorBody struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

func decodeStartupBlockError(t *testing.T, rr *httptest.ResponseRecorder) startupBlockErrorBody {
	t.Helper()
	var b startupBlockErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	return b
}

func TestWorkstation_PutServerConfig_StartupBlockValidSaved(t *testing.T) {
	cases := []struct {
		name string
		body string
		path []string
		want string
	}{
		{"port_proxy response_header_timeout", `{"server":{"hub":{"port_proxy":{"response_header_timeout":"30s"}}}}`,
			[]string{"server", "hub", "port_proxy", "response_header_timeout"}, "30s"},
		{"conduit lifetime_cap", `{"server":{"hub":{"conduit":{"lifetime_cap":"3h"}}}}`,
			[]string{"server", "hub", "conduit", "lifetime_cap"}, "3h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := startupBlockHome(t, startupBlockSettingsYAML)
			srv, _, _ := newSQLiteHubInMode(t, true, nil)
			rr := putServerConfig(t, srv, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			if got := yamlAt(readYAMLMap(t, settingsPath), tc.path...); got != tc.want {
				t.Errorf("%v = %v, want %q\n%s", tc.path, got, tc.want, readFileString(t, settingsPath))
			}
		})
	}
}

func TestWorkstation_PutServerConfig_StartupBlockInvalidRejected(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		field   string
		wantMsg string
	}{
		{"port_proxy response_header_timeout below minimum",
			`"port_proxy":{"response_header_timeout":"1s"}`,
			"server.hub.port_proxy.response_header_timeout",
			config.HubPortProxyConfig{ResponseHeaderTimeout: "1s"}.Validate().Error()},
		{"conduit lifetime_cap below minimum",
			`"conduit":{"lifetime_cap":"10s"}`,
			"server.hub.conduit.lifetime_cap",
			config.HubConduitConfig{LifetimeCap: "10s"}.Validate().Error()},
		{"conduit stream_authz_max.user below minimum",
			`"conduit":{"stream_authz_max":{"user":"10s"}}`,
			"server.hub.conduit.stream_authz_max.user",
			config.HubConduitConfig{StreamAuthzMax: config.HubConduitStreamAuthzMax{User: "10s"}}.Validate().Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := startupBlockHome(t, startupBlockSettingsYAML)
			srv, st, _ := newSQLiteHubInMode(t, true, map[string]string{
				"quotas": `{"enforce_broker_quotas":true}`,
			})
			before := readFileString(t, settingsPath)
			rowsBefore := hubSettingRevisions(t, st)

			// A Layer-1 change rides along: it must not be written either.
			rr := putServerConfig(t, srv, `{"quotas":{"enforce_broker_quotas":false},"server":{"hub":{`+tc.block+`}}}`)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
			}
			b := decodeStartupBlockError(t, rr)
			if b.Error.Code != ErrCodeValidationError {
				t.Errorf("code = %q, want %q", b.Error.Code, ErrCodeValidationError)
			}
			if b.Error.Details["field"] != tc.field {
				t.Errorf("details.field = %v, want %q", b.Error.Details["field"], tc.field)
			}
			if b.Error.Message != tc.wantMsg {
				t.Errorf("message = %q, want the startup message %q", b.Error.Message, tc.wantMsg)
			}
			if after := readFileString(t, settingsPath); after != before {
				t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
			}
			if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
				t.Errorf("DB changed on a refused PUT:\nbefore: %v\nafter:  %v", rowsBefore, after)
			}
			noTempSettingsFiles(t, settingsPath)
		})
	}
}

// The refusal is exactly {"error":{"code","message","details":{"field"}}}:
// the shape of the other settings validation errors, with no extra field.
func TestWorkstation_PutServerConfig_StartupBlockErrorShape(t *testing.T) {
	startupBlockHome(t, startupBlockSettingsYAML)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	rr := putServerConfig(t, srv, `{"server":{"hub":{"port_proxy":{"response_header_timeout":"1h"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	want := map[string]interface{}{
		"error": map[string]interface{}{
			"code":    ErrCodeValidationError,
			"message": config.HubPortProxyConfig{ResponseHeaderTimeout: "1h"}.Validate().Error(),
			"details": map[string]interface{}{"field": "server.hub.port_proxy.response_header_timeout"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body = %s\nwant %v", rr.Body.String(), want)
	}
}

// When several values of a block fail, details.field names the first sent
// one and the message is the joined text startup prints for the block.
func TestWorkstation_PutServerConfig_StartupBlockSeveralInvalid(t *testing.T) {
	settingsPath := startupBlockHome(t, startupBlockSettingsYAML)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	before := readFileString(t, settingsPath)
	rr := putServerConfig(t, srv, `{"server":{"hub":{"conduit":{"reconnect_window":"1h","lifetime_cap":"10s"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	b := decodeStartupBlockError(t, rr)
	if b.Error.Code != ErrCodeValidationError || b.Error.Details["field"] != "server.hub.conduit.lifetime_cap" {
		t.Errorf("got code %q field %v, want %q server.hub.conduit.lifetime_cap", b.Error.Code, b.Error.Details["field"], ErrCodeValidationError)
	}
	if want := (config.HubConduitConfig{LifetimeCap: "10s", ReconnectWindow: "1h"}).Validate().Error(); b.Error.Message != want {
		t.Errorf("message = %q, want the startup message %q", b.Error.Message, want)
	}
	if after := readFileString(t, settingsPath); after != before {
		t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
	}
}

// A save that sends no key under these blocks neither checks nor changes
// them, even when the file holds a value startup would refuse.
func TestWorkstation_PutServerConfig_StartupBlockUnrelatedSave(t *testing.T) {
	settingsPath := startupBlockHome(t, `schema_version: "1"
server:
  hub:
    conduit:
      lifetime_cap: 10s
    port_proxy:
      response_header_timeout: 1s
`)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	rr := putServerConfig(t, srv, `{"server":{"log_level":"debug","hub":{"port":9811}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	m := readYAMLMap(t, settingsPath)
	if got := yamlAt(m, "server", "hub", "conduit"); !reflect.DeepEqual(got, map[string]interface{}{"lifetime_cap": "10s"}) {
		t.Errorf("conduit = %v, want untouched", got)
	}
	if got := yamlAt(m, "server", "hub", "port_proxy"); !reflect.DeepEqual(got, map[string]interface{}{"response_header_timeout": "1s"}) {
		t.Errorf("port_proxy = %v, want untouched", got)
	}
	if got := yamlAt(m, "server", "hub", "port"); got != 9811 {
		t.Errorf("port = %v, want 9811 (the unrelated save was written)", got)
	}
}

// The check runs on the block as the file would hold it after the write:
// a stored value startup would refuse fails a save that touches its block
// (naming the stored key), and a save that fixes or removes it succeeds.
func TestWorkstation_PutServerConfig_StartupBlockMergedWithStored(t *testing.T) {
	const stored = `schema_version: "1"
server:
  hub:
    conduit:
      lifetime_cap: 10s
`
	settingsPath := startupBlockHome(t, stored)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)

	rr := putServerConfig(t, srv, `{"server":{"hub":{"conduit":{"reconnect_window":"10s"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if b := decodeStartupBlockError(t, rr); b.Error.Details["field"] != "server.hub.conduit.lifetime_cap" {
		t.Errorf("details.field = %v, want server.hub.conduit.lifetime_cap", b.Error.Details["field"])
	}
	if after := readFileString(t, settingsPath); after != stored {
		t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
	}

	// A sent invalid value is named before a stored one.
	// Both fail, so the message is the joined text startup prints.
	rr = putServerConfig(t, srv, `{"server":{"hub":{"conduit":{"reconnect_window":"1h"}}}}`)
	if b := decodeStartupBlockError(t, rr); rr.Code != http.StatusUnprocessableEntity || b.Error.Details["field"] != "server.hub.conduit.reconnect_window" {
		t.Errorf("got %d field %v, want 422 server.hub.conduit.reconnect_window", rr.Code, b.Error.Details["field"])
	} else if want := (config.HubConduitConfig{LifetimeCap: "10s", ReconnectWindow: "1h"}).Validate().Error(); b.Error.Message != want {
		t.Errorf("message = %q, want the startup message %q", b.Error.Message, want)
	}

	if rr := putServerConfig(t, srv, `{"server":{"hub":{"conduit":{"lifetime_cap":"2h","reconnect_window":"10s"}}}}`); rr.Code != http.StatusOK {
		t.Fatalf("fixing save: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := yamlAt(readYAMLMap(t, settingsPath), "server", "hub", "conduit", "lifetime_cap"); got != "2h" {
		t.Errorf("lifetime_cap = %v, want 2h", got)
	}

	startupBlockHome(t, stored)
	srv, _, _ = newSQLiteHubInMode(t, true, nil)
	if rr := putServerConfig(t, srv, `{"server":{"hub":{"conduit":null}}}`); rr.Code != http.StatusOK {
		t.Fatalf("null conduit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// A hosted hub still refuses these keys as Layer-0, valid or not; the
// startup check never runs there.
func TestHosted_PutServerConfig_StartupBlocksStillLayer0(t *testing.T) {
	cases := []struct {
		body string
		key  string
	}{
		{`{"server":{"hub":{"conduit":{"lifetime_cap":"3h"}}}}`, "server.hub.conduit.lifetime_cap"},
		{`{"server":{"hub":{"conduit":{"lifetime_cap":"10s"}}}}`, "server.hub.conduit.lifetime_cap"},
		{`{"server":{"hub":{"port_proxy":{"response_header_timeout":"30s"}}}}`, "server.hub.port_proxy.response_header_timeout"},
		{`{"server":{"hub":{"port_proxy":{"response_header_timeout":"1s"}}}}`, "server.hub.port_proxy.response_header_timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			settingsPath := startupBlockHome(t, startupBlockSettingsYAML)
			srv, _, _ := newSQLiteHubInMode(t, false, nil)
			before := readFileString(t, settingsPath)
			rr := putServerConfig(t, srv, tc.body)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
			}
			if code, keys := rejectedKeys(t, rr); code != "layer0_rejected" || !reflect.DeepEqual(keys, []string{tc.key}) {
				t.Errorf("got %q %v, want layer0_rejected [%s]", code, keys, tc.key)
			}
			if after := readFileString(t, settingsPath); after != before {
				t.Errorf("settings.yaml changed:\n%s", after)
			}
		})
	}
}
