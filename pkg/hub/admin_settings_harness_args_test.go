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

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestHandlePutServerConfig_HarnessArgsIgnoredWarnings(t *testing.T) {
	srv := &Server{}

	t.Run("non-generic harness with args warns and still saves", func(t *testing.T) {
		rr, settingsPath := fileModePutServerConfig(t, srv, `{
			"harness_configs": {
				"claude-custom": {"harness": "claude", "args": ["--verbose"]},
				"gemini-cli": {"args": ["--flag"]}
			}
		}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}

		got := putWarnings(t, rr)
		if len(got) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %q", len(got), got)
		}
		if !strings.Contains(got[0], "harness_configs.claude-custom.args") || !strings.Contains(got[0], `"claude"`) {
			t.Errorf("warning[0] unexpected: %q", got[0])
		}
		if !strings.Contains(got[1], "harness_configs.gemini-cli.args") || !strings.Contains(got[1], `"gemini-cli"`) {
			t.Errorf("warning[1] unexpected: %q", got[1])
		}

		// Verify the save was not refused and args were persisted unchanged.
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var vs config.VersionedSettings
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(vs.HarnessConfigs["claude-custom"].Args, []string{"--verbose"}) {
			t.Errorf("stored claude-custom.args = %v, want [--verbose]", vs.HarnessConfigs["claude-custom"].Args)
		}
		if !reflect.DeepEqual(vs.HarnessConfigs["gemini-cli"].Args, []string{"--flag"}) {
			t.Errorf("stored gemini-cli.args = %v, want [--flag]", vs.HarnessConfigs["gemini-cli"].Args)
		}
	})

	t.Run("generic harness with args returns no warnings", func(t *testing.T) {
		rr, settingsPath := fileModePutServerConfig(t, srv, `{
			"harness_configs": {
				"my-generic": {"harness": "generic", "args": ["--flag"]},
				"generic": {"args": ["--other"]}
			}
		}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var rawResp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &rawResp); err != nil {
			t.Fatal(err)
		}
		if _, hasWarnings := rawResp["warnings"]; hasWarnings {
			t.Errorf("expected warnings key to be omitted, got %v", rawResp["warnings"])
		}

		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var vs config.VersionedSettings
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(vs.HarnessConfigs["my-generic"].Args, []string{"--flag"}) {
			t.Errorf("stored my-generic.args = %v, want [--flag]", vs.HarnessConfigs["my-generic"].Args)
		}
	})

	t.Run("non-generic harness without args returns no warnings", func(t *testing.T) {
		rr, _ := fileModePutServerConfig(t, srv, `{
			"harness_configs": {
				"claude": {"harness": "claude", "image": "claude:latest"},
				"empty-args": {"harness": "codex", "args": []}
			}
		}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if got := putWarnings(t, rr); len(got) != 0 {
			t.Errorf("expected no warnings, got %q", got)
		}
	})
}

func TestPutServerConfigDB_HarnessArgsIgnoredWarnings(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		return rr
	}

	t.Run("non-generic harness with args warns and still saves", func(t *testing.T) {
		rr := put(`{
			"harness_configs": {
				"claude-custom": {"harness": "claude", "args": ["--verbose"]},
				"codex": {"args": ["--flag"]}
			}
		}`)

		var rawResp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &rawResp); err != nil {
			t.Fatal(err)
		}
		if rawResp["status"] != "saved" {
			t.Errorf("status = %v, want saved", rawResp["status"])
		}
		if _, hasReload := rawResp["reload"]; !hasReload {
			t.Errorf("expected reload object in response: %v", rawResp)
		}

		got := putWarnings(t, rr)
		if len(got) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %q", len(got), got)
		}
		if !strings.Contains(got[0], "harness_configs.claude-custom.args") || !strings.Contains(got[0], `"claude"`) {
			t.Errorf("warning[0] unexpected: %q", got[0])
		}
		if !strings.Contains(got[1], "harness_configs.codex.args") || !strings.Contains(got[1], `"codex"`) {
			t.Errorf("warning[1] unexpected: %q", got[1])
		}

		snap := ops.Snapshot()
		if !reflect.DeepEqual(snap.HarnessConfigs["claude-custom"].Args, []string{"--verbose"}) {
			t.Errorf("stored claude-custom.args = %v, want [--verbose]", snap.HarnessConfigs["claude-custom"].Args)
		}
		if !reflect.DeepEqual(snap.HarnessConfigs["codex"].Args, []string{"--flag"}) {
			t.Errorf("stored codex.args = %v, want [--flag]", snap.HarnessConfigs["codex"].Args)
		}
	})

	t.Run("unrelated save does not re-emit harness_configs warnings", func(t *testing.T) {
		rr := put(`{"default_max_turns": 25}`)
		if got := putWarnings(t, rr); len(got) != 0 {
			t.Errorf("expected no warnings on unrelated save, got %q", got)
		}
	})

	t.Run("generic harness with args returns no warnings", func(t *testing.T) {
		rr := put(`{
			"harness_configs": {
				"my-generic": {"harness": "generic", "args": ["--flag"]},
				"generic": {"args": ["--other"]}
			}
		}`)
		var rawResp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &rawResp); err != nil {
			t.Fatal(err)
		}
		if _, hasWarnings := rawResp["warnings"]; hasWarnings {
			t.Errorf("expected warnings key to be omitted, got %v", rawResp["warnings"])
		}
		snap := ops.Snapshot()
		if !reflect.DeepEqual(snap.HarnessConfigs["my-generic"].Args, []string{"--flag"}) {
			t.Errorf("stored my-generic.args = %v, want [--flag]", snap.HarnessConfigs["my-generic"].Args)
		}
	})

	t.Run("non-generic harness without args returns no warnings", func(t *testing.T) {
		rr := put(`{
			"harness_configs": {
				"claude": {"harness": "claude", "image": "claude:latest"},
				"empty-args": {"harness": "codex", "args": []}
			}
		}`)
		if got := putWarnings(t, rr); len(got) != 0 {
			t.Errorf("expected no warnings, got %q", got)
		}
	})

	t.Run("clearing harness_configs returns no warnings", func(t *testing.T) {
		rr := put(`{"harness_configs": null}`)
		if got := putWarnings(t, rr); len(got) != 0 {
			t.Errorf("expected no warnings on clear, got %q", got)
		}
	})
}
