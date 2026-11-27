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

// File-mode server-config PUT (no OperationalSettings store wired) checks the
// startup-validated server blocks (server.hub.conduit, server.hub.port_proxy,
// and the rest of startupCheckedFileBlocks) with the same Validate methods
// startup and the workstation path use, on the merged settings.yaml block,
// refusing an invalid value with 422 validation_error before writing the file.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

const fileModeStartupBlockSettingsYAML = `schema_version: "1"
server:
  hub:
    port: 9810
    conduit:
      lifetime_cap: 2h
    port_proxy:
      response_header_timeout: 45s
`

func putFileModeStartupBlock(t *testing.T, stored, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(stored), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr, settingsPath
}

type fileModeStartupBlockErrorBody struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

func decodeFileModeStartupBlockError(t *testing.T, rr *httptest.ResponseRecorder) fileModeStartupBlockErrorBody {
	t.Helper()
	var b fileModeStartupBlockErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	return b
}

func readFileModeYAMLAt(t *testing.T, path string, keys ...string) interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	var cur interface{} = m
	for _, k := range keys {
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

func readFileModeString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFileMode_PutServerConfig_StartupBlockValidSaved(t *testing.T) {
	cases := []struct {
		name string
		body string
		path []string
		want string
	}{
		{"port_proxy response_header_timeout", `{"server":{"hub":{"port_proxy":{"response_header_timeout":"30s"}}}}`,
			[]string{"server", "hub", "port_proxy", "response_header_timeout"}, "30s"},
		{"port_proxy view_ttl", `{"server":{"hub":{"port_proxy":{"view_ttl":"4h"}}}}`,
			[]string{"server", "hub", "port_proxy", "view_ttl"}, "4h"},
		{"conduit lifetime_cap", `{"server":{"hub":{"conduit":{"lifetime_cap":"3h"}}}}`,
			[]string{"server", "hub", "conduit", "lifetime_cap"}, "3h"},
		{"sse max_connection_age", `{"server":{"hub":{"sse":{"max_connection_age":"5m"}}}}`,
			[]string{"server", "hub", "sse", "max_connection_age"}, "5m"},
		{"platform_request_timeout", `{"server":{"hub":{"platform_request_timeout":"900s"}}}`,
			[]string{"server", "hub", "platform_request_timeout"}, "900s"},
		{"chat search timeout", `{"server":{"hub":{"chat":{"search":{"timeout":"5s"}}}}}`,
			[]string{"server", "hub", "chat", "search", "timeout"}, "5s"},
		{"system_one max_eval_time", `{"server":{"hub":{"system_one":{"max_eval_time":"5s"}}}}`,
			[]string{"server", "hub", "system_one", "max_eval_time"}, "5s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, settingsPath := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			if got := readFileModeYAMLAt(t, settingsPath, tc.path...); got != tc.want {
				t.Errorf("%v = %v, want %q\n%s", tc.path, got, tc.want, readFileModeString(t, settingsPath))
			}
		})
	}
}

func TestFileMode_PutServerConfig_StartupBlockInvalidRejected(t *testing.T) {
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
		{"port_proxy view_ttl above maximum",
			`"port_proxy":{"view_ttl":"25h"}`,
			"server.hub.port_proxy.view_ttl",
			config.HubPortProxyConfig{ViewTTL: "25h"}.Validate().Error()},
		{"conduit lifetime_cap below minimum",
			`"conduit":{"lifetime_cap":"10s"}`,
			"server.hub.conduit.lifetime_cap",
			config.HubConduitConfig{LifetimeCap: "10s"}.Validate().Error()},
		{"conduit stream_authz_max.user below minimum",
			`"conduit":{"stream_authz_max":{"user":"10s"}}`,
			"server.hub.conduit.stream_authz_max.user",
			config.HubConduitConfig{StreamAuthzMax: config.HubConduitStreamAuthzMax{User: "10s"}}.Validate().Error()},
		{"sse heartbeat_interval above maximum",
			`"sse":{"heartbeat_interval":"2m"}`,
			"server.hub.sse.heartbeat_interval",
			config.HubSSEConfig{HeartbeatInterval: "2m"}.Validate().Error()},
		{"platform_request_timeout below minimum",
			`"platform_request_timeout":"30s"`,
			"server.hub.platform_request_timeout",
			config.ValidatePlatformRequestTimeout("30s").Error()},
		{"chat search timeout not a duration",
			`"chat":{"search":{"timeout":"soon"}}`,
			"server.hub.chat.search.timeout",
			config.HubChatSearchConfig{Timeout: "soon"}.Validate().Error()},
		{"chat search timeout negative",
			`"chat":{"search":{"timeout":"-1s"}}`,
			"server.hub.chat.search.timeout",
			config.HubChatSearchConfig{Timeout: "-1s"}.Validate().Error()},
		{"system_one max_eval_time not a duration",
			`"system_one":{"max_eval_time":"soon"}`,
			"server.hub.system_one.max_eval_time",
			config.HubSystemOneConfig{MaxEvalTime: "soon"}.Validate().Error()},
		{"system_one max_eval_time above maximum",
			`"system_one":{"max_eval_time":"45s"}`,
			"server.hub.system_one.max_eval_time",
			config.HubSystemOneConfig{MaxEvalTime: "45s"}.Validate().Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A sibling change rides along: it must not be written either.
			body := `{"default_timezone":"America/New_York","server":{"hub":{` + tc.block + `}}}`
			rr, settingsPath := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML, body)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
			}
			b := decodeFileModeStartupBlockError(t, rr)
			if b.Error.Code != ErrCodeValidationError {
				t.Errorf("code = %q, want %q", b.Error.Code, ErrCodeValidationError)
			}
			if b.Error.Details["field"] != tc.field {
				t.Errorf("details.field = %v, want %q", b.Error.Details["field"], tc.field)
			}
			if b.Error.Message != tc.wantMsg {
				t.Errorf("message = %q, want the startup message %q", b.Error.Message, tc.wantMsg)
			}
			if after := readFileModeString(t, settingsPath); after != fileModeStartupBlockSettingsYAML {
				t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
			}
		})
	}
}

// The refusal envelope matches the workstation path:
// {"error":{"code","message","details":{"field","errors"}}}.
func TestFileMode_PutServerConfig_StartupBlockErrorShape(t *testing.T) {
	rr, _ := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML,
		`{"server":{"hub":{"port_proxy":{"response_header_timeout":"1h"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	msg := config.HubPortProxyConfig{ResponseHeaderTimeout: "1h"}.Validate().Error()
	want := map[string]interface{}{
		"error": map[string]interface{}{
			"code":    ErrCodeValidationError,
			"message": msg,
			"details": map[string]interface{}{
				"field": "server.hub.port_proxy.response_header_timeout",
				"errors": []interface{}{map[string]interface{}{
					"field":   "server.hub.port_proxy.response_header_timeout",
					"message": msg,
				}},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body = %s\nwant %v", rr.Body.String(), want)
	}
}

// When several values of a block fail, details.field names the first sent
// one (sorted by koanf path) and the message is the joined startup message.
func TestFileMode_PutServerConfig_StartupBlockSeveralInvalid(t *testing.T) {
	rr, settingsPath := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML,
		`{"server":{"hub":{"conduit":{"reconnect_window":"1h","lifetime_cap":"10s"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	b := decodeFileModeStartupBlockError(t, rr)
	if b.Error.Code != ErrCodeValidationError || b.Error.Details["field"] != "server.hub.conduit.lifetime_cap" {
		t.Errorf("got code %q field %v, want %q server.hub.conduit.lifetime_cap", b.Error.Code, b.Error.Details["field"], ErrCodeValidationError)
	}
	if want := (config.HubConduitConfig{LifetimeCap: "10s", ReconnectWindow: "1h"}).Validate().Error(); b.Error.Message != want {
		t.Errorf("message = %q, want the startup message %q", b.Error.Message, want)
	}
	if after := readFileModeString(t, settingsPath); after != fileModeStartupBlockSettingsYAML {
		t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
	}
}

// A save that sends no key under these blocks neither checks nor changes
// them, even when the file holds values startup would refuse.
func TestFileMode_PutServerConfig_StartupBlockUnrelatedSave(t *testing.T) {
	const stored = `schema_version: "1"
server:
  hub:
    conduit:
      lifetime_cap: 10s
    port_proxy:
      response_header_timeout: 1s
`
	rr, settingsPath := putFileModeStartupBlock(t, stored, `{"server":{"log_level":"debug","hub":{"port":9811}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := readFileModeYAMLAt(t, settingsPath, "server", "hub", "conduit"); !reflect.DeepEqual(got, map[string]interface{}{"lifetime_cap": "10s"}) {
		t.Errorf("conduit = %v, want untouched", got)
	}
	if got := readFileModeYAMLAt(t, settingsPath, "server", "hub", "port_proxy"); !reflect.DeepEqual(got, map[string]interface{}{"response_header_timeout": "1s"}) {
		t.Errorf("port_proxy = %v, want untouched", got)
	}
	if got := readFileModeYAMLAt(t, settingsPath, "server", "hub", "port"); got != 9811 {
		t.Errorf("port = %v, want 9811", got)
	}
}

// The check runs on the merged block settings.yaml would hold after the
// write: a stored value startup would refuse fails a save that touches its
// block (naming the stored key, including under miscased request keys), and
// a save that fixes or removes it succeeds.
func TestFileMode_PutServerConfig_StartupBlockMergedWithStored(t *testing.T) {
	const stored = `schema_version: "1"
server:
  hub:
    conduit:
      lifetime_cap: 10s
`
	rr, settingsPath := putFileModeStartupBlock(t, stored, `{"server":{"hub":{"conduit":{"reconnect_window":"10s"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if b := decodeFileModeStartupBlockError(t, rr); b.Error.Details["field"] != "server.hub.conduit.lifetime_cap" {
		t.Errorf("details.field = %v, want server.hub.conduit.lifetime_cap", b.Error.Details["field"])
	}
	if after := readFileModeString(t, settingsPath); after != stored {
		t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
	}

	// Miscased keys select the same block and validate the merged result.
	rr, settingsPath = putFileModeStartupBlock(t, stored, `{"server":{"Hub":{"Conduit":{"Reconnect_Window":"10s"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("miscased: expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if b := decodeFileModeStartupBlockError(t, rr); b.Error.Details["field"] != "server.hub.conduit.lifetime_cap" {
		t.Errorf("miscased details.field = %v, want server.hub.conduit.lifetime_cap", b.Error.Details["field"])
	}
	if after := readFileModeString(t, settingsPath); after != stored {
		t.Errorf("settings.yaml changed on a refused miscased PUT:\n%s", after)
	}

	// A sent invalid value is named before a stored one; both fail, so the
	// message is the joined text startup prints.
	rr, settingsPath = putFileModeStartupBlock(t, stored, `{"server":{"hub":{"conduit":{"reconnect_window":"1h"}}}}`)
	if b := decodeFileModeStartupBlockError(t, rr); rr.Code != http.StatusUnprocessableEntity || b.Error.Details["field"] != "server.hub.conduit.reconnect_window" {
		t.Errorf("got %d field %v, want 422 server.hub.conduit.reconnect_window", rr.Code, b.Error.Details["field"])
	} else if want := (config.HubConduitConfig{LifetimeCap: "10s", ReconnectWindow: "1h"}).Validate().Error(); b.Error.Message != want {
		t.Errorf("message = %q, want the startup message %q", b.Error.Message, want)
	}
	if after := readFileModeString(t, settingsPath); after != stored {
		t.Errorf("settings.yaml changed on a refused PUT:\n%s", after)
	}

	// A save that fixes the stored value succeeds.
	rr, settingsPath = putFileModeStartupBlock(t, stored, `{"server":{"hub":{"conduit":{"lifetime_cap":"2h","reconnect_window":"10s"}}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("fixing save: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := readFileModeYAMLAt(t, settingsPath, "server", "hub", "conduit", "lifetime_cap"); got != "2h" {
		t.Errorf("lifetime_cap = %v, want 2h", got)
	}

	// Clearing the block with null succeeds.
	rr, settingsPath = putFileModeStartupBlock(t, stored, `{"server":{"hub":{"conduit":null}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("null conduit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := readFileModeYAMLAt(t, settingsPath, "server", "hub", "conduit"); got != nil {
		t.Errorf("conduit = %v, want cleared", got)
	}
}

func TestFileMode_PutServerConfig_ConduitDeliverRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"apple alias", `{"server":{"broker":{"conduit_deliver_runtimes":["apple"]}}}`},
		{"wrong case Docker", `{"server":{"broker":{"conduit_deliver_runtimes":["Docker"]}}}`},
		{"empty entry", `{"server":{"broker":{"conduit_deliver_runtimes":[""]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr, settingsPath := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML, tc.body)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
			}
			b := decodeFileModeStartupBlockError(t, rr)
			if b.Error.Code != ErrCodeValidationError {
				t.Errorf("code = %q, want %q", b.Error.Code, ErrCodeValidationError)
			}
			if b.Error.Details["field"] != "server.broker.conduit_deliver_runtimes" {
				t.Errorf("details.field = %v, want server.broker.conduit_deliver_runtimes", b.Error.Details["field"])
			}
			if after := readFileModeString(t, settingsPath); after != fileModeStartupBlockSettingsYAML {
				t.Errorf("settings.yaml changed after rejected PUT:\n%s", after)
			}
		})
	}

	t.Run("valid list saved", func(t *testing.T) {
		rr, _ := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML, `{"server":{"broker":{"conduit_deliver_runtimes":["cloudrun","podman"]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		gc, err := config.LoadGlobalConfig("")
		if err != nil {
			t.Fatalf("LoadGlobalConfig after valid PUT failed: %v", err)
		}
		if !reflect.DeepEqual(gc.RuntimeBroker.ConduitDeliverRuntimes, []string{"cloudrun", "podman"}) {
			t.Errorf("loaded ConduitDeliverRuntimes = %#v, want [cloudrun podman]", gc.RuntimeBroker.ConduitDeliverRuntimes)
		}
	})

	t.Run("explicit empty list saved", func(t *testing.T) {
		rr, _ := putFileModeStartupBlock(t, fileModeStartupBlockSettingsYAML, `{"server":{"broker":{"conduit_deliver_runtimes":[]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		gc, err := config.LoadGlobalConfig("")
		if err != nil {
			t.Fatalf("LoadGlobalConfig after empty PUT failed: %v", err)
		}
		if gc.RuntimeBroker.ConduitDeliverRuntimes == nil || len(gc.RuntimeBroker.ConduitDeliverRuntimes) != 0 {
			t.Errorf("loaded ConduitDeliverRuntimes = %#v, want non-nil empty slice", gc.RuntimeBroker.ConduitDeliverRuntimes)
		}
	})
}
